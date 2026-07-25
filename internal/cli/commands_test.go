package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/daemon"
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

func TestRunRequests_MaxStatusValidation(t *testing.T) {
	tests := []struct {
		name        string
		maxStatus   int
		expectError bool
	}{
		{"valid max 100", 100, false},
		{"valid max 499", 499, false},
		{"valid max 599", 599, false},
		{"zero max (treated as no filter)", 0, false},
		{"invalid max 99", 99, true},
		{"invalid max 600", 600, true},
		{"invalid max negative", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origMaxStatus := requestsMaxStatus
			origFollow := requestsFollow
			origJSON := requestsJSON
			defer func() {
				requestsMaxStatus = origMaxStatus
				requestsFollow = origFollow
				requestsJSON = origJSON
			}()

			requestsMaxStatus = tt.maxStatus
			requestsFollow = false
			requestsJSON = false

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
						t.Error("expected error for invalid max-status")
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

func TestRunRequests_StatusRangeValidation(t *testing.T) {
	origMinStatus := requestsMinStatus
	origMaxStatus := requestsMaxStatus
	origFollow := requestsFollow
	origJSON := requestsJSON
	defer func() {
		requestsMinStatus = origMinStatus
		requestsMaxStatus = origMaxStatus
		requestsFollow = origFollow
		requestsJSON = origJSON
	}()

	requestsMinStatus = 500
	requestsMaxStatus = 400
	requestsFollow = false
	requestsJSON = false

	_, _ = captureOutput(t, func() {
		err := runRequests(requestsCmd, []string{})
		if err == nil {
			t.Error("expected error when --min-status exceeds --max-status")
		}
	})
}

func TestParseSinceFlag(t *testing.T) {
	t.Run("empty value returns zero time", func(t *testing.T) {
		got, err := parseSinceFlag("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("RFC3339 timestamp parsed exactly", func(t *testing.T) {
		got, err := parseSinceFlag("2026-07-18T10:00:00Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})

	t.Run("duration sugar resolves relative to now", func(t *testing.T) {
		before := time.Now()
		got, err := parseSinceFlag("5m")
		after := time.Now()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantEarliest := before.Add(-5 * time.Minute)
		wantLatest := after.Add(-5 * time.Minute)
		if got.Before(wantEarliest) || got.After(wantLatest) {
			t.Errorf("expected time between %v and %v, got %v", wantEarliest, wantLatest, got)
		}
	})

	t.Run("malformed value errors", func(t *testing.T) {
		_, err := parseSinceFlag("not-a-time")
		if err == nil {
			t.Error("expected error for malformed --since value")
		}
	})

	t.Run("negative duration errors", func(t *testing.T) {
		_, err := parseSinceFlag("-5m")
		if err == nil {
			t.Error("expected error for negative --since duration")
		}
	})
}

func TestRunRequests_SinceValidation(t *testing.T) {
	origSince := requestsSince
	origFollow := requestsFollow
	origJSON := requestsJSON
	defer func() {
		requestsSince = origSince
		requestsFollow = origFollow
		requestsJSON = origJSON
	}()

	requestsSince = "not-a-valid-since-value"
	requestsFollow = false
	requestsJSON = false

	_, _ = captureOutput(t, func() {
		err := runRequests(requestsCmd, []string{})
		if err == nil {
			t.Error("expected error for malformed --since flag")
		}
	})
}

func TestRunRequests_URLFilterPassthrough(t *testing.T) {
	origURL := requestsURL
	origFollow := requestsFollow
	origJSON := requestsJSON
	defer func() {
		requestsURL = origURL
		requestsFollow = origFollow
		requestsJSON = origJSON
	}()

	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("url_contains")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ProxyRequestsResponse{
			Requests:      []api.ProxyRequestResponse{},
			FilteredCount: 0,
			TotalCount:    0,
		})
	}))
	defer server.Close()
	apiAddr = server.URL

	requestsURL = "/api"
	requestsFollow = false
	requestsJSON = false

	_, _ = captureOutput(t, func() {
		if err := runRequests(requestsCmd, []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if gotQuery != "/api" {
		t.Errorf("expected url_contains=/api to reach the server, got %q", gotQuery)
	}
}

// TestRunRequests_TableRendersInFlightDuration verifies the list table's
// DURATION column shows "..." for an in-flight row instead of the
// misleading "0ms" (D10).
func TestRunRequests_TableRendersInFlightDuration(t *testing.T) {
	origFollow := requestsFollow
	origJSON := requestsJSON
	defer func() {
		requestsFollow = origFollow
		requestsJSON = origJSON
	}()

	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ProxyRequestsResponse{
			Requests: []api.ProxyRequestResponse{
				{
					ID:         "inflight1",
					Timestamp:  time.Now().Format(time.RFC3339Nano),
					Method:     "GET",
					URL:        "/stream",
					StatusCode: 200,
					DurationMs: 0,
					InFlight:   true,
				},
			},
			FilteredCount: 1,
			TotalCount:    1,
		})
	}))
	defer server.Close()
	apiAddr = server.URL

	requestsFollow = false
	requestsJSON = false

	stdout, _ := captureOutput(t, func() {
		if err := runRequests(requestsCmd, []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "...") {
		t.Errorf("expected DURATION column to render '...' for an in-flight row, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "0ms") {
		t.Errorf("expected no '0ms' duration for an in-flight row, got:\n%s", stdout)
	}
}

// TestRunRequests_TableRendersStale verifies the list table's DURATION
// column shows "stale?" instead of "..." for an in-flight row the server
// reports as stale (D8, #53).
func TestRunRequests_TableRendersStale(t *testing.T) {
	origFollow := requestsFollow
	origJSON := requestsJSON
	defer func() {
		requestsFollow = origFollow
		requestsJSON = origJSON
	}()

	originalApiAddr := apiAddr
	defer func() { apiAddr = originalApiAddr }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ProxyRequestsResponse{
			Requests: []api.ProxyRequestResponse{
				{
					ID:         "stale1",
					Timestamp:  time.Now().Add(-10 * time.Minute).Format(time.RFC3339Nano),
					Method:     "GET",
					URL:        "/stream",
					StatusCode: 200,
					DurationMs: 0,
					InFlight:   true,
					Stale:      true,
				},
			},
			FilteredCount: 1,
			TotalCount:    1,
		})
	}))
	defer server.Close()
	apiAddr = server.URL

	requestsFollow = false
	requestsJSON = false

	stdout, _ := captureOutput(t, func() {
		if err := runRequests(requestsCmd, []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "stale?") {
		t.Errorf("expected DURATION column to render 'stale?' for a stale in-flight row, got:\n%s", stdout)
	}
}

// TestPrintProxyRequest_InFlight verifies follow-mode prints "(in flight)"
// instead of a fake "(0ms)" for an in-flight streamed record (D10).
func TestPrintProxyRequest_InFlight(t *testing.T) {
	req := api.ProxyRequestResponse{
		ID:         "inflight1",
		Timestamp:  time.Now().Format(time.RFC3339Nano),
		Method:     "GET",
		URL:        "/stream",
		StatusCode: 200,
		DurationMs: 0,
		InFlight:   true,
	}

	stdout, _ := captureOutput(t, func() {
		printProxyRequest(req)
	})

	if !strings.Contains(stdout, "(in flight)") {
		t.Errorf("expected '(in flight)' in follow-mode output, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "(0ms)") {
		t.Errorf("expected no '(0ms)' for an in-flight row, got:\n%s", stdout)
	}
}

// TestShowRequestDetail_InFlight verifies the detail view shows
// "(in flight)" for Duration and the in-flight note in place of the
// "capture not enabled" hint when Details is nil because the request is
// still streaming (D10).
func TestShowRequestDetail_InFlight(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ProxyRequestDetailResponse{
			ProxyRequestResponse: api.ProxyRequestResponse{
				ID:         "inflight1",
				Timestamp:  time.Now().Format(time.RFC3339Nano),
				Method:     "GET",
				URL:        "/stream",
				StatusCode: 200,
				DurationMs: 0,
				InFlight:   true,
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	stdout, _ := captureOutput(t, func() {
		if err := showRequestDetail(client, "inflight1", false, false); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "Duration: (in flight)") {
		t.Errorf("expected 'Duration: (in flight)', got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "(request in flight — details arrive on completion)") {
		t.Errorf("expected in-flight details note, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "capture not enabled") {
		t.Errorf("expected the misleading capture-not-enabled hint to be suppressed, got:\n%s", stdout)
	}
}

// TestShowRequestDetail_Stale verifies the detail view indicates staleness
// for a stale in-flight request (D8, #53): the Duration line and the
// in-flight note both call out "stale?" instead of the ordinary in-flight
// wording.
func TestShowRequestDetail_Stale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ProxyRequestDetailResponse{
			ProxyRequestResponse: api.ProxyRequestResponse{
				ID:         "stale1",
				Timestamp:  time.Now().Add(-10 * time.Minute).Format(time.RFC3339Nano),
				Method:     "GET",
				URL:        "/stream",
				StatusCode: 200,
				DurationMs: 0,
				InFlight:   true,
				Stale:      true,
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	stdout, _ := captureOutput(t, func() {
		if err := showRequestDetail(client, "stale1", false, false); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "Duration: (in flight, stale?)") {
		t.Errorf("expected 'Duration: (in flight, stale?)', got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "stale?") {
		t.Errorf("expected a stale note in the output, got:\n%s", stdout)
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

// shutdownStub builds an httptest server that answers POST /api/v1/shutdown with
// the given status + body, so runFullStop's outcome matrix can be driven without
// a live daemon.
func shutdownStub(t *testing.T, status int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shutdown" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

// runFullStopStub spins up a shutdown stub returning status/body, runs
// runFullStop against it with a fresh temp cwd, and returns the captured
// output plus the resulting error. Shared setup for the outcome-matrix tests
// below that don't need to control the server's lifetime themselves (compare
// TestRunFullStop_TransportFailure, which closes the server early).
func runFullStopStub(t *testing.T, status int, body interface{}) (stdout, stderr string, err error) {
	t.Helper()
	server := shutdownStub(t, status, body)
	defer server.Close()

	client := NewClient(server.URL)
	stdout, stderr = captureOutput(t, func() {
		// Temp cwd has no state/PID files, so the daemon-exit wait returns at once
		// regardless of the bound; a short bound keeps the test snappy anyway.
		err = runFullStop(client, t.TempDir(), 2*time.Second)
	})
	return stdout, stderr, err
}

// TestRunFullStop_CleanVerdict: a waited clean response with no failures, and a
// cwd with no state/PID files (so the daemon-exit wait returns immediately),
// yields a nil error (exit 0) and a stopped summary.
func TestRunFullStop_CleanVerdict(t *testing.T) {
	stdout, _, err := runFullStopStub(t, http.StatusOK, api.ShutdownResponse{
		Success: true, Waited: true, Failures: []api.ShutdownFailureResponse{},
	})
	if err != nil {
		t.Fatalf("expected nil error on clean stop, got %v", err)
	}
	if !strings.Contains(stdout, "Stopped") {
		t.Errorf("expected a stopped summary, got %q", stdout)
	}
}

// TestRunFullStop_Survivors: a waited response listing survivors prints one line
// each and returns a non-nil summary error (cobra -> exit 1). The returned error
// must NOT reprint the whole per-process list.
func TestRunFullStop_Survivors(t *testing.T) {
	_, stderr, err := runFullStopStub(t, http.StatusOK, api.ShutdownResponse{
		Success: false, Waited: true,
		Failures: []api.ShutdownFailureResponse{
			{Process: "web", Error: "process group could not be terminated: web", Code: "PROCESS_GROUP_NOT_REAPED"},
			{Process: "worker", Error: "process group could not be terminated: worker", Code: "PROCESS_GROUP_NOT_REAPED"},
		},
	})
	if err == nil {
		t.Fatal("expected a non-nil error when process groups survive")
	}
	if !strings.Contains(stderr, "web:") || !strings.Contains(stderr, "worker:") {
		t.Errorf("expected each survivor printed to stderr, got %q", stderr)
	}
	// The summary error is a short count, not the whole list re-rendered.
	if strings.Contains(err.Error(), "worker:") {
		t.Errorf("summary error should not reprint the per-process list, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "2 process") {
		t.Errorf("expected a survivor count in the summary error, got %q", err.Error())
	}
}

// TestRunFullStop_OldDaemon: a bare {"success":true} (no waited field) is the
// old-daemon fallback -> legacy message, exit 0.
func TestRunFullStop_OldDaemon(t *testing.T) {
	stdout, _, err := runFullStopStub(t, http.StatusOK, api.SuccessResponse{Success: true})
	if err != nil {
		t.Fatalf("old-daemon fallback should exit 0, got %v", err)
	}
	if !strings.Contains(stdout, "Shutdown initiated") {
		t.Errorf("expected legacy message, got %q", stdout)
	}
}

// TestRunFullStop_TransportFailure: a dead server (connection refused) mid-wait
// yields an unknown-outcome error (exit 1).
func TestRunFullStop_TransportFailure(t *testing.T) {
	server := shutdownStub(t, http.StatusOK, api.SuccessResponse{Success: true})
	url := server.URL
	server.Close() // now refuses connections

	client := NewClient(url)
	var err error
	captureOutput(t, func() {
		err = runFullStop(client, t.TempDir(), 2*time.Second)
	})
	if err == nil {
		t.Fatal("expected an error on transport failure")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected an unknown-outcome message, got %q", err.Error())
	}
}

// TestRunFullStop_CleanVerdictPollTimeout: a clean verdict but the daemon's
// state/PID files never disappear within the bounded wait -> the CLI prints a
// Warning to stderr AND returns a non-zero "shutdown incomplete" error (exit 1),
// since the daemon's own teardown was never confirmed (plan 011 D2, #73). The
// state + PID files are pre-created so the poll actually times out under a
// short injected bound.
func TestRunFullStop_CleanVerdictPollTimeout(t *testing.T) {
	server := shutdownStub(t, http.StatusOK, api.ShutdownResponse{
		Success: true, Waited: true, Failures: []api.ShutdownFailureResponse{},
	})
	defer server.Close()

	cwd := t.TempDir()
	if err := daemon.EnsureStateDir(cwd); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}
	// Leave both files in place so waitForDaemonExit never sees them vanish.
	if err := os.WriteFile(daemon.StatePath(cwd), []byte("{}"), 0o600); err != nil {
		t.Fatalf("failed to write state file: %v", err)
	}
	if err := os.WriteFile(daemon.PIDPath(cwd), []byte("1"), 0o600); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	client := NewClient(server.URL)
	var err error
	stdout, stderr := captureOutput(t, func() {
		err = runFullStop(client, cwd, 150*time.Millisecond)
	})
	if err == nil {
		t.Fatal("expected a non-nil error on poll timeout (exit 1)")
	}
	if !strings.Contains(err.Error(), "shutdown incomplete") {
		t.Errorf("expected a shutdown incomplete error, got %q", err.Error())
	}
	if !strings.Contains(stdout, "Stopped processes") {
		t.Errorf("expected a stopped summary on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "Warning:") || !strings.Contains(stderr, "still finishing shutdown") {
		t.Errorf("expected a Warning about the daemon still finishing, got stderr %q", stderr)
	}
}

// statusServerWithProxy starts a fake API server that returns a status response
// carrying the given proxy block (nil = no block) plus a process list, and
// points apiAddr at it for the duration of the test. When no processes are
// supplied it defaults to a single healthy "web" process so existing callers
// keep their original stub; tests exercising crashed/other states pass their
// own list.
func statusServerWithProxy(t *testing.T, proxy *api.ProxyStatusResponse, procs ...api.ProcessResponse) {
	t.Helper()
	originalApiAddr := apiAddr
	t.Cleanup(func() { apiAddr = originalApiAddr })

	if len(procs) == 0 {
		procs = []api.ProcessResponse{
			{Name: "web", Status: "running", PID: 1234, UptimeSeconds: 5, Health: "healthy"},
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status":
			_ = json.NewEncoder(w).Encode(api.StatusResponse{
				Status:        "running",
				UptimeSeconds: 10,
				ConfigFile:    "prox.yaml",
				APIVersion:    "v1",
				Proxy:         proxy,
			})
		case "/api/v1/processes":
			_ = json.NewEncoder(w).Encode(api.ProcessListResponse{
				Processes: procs,
			})
		}
	}))
	t.Cleanup(server.Close)
	apiAddr = server.URL
}

// TestRunStatus_SharedProxyDownExits1 pins D5: when the status block reports a
// shared proxy that is unreachable, runStatus prints the DOWN line and returns a
// non-nil error (exit 1) even though the child process is healthy.
func TestRunStatus_SharedProxyDownExits1(t *testing.T) {
	statusServerWithProxy(t, &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: false,
	})

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr == nil {
		t.Fatal("runStatus returned nil, want a non-nil error (exit 1) when the shared proxy is down")
	}
	if !strings.Contains(stdout, "Proxy: DOWN") {
		t.Errorf("stdout missing the DOWN line; got:\n%s", stdout)
	}
	// The process table must still have printed first.
	if !strings.Contains(stdout, "web") {
		t.Errorf("stdout missing the process table; got:\n%s", stdout)
	}
}

// TestRunStatus_SharedProxyUpNoError pins that a reachable shared proxy renders
// the running line and returns nil (exit 0).
func TestRunStatus_SharedProxyUpNoError(t *testing.T) {
	statusServerWithProxy(t, &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: true,
		DaemonVersion:   "1.2.3",
	})

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr != nil {
		t.Fatalf("runStatus returned %v, want nil when the shared proxy is up", runErr)
	}
	if !strings.Contains(stdout, "Proxy: shared (running, v1.2.3)") {
		t.Errorf("stdout missing the running proxy line; got:\n%s", stdout)
	}
}

// TestRunStatus_SharedProxyDownJSONExits1 pins that JSON mode also exits 1 when
// the shared proxy is down (the block is emitted verbatim; scripts parsing JSON
// still get the failure signal).
func TestRunStatus_SharedProxyDownJSONExits1(t *testing.T) {
	statusServerWithProxy(t, &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: false,
	})
	statusJSON = true
	t.Cleanup(func() { statusJSON = false })

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr == nil {
		t.Fatal("runStatus (JSON) returned nil, want a non-nil error when the shared proxy is down")
	}
	if !strings.Contains(stdout, "\"daemon_reachable\":false") {
		t.Errorf("JSON output missing the proxy block; got:\n%s", stdout)
	}
}

// TestRunStatus_CrashedChildExits1 pins D1 (#72): a crashed child makes
// `prox status` return a non-nil error (exit 1) in table mode and print the
// Crashed line naming the process.
func TestRunStatus_CrashedChildExits1(t *testing.T) {
	statusServerWithProxy(t, nil,
		api.ProcessResponse{Name: "worker", Status: "crashed", PID: 0, Health: "unknown"},
	)

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr == nil {
		t.Fatal("runStatus returned nil, want a non-nil error (exit 1) when a child is crashed")
	}
	if !strings.Contains(stdout, "Crashed: worker") {
		t.Errorf("stdout missing the Crashed line; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "prox logs worker") {
		t.Errorf("stdout missing the logs pointer; got:\n%s", stdout)
	}
}

// TestRunStatus_CrashedChildJSONExits1 pins that JSON mode also exits 1 on a
// crashed child while the payload still carries the per-process status verbatim
// (schema unchanged — scripts parsing JSON still see the crash).
func TestRunStatus_CrashedChildJSONExits1(t *testing.T) {
	statusServerWithProxy(t, nil,
		api.ProcessResponse{Name: "worker", Status: "crashed", PID: 0, Health: "unknown"},
	)
	statusJSON = true
	t.Cleanup(func() { statusJSON = false })

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr == nil {
		t.Fatal("runStatus (JSON) returned nil, want a non-nil error when a child is crashed")
	}
	if !strings.Contains(stdout, "\"name\":\"worker\"") || !strings.Contains(stdout, "\"status\":\"crashed\"") {
		t.Errorf("JSON output missing the crashed process; got:\n%s", stdout)
	}
	// The JSON path prints no extra Crashed line — the payload carries the state.
	if strings.Contains(stdout, "Crashed:") {
		t.Errorf("JSON output should not carry the human Crashed line; got:\n%s", stdout)
	}
}

// TestRunStatus_MultipleCrashedChildren pins that every crashed child is named
// in the Crashed line, in the order the supervisor reported them (no sort).
func TestRunStatus_MultipleCrashedChildren(t *testing.T) {
	statusServerWithProxy(t, nil,
		api.ProcessResponse{Name: "beta", Status: "crashed"},
		api.ProcessResponse{Name: "web", Status: "running", Health: "healthy"},
		api.ProcessResponse{Name: "alpha", Status: "crashed"},
	)

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr == nil {
		t.Fatal("runStatus returned nil, want a non-nil error when children are crashed")
	}
	// Response order, not alphabetical: beta before alpha.
	if !strings.Contains(stdout, "Crashed: beta, alpha") {
		t.Errorf("Crashed line should list both names in response order; got:\n%s", stdout)
	}
}

// TestRunStatus_NonCrashedStatesExit0 pins the exit-0 contract (D1): stopped,
// starting, stopping, and running-but-unhealthy children — none crashed — all
// return nil (exit 0). Health is out of the exit contract.
func TestRunStatus_NonCrashedStatesExit0(t *testing.T) {
	statusServerWithProxy(t, nil,
		api.ProcessResponse{Name: "seeded", Status: "stopped"},
		api.ProcessResponse{Name: "booting", Status: "starting"},
		api.ProcessResponse{Name: "draining", Status: "stopping"},
		api.ProcessResponse{Name: "web", Status: "running", Health: "unhealthy"},
	)

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr != nil {
		t.Fatalf("runStatus returned %v, want nil when no child is crashed", runErr)
	}
	if strings.Contains(stdout, "Crashed:") {
		t.Errorf("no Crashed line expected when nothing crashed; got:\n%s", stdout)
	}
}

// TestRunStatus_CrashedAndProxyDownPrecedence pins that when both a crash and a
// proxy-down hold, both signals print but the proxy sentinel is returned (table
// mode).
func TestRunStatus_CrashedAndProxyDownPrecedence(t *testing.T) {
	statusServerWithProxy(t, &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: false,
	},
		api.ProcessResponse{Name: "worker", Status: "crashed"},
	)

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr != errSharedProxyDown {
		t.Fatalf("runStatus returned %v, want the proxy sentinel (precedence)", runErr)
	}
	if !strings.Contains(stdout, "Proxy: DOWN") {
		t.Errorf("stdout missing the DOWN line; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Crashed: worker") {
		t.Errorf("stdout missing the Crashed line; got:\n%s", stdout)
	}
}

// TestRunStatus_CrashedAndProxyDownPrecedenceJSON pins the same precedence in
// JSON mode: the proxy sentinel is returned and the payload carries the crash.
func TestRunStatus_CrashedAndProxyDownPrecedenceJSON(t *testing.T) {
	statusServerWithProxy(t, &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: false,
	},
		api.ProcessResponse{Name: "worker", Status: "crashed"},
	)
	statusJSON = true
	t.Cleanup(func() { statusJSON = false })

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	if runErr != errSharedProxyDown {
		t.Fatalf("runStatus (JSON) returned %v, want the proxy sentinel (precedence)", runErr)
	}
	if !strings.Contains(stdout, "\"daemon_reachable\":false") {
		t.Errorf("JSON output missing the proxy block; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "\"status\":\"crashed\"") {
		t.Errorf("JSON output missing the crashed process; got:\n%s", stdout)
	}
}
