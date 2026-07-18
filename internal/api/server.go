package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/charliek/prox/internal/constants"
)

// defaultRequestTimeout bounds all non-lifecycle API requests. The lifecycle
// routes (start/stop/restart) are exempt because the supervisor bounds them
// internally per-process; they use lifecycleRequestTimeout instead.
const defaultRequestTimeout = constants.DefaultRequestTimeout

// lifecycleRequestTimeout is the router-level hang-protection ceiling for the
// start/stop/restart routes. It sits safely above the configured stop-budget
// cap (constants.MaxStopTimeout) so a legitimately long stop is never cut off
// by the router; the supervisor is the authoritative bound (#35, D2).
const lifecycleRequestTimeout = constants.LifecycleTimeoutCeiling

// ServerConfig holds configuration for the API server
type ServerConfig struct {
	Host        string
	Port        int
	AuthEnabled bool   // Whether authentication is required
	Token       string // Authentication token (only used if AuthEnabled is true)
}

// Server represents the HTTP API server
type Server struct {
	config     ServerConfig
	router     *chi.Mux
	httpServer *http.Server
	handlers   *Handlers
	mu         sync.Mutex
}

// NewServer creates a new API server
func NewServer(config ServerConfig, handlers *Handlers) *Server {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// NOTE: the request timeout is applied per route group in registerRoutes,
	// not globally here. Most routes get defaultRequestTimeout (30s); the
	// lifecycle routes (start/stop/restart) get a much larger ceiling because
	// the supervisor bounds those operations internally by each process's
	// configured stop budget (#35, D2).

	// CORS - restricted to localhost only for security
	r.Use(corsMiddleware())

	s := &Server{
		config:   config,
		router:   r,
		handlers: handlers,
	}

	// Register routes
	s.registerRoutes()

	return s
}

// corsMiddleware returns a CORS middleware restricted to localhost
func corsMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			// Only allow localhost origins
			if isLocalhostOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLocalhostOrigin checks if the origin is from localhost.
// It validates that the origin is exactly a localhost address (with optional port).
func isLocalhostOrigin(origin string) bool {
	if origin == "" {
		return false
	}

	// Allow common localhost patterns with optional port
	// Match: http://localhost, http://localhost:3000, https://localhost, etc.
	localhostPrefixes := []string{
		"http://localhost",
		"https://localhost",
		"http://127.0.0.1",
		"https://127.0.0.1",
		"http://[::1]",
		"https://[::1]",
	}

	for _, prefix := range localhostPrefixes {
		if origin == prefix {
			return true
		}
		// Check for origin with port (prefix followed by ":")
		if strings.HasPrefix(origin, prefix+":") {
			return true
		}
	}
	return false
}

// authMiddleware returns an authentication middleware
func authMiddleware(authEnabled bool, token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth if not enabled
			if !authEnabled {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"missing authorization header","code":"UNAUTHORIZED"}`))
				return
			}

			// Expect "Bearer <token>" format
			const prefix = "Bearer "
			if !strings.HasPrefix(authHeader, prefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid authorization header format","code":"UNAUTHORIZED"}`))
				return
			}

			providedToken := strings.TrimPrefix(authHeader, prefix)
			// Use constant-time comparison to prevent timing attacks
			if subtle.ConstantTimeCompare([]byte(providedToken), []byte(token)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid token","code":"UNAUTHORIZED"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// registerRoutes sets up all API routes.
//
// The request-timeout middleware is applied per route group, not globally, so
// the lifecycle routes can carry a larger ceiling than everything else:
//   - Lifecycle group (start/stop/restart, /shutdown): lifecycleRequestTimeout
//     (11m). The supervisor bounds start/stop/restart internally per-process by
//     the configured stop budget; /shutdown?wait=true blocks for the whole drain.
//     This router ceiling is hang protection only.
//   - SSE group (/logs/stream, /proxy/requests/stream): NO timeout middleware.
//     The streams are long-lived by design (WriteTimeout is 0 for the same
//     reason); they end when the client disconnects (r.Context() is cancelled
//     on connection close) or when the daemon shuts down (the log manager
//     closes subscriber channels at shutdown). (#42)
//   - Default group (everything else): defaultRequestTimeout (30s).
func (s *Server) registerRoutes() {
	// Health check at root (no auth required). Kept under the default 30s
	// ceiling to preserve prior behavior (it returns immediately regardless).
	s.router.With(middleware.Timeout(defaultRequestTimeout)).
		Get("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})

	s.router.Route("/api/v1", func(r chi.Router) {
		// Apply auth middleware to all API routes (only if auth is enabled)
		r.Use(authMiddleware(s.config.AuthEnabled, s.config.Token))

		// Lifecycle routes: bounded internally by the supervisor per-process, so
		// they get the large hang-protection ceiling rather than the 30s default.
		// /shutdown lives here too: its wait=true path blocks for the whole drain
		// (supervisor stop + finalization), which can exceed the 30s default (#36,
		// D4). The legacy async /shutdown returns immediately, so the larger ceiling
		// is harmless for it.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(lifecycleRequestTimeout))

			r.Post("/processes/{name}/start", s.handlers.StartProcess)
			r.Post("/processes/{name}/stop", s.handlers.StopProcess)
			r.Post("/processes/{name}/restart", s.handlers.RestartProcess)

			// Shutdown (wait=true blocks for the drain; legacy default acks async).
			r.Post("/shutdown", s.handlers.Shutdown)
		})

		// SSE streams: no timeout middleware. These are long-lived connections
		// that end on client disconnect or daemon shutdown, matching the
		// server's WriteTimeout of 0 (#42).
		r.Group(func(r chi.Router) {
			r.Get("/logs/stream", s.handlers.StreamLogs)
			// Note: /proxy/requests/stream is registered here while
			// /proxy/requests/{id} lives in the default group below; chi routes
			// the literal "stream" segment ahead of the {id} parameter.
			r.Get("/proxy/requests/stream", s.handlers.StreamProxyRequests)
		})

		// Everything else keeps the default 30s ceiling.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(defaultRequestTimeout))

			// Supervisor status
			r.Get("/status", s.handlers.GetStatus)

			// Processes (read-only)
			r.Get("/processes", s.handlers.GetProcesses)
			r.Get("/processes/{name}", s.handlers.GetProcess)

			// Logs
			r.Get("/logs", s.handlers.GetLogs)

			// Proxy requests
			r.Get("/proxy/requests", s.handlers.GetProxyRequests)
			r.Get("/proxy/requests/{id}", s.handlers.GetProxyRequest)
		})
	})
}

// Start starts the HTTP server
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)

	s.mu.Lock()
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // Disable for SSE
		IdleTimeout:  60 * time.Second,
	}
	server := s.httpServer
	s.mu.Unlock()

	return server.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	server := s.httpServer
	s.mu.Unlock()

	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

// Addr returns the server address
func (s *Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
}
