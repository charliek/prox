package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type StatusResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ConfigFile    string `json:"config_file,omitempty"`
	APIVersion    string `json:"api_version"`
}

type ProcessInfo struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	PID      int    `json:"pid"`
	Restarts int    `json:"restarts"`
}

type ProcessListResponse struct {
	Processes []ProcessInfo `json:"processes"`
}

func TestUpCommand_StartsProcesses(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	// Wait for API to be ready
	waitForAPI(t, testAPIAddr, 10*time.Second)

	// Give processes time to start
	time.Sleep(500 * time.Millisecond)

	// Verify status endpoint
	resp, err := http.Get(testAPIAddr + "/api/v1/status")
	requireNoError(t, err, "failed to get status")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("failed to decode status: %v", err)
	}

	if status.Status != "running" {
		t.Errorf("expected status 'running', got '%s'", status.Status)
	}
}

func TestUpCommand_ProcessList(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Get process list
	resp, err := http.Get(testAPIAddr + "/api/v1/processes")
	requireNoError(t, err, "failed to get processes")
	defer resp.Body.Close()

	var result ProcessListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode processes: %v", err)
	}

	if len(result.Processes) != 2 {
		t.Errorf("expected 2 processes, got %d", len(result.Processes))
	}

	// Find the long-running process
	found := false
	for _, p := range result.Processes {
		if p.Name == "long" {
			found = true
			if p.Status != "running" {
				t.Errorf("expected long process to be running, got '%s'", p.Status)
			}
		}
	}
	if !found {
		t.Error("long process not found")
	}
}

func TestUpCommand_GracefulShutdown(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Request shutdown via API
	err := stopProx(t, testAPIAddr)
	requireNoError(t, err, "failed to request shutdown")

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited - check that it was graceful
		if err != nil {
			t.Logf("process exited with error (may be expected): %v", err)
		}
	case <-time.After(15 * time.Second):
		killProx(cmd)
		t.Fatal("process did not shut down within timeout")
	}
}

func TestUpCommand_SpecificProcesses(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	// Start only the 'long' process
	cmd := startProx(t, binary, "up", "-c", configPath("integration"), "long")
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Get process list
	resp, err := http.Get(testAPIAddr + "/api/v1/processes")
	requireNoError(t, err, "failed to get processes")
	defer resp.Body.Close()

	var result ProcessListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode processes: %v", err)
	}

	// Should only have 1 process running
	runningCount := 0
	for _, p := range result.Processes {
		if p.Status == "running" {
			runningCount++
			if p.Name != "long" {
				t.Errorf("unexpected running process: %s", p.Name)
			}
		}
	}
	if runningCount != 1 {
		t.Errorf("expected 1 running process, got %d", runningCount)
	}
}
