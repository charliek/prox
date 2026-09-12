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
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
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
	registry *Registry
	proxy    *DynamicProxy
	// managers holds the per-project request rings (D13, #49): one full-capacity
	// ring per registered project, so one project's flood cannot evict another's
	// records. It replaces the single shared RequestManager. Guarded by its own
	// lock (see Managers); the hot path reaches it via DynamicProxy, never
	// touching lifecycleMu.
	managers *Managers
	// captureMgr is the daemon's single shared capture manager (the same instance
	// held by the DynamicProxy). syncCaptureBudget pushes the registry's effective
	// disk budget onto it after every committed registry mutation (#69). nil when
	// capture is disabled (home dir unresolved / init failure).
	captureMgr *proxy.CaptureManager
	// captureInitErr is the reason captureMgr is nil, when it failed to
	// initialize at daemon startup (plan 012 D1, C4) -- as opposed to a project
	// simply choosing capture off in its own config. Empty when capture
	// initialized fine (or the daemon never tried, e.g. in older-shaped tests).
	// Surfaced via /status as capture_available=false + capture_error.
	captureInitErr string

	// --- hub mode (plan 031 C3) ---
	// hubMu guards the hub-mode fields below. It is a LEAF in the daemon's lock
	// order (lifecycleMu → hubMu): hubEnabled() is consulted from inside
	// lifecycle transactions, so hubMu must never be held while taking
	// lifecycleMu, and never across I/O — StartHub binds its listener before
	// taking it, and StopHub copies the server/listener out, releases the lock,
	// and only THEN shuts the server down. Holding it across Shutdown would
	// deadlock against an in-flight network handler waiting to read the token.
	//
	// hubLifecycleMu sits ABOVE hubMu and serializes whole hub lifecycle
	// TRANSACTIONS — start, stop, and token rotation — against each other (plan
	// 031 F14). hubMu alone was not enough: `hub start` read the token, released
	// the lock, bound a listener, and only then wrote the token back, so a
	// rotation landing in that window was silently overwritten and the revoked
	// credential kept working. Order: hubLifecycleMu → lifecycleMu → registry.mu,
	// and hubLifecycleMu → hubMu; nothing takes it while holding any of them.
	hubLifecycleMu sync.Mutex

	hubMu sync.RWMutex
	// hubCfg is the configuration the running hub is serving (its Listen is the
	// CONFIGURED address; HubListenAddr reports the bound one, which differs
	// when the port is 0). Zero when hub mode is off.
	hubCfg HubConfig
	// hubToken is the bearer credential the network mount currently accepts.
	// Rotation replaces it in place with no grace (D18), which is why the
	// middleware reads it per request rather than closing over it.
	hubToken     string
	hubListener  net.Listener
	hubServer    *http.Server
	hubStartedAt time.Time

	// tunnels is the hub's reverse-tunnel session manager (plan 031 C4). It is
	// always non-nil, so every caller can use it without a guard; on a daemon
	// that never turns hub mode on it simply stays empty.
	//
	// Its own mutex is a LEAF below lifecycleMu and registry.mu (D17): this
	// server never touches it while holding either. The two places that would
	// have been tempted to — the takeover path in register, and StopHub's
	// cleanup — both collect the sessions to close and close them AFTER
	// releasing the higher locks.
	tunnels *tunnelSessions
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
		tunnels:       newTunnelSessions(),
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

// SetManagers sets the per-project request-ring set (called during daemon
// setup). The same set is shared with the DynamicProxy so the hot path and the
// control-plane endpoints resolve the identical per-project rings.
func (s *Server) SetManagers(ms *Managers) {
	s.managers = ms
}

// SetCaptureManager sets the daemon's shared capture manager (called during
// daemon setup) so syncCaptureBudget can push the registry's effective disk
// budget onto it (#69). It is the same instance the DynamicProxy holds.
func (s *Server) SetCaptureManager(cm *proxy.CaptureManager) {
	s.captureMgr = cm
}

// SetCaptureInitError records why the daemon's capture manager failed to
// initialize at startup (plan 012 D1, C4), so /status can distinguish
// "capture unavailable in the daemon" from a project's own capture config
// being off. Called with an empty string when init succeeded (or was never
// attempted); a no-op either way beyond storing the string.
func (s *Server) SetCaptureInitError(msg string) {
	s.captureInitErr = msg
}

// syncCaptureBudget recomputes the effective daemon-wide capture disk budget from
// the current registry and pushes it onto the capture manager, which enforces it
// immediately (evicting oldest record groups if the new bound is lower). It must
// be called after EVERY committed registry mutation — successful register (incl.
// changed-config and stale-replacement), rollback/restore after a failed
// register, deregister, and stale-PID removal — so the bound converges as
// projects come and go (#69). No-op when capture is disabled. Cheap and
// idempotent, so over-calling within one transaction is harmless.
func (s *Server) syncCaptureBudget() {
	if s.captureMgr == nil || s.registry == nil {
		return
	}
	s.captureMgr.SetDiskBudget(s.registry.EffectiveCaptureDiskBudget())
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
	s.router.Get("/health", s.handleHealth)

	s.router.Route("/api/v1", func(r chi.Router) {
		r.Post("/register", s.handleRegister)
		r.Post("/deregister", s.handleDeregister)
		r.Get("/status", s.handleStatus)
		r.Get("/routes", s.handleRoutes)
		r.Post("/shutdown", s.handleShutdown)
		r.Get("/requests/stream", s.handleStreamRequests)
		r.Get("/requests", s.handleGetRequests)
		// Hub-mode control is SOCKET-ONLY (plan 031 D6): a remote publisher must
		// never be able to reconfigure, or switch off, the hub it publishes
		// through. The network mount is a separate router that simply does not
		// carry these routes (see hub_server.go), so this is structural rather
		// than a subtraction someone can forget.
		r.Post("/hub/start", s.handleHubStart)
		r.Post("/hub/stop", s.handleHubStop)
		r.Get("/hub/status", s.handleHubStatus)
		r.Post("/hub/token", s.handleHubRotateToken)
	})
}

// handleHealth answers /health on BOTH mounts. On the network mount it is the
// one route that carries no bearer-token middleware, so a publisher can probe
// reachability (and the hub's version) before it holds a credential.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": s.version,
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

	// The network-mount fields are not part of the socket contract (plan 031
	// §4.3). Clearing Origin here is what keeps a local registration's key a
	// BARE directory: a socket client cannot claim an origin, so it can never
	// produce an origin-qualified key, and conversely no composed key can ever
	// name a local project (D15).
	req.Origin = ""
	req.Takeover = false

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

	// The second half of the key-space invariant (plan 031 F2): every hub key
	// starts with hubKeyPrefix, so no LOCAL key may. Refusing it here — rather
	// than arguing about what an absolute path can look like on some platform —
	// is what makes "no origin/dir composition can name a local project"
	// structurally true instead of true by coincidence.
	if isHubProjectKey(req.ProjectDir) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("project_dir must not begin with %q (reserved for hub registrations)", hubKeyPrefix),
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

// registerOutcome is what one register transaction produced, beyond the HTTP
// answer: the tunnel work that must happen AFTER lifecycleMu is released.
type registerOutcome struct {
	status int
	body   any
	// displaced names remote publishers whose registrations this call removed
	// and COMMITTED, each paired with the lease GENERATION its registration
	// held. Their tunnels are closed by the caller, outside the lock, and only
	// at that exact generation (plan 031, review B1): a publisher that already
	// re-registered and reattached between the removal here and the close there
	// owns a newer session that this call has no business touching.
	// A takeover that later failed and was rolled back names nobody here — its
	// losers are back in the registry and must keep their tunnels (plan 031 F6).
	displaced []tunnelRef
	// restored names registrations put back from a snapshot: the newcomer's own
	// prior registration after a failed re-register, or displaced holders after
	// a failed takeover. Their lease is re-checked against the session manager
	// outside the lock, because a session that closed while the registration was
	// briefly absent found nothing to mark disconnected.
	restored []string
}

// register runs the register lifecycle transaction and then does the tunnel
// work the transaction could not do while holding the lock.
//
// The split exists for the lock order (plan 031 D17): the transaction holds
// lifecycleMu, and the tunnel session mutex is a LEAF that must never be taken
// underneath it — nor held across a session close, which is I/O. So the
// transaction only NAMES the publishers whose sessions need attention and this
// wrapper acts once the lock is released.
func (s *Server) register(req RegisterRequest) (int, any) {
	out := s.registerTransaction(req)
	for _, ref := range out.displaced {
		if s.closeTunnelGen(ref.key, ref.gen) {
			s.logger.Info("closed the tunnel of a displaced publisher",
				"project", ref.key, "generation", ref.gen)
		}
	}
	for _, key := range out.restored {
		s.reconcileTunnelLease(key)
	}
	return out.status, out.body
}

// reconcileTunnelLease re-syncs a restored registration against the session
// manager (plan 031 F3/F6).
//
// A registration that was briefly out of the registry — removed by a
// replacement or a takeover that then failed — is invisible to
// MarkDisconnected, so a session that closed during that window marked nothing
// and the restored registration would claim a tunnel that no longer exists:
// connected forever, unserveable, and unsweepable (the disconnect lease needs a
// DisconnectedAt that only MarkDisconnected sets). Asking the session manager
// what is actually installed, once, closes that.
//
// It runs OUTSIDE lifecycleMu, which is what keeps the session mutex a leaf.
//
// It COMPARES GENERATIONS rather than accepting any installed session (plan 031,
// review B3). "A tunnel exists for this key" is not the question — a tunnel of a
// DIFFERENT generation than the one the restored lease names is a different
// session entirely, and treating it as proof of connectedness is how a
// registration ends up pinned to a generation nobody holds: connected forever,
// unserveable, and unsweepable, because MarkDisconnected no-ops on a generation
// mismatch and the sweep needs a DisconnectedAt only it can set.
func (s *Server) reconcileTunnelLease(key string) {
	if s.registry == nil {
		return
	}
	lease, ok := s.registry.RemoteLease(key)
	if !ok {
		return
	}
	t := s.tunnels.get(key)

	if t == nil {
		// Nothing installed. A lease that claims a live session is claiming one
		// that closed while the registration was briefly out of the registry.
		if lease.SessionGen == 0 || !lease.DisconnectedAt.IsZero() {
			return
		}
		if s.registry.MarkDisconnected(key, lease.SessionGen, time.Now()) {
			s.logger.Info("marked a restored registration disconnected: its tunnel closed while it was being replaced",
				"project", key, "generation", lease.SessionGen)
		}
		return
	}

	if t.gen == lease.SessionGen && lease.DisconnectedAt.IsZero() {
		return // already in sync with the session that is actually installed
	}
	// A session IS installed, under a generation the restored lease does not
	// name (or names as disconnected). The installed session is the truth:
	// adopt it, so the registration is connected to the tunnel that exists
	// instead of to the one the snapshot remembered. MarkConnected refuses an
	// OLDER generation, which is the guard that keeps this from rolling a live
	// registration backwards.
	if s.registry.MarkConnected(key, t.gen) {
		s.logger.Info("re-synced a restored registration onto the tunnel that is actually installed",
			"project", key, "lease_generation", lease.SessionGen, "session_generation", t.gen)
	}
}

// registerTransaction runs the full register-through-listener-start lifecycle
// transaction under lifecycleMu (registry mutation, crash-restart self-heal,
// cert generation, listener add, and all rollbacks) and returns the HTTP status
// and response body for the caller to write once the lock is released.
// Serializing the whole transaction means a concurrent sweep tick can't tear
// down the new generation's listeners or purge its records mid-flow.
//
// The returned registerOutcome names the tunnel work the caller must do once
// the lock is released (displaced publishers' sessions, restored registrations'
// leases).
func (s *Server) registerTransaction(req RegisterRequest) (out registerOutcome) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// A register that queued behind a shutdown decision must not report
	// success from a daemon that is already exiting.
	if s.isShuttingDown() {
		return registerOutcome{status: http.StatusServiceUnavailable, body: ErrorResponse{
			Error: "daemon is shutting down; retry to start a fresh daemon",
			Code:  "SHUTTING_DOWN",
		}}
	}

	// D4 — and the ATOMICITY of D4 (plan 031, review B2). The hub's domain and
	// data-plane ports are read HERE, inside the lifecycle transaction, and the
	// routes are committed a few dozen lines below without ever releasing the
	// lock. startHub takes the same lock around its publisher-count check and its
	// commit, so the two can no longer interleave: previously the handler
	// snapshotted the config, startHub saw zero publishers, and this transaction
	// then committed routes built from the config startHub had already replaced —
	// the "status reports a config the routes do not use" state the reconfigure
	// guard exists to prevent.
	var hubInfo *RegisterHubInfo
	if req.Origin != "" {
		hub := s.hubConfigSnapshotLocked()
		if !hub.enabled {
			// Belt and braces: the listener is torn down before this can be
			// observed, but a request in flight during StopHub must not register.
			return registerOutcome{status: http.StatusServiceUnavailable, body: ErrorResponse{
				Error: "hub mode is not enabled",
				Code:  "HUB_DISABLED",
			}}
		}
		req.Domain = hub.cfg.Domain
		req.HTTPSPort = hub.cfg.HTTPSPort
		req.HTTPPort = hub.cfg.HTTPPort
		// The publisher has no way to know where its names were published, since
		// the three facts above were just overwritten. Hand them back on the
		// success body — the only place they are added, so every socket response
		// is untouched (plan 031 C5).
		hubInfo = &RegisterHubInfo{
			Domain:    hub.cfg.Domain,
			HTTPSPort: hub.cfg.HTTPSPort,
			HTTPPort:  hub.cfg.HTTPPort,
		}
	}
	// successBody builds a 200 body, attaching the hub facts for a remote caller.
	successBody := func(hostnames []string) RegisterResponse {
		return RegisterResponse{Registered: hostnames, Warnings: s.currentWarnings(), Hub: hubInfo}
	}

	// Cross-publisher name collisions are settled BEFORE the registry sees the
	// request (plan 031 D10), because the registry's own route-conflict error
	// cannot tell "a connected publisher owns this name" from "a publisher that
	// went away last week still has a row". Only remote registrations go through
	// it: a socket registration's names are the local machine's own business.
	//
	// The losers are removed but NOT finalized here (plan 031 F6): their rings
	// survive, their tunnels stay open, and their snapshots are kept, so any
	// later failure in this transaction can put them back exactly as they were.
	// A takeover that displaces three publishers and then fails to bind a port
	// must not leave four projects unpublished.
	var displacedSnaps []projectSnapshot
	if req.Origin != "" {
		losers, err := s.resolveHubNameCollisionsLocked(req)
		if err != nil {
			var held *HubNameHeldError
			if errors.As(err, &held) {
				return registerOutcome{status: http.StatusConflict, body: ErrorResponse{
					Error:   held.Error(),
					Code:    "HUB_NAME_HELD",
					Holders: held.Holders,
				}}
			}
			return registerOutcome{status: http.StatusConflict, body: ErrorResponse{
				Error: err.Error(), Code: "REGISTRATION_CONFLICT",
			}}
		}
		displacedSnaps = losers
	}

	// restoreDisplaced puts every displaced holder back and reports their keys,
	// for any failure arm below. It is the inverse of the removal loop in
	// resolveHubNameCollisionsLocked and runs only on a failure path.
	restoreDisplaced := func() []string {
		var keys []string
		for _, snap := range displacedSnaps {
			s.restoreSnapshotLocked(snap)
			keys = append(keys, snap.proj.Dir)
		}
		if len(keys) > 0 {
			s.syncCaptureBudget()
			s.logger.Warn("restored displaced hub registrations after the newcomer failed",
				"projects", keys, "claimed_by", req.Origin)
		}
		return keys
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
			return registerOutcome{
				status:   http.StatusConflict,
				body:     ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"},
				restored: restoreDisplaced(),
			}
		}
		if req.Origin != "" {
			// REMOTE conflict path (plan 031 P4/D17). The key is the composed
			// hub key, so a same-key conflict is by construction the SAME
			// publisher re-registering — identity is settled by the key itself,
			// and daemon.IsProcessAlive is never called: the holder's PID names
			// a process on the publisher's machine, where "alive here" would be
			// a coincidence and "dead here" would be a lie.
			if s.registry.registrationMatches(req) {
				// Config unchanged: a true no-op refresh, exactly like the local
				// idempotent arm — no listener churn, no record purge.
				//
				// The reservation is still renewed (plan 031 F5): this arm
				// answers 200 to a publisher that is demonstrably alive and may
				// not have attached yet, and without the renewal the sweep or a
				// newcomer could remove the very registration just confirmed.
				s.registry.RenewAttachReservation(conflict.Dir, constants.HubAttachGrace)
				// The PUBLISHING PROCESS may have changed even though the config
				// did not — a `prox up` restarted in the same directory
				// re-registers the same services under a new PID. Recording it
				// is what keeps the deregister guard meaningful (plan 031,
				// review B1): whoever registered last is who may deregister, so
				// an older generation's teardown cannot delete its successor,
				// and the current one can still remove its own.
				s.registry.RefreshRemoteOwner(conflict.Dir, req.PID, req.StartTime)
				s.logger.Info("idempotent remote re-register: config unchanged, no-op refresh",
					"project", conflict.Dir, "origin", req.Origin)
				return registerOutcome{
					status:    http.StatusOK,
					body:      successBody(s.registry.ProjectHostnames(conflict.Dir)),
					displaced: s.commitDisplacedLocked(displacedSnaps),
				}
			}
			// Config changed: replace. The removal, the re-registration and the
			// LEASE CARRY are ONE registry operation (plan 031 F3) — a tunnel
			// attaching mid-replace used to land on the new registration and
			// then be clobbered by the stale lease written afterwards, leaving a
			// registration permanently "connected" to a session that did not
			// exist. Nothing physical has happened yet if it fails.
			s.logger.Info("remote re-register: config changed, replacing registration",
				"project", conflict.Dir, "origin", req.Origin)
			res, rerr := s.registry.ReplaceRegistration(conflict.Dir, req)
			if rerr != nil {
				s.syncCaptureBudget()
				return registerOutcome{
					status:   http.StatusConflict,
					body:     ErrorResponse{Error: rerr.Error(), Code: "REGISTRATION_CONFLICT"},
					restored: restoreDisplaced(),
				}
			}
			snap := res.Snapshot
			restoreSnap = &snap
			// The registry committed; now do the removal's physical half (close
			// listeners the removal emptied, purge the old records) exactly as
			// removeProjectLocked would have.
			s.lifecycleEpoch.Add(1)
			s.finishRemoval(conflict.Dir, res.EmptyPorts)
			hostnames, newPorts, err = res.Hostnames, res.NewPorts, nil
		} else if daemon.IsProcessAlive(conflict.PID, conflict.StartTime) {
			// The same-dir holder is still live. Sub-cases:
			//   - SAME generation as the requester (same PID AND matching non-zero
			//     start token): an idempotent re-register (D6a) — the heal path
			//     (D6b) re-registering against a live daemon whose SSE stream broke,
			//     or a client retrying a register it already won.
			//   - A DIFFERENT generation (different PID, or an unreadable token on
			//     either side that can't be proven identical): a genuine second
			//     `prox up` in the same dir — stays a hard 409.
			if !sameRegistrationIdentity(conflict.PID, conflict.StartTime, req.PID, req.StartTime) {
				return registerOutcome{
					status:   http.StatusConflict,
					body:     ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"},
					restored: restoreDisplaced(),
				}
			}
			// FIX 1: when the re-registering process's config is UNCHANGED (the
			// common heal case), take a true no-op refresh — echo the current
			// hostnames without any remove+add, so no listeners churn and, crucially,
			// no daemon-side records are purged. This is what a heal against a live
			// daemon whose SSE stream merely broke actually needs.
			if s.registry.registrationMatches(req) {
				s.logger.Info("idempotent re-register: config unchanged, no-op refresh",
					"project", conflict.Dir, "pid", conflict.PID, "start_time", conflict.StartTime)
				// The no-op arm returns BEFORE the cert phase, so it generates no
				// warnings of its own — but it must still report the daemon's current
				// ones. This is the self-heal path (cli.proxy_runtime), and a client
				// reconnecting here is exactly a client that has not seen them yet.
				return registerOutcome{
					status:    http.StatusOK,
					body:      successBody(s.registry.ProjectHostnames(conflict.Dir)),
					displaced: s.commitDisplacedLocked(displacedSnaps),
				}
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
		// race, so this is a correctness guarantee, not a hopeful retry. The remote
		// replace arm above did its own atomic swap and set err to nil, so it does
		// not come through here.
		if err != nil {
			hostnames, newPorts, err = s.registry.Register(req)
		}
		if err != nil {
			var restored []string
			if restoreSnap != nil {
				s.restoreSnapshotLocked(*restoreSnap)
				restored = append(restored, restoreSnap.proj.Dir)
			}
			restored = append(restored, restoreDisplaced()...)
			// The prior removal (and any restore) committed a registry change;
			// re-sync the effective capture disk budget (#69).
			s.syncCaptureBudget()
			return registerOutcome{
				status:   http.StatusConflict,
				body:     ErrorResponse{Error: err.Error(), Code: "REGISTRATION_CONFLICT"},
				restored: restored,
			}
		}
	}

	// The registry now holds this project's routes, so the hot path can resolve
	// them. Ensure its per-project ring exists BEFORE the cert/listener phases so
	// records captured while the routes are briefly visible on already-bound
	// shared-port listeners land in the ring (and are purged on rollback), and so
	// the no-op idempotent refresh — which returned earlier without reaching this
	// point — keeps the project's existing ring untouched. ensure is idempotent:
	// the config-changed and crash-replace arms above kept the manager, and this
	// returns it unchanged.
	if s.managers != nil {
		s.managers.ensure(req.ProjectDir)
	}

	// Ensure a cert exists for the registration's domain whenever it registers any
	// HTTPS route. Gating on req.HTTPSPort > 0 (not on a NEW listener port) is the
	// #58 fix: a domain joining an already-bound shared HTTPS port has no new port
	// in newPorts, so the old loop skipped its cert and the SNI handshake failed.
	// EnsureDomain is idempotent (one cert per base domain), so re-calls are cheap.
	if s.proxy != nil && s.proxy.certMgr != nil && req.HTTPSPort > 0 {
		if err := s.proxy.certMgr.EnsureDomain(req.Domain); err != nil {
			restored := s.unwindRegistration(req.ProjectDir, restoreSnap)
			restored = append(restored, restoreDisplaced()...)
			return registerOutcome{
				status: http.StatusInternalServerError,
				body: ErrorResponse{
					Error: fmt.Sprintf("failed to generate certs for %s: %v", req.Domain, err),
					Code:  "CERT_GENERATION_FAILED",
				},
				restored: restored,
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
				restored := s.unwindRegistration(req.ProjectDir, restoreSnap)
				restored = append(restored, restoreDisplaced()...)
				return registerOutcome{
					status: http.StatusInternalServerError,
					body: ErrorResponse{
						Error: fmt.Sprintf("failed to bind port %d: %v", ps.Port, err),
						Code:  "PORT_BIND_FAILED",
					},
					restored: restored,
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
	// The registry now reflects this project; fold its capture disk budget into
	// the effective daemon-wide bound and enforce it (#69). Covers the fresh,
	// changed-config, and stale-replacement register arms (all reach here).
	s.syncCaptureBudget()

	// A freshly accepted remote registration is reserved for HubAttachGrace, and
	// so is a re-registered one whose lease was just carried across a replace:
	// the carried ReservedUntil belongs to the PREVIOUS registration and may
	// already have lapsed (plan 031 F5). RenewAttachReservation no-ops on a
	// connected registration, whose tunnel is its lease.
	//
	// It happens HERE, immediately before the successful return, rather than
	// before the cert and listener phases (plan 031, review B7). Certificate
	// generation can take seconds on a cold certs directory and a listener bind
	// retries briefly; a reservation started before them can be most of the way
	// through its 10s grace by the time the publisher reads the 200, which would
	// answer "you are registered" about a registration a sweep or a newcomer
	// could already take. Starting the clock when the answer is sent gives the
	// publisher the whole grace to attach.
	if req.Origin != "" {
		s.registry.RenewAttachReservation(req.ProjectDir, constants.HubAttachGrace)
	}

	// The newcomer is committed, so the displaced holders are finally beyond
	// recovery: destroy their rings and let the caller close their tunnels
	// (plan 031 F6). Nothing before this point could have needed them back.
	displaced := s.commitDisplacedLocked(displacedSnaps)
	return registerOutcome{
		status:    http.StatusOK,
		body:      successBody(hostnames),
		displaced: displaced,
	}
}

// resolveHubNameCollisionsLocked applies D10 to a remote registration and
// returns the keys of the registrations it removed to make room. lifecycleMu
// must be held; the losers' TUNNELS are closed by the caller after it is
// released (D17 — the session mutex is a leaf).
//
// The rules, in the order they matter:
//
//   - A LOCAL holder is never displaced, with or without takeover. The hub host
//     is someone's own machine first and a hub second; a remote publisher must
//     not be able to take a name out from under the developer sitting at it.
//   - A holder that is CONNECTED, or still inside its attach reservation (P3),
//     needs consent: without takeover this is a 409 HUB_NAME_HELD listing every
//     conflicting holder.
//   - A holder that is neither is stale and is taken silently.
//
// Removal is all-or-nothing in both directions. If ANY name is blocked, nothing
// is removed and the whole registration is refused — a half-published project
// is the outcome nobody wants. And a loser is removed ENTIRELY, including the
// names that did NOT collide, for the same reason: leaving a displaced
// publisher half-registered would publish a project that no longer works.
func (s *Server) resolveHubNameCollisionsLocked(req RegisterRequest) (displaced []projectSnapshot, err error) {
	if s.registry == nil {
		return nil, nil
	}

	var blocked []HubHolder
	losers := make(map[string]struct{})
	for _, c := range s.registry.HubNameCollisions(req) {
		switch {
		case c.Local:
			blocked = append(blocked, c.Holder)
		case c.Active && !req.Takeover:
			blocked = append(blocked, c.Holder)
		default:
			losers[c.HolderKey] = struct{}{}
		}
	}
	if len(blocked) > 0 {
		return nil, &HubNameHeldError{Holders: blocked}
	}

	keys := make([]string, 0, len(losers))
	for key := range losers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// SNAPSHOT AND REMOVE IN ONE REGISTRY OPERATION (plan 031 F6, tightened
		// by review B3). The newcomer has not registered, generated a cert, or
		// bound a listener yet, and any of those can still fail — at which point
		// every holder displaced here must go back exactly as it was. Their
		// RINGS are kept (removeDisplacedLocked purges nothing) and their
		// TUNNELS are left open for the same reason; both are finalized by
		// commitDisplacedLocked once the newcomer is committed.
		//
		// The two halves were once two registry calls, and a tunnel attaching
		// between them left the snapshot carrying a lease older than the session
		// the manager had just installed — so a rollback restored a registration
		// pinned to a generation nobody held.
		snap, removed, ports, ok := s.removeDisplacedLocked(key)
		if !ok {
			continue
		}
		displaced = append(displaced, snap)
		s.logger.Warn("removed a hub registration that lost a name collision",
			"project", key, "takeover", req.Takeover,
			"claimed_by", req.Origin, "removed_hostnames", removed, "closed_ports", ports)
	}
	if len(displaced) > 0 {
		s.syncCaptureBudget()
	}
	return displaced, nil
}

// removeDisplacedLocked removes a registration that LOST a name collision,
// keeping everything a restore would need (plan 031 F6).
//
// It is removeProjectLocked minus the record purge. The purge is what makes an
// ordinary removal final, and a displaced holder is not final yet: the newcomer
// can still fail in the registry, cert or listener phase, and the holder then
// goes back whole — routes, ring AND the requests it had captured. On the
// newcomer's commit, destroyProjectManager purges the records and their body
// files exactly as the ordinary path would have. Listeners the removal emptied
// ARE closed here, because restoreSnapshotLocked re-opens them and because the
// newcomer usually wants the very same port.
func (s *Server) removeDisplacedLocked(key string) (snap projectSnapshot, removedHostnames []string, emptyPorts []int, ok bool) {
	if s.registry == nil {
		return projectSnapshot{}, nil, nil, false
	}
	snap, removedHostnames, emptyPorts, ok = s.registry.SnapshotAndDeregister(key)
	if !ok {
		return projectSnapshot{}, nil, nil, false
	}
	s.lifecycleEpoch.Add(1)
	if s.proxy != nil {
		for _, port := range emptyPorts {
			if err := s.proxy.RemoveListener(port); err != nil {
				s.logger.Warn("failed to remove listener", "port", port, "error", err)
			}
		}
	}
	return snap, removedHostnames, emptyPorts, true
}

// commitDisplacedLocked finalizes the holders a successful takeover displaced
// (plan 031 F6): their rings are destroyed here and their keys — each with the
// lease GENERATION its registration held — returned so the caller can close
// exactly those tunnels once lifecycleMu is released (review B1). It runs ONLY
// after the newcomer has committed — before that point every one of them may
// still have to be restored. lifecycleMu must be held.
func (s *Server) commitDisplacedLocked(displaced []projectSnapshot) []tunnelRef {
	if len(displaced) == 0 {
		return nil
	}
	refs := make([]tunnelRef, 0, len(displaced))
	for _, snap := range displaced {
		s.destroyProjectManager(snap.proj.Dir)
		refs = append(refs, tunnelRef{key: snap.proj.Dir, gen: snap.proj.SessionGen})
	}
	s.syncCaptureBudget()
	return refs
}

// removeDisconnectedRemote is the lease sweep's removal path (plan 031 D3/D17):
// it removes a remote registration only if it is still disconnected past the
// grace AND its session generation is unchanged since the sweep observed it.
//
// It deliberately does NOT go through removeStaleProject. That path's guard is
// (dir, PID, start token), all of which survive a tunnel reattach, so it would
// happily delete a publisher that reconnected between detection and removal
// (P2). DeregisterIfDisconnected guards on the one identity a reattach changes.
func (s *Server) removeDisconnectedRemote(key string, sessionGen uint64) (removed bool, removedHostnames []string, emptyPorts []int) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.isShuttingDown() || s.registry == nil {
		return false, nil, nil
	}
	removed, removedHostnames, emptyPorts = s.registry.DeregisterIfDisconnected(key, sessionGen)
	if !removed {
		return false, nil, nil
	}
	s.lifecycleEpoch.Add(1)
	s.finishRemoval(key, emptyPorts)
	s.destroyProjectManager(key)
	s.syncCaptureBudget()
	return true, removedHostnames, emptyPorts
}

// currentWarnings returns the daemon's user-facing warnings for a register
// response, deduped. The warnings live on the cert manager because that is where
// they are observed and because they are CA-scoped rather than per-project: every
// registration gets the same current set, including registrations that generated
// no certs at all (a warm certs dir) and the no-op refresh that never reaches the
// cert phase.
//
// Callers hold lifecycleMu; the holder's mutex is a leaf taken only for a slice
// copy, so this never blocks the lifecycle transaction on cert generation.
func (s *Server) currentWarnings() []domain.Warning {
	if s.proxy == nil || s.proxy.certMgr == nil {
		return nil
	}
	return domain.DedupeWarnings(s.proxy.certMgr.Warnings())
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

	// The socket mount addresses projects by their bare dir; the network mount
	// composes its key from the caller's own origin (deregisterProject is
	// shared, the key derivation is not — D15).
	s.deregisterProject(w, req.ProjectDir, req.PID)
}

// deregisterProject is the shared body of both mounts' deregister handler: it
// removes the project the caller's key names, reports the removed hostnames,
// and schedules the empty-daemon shutdown check. The CALLER resolves the key,
// which is the whole authorization story on the network mount (plan 031 D15).
func (s *Server) deregisterProject(w http.ResponseWriter, projectKey string, pid int) {
	// GUARDED by the caller's own PID, and reporting the lease generation it
	// removed (plan 031, review B1). Both halves close the same race from
	// opposite ends: a `prox down` whose registration has already been replaced
	// — a crashed-and-restarted `prox up` in the same directory, a publisher
	// that re-registered under a new PID — used to delete its SUCCESSOR's
	// registration, and then close the successor's tunnel on the way out.
	res := s.removeProjectOwned(projectKey, pid)

	if res.Mismatch {
		s.logger.Warn("ignored a deregister from a process that no longer owns the registration",
			"project", projectKey, "pid", pid)
	}

	// An explicit deregister ends the publisher's lease, so its tunnel has
	// nothing left to serve (plan 031 F10). Leaving it open leaked a yamux
	// session, its keepalive goroutines, its watcher, its per-session transport
	// and the file descriptors under all of them until the publisher happened to
	// disconnect or the hub stopped — and `prox down` followed by `prox up` on a
	// long-lived hub did it every time.
	//
	// Only the generation this deregister actually removed is closed: a session
	// installed since then belongs to a registration this call never touched.
	//
	// OUTSIDE lifecycleMu, which removeProjectOwned has already released: the
	// session mutex is a leaf and a session close is I/O (D17). A no-op for a
	// local key, which never has a session.
	if res.Removed {
		s.closeTunnelGen(projectKey, res.SessionGen)
	}

	s.logger.Info("deregistered project",
		"project", projectKey,
		"pid", pid,
		"removed_hostnames", res.Hostnames,
		"closed_ports", res.EmptyPorts,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"removed": res.Hostnames,
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
// It is also a no-op while HUB MODE is on (plan 031 D13): a hub exists to be
// available for publishers that are not registered yet, so "no routes left"
// says nothing about whether the daemon is still needed. `prox hub stop` (or a
// signal) is what ends a hub daemon's life, and it schedules this check itself
// once the network listener is closed.
func (s *Server) scheduleShutdownWhenEmpty() {
	if s.registry == nil || !s.registry.IsEmpty() {
		return
	}
	if s.hubEnabled() {
		s.logger.Info("no routes registered, but hub mode is on; staying alive")
		return
	}
	s.logger.Info("no routes registered, scheduling shutdown check")
	epoch := s.lifecycleEpoch.Load()
	go func() {
		time.Sleep(s.shutdownDelay)
		// Re-check under lifecycleMu so an in-flight register transaction
		// finishes before the decision, and stand down if ANY lifecycle
		// mutation happened since scheduling — a newer empty period gets its
		// own timer with the full grace. Hub mode is re-checked here too: it can
		// be switched on during the grace, and a timer from before that must not
		// take the daemon down under it.
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.lifecycleEpoch.Load() == epoch && s.registry.IsEmpty() && !s.hubEnabled() {
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
	removedHostnames, emptyPorts = s.removeProjectLocked(projectDir)
	// Genuine deregister: tear down the project's per-project ring after the
	// shared removeProjectLocked purged its records. The register-inline arms
	// (config-changed / crash-replace re-register) call removeProjectLocked
	// directly and KEEP the ring — only this top-level entry destroys it.
	s.destroyProjectManager(projectDir)
	// The project left the registry; recompute the effective capture disk budget
	// (a departing project can only relax the daemon-wide min) (#69).
	s.syncCaptureBudget()
	return removedHostnames, emptyPorts
}

// removeProjectOwned is removeProject for an explicit deregister: the removal is
// guarded on pid still naming the registration's owner, and it reports the lease
// generation the removed registration held so the caller can close that exact
// tunnel session (plan 031, review B1). A guarded no-op leaves the live
// generation's routes, ring and tunnel completely untouched.
func (s *Server) removeProjectOwned(projectKey string, pid int) DeregisterResult {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// Once teardown has begun, physical cleanup is the exiting daemon's job (see
	// removeProject).
	if s.isShuttingDown() || s.registry == nil {
		return DeregisterResult{}
	}
	res := s.registry.DeregisterOwned(projectKey, pid)
	if !res.Removed {
		return res
	}
	s.lifecycleEpoch.Add(1)
	s.finishRemoval(projectKey, res.EmptyPorts)
	// Genuine deregister: tear down the project's per-project ring after
	// finishRemoval purged its records.
	s.destroyProjectManager(projectKey)
	// The project left the registry; recompute the effective capture disk budget
	// (a departing project can only relax the daemon-wide min) (#69).
	s.syncCaptureBudget()
	return res
}

// removeProjectLocked is removeProject's body; lifecycleMu must be held. It lets
// register run a stale-registration replace while already holding the mutex.
// It purges the project's records but KEEPS its ring manager (the re-register
// arms reuse it); only the top-level removeProject destroys the manager.
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
	removed, removedHostnames, emptyPorts = s.removeStaleProjectLocked(projectDir, pid, startTime)
	// Only destroy the ring when a removal actually happened: a guarded no-op
	// (the project re-registered under a live PID between detection and removal)
	// must leave the live generation's ring — and its records — untouched.
	if removed {
		s.destroyProjectManager(projectDir)
		// A stale project left the registry; recompute the effective capture disk
		// budget (#69).
		s.syncCaptureBudget()
	}
	return removed, removedHostnames, emptyPorts
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
// captured requests from its per-project ring (firing eviction callbacks so
// on-disk body files are cleaned up). It KEEPS the ring manager itself — the
// register-inline re-register arms reuse it. Genuine removal additionally
// destroys the manager via destroyProjectManager.
func (s *Server) finishRemoval(projectDir string, emptyPorts []int) {
	if s.proxy != nil {
		for _, port := range emptyPorts {
			if err := s.proxy.RemoveListener(port); err != nil {
				s.logger.Warn("failed to remove listener", "port", port, "error", err)
			}
		}
	}

	s.managers.purge(projectDir)
}

// destroyProjectManager tears down a project's request ring on genuine removal
// (explicit deregister, stale-PID sweep, or a fresh registration's rollback):
// it Closes the manager FIRST — releasing any daemon-side SSE handler blocked on
// its subscription — then removes it from the set so later hot-path lookups
// return nil. finishRemoval (or rollbackRegistration) already purged the
// records+bodies for the common path; the final PurgeByProject on the detached
// manager is a safety sweep so a record that landed in the narrow window between
// that purge and the Close doesn't orphan its on-disk body file. lifecycleMu
// must be held. No-op when the project has no ring.
func (s *Server) destroyProjectManager(projectDir string) {
	if s.managers == nil {
		return
	}
	if mgr := s.managers.remove(projectDir); mgr != nil {
		mgr.PurgeByProject(projectDir)
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
	s.managers.purge(projectDir)
	s.lifecycleEpoch.Add(1)
	s.scheduleShutdownWhenEmpty()
}

// unwindRegistration reverses a registration attempt that failed AFTER its ring
// was ensured (cert or listener phase): it rolls back the registry entry and
// purges the ring, then either restores the project's prior registration (the
// same-identity config-change replace arm) or destroys the freshly-ensured ring
// (a brand-new project's rollback). lifecycleMu must be held.
// It returns the keys it restored, so the caller can reconcile their tunnel
// leases outside lifecycleMu (plan 031 F3).
func (s *Server) unwindRegistration(projectDir string, restoreSnap *projectSnapshot) (restored []string) {
	if restoreSnap != nil {
		// One registry operation: drop the failed registration, put the previous
		// one back, and carry forward any lease the failed one accumulated (plan
		// 031 F3). Doing it as remove-then-restore left a window in which a
		// tunnel could attach to a registration that was about to be overwritten.
		s.managers.purge(projectDir)
		reopen := s.registry.RestoreReplaced(projectDir, *restoreSnap)
		if s.proxy != nil {
			for _, ps := range reopen {
				if err := s.addListenerWithBriefRetry(ps.Port, ps.Protocol); err != nil {
					s.logger.Error("failed to re-open listener while restoring a registration after a failed re-register",
						"project", restoreSnap.proj.Dir, "port", ps.Port, "protocol", ps.Protocol, "error", err)
				}
			}
		}
		s.lifecycleEpoch.Add(1)
		s.logger.Info("restored prior registration after a failed re-register",
			"project", restoreSnap.proj.Dir, "pid", restoreSnap.proj.PID)
		restored = append(restored, restoreSnap.proj.Dir)
	} else {
		s.rollbackRegistration(projectDir)
		s.destroyProjectManager(projectDir)
	}
	// Registry state changed (rolled back, and possibly restored to the prior
	// registration); recompute the effective capture disk budget (#69).
	s.syncCaptureBudget()
	return restored
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
	if s.managers != nil {
		// Dropped events are summed across every project's ring; per-project
		// record counts make the N×ring memory trade-off diagnosable (D13).
		resp.DroppedEvents = s.managers.droppedTotal()
		resp.RecordCounts = s.managers.recordCounts()
	}
	// Hub mode is reported only while it is ON, so a hub-less daemon's status
	// JSON is byte-for-byte what it was (plan 031 AC1).
	if hub := s.hubStatus(); hub.Enabled {
		resp.Hub = &hub
	}
	resp.CaptureAvailable = s.captureMgr != nil
	if s.captureMgr != nil {
		// Capture disk accounting for the one shared capture dir, read under a
		// single lock so used/budget always coexisted (#69).
		resp.CaptureDiskUsed, resp.CaptureDiskBudget = s.captureMgr.DiskStats()
	} else {
		resp.CaptureError = s.captureInitErr
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
	// Filter the stream by owning project (exact match). Scoping by project
	// dir rather than hostname prevents cross-project record delivery when two
	// projects own the same hostname on different ports. The param is
	// mandatory: an empty ProjectDir filter would resolve no ring, so a caller
	// that forgot the param would silently receive nothing.
	projectDir := r.URL.Query().Get("project")
	if projectDir == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "project query parameter is required",
			Code:  "BAD_REQUEST",
		})
		return
	}
	s.streamRequestsFor(w, r, projectDir)
}

// streamRequestsFor is the shared SSE body of both mounts' request stream. The
// caller supplies the already-resolved registry key: the socket mount passes
// the `project` param verbatim, the network mount passes
// HubProjectKey(caller origin, project), so a publisher that names someone
// else's project subscribes to its OWN ring rather than theirs (plan 031 D15).
// That is an accident-proofing property, not authentication: the origin is the
// caller's claim — see validateHubOrigin.
func (s *Server) streamRequestsFor(w http.ResponseWriter, r *http.Request, projectDir string) {
	if s.managers == nil {
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

	// Set SSE headers before resolving the ring so a missing-project stream
	// still returns 200 (the forwarder treats a clean stream end as a reconnect
	// signal, which is exactly right during a heal/deregister window).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// A project with no ring (never registered, or already deregistered)
	// subscribes to nothing and ends cleanly right after the headers rather than
	// blocking forever on a subscription no writer will ever feed (D13).
	mgr := s.managers.get(projectDir)
	if mgr == nil {
		s.logger.Info("requests stream for a project with no ring; ending cleanly", "project", projectDir)
		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()
		return
	}

	filter := proxy.RequestFilter{ProjectDir: projectDir}
	sub := mgr.Subscribe(filter)
	defer mgr.Unsubscribe(sub.ID)

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
	// Mandatory for the same reason as the stream endpoint: an empty
	// ProjectDir resolves no ring.
	projectDir := r.URL.Query().Get("project")
	if projectDir == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "project query parameter is required",
			Code:  "BAD_REQUEST",
		})
		return
	}
	s.getRequestsFor(w, r, projectDir)
}

// getRequestsFor is the shared snapshot body of both mounts' requests endpoint.
// As with the stream, the caller supplies the resolved registry key — the
// network mount composes it from the caller's own origin (plan 031 D15), so a
// publisher asking for another publisher's dir simply reads its OWN (empty)
// ring rather than anyone else's captured bodies.
func (s *Server) getRequestsFor(w http.ResponseWriter, r *http.Request, projectDir string) {
	if s.managers == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "request manager not available",
			Code:  "NOT_READY",
		})
		return
	}

	// A project with no ring (never registered, or already deregistered)
	// returns 200 with an empty list — a stable contract for the forwarder's
	// backfill during a heal/deregister window (D13). One log line records it.
	mgr := s.managers.get(projectDir)
	if mgr == nil {
		s.logger.Info("requests snapshot for a project with no ring; returning empty list", "project", projectDir)
		writeJSON(w, http.StatusOK, map[string]any{"requests": []proxy.RequestRecord{}})
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

	records := mgr.Recent(filter)
	if records == nil {
		records = []proxy.RequestRecord{}
	}
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
