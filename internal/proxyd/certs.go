package proxyd

import (
	"crypto/tls"
	"fmt"
	"strings"
	"sync"

	"github.com/charliek/prox/internal/proxy/certs"
)

// MultiDomainCertManager manages TLS certificates for multiple base domains.
// It wraps the existing certs.Manager, creating one per base domain, and
// provides a GetCertificate callback for TLS SNI-based cert selection.
type MultiDomainCertManager struct {
	mu       sync.RWMutex
	certsDir string
	managers map[string]*certs.Manager    // base domain -> cert manager
	loaded   map[string]*tls.Certificate  // base domain -> loaded cert
}

// NewMultiDomainCertManager creates a new multi-domain cert manager.
func NewMultiDomainCertManager(certsDir string) *MultiDomainCertManager {
	return &MultiDomainCertManager{
		certsDir: certsDir,
		managers: make(map[string]*certs.Manager),
		loaded:   make(map[string]*tls.Certificate),
	}
}

// EnsureDomain ensures a wildcard certificate exists for the given base domain.
// It generates one via mkcert if it doesn't exist yet.
func (m *MultiDomainCertManager) EnsureDomain(domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Already loaded
	if _, ok := m.loaded[domain]; ok {
		return nil
	}

	// Create or reuse manager for this domain
	mgr, ok := m.managers[domain]
	if !ok {
		mgr = certs.NewManager(m.certsDir, domain)
		m.managers[domain] = mgr
	}

	// Ensure certs exist (generate if needed)
	paths, err := mgr.EnsureCerts()
	if err != nil {
		return fmt.Errorf("ensuring certs for %s: %w", domain, err)
	}

	// Load the certificate
	cert, err := tls.LoadX509KeyPair(paths.CertFile, paths.KeyFile)
	if err != nil {
		return fmt.Errorf("loading cert for %s: %w", domain, err)
	}

	m.loaded[domain] = &cert
	return nil
}

// GetCertificate is the TLS SNI callback. It selects the appropriate
// certificate based on the server name in the TLS ClientHello.
func (m *MultiDomainCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello.ServerName == "" {
		return nil, fmt.Errorf("no SNI server name provided")
	}

	domain := extractBaseDomain(hello.ServerName)

	m.mu.RLock()
	cert, ok := m.loaded[domain]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no certificate for domain %s (from %s)", domain, hello.ServerName)
	}
	return cert, nil
}

// RemoveDomain removes a domain from the cache. Cert files are left on disk
// for reuse if the domain is registered again later.
func (m *MultiDomainCertManager) RemoveDomain(domain string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.loaded, domain)
	delete(m.managers, domain)
}

// extractBaseDomain extracts the base domain from a full hostname.
// e.g., "api.local.stridelabs.ai" -> "local.stridelabs.ai"
func extractBaseDomain(hostname string) string {
	parts := strings.SplitN(hostname, ".", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return hostname
}
