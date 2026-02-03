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

func TestRunStatus_JSONOutput(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

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

	apiAddr = server.URL
	statusJSON = true
	defer func() { statusJSON = false }()

	stdout, _ := captureOutput(t, func() {
		runStatus(statusCmd, []string{})
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

func TestRunLogs_FilterParsing(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

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

	apiAddr = server.URL

	// Set flags
	logsProcess = "web"
	logsPattern = "error"
	logsRegex = true
	logsLines = 50
	logsFollow = false
	logsJSON = false
	defer func() {
		logsProcess = ""
		logsPattern = ""
		logsRegex = false
		logsLines = 100
	}()

	captureOutput(t, func() {
		runLogs(logsCmd, []string{})
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

func TestRunLogs_ProcessAsPositionalArg(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

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

	apiAddr = server.URL

	// Reset flags
	logsProcess = ""
	logsPattern = ""
	logsRegex = false
	logsLines = 100
	logsFollow = false
	logsJSON = false

	captureOutput(t, func() {
		runLogs(logsCmd, []string{"web"})
	})

	if receivedProcess != "web" {
		t.Errorf("expected process 'web' from positional arg, got %q", receivedProcess)
	}
}

func TestRunLogs_JSONOutput(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

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

	apiAddr = server.URL

	// Set flags
	logsProcess = ""
	logsPattern = ""
	logsRegex = false
	logsLines = 100
	logsFollow = false
	logsJSON = true
	defer func() { logsJSON = false }()

	stdout, _ := captureOutput(t, func() {
		runLogs(logsCmd, []string{})
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

func TestRunStop_Success(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/shutdown" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	apiAddr = server.URL

	_, _ = captureOutput(t, func() {
		runStop(stopCmd, []string{})
	})

	if !called {
		t.Error("expected shutdown endpoint to be called")
	}
}

func TestRunRestart_Success(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/processes/web/restart" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	apiAddr = server.URL

	_, _ = captureOutput(t, func() {
		runRestart(restartCmd, []string{"web"})
	})

	if !called {
		t.Error("expected restart endpoint to be called")
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

func TestRunRequests_MinStatusValidation(t *testing.T) {
	tests := []struct {
		name        string
		minStatus   int
		expectError bool
	}{
		{"valid min 100", 100, false},
		{"valid min 200", 200, false},
		{"valid min 400", 400, false},
		{"valid min 599", 599, false},
		{"invalid min 0 (treated as no filter)", 0, false},
		{"invalid min 99", 99, true},
		{"invalid min 600", 600, true},
		{"invalid min 1000", 1000, true},
		{"invalid min negative", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore flags
			origMinStatus := requestsMinStatus
			origFollow := requestsFollow
			origJSON := requestsJSON
			defer func() {
				requestsMinStatus = origMinStatus
				requestsFollow = origFollow
				requestsJSON = origJSON
			}()

			requestsMinStatus = tt.minStatus
			requestsFollow = false
			requestsJSON = false

			// For valid cases, we need a server to respond
			if !tt.expectError {
				originalApiAddr := apiAddr
				defer func() { apiAddr = originalApiAddr }()

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(api.ProxyRequestsResponse{
						Requests:      []api.ProxyRequestResponse{},
						FilteredCount: 0,
						TotalCount:    0,
					})
				}))
				defer server.Close()
				apiAddr = server.URL
			}

			_, _ = captureOutput(t, func() {
				err := runRequests(requestsCmd, []string{})
				if tt.expectError {
					if err == nil {
						t.Error("expected error for invalid min-status")
					}
				} else {
					if err != nil {
						t.Errorf("unexpected error: %v", err)
					}
				}
			})
		})
	}
}

func TestDownCmd_NoArgs(t *testing.T) {
	// Verify downCmd has NoArgs validation
	if downCmd.Args == nil {
		t.Error("expected downCmd to have Args validator")
	}

	// Test that args are rejected
	err := downCmd.Args(downCmd, []string{"api"})
	if err == nil {
		t.Error("expected error when passing args to down command")
	}

	// Test that no args is accepted
	err = downCmd.Args(downCmd, []string{})
	if err != nil {
		t.Errorf("unexpected error with no args: %v", err)
	}
}

func TestRunStartProcess_Success(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/processes/web/start" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	apiAddr = server.URL

	_, _ = captureOutput(t, func() {
		runStartProcess(startProcessCmd, []string{"web"})
	})

	if !called {
		t.Error("expected start endpoint to be called")
	}
}

func TestRunStop_StopSingleProcess(t *testing.T) {
	// Save original apiAddr and restore after test
	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/processes/api/stop" && r.Method == "POST" {
			called = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
		}
	}))
	defer server.Close()

	apiAddr = server.URL

	_, _ = captureOutput(t, func() {
		runStop(stopCmd, []string{"api"})
	})

	if !called {
		t.Error("expected stop process endpoint to be called")
	}
}

func TestLogPrinter(t *testing.T) {
	printer := NewLogPrinter()

	// Test that same process gets same color
	color1 := printer.getColor("web")
	color2 := printer.getColor("web")
	if color1 != color2 {
		t.Error("same process name should get same color")
	}

	// Test that different processes get different colors (first two at least)
	color3 := printer.getColor("api")
	if color1 == color3 {
		t.Error("different processes should get different colors initially")
	}

	// Verify colors are from the expected set
	colors := []string{
		"\033[36m", // cyan
		"\033[33m", // yellow
		"\033[32m", // green
		"\033[35m", // magenta
		"\033[34m", // blue
		"\033[31m", // red
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
