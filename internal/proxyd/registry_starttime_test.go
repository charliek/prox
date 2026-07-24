package proxyd

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/charliek/prox/internal/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustSelfToken reads the current process's start token, or skips the test
// when the platform can't produce one (the process_other.go stub) — the
// token-identity semantics under test aren't exercisable without a real token
// to compare against.
func mustSelfToken(t *testing.T) int64 {
	t.Helper()
	token, ok := daemon.ProcessStartTime(os.Getpid())
	if !ok {
		t.Skip("process start token unreadable on this platform; token-identity semantics not exercisable")
	}
	return token
}

// selfPIDRequest builds a RegisterRequest for dir on api.local.dev:443, owned
// by the current process and carrying startTime as its stored token (pass 0
// for the bare-PID fallback case).
func selfPIDRequest(dir string, startTime int64) RegisterRequest {
	req := newTestRequest(dir, "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = os.Getpid()
	req.StartTime = startTime
	return req
}

// TestStalePIDs_TokenMismatchIsStale pins the #61 fix: a live PID whose stored
// start token no longer matches the running generation reads as stale (its
// original generation died and the PID was reused), while a live PID stored
// with StartTime==0 (bare-PID fallback) is NOT stale.
func TestStalePIDs_TokenMismatchIsStale(t *testing.T) {
	realToken := mustSelfToken(t)

	reg := NewRegistry()

	// Live PID, but the stored token names a different (dead) generation.
	mismatch := selfPIDRequest("/projects/mismatch", realToken+1)
	_, _, err := reg.Register(mismatch)
	require.NoError(t, err)

	// Live PID with no token captured — bare-PID liveness keeps it alive.
	bare := newTestRequest("/projects/barealive", "other.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 5000}}, 0, 8443)
	bare.PID = os.Getpid()
	bare.StartTime = 0
	_, _, err = reg.Register(bare)
	require.NoError(t, err)

	stale := reg.StalePIDs()
	require.Len(t, stale, 1, "only the token-mismatched project should be stale")
	assert.Equal(t, "/projects/mismatch", stale[0].Dir)
	assert.Equal(t, os.Getpid(), stale[0].PID)
}

// TestReRegister_SameIdentityIsIdempotent pins D6a (supersedes the pre-C6 hard
// 409): a same-dir register whose live holder is the SAME generation (PID=self,
// StartTime=realToken) is an idempotent re-register — it returns 200 with the
// registered hostnames instead of conflicting, so a heal against a live daemon
// whose SSE stream broke can re-register instead of looping on
// REGISTRATION_CONFLICT forever. Only the same process (a heal or a retry) can
// present the same PID+token; two distinct `prox up` invocations always differ.
func TestReRegister_SameIdentityIsIdempotent(t *testing.T) {
	realToken := mustSelfToken(t)

	s := newLifecycleServer()
	req := selfPIDRequest("/projects/live", realToken)
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	status, body := s.register(req)
	require.Equal(t, http.StatusOK, status, "live, same-identity holder must re-register idempotently: %v", body)
	resp, ok := body.(RegisterResponse)
	require.True(t, ok, "idempotent re-register body should be a RegisterResponse")
	assert.Contains(t, resp.Registered, "api.local.dev")
	assert.Equal(t, 1, s.registry.ProjectCount(), "idempotent re-register must not duplicate the project")
}

// TestSelfHeal_TokenMismatchIsReplaced pins the #61 self-heal path: a same-dir
// conflict whose stored holder is (PID=self, StartTime=realToken+1) is treated
// as a crashed generation whose PID was reused, so the re-register replaces it
// and returns 200.
func TestSelfHeal_TokenMismatchIsReplaced(t *testing.T) {
	realToken := mustSelfToken(t)

	s := newLifecycleServer()

	// Stored holder: live PID but a stale token (the reused-PID scenario).
	stored := selfPIDRequest("/projects/reused", realToken+1)
	_, _, err := s.registry.Register(stored)
	require.NoError(t, err)

	// The restart carries the real (current) token for the same live PID.
	reReq := stored
	reReq.StartTime = realToken
	status, body := s.register(reReq)
	require.Equal(t, http.StatusOK, status, "token mismatch must be treated as dead and self-healed: %v", body)

	// The registration now carries the live generation's real token.
	route, ok := s.registry.Lookup("api.local.dev", 443)
	require.True(t, ok, "self-healed route must be registered")
	assert.Equal(t, os.Getpid(), route.PID)
}

// TestRegisterRequest_StartTimeWire pins the JSON contract: a body without
// "start_time" decodes to StartTime==0 (bare-PID fallback), and a zero
// StartTime is omitted on the wire (omitempty), while a nonzero one round-trips.
func TestRegisterRequest_StartTimeWire(t *testing.T) {
	var req RegisterRequest
	require.NoError(t, json.Unmarshal(
		[]byte(`{"project_dir":"/p","pid":1,"domain":"d","services":{}}`), &req))
	assert.Equal(t, int64(0), req.StartTime, "missing start_time must decode to 0")

	zero, err := json.Marshal(RegisterRequest{ProjectDir: "/p", PID: 1})
	require.NoError(t, err)
	assert.NotContains(t, string(zero), "start_time", "zero StartTime must be omitted (omitempty)")

	nonzero, err := json.Marshal(RegisterRequest{ProjectDir: "/p", PID: 1, StartTime: 42})
	require.NoError(t, err)
	assert.Contains(t, string(nonzero), `"start_time":42`, "nonzero StartTime must be serialized")
}
