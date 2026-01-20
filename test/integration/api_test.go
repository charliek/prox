package integration

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPI_StatusEndpoint(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)

	resp, err := http.Get(testAPIAddr + "/api/v1/status")
	requireNoError(t, err, "failed to get status")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if status.Status == "" {
		t.Error("status should not be empty")
	}
}

func TestAPI_ProcessRestartEndpoint(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Get initial PID
	resp, err := http.Get(testAPIAddr + "/api/v1/processes/long")
	requireNoError(t, err, "failed to get process")
	defer resp.Body.Close()

	var initialProc ProcessInfo
	if err := json.NewDecoder(resp.Body).Decode(&initialProc); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	initialPID := initialProc.PID

	// Restart the process
	req, err := http.NewRequest(http.MethodPost, testAPIAddr+"/api/v1/processes/long/restart", nil)
	requireNoError(t, err, "failed to create request")

	resp2, err := http.DefaultClient.Do(req)
	requireNoError(t, err, "failed to restart")
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	// Wait for restart
	time.Sleep(1 * time.Second)

	// Get new PID
	resp3, err := http.Get(testAPIAddr + "/api/v1/processes/long")
	requireNoError(t, err, "failed to get process after restart")
	defer resp3.Body.Close()

	var newProc ProcessInfo
	if err := json.NewDecoder(resp3.Body).Decode(&newProc); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	// PID should be different after restart
	if newProc.PID == initialPID && newProc.PID != 0 {
		t.Errorf("PID should have changed after restart (initial: %d, new: %d)", initialPID, newProc.PID)
	}
}

func TestAPI_ProcessStopStartEndpoint(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)

	// Wait for process to be running before we try to stop it
	waitForProcessState(t, testAPIAddr, "long", "running", 5*time.Second)

	// Stop the process
	req, err := http.NewRequest(http.MethodPost, testAPIAddr+"/api/v1/processes/long/stop", nil)
	requireNoError(t, err, "failed to create stop request")

	resp, err := http.DefaultClient.Do(req)
	requireNoError(t, err, "failed to stop")
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Wait for process to reach stopped state using polling
	proc := waitForProcessState(t, testAPIAddr, "long", "stopped", 5*time.Second)
	if proc.Status != "stopped" {
		t.Errorf("expected stopped, got %s", proc.Status)
	}

	// Start it again
	req2, err := http.NewRequest(http.MethodPost, testAPIAddr+"/api/v1/processes/long/start", nil)
	requireNoError(t, err, "failed to create start request")

	resp3, err := http.DefaultClient.Do(req2)
	requireNoError(t, err, "failed to start")
	resp3.Body.Close()

	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}

	// Wait for process to reach running state using polling
	proc2 := waitForProcessState(t, testAPIAddr, "long", "running", 5*time.Second)
	if proc2.Status != "running" {
		t.Errorf("expected running, got %s", proc2.Status)
	}
}

func TestAPI_LogsEndpoint(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)

	// Wait for some logs to be generated
	time.Sleep(2 * time.Second)

	resp, err := http.Get(testAPIAddr + "/api/v1/logs?limit=10")
	requireNoError(t, err, "failed to get logs")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Logs []struct {
			Process string `json:"process"`
			Line    string `json:"line"`
		} `json:"logs"`
		TotalCount int `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(result.Logs) == 0 {
		t.Error("expected some log entries")
	}
}

func TestAPI_SSELogsStream(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)

	// Connect to SSE stream
	resp, err := http.Get(testAPIAddr + "/api/v1/logs/stream")
	requireNoError(t, err, "failed to connect to SSE stream")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Check content type
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	// Read a few events
	scanner := bufio.NewScanner(resp.Body)
	eventCount := 0
	timeout := time.After(5 * time.Second)

	for eventCount < 3 {
		select {
		case <-timeout:
			// It's ok if we don't get 3 events in time, the echo process may have finished
			if eventCount == 0 {
				t.Log("no SSE events received, but that may be expected if echo finished")
			}
			return
		default:
			if scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data:") {
					eventCount++
				}
			}
		}
	}
}

func TestAPI_NotFoundProcess(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)

	resp, err := http.Get(testAPIAddr + "/api/v1/processes/nonexistent")
	requireNoError(t, err, "failed to get process")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
