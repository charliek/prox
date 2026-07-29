package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
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

// TestWriteError_ReloadSentinels covers the two config-reload error mappings
// added for #33: ErrConfigReloadFailed -> 422 CONFIG_RELOAD_FAILED and
// ErrProcessNotInConfig -> 409 PROCESS_NOT_IN_CONFIG, with the underlying detail
// surfaced in the message.
func TestWriteError_ReloadSentinels(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "config reload failed",
			err:        fmt.Errorf("%w: parsing yaml: bad", domain.ErrConfigReloadFailed),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   domain.ErrCodeConfigReloadFailed,
		},
		{
			name:       "process not in config",
			err:        fmt.Errorf("process %q %w; run 'prox up' to reconcile", "web", domain.ErrProcessNotInConfig),
			wantStatus: http.StatusConflict,
			wantCode:   domain.ErrCodeProcessNotInConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tc.err)

			assert.Equal(t, tc.wantStatus, w.Code)

			var resp ErrorResponse
			require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
			assert.Equal(t, tc.wantCode, resp.Code)
			assert.Equal(t, tc.err.Error(), resp.Error, "detail must be surfaced")
		})
	}
}

func TestGetProxyRequests(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Create request manager and add some test requests
	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	now := time.Now()
	rm.Record(proxy.RequestRecord{
		Timestamp:  now.Add(-2 * time.Minute),
		Method:     "GET",
		URL:        "/api/users",
		Subdomain:  "app",
		StatusCode: 200,
		Duration:   50 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
	})
	rm.Record(proxy.RequestRecord{
		Timestamp:  now.Add(-1 * time.Minute),
		Method:     "POST",
		URL:        "/api/orders",
		Subdomain:  "api",
		StatusCode: 201,
		Duration:   100 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
	})
	rm.Record(proxy.RequestRecord{
		Timestamp:  now,
		Method:     "GET",
		URL:        "/api/products",
		Subdomain:  "app",
		StatusCode: 500,
		Duration:   200 * time.Millisecond,
		RemoteAddr: "192.168.1.1",
	})

	t.Run("get all requests", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 3)
		assert.Equal(t, 3, resp.TotalCount)
		assert.Equal(t, 3, resp.FilteredCount)
	})

	t.Run("filter by subdomain", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?subdomain=app", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 2)
		for _, r := range resp.Requests {
			assert.Equal(t, "app", r.Subdomain)
		}
	})

	t.Run("filter by method", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?method=POST", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 1)
		assert.Equal(t, "POST", resp.Requests[0].Method)
	})

	t.Run("filter by min_status", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?min_status=400", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 1)
		assert.Equal(t, 500, resp.Requests[0].StatusCode)
	})

	t.Run("filter by url_contains", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?url_contains=Orders", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 1)
		assert.Equal(t, "/api/orders", resp.Requests[0].URL)
	})

	t.Run("filter by limit", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?limit=2", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 2)
		assert.Equal(t, 3, resp.TotalCount)
	})

	t.Run("filter by since", func(t *testing.T) {
		// Only get requests from last 90 seconds
		since := now.Add(-90 * time.Second).Format(time.RFC3339)
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?since="+since, nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Len(t, resp.Requests, 2)
	})

	t.Run("plain first page carries next_before_id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?limit=1", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"next_before_id"`)

		var resp ProxyRequestsResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		require.Len(t, resp.Requests, 1)
		assert.NotEmpty(t, resp.NextBeforeID)
	})

	t.Run("before_id pages strictly older records and last page omits cursor", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?limit=2", nil)
		w := httptest.NewRecorder()
		handlers.GetProxyRequests(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var first ProxyRequestsResponse
		require.NoError(t, json.NewDecoder(w.Body).Decode(&first))
		require.Len(t, first.Requests, 2)
		require.NotEmpty(t, first.NextBeforeID, "partial page must carry a cursor")

		req2 := httptest.NewRequest("GET", "/api/v1/proxy/requests?limit=2&before_id="+first.NextBeforeID, nil)
		w2 := httptest.NewRecorder()
		handlers.GetProxyRequests(w2, req2)
		require.Equal(t, http.StatusOK, w2.Code)
		assert.NotContains(t, w2.Body.String(), `"next_before_id"`, "last page omits the field entirely (omitempty)")

		var second ProxyRequestsResponse
		require.NoError(t, json.NewDecoder(w2.Body).Decode(&second))
		require.Len(t, second.Requests, 1)
		assert.Empty(t, second.NextBeforeID, "last page omits the cursor")

		firstIDs := map[string]bool{}
		for _, r := range first.Requests {
			firstIDs[r.ID] = true
		}
		for _, r := range second.Requests {
			assert.False(t, firstIDs[r.ID], "cursor page repeated a record already returned by the first page")
		}
	})
}

func TestGetProxyRequests_CursorGone(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	// Capacity 2: recording a third record evicts "r1" from the ring.
	rm := proxy.NewRequestManager(2)
	handlers.SetRequestManager(rm)
	rm.Record(proxy.RequestRecord{ID: "r1", Method: "GET", URL: "/a"})
	rm.Record(proxy.RequestRecord{ID: "r2", Method: "GET", URL: "/b"})
	rm.Record(proxy.RequestRecord{ID: "r3", Method: "GET", URL: "/c"})

	t.Run("evicted anchor", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?before_id=r1", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusGone, w.Code)
		var resp ErrorResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, domain.ErrCodeCursorGone, resp.Code)
	})

	t.Run("unknown anchor", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests?before_id=does-not-exist", nil)
		w := httptest.NewRecorder()

		handlers.GetProxyRequests(w, req)

		assert.Equal(t, http.StatusGone, w.Code)
		var resp ErrorResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, domain.ErrCodeCursorGone, resp.Code)
	})
}

func TestGetProxyRequests_ProxyNotEnabled(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)
	// Don't set request manager to simulate proxy not enabled

	req := httptest.NewRequest("GET", "/api/v1/proxy/requests", nil)
	w := httptest.NewRecorder()

	handlers.GetProxyRequests(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, domain.ErrCodeProxyNotEnabled, resp.Code)
}

func TestStreamProxyRequests(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	t.Run("SSE headers set correctly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/stream", nil)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()

		// Run handler in goroutine so we can cancel it
		done := make(chan struct{})
		go func() {
			handlers.StreamProxyRequests(w, req)
			close(done)
		}()

		// Give handler time to set headers and write initial event
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done

		assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
		assert.Equal(t, "keep-alive", w.Header().Get("Connection"))
		assert.Equal(t, "no", w.Header().Get("X-Accel-Buffering"))
		assert.Contains(t, w.Body.String(), ": connected")
	})

	t.Run("receives streamed requests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/stream", nil)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			handlers.StreamProxyRequests(w, req)
			close(done)
		}()

		// Wait for handler to start
		time.Sleep(50 * time.Millisecond)

		// Record a new request
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/streamed",
			Subdomain:  "test",
			StatusCode: 200,
			Duration:   10 * time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})

		// Give time for the event to be written
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done

		body := w.Body.String()
		assert.Contains(t, body, ": connected")
		assert.Contains(t, body, "/streamed")
		assert.Contains(t, body, "test")
	})

	t.Run("filters streamed requests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/stream?subdomain=match", nil)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			handlers.StreamProxyRequests(w, req)
			close(done)
		}()

		time.Sleep(50 * time.Millisecond)

		// Record a request that doesn't match the filter
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/nomatch",
			Subdomain:  "other",
			StatusCode: 200,
			Duration:   10 * time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})

		// Record a request that matches
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/matched",
			Subdomain:  "match",
			StatusCode: 200,
			Duration:   10 * time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})

		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done

		body := w.Body.String()
		// Parse SSE events - look for request data lines (not the connected event)
		reader := bufio.NewScanner(strings.NewReader(body))
		var requestDataLines []string
		for reader.Scan() {
			line := reader.Text()
			// Filter for data lines that contain actual request data (have a URL)
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"url":`) {
				requestDataLines = append(requestDataLines, line)
			}
		}

		// Should only have one request data line (the matched request)
		assert.Len(t, requestDataLines, 1)
		assert.Contains(t, requestDataLines[0], "/matched")
		assert.NotContains(t, body, "/nomatch")
	})

	t.Run("filters streamed requests by url_contains", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/stream?url_contains=Orders", nil)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			handlers.StreamProxyRequests(w, req)
			close(done)
		}()

		time.Sleep(50 * time.Millisecond)

		// Record a request that doesn't match the URL substring filter
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/api/products",
			Subdomain:  "app",
			StatusCode: 200,
			Duration:   10 * time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})

		// Record a request that matches (case-insensitive substring)
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/api/orders/123",
			Subdomain:  "app",
			StatusCode: 200,
			Duration:   10 * time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})

		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done

		body := w.Body.String()
		reader := bufio.NewScanner(strings.NewReader(body))
		var requestDataLines []string
		for reader.Scan() {
			line := reader.Text()
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"url":`) {
				requestDataLines = append(requestDataLines, line)
			}
		}

		assert.Len(t, requestDataLines, 1)
		assert.Contains(t, requestDataLines[0], "/api/orders/123")
		assert.NotContains(t, body, "/api/products")
	})

	t.Run("emits in_flight for an in-flight record", func(t *testing.T) {
		// A real server + incremental body read makes this deterministic: the
		// handler writes ": connected" only after subscribing, so once that
		// line is read the push below is guaranteed to be delivered.
		srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProxyRequests))
		defer srv.Close()

		// Bound the reads: if the handler stops emitting, the context ends the
		// request and the blocked ReadString fails instead of hanging the suite.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		require.Contains(t, line, ": connected")

		// Push an in-flight record straight through the manager (the record
		// itself, not the producer path, is under test here).
		rm.Record(proxy.RequestRecord{
			ID:         "inflight1",
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/inflight",
			Subdomain:  "test",
			StatusCode: 200,
			InFlight:   true,
			RemoteAddr: "127.0.0.1",
		})

		var dataLine string
		for {
			l, err := reader.ReadString('\n')
			require.NoError(t, err)
			if strings.HasPrefix(l, "data: ") {
				dataLine = l
				break
			}
		}
		assert.Contains(t, dataLine, "/inflight")
		assert.Contains(t, dataLine, `"in_flight":true`)
	})
}

// TestStreamProxyRequests_Heartbeat mirrors TestStreamLogs_Heartbeat: a real
// httptest.Server with a short injected heartbeat interval, asserting an idle
// stream still emits ": ping" on cadence and that a record published
// mid-stream arrives interleaved with the heartbeats.
func TestStreamProxyRequests_Heartbeat(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)
	handlers.sseHeartbeatInterval = 20 * time.Millisecond

	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProxyRequests))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)

	go func() {
		time.Sleep(4 * handlers.sseHeartbeatInterval)
		rm.Record(proxy.RequestRecord{
			Timestamp:  time.Now(),
			Method:     "GET",
			URL:        "/heartbeat-interleave",
			Subdomain:  "test",
			StatusCode: 200,
			Duration:   time.Millisecond,
			RemoteAddr: "127.0.0.1",
		})
	}()

	// 3 pings within 1s at a 20ms interval: generous 16× margin against CI
	// scheduling, yet a cadence regression to even 500ms/ping cannot pass.
	requireSSEHeartbeats(t, reader, "/heartbeat-interleave", 3, time.Second)
}

// TestStreamProxyRequests_ClientDisconnect_ReturnsHandler covers the teardown
// path with a real connection close, complementing the "SSE headers set
// correctly" context-cancellation subtest in TestStreamProxyRequests: once the
// client closes its side of the connection, the handler's next write must
// fail and it must return, freeing the subscription via its deferred
// Unsubscribe.
func TestStreamProxyRequests_ClientDisconnect_ReturnsHandler(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)
	handlers.sseHeartbeatInterval = 10 * time.Millisecond

	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.StreamProxyRequests(w, r)
		close(done)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	readSSEConnected(t, resp)

	require.NoError(t, resp.Body.Close())

	requireSSEHandlerReturns(t, done, "StreamProxyRequests")
}

func TestStreamProxyRequests_ProxyNotEnabled(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)
	// Don't set request manager

	req := httptest.NewRequest("GET", "/api/v1/proxy/requests/stream", nil)
	w := httptest.NewRecorder()

	handlers.StreamProxyRequests(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, domain.ErrCodeProxyNotEnabled, resp.Code)
}

func TestGetProxyRequest(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	// Create a capture manager for body loading
	captureCfg := &config.CaptureConfig{Enabled: true}
	cm, err := proxy.NewCaptureManager(captureCfg, t.TempDir())
	require.NoError(t, err)
	handlers.SetCaptureManager(cm)

	// Record a request with details (text body)
	rm.Record(proxy.RequestRecord{
		ID:         "abc1234",
		Timestamp:  time.Now(),
		Method:     "POST",
		URL:        "/api/users",
		Subdomain:  "api",
		StatusCode: 201,
		Duration:   50 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
		Details: &proxy.RequestDetails{
			RequestHeaders:  http.Header{"Content-Type": {"application/json"}},
			ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
			RequestBody: &proxy.CapturedBody{
				Size:        13,
				ContentType: "application/json",
				Data:        []byte(`{"name":"Jo"}`),
			},
			ResponseBody: &proxy.CapturedBody{
				Size:        15,
				ContentType: "application/json",
				Data:        []byte(`{"id":1,"ok":1}`),
			},
		},
	})

	// Record a request with binary body
	binaryData := []byte{0xff, 0xfe, 0x00, 0x01}
	rm.Record(proxy.RequestRecord{
		ID:         "bin5678",
		Timestamp:  time.Now(),
		Method:     "POST",
		URL:        "/upload",
		Subdomain:  "app",
		StatusCode: 200,
		Duration:   100 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
		Details: &proxy.RequestDetails{
			RequestHeaders: http.Header{"Content-Type": {"application/octet-stream"}},
			RequestBody: &proxy.CapturedBody{
				Size:        int64(len(binaryData)),
				ContentType: "application/octet-stream",
				IsBinary:    true,
				Data:        binaryData,
			},
		},
	})

	// Record a request without details
	rm.Record(proxy.RequestRecord{
		ID:         "nod9999",
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/health",
		Subdomain:  "api",
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
	})

	t.Run("returns 503 when proxy not enabled", func(t *testing.T) {
		h := NewHandlers(sup, logMgr, "prox.yaml", nil)
		// Don't set request manager

		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/abc1234", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "abc1234")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		h.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		var resp ErrorResponse
		json.NewDecoder(w.Body).Decode(&resp)
		assert.Equal(t, domain.ErrCodeProxyNotEnabled, resp.Code)
	})

	t.Run("returns 400 for missing request ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var resp ErrorResponse
		json.NewDecoder(w.Body).Decode(&resp)
		assert.Equal(t, domain.ErrCodeMissingRequestID, resp.Code)
	})

	t.Run("returns 404 for unknown request ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/unknown", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "unknown")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
		var resp ErrorResponse
		json.NewDecoder(w.Body).Decode(&resp)
		assert.Equal(t, domain.ErrCodeRequestNotFound, resp.Code)
	})

	t.Run("returns basic response without body when include not set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/abc1234", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "abc1234")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Equal(t, "abc1234", resp.ID)
		assert.Equal(t, "POST", resp.Method)
		assert.Equal(t, "/api/users", resp.URL)
		assert.Equal(t, 201, resp.StatusCode)

		// Details should be present but body data should not be included
		require.NotNil(t, resp.Details)
		assert.NotNil(t, resp.Details.RequestHeaders)
		assert.NotNil(t, resp.Details.ResponseHeaders)
		if resp.Details.RequestBody != nil {
			assert.Empty(t, resp.Details.RequestBody.Data)
		}
		if resp.Details.ResponseBody != nil {
			assert.Empty(t, resp.Details.ResponseBody.Data)
		}
	})

	t.Run("returns text body data when include=body", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/abc1234?include=body", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "abc1234")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		require.NotNil(t, resp.Details)
		require.NotNil(t, resp.Details.RequestBody)
		assert.Equal(t, int64(13), resp.Details.RequestBody.Size)
		assert.Equal(t, "application/json", resp.Details.RequestBody.ContentType)
		assert.False(t, resp.Details.RequestBody.IsBinary)
		assert.Equal(t, `{"name":"Jo"}`, resp.Details.RequestBody.Data)

		require.NotNil(t, resp.Details.ResponseBody)
		assert.Equal(t, `{"id":1,"ok":1}`, resp.Details.ResponseBody.Data)
	})

	t.Run("returns base64-encoded binary body when include=body", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/bin5678?include=body", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "bin5678")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		require.NotNil(t, resp.Details)
		require.NotNil(t, resp.Details.RequestBody)
		assert.True(t, resp.Details.RequestBody.IsBinary)

		// Should be base64-encoded
		expected := base64Encode(binaryData)
		assert.Equal(t, expected, resp.Details.RequestBody.Data)
	})

	t.Run("handles request with nil details gracefully", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/nod9999?include=body", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "nod9999")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		assert.Equal(t, "nod9999", resp.ID)
		assert.Nil(t, resp.Details)
	})

	t.Run("includes request and response headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/proxy/requests/abc1234", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "abc1234")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handlers.GetProxyRequest(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp ProxyRequestDetailResponse
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)

		require.NotNil(t, resp.Details)
		assert.Equal(t, []string{"application/json"}, resp.Details.RequestHeaders["Content-Type"])
		assert.Equal(t, []string{"application/json"}, resp.Details.ResponseHeaders["Content-Type"])
	})
}

// fakeShutdownController is a hand-driven ShutdownController for the shutdown
// handler tests: complete() latches the outcome and closes Done().
type fakeShutdownController struct {
	mu           sync.Mutex
	triggered    int
	done         chan struct{}
	completeOnce sync.Once
	outcome      *domain.ProcessStopError
}

func newFakeShutdownController() *fakeShutdownController {
	return &fakeShutdownController{done: make(chan struct{})}
}

func (f *fakeShutdownController) Trigger() {
	f.mu.Lock()
	f.triggered++
	f.mu.Unlock()
}

func (f *fakeShutdownController) triggerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggered
}

func (f *fakeShutdownController) Done() <-chan struct{} { return f.done }

func (f *fakeShutdownController) Outcome() *domain.ProcessStopError { return f.outcome }

// complete latches the outcome and closes Done() first-call-wins, matching the
// production coordinator's contract: a later complete cannot overwrite the stored
// outcome after Done() has closed.
func (f *fakeShutdownController) complete(outcome *domain.ProcessStopError) {
	f.completeOnce.Do(func() {
		f.outcome = outcome
		close(f.done)
	})
}

func TestShutdownHandler_WaitClean(t *testing.T) {
	fake := newFakeShutdownController()
	fake.complete(nil) // clean verdict already latched
	h := NewHandlers(nil, nil, "prox.yaml", fake)

	req := httptest.NewRequest("POST", "/api/v1/shutdown?wait=true", nil)
	w := httptest.NewRecorder()
	h.Shutdown(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.GreaterOrEqual(t, fake.triggerCount(), 1)

	var resp ShutdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.Success)
	assert.True(t, resp.Waited)
	assert.Empty(t, resp.Failures)
}

func TestShutdownHandler_WaitFailures(t *testing.T) {
	fake := newFakeShutdownController()
	fake.complete(&domain.ProcessStopError{
		Failures: []domain.ProcessStopFailure{
			{Name: "web", Err: fmt.Errorf("%w: web", domain.ErrProcessGroupNotReaped)},
		},
	})
	h := NewHandlers(nil, nil, "prox.yaml", fake)

	req := httptest.NewRequest("POST", "/api/v1/shutdown?wait=true", nil)
	w := httptest.NewRecorder()
	h.Shutdown(w, req)

	// 200 even with survivors: the CLI drops structured bodies on non-2xx.
	assert.Equal(t, http.StatusOK, w.Code)

	var resp ShutdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.False(t, resp.Success)
	assert.True(t, resp.Waited)
	require.Len(t, resp.Failures, 1)
	assert.Equal(t, "web", resp.Failures[0].Process)
	assert.Equal(t, domain.ErrCodeProcessGroupNotReaped, resp.Failures[0].Code)
}

// TestShutdownHandler_WaitClientGone: when the request context is canceled before
// the verdict lands, the handler returns without writing a body.
func TestShutdownHandler_WaitClientGone(t *testing.T) {
	fake := newFakeShutdownController() // never completed
	h := NewHandlers(nil, nil, "prox.yaml", fake)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/shutdown?wait=true", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	returned := make(chan struct{})
	go func() {
		h.Shutdown(w, req)
		close(returned)
	}()

	cancel() // client disconnects

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	assert.Equal(t, 1, fake.triggerCount())
	assert.Empty(t, w.Body.String(), "nothing should be written to a dead connection")
}

// TestShutdownHandler_LegacyAsync: without wait=true, the handler acks 200
// immediately and triggers shutdown asynchronously.
func TestShutdownHandler_LegacyAsync(t *testing.T) {
	fake := newFakeShutdownController()
	h := NewHandlers(nil, nil, "prox.yaml", fake)

	req := httptest.NewRequest("POST", "/api/v1/shutdown", nil)
	w := httptest.NewRecorder()
	h.Shutdown(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.Success)

	// Trigger fires from a background goroutine after a short delay.
	require.Eventually(t, func() bool { return fake.triggerCount() >= 1 }, 2*time.Second, 10*time.Millisecond)
}

// TestShutdownHandler_NilControllerAcks: a handler with no coordinator wired
// still acks (used by tests that never exercise the shutdown path).
func TestShutdownHandler_NilControllerAcks(t *testing.T) {
	h := NewHandlers(nil, nil, "prox.yaml", nil)
	req := httptest.NewRequest("POST", "/api/v1/shutdown?wait=true", nil)
	w := httptest.NewRecorder()
	h.Shutdown(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp SuccessResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.Success)
}
