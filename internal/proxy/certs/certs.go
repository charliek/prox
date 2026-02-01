// Package certs provides certificate management for the HTTPS reverse proxy.
// It integrates with mkcert to generate locally-trusted development certificates.
package certs

import (
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
}

// CertPaths contains the paths to the certificate and key files.
type CertPaths struct {
	CertFile string
	KeyFile  string
}

// NewManager creates a new certificate manager.
func NewManager(certsDir, domain string) *Manager {
	return &Manager{
		certsDir: expandPath(certsDir),
		domain:   domain,
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

// CheckCAInstalled verifies that the mkcert CA is installed in the system trust store.
func (m *Manager) CheckCAInstalled() (bool, error) {
	if err := m.CheckMkcert(); err != nil {
		return false, err
	}

	// mkcert -check returns 0 if CA is installed, non-zero otherwise
	// However, mkcert doesn't have a -check flag, so we use CAROOT to check
	cmd := exec.Command("mkcert", "-CAROOT")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("checking mkcert CAROOT: %w", err)
	}

	caRoot := strings.TrimSpace(string(output))
	if caRoot == "" {
		return false, nil
	}

	// Check if the CA files exist
	rootCA := filepath.Join(caRoot, "rootCA.pem")
	if _, err := os.Stat(rootCA); os.IsNotExist(err) {
		return false, nil
	}

	return true, nil
}

// InstallCA installs the mkcert CA into the system trust store.
// This typically requires elevated privileges (sudo).
func (m *Manager) InstallCA() error {
	if err := m.CheckMkcert(); err != nil {
		return err
	}

	cmd := exec.Command("mkcert", "-install")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing mkcert CA: %w", err)
	}
	return nil
}

// EnsureCerts ensures that valid certificates exist for the configured domain.
// If certificates don't exist, they will be generated.
// Returns the paths to the certificate and key files.
func (m *Manager) EnsureCerts() (*CertPaths, error) {
	paths := m.getCertPaths()

	// Check if certificates already exist
	if m.certsExist(paths) {
		return paths, nil
	}

	// Generate new certificates
	if err := m.generateCerts(paths); err != nil {
		return nil, err
	}

	return paths, nil
}

// GetCertPaths returns the expected paths for certificate files.
func (m *Manager) GetCertPaths() *CertPaths {
	return m.getCertPaths()
}

// RegenerateCerts forces regeneration of certificates even if they exist.
func (m *Manager) RegenerateCerts() (*CertPaths, error) {
	paths := m.getCertPaths()

	// Remove existing certificates
	_ = os.Remove(paths.CertFile)
	_ = os.Remove(paths.KeyFile)

	// Generate new certificates
	if err := m.generateCerts(paths); err != nil {
		return nil, err
	}

	return paths, nil
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

func (m *Manager) generateCerts(paths *CertPaths) error {
	if err := m.CheckMkcert(); err != nil {
		return err
	}

	// Ensure the certs directory exists
	if err := os.MkdirAll(m.certsDir, constants.DirPermissionPrivate); err != nil {
		return fmt.Errorf("creating certs directory: %w", err)
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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generating certificates for %s: %w", m.domain, err)
	}

	return nil
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
