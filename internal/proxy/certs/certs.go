// Package certs provides certificate management for the HTTPS reverse proxy.
// It integrates with mkcert to generate locally-trusted development certificates.
package certs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charliek/prox/internal/constants"
)

// Manager handles certificate generation and management using mkcert.
type Manager struct {
	certsDir string
	domain   string
	// trust is where this manager reports what mkcert said about its own local
	// CA. It is process-scoped, not manager-scoped (see TrustResolver): every
	// manager in a process feeds the same verdict, so one real generation
	// answers for every other manager and spares them the probe.
	trust *TrustResolver
}

// CertPaths contains the paths to the certificate and key files.
type CertPaths struct {
	CertFile string
	KeyFile  string
}

// NewManager creates a new certificate manager reporting to the process-wide
// trust resolver.
func NewManager(certsDir, domain string) *Manager {
	return NewManagerWithTrust(certsDir, domain, SharedTrust())
}

// NewManagerWithTrust is NewManager with an explicit trust resolver. It exists
// for callers that own a resolver deliberately — chiefly tests, which must not
// have one test's mkcert verdict latched into the next test's process-wide
// resolver.
func NewManagerWithTrust(certsDir, domain string, trust *TrustResolver) *Manager {
	if trust == nil {
		trust = SharedTrust()
	}
	return &Manager{
		certsDir: expandPath(certsDir),
		domain:   domain,
		trust:    trust,
	}
}

// CheckMkcert verifies that mkcert is installed and accessible.
func (m *Manager) CheckMkcert() error {
	_, err := exec.LookPath("mkcert")
	if err != nil {
		return fmt.Errorf("mkcert not found in PATH (install from https://github.com/FiloSottile/mkcert): %w", err)
	}
	return nil
}

// EnsureCerts ensures that valid certificates exist for the configured domain.
// If certificates don't exist, they will be generated.
//
// It returns the paths to the certificate and key files plus the notable lines
// mkcert printed while generating them — empty when nothing was generated,
// because the certs were already on disk. Those lines are the only channel
// mkcert's advice has: in shared mode this runs inside the daemon, whose
// stdout and stderr are /dev/null (internal/proxyd/daemon.go), so anything
// mkcert says is discarded outright unless it is captured here.
//
// The lines are also fed to the process-wide TrustResolver, so a generation
// that happened for ANY domain answers the CA-trust question for the whole
// process for free.
func (m *Manager) EnsureCerts() (*CertPaths, []string, error) {
	paths := m.getCertPaths()

	// Check if certificates already exist
	if m.certsExist(paths) {
		return paths, nil, nil
	}

	// Generate new certificates
	out, err := m.generateCerts(paths)
	if err != nil {
		return nil, out, err
	}
	m.trust.observe(out)

	return paths, out, nil
}

func (m *Manager) getCertPaths() *CertPaths {
	// Sanitize domain for filename (replace dots with underscores)
	safeDomain := strings.ReplaceAll(m.domain, ".", "_")
	return &CertPaths{
		CertFile: filepath.Join(m.certsDir, fmt.Sprintf("%s.pem", safeDomain)),
		KeyFile:  filepath.Join(m.certsDir, fmt.Sprintf("%s-key.pem", safeDomain)),
	}
}

func (m *Manager) certsExist(paths *CertPaths) bool {
	if _, err := os.Stat(paths.CertFile); err != nil {
		return false
	}
	if _, err := os.Stat(paths.KeyFile); err != nil {
		return false
	}
	return true
}

// generateCerts shells to mkcert and returns the notable lines it printed.
//
// It CAPTURES mkcert's combined stdout and stderr rather than wiring them to
// this process's own streams, which is what makes the CA-untrusted note usable
// at all: in the shared daemon those streams are /dev/null, so the one warning
// that tells a user why every HTTPS request in their browser fails was being
// thrown away. Captured, it reaches the person who typed the command — and on
// failure it is attached to the error, which is a strict improvement over
// today for the same reason.
func (m *Manager) generateCerts(paths *CertPaths) ([]string, error) {
	if err := m.CheckMkcert(); err != nil {
		return nil, err
	}

	// Ensure the certs directory exists
	if err := os.MkdirAll(m.certsDir, constants.DirPermissionPrivate); err != nil {
		return nil, fmt.Errorf("creating certs directory: %w", err)
	}

	// Generate wildcard certificate for the domain
	// mkcert -cert-file <cert> -key-file <key> "*.domain" "domain"
	wildcardDomain := fmt.Sprintf("*.%s", m.domain)
	cmd := exec.Command("mkcert",
		"-cert-file", paths.CertFile,
		"-key-file", paths.KeyFile,
		wildcardDomain,
		m.domain,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// Capturing into a buffer means exec creates a pipe and Wait blocks until
	// EOF -- so any grandchild mkcert leaves holding that pipe would hang
	// EnsureCerts, and with it register() under lifecycleMu (CodeRabbit
	// review). Before the capture, Stdout was an *os.File handed over as a raw
	// fd and Wait only waited on the process.
	cmd.WaitDelay = probeWaitDelay

	if err := cmd.Run(); err != nil {
		lines := notableLines(buf.String())
		if len(lines) > 0 {
			return lines, fmt.Errorf("generating certificates for %s: %w\nmkcert output:\n%s",
				m.domain, err, strings.Join(lines, "\n"))
		}
		return nil, fmt.Errorf("generating certificates for %s: %w", m.domain, err)
	}

	return notableLines(buf.String()), nil
}

// expandPath expands ~ to the user's home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
