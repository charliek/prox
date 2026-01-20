package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

func setupTestServer(t *testing.T) (*Server, *supervisor.Supervisor, *logs.Manager, func()) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})

	cfg := &config.Config{
		API: config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{
			"test": {Cmd: "sleep 30"},
		},
	}

	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)

	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	cleanup := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sup.Stop(stopCtx)
		logMgr.Close()
	}

	return server, sup, logMgr, cleanup
}

func TestGetStatus(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "running", resp.Status)
	assert.Equal(t, "v1", resp.APIVersion)
	assert.Equal(t, "prox.yaml", resp.ConfigFile)
}

func TestGetProcesses(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/processes", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp ProcessListResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Len(t, resp.Processes, 1)
	assert.Equal(t, "test", resp.Processes[0].Name)
	assert.Equal(t, "running", resp.Processes[0].Status)
}

func TestGetProcess(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	t.Run("existing process", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/processes/test", nil)
		w := httptest.NewRecorder()

		// Need to set up chi context for URL params
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "test")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.GetProcess(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProcessDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Equal(t, "test", resp.Name)
		assert.Equal(t, "running", resp.Status)
		assert.Equal(t, "sleep 30", resp.Cmd)
	})

	t.Run("nonexistent process", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/processes/nonexistent", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "nonexistent")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.GetProcess(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var resp ErrorResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Equal(t, domain.ErrCodeProcessNotFound, resp.Code)
	})
}

func TestProcessControl(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	t.Run("stop process", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/test/stop", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "test")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.StopProcess(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp SuccessResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.True(t, resp.Success)
	})

	t.Run("start stopped process", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/test/start", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "test")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.StartProcess(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("restart process", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/test/restart", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "test")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.RestartProcess(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestGetLogs(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	// Add some test logs
	for i := 0; i < 10; i++ {
		logMgr.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "web",
			Stream:    domain.StreamStdout,
			Line:      "test line",
		})
	}

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())

	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	t.Run("get all logs", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/logs", nil)
		w := httptest.NewRecorder()

		handlers.GetLogs(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp LogsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Logs, 10)
	})

	t.Run("get logs with limit", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/logs?lines=5", nil)
		w := httptest.NewRecorder()

		handlers.GetLogs(w, req)

		var resp LogsResponse
		json.NewDecoder(w.Body).Decode(&resp)

		assert.Len(t, resp.Logs, 5)
		assert.Equal(t, 10, resp.TotalCount)
	})

	t.Run("filter by process", func(t *testing.T) {
		// Add logs from another process
		logMgr.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "api",
			Stream:    domain.StreamStdout,
			Line:      "api line",
		})

		req := httptest.NewRequest("GET", "/api/v1/logs?process=api", nil)
		w := httptest.NewRecorder()

		handlers.GetLogs(w, req)

		var resp LogsResponse
		json.NewDecoder(w.Body).Decode(&resp)

		assert.Len(t, resp.Logs, 1)
		assert.Equal(t, "api", resp.Logs[0].Process)
	})
}

func TestHealthEndpoint(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestGetLogs_MaxLinesLimit(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Request a huge number of lines
	req := httptest.NewRequest("GET", "/api/v1/logs?lines=999999999", nil)
	w := httptest.NewRecorder()

	handlers.GetLogs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// The request should succeed but be capped at MaxLogLines
}

func TestGetLogs_InvalidLinesParameter(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Request with invalid lines value - should use default
	req := httptest.NewRequest("GET", "/api/v1/logs?lines=invalid", nil)
	w := httptest.NewRecorder()

	handlers.GetLogs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetLogs_NegativeLinesParameter(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Request with negative lines value - should use default
	req := httptest.NewRequest("GET", "/api/v1/logs?lines=-1", nil)
	w := httptest.NewRecorder()

	handlers.GetLogs(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetLogs_InvalidRegexPattern(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Request with invalid regex pattern
	req := httptest.NewRequest("GET", "/api/v1/logs?pattern=[invalid&regex=true", nil)
	w := httptest.NewRecorder()

	handlers.GetLogs(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, domain.ErrCodeInvalidPattern, resp.Code)
}

func TestProcessControl_NotFound(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	t.Run("start nonexistent", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/nonexistent/start", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "nonexistent")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.StartProcess(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)

		var resp ErrorResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, domain.ErrCodeProcessNotFound, resp.Code)
	})

	t.Run("stop nonexistent", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/nonexistent/stop", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "nonexistent")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.StopProcess(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("restart nonexistent", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/nonexistent/restart", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "nonexistent")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.RestartProcess(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestProcessControl_Conflict(t *testing.T) {
	server, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	t.Run("start already running", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/processes/test/start", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "test")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		server.handlers.StartProcess(w, req)

		assert.Equal(t, http.StatusConflict, w.Code)

		var resp ErrorResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, domain.ErrCodeProcessAlreadyRunning, resp.Code)
	})
}
