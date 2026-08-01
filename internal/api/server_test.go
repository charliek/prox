package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)
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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)
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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)

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

// deadlineProbe returns a handler that records the remaining time until its
// request's context deadline into dst, so a test can assert which timeout
// middleware group a route landed in without exercising the real handler body.
func deadlineProbe(dst *time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dl, ok := r.Context().Deadline(); ok {
			*dst = time.Until(dl)
		}
		w.WriteHeader(http.StatusOK)
	}
}

// TestRouteGroupTimeoutWiring verifies the per-group timeout mechanism that
// registerRoutes relies on: a route placed in a group with
// middleware.Timeout(lifecycleRequestTimeout) sees a context deadline of ~11m,
// a route under defaultRequestTimeout sees ~30s, and the SSE group carries NO
// deadline at all (#42). It mirrors the exact group structure of registerRoutes
// with probe handlers (the real lifecycle handlers cannot report their own
// deadline), keeping the test instant -- no >30s sleep. Together with
// TestRequestTimeoutConstants and the routing smoke test, this establishes
// that lifecycle requests are not cut at the old 30s boundary and that the SSE
// streams are not deadline-bounded at all. It also pins chi's static-over-param
// precedence: /proxy/requests/stream (SSE group) must win over
// /proxy/requests/{id} (default group) even though they live in different
// groups.
func TestRouteGroupTimeoutWiring(t *testing.T) {
	var lifecycleBudget, defaultBudget, idBudget time.Duration
	sseLogsSawDeadline := true
	sseProxySawDeadline := true
	sseProcessesSawDeadline := true

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(lifecycleRequestTimeout))
		r.Post("/processes/{name}/stop", deadlineProbe(&lifecycleBudget))
	})
	r.Group(func(r chi.Router) {
		r.Get("/logs/stream", func(w http.ResponseWriter, r *http.Request) {
			_, sseLogsSawDeadline = r.Context().Deadline()
			w.WriteHeader(http.StatusOK)
		})
		r.Get("/proxy/requests/stream", func(w http.ResponseWriter, r *http.Request) {
			_, sseProxySawDeadline = r.Context().Deadline()
			w.WriteHeader(http.StatusOK)
		})
		r.Get("/processes/stream", func(w http.ResponseWriter, r *http.Request) {
			_, sseProcessesSawDeadline = r.Context().Deadline()
			w.WriteHeader(http.StatusOK)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(defaultRequestTimeout))
		r.Get("/status", deadlineProbe(&defaultBudget))
		r.Get("/proxy/requests/{id}", deadlineProbe(&idBudget))
	})

	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/processes/web/stop", nil),
		httptest.NewRequest("GET", "/status", nil),
		httptest.NewRequest("GET", "/logs/stream", nil),
		httptest.NewRequest("GET", "/proxy/requests/stream", nil),
		httptest.NewRequest("GET", "/processes/stream", nil),
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s %s", req.Method, req.URL.Path)
	}

	// Lifecycle route must carry the large ceiling, well past the old 30s.
	assert.Greater(t, lifecycleBudget, 30*time.Second, "lifecycle route deadline must exceed the old 30s boundary")
	assert.WithinDuration(t, time.Now().Add(lifecycleRequestTimeout), time.Now().Add(lifecycleBudget), 5*time.Second)
	// Default route keeps the 30s ceiling.
	assert.LessOrEqual(t, defaultBudget, 30*time.Second)
	assert.Greater(t, defaultBudget, 25*time.Second)
	// SSE routes carry no deadline at all (#42).
	assert.False(t, sseLogsSawDeadline, "/logs/stream must have no context deadline")
	assert.False(t, sseProxySawDeadline, "/proxy/requests/stream must have no context deadline")
	assert.False(t, sseProcessesSawDeadline, "/processes/stream must have no context deadline")
	// GET /proxy/requests/stream resolved to the SSE probe, so the {id} probe
	// under the timed group must never have run.
	assert.Zero(t, idBudget, "/proxy/requests/stream must not match /proxy/requests/{id}")
}

// TestSSEStreamSurvivesTimeoutBoundary runs the real StreamLogs handler inside
// the SSE (no-timeout) group structure of registerRoutes, next to a sibling
// group carrying a short stand-in timeout (150ms instead of 30s, keeping the
// test instant). The sibling is genuinely cut at the injected boundary, while
// the stream still delivers an entry published well after it -- proving the
// SSE group escapes the timeout class that would have killed it (#42). Like
// TestRouteGroupTimeoutWiring, the router here is a reconstruction of
// registerRoutes' group structure, not the real router.
func TestSSEStreamSurvivesTimeoutBoundary(t *testing.T) {
	const injectedTimeout = 150 * time.Millisecond

	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100, SubscriptionBuffer: 10})
	defer logMgr.Close()
	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Get("/logs/stream", handlers.StreamLogs)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(injectedTimeout))
		r.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	// The timed sibling is cut at the injected boundary.
	resp, err := http.Get(srv.URL + "/slow")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	// The stream outlives that same boundary.
	stream, err := http.Get(srv.URL + "/logs/stream")
	require.NoError(t, err)
	defer stream.Body.Close()

	reader := bufio.NewReader(stream.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, ": connected")

	// Sail well past the injected boundary, then publish.
	time.Sleep(3 * injectedTimeout)
	logMgr.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "late",
		Stream:    domain.StreamStdout,
		Line:      "still alive",
	})

	got := make(chan struct{})
	go func() {
		for {
			l, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.Contains(l, "still alive") {
				close(got)
				return
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not deliver an entry published after the injected timeout boundary")
	}
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
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)
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
		// Legacy async /shutdown (no wait) resolves to its handler and acks 200
		// even with a nil coordinator wired.
		{"POST", "/api/v1/shutdown", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

// TestShutdownRouteInLifecycleGroup verifies POST /shutdown carries the large
// lifecycle ceiling, not the 30s default: its wait=true path can block for the
// whole drain, which would otherwise be cut at 30s (#36, D4). The handler reads
// its request deadline and reports it back so the test can assert the ceiling.
func TestShutdownRouteInLifecycleGroup(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", nil)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	// Probe the deadline the router grants /shutdown. NOTE: probeRouter is a
	// RECONSTRUCTION of registerRoutes' lifecycle group, not the real router — chi
	// does not expose a per-route middleware chain to introspect, and proving the
	// timeout CLASS distinction (11m vs 30s) directly would need a >30s test. It
	// asserts only that a route wired the way registerRoutes wires /shutdown sees
	// the lifecycle ceiling. The real route path (router + middleware + handler) is
	// exercised by the sanity ack below, by TestRegisterRoutes_RoutingSmoke, and by
	// TestShutdownRoute_WaitedResponseThroughRealRouter.
	var shutdownBudget time.Duration
	probeRouter := chi.NewRouter()
	probeRouter.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(lifecycleRequestTimeout))
			r.Post("/shutdown", deadlineProbe(&shutdownBudget))
		})
	})
	rec := httptest.NewRecorder()
	probeRouter.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/shutdown", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Greater(t, shutdownBudget, 30*time.Second, "/shutdown must carry the lifecycle ceiling, not the 30s default")

	// Sanity: the real server still routes legacy /shutdown to a 200 ack.
	rec = httptest.NewRecorder()
	server.router.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/shutdown", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestShutdownRoute_WaitedResponseThroughRealRouter drives POST
// /shutdown?wait=true through the REAL router + middleware chain (not the probe)
// against a coordinator already completed with a survivor outcome, and asserts the
// waited JSON body (HTTP 200, success=false, failures[]) comes back intact. This
// proves the real route path delivers the waited verdict end to end.
func TestShutdownRoute_WaitedResponseThroughRealRouter(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())

	fake := newFakeShutdownController()
	fake.complete(&domain.ProcessStopError{
		Failures: []domain.ProcessStopFailure{
			{Name: "web", Err: fmt.Errorf("%w: web", domain.ErrProcessGroupNotReaped)},
		},
	})
	handlers := NewHandlers(sup, logMgr, "test.yaml", "", fake)
	server := NewServer(ServerConfig{Host: "127.0.0.1", Port: 0}, handlers)

	rec := httptest.NewRecorder()
	server.router.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/shutdown?wait=true", nil))

	require.Equal(t, http.StatusOK, rec.Code, "survivors still ride a 200 so the body is not discarded")
	var resp ShutdownResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Success)
	assert.True(t, resp.Waited)
	require.Len(t, resp.Failures, 1)
	assert.Equal(t, "web", resp.Failures[0].Process)
	assert.Equal(t, domain.ErrCodeProcessGroupNotReaped, resp.Failures[0].Code)
	assert.GreaterOrEqual(t, fake.triggerCount(), 1)
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
