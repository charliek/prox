package integration

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startAPIFixture launches `prox up` against the integration config in a
// private working directory and returns the API address its daemon actually
// bound, once that API answers.
//
// Every test in this file used to share one repo-root .prox/ and the port
// pinned in testdata/configs/integration.yaml, so any overlap between them
// surfaced as "API did not become ready within 20s" in whichever test lost.
// Now each gets its own directory and its own dynamically allocated port; the
// run handle owns the single Cmd.Wait and kills the process at test end.
func startAPIFixture(t *testing.T) string {
	t.Helper()

	binary := buildBinary(t)
	fixture := newFixture(t, "integration")
	run := fixture.Start(t, binary, "up", "-c", fixture.configPath)

	addr := run.Addr()
	waitForAPI(t, addr, apiReadyTimeout)
	return addr
}

func TestAPI_StatusEndpoint(t *testing.T) {
	skipShort(t)

	addr := startAPIFixture(t)

	resp, err := http.Get(addr + "/api/v1/status")
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

	addr := startAPIFixture(t)
	time.Sleep(500 * time.Millisecond)

	// Get initial PID
	resp, err := http.Get(addr + "/api/v1/processes/long")
	requireNoError(t, err, "failed to get process")
	defer resp.Body.Close()

	var initialProc ProcessInfo
	if err := json.NewDecoder(resp.Body).Decode(&initialProc); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	initialPID := initialProc.PID

	// Restart the process
	req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/processes/long/restart", nil)
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
	resp3, err := http.Get(addr + "/api/v1/processes/long")
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

	addr := startAPIFixture(t)

	// Wait for process to be running before we try to stop it
	waitForProcessState(t, addr, "long", "running", 5*time.Second)

	// Stop the process
	req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/processes/long/stop", nil)
	requireNoError(t, err, "failed to create stop request")

	resp, err := http.DefaultClient.Do(req)
	requireNoError(t, err, "failed to stop")
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Wait for process to reach stopped state using polling
	proc := waitForProcessState(t, addr, "long", "stopped", 5*time.Second)
	if proc.Status != "stopped" {
		t.Errorf("expected stopped, got %s", proc.Status)
	}

	// Start it again
	req2, err := http.NewRequest(http.MethodPost, addr+"/api/v1/processes/long/start", nil)
	requireNoError(t, err, "failed to create start request")

	resp3, err := http.DefaultClient.Do(req2)
	requireNoError(t, err, "failed to start")
	resp3.Body.Close()

	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}

	// Wait for process to reach running state using polling
	proc2 := waitForProcessState(t, addr, "long", "running", 5*time.Second)
	if proc2.Status != "running" {
		t.Errorf("expected running, got %s", proc2.Status)
	}
}

func TestAPI_LogsEndpoint(t *testing.T) {
	skipShort(t)

	addr := startAPIFixture(t)

	// Wait for some logs to be generated
	time.Sleep(2 * time.Second)

	resp, err := http.Get(addr + "/api/v1/logs?limit=10")
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

	addr := startAPIFixture(t)

	// Connect to SSE stream
	resp, err := http.Get(addr + "/api/v1/logs/stream")
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

	// Read a few events.
	//
	// The scan runs in a goroutine feeding a channel because bufio.Scanner.Scan
	// BLOCKS. Selecting on a timeout with Scan in the default branch -- which is
	// what this loop used to do -- never observes the timeout at all: the select
	// takes the default arm, blocks inside Scan, and if the stream goes quiet the
	// test hangs until the whole package times out. Closing done on return
	// unblocks the sender; the deferred Body.Close above then ends the scan
	// (deferred calls run LIFO, so done closes first).
	done := make(chan struct{})
	defer close(done)
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-done:
				return
			}
		}
	}()

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
		case line, ok := <-lines:
			if !ok {
				// Stream ended; same tolerance as the timeout arm.
				if eventCount == 0 {
					t.Log("SSE stream ended before 3 events, but that may be expected if echo finished")
				}
				return
			}
			if strings.HasPrefix(line, "data:") {
				eventCount++
			}
		}
	}
}

func TestAPI_NotFoundProcess(t *testing.T) {
	skipShort(t)

	addr := startAPIFixture(t)

	resp, err := http.Get(addr + "/api/v1/processes/nonexistent")
	requireNoError(t, err, "failed to get process")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
