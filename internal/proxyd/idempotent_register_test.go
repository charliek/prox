package proxyd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveIdentity returns this process's PID and its real start token, skipping the
// test when the token is unreadable or zero (an idempotent re-register requires a
// provable non-zero token; without one the register is treated as a genuine
// conflict, so the idempotent path can't be exercised). It builds on the
// package's mustSelfToken skip helper, adding the non-zero requirement.
func liveIdentity(t *testing.T) (int, int64) {
	t.Helper()
	token := mustSelfToken(t)
	if token == 0 {
		t.Skip("process start token is zero on this platform; idempotent re-register needs a non-zero token")
	}
	return os.Getpid(), token
}

// TestIdempotentReRegister_SameLiveIdentitySucceeds pins D6a: a same-dir register
// whose live holder is the SAME process generation (same PID + matching non-zero
// start token) is an idempotent re-register — it returns 200 with the registered
// hostnames and leaves the routes in place, rather than the hard 409 a genuine
// second `prox up` gets. It also pins listener refcount correctness: re-register
// twice, deregister once, and the port must close.
func TestIdempotentReRegister_SameLiveIdentitySucceeds(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	s := newProxyServer(t)
	port := freePort(t)
	pid, token := liveIdentity(t)

	req := RegisterRequest{
		ProjectDir: "/projects/live", PID: pid, StartTime: token, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}

	// Initial registration binds the port (refcount 1).
	registerOK(t, s, req)
	require.Equal(t, 1, s.registry.ProjectCount())

	// Re-register twice: each is idempotent, replacing the registration wholesale
	// while keeping the net listener refcount at exactly one.
	registerOK(t, s, req)
	registerOK(t, s, req)

	require.Equal(t, 1, s.registry.ProjectCount(), "idempotent re-register must not duplicate the project")
	route, ok := s.registry.Lookup("api.local.dev", port)
	require.True(t, ok, "route must survive idempotent re-register")
	assert.Equal(t, "/projects/live", route.ProjectDir)

	// The rebound listener must accept connections after the re-registers.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "idempotent re-register must leave a routable listener")
	_ = conn.Close()

	// A SINGLE deregister must close the port — proof the two re-registers did not
	// leak the listener refcount to two.
	s.removeProject("/projects/live")
	require.Equal(t, 0, s.registry.ProjectCount())
	_, ok = s.registry.Lookup("api.local.dev", port)
	assert.False(t, ok, "route must be gone after one deregister")
	assert.NotContains(t, s.registry.ListenerPorts(), port, "one deregister must close the port after two re-registers")
	_, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	assert.Error(t, err, "port must be closed after a single deregister")
}

// TestIdempotentReRegister_NoOpRefreshPreservesRecords pins the FIX 1 no-op
// refresh path (D6a): a same-identity re-register whose config is UNCHANGED must
// NOT purge the project's daemon-side records (the destructive remove+add would),
// and the route must keep resolving. This is the real heal case — the SSE stream
// broke but the daemon and registration are intact.
func TestIdempotentReRegister_NoOpRefreshPreservesRecords(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	s := newProxyServer(t)
	port := freePort(t)
	pid, token := liveIdentity(t)

	req := RegisterRequest{
		ProjectDir: "/projects/rec", PID: pid, StartTime: token, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	registerOK(t, s, req)

	// Record a daemon-side request for the project (as the proxy hot path would).
	s.requestManager.Record(proxy.RequestRecord{
		ID:         "rec000000001",
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "http://api.local.dev/health",
		Hostname:   "api.local.dev",
		StatusCode: 200,
		ProjectDir: "/projects/rec",
	})
	before := s.requestManager.Recent(proxy.RequestFilter{ProjectDir: "/projects/rec"})
	require.Len(t, before, 1, "precondition: one record captured for the project")

	// Re-register the SAME identity with the SAME config: a true no-op refresh.
	status, body := s.register(req)
	require.Equal(t, http.StatusOK, status, "no-op refresh must succeed: %v", body)

	// The record must survive (the destructive path would have purged it), and the
	// route must be unchanged.
	after := s.requestManager.Recent(proxy.RequestFilter{ProjectDir: "/projects/rec"})
	require.Len(t, after, 1, "no-op refresh must NOT purge the project's daemon-side records")
	assert.Equal(t, "rec000000001", after[0].ID, "the exact record must survive the no-op refresh")

	route, ok := s.registry.Lookup("api.local.dev", port)
	require.True(t, ok, "route must be unchanged after a no-op refresh")
	assert.Equal(t, "/projects/rec", route.ProjectDir)
}

// TestIdempotentReRegister_SharedPortOtherProjectUnaffected pins that a
// same-identity re-register of one project does not disturb ANOTHER project's
// route sharing the same listener port (D6a). Two projects share one HTTP port on
// different domains; re-registering one must leave the other's route resolvable
// and the shared listener open.
func TestIdempotentReRegister_SharedPortOtherProjectUnaffected(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	s := newProxyServer(t)
	port := freePort(t)
	pid, token := liveIdentity(t)

	reqA := RegisterRequest{
		ProjectDir: "/projects/a", PID: pid, StartTime: token, Version: "test", Domain: "a.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	reqB := RegisterRequest{
		ProjectDir: "/projects/b", PID: pid, StartTime: token, Version: "test", Domain: "b.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	registerOK(t, s, reqA)
	registerOK(t, s, reqB) // shares the same HTTP port (refcount 2)

	// Re-register A (same identity, unchanged config -> no-op refresh).
	status, body := s.register(reqA)
	require.Equal(t, http.StatusOK, status, "same-identity re-register must succeed: %v", body)

	// B's route must still resolve and the shared port must still be open.
	route, ok := s.registry.Lookup("api.b.dev", port)
	require.True(t, ok, "the other project's shared-port route must survive a same-identity re-register")
	assert.Equal(t, "/projects/b", route.ProjectDir)
	assert.Contains(t, s.registry.ListenerPorts(), port, "the shared listener port must stay open")

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "the shared listener must still accept connections")
	_ = conn.Close()
}

// TestIdempotentReRegister_DifferentLivePIDStays409 pins that a same-dir register
// from a DIFFERENT live generation (different PID) is a genuine conflict — 409,
// never an idempotent replace.
func TestIdempotentReRegister_DifferentLivePIDStays409(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(http.ResponseWriter, *http.Request) {})
	s := newProxyServer(t)
	port := freePort(t)
	pid, token := liveIdentity(t)

	holder := RegisterRequest{
		ProjectDir: "/projects/x", PID: pid, StartTime: token, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	registerOK(t, s, holder)

	// A different live PID (this process is alive, so the holder passes the
	// liveness check) with a different identity must be refused.
	other := holder
	other.PID = pid + 1
	other.StartTime = token + 1
	status, body := s.register(other)
	assert.Equal(t, http.StatusConflict, status, "different-identity live holder must stay 409: %v", body)
}

// TestIdempotentReRegister_ZeroTokenSamePIDStays409 pins that a same-PID register
// with a zero start token on either side is NOT idempotent (the generation can't
// be proven identical vs a reused PID) — it stays 409 against a live holder.
func TestIdempotentReRegister_ZeroTokenSamePIDStays409(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(http.ResponseWriter, *http.Request) {})
	s := newProxyServer(t)
	port := freePort(t)
	pid, token := liveIdentity(t)

	// Holder registered with a real (non-zero) token so its liveness check reads
	// as alive.
	holder := RegisterRequest{
		ProjectDir: "/projects/z", PID: pid, StartTime: token, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	registerOK(t, s, holder)

	// Same PID but a zero requester token: a zero token on either side must not
	// match, so this is a genuine conflict.
	reReq := holder
	reReq.StartTime = 0
	status, body := s.register(reReq)
	assert.Equal(t, http.StatusConflict, status, "zero-token same-PID must stay 409: %v", body)
}
