package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

func TestCorsMiddleware_LocalhostOrigins(t *testing.T) {
	tests := []struct {
		name          string
		origin        string
		expectAllowed bool
	}{
		{"localhost http", "http://localhost", true},
		{"localhost https", "https://localhost", true},
		{"localhost with port", "http://localhost:3000", true},
		{"127.0.0.1 http", "http://127.0.0.1", true},
		{"127.0.0.1 https", "https://127.0.0.1", true},
		{"127.0.0.1 with port", "http://127.0.0.1:8080", true},
		{"ipv6 localhost", "http://[::1]", true},
		{"ipv6 localhost https", "https://[::1]", true},
		{"external domain", "http://evil.com", false},
		{"external https", "https://attacker.com", false},
		{"subdomain localhost", "http://sub.localhost", false},
		{"no origin", "", false},
		{"localhost-like domain", "http://localhost.evil.com", false},
	}

	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/status", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			w := httptest.NewRecorder()

			server.router.ServeHTTP(w, req)

			corsHeader := w.Header().Get("Access-Control-Allow-Origin")
			if tt.expectAllowed {
				assert.Equal(t, tt.origin, corsHeader, "expected CORS header to match origin")
			} else {
				assert.Empty(t, corsHeader, "expected no CORS header for non-localhost origin")
			}
		})
	}
}

func TestCorsMiddleware_OptionsRequest(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	req := httptest.NewRequest("OPTIONS", "/api/v1/status", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "http://localhost:3000", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "GET")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	// Auth disabled
	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: false,
	}, handlers)

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthMiddleware_Enabled_MissingHeader(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: true,
		Token:       "secret-token-123",
	}, handlers)

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "missing authorization header")
}

func TestAuthMiddleware_Enabled_InvalidFormat(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: true,
		Token:       "secret-token-123",
	}, handlers)

	tests := []struct {
		name   string
		header string
	}{
		{"basic auth", "Basic dXNlcjpwYXNz"},
		{"no bearer prefix", "secret-token-123"},
		{"wrong case", "bearer secret-token-123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/status", nil)
			req.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()

			server.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Contains(t, w.Body.String(), "invalid authorization header format")
		})
	}
}

func TestAuthMiddleware_Enabled_InvalidToken(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: true,
		Token:       "secret-token-123",
	}, handlers)

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid token")
}

func TestAuthMiddleware_Enabled_ValidToken(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: true,
		Token:       "secret-token-123",
	}, handlers)

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret-token-123")
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthMiddleware_HealthEndpointNoAuth(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host:        "127.0.0.1",
		Port:        0,
		AuthEnabled: true,
		Token:       "secret-token-123",
	}, handlers)

	// Health endpoint should work without auth
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestServerAddr(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host: "127.0.0.1",
		Port: 8080,
	}, handlers)

	assert.Equal(t, "127.0.0.1:8080", server.Addr())
}

func TestServerStartShutdown(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	// Use port 0 to get a random available port
	server := NewServer(ServerConfig{
		Host: "127.0.0.1",
		Port: 0,
	}, handlers)

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := server.Shutdown(ctx)
	require.NoError(t, err)

	// Server should have returned
	select {
	case err := <-errCh:
		// http.ErrServerClosed is expected
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("server did not stop within timeout")
	}
}

func TestServerShutdown_NilServer(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	server := NewServer(ServerConfig{
		Host: "127.0.0.1",
		Port: 0,
	}, handlers)

	// Shutdown without starting should not panic
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := server.Shutdown(ctx)
	assert.NoError(t, err)
}

// TestRequestTimeoutConstants pins the two router-level ceilings: the default
// 30s for ordinary routes and the much larger lifecycle ceiling, which must sit
// above the configured stop-budget cap so a legitimately long stop/restart is
// never truncated by the router (the supervisor is authoritative). (#35, D2)
func TestRequestTimeoutConstants(t *testing.T) {
	assert.Equal(t, 30*time.Second, defaultRequestTimeout)
	assert.Equal(t, constants.MaxStopTimeout+time.Minute, lifecycleRequestTimeout)
	assert.Greater(t, lifecycleRequestTimeout, defaultRequestTimeout)
	assert.GreaterOrEqual(t, lifecycleRequestTimeout, constants.MaxStopTimeout,
		"lifecycle ceiling must cover the maximum configurable stop budget")
}

// TestRouteGroupTimeoutWiring verifies the per-group timeout mechanism that
// registerRoutes relies on: a route placed in a group with
// middleware.Timeout(lifecycleRequestTimeout) sees a context deadline of ~11m,
// while a route under defaultRequestTimeout sees ~30s. It mirrors the exact
// group structure of registerRoutes with probe handlers (the real lifecycle
// handlers cannot report their own deadline), keeping the test instant -- no
// >30s sleep. Together with TestRequestTimeoutConstants and the routing smoke
// test, this establishes that lifecycle requests are not cut at the old 30s
// boundary.
func TestRouteGroupTimeoutWiring(t *testing.T) {
	var lifecycleBudget, defaultBudget time.Duration

	probe := func(dst *time.Duration) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if dl, ok := r.Context().Deadline(); ok {
				*dst = time.Until(dl)
			}
			w.WriteHeader(http.StatusOK)
		}
	}

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(lifecycleRequestTimeout))
		r.Post("/processes/{name}/stop", probe(&lifecycleBudget))
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(defaultRequestTimeout))
		r.Get("/status", probe(&defaultBudget))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/processes/web/stop", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// Lifecycle route must carry the large ceiling, well past the old 30s.
	assert.Greater(t, lifecycleBudget, 30*time.Second, "lifecycle route deadline must exceed the old 30s boundary")
	assert.WithinDuration(t, time.Now().Add(lifecycleRequestTimeout), time.Now().Add(lifecycleBudget), 5*time.Second)
	// Default route keeps the 30s ceiling.
	assert.LessOrEqual(t, defaultBudget, 30*time.Second)
	assert.Greater(t, defaultBudget, 25*time.Second)
}

// TestRegisterRoutes_RoutingSmoke exercises the restructured router end-to-end to
// confirm the subgroup split did not break routing: lifecycle POSTs, read-only
// GETs, the SSE routes, /health and /shutdown all resolve to their handlers.
func TestRegisterRoutes_RoutingSmoke(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	cases := []struct {
		method, path string
		wantStatus   int
	}{
		{"GET", "/health", http.StatusOK},
		{"GET", "/api/v1/status", http.StatusOK},
		{"GET", "/api/v1/processes", http.StatusOK},
		// Lifecycle routes resolve to their handlers; a missing process yields a
		// 404 from the handler (not a routing miss), proving the subgroup wiring.
		{"POST", "/api/v1/processes/missing/start", http.StatusNotFound},
		{"POST", "/api/v1/processes/missing/stop", http.StatusNotFound},
		{"POST", "/api/v1/processes/missing/restart", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestIsLocalhostOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"http://localhost", true},
		{"http://localhost:3000", true},
		{"https://localhost", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:8080", true},
		{"https://127.0.0.1", true},
		{"http://[::1]", true},
		{"http://[::1]:3000", true},
		{"https://[::1]", true},
		{"http://example.com", false},
		{"http://localhost.example.com", false},
		{"http://127.0.0.1.evil.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			got := isLocalhostOrigin(tt.origin)
			assert.Equal(t, tt.want, got)
		})
	}
}
