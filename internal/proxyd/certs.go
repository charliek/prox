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
	managers map[string]*certs.Manager   // base domain -> cert manager
	loaded   map[string]*tls.Certificate // base domain -> loaded cert

	// generate loads (managerFor + mkcert EnsureCerts + LoadX509KeyPair) a cert
	// for a base domain, holding NO cache lock across mkcert/file I/O. Set once
	// in the constructor; overridable in tests ONLY (do not mutate
	// post-construction).
	generate func(domain string) (*tls.Certificate, error)
}

// NewMultiDomainCertManager creates a new multi-domain cert manager.
func NewMultiDomainCertManager(certsDir string) *MultiDomainCertManager {
	m := &MultiDomainCertManager{
		certsDir: certsDir,
		managers: make(map[string]*certs.Manager),
		loaded:   make(map[string]*tls.Certificate),
	}
	m.generate = m.generateViaMkcert
	return m
}

// EnsureDomain ensures a wildcard certificate exists for the given base domain,
// generating one via mkcert if it isn't loaded yet. Generation (mkcert + file
// load) now runs OUTSIDE m.mu — only a brief RLock (fast path) and a brief Lock
// (publish) touch the cache — so a joining domain's first-time generation never
// stalls GetCertificate's RLock and thus never blocks TLS handshakes for OTHER
// domains on the shared HTTPS listener.
//
// The sole caller, register(), is serialized under lifecycleMu, so there is no
// concurrent EnsureDomain for the same domain and a per-domain single-flight is
// unnecessary. IF a future non-register caller is added, a per-domain
// single-flight MUST be reintroduced: two concurrent EnsureCerts on the same
// *certs.Manager would race on the cert files.
func (m *MultiDomainCertManager) EnsureDomain(domain string) error {
	// Fast path: already loaded — RLock only, never blocks on generation.
	m.mu.RLock()
	_, ok := m.loaded[domain]
	m.mu.RUnlock()
	if ok {
		return nil
	}

	// Generate with NO cache lock held (m.generate wraps managerFor +
	// EnsureCerts(mkcert) + LoadX509KeyPair; only managerFor briefly locks
	// m.mu for the managers map). GetCertificate's RLock is therefore never
	// blocked by mkcert — the whole point of the refactor.
	cert, err := m.generate(domain)
	if err != nil {
		return err // already wrapped by m.generate
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.loaded[domain]; ok { // double-check: a concurrent caller won
		return nil
	}
	m.loaded[domain] = cert
	return nil
}

// generateViaMkcert loads a cert for a base domain: it resolves the domain's
// certs.Manager, shells to mkcert to generate the cert files if needed, then
// loads the key pair. It holds NO m.mu except via managerFor's brief lock on the
// managers map — never across mkcert or file I/O.
func (m *MultiDomainCertManager) generateViaMkcert(domain string) (*tls.Certificate, error) {
	mgr := m.managerFor(domain)

	// Ensure certs exist (generate via mkcert if needed).
	paths, err := mgr.EnsureCerts()
	if err != nil {
		return nil, fmt.Errorf("ensuring certs for %s: %w", domain, err)
	}

	// Load the certificate.
	cert, err := tls.LoadX509KeyPair(paths.CertFile, paths.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading cert for %s: %w", domain, err)
	}
	return &cert, nil
}

// managerFor returns the certs.Manager for a base domain, creating and caching
// it on first use. It briefly locks m.mu for the managers map only; it never
// runs mkcert.
func (m *MultiDomainCertManager) managerFor(domain string) *certs.Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	mgr, ok := m.managers[domain]
	if !ok {
		mgr = certs.NewManager(m.certsDir, domain)
		m.managers[domain] = mgr
	}
	return mgr
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
//
// Currently unused in production. It is NOT concurrency-safe with EnsureDomain's
// out-of-lock generation: a RemoveDomain racing a concurrent generation could be
// followed by that generation's publish, resurrecting the domain. Any future use
// must serialize against in-flight EnsureDomain (e.g. under lifecycleMu) or
// reintroduce a per-domain single-flight.
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
