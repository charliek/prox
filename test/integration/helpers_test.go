package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain scrubs PROX_TUI from this test process's environment before any test
// runs, so no `prox` subprocess started here can inherit a developer's own
// setting (codex review of plan 026 C7).
//
// PROX_TUI is a documented per-shell knob that outranks terminal capability, and
// nearly every test in this package launches the real binary with an inherited
// environment. An ambient PROX_TUI=1 makes the TUI-default tests pass even
// against a reverted default; an ambient PROX_TUI=0 makes them fail against
// correct code; an ambient garbage value adds a warning line to output several
// tests assert on. Tests that mean to exercise the variable set it explicitly on
// cmd.Env, which is unaffected by this. (The pty helpers filter it out of
// cmd.Env as well, so they do not silently depend on this.)
func TestMain(m *testing.M) {
	_ = os.Unsetenv("PROX_TUI")
	code := m.Run()
	// The shared binary outlives every t.TempDir(), so it is removed here.
	if sharedBinary.dir != "" {
		_ = os.RemoveAll(sharedBinary.dir)
	}
	os.Exit(code)
}

// buildBinary returns the path to a prox binary built once for the whole
// package run.
//
// It used to build into t.TempDir(), i.e. once per test: 61 builds per run,
// each producing a BRAND NEW binary on disk. That is not free even with a warm
// build cache. On macOS the first execution of a freshly written binary costs
// 0.22-0.40s of code-signing/security evaluation, against 0.00s for a binary
// that has been run before (measured, plan 027 C4 gate), and 61 concurrent
// `go build` invocations also contend for the build cache lock while the ~1500
// unit tests in other packages run in parallel.
//
// Sharing one binary does NOT reintroduce the shared-resource problem this
// plan removes: the binary is read-only and identical for every test, whereas
// the resources that made this suite unreliable -- one .prox state directory
// and one API port -- were mutable and contended. The per-test isolation that
// matters lives in proxFixture, not here.
//
// The binary lives in a package-scoped temp dir removed by TestMain after
// m.Run(), since t.TempDir() is per-test and would delete it while other tests
// still need it.
func buildBinary(t *testing.T) string {
	t.Helper()

	sharedBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "prox-integration-bin-")
		if err != nil {
			sharedBinary.err = fmt.Errorf("creating shared binary dir: %w", err)
			return
		}
		sharedBinary.dir = dir

		binary := filepath.Join(dir, "prox")
		cmd := exec.Command("go", "build", "-o", binary, "./cmd/prox")
		cmd.Dir = projectRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			sharedBinary.err = fmt.Errorf("building prox: %w\n%s", err, out)
			return
		}

		// Pay the first-exec cost once, here, rather than letting whichever
		// test happens to run first absorb it inside its own readiness budget.
		if out, err := exec.Command(binary, "--version").CombinedOutput(); err != nil {
			sharedBinary.err = fmt.Errorf("warming prox binary: %w\n%s", err, out)
			return
		}
		sharedBinary.path = binary
	})

	if sharedBinary.err != nil {
		t.Fatalf("failed to build binary: %v", sharedBinary.err)
	}
	return sharedBinary.path
}

// sharedBinary holds the one prox build shared by every test in this package.
var sharedBinary struct {
	once sync.Once
	path string
	dir  string
	err  error
}

// apiReadyTimeout is the standard budget for "has the daemon's API come up
// yet?" across this suite.
//
// It is not a performance assertion — every use polls for something that either
// happens or fails the test — so a generous budget costs only how long a real
// failure takes to report. It was a fixed 10s, which does not survive
// `make test-race`: that builds and runs every package concurrently with race
// instrumentation, so the whole unit suite competes with these integration
// tests for cores and a race-instrumented `prox up` regularly needs more than
// 10s to bind and answer. The failure signature is a wave of "API did not
// become ready within 10s" across unrelated tests, which reads exactly like a
// real regression and is not one. See ptyWaitTimeout in tui_pty_test.go for the
// same problem on the pty side.
const apiReadyTimeout = 20 * time.Second

// pollClient bounds every poll in this file with a per-request deadline.
//
// The naked http.Get these helpers used goes through http.DefaultClient, which
// has NO timeout: a server that accepts the connection and then stalls blocks
// the call indefinitely, so the helper sails past its own budget and the package
// eventually dies on go test's timeout instead of failing one assertion
// (CodeRabbit, PR #106). The per-request budget is deliberately much shorter
// than the surrounding poll loop, since every attempt is retried anyway.
var pollClient = &http.Client{Timeout: pollRequestTimeout}

// pollRequestTimeout caps a single poll. It is a ceiling, not the budget —
// pollGetWithinDeadline shortens it to whatever is left of the caller's own
// deadline.
const pollRequestTimeout = 5 * time.Second

// pollGetWithinDeadline issues one poll bounded by BOTH the per-request ceiling
// and the caller's remaining budget, returning a cancel the caller must run
// after closing the body.
//
// The client timeout alone bounds a stalled call but not the operation: a
// request that starts just before the deadline may still run the full ceiling
// past it, so a helper given a 5s budget could take ~10s and then report a
// timeout "within 5s" (CodeRabbit, PR #106). A timeout message that misstates
// its own budget is exactly the kind of misleading signal that makes these
// failures expensive to diagnose.
func pollGetWithinDeadline(url string, deadline time.Time) (*http.Response, context.CancelFunc, error) {
	budget := min(time.Until(deadline), pollRequestTimeout)
	if budget <= 0 {
		// The budget ran out between the caller's loop check and here. Do not
		// start a request at all: a very short one could still succeed and let
		// the helper report readiness AFTER its deadline (CodeRabbit, PR #106).
		// Returning the error lets the loop's own condition end the wait.
		return nil, nil, context.DeadlineExceeded
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	resp, err := pollClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// waitForAPI waits for the API to be ready
func waitForAPI(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, cancel, err := pollGetWithinDeadline(addr+"/api/v1/status", deadline)
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			cancel()
			if ok {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("API did not become ready within %v", timeout)
}

// syncBuffer is a goroutine-safe bytes.Buffer: the exec copier goroutines
// write while tests poll Output() before the process has exited.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stopProx sends shutdown request to prox via API
func stopProx(t *testing.T, addr string) error {
	req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/shutdown", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// restartProcess sends a POST /api/v1/processes/{name}/restart request and
// returns the HTTP status code and the parsed error response (empty Code/Error
// on success).
func restartProcess(t *testing.T, addr, name string) (int, ErrorResponse) {
	t.Helper()
	return postProcessAction(t, addr, name, "restart")
}

// stopProcess sends a POST /api/v1/processes/{name}/stop request and returns
// the HTTP status code and parsed error response (empty Code/Error on
// success).
func stopProcess(t *testing.T, addr, name string) (int, ErrorResponse) {
	t.Helper()
	return postProcessAction(t, addr, name, "stop")
}

// postProcessAction posts to /api/v1/processes/{name}/{action} (start, stop,
// restart) and returns the status code plus a decoded error body (zero value
// if the response wasn't an error payload).
func postProcessAction(t *testing.T, addr, name, action string) (int, ErrorResponse) {
	t.Helper()

	url := fmt.Sprintf("%s/api/v1/processes/%s/%s", addr, name, action)
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatalf("failed to POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	var errResp ErrorResponse
	if resp.StatusCode != http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
	}
	return resp.StatusCode, errResp
}

// ErrorResponse mirrors internal/api.ErrorResponse for decoding error bodies
// in integration tests without importing the internal/api package.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// projectRoot returns the repo root (two dirs up from test/integration), where
// the daemon and CLI both run so they share one .prox state directory.
func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	return filepath.Join(wd, "..", "..")
}

// waitCmdExit waits for a started command to exit within timeout and returns the
// exit error from Cmd.Wait (nil on a clean exit). On timeout it kills the process
// directly (not via killProx, which would call Cmd.Wait a second time and race
// this goroutine's Wait) and fails the test. Callers at clean-exit sites should
// assert the returned error is nil.
func waitCmdExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatalf("process did not exit within %v", timeout)
		return nil // unreachable; t.Fatalf stops the test
	}
}

// killProx forcefully kills the prox process
func killProx(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// freePortAttempts bounds how many ports registerOnFreePort will try before
// giving up. Losing the reserve/bind race once is plausible; losing it three
// times running means the machine is out of ephemeral ports or something is
// scanning them, and neither is worth retrying through.
const freePortAttempts = 3

// registerOnFreePort reserves an ephemeral port, releases the reservation, and
// calls register(port) -- the call that makes the shared daemon actually bind
// it -- retrying with a fresh port if something else took it in between.
//
// It returns the port that was successfully bound.
func registerOnFreePort(t *testing.T, register func(port int) error) int {
	t.Helper()

	var lastErr error
	for attempt := 1; attempt <= freePortAttempts; attempt++ {
		port, reservation := freePort(t)
		if err := reservation.Close(); err != nil {
			t.Fatalf("release reserved port %d: %v", port, err)
		}
		err := register(port)
		if err == nil {
			return port
		}
		if !isAddrInUse(err) {
			t.Fatalf("register on port %d: %v", port, err)
		}
		lastErr = err
		t.Logf("port %d was taken between reservation and bind (attempt %d/%d): %v",
			port, attempt, freePortAttempts, err)
	}
	t.Fatalf("no free port survived reservation in %d attempts; last error: %v", freePortAttempts, lastErr)
	return 0 // unreachable; t.Fatalf stops the test
}

// isAddrInUse reports whether err is an EADDRINUSE, including when it arrived
// as text: these binds happen inside the proxy daemon and come back over its
// socket as a string, so the typed errno does not survive the trip.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use")
}

// waitForProcessState waits for a process to reach a specific state
func waitForProcessState(t *testing.T, addr, name, expectedStatus string, timeout time.Duration) ProcessInfo {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastStatus string
	for time.Now().Before(deadline) {
		resp, cancel, err := pollGetWithinDeadline(fmt.Sprintf("%s/api/v1/processes/%s", addr, name), deadline)
		if err == nil {
			var proc ProcessInfo
			matched := false
			if err := json.NewDecoder(resp.Body).Decode(&proc); err == nil {
				lastStatus = proc.Status
				matched = proc.Status == expectedStatus
			}
			resp.Body.Close()
			cancel()
			if matched {
				return proc
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("process %s did not reach state %q within %v (last status: %q)", name, expectedStatus, timeout, lastStatus)
	return ProcessInfo{}
}

// requireNoError fails the test if err is not nil
func requireNoError(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// skipShort skips the test if -short flag is provided
func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
}

// waitForStateFile waits for the state file to be created
func waitForStateFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("state file %s not created within %v", path, timeout)
}
