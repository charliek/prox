package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastRegisterRetryOps returns a registerRetryOps with instant sleeps so tests
// never pay wall-clock waits, mirroring fastSkewOps (version_skew_test.go).
// Individual fields are overridden per test.
func fastRegisterRetryOps() registerRetryOps {
	return registerRetryOps{
		sleep:        func(time.Duration) {},
		now:          time.Now,
		drainTimeout: 10 * time.Second,
		drainPoll:    time.Millisecond,
		retryDelay:   time.Millisecond,
	}
}

// startFakeRegisterDaemon starts an HTTP server on a Unix socket serving POST
// /api/v1/register via registerH, so retryRegisterAfterShutdown's real
// client.Register call exercises proxyd.Client's actual encode/decode and
// readError->DaemonAPIError path end-to-end (only the drain probe and the
// daemon restart are stubbed via registerRetryOps, matching the injectable-ops
// style in version_skew_test.go).
func startFakeRegisterDaemon(t *testing.T, registerH http.HandlerFunc) string {
	t.Helper()

	// Short temp dir: a unix socket path must fit the platform's sun_path
	// limit (~104 bytes on macOS), which t.TempDir() can overflow.
	tmpDir, err := os.MkdirTemp("/tmp", "prox-rr-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	sockPath := filepath.Join(tmpDir, "d.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/register", registerH)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sockPath
}

// writeDaemonError writes the daemon's ErrorResponse JSON shape (server.go),
// so the client-side decode exercises the real readError path.
func writeDaemonError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(proxyd.ErrorResponse{Error: message, Code: code})
}

func shuttingDownHandler(w http.ResponseWriter, _ *http.Request) {
	writeDaemonError(w, http.StatusServiceUnavailable, "SHUTTING_DOWN", "daemon is shutting down; retry to start a fresh daemon")
}

func registerConflictHandler(w http.ResponseWriter, _ *http.Request) {
	writeDaemonError(w, http.StatusConflict, "REGISTRATION_CONFLICT", "project already registered")
}

func registerSuccessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(proxyd.RegisterResponse{Registered: []string{"a.local.dev"}})
}

// TestRetryRegisterAfterShutdown_RecoversAfterDrain pins D4's happy path: the
// old daemon's socket answers /health once more (still draining), then stops
// answering; a fresh daemon starts and the re-register succeeds.
func TestRetryRegisterAfterShutdown_RecoversAfterDrain(t *testing.T) {
	freshSock := startFakeRegisterDaemon(t, registerSuccessHandler)

	ops := fastRegisterRetryOps()
	healthCalls := 0
	ops.healthAnswers = func() bool {
		healthCalls++
		return healthCalls == 1 // answers once (still draining), then gone
	}
	ensureCalls := 0
	ops.ensureRunning = func() (*proxyd.Client, error) {
		ensureCalls++
		return proxyd.NewClient(freshSock), nil
	}

	req := proxyd.RegisterRequest{ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test"}
	client, resp, err := retryRegisterAfterShutdown(req, ops)
	require.NoError(t, err)
	require.NotNil(t, client)
	require.NotNil(t, resp)
	assert.Equal(t, []string{"a.local.dev"}, resp.Registered)
	assert.Equal(t, 1, ensureCalls, "one EnsureRunning is enough when the first start succeeds")
	assert.GreaterOrEqual(t, healthCalls, 2, "must poll until the old daemon's /health stops answering")
}

// TestRetryRegisterAfterShutdown_DoubleShuttingDownFatal pins that a second
// SHUTTING_DOWN from the fresh daemon's register is returned as-is — the
// caller (tryDaemonProxy) treats it as fatal, same as any other unrecovered
// register error, with only one retry layer.
func TestRetryRegisterAfterShutdown_DoubleShuttingDownFatal(t *testing.T) {
	freshSock := startFakeRegisterDaemon(t, shuttingDownHandler)

	ops := fastRegisterRetryOps()
	ops.healthAnswers = func() bool { return false } // old daemon already gone
	ops.ensureRunning = func() (*proxyd.Client, error) {
		return proxyd.NewClient(freshSock), nil
	}

	req := proxyd.RegisterRequest{ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test"}
	client, resp, err := retryRegisterAfterShutdown(req, ops)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Nil(t, resp)
	assert.True(t, isShuttingDownError(err),
		"a second SHUTTING_DOWN is still identifiable by code, even though the caller no longer retries it")
}

// TestRetryRegisterAfterShutdown_DrainTimeoutFatal pins that a socket which
// keeps answering /health past the drain deadline is treated as still busy
// (never force-restarted) — fatal, mirroring the version-skew slow-drain path.
func TestRetryRegisterAfterShutdown_DrainTimeoutFatal(t *testing.T) {
	ops := fastRegisterRetryOps()
	ops.drainTimeout = 0 // deadline already passed -> single probe, then give up
	ops.healthAnswers = func() bool { return true }
	ops.ensureRunning = func() (*proxyd.Client, error) {
		t.Fatal("must not start over a still-draining daemon")
		return nil, nil
	}

	req := proxyd.RegisterRequest{ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test"}
	client, resp, err := retryRegisterAfterShutdown(req, ops)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Nil(t, resp)
}

// TestIsShuttingDownError pins the routing decision in tryDaemonProxy: only a
// *proxyd.DaemonAPIError with Code "SHUTTING_DOWN" triggers the D4 retry.
// Every other register error — a different daemon error code, a wrapped plain
// error, or a connection failure — must fall straight through to the existing
// fatal path unchanged (no retry, no fresh daemon, no re-register).
func TestIsShuttingDownError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"shutting down code", &proxyd.DaemonAPIError{Code: "SHUTTING_DOWN", Message: "shutting down"}, true},
		{"wrapped shutting down code", fmt.Errorf("register: %w", &proxyd.DaemonAPIError{Code: "SHUTTING_DOWN"}), true},
		{"other daemon code", &proxyd.DaemonAPIError{Code: "REGISTRATION_CONFLICT", Message: "conflict"}, false},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isShuttingDownError(tt.err))
		})
	}
}

// TestRetryRegisterAfterShutdown_NonShuttingDownRegisterErrorUnchanged pins
// that when the re-register (against the fresh daemon) fails with a
// non-SHUTTING_DOWN error, retryRegisterAfterShutdown surfaces it unchanged —
// the caller's fatal path is identical to today's unrecovered register
// failure (same error, no special-casing).
func TestRetryRegisterAfterShutdown_NonShuttingDownRegisterErrorUnchanged(t *testing.T) {
	freshSock := startFakeRegisterDaemon(t, registerConflictHandler)

	ops := fastRegisterRetryOps()
	ops.healthAnswers = func() bool { return false }
	ops.ensureRunning = func() (*proxyd.Client, error) {
		return proxyd.NewClient(freshSock), nil
	}

	req := proxyd.RegisterRequest{ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test"}
	client, resp, err := retryRegisterAfterShutdown(req, ops)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Nil(t, resp)
	assert.False(t, isShuttingDownError(err))

	var apiErr *proxyd.DaemonAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "REGISTRATION_CONFLICT", apiErr.Code)
}

// TestRetryRegisterAfterShutdown_TransientHealthMissNotDrain pins the
// two-consecutive-miss drain confirmation (codex C2 review): a single
// transient /health miss on a still-draining daemon must not be read as
// "drained" — the poll keeps waiting through the flap and only confirms
// after two consecutive misses, so the one re-register attempt is not
// burned against the still-live old daemon.
func TestRetryRegisterAfterShutdown_TransientHealthMissNotDrain(t *testing.T) {
	freshSock := startFakeRegisterDaemon(t, registerSuccessHandler)

	ops := fastRegisterRetryOps()
	// answers, transient miss, answers again (still draining), then gone.
	sequence := []bool{true, false, true, false, false}
	healthCalls := 0
	ops.healthAnswers = func() bool {
		if healthCalls < len(sequence) {
			healthCalls++
			return sequence[healthCalls-1]
		}
		return false
	}
	ops.ensureRunning = func() (*proxyd.Client, error) {
		require.GreaterOrEqual(t, healthCalls, len(sequence),
			"a fresh daemon must not start until the drain is confirmed by two consecutive misses")
		return proxyd.NewClient(freshSock), nil
	}

	req := proxyd.RegisterRequest{ProjectDir: "/projects/a", PID: os.Getpid(), Version: "test"}
	client, resp, err := retryRegisterAfterShutdown(req, ops)
	require.NoError(t, err)
	require.NotNil(t, client)
	require.NotNil(t, resp)
	assert.Equal(t, len(sequence), healthCalls)
}
