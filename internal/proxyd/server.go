package proxyd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
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

	// shutdownDelay is the grace period scheduleShutdownWhenEmpty waits before
	// re-checking an emptied registry, giving a rapid down/up (or a crash
	// restart landing during a sweep) time to re-register. Injectable in tests.
	shutdownDelay time.Duration

	// lifecycleMu serializes control-plane lifecycle transactions
	// (registry mutation + physical listener add/remove + record purge) so a
	// concurrent sweep tick can't close a fresh registration's listeners, fail
	// its retry with PORT_BIND_FAILED, or purge its records. The data plane
	// (route Lookup, proxying, SSE) never touches this mutex.
	lifecycleMu sync.Mutex

	// lifecycleEpoch increments on every registry mutation. A grace timer
	// scheduled by scheduleShutdownWhenEmpty captures the epoch and stands
	// down if it changed, so a timer from an older empty period can't cut a
	// newer grace period short.
	lifecycleEpoch atomic.Uint64

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
		socketPath:    cfg.SocketPath,
		version:       cfg.Version,
		router:        r,
		logger:        cfg.Logger,
		shutdownCh:    make(chan struct{}),
		startedAt:     time.Now(),
		shutdownDelay: 5 * time.Second,
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

// isShuttingDown reports whether daemon shutdown has been requested.
func (s *Server) isShuttingDown() bool {
	select {
	case <-s.shutdownCh:
		return true
	default:
		return false
	}
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

// quiesceForTeardown makes daemon teardown mutually exclusive with lifecycle
// transactions. It sets the shutdown flag FIRST (so every later register,
// deregister, and stale-removal self-gates on isShuttingDown()), then takes
// lifecycleMu once as a barrier so the single transaction that was already in
// flight when the flag was set completes atomically before the caller tears down
// listeners and records. Flag-before-lock is the contract; do not reorder.
//
// Lock ordering: RequestShutdown takes shutdownMu and releases it before this
// method acquires lifecycleMu, so there is no shutdownMu<->lifecycleMu cycle with
// the scheduleShutdownWhenEmpty timer (which takes lifecycleMu, then shutdownMu
// via RequestShutdown, with no overlapping hold).
func (s *Server) quiesceForTeardown() {
	s.RequestShutdown()
	s.lifecycleMu.Lock()
	s.lifecycleMu.Unlock() //nolint:staticcheck // SA2001: intentional lifecycle barrier
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

	// PID identifies the registration's generation and is the liveness key for
	// crash-restart self-heal. ProcessExists(0)/negative PIDs have
	// signal-broadcast semantics and would read as permanently alive, so a
	// crashed generation with a bad PID could never be replaced.
	if req.PID <= 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "pid must be a positive process id",
			Code:  "BAD_REQUEST",
		})
		return
	}

	status, body := s.register(req)
	writeJSON(w, status, body)
}

// register runs the full register-through-listener-start lifecycle transaction
// under lifecycleMu (registry mutation, crash-restart self-heal, cert
// generation, listener add, and all rollbacks) and returns the HTTP status and
// response body for the caller to write once the lock is released. Serializing
// the whole transaction means a concurrent sweep tick can't tear down the new
// generation's listeners or purge its records mid-flow.
func (s *Server) register(req RegisterRequest) (int, any) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// A register that queued behind a shutdown decision must not report
	// success from a daemon that is already exiting.
	if s.isShuttingDown() {
		return http.StatusServiceUnavailable, ErrorResponse{
			Error: "daemon is shutting down; retry to start a fresh daemon",
			Code:  "SHUTTING_DOWN",
		}
	}

	// restoreSnap, when non-nil, is the pre-removal snapshot of a same-identity
	// holder whose config CHANGED (the D6a DIFFERENT arm): every failure point
	// after its removal must restore it rather than leave the project
	// unregistered. It stays nil for the crash-replace and fresh-register paths.
	var restoreSnap *projectSnapshot

	hostnames, newPorts, err := s.registry.Register(req)
	if err != nil {
		var conflict *ProjectConflictError
		if !errors.As(err, &conflict) {
			// Route/validation conflict — a real error, never retried.
			return http.StatusConflict, ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"}
		}
		if daemon.IsProcessAlive(conflict.PID, conflict.StartTime) {
			// The same-dir holder is still live. Sub-cases:
			//   - SAME generation as the requester (same PID AND matching non-zero
			//     start token): an idempotent re-register (D6a) — the heal path
			//     (D6b) re-registering against a live daemon whose SSE stream broke,
			//     or a client retrying a register it already won.
			//   - A DIFFERENT generation (different PID, or an unreadable token on
			//     either side that can't be proven identical): a genuine second
			//     `prox up` in the same dir — stays a hard 409.
			if !sameRegistrationIdentity(conflict.PID, conflict.StartTime, req.PID, req.StartTime) {
				return http.StatusConflict, ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"}
			}
			// FIX 1: when the re-registering process's config is UNCHANGED (the
			// common heal case), take a true no-op refresh — echo the current
			// hostnames without any remove+add, so no listeners churn and, crucially,
			// no daemon-side records are purged. This is what a heal against a live
			// daemon whose SSE stream merely broke actually needs.
			if s.registry.registrationMatches(req) {
				s.logger.Info("idempotent re-register: config unchanged, no-op refresh",
					"project", conflict.Dir, "pid", conflict.PID, "start_time", conflict.StartTime)
				return http.StatusOK, RegisterResponse{Registered: s.registry.ProjectHostnames(conflict.Dir)}
			}
			// FIX 2: the same process re-registers with a CHANGED config (rare).
			// Keep the remove+add, but make it failure-atomic: snapshot the current
			// registration first so any failure in the retry Register / cert /
			// listener phases below can restore it — a failed re-register must never
			// leave the project unregistered.
			if snap, ok := s.registry.snapshotProject(conflict.Dir); ok {
				restoreSnap = &snap
			}
			s.logger.Info("idempotent re-register: config changed, replacing registration",
				"project", conflict.Dir, "pid", conflict.PID, "start_time", conflict.StartTime)
			// removeProjectLocked drops the holder's routes and refcounts (closing
			// now-empty listeners) and purges its records; the shared re-register
			// below rebinds the ports and refcounts them back to one, so N
			// re-registers followed by ONE deregister still close the port.
			s.removeProjectLocked(conflict.Dir)
		} else {
			// The holder crashed without deregistering. Replace its stale
			// registration inline (PID-guarded; purges its records + body files and
			// closes its listeners) and retry once. Under lifecycleMu the retry
			// cannot lose a race, so a single retry is a correctness guarantee.
			s.logger.Warn("replacing stale registration", "project", conflict.Dir, "pid", conflict.PID, "start_time", conflict.StartTime)
			s.removeStaleProjectLocked(conflict.Dir, conflict.PID, conflict.StartTime)
		}
		// Shared retry tail: whichever removal ran above freed the same-dir holder,
		// so rebind under the still-held lifecycleMu. A single retry cannot lose a
		// race, so this is a correctness guarantee, not a hopeful retry.
		hostnames, newPorts, err = s.registry.Register(req)
		if err != nil {
			if restoreSnap != nil {
				s.restoreSnapshotLocked(*restoreSnap)
			}
			return http.StatusConflict, ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"}
		}
	}

	// Ensure a cert exists for the registration's domain whenever it registers any
	// HTTPS route. Gating on req.HTTPSPort > 0 (not on a NEW listener port) is the
	// #58 fix: a domain joining an already-bound shared HTTPS port has no new port
	// in newPorts, so the old loop skipped its cert and the SNI handshake failed.
	// EnsureDomain is idempotent (one cert per base domain), so re-calls are cheap.
	if s.proxy != nil && s.proxy.certMgr != nil && req.HTTPSPort > 0 {
		if err := s.proxy.certMgr.EnsureDomain(req.Domain); err != nil {
			s.rollbackRegistration(req.ProjectDir)
			if restoreSnap != nil {
				s.restoreSnapshotLocked(*restoreSnap)
			}
			return http.StatusInternalServerError, ErrorResponse{
				Error: fmt.Sprintf("failed to generate certs for %s: %v", req.Domain, err),
				Code:  "CERT_GENERATION_FAILED",
			}
		}
	}

	// Start listeners for new ports, retrying briefly on a transient bind
	// failure: the OS can hold a just-closed port for a few ms, and the close
	// may be OURS from moments ago on EITHER path — the self-heal replace
	// closes the crashed generation's listener inline, and a registration
	// landing right after a sweep tick rebinds a port the sweep just closed
	// (the sweep-wins ordering; caught by CI in the register-vs-sweep race
	// test). A genuinely occupied port still fails after the bounded window.
	if s.proxy != nil {
		var startedPorts []int
		for _, ps := range newPorts {
			if err := s.addListenerWithBriefRetry(ps.Port, ps.Protocol); err != nil {
				// Rollback: stop listeners we already started, then deregister.
				for _, p := range startedPorts {
					_ = s.proxy.RemoveListener(p)
				}
				s.rollbackRegistration(req.ProjectDir)
				if restoreSnap != nil {
					s.restoreSnapshotLocked(*restoreSnap)
				}
				return http.StatusInternalServerError, ErrorResponse{
					Error: fmt.Sprintf("failed to bind port %d: %v", ps.Port, err),
					Code:  "PORT_BIND_FAILED",
				}
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

	s.lifecycleEpoch.Add(1)
	return http.StatusOK, RegisterResponse{Registered: hostnames}
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

	// If registry is empty, schedule a graced shutdown check.
	s.scheduleShutdownWhenEmpty()
}

// scheduleShutdownWhenEmpty requests daemon shutdown after a grace period when
// the registry has no routes left, but only if it is STILL empty when the grace
// expires — a rapid down/up cycle, or a crash restart landing during a sweep,
// re-registers within the window and cancels the shutdown. Used by explicit
// deregister, the stale-PID sweep, and register rollback paths (an emptied
// registry after a failed self-heal replace would otherwise strand an idle
// daemon forever). No-op when the registry is non-empty at call time.
func (s *Server) scheduleShutdownWhenEmpty() {
	if s.registry == nil || !s.registry.IsEmpty() {
		return
	}
	s.logger.Info("no routes registered, scheduling shutdown check")
	epoch := s.lifecycleEpoch.Load()
	go func() {
		time.Sleep(s.shutdownDelay)
		// Re-check under lifecycleMu so an in-flight register transaction
		// finishes before the decision, and stand down if ANY lifecycle
		// mutation happened since scheduling — a newer empty period gets its
		// own timer with the full grace.
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.lifecycleEpoch.Load() == epoch && s.registry.IsEmpty() {
			s.RequestShutdown()
		}
	}()
}

// removeProject is the single consolidated project-removal path: it removes the
// project's routes, closes listeners for ports that now have no routes, and
// purges the project's captured requests (firing eviction callbacks so on-disk
// body files are cleaned up). It is called from explicit deregister and from
// the stale-PID crash-recovery sweep so the crash path can't leak records or
// body files. Returns the removed hostnames and now-empty ports for logging.
func (s *Server) removeProject(projectDir string) (removedHostnames []string, emptyPorts []int) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// Once teardown has begun, physical cleanup is the exiting daemon's job:
	// dynamicProxy.Shutdown closes every listener and captureMgr.Cleanup removes
	// all body files on exit. A deregister here would race that teardown for the
	// listeners and the closing RequestManager, so no-op under the flag.
	if s.isShuttingDown() {
		return nil, nil
	}
	return s.removeProjectLocked(projectDir)
}

// removeProjectLocked is removeProject's body; lifecycleMu must be held. It lets
// register run a stale-registration replace while already holding the mutex.
func (s *Server) removeProjectLocked(projectDir string) (removedHostnames []string, emptyPorts []int) {
	if s.registry == nil {
		return nil, nil
	}
	removedHostnames, emptyPorts = s.registry.Deregister(projectDir)
	s.lifecycleEpoch.Add(1)
	s.finishRemoval(projectDir, emptyPorts)
	return removedHostnames, emptyPorts
}

// removeStaleProject is removeProject for the crash-recovery sweep: removal is
// guarded on the registration still carrying the detected dead PID AND start
// token, so a restart that reused the crashed PID between detection and removal
// is left alone.
func (s *Server) removeStaleProject(projectDir string, pid int, startTime int64) (removed bool, removedHostnames []string, emptyPorts []int) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// Once teardown has begun, physical cleanup is the exiting daemon's job:
	// dynamicProxy.Shutdown closes every listener and captureMgr.Cleanup removes
	// all body files on exit. A sweep removal here would race that teardown for
	// the listeners and the closing RequestManager, so no-op under the flag.
	if s.isShuttingDown() {
		return false, nil, nil
	}
	return s.removeStaleProjectLocked(projectDir, pid, startTime)
}

// removeStaleProjectLocked is removeStaleProject's body; lifecycleMu must be
// held. register calls it to replace a crashed generation inline.
func (s *Server) removeStaleProjectLocked(projectDir string, pid int, startTime int64) (removed bool, removedHostnames []string, emptyPorts []int) {
	if s.registry == nil {
		return false, nil, nil
	}
	removed, removedHostnames, emptyPorts = s.registry.DeregisterIfIdentity(projectDir, pid, startTime)
	if !removed {
		return false, nil, nil
	}
	s.lifecycleEpoch.Add(1)
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

// rollbackRegistration unwinds a registration whose cert or listener phase
// failed: deregister, purge any records captured for the project in the window
// where its routes were already visible to existing shared-port listeners
// (raw Deregister would orphan them until FIFO eviction), bump the lifecycle
// epoch, and schedule the graced shutdown check so an emptied registry can't
// strand an idle daemon. lifecycleMu must be held.
func (s *Server) rollbackRegistration(projectDir string) {
	s.registry.Deregister(projectDir)
	if s.requestManager != nil {
		s.requestManager.PurgeByProject(projectDir)
	}
	s.lifecycleEpoch.Add(1)
	s.scheduleShutdownWhenEmpty()
}

// restoreSnapshotLocked re-injects a project snapshot into the registry and
// re-opens any physical listener the removal closed, then bumps the lifecycle
// epoch. lifecycleMu must be held. It is register's failure-atomic recovery for
// the same-identity DIFFERENT arm (D6a): when a config-changed remove+add fails
// partway, the original registration is restored so the project is never left
// unregistered. A listener that cannot be re-opened is logged (best-effort): the
// alternative — returning without restoring — would strand the project entirely.
func (s *Server) restoreSnapshotLocked(snap projectSnapshot) {
	reopen := s.registry.restoreProject(snap)
	if s.proxy != nil {
		for _, ps := range reopen {
			if err := s.addListenerWithBriefRetry(ps.Port, ps.Protocol); err != nil {
				s.logger.Error("failed to re-open listener while restoring a registration after a failed re-register",
					"project", snap.proj.Dir, "port", ps.Port, "protocol", ps.Protocol, "error", err)
			}
		}
	}
	s.lifecycleEpoch.Add(1)
	s.logger.Info("restored prior registration after a failed re-register",
		"project", snap.proj.Dir, "pid", snap.proj.PID)
}

// addListenerWithBriefRetry binds a listener, retrying briefly on a transient
// bind failure: the kernel can hold a just-closed port for a few milliseconds,
// and the daemon itself may have closed it moments earlier — the self-heal
// replace closes the crashed generation's listener inline, and a registration
// arriving right after a sweep tick rebinds a port the sweep just closed.
// A single bind would intermittently return EADDRINUSE and fail the very
// restart the self-heal exists to make succeed. Used for every registration
// bind; a genuinely occupied port still fails after the bounded window.
func (s *Server) addListenerWithBriefRetry(port int, protocol string) error {
	const (
		attempts = 10
		backoff  = 20 * time.Millisecond
	)
	var err error
	for i := 0; i < attempts; i++ {
		if err = s.proxy.AddListener(port, protocol); err == nil {
			return nil
		}
		time.Sleep(backoff)
	}
	return err
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
	if s.requestManager != nil {
		resp.DroppedEvents = s.requestManager.DroppedEvents()
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

	// The emptiness check and the shutdown decision must be one atomic step
	// with respect to lifecycle transactions: an unserialized IsEmpty() could
	// read "empty" while a register handler (holding lifecycleMu, having
	// passed its isShuttingDown gate) is about to commit — the register would
	// return 200 and the daemon would exit anyway, stranding that project.
	// Under lifecycleMu, a register either committed first (refused here with
	// ACTIVE_ROUTES) or arrives after the flag is set (503 SHUTTING_DOWN).
	// Lock ordering (lifecycleMu → shutdownMu via RequestShutdown) matches
	// the scheduleShutdownWhenEmpty timer path.
	s.lifecycleMu.Lock()
	if s.registry != nil && !s.registry.IsEmpty() && !force {
		routeCount := len(s.registry.AllRoutes())
		projectCount := s.registry.ProjectCount()
		s.lifecycleMu.Unlock()
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf(
				"proxy has %d active route(s) from %d project(s). Use --force to stop anyway.",
				routeCount, projectCount,
			),
			Code: "ACTIVE_ROUTES",
		})
		return
	}

	s.logger.Info("shutdown requested", "force", force)
	// Set the flag synchronously BEFORE the response so the 200 is the shutdown
	// linearization point: a register arriving after it observes isShuttingDown()
	// and gets 503. server.Shutdown is graceful (it waits for this active handler),
	// so the response still flushes.
	s.RequestShutdown()
	s.lifecycleMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
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

	// Clamp limit with the same semantics as the project API's
	// parseProxyRequestParams: invalid or out-of-range values fall back to
	// the default rather than erroring, so a malformed limit degrades
	// gracefully instead of rejecting the whole request.
	limit := constants.DefaultProxyRequestLimit
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= constants.MaxProxyRequests {
			limit = l
		}
	}

	filter := proxy.RequestFilter{
		ProjectDir: projectDir,
		Limit:      limit,
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
