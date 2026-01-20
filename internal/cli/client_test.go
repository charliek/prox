package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/prox/internal/api"
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

func TestClient_Shutdown(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/shutdown" {
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
	err := client.Shutdown()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected server to be called")
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
	logs, err := client.GetLogs(LogParams{
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
