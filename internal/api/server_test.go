package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
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
