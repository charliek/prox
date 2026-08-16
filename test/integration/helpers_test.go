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
	"testing"
	"time"
)

const testAPIAddr = "http://127.0.0.1:15555"

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
	os.Exit(m.Run())
}

// buildBinary builds the prox binary and returns its path
func buildBinary(t *testing.T) string {
	t.Helper()

	// Get project root (two directories up from test/integration)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	projectRoot := filepath.Join(wd, "..", "..")

	binary := filepath.Join(t.TempDir(), "prox")

	cmd := exec.Command("go", "build", "-o", binary, "./cmd/prox")
	cmd.Dir = projectRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build binary: %v\n%s", err, output)
	}

	return binary
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

// startProx starts the prox binary with the given arguments
func startProx(t *testing.T, binary string, args ...string) *exec.Cmd {
	t.Helper()

	// Get project root
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	projectRoot := filepath.Join(wd, "..", "..")

	cmd := exec.Command(binary, args...)
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}

	return cmd
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

// proxWithOutput holds a prox command and its captured output
type proxWithOutput struct {
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
}

// startProxWithOutput starts prox and captures its stdout/stderr
func startProxWithOutput(t *testing.T, binary string, args ...string) *proxWithOutput {
	t.Helper()

	// Get project root
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	projectRoot := filepath.Join(wd, "..", "..")

	cmd := exec.Command(binary, args...)
	cmd.Dir = projectRoot

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}

	return &proxWithOutput{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
	}
}

// Output returns the combined stdout and stderr
func (p *proxWithOutput) Output() string {
	return p.stdout.String() + p.stderr.String()
}

// waitForOutputContains polls the captured combined output until it contains
// substr, or fails the test after timeout.
func waitForOutputContains(t *testing.T, prox *proxWithOutput, substr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = prox.Output()
		if strings.Contains(last, substr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Final sample: the marker may have arrived during the last sleep.
	if last = prox.Output(); strings.Contains(last, substr) {
		return
	}
	t.Fatalf("output did not contain %q within %v; captured output: %s", substr, timeout, last)
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

// runProx runs a prox subcommand to completion in the repo root and returns its
// combined output and exit code. Used to exercise the real CLI (e.g. `prox stop`)
// rather than poking the API directly.
func runProx(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Dir = projectRoot(t)
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to run prox %v: %v", args, err)
		}
	}
	return string(out), exitCode
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

// configPath returns the path to a test config
func configPath(name string) string {
	return fmt.Sprintf("testdata/configs/%s.yaml", name)
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
