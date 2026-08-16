package proxyd

import (
	"crypto/tls"
	"fmt"
	"strings"
	"sync"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy/certs"
)

// certWarningHolder caches the user-facing warnings the cert layer observed, so
// register() can hand them back to the CLI that cannot see the daemon's output.
//
// It is deliberately CA-SCOPED, not per-domain: the state these warnings
// describe (is mkcert's local CA installed in the trust stores?) is a property
// of the CA and the machine, not of any one base domain, so one verdict is held
// for the daemon's whole process lifetime and every registration reads the same
// one.
//
// That scoping is what makes a warning survive the warm-cert case. mkcert only
// runs when the cert FILES are absent (internal/proxy/certs/certs.go) and
// EnsureDomain short-circuits on its own loaded-cert cache, so a daemon that
// starts with certs already on disk — the normal case, since a release requires
// restarting the daemon while its certs dir persists — may never re-enter
// generation. A verdict recorded once (by whatever observes it) therefore has to
// keep answering for later registrations that generate nothing.
//
// Its mutex is a leaf: it is never held across mkcert, file I/O, or any other
// lock, and in particular it is NOT MultiDomainCertManager.mu — recording a
// warning mid-generation must not take the cache lock that EnsureDomain
// deliberately drops for the duration of generation.
//
// It is keyed BY CODE and holds at most one warning per code, which does two
// things an append-only slice could not (CodeRabbit review, N1/N2):
//
//   - It makes a warning CLEARABLE. A condition like "the CA is untrusted" is
//     one the user goes and fixes; a verdict that could only ever be added
//     would keep telling them their CA is untrusted for the rest of a
//     long-lived daemon's life after they ran `mkcert -install`. Telling the
//     user something false is the exact bug this whole plan exists to remove,
//     so the producer (B1) re-derives the verdict and calls clear when it no
//     longer holds.
//   - It bounds the holder by the number of distinct codes rather than by how
//     many times a producer happens to observe something.
//
// Insertion order is preserved so the output is stable across reads.
type certWarningHolder struct {
	mu       sync.Mutex
	order    []string
	warnings map[string]domain.Warning
}

// set records (or replaces) the warning for w.Code. Replacing rather than
// appending means a producer that observes the same condition on several
// domains yields one warning, not one per domain.
func (h *certWarningHolder) set(w domain.Warning) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.warnings == nil {
		h.warnings = make(map[string]domain.Warning, 1)
	}
	if _, seen := h.warnings[w.Code]; !seen {
		h.order = append(h.order, w.Code)
	}
	h.warnings[w.Code] = w
}

// clear drops the warning for code, if any. It is how a resolved condition
// stops being reported without waiting for the daemon to restart.
func (h *certWarningHolder) clear(code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.warnings[code]; !ok {
		return
	}
	delete(h.warnings, code)
	for i, c := range h.order {
		if c == code {
			h.order = append(h.order[:i:i], h.order[i+1:]...)
			break
		}
	}
}

// snapshot returns a copy of the recorded warnings in insertion order; the
// caller may retain or mutate it freely.
func (h *certWarningHolder) snapshot() []domain.Warning {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.warnings) == 0 {
		return nil
	}
	out := make([]domain.Warning, 0, len(h.warnings))
	for _, code := range h.order {
		if w, ok := h.warnings[code]; ok {
			out = append(out, w)
		}
	}
	return out
}

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

	// warnings holds the CA-scoped user-facing warnings observed by the cert
	// layer (see certWarningHolder). Guarded by its own mutex, never by mu.
	warnings certWarningHolder
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
func (m *MultiDomainCertManager) EnsureDomain(baseDomain string) error {
	// Fast path: already loaded — RLock only, never blocks on generation.
	m.mu.RLock()
	_, ok := m.loaded[baseDomain]
	m.mu.RUnlock()
	if ok {
		return nil
	}

	// Generate with NO cache lock held (m.generate wraps managerFor +
	// EnsureCerts(mkcert) + LoadX509KeyPair; only managerFor briefly locks
	// m.mu for the managers map). GetCertificate's RLock is therefore never
	// blocked by mkcert — the whole point of the refactor.
	cert, err := m.generate(baseDomain)
	if err != nil {
		return err // already wrapped by m.generate
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.loaded[baseDomain]; ok { // double-check: a concurrent caller won
		return nil
	}
	m.loaded[baseDomain] = cert
	return nil
}

// generateViaMkcert loads a cert for a base domain: it resolves the domain's
// certs.Manager, shells to mkcert to generate the cert files if needed, then
// loads the key pair. It holds NO m.mu except via managerFor's brief lock on the
// managers map — never across mkcert or file I/O.
func (m *MultiDomainCertManager) generateViaMkcert(baseDomain string) (*tls.Certificate, error) {
	mgr := m.managerFor(baseDomain)

	// Ensure certs exist (generate via mkcert if needed).
	paths, err := mgr.EnsureCerts()
	if err != nil {
		return nil, fmt.Errorf("ensuring certs for %s: %w", baseDomain, err)
	}

	// Load the certificate.
	cert, err := tls.LoadX509KeyPair(paths.CertFile, paths.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading cert for %s: %w", baseDomain, err)
	}
	return &cert, nil
}

// RecordWarning records (or replaces) a user-facing warning observed while
// preparing certificates. It is the PRODUCER SEAM of the daemon→client warning
// channel: B1's mkcert detection calls this once it can actually detect an
// untrusted CA, and tests call it directly to stand in for that detection. It
// takes no cache lock, so it is safe to call from inside generation, which runs
// outside mu by design.
func (m *MultiDomainCertManager) RecordWarning(w domain.Warning) {
	m.warnings.set(w)
}

// ClearWarning withdraws a previously recorded warning. It is half of the
// producer seam and not optional decoration: a warning that could only be added
// would outlive the condition it describes, so a user who fixed the problem
// would keep being told it was still there until the daemon restarted.
func (m *MultiDomainCertManager) ClearWarning(code string) {
	m.warnings.clear(code)
}

// Warnings returns the warnings recorded so far, newest last, as a fresh slice.
// register() calls it on every success arm; it never blocks on cert generation.
func (m *MultiDomainCertManager) Warnings() []domain.Warning {
	return m.warnings.snapshot()
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
