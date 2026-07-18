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
	"testing"
	"time"
)

const (
	testAPIPort = 15555
	testAPIAddr = "http://127.0.0.1:15555"
)

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

// waitForAPI waits for the API to be ready
func waitForAPI(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr + "/api/v1/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
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

// proxWithOutput holds a prox command and its captured output
type proxWithOutput struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}

	return &proxWithOutput{
		cmd:    cmd,
		stdout: &stdout,
		stderr: &stderr,
	}
}

// Output returns the combined stdout and stderr
func (p *proxWithOutput) Output() string {
	return p.stdout.String() + p.stderr.String()
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

// waitCmdExit waits for a started command to exit within timeout, killing it and
// failing the test on timeout.
func waitCmdExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		killProx(cmd)
		t.Fatalf("process did not exit within %v", timeout)
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
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/processes/%s", addr, name))
		if err == nil {
			var proc ProcessInfo
			if err := json.NewDecoder(resp.Body).Decode(&proc); err == nil {
				lastStatus = proc.Status
				if proc.Status == expectedStatus {
					resp.Body.Close()
					return proc
				}
			}
			resp.Body.Close()
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

// withTimeout runs the test with a timeout
func withTimeout(t *testing.T, timeout time.Duration, f func()) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()

	select {
	case <-done:
		// Test completed
	case <-ctx.Done():
		t.Fatal("test timed out")
	}
}
