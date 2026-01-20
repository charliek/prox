package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
)

// captureOutput redirects stdout and stderr for testing
func captureOutput(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()

	// Save original stdout/stderr
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	// Create pipes
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	// Run function
	f()

	// Close write ends
	wOut.Close()
	wErr.Close()

	// Read captured output
	var bufOut, bufErr bytes.Buffer
	io.Copy(&bufOut, rOut)
	io.Copy(&bufErr, rErr)

	// Restore
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	return bufOut.String(), bufErr.String()
}

func TestCmdStatus_JSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/status":
			json.NewEncoder(w).Encode(api.StatusResponse{
				Status:        "running",
				UptimeSeconds: 3600,
				ConfigFile:    "prox.yaml",
				APIVersion:    "v1",
			})
		case "/api/v1/processes":
			json.NewEncoder(w).Encode(api.ProcessListResponse{
				Processes: []api.ProcessResponse{
					{Name: "web", Status: "running", PID: 1234, UptimeSeconds: 100},
					{Name: "worker", Status: "stopped", PID: 0},
				},
			})
		}
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	stdout, _ := captureOutput(t, func() {
		code := app.cmdStatus([]string{"--json"})
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	// Parse JSON output
	var output map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}

	status, ok := output["status"].(map[string]interface{})
	if !ok {
		t.Fatal("expected status field in output")
	}
	if status["status"] != "running" {
		t.Errorf("expected status 'running', got %v", status["status"])
	}

	processes, ok := output["processes"].([]interface{})
	if !ok {
		t.Fatal("expected processes field in output")
	}
	if len(processes) != 2 {
		t.Errorf("expected 2 processes, got %d", len(processes))
	}
}

func TestCmdStatus_ConnectionError(t *testing.T) {
	// Use an address that won't respond
	app := &App{apiAddr: "http://127.0.0.1:59999"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdStatus([]string{})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected error message on stderr")
	}
}

func TestCmdLogs_FilterParsing(t *testing.T) {
	var receivedProcess, receivedPattern, receivedRegex, receivedLines string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProcess = r.URL.Query().Get("process")
		receivedPattern = r.URL.Query().Get("pattern")
		receivedRegex = r.URL.Query().Get("regex")
		receivedLines = r.URL.Query().Get("lines")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.LogsResponse{
			Logs:          []api.LogEntryResponse{},
			FilteredCount: 0,
			TotalCount:    0,
		})
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	captureOutput(t, func() {
		app.cmdLogs([]string{
			"--process", "web",
			"--pattern", "error",
			"--regex",
			"-n", "50",
		})
	})

	if receivedProcess != "web" {
		t.Errorf("expected process 'web', got %q", receivedProcess)
	}
	if receivedPattern != "error" {
		t.Errorf("expected pattern 'error', got %q", receivedPattern)
	}
	if receivedRegex != "true" {
		t.Errorf("expected regex 'true', got %q", receivedRegex)
	}
	if receivedLines != "50" {
		t.Errorf("expected lines '50', got %q", receivedLines)
	}
}

func TestCmdLogs_ProcessAsPositionalArg(t *testing.T) {
	var receivedProcess string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProcess = r.URL.Query().Get("process")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.LogsResponse{
			Logs:          []api.LogEntryResponse{},
			FilteredCount: 0,
			TotalCount:    0,
		})
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	captureOutput(t, func() {
		app.cmdLogs([]string{"web"})
	})

	if receivedProcess != "web" {
		t.Errorf("expected process 'web' from positional arg, got %q", receivedProcess)
	}
}

func TestCmdLogs_InvalidLinesValue(t *testing.T) {
	app := &App{apiAddr: "http://localhost:5555"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdLogs([]string{"-n", "invalid"})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected error message on stderr")
	}
}

func TestCmdLogs_JSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.LogsResponse{
			Logs: []api.LogEntryResponse{
				{
					Timestamp: time.Now().Format(time.RFC3339Nano),
					Process:   "web",
					Stream:    "stdout",
					Line:      "test message",
				},
			},
			FilteredCount: 1,
			TotalCount:    1,
		})
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	stdout, _ := captureOutput(t, func() {
		code := app.cmdLogs([]string{"--json"})
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	// Parse JSON output
	var output api.LogsResponse
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}

	if len(output.Logs) != 1 {
		t.Errorf("expected 1 log entry, got %d", len(output.Logs))
	}
}

func TestCmdLogs_ConnectionError(t *testing.T) {
	// Use an address that won't respond
	app := &App{apiAddr: "http://127.0.0.1:59999"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdLogs([]string{})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected error message on stderr")
	}
}

func TestCmdStop_Success(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/shutdown" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	_, _ = captureOutput(t, func() {
		code := app.cmdStop([]string{})
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	if !called {
		t.Error("expected shutdown endpoint to be called")
	}
}

func TestCmdStop_ConnectionError(t *testing.T) {
	app := &App{apiAddr: "http://127.0.0.1:59999"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdStop([]string{})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected error message on stderr")
	}
}

func TestCmdRestart_Success(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/processes/web/restart" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	app := &App{apiAddr: server.URL}

	_, _ = captureOutput(t, func() {
		code := app.cmdRestart([]string{"web"})
		if code != 0 {
			t.Errorf("expected exit code 0, got %d", code)
		}
	})

	if !called {
		t.Error("expected restart endpoint to be called")
	}
}

func TestCmdRestart_NoProcess(t *testing.T) {
	app := &App{apiAddr: "http://localhost:5555"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdRestart([]string{})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected usage error message on stderr")
	}
}

func TestCmdRestart_ConnectionError(t *testing.T) {
	app := &App{apiAddr: "http://127.0.0.1:59999"}

	_, stderr := captureOutput(t, func() {
		code := app.cmdRestart([]string{"web"})
		if code != 1 {
			t.Errorf("expected exit code 1, got %d", code)
		}
	})

	if stderr == "" {
		t.Error("expected error message on stderr")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{90 * time.Second, "1m30s"},
		{3600 * time.Second, "1h0m"},
		{3661 * time.Second, "1h1m"},
		{7200 * time.Second, "2h0m"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatDuration(%v) = %q, expected %q", tt.duration, result, tt.expected)
			}
		})
	}
}

func TestProcessColor(t *testing.T) {
	// Test that different names get colors
	color1 := processColor("web")
	color2 := processColor("api")

	// Same name should get same color
	if processColor("web") != color1 {
		t.Error("same process name should get same color")
	}

	// Different names with different hash should get different colors (most likely)
	// This is probabilistic but "web" and "api" have different hashes
	_ = color2 // Just verify it doesn't panic

	// Verify colors are from the expected set
	colors := []string{
		"\033[36m", // cyan
		"\033[33m", // yellow
		"\033[32m", // green
		"\033[35m", // magenta
		"\033[34m", // blue
	}

	found := false
	for _, c := range colors {
		if color1 == c {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("unexpected color: %q", color1)
	}
}
