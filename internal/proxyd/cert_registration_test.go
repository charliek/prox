package proxyd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCertManager is an in-memory certManager for deterministic tests. It
// mirrors MultiDomainCertManager's SNI selection (keyed by extractBaseDomain)
// but generates self-signed certs in-process instead of shelling out to mkcert,
// and records which domains were EnsureDomain'd so tests can assert the register
// flow's cert gating.
type fakeCertManager struct {
	mu      sync.Mutex
	certs   map[string]*tls.Certificate // base domain -> cert
	ensured map[string]int              // domain -> EnsureDomain call count
	failFor map[string]error            // domain -> forced EnsureDomain error
}

func newFakeCertManager() *fakeCertManager {
	return &fakeCertManager{
		certs:   make(map[string]*tls.Certificate),
		ensured: make(map[string]int),
		failFor: make(map[string]error),
	}
}

// failDomain configures EnsureDomain to fail for the given domain.
func (f *fakeCertManager) failDomain(domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFor[domain] = fmt.Errorf("forced cert failure for %s", domain)
}

// ensureCount reports how many times EnsureDomain was called for a domain.
func (f *fakeCertManager) ensureCount(domain string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensured[domain]
}

// EnsureDomain generates and caches a self-signed wildcard cert for domain,
// recording the call. It is idempotent (one cert per domain) like the real one.
func (f *fakeCertManager) EnsureDomain(domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensured[domain]++

	if err, ok := f.failFor[domain]; ok {
		return err
	}
	if _, ok := f.certs[domain]; ok {
		return nil
	}

	cert, err := generateWildcardCert(domain)
	if err != nil {
		return err
	}
	f.certs[domain] = cert
	return nil
}

// GetCertificate is the SNI callback: it selects a cert by the ClientHello's
// base domain using the same extractBaseDomain logic as the real manager.
func (f *fakeCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello.ServerName == "" {
		return nil, fmt.Errorf("no SNI server name provided")
	}
	domain := extractBaseDomain(hello.ServerName)

	f.mu.Lock()
	cert, ok := f.certs[domain]
	f.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("no certificate for domain %s (from %s)", domain, hello.ServerName)
	}
	return cert, nil
}

// generateWildcardCert builds an in-memory self-signed leaf cert covering both
// "*.<domain>" and "<domain>" with an ECDSA P-256 key.
func generateWildcardCert(domain string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domain},
		DNSNames:              []string{"*." + domain, domain},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// backendTarget returns a syntactically valid, unbound host:port for a
// RegisterRequest's ServiceTarget. These tests only exercise the cert gate and
// the TLS handshake against the proxy's own listener (via dialTLSVerify) —
// never a proxied HTTP round trip to the backend — so the target need not be a
// live server.
func backendTarget(t *testing.T) (host string, port int) {
	t.Helper()
	return "127.0.0.1", freePort(t)
}

// newProxyServerWithCertMgr mirrors newProxyServer but wires the DynamicProxy to
// a caller-supplied certManager so HTTPS registrations exercise the real cert
// gate and SNI cert selection.
func newProxyServerWithCertMgr(t *testing.T, cm certManager) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	s := NewServer(ServerConfig{SocketPath: "", Logger: logger, Version: "test"})
	reg := NewRegistry()
	ms := NewManagers(100, nil)
	dp := NewDynamicProxy(reg, cm, ms, nil, logger)
	s.SetRegistry(reg)
	s.SetProxy(dp)
	s.SetManagers(ms)
	t.Cleanup(func() { _ = dp.Shutdown(context.Background()) })
	return s
}

// dialTLSVerify TLS-dials addr with the given SNI server name, retrying briefly
// while the listener warms up, and asserts the served leaf cert verifies the
// hostname. InsecureSkipVerify lets the handshake complete so we can inspect the
// presented cert directly (the fake certs are self-signed).
func dialTLSVerify(t *testing.T, addr, serverName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true,
		})
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		state := conn.ConnectionState()
		_ = conn.Close()
		require.NotEmpty(t, state.PeerCertificates,
			"handshake produced no peer certificates for %s", serverName)
		require.NoError(t, state.PeerCertificates[0].VerifyHostname(serverName),
			"served cert must verify hostname %s", serverName)
		return
	}
	t.Fatalf("TLS dial to %s (SNI %s) never succeeded: %v", addr, serverName, lastErr)
}

// TestCertRegistration_SharedHTTPSPort_BothDomainsServed is the #58 regression:
// a second domain joining an already-bound shared HTTPS port must still get its
// cert generated, so its SNI handshake succeeds. The old cert loop scanned only
// NEW listener ports, so the joining domain (no new port) was skipped.
func TestCertRegistration_SharedHTTPSPort_BothDomainsServed(t *testing.T) {
	backendHost, backendPort := backendTarget(t)

	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)
	port := freePort(t)

	reqA := RegisterRequest{
		ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test", Domain: "a.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	}
	registerOK(t, s, reqA)

	// B shares A's already-bound HTTPS port: no new listener, so the old loop
	// would have skipped B's cert.
	reqB := RegisterRequest{
		ProjectDir: "/projects/b", PID: os.Getpid(), Version: "test", Domain: "b.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	}
	registerOK(t, s, reqB)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	dialTLSVerify(t, addr, "svc.a.test")
	dialTLSVerify(t, addr, "svc.b.test")

	assert.Positive(t, fake.ensureCount("a.test"), "A's domain must get a cert")
	assert.Positive(t, fake.ensureCount("b.test"),
		"B's domain must get a cert even though it joined an already-bound port")
}

// TestCertRegistration_JoiningCertFailureRollsBack pins that when a domain
// joining a shared HTTPS port fails cert generation, its registration rolls back
// cleanly while the shared listener and the incumbent domain survive intact.
func TestCertRegistration_JoiningCertFailureRollsBack(t *testing.T) {
	backendHost, backendPort := backendTarget(t)

	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)
	port := freePort(t)

	reqA := RegisterRequest{
		ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test", Domain: "a.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	}
	registerOK(t, s, reqA)

	fake.failDomain("b.test")
	reqB := RegisterRequest{
		ProjectDir: "/projects/b", PID: os.Getpid(), Version: "test", Domain: "b.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: port,
	}
	status, body := s.register(reqB)
	require.Equal(t, http.StatusInternalServerError, status, "cert failure must fail the register: %v", body)
	errResp, ok := body.(ErrorResponse)
	require.True(t, ok, "failure body should be an ErrorResponse, got %T", body)
	assert.Equal(t, "CERT_GENERATION_FAILED", errResp.Code)

	// B's routes are gone; A's survive.
	_, ok = s.registry.Lookup("svc.b.test", port)
	assert.False(t, ok, "B's route must be rolled back")
	_, ok = s.registry.Lookup("svc.a.test", port)
	assert.True(t, ok, "A's route must survive B's rollback")

	// The shared listener survives the rollback and still serves A.
	dialTLSVerify(t, fmt.Sprintf("127.0.0.1:%d", port), "svc.a.test")
}

// TestCertRegistration_HTTPOnly_NoEnsureDomain verifies an HTTP-only
// registration never triggers cert generation.
func TestCertRegistration_HTTPOnly_NoEnsureDomain(t *testing.T) {
	backendHost, backendPort := backendTarget(t)

	fake := newFakeCertManager()
	s := newProxyServerWithCertMgr(t, fake)

	req := RegisterRequest{
		ProjectDir: "/projects/http", PID: os.Getpid(), Version: "test", Domain: "http.test",
		Services: map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPPort: freePort(t),
	}
	registerOK(t, s, req)

	assert.Zero(t, fake.ensureCount("http.test"),
		"an HTTP-only registration must not generate a cert")
}

// TestCertRegistration_NilCertMgr_HTTPSNoPanic pins that an HTTPS registration
// against a proxy with no cert manager fails cleanly (via the explicit
// AddListener guard) instead of panicking on a nil-interface method value.
func TestCertRegistration_NilCertMgr_HTTPSNoPanic(t *testing.T) {
	backendHost, backendPort := backendTarget(t)

	// newProxyServer wires the DynamicProxy with a nil certMgr.
	s := newProxyServer(t)

	req := RegisterRequest{
		ProjectDir: "/projects/nilcert", PID: os.Getpid(), Version: "test", Domain: "nil.test",
		Services:  map[string]ServiceTarget{"svc": {Host: backendHost, Port: backendPort}},
		HTTPSPort: freePort(t),
	}

	var status int
	var body any
	require.NotPanics(t, func() {
		status, body = s.register(req)
	}, "an HTTPS register with no cert manager must not panic")

	require.NotEqual(t, http.StatusOK, status, "HTTPS register with no cert manager must fail: %v", body)
	errResp, ok := body.(ErrorResponse)
	require.True(t, ok, "failure body should be an ErrorResponse, got %T", body)
	assert.Equal(t, "PORT_BIND_FAILED", errResp.Code)
	assert.Contains(t, errResp.Error, "no certificate manager")
}
