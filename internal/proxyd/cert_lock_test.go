package proxyd

import (
	"crypto/tls"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMultiDomainCertManager_SlowGenerateDoesNotBlockOtherDomain is the D3/D4
// lock-discipline regression: a slow first-time generation for domain B must NOT
// block GetCertificate for the already-loaded domain A. It proves the refactor's
// core property — EnsureDomain runs the generator OUTSIDE m.mu — by overriding
// m.generate (white-box, same package) with an in-memory fake instead of shelling
// mkcert.
//
// Coverage boundary (intentional): overriding m.generate bypasses the real
// generateViaMkcert, so this does not exercise the production mkcert/LoadX509KeyPair
// path itself. Testing that path's lock discipline directly would require mkcert on
// the runner or a deeper inner seam (out of scope). The real generateViaMkcert body
// holds m.mu only via managerFor's brief map-lock; because that lock is NOT
// reentrant, a regression that wrapped the whole generator in m.mu.Lock would
// deadlock rather than silently stall — the structural backstop for this boundary.
func TestMultiDomainCertManager_SlowGenerateDoesNotBlockOtherDomain(t *testing.T) {
	m := NewMultiDomainCertManager(t.TempDir())

	certA, err := generateWildcardCert("a.test")
	require.NoError(t, err)
	certB, err := generateWildcardCert("b.test")
	require.NoError(t, err)

	// bStarted signals that generate("b.test") has been entered (so B is provably
	// blocked with NO cache lock held); bRelease unblocks it. A returns
	// immediately; any other domain is unexpected.
	bStarted := make(chan struct{})
	bRelease := make(chan struct{})
	m.generate = func(domain string) (*tls.Certificate, error) {
		switch domain {
		case "a.test":
			return certA, nil
		case "b.test":
			close(bStarted)
			<-bRelease
			return certB, nil
		default:
			return nil, fmt.Errorf("unexpected domain %s", domain)
		}
	}

	// Preload A so m.loaded["a.test"] is set (fast path, no blocking).
	require.NoError(t, m.EnsureDomain("a.test"))

	// Start B's generation; it blocks inside generate until bRelease is closed.
	bErr := make(chan error, 1)
	go func() { bErr <- m.EnsureDomain("b.test") }()

	// Confirm B is inside generate (blocked on bRelease, holding no lock).
	<-bStarted

	// While B is provably still blocked, GetCertificate for an A host must return
	// A's cert. This assertion completes BEFORE bRelease is closed below, so A's
	// handshake path deterministically did not wait on B's generation.
	gotA, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "x.a.test"})
	require.NoError(t, err)
	require.Same(t, certA, gotA, "A's host must serve A's cert while B is blocked")

	// Unblock B; its EnsureDomain must succeed and publish B's cert.
	close(bRelease)
	require.NoError(t, <-bErr)

	gotB, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "svc.b.test"})
	require.NoError(t, err)
	require.Same(t, certB, gotB, "B's host must serve B's cert once generation completes")
}

// TestMultiDomainCertManager_GenerateError_PublishesNothing pins the rollback
// contract: when generate fails, EnsureDomain returns that error unchanged and
// publishes nothing (m.loaded still lacks the domain), so register() can map it
// to CERT_GENERATION_FAILED and roll back cleanly.
func TestMultiDomainCertManager_GenerateError_PublishesNothing(t *testing.T) {
	m := NewMultiDomainCertManager(t.TempDir())

	wantErr := errors.New("forced generate failure")
	m.generate = func(domain string) (*tls.Certificate, error) {
		return nil, wantErr
	}

	err := m.EnsureDomain("fail.test")
	require.Same(t, wantErr, err, "EnsureDomain must return the generate error unchanged")

	m.mu.RLock()
	_, ok := m.loaded["fail.test"]
	m.mu.RUnlock()
	assert.False(t, ok, "a failed generation must publish nothing")
}
