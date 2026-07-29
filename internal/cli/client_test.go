package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/tui"
)

func TestNewClient(t *testing.T) {
	client := NewClient("http://localhost:5555")

	if client.baseURL != "http://localhost:5555" {
		t.Errorf("expected baseURL 'http://localhost:5555', got %q", client.baseURL)
	}
	if client.httpClient == nil {
		t.Error("expected httpClient to be non-nil")
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	client := NewClient("http://localhost:5555/")

	if client.baseURL != "http://localhost:5555" {
		t.Errorf("expected baseURL without trailing slash, got %q", client.baseURL)
	}
}

// TestNewClient_LifecycleClientTimeout verifies the two-client split: ordinary
// calls use the 30s default client, while the lifecycle calls (start/stop/
// restart) use a dedicated client whose timeout sits above the configured
// stop-budget cap so a legitimately long stop is never aborted by the CLI
// (#35, D2).
func TestNewClient_LifecycleClientTimeout(t *testing.T) {
	client := NewClient("http://localhost:5555")

	if client.httpClient == nil || client.lifecycleClient == nil {
		t.Fatal("expected both httpClient and lifecycleClient to be non-nil")
	}
	if client.httpClient.Timeout != 30*time.Second {
		t.Errorf("expected default client timeout 30s, got %v", client.httpClient.Timeout)
	}
	wantLifecycle := constants.MaxStopTimeout + time.Minute
	if client.lifecycleClient.Timeout != wantLifecycle {
		t.Errorf("expected lifecycle client timeout %v, got %v", wantLifecycle, client.lifecycleClient.Timeout)
	}
	if client.lifecycleClient.Timeout <= client.httpClient.Timeout {
		t.Error("lifecycle client timeout must exceed the default client timeout")
	}
}

// TestClient_LifecycleCallsUseLifecycleClient confirms StartProcess/StopProcess/
// RestartProcess route through the lifecycle client, not the default one, by
// swapping in a marked client and asserting the ordinary calls do not use it.
func TestClient_LifecycleCallsUseLifecycleClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
	}))
	defer server.Close()

	client := NewClient(server.URL)

	// Distinguish the two clients by giving the lifecycle client a sentinel
	// transport that records whether it was used.
	used := false
	client.lifecycleClient = &http.Client{
		Timeout:   constants.MaxStopTimeout + time.Minute,
		Transport: recordingTransport{used: &used},
	}

	if err := client.StopProcess("web"); err != nil {
		t.Fatalf("StopProcess: %v", err)
	}
	if !used {
		t.Error("StopProcess should use the lifecycle client")
	}

	used = false
	if err := client.StartProcess("web"); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if !used {
		t.Error("StartProcess should use the lifecycle client")
	}

	used = false
	if err := client.RestartProcess("web"); err != nil {
		t.Fatalf("RestartProcess: %v", err)
	}
	if !used {
		t.Error("RestartProcess should use the lifecycle client")
	}

	// A non-lifecycle call must NOT use the lifecycle client.
	used = false
	if _, err := client.GetStatus(); err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if used {
		t.Error("GetStatus should use the default client, not the lifecycle client")
	}
}

// recordingTransport marks used=true on every round trip and delegates to the
// default transport.
type recordingTransport struct{ used *bool }

func (rt recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*rt.used = true
	return http.DefaultTransport.RoundTrip(req)
}

func TestClient_GetStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}

		resp := api.StatusResponse{
			Status:        "running",
			UptimeSeconds: 3600,
			ConfigFile:    "prox.yaml",
			APIVersion:    "v1",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	status, err := client.GetStatus()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != "running" {
		t.Errorf("expected Status 'running', got %q", status.Status)
	}
	if status.UptimeSeconds != 3600 {
		t.Errorf("expected UptimeSeconds 3600, got %d", status.UptimeSeconds)
	}
}

func TestClient_GetProcesses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := api.ProcessListResponse{
			Processes: []api.ProcessResponse{
				{Name: "web", Status: "running", PID: 1234},
				{Name: "worker", Status: "stopped", PID: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	processes, err := client.GetProcesses()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(processes.Processes) != 2 {
		t.Errorf("expected 2 processes, got %d", len(processes.Processes))
	}
	if processes.Processes[0].Name != "web" {
		t.Errorf("expected first process 'web', got %q", processes.Processes[0].Name)
	}
}

func TestClient_GetProcess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes/web" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		resp := api.ProcessDetailResponse{
			Name:   "web",
			Status: "running",
			PID:    1234,
			Cmd:    "npm start",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	process, err := client.GetProcess("web")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if process.Name != "web" {
		t.Errorf("expected Name 'web', got %q", process.Name)
	}
	if process.Cmd != "npm start" {
		t.Errorf("expected Cmd 'npm start', got %q", process.Cmd)
	}
}

func TestClient_StartProcess(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes/web/start" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		called = true

		resp := api.SuccessResponse{Success: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	err := client.StartProcess("web")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected server to be called")
	}
}

func TestClient_StopProcess(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes/worker/stop" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		called = true

		resp := api.SuccessResponse{Success: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	err := client.StopProcess("worker")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected server to be called")
	}
}

func TestClient_RestartProcess(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes/api/restart" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		called = true

		resp := api.SuccessResponse{Success: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	err := client.RestartProcess("api")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected server to be called")
	}
}

func TestClient_Shutdown_AsyncLegacy(t *testing.T) {
	var gotWait string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shutdown" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		gotWait = r.URL.Query().Get("wait")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	result, err := client.Shutdown(false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("async shutdown should return a nil result, got %+v", result)
	}
	if gotWait != "" {
		t.Errorf("async shutdown must not set wait, got wait=%q", gotWait)
	}
}

func TestClient_Shutdown_WaitClean(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("wait"); got != "true" {
			t.Errorf("expected wait=true, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ShutdownResponse{
			Success:  true,
			Waited:   true,
			Failures: []api.ShutdownFailureResponse{},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	result, err := client.Shutdown(true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Waited == nil || !*result.Waited {
		t.Fatalf("expected Waited=true, got %+v", result)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected no failures, got %+v", result.Failures)
	}
}

func TestClient_Shutdown_WaitFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("wait"); got != "true" {
			t.Errorf("expected wait=true, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ShutdownResponse{
			Success: false,
			Waited:  true,
			Failures: []api.ShutdownFailureResponse{
				{Process: "web", Error: "process group could not be terminated: web", Code: "PROCESS_GROUP_NOT_REAPED"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	result, err := client.Shutdown(true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.Waited == nil || !*result.Waited {
		t.Fatalf("expected Waited=true, got %+v", result)
	}
	if len(result.Failures) != 1 || result.Failures[0].Process != "web" {
		t.Fatalf("expected one failure for web, got %+v", result.Failures)
	}
	if result.Failures[0].Code != "PROCESS_GROUP_NOT_REAPED" {
		t.Errorf("expected PROCESS_GROUP_NOT_REAPED code, got %q", result.Failures[0].Code)
	}
}

// TestClient_Shutdown_WaitOldDaemon: an old daemon ignores wait=true and returns
// a bare {"success":true}. The absent "waited" field must decode to a nil
// pointer, so the CLI can distinguish it from a real waited response.
func TestClient_Shutdown_WaitOldDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("wait"); got != "true" {
			t.Errorf("expected wait=true, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.SuccessResponse{Success: true})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	result, err := client.Shutdown(true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected a decoded result")
	}
	if result.Waited != nil {
		t.Errorf("old-daemon response must leave Waited nil, got %v", *result.Waited)
	}
}

func TestClient_GetLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Check query params
		if r.URL.Query().Get("process") != "web" {
			t.Errorf("expected process=web, got %q", r.URL.Query().Get("process"))
		}
		if r.URL.Query().Get("lines") != "50" {
			t.Errorf("expected lines=50, got %q", r.URL.Query().Get("lines"))
		}
		if r.URL.Query().Get("pattern") != "error" {
			t.Errorf("expected pattern=error, got %q", r.URL.Query().Get("pattern"))
		}
		if r.URL.Query().Get("regex") != "true" {
			t.Errorf("expected regex=true, got %q", r.URL.Query().Get("regex"))
		}

		resp := api.LogsResponse{
			Logs: []api.LogEntryResponse{
				{Timestamp: "2024-01-01T00:00:00Z", Process: "web", Stream: "stdout", Line: "error occurred"},
			},
			FilteredCount: 1,
			TotalCount:    100,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	logs, err := client.GetLogs(context.Background(), domain.LogParams{
		Process: "web",
		Lines:   50,
		Pattern: "error",
		Regex:   true,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(logs.Logs) != 1 {
		t.Errorf("expected 1 log entry, got %d", len(logs.Logs))
	}
	if logs.FilteredCount != 1 {
		t.Errorf("expected FilteredCount 1, got %d", logs.FilteredCount)
	}
	if logs.TotalCount != 100 {
		t.Errorf("expected TotalCount 100, got %d", logs.TotalCount)
	}
}

func TestClient_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(api.ErrorResponse{
			Error: "process not found",
			Code:  "PROCESS_NOT_FOUND",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.GetProcess("nonexistent")

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "PROCESS_NOT_FOUND: process not found" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestClient_AuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-token" {
			t.Errorf("expected Authorization 'Bearer test-token', got %q", authHeader)
		}

		resp := api.StatusResponse{Status: "running"}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		token:      "test-token",
		httpClient: http.DefaultClient,
	}
	_, err := client.GetStatus()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_NoAuthHeaderWhenNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			t.Errorf("expected no Authorization header, got %q", authHeader)
		}

		resp := api.StatusResponse{Status: "running"}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := &Client{
		baseURL:    server.URL,
		token:      "",
		httpClient: http.DefaultClient,
	}
	_, err := client.GetStatus()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseSSELogEntry_ValidJSON(t *testing.T) {
	data := `{"timestamp":"2024-01-01T12:00:00Z","process":"web","stream":"stdout","line":"hello world"}`

	entry, ok := parseSSELogEntry(data)

	if !ok {
		t.Fatal("expected parsing to succeed")
	}
	if entry.Timestamp != "2024-01-01T12:00:00Z" {
		t.Errorf("expected timestamp '2024-01-01T12:00:00Z', got %q", entry.Timestamp)
	}
	if entry.Process != "web" {
		t.Errorf("expected process 'web', got %q", entry.Process)
	}
	if entry.Stream != "stdout" {
		t.Errorf("expected stream 'stdout', got %q", entry.Stream)
	}
	if entry.Line != "hello world" {
		t.Errorf("expected line 'hello world', got %q", entry.Line)
	}
}

func TestParseSSELogEntry_InvalidJSON(t *testing.T) {
	data := `not valid json`

	_, ok := parseSSELogEntry(data)

	if ok {
		t.Error("expected parsing to fail for invalid JSON")
	}
}

// TestParseSSELogEntry_EmptyObject documents the plan 017 C8 guard: an empty
// object unmarshals into a zero-valued LogEntryResponse without a JSON error
// (unknown/missing fields are simply left at their zero value), but a real
// log entry can never have empty Process AND empty Line AND Seq==0 all at
// once, so this shape is rejected rather than surfaced as a phantom log row.
func TestParseSSELogEntry_EmptyObject(t *testing.T) {
	data := `{}`

	_, ok := parseSSELogEntry(data)

	if ok {
		t.Fatal("expected parsing to reject an empty object")
	}
}

// TestParseSSELogEntry_HandshakePayload asserts the guard specifically
// against the stream handshake event's body (api.HandshakeResponse): its
// only field, stream_id, is not a LogEntryResponse field, so it unmarshals
// the same as an empty object and must be rejected the same way (plan 017
// C8) -- this is what keeps the "event: handshake" frame StreamLogs sends
// from materializing as a phantom empty log row in the attach TUI.
func TestParseSSELogEntry_HandshakePayload(t *testing.T) {
	data := `{"stream_id":"deadbeefcafef00d"}`

	_, ok := parseSSELogEntry(data)

	if ok {
		t.Fatal("expected parsing to reject a handshake-shaped payload")
	}
}

// TestParseSSELogEntry_SeqOnlyRealEntry guards the other side of the fix: a
// real entry with an empty Line (a blank log line is legitimate output) must
// still be accepted as long as Process or Seq is non-zero, so the guard
// cannot be loosened to reject on Line alone.
func TestParseSSELogEntry_SeqOnlyRealEntry(t *testing.T) {
	data := `{"process":"web","stream":"stdout","line":"","seq":7}`

	entry, ok := parseSSELogEntry(data)

	if !ok {
		t.Fatal("expected parsing to succeed for a real entry with an empty line")
	}
	if entry.Seq != 7 {
		t.Errorf("expected seq 7, got %d", entry.Seq)
	}
}

func TestBuildLogQueryParams(t *testing.T) {
	tests := []struct {
		name     string
		params   domain.LogParams
		expected map[string]string
	}{
		{
			name:     "empty params",
			params:   domain.LogParams{},
			expected: map[string]string{},
		},
		{
			name: "process only",
			params: domain.LogParams{
				Process: "web",
			},
			expected: map[string]string{
				"process": "web",
			},
		},
		{
			name: "all params",
			params: domain.LogParams{
				Process: "api",
				Lines:   100,
				Pattern: "error",
				Regex:   true,
			},
			expected: map[string]string{
				"process": "api",
				"lines":   "100",
				"pattern": "error",
				"regex":   "true",
			},
		},
		{
			name: "lines zero not included",
			params: domain.LogParams{
				Process: "web",
				Lines:   0,
			},
			expected: map[string]string{
				"process": "web",
			},
		},
		{
			name: "regex false not included",
			params: domain.LogParams{
				Pattern: "test",
				Regex:   false,
			},
			expected: map[string]string{
				"pattern": "test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := buildLogQueryParams(tt.params)

			// Check expected values are present
			for key, expectedVal := range tt.expected {
				if query.Get(key) != expectedVal {
					t.Errorf("expected %s=%q, got %q", key, expectedVal, query.Get(key))
				}
			}

			// Check no unexpected values
			if len(query) != len(tt.expected) {
				t.Errorf("expected %d params, got %d: %v", len(tt.expected), len(query), query)
			}
		})
	}
}

func TestBuildProxyRequestQueryParams(t *testing.T) {
	sinceFixture := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		params   domain.ProxyRequestParams
		expected map[string]string
	}{
		{
			name:     "empty params",
			params:   domain.ProxyRequestParams{},
			expected: map[string]string{},
		},
		{
			name: "subdomain only",
			params: domain.ProxyRequestParams{
				Subdomain: "api",
			},
			expected: map[string]string{
				"subdomain": "api",
			},
		},
		{
			name: "method only",
			params: domain.ProxyRequestParams{
				Method: "GET",
			},
			expected: map[string]string{
				"method": "GET",
			},
		},
		{
			name: "all params",
			params: domain.ProxyRequestParams{
				Subdomain:   "api",
				Method:      "POST",
				MinStatus:   400,
				MaxStatus:   599,
				Since:       sinceFixture,
				URLContains: "/orders",
				Limit:       50,
			},
			expected: map[string]string{
				"subdomain":    "api",
				"method":       "POST",
				"min_status":   "400",
				"max_status":   "599",
				"since":        sinceFixture.Format(time.RFC3339Nano),
				"url_contains": "/orders",
				"limit":        "50",
			},
		},
		{
			name: "zero values not included",
			params: domain.ProxyRequestParams{
				Subdomain: "api",
				MinStatus: 0,
				Limit:     0,
			},
			expected: map[string]string{
				"subdomain": "api",
			},
		},
		{
			name: "url_contains only",
			params: domain.ProxyRequestParams{
				URLContains: "/api",
			},
			expected: map[string]string{
				"url_contains": "/api",
			},
		},
		{
			name: "since only",
			params: domain.ProxyRequestParams{
				Since: sinceFixture,
			},
			expected: map[string]string{
				"since": sinceFixture.Format(time.RFC3339Nano),
			},
		},
		{
			name: "zero since omitted",
			params: domain.ProxyRequestParams{
				Since: time.Time{},
			},
			expected: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := buildProxyRequestQueryParams(tt.params)

			// Check expected values are present
			for key, expectedVal := range tt.expected {
				if query.Get(key) != expectedVal {
					t.Errorf("expected %s=%q, got %q", key, expectedVal, query.Get(key))
				}
			}

			// Check no unexpected values
			if len(query) != len(tt.expected) {
				t.Errorf("expected %d params, got %d: %v", len(tt.expected), len(query), query)
			}
		})
	}
}

func TestClient_GetProxyRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/proxy/requests" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Check query params
		if r.URL.Query().Get("subdomain") != "api" {
			t.Errorf("expected subdomain=api, got %q", r.URL.Query().Get("subdomain"))
		}
		if r.URL.Query().Get("method") != "GET" {
			t.Errorf("expected method=GET, got %q", r.URL.Query().Get("method"))
		}
		if r.URL.Query().Get("min_status") != "400" {
			t.Errorf("expected min_status=400, got %q", r.URL.Query().Get("min_status"))
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("expected limit=50, got %q", r.URL.Query().Get("limit"))
		}

		resp := api.ProxyRequestsResponse{
			Requests: []api.ProxyRequestResponse{
				{
					ID:         "a1b2c3d",
					Timestamp:  "2024-01-01T00:00:00Z",
					Method:     "GET",
					URL:        "/api/users",
					Subdomain:  "api",
					StatusCode: 404,
					DurationMs: 45,
					RemoteAddr: "127.0.0.1",
				},
			},
			FilteredCount: 1,
			TotalCount:    100,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	resp, err := client.GetProxyRequests(context.Background(), domain.ProxyRequestParams{
		Subdomain: "api",
		Method:    "GET",
		MinStatus: 400,
		Limit:     50,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Requests) != 1 {
		t.Errorf("expected 1 request, got %d", len(resp.Requests))
	}
	if resp.Requests[0].ID != "a1b2c3d" {
		t.Errorf("expected ID 'a1b2c3d', got %q", resp.Requests[0].ID)
	}
	if resp.FilteredCount != 1 {
		t.Errorf("expected FilteredCount 1, got %d", resp.FilteredCount)
	}
	if resp.TotalCount != 100 {
		t.Errorf("expected TotalCount 100, got %d", resp.TotalCount)
	}
}

func TestParseSSEProxyRequest_ValidJSON(t *testing.T) {
	data := `{"id":"a1b2c3d","timestamp":"2024-01-01T12:00:00Z","method":"GET","url":"/api/users","subdomain":"api","status_code":200,"duration_ms":45,"remote_addr":"127.0.0.1"}`

	req, ok := parseSSEProxyRequest(data)

	if !ok {
		t.Fatal("expected parsing to succeed")
	}
	if req.ID != "a1b2c3d" {
		t.Errorf("expected ID 'a1b2c3d', got %q", req.ID)
	}
	if req.Method != "GET" {
		t.Errorf("expected method 'GET', got %q", req.Method)
	}
	if req.Subdomain != "api" {
		t.Errorf("expected subdomain 'api', got %q", req.Subdomain)
	}
	if req.StatusCode != 200 {
		t.Errorf("expected status_code 200, got %d", req.StatusCode)
	}
}

func TestParseSSEProxyRequest_InvalidJSON(t *testing.T) {
	data := `not valid json`

	_, ok := parseSSEProxyRequest(data)

	if ok {
		t.Error("expected parsing to fail for invalid JSON")
	}
}

func TestClient_StreamLogsChannel_QueryParams(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/logs/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		receivedQuery = r.URL.RawQuery

		// Send headers for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		// Send one log entry then close
		flusher, ok := w.(http.Flusher)
		if ok {
			w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"test\"}\n\n"))
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.StreamLogsChannel(context.Background(), domain.LogParams{
		Process: "web",
		Lines:   50,
		Pattern: "error",
		Regex:   true,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check query params were sent correctly
	if receivedQuery == "" {
		t.Fatal("expected query params to be sent")
	}
	if !strings.Contains(receivedQuery, "process=web") {
		t.Errorf("expected process=web in query, got %s", receivedQuery)
	}
	if !strings.Contains(receivedQuery, "lines=50") {
		t.Errorf("expected lines=50 in query, got %s", receivedQuery)
	}
	if !strings.Contains(receivedQuery, "pattern=error") {
		t.Errorf("expected pattern=error in query, got %s", receivedQuery)
	}
	if !strings.Contains(receivedQuery, "regex=true") {
		t.Errorf("expected regex=true in query, got %s", receivedQuery)
	}
}

// TestClient_APIError pins the typed error surfaced for every non-2xx response:
// callers can errors.As it for the status/code, and the rendered text still
// matches the human-readable format the CLI printed before APIError existed.
func TestClient_APIError(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantCode    string
		wantMessage string
		wantText    string
	}{
		{
			name:     "unauthorized without body",
			status:   http.StatusUnauthorized,
			wantText: "authentication failed: invalid or missing token",
		},
		{
			name:     "forbidden without body",
			status:   http.StatusForbidden,
			wantText: "access denied: insufficient permissions",
		},
		{
			name:        "message without code renders bare message",
			status:      http.StatusServiceUnavailable,
			contentType: "application/json",
			body:        `{"error":"proxy is not enabled"}`,
			wantMessage: "proxy is not enabled",
			wantText:    "proxy is not enabled",
		},
		{
			name:        "not found with error body",
			status:      http.StatusNotFound,
			contentType: "application/json",
			body:        `{"error":"process not found","code":"PROCESS_NOT_FOUND"}`,
			wantCode:    "PROCESS_NOT_FOUND",
			wantMessage: "process not found",
			wantText:    "PROCESS_NOT_FOUND: process not found",
		},
		{
			name:        "service unavailable with proxy code",
			status:      http.StatusServiceUnavailable,
			contentType: "application/json",
			body:        `{"error":"proxy is not enabled","code":"PROXY_NOT_ENABLED"}`,
			wantCode:    "PROXY_NOT_ENABLED",
			wantMessage: "proxy is not enabled",
			wantText:    "PROXY_NOT_ENABLED: proxy is not enabled",
		},
		{
			name:        "internal error with unparseable body",
			status:      http.StatusInternalServerError,
			contentType: "text/html",
			body:        "<html>boom</html>",
			wantText:    "server error: the prox daemon encountered an internal error",
		},
		{
			name:     "unmapped status",
			status:   http.StatusTeapot,
			wantText: "request failed with status 418",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.WriteHeader(tt.status)
				if tt.body != "" {
					w.Write([]byte(tt.body))
				}
			}))
			defer server.Close()

			client := NewClient(server.URL)
			_, err := client.GetStatus()
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError, got %T: %v", err, err)
			}
			if apiErr.Status != tt.status {
				t.Errorf("expected Status %d, got %d", tt.status, apiErr.Status)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("expected Code %q, got %q", tt.wantCode, apiErr.Code)
			}
			if apiErr.Message != tt.wantMessage {
				t.Errorf("expected Message %q, got %q", tt.wantMessage, apiErr.Message)
			}
			if err.Error() != tt.wantText {
				t.Errorf("expected error text %q, got %q", tt.wantText, err.Error())
			}
		})
	}
}

// proxyNotEnabledServer answers every request with the 503 the daemon sends
// when it is running without the proxy.
func proxyNotEnabledServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(api.ErrorResponse{
			Error: "proxy is not enabled",
			Code:  domain.ErrCodeProxyNotEnabled,
		})
	}))
}

// requireProxyNotEnabledError asserts err is the discriminable connect failure
// the TUI's requests reconnect policy parks on: an *APIError carrying both the
// 503 status and the machine-readable code.
func requireProxyNotEnabledError(t *testing.T, err error) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != domain.ErrCodeProxyNotEnabled {
		t.Errorf("expected 503/%s, got %d/%s", domain.ErrCodeProxyNotEnabled, apiErr.Status, apiErr.Code)
	}
}

// TestClient_StreamProxyRequestsChannel_ProxyNotEnabled proves the channel form
// surfaces the 503 synchronously, at connect time.
func TestClient_StreamProxyRequestsChannel_ProxyNotEnabled(t *testing.T) {
	server := proxyNotEnabledServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.StreamProxyRequestsChannel(context.Background(), domain.ProxyRequestParams{})
	requireProxyNotEnabledError(t, err)
}

// TestAPIError_SatisfiesTUIStatusError pins the structural contract the TUI's
// reconnect policies classify on: internal/tui cannot import internal/cli, so
// it matches *APIError through this interface.
func TestAPIError_SatisfiesTUIStatusError(t *testing.T) {
	var statusErr tui.APIStatusError = &APIError{
		Status: http.StatusServiceUnavailable,
		Code:   domain.ErrCodeProxyNotEnabled,
	}
	if statusErr.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("expected StatusCode 503, got %d", statusErr.StatusCode())
	}
	if statusErr.ErrorCode() != domain.ErrCodeProxyNotEnabled {
		t.Errorf("expected ErrorCode %q, got %q", domain.ErrCodeProxyNotEnabled, statusErr.ErrorCode())
	}
}

// TestClient_ConsumeProxyRequests_ProxyNotEnabled proves the attempt form
// returns the same discriminable error rather than swallowing it.
func TestClient_ConsumeProxyRequests_ProxyNotEnabled(t *testing.T) {
	server := proxyNotEnabledServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	err := client.ConsumeProxyRequests(context.Background(), domain.ProxyRequestParams{},
		func() { t.Error("onConnect must not fire for a failed dial") },
		func(api.ProxyRequestResponse) { t.Error("no event should be delivered") })
	requireProxyNotEnabledError(t, err)
}

// TestClient_StreamLogsChannel_RejectsNonEventStream fails the connect when the
// daemon answers 200 with something that is not an SSE stream.
func TestClient_StreamLogsChannel_RejectsNonEventStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html>not a stream</html>"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.StreamLogsChannel(context.Background(), domain.LogParams{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "content type") || !strings.Contains(err.Error(), "text/event-stream") {
		t.Errorf("expected descriptive content-type error, got %q", err.Error())
	}
}

// TestDialSSE_AcceptsContentTypeParameters accepts the media type with
// parameters appended, which is what net/http writes by default.
func TestDialSSE_AcceptsContentTypeParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	s, err := dialSSE(context.Background(), client, logsStreamPath(domain.LogParams{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s.close()
}

// sseHangupServer writes one log event and then returns, ending the stream.
// TestParseSSEProcessList_ValidSnapshot pins the processes-stream parser: a
// full snapshot decodes field for field.
func TestParseSSEProcessList_ValidSnapshot(t *testing.T) {
	resp, ok := parseSSEProcessList(`{"processes":[{"name":"web","status":"running","pid":42,"restarts":1,"health":"healthy","kind":"process"}]}`)
	if !ok {
		t.Fatal("expected a valid snapshot to parse")
	}
	if len(resp.Processes) != 1 {
		t.Fatalf("expected 1 process, got %d", len(resp.Processes))
	}
	if resp.Processes[0].Name != "web" || resp.Processes[0].PID != 42 {
		t.Errorf("unexpected process %+v", resp.Processes[0])
	}
}

// TestParseSSEProcessList_EmptyIsValid pins that an empty list is a real
// snapshot ("nothing running"), not a frame to drop — unlike the logs parser,
// whose all-zero guard filters non-entry payloads.
func TestParseSSEProcessList_EmptyIsValid(t *testing.T) {
	resp, ok := parseSSEProcessList(`{"processes":[]}`)
	if !ok {
		t.Fatal("an empty process list is a legitimate snapshot")
	}
	if len(resp.Processes) != 0 {
		t.Errorf("expected no processes, got %d", len(resp.Processes))
	}
}

// TestParseSSEProcessList_InvalidJSON drops the frame rather than failing the
// stream.
func TestParseSSEProcessList_InvalidJSON(t *testing.T) {
	if _, ok := parseSSEProcessList("{not json"); ok {
		t.Error("expected malformed JSON to be rejected")
	}
}

// TestClient_ConsumeProcesses_DeliversSnapshots is the attempt-level contract
// the attach TUI's processes loop depends on: onConnect fires once, every data
// frame is delivered as a full snapshot, and the read error that ends the
// stream is returned rather than swallowed.
func TestClient_ConsumeProcesses_DeliversSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processes/stream" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("the processes stream takes no params, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(": connected\n\n"))
		w.Write([]byte("data: {\"processes\":[{\"name\":\"web\",\"status\":\"starting\"}]}\n\n"))
		w.Write([]byte("data: {\"processes\":[{\"name\":\"web\",\"status\":\"running\"}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	connected := 0
	var got []api.ProcessListResponse
	err := client.ConsumeProcesses(context.Background(),
		func() { connected++ },
		func(resp api.ProcessListResponse) { got = append(got, resp) })

	if err == nil {
		t.Fatal("expected the terminal read error when the server ends the stream")
	}
	if connected != 1 {
		t.Errorf("expected onConnect exactly once, got %d", connected)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(got))
	}
	if got[0].Processes[0].Status != "starting" || got[1].Processes[0].Status != "running" {
		t.Errorf("snapshots delivered out of order or mangled: %+v", got)
	}
}

// TestClient_ConsumeProcesses_NotFoundSurfacesStatus pins the version-skew
// signal the TUI classifier keys on: a daemon without the endpoint answers 404,
// and that status must reach the caller discriminably.
func TestClient_ConsumeProcesses_NotFoundSurfacesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	err := client.ConsumeProcesses(context.Background(),
		func() { t.Error("onConnect must not fire for a failed dial") },
		func(api.ProcessListResponse) { t.Error("no event should be delivered") })

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode() != http.StatusNotFound {
		t.Errorf("expected 404, got %d", apiErr.StatusCode())
	}
}

func sseHangupServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(": heartbeat\n\n"))
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"one\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// TestClient_ConsumeLogs_ReturnsTerminalError is the attempt-level contract the
// reconnect loop depends on: a stream that ends surfaces the read error rather
// than disappearing silently, and events delivered before the end are kept.
func TestClient_ConsumeLogs_ReturnsTerminalError(t *testing.T) {
	server := sseHangupServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	var got []api.LogEntryResponse
	connected := false
	err := client.ConsumeLogs(context.Background(), domain.LogParams{},
		func() { connected = true },
		nil, // no handshake hook: this server sends none
		func(entry api.LogEntryResponse) { got = append(got, entry) })

	if err == nil {
		t.Fatal("expected terminal error when the server ends the stream, got nil")
	}
	if !connected {
		t.Error("onConnect must fire once the dial succeeds")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 delivered event, got %d", len(got))
	}
	if got[0].Line != "one" {
		t.Errorf("expected line %q, got %q", "one", got[0].Line)
	}
}

// drainUntilClosed reads ch to completion and returns everything it delivered,
// failing the test with failMsg if the channel is not closed within 5s.
func drainUntilClosed(t *testing.T, ch <-chan api.LogEntryResponse, failMsg string) []api.LogEntryResponse {
	t.Helper()
	var got []api.LogEntryResponse
	deadline := time.After(5 * time.Second)
	for {
		select {
		case entry, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, entry)
		case <-deadline:
			t.Fatal(failMsg)
		}
	}
}

// TestClient_StreamLogsChannel_ClosesOnHangup keeps the existing consumer
// contract: the channel variant only closes, it does not surface the error.
func TestClient_StreamLogsChannel_ClosesOnHangup(t *testing.T) {
	server := sseHangupServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	ch, err := client.StreamLogsChannel(context.Background(), domain.LogParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := drainUntilClosed(t, ch, "channel was not closed after the server ended the stream")
	if len(got) != 1 {
		t.Fatalf("expected 1 event before close, got %d", len(got))
	}
}

// TestClient_StreamLogsChannel_ContextCancel proves cancellation tears the
// stream down immediately -- well inside constants.SSEReadTimeout -- and that
// the reader goroutine exits (it closes the channel on its way out).
func TestClient_StreamLogsChannel_ContextCancel(t *testing.T) {
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"one\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the stream open until the client goes away.
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(server.URL)
	ch, err := client.StreamLogsChannel(ctx, domain.LogParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case entry, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before the first event")
		}
		if entry.Line != "one" {
			t.Fatalf("expected line %q, got %q", "one", entry.Line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first event")
	}

	start := time.Now()
	cancel()

	drainUntilClosed(t, ch, "channel was not closed within 5s of cancellation")

	if elapsed := time.Since(start); elapsed >= constants.SSEReadTimeout {
		t.Errorf("cancellation took %v, expected well under the %v read deadline", elapsed, constants.SSEReadTimeout)
	}

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler still holding the cancelled connection")
	}
}

// sseHandshakeServer writes the exact frame sequence the logs endpoint sends
// (plan 017 C8): the ": connected" comment, a named handshake frame, log
// entries, and — to pin the event-name tracking — a SECOND handshake mid-stream
// followed by one more entry. It then returns, ending the stream.
func sseHandshakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(": connected\n\n"))
		w.Write([]byte("event: handshake\ndata: {\"stream_id\":\"epoch-1\"}\n\n"))
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"one\",\"seq\":1}\n\n"))
		w.Write([]byte(": ping\n\n"))
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"two\",\"seq\":2}\n\n"))
		w.Write([]byte("event: handshake\ndata: {\"stream_id\":\"epoch-1\"}\n\n"))
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"three\",\"seq\":3}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// TestClient_ConsumeLogs_HandshakeReachesHookNotEntryParser pins the C9 reader
// contract: a handshake frame is delivered to the handshake hook and never to
// the entry parser, the entries around it still parse (with their seq), and a
// repeated handshake mid-stream is delivered again — harmless, and the frame
// name must not leak into the entry that follows it.
func TestClient_ConsumeLogs_HandshakeReachesHookNotEntryParser(t *testing.T) {
	server := sseHandshakeServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	var handshakes []api.HandshakeResponse
	var got []api.LogEntryResponse
	err := client.ConsumeLogs(context.Background(), domain.LogParams{},
		nil,
		func(hs api.HandshakeResponse) { handshakes = append(handshakes, hs) },
		func(entry api.LogEntryResponse) { got = append(got, entry) })

	if err == nil {
		t.Fatal("expected terminal error when the server ends the stream, got nil")
	}
	if len(handshakes) != 2 {
		t.Fatalf("expected 2 handshakes delivered, got %d (%v)", len(handshakes), handshakes)
	}
	for i, hs := range handshakes {
		if hs.StreamID != "epoch-1" {
			t.Errorf("handshake %d: expected stream_id %q, got %q", i, "epoch-1", hs.StreamID)
		}
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 log entries, got %d (%v)", len(got), got)
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Line != want {
			t.Errorf("entry %d: expected line %q, got %q", i, want, got[i].Line)
		}
		if got[i].Seq != uint64(i+1) {
			t.Errorf("entry %d: expected seq %d, got %d", i, i+1, got[i].Seq)
		}
	}
}

// TestClient_ConsumeLogs_NilHandshakeHookDropsFrame pins the nilable hook: a
// consumer that does not care about the epoch (the --follow commands, the
// channel form) sees the handshake frame dropped, never a phantom log entry.
func TestClient_ConsumeLogs_NilHandshakeHookDropsFrame(t *testing.T) {
	server := sseHandshakeServer(t)
	defer server.Close()

	client := NewClient(server.URL)
	var got []api.LogEntryResponse
	_ = client.ConsumeLogs(context.Background(), domain.LogParams{}, nil, nil,
		func(entry api.LogEntryResponse) { got = append(got, entry) })

	if len(got) != 3 {
		t.Fatalf("expected 3 log entries with a nil handshake hook, got %d (%v)", len(got), got)
	}
}

// TestClient_GetLogs_SinceSeqAndCursorMetadata covers the resume half of the
// cursor API on the client: since_seq rides the query when set, and the
// response's cursor metadata is decoded for the sync protocol to compare
// against.
func TestClient_GetLogs_SinceSeqAndCursorMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since_seq"); got != "7" {
			t.Errorf("expected since_seq=7, got %q", got)
		}
		if got := r.URL.Query().Get("lines"); got != "1000" {
			t.Errorf("expected lines=1000, got %q", got)
		}
		resp := api.LogsResponse{
			Logs:      []api.LogEntryResponse{{Line: "eight", Seq: 8}},
			StreamID:  "epoch-1",
			OldestSeq: 3,
			LatestSeq: 8,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	logs, err := client.GetLogs(context.Background(), domain.LogParams{Lines: 1000, SinceSeq: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logs.StreamID != "epoch-1" {
		t.Errorf("expected stream_id %q, got %q", "epoch-1", logs.StreamID)
	}
	if logs.OldestSeq != 3 || logs.LatestSeq != 8 {
		t.Errorf("expected bounds 3..8, got %d..%d", logs.OldestSeq, logs.LatestSeq)
	}
	if len(logs.Logs) != 1 || logs.Logs[0].Seq != 8 {
		t.Errorf("expected one entry with seq 8, got %v", logs.Logs)
	}
}

// TestBuildLogQueryParams_OmitsZeroSinceSeq pins the one wire subtlety of the
// cursor: a zero SinceSeq must not be sent, because `since_seq=0` selects the
// server's resume path (oldest-first from the start of the ring) rather than
// the last-`lines` path a cursorless caller wants.
func TestBuildLogQueryParams_OmitsZeroSinceSeq(t *testing.T) {
	query := buildLogQueryParams(domain.LogParams{Lines: 1000})
	if _, present := query["since_seq"]; present {
		t.Errorf("expected no since_seq for a zero cursor, got %q", query.Get("since_seq"))
	}

	query = buildLogQueryParams(domain.LogParams{Lines: 1000, SinceSeq: 1})
	if got := query.Get("since_seq"); got != "1" {
		t.Errorf("expected since_seq=1, got %q", got)
	}
}

// TestClient_ConsumeLogs_DataLineConsumesEventName pins the SSE dispatch rule
// (codex C9 review): one event: field names ONE dispatch. A data line following
// the handshake's data line without its own event: prefix — and before any
// blank-line frame delimiter — is a plain log entry, not a second handshake.
func TestClient_ConsumeLogs_DataLineConsumesEventName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Deliberately NO blank line between the handshake's data and the entry.
		w.Write([]byte("event: handshake\n"))
		w.Write([]byte("data: {\"stream_id\":\"epoch-x\"}\n"))
		w.Write([]byte("data: {\"timestamp\":\"2024-01-01T00:00:00Z\",\"process\":\"web\",\"stream\":\"stdout\",\"line\":\"after-handshake\",\"seq\":7}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var handshakes []api.HandshakeResponse
	var entries []api.LogEntryResponse
	_ = client.ConsumeLogs(context.Background(), domain.LogParams{},
		nil,
		func(hs api.HandshakeResponse) { handshakes = append(handshakes, hs) },
		func(e api.LogEntryResponse) { entries = append(entries, e) })

	if len(handshakes) != 1 || handshakes[0].StreamID != "epoch-x" {
		t.Fatalf("expected exactly one handshake for epoch-x, got %+v", handshakes)
	}
	if len(entries) != 1 || entries[0].Line != "after-handshake" {
		t.Fatalf("expected the follow-on data line to parse as a log entry, got %+v", entries)
	}
}
