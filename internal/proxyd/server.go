package proxyd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/go-chi/chi/v5"
)

// Server is the daemon's Unix socket HTTP server.
type Server struct {
	socketPath string
	version    string
	router     *chi.Mux
	httpServer *http.Server
	listener   net.Listener
	logger     *slog.Logger

	// shutdownCh is closed when the daemon should exit (e.g., no more routes).
	shutdownCh chan struct{}
	shutdownMu sync.Mutex
	startedAt  time.Time

	// Core components
	registry       *Registry
	proxy          *DynamicProxy
	requestManager *proxy.RequestManager
}

// ServerConfig holds configuration for creating a daemon server.
type ServerConfig struct {
	SocketPath string
	Logger     *slog.Logger
	Version    string
}

// NewServer creates a new daemon Unix socket server.
func NewServer(cfg ServerConfig) *Server {
	r := chi.NewRouter()

	s := &Server{
		socketPath: cfg.SocketPath,
		version:    cfg.Version,
		router:     r,
		logger:     cfg.Logger,
		shutdownCh: make(chan struct{}),
		startedAt:  time.Now(),
	}

	s.registerRoutes()
	return s
}

// SetRegistry sets the route registry (called during daemon setup).
func (s *Server) SetRegistry(reg *Registry) {
	s.registry = reg
}

// SetProxy sets the dynamic proxy (called during daemon setup).
func (s *Server) SetProxy(p *DynamicProxy) {
	s.proxy = p
}

// SetRequestManager sets the request manager (called during daemon setup).
func (s *Server) SetRequestManager(rm *proxy.RequestManager) {
	s.requestManager = rm
}

// ShutdownCh returns a channel that is closed when the daemon should exit.
func (s *Server) ShutdownCh() <-chan struct{} {
	return s.shutdownCh
}

// RequestShutdown signals the daemon to exit.
func (s *Server) RequestShutdown() {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	select {
	case <-s.shutdownCh:
		// already closed
	default:
		close(s.shutdownCh)
	}
}

func (s *Server) registerRoutes() {
	// Health check — no prefix, lightweight
	s.router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"version": s.version,
		})
	})

	s.router.Route("/api/v1", func(r chi.Router) {
		r.Post("/register", s.handleRegister)
		r.Post("/deregister", s.handleDeregister)
		r.Get("/status", s.handleStatus)
		r.Get("/routes", s.handleRoutes)
		r.Post("/shutdown", s.handleShutdown)
		r.Get("/requests/stream", s.handleStreamRequests)
		r.Get("/requests", s.handleGetRequests)
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("invalid request body: %v", err),
			Code:  "BAD_REQUEST",
		})
		return
	}

	// Version check: exact match required
	if req.Version != s.version {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf(
				"version mismatch: daemon is %s, client is %s. Stop all projects and restart, or run 'prox proxy stop --force'",
				s.version, req.Version,
			),
			Code: "VERSION_MISMATCH",
		})
		return
	}

	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "daemon is starting up",
			Code:  "NOT_READY",
		})
		return
	}

	// ProjectDir is the identity records are filtered and purged by; an empty
	// one would produce records that no deregistration could ever clean up.
	if req.ProjectDir == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "project_dir is required",
			Code:  "BAD_REQUEST",
		})
		return
	}

	// Register routes
	hostnames, newPorts, err := s.registry.Register(req)
	if err != nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: err.Error(),
			Code:  "REGISTRATION_CONFLICT",
		})
		return
	}

	// Ensure certs exist for HTTPS domains before starting listeners
	if s.proxy != nil && s.proxy.certMgr != nil {
		for _, ps := range newPorts {
			if ps.Protocol == "https" {
				if err := s.proxy.certMgr.EnsureDomain(req.Domain); err != nil {
					s.registry.Deregister(req.ProjectDir)
					writeJSON(w, http.StatusInternalServerError, ErrorResponse{
						Error: fmt.Sprintf("failed to generate certs for %s: %v", req.Domain, err),
						Code:  "CERT_GENERATION_FAILED",
					})
					return
				}
				break // one EnsureDomain per domain is sufficient
			}
		}
	}

	// Start listeners for new ports
	if s.proxy != nil {
		var startedPorts []int
		for _, ps := range newPorts {
			if err := s.proxy.AddListener(ps.Port, ps.Protocol); err != nil {
				// Rollback: stop listeners we already started, then deregister
				for _, p := range startedPorts {
					_ = s.proxy.RemoveListener(p)
				}
				s.registry.Deregister(req.ProjectDir)
				writeJSON(w, http.StatusInternalServerError, ErrorResponse{
					Error: fmt.Sprintf("failed to bind port %d: %v", ps.Port, err),
					Code:  "PORT_BIND_FAILED",
				})
				return
			}
			startedPorts = append(startedPorts, ps.Port)
		}
	}

	s.logger.Info("registered project",
		"project", req.ProjectDir,
		"pid", req.PID,
		"domain", req.Domain,
		"hostnames", hostnames,
	)

	writeJSON(w, http.StatusOK, RegisterResponse{Registered: hostnames})
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var req DeregisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("invalid request body: %v", err),
			Code:  "BAD_REQUEST",
		})
		return
	}

	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "daemon is starting up",
			Code:  "NOT_READY",
		})
		return
	}

	removedHostnames, emptyPorts := s.removeProject(req.ProjectDir)

	s.logger.Info("deregistered project",
		"project", req.ProjectDir,
		"pid", req.PID,
		"removed_hostnames", removedHostnames,
		"closed_ports", emptyPorts,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removedHostnames,
	})

	// If registry is empty, shut down the daemon
	if s.registry.IsEmpty() {
		s.logger.Info("no routes registered, shutting down daemon")
		go func() {
			// Brief grace period for rapid down/up cycles
			time.Sleep(5 * time.Second)
			if s.registry.IsEmpty() {
				s.RequestShutdown()
			}
		}()
	}
}

// removeProject is the single consolidated project-removal path: it removes the
// project's routes, closes listeners for ports that now have no routes, and
// purges the project's captured requests (firing eviction callbacks so on-disk
// body files are cleaned up). It is called from explicit deregister and from
// the stale-PID crash-recovery sweep so the crash path can't leak records or
// body files. Returns the removed hostnames and now-empty ports for logging.
func (s *Server) removeProject(projectDir string) (removedHostnames []string, emptyPorts []int) {
	if s.registry == nil {
		return nil, nil
	}

	removedHostnames, emptyPorts = s.registry.Deregister(projectDir)
	s.finishRemoval(projectDir, emptyPorts)
	return removedHostnames, emptyPorts
}

// removeStaleProject is removeProject for the crash-recovery sweep: removal is
// guarded on the registration still carrying the detected dead PID, so a
// project that re-registered between detection and removal is left alone.
func (s *Server) removeStaleProject(projectDir string, pid int) (removed bool, removedHostnames []string, emptyPorts []int) {
	if s.registry == nil {
		return false, nil, nil
	}

	removed, removedHostnames, emptyPorts = s.registry.DeregisterIfPID(projectDir, pid)
	if !removed {
		return false, nil, nil
	}
	s.finishRemoval(projectDir, emptyPorts)
	return true, removedHostnames, emptyPorts
}

// finishRemoval closes listeners for now-empty ports and purges the project's
// captured requests (firing eviction callbacks so on-disk body files are
// cleaned up).
func (s *Server) finishRemoval(projectDir string, emptyPorts []int) {
	if s.proxy != nil {
		for _, port := range emptyPorts {
			if err := s.proxy.RemoveListener(port); err != nil {
				s.logger.Warn("failed to remove listener", "port", port, "error", err)
			}
		}
	}

	if s.requestManager != nil {
		s.requestManager.PurgeByProject(projectDir)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := DaemonStatusResponse{
		Version:   s.version,
		PID:       os.Getpid(),
		StartedAt: s.startedAt,
		Uptime:    time.Since(s.startedAt).Truncate(time.Second).String(),
	}

	if s.registry != nil {
		resp.Routes = s.registry.AllRoutes()
		resp.ListenerPorts = s.registry.ListenerPorts()
		resp.ProjectCount = s.registry.ProjectCount()
		resp.RouteCount = len(resp.Routes)
	}
	if resp.Routes == nil {
		resp.Routes = []RouteInfo{}
	}
	if resp.ListenerPorts == nil {
		resp.ListenerPorts = []int{}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	var routes []RouteInfo
	if s.registry != nil {
		routes = s.registry.AllRoutes()
	}
	if routes == nil {
		routes = []RouteInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": routes})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	if s.registry != nil && !s.registry.IsEmpty() && !force {
		routes := s.registry.AllRoutes()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf(
				"proxy has %d active route(s) from %d project(s). Use --force to stop anyway.",
				len(routes), s.registry.ProjectCount(),
			),
			Code: "ACTIVE_ROUTES",
		})
		return
	}

	s.logger.Info("shutdown requested", "force", force)
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})

	go s.RequestShutdown()
}

func (s *Server) handleStreamRequests(w http.ResponseWriter, r *http.Request) {
	if s.requestManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "request manager not available",
			Code:  "NOT_READY",
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "streaming not supported",
			Code:  "STREAMING_NOT_SUPPORTED",
		})
		return
	}

	// Filter the stream by owning project (exact match). Scoping by project
	// dir rather than hostname prevents cross-project record delivery when two
	// projects own the same hostname on different ports. The param is
	// mandatory: an empty ProjectDir filter would match ALL projects' records,
	// so a caller that forgot the param would silently receive everything.
	projectDir := r.URL.Query().Get("project")
	if projectDir == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "project query parameter is required",
			Code:  "BAD_REQUEST",
		})
		return
	}
	filter := proxy.RequestFilter{ProjectDir: projectDir}
	sub := s.requestManager.Subscribe(filter)
	defer s.requestManager.Unsubscribe(sub.ID)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case rec, ok := <-sub.Ch:
			if !ok {
				return
			}
			data, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleGetRequests(w http.ResponseWriter, r *http.Request) {
	if s.requestManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "request manager not available",
			Code:  "NOT_READY",
		})
		return
	}

	// Mandatory for the same reason as the stream endpoint: an empty
	// ProjectDir filter matches every project's records.
	projectDir := r.URL.Query().Get("project")
	if projectDir == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "project query parameter is required",
			Code:  "BAD_REQUEST",
		})
		return
	}

	filter := proxy.RequestFilter{
		ProjectDir: projectDir,
		Limit:      100,
	}

	records := s.requestManager.Recent(filter)
	writeJSON(w, http.StatusOK, map[string]any{"requests": records})
}

// Start starts listening on the Unix socket.
func (s *Server) Start() error {
	// Remove stale socket file
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket %s: %w", s.socketPath, err)
	}

	// Set socket permissions so only the owner can connect
	if err := os.Chmod(s.socketPath, 0700); err != nil {
		ln.Close()
		return fmt.Errorf("setting socket permissions: %w", err)
	}

	s.listener = ln
	s.httpServer = &http.Server{
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // Disable for SSE
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("daemon listening", "socket", s.socketPath)
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	// Remove socket file
	defer os.Remove(s.socketPath)
	return s.httpServer.Shutdown(ctx)
}

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
