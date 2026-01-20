package integration

import (
	"context"
	"encoding/json"
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
