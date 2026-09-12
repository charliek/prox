package proxyd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/go-chi/chi/v5"
)

// newHubRouter builds the NETWORK control plane's router (plan 031 D6).
//
// It is a SEPARATE chi router with its own handler set, deliberately not the
// socket router with routes subtracted: the allow-list is the whole security
// story of this mount, and "which routes did we remember to remove?" is the
// wrong question to have to answer. What a remote publisher can reach is
// exactly what is written below —
//
//	/health                     (no auth: a reachability probe)
//	/api/v1/register            origin-scoped, key composed here (D15)
//	/api/v1/deregister          origin-scoped, key composed here (D15)
//	/api/v1/status              unscoped, by decision (§8)
//	/api/v1/routes              unscoped, by decision (§8)
//	/api/v1/requests            origin-scoped, key composed here (D15)
//	/api/v1/requests/stream     origin-scoped, key composed here (D15)
//	/api/v1/tunnel              origin-scoped, key composed here (D15)
//
// — and NOTHING else. In particular there is no /api/v1/shutdown (a publisher
// must never be able to stop the daemon that serves everyone else) and no
// /api/v1/hub/* (a publisher must never be able to reconfigure or switch off
// the hub it publishes through). Both are 404 here and unchanged on the socket.
func (s *Server) newHubRouter() *chi.Mux {
	r := chi.NewRouter()

	// /health carries no auth: a publisher needs to be able to tell "hub is
	// reachable" from "hub rejected my credential" before it has one.
	r.Get("/health", s.handleHealth)

	r.Group(func(r chi.Router) {
		r.Use(s.hubAuthMiddleware)
		r.Route("/api/v1", func(r chi.Router) {
			r.Post("/register", s.handleHubRegister)
			r.Post("/deregister", s.handleHubDeregister)
			// status and routes are deliberately NOT origin-scoped: the hub
			// operator holds the token and `prox hub status` needs the whole
			// publisher list. Recorded as an accepted exposure in the plan's §8.
			r.Get("/status", s.handleStatus)
			r.Get("/routes", s.handleRoutes)
			r.Get("/requests", s.handleHubGetRequests)
			r.Get("/requests/stream", s.handleHubStreamRequests)
			// The reverse tunnel (plan 031 C4). It sits behind the same
			// bearer middleware as everything else and composes its key
			// from the caller's own headers, so a publisher can no more
			// attach a tunnel to another publisher's registration than it
			// can deregister one (D15).
			r.Post("/tunnel", s.handleHubTunnel)
		})
	})

	return r
}

// Bounds on what one control-plane request may assert (plan 031 F9). The
// network mount takes JSON from another machine, so "decode whatever arrives"
// is a memory-exhaustion hole and a registry full of megabyte-long strings is
// a log, status, and route table nobody can read.
const (
	// maxControlRequestBytes caps a control-plane JSON body. A register request
	// with the service cap below is a few kilobytes; 1 MiB is generous.
	maxControlRequestBytes = 1 << 20
	// hubMaxServices caps the service names one registration may claim. Each
	// one becomes a hostname, a route, and (for HTTPS) an SNI name on this
	// machine's listeners.
	hubMaxServices = 64
)

// decodeControlJSON reads exactly one JSON value from a bounded request body.
//
// Two rules beyond a plain Decode (plan 031 F9): the body is capped with
// http.MaxBytesReader so an unbounded upload cannot be buffered into memory,
// and TRAILING data is rejected — a body of "{...}{...}" must not quietly
// register the first object and discard whatever the second one was trying to
// say. The same shape Client.Requests already applies to responses.
func decodeControlJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlRequestBytes))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON body")
	}
	return nil
}

// validateHubRegistration applies to a NETWORK registration the same naming and
// target rules internal/config applies to a project's own prox.yaml (plan 031
// F9), through the one shared implementation in internal/domain.
//
// The socket mount can lean on the CLI having validated the config it is about
// to send; the network mount cannot lean on anything. Without this, a remote
// publisher could register a service named "*" or "../x" (a hostname the route
// table and the cert manager were never meant to see), a target host that is
// not a host at all, or ten thousand services.
func validateHubRegistration(req RegisterRequest) error {
	if err := validateHubProjectDir(req.ProjectDir); err != nil {
		return err
	}
	if req.PID <= 0 {
		return errors.New("pid must be a positive process id")
	}
	if len(req.Services) == 0 {
		return errors.New("at least one service is required")
	}
	if len(req.Services) > hubMaxServices {
		return fmt.Errorf("too many services: %d (max %d)", len(req.Services), hubMaxServices)
	}
	for _, name := range sortedKeys(req.Services) {
		target := req.Services[name]
		if err := domain.ValidateServiceName(name); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if err := domain.ValidateHost(target.Host); err != nil {
			return fmt.Errorf("service %q host: %w", name, err)
		}
		if err := domain.ValidatePort(target.Port); err != nil {
			return fmt.Errorf("service %q port: %w", name, err)
		}
	}
	if req.MaxBodySize < 0 {
		return fmt.Errorf("max_body_size must not be negative, got %d", req.MaxBodySize)
	}
	return nil
}

// sortedKeys returns a map's keys in sorted order, so a validation failure over
// a map reports the same offender every time rather than whichever one Go's
// randomized iteration reached first.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hubAuthMiddleware enforces the bearer token on every network-mount route it
// wraps (/health is registered outside it). Token comparison is
// constant-time: a byte-by-byte early exit would leak the token one character
// at a time to a caller that can measure the difference.
//
// `auth: none` skips the check entirely — the user's explicit choice for a
// private network (D6) — and is read per request, so `prox hub start --auth`
// takes effect on a rebind without restarting the daemon.
func (s *Server) hubAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode, token := s.hubAuthSnapshot()
		if mode == HubAuthNone {
			next.ServeHTTP(w, r)
			return
		}
		provided := bearerCredential(r.Header.Get("Authorization"))
		// An empty configured token can never be satisfied: it would otherwise
		// make a token-mode hub with no token accept an empty credential.
		if token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{
				Error: "missing or invalid bearer token for the hub control plane",
				Code:  "UNAUTHORIZED",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerCredential extracts the credential from an Authorization header,
// accepting the scheme case-insensitively as RFC 7235 requires. A header with
// any other scheme yields an empty credential, which never compares equal to a
// configured token.
func bearerCredential(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// handleHubRegister is the network mount's register (plan 031 §4.3).
//
// Everything security-relevant happens before the shared register path runs:
//
//  1. protocol_version is checked against constants.HubProtocolVersion, NOT the
//     binary version (D7) — machines upgrade at different times.
//  2. origin is validated and the registry key is COMPOSED here from the
//     caller's own origin and dir (D5/D15). A pre-qualified key on the wire is
//     not merely rejected: it cannot be expressed, because project_dir is only
//     ever used as the second half of the composition. This scopes an HONEST
//     caller to its own registrations and puts every local project out of
//     reach entirely; it does not authenticate the origin (validateHubOrigin).
//     The body is also bounded and validated (F9) with the same service-name,
//     target and port rules internal/config applies to a local prox.yaml, plus
//     a size cap and a service-count cap, because nothing upstream of this
//     mount has checked anything.
//  3. domain and both data-plane ports are overwritten from hub.yaml (D4) — the
//     hub owns the hostnames it publishes and the ports it exposes.
func (s *Server) handleHubRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("invalid request body: %v", err),
			Code:  "BAD_REQUEST",
		})
		return
	}

	if req.ProtocolVersion != constants.HubProtocolVersion {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf(
				"hub protocol mismatch: hub speaks %d, publisher speaks %d. Upgrade the older prox",
				constants.HubProtocolVersion, req.ProtocolVersion,
			),
			Code: "PROTOCOL_MISMATCH",
			// The publisher renders "protocol mismatch: hub 2, this prox 1" in
			// `prox status` and must not have to parse it back out of the
			// sentence above (plan 031 C5).
			HubProtocol: constants.HubProtocolVersion,
		})
		return
	}

	origin := strings.TrimSpace(req.Origin)
	if err := validateHubOrigin(origin); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return
	}
	if err := validateHubRegistration(req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return
	}
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "daemon is starting up",
			Code:  "NOT_READY",
		})
		return
	}

	cfg := s.hubConfigSnapshot()
	if !cfg.enabled {
		// Belt and braces: the listener is torn down before this can be
		// observed, but a request in flight during StopHub must not register.
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "hub mode is not enabled",
			Code:  "HUB_DISABLED",
		})
		return
	}

	// D4: the hub owns the domain and the data-plane ports. Whatever the
	// publisher's own prox.yaml says about them is irrelevant here.
	req.Domain = cfg.cfg.Domain
	req.HTTPSPort = cfg.cfg.HTTPSPort
	req.HTTPPort = cfg.cfg.HTTPPort

	// The capture DISK BUDGET is the hub host's own disk and none of a
	// publisher's business (plan 031 F9). It is not a per-project setting at
	// all: EffectiveCaptureDiskBudget folds every capture-enabled project's
	// value into ONE daemon-wide minimum, so an accepted remote registration
	// asking for 1 MiB would start evicting every other project's captured
	// bodies on this machine — a genuine cross-tenant effect from a publisher
	// that is only supposed to be able to touch its own registration. Cleared
	// here, and remote registrations are skipped by the accountant as well
	// (Registry.EffectiveCaptureDiskBudget), so neither half can be forgotten.
	req.DiskBudget = 0

	// D5/D15: the key is derived, never accepted. From here down, ProjectDir IS
	// the composed key — the registry, the per-project ring, the request
	// filters, and the publisher's own later deregister all use the same value.
	req.Origin = origin
	req.ProjectDir = HubProjectKey(origin, req.ProjectDir)

	status, body := s.register(req)
	// D4 again, from the publisher's side: the hub just overwrote the domain and
	// both data-plane ports, so the publisher has no way to know where its
	// service names were actually published. Hand those three facts back on the
	// success body — the ONLY place they are added, so every socket response is
	// untouched (plan 031 C5).
	if resp, ok := body.(RegisterResponse); ok {
		resp.Hub = &RegisterHubInfo{
			Domain:    cfg.cfg.Domain,
			HTTPSPort: cfg.cfg.HTTPSPort,
			HTTPPort:  cfg.cfg.HTTPPort,
		}
		body = resp
	}
	writeJSON(w, status, body)
}

// handleHubDeregister is the network mount's deregister (plan 031 §4.3, D15).
// It composes the key from the CALLER's origin, which gives two different
// strengths of guarantee, and the difference matters:
//
//   - Against a LOCAL project, the guarantee is structural and holds against a
//     hostile caller: every composed key carries the "hub:" prefix, the socket
//     mount refuses a project_dir that does, so no origin/dir pair composes to
//     a local key.
//   - Against ANOTHER PUBLISHER, the guarantee is only that it cannot happen by
//     accident: a different origin composes a different key, but the origin is
//     the caller's own claim and the hub holds one shared token, so a publisher
//     that deliberately sends another's origin reaches their registration.
//     That is D12's accepted trust model (§8), demonstrated by
//     TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation.
func (s *Server) handleHubDeregister(w http.ResponseWriter, r *http.Request) {
	var req DeregisterRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("invalid request body: %v", err),
			Code:  "BAD_REQUEST",
		})
		return
	}
	origin := strings.TrimSpace(req.Origin)
	if err := validateHubOrigin(origin); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return
	}
	if err := validateHubProjectDir(req.ProjectDir); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return
	}
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "daemon is starting up",
			Code:  "NOT_READY",
		})
		return
	}

	s.deregisterProject(w, HubProjectKey(origin, req.ProjectDir), req.PID)
}

// handleHubGetRequests and handleHubStreamRequests are the network mount's
// capture endpoints. Both take origin AND project and compose the key (D15), so
// a publisher asking for someone else's DIR under its own origin subscribes to
// a ring that does not exist and gets its own empty result. A publisher that
// forges the origin TOO does read the other's traffic: see handleHubDeregister
// for why that is the accepted posture rather than a bug.
func (s *Server) handleHubGetRequests(w http.ResponseWriter, r *http.Request) {
	key, ok := s.hubRequestKey(w, r)
	if !ok {
		return
	}
	s.getRequestsFor(w, r, key)
}

func (s *Server) handleHubStreamRequests(w http.ResponseWriter, r *http.Request) {
	key, ok := s.hubRequestKey(w, r)
	if !ok {
		return
	}
	s.streamRequestsFor(w, r, key)
}

// hubRequestKey validates the origin/project query pair and composes the
// scoped registry key, writing the error response itself when either is
// missing or malformed.
func (s *Server) hubRequestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	origin := strings.TrimSpace(r.URL.Query().Get("origin"))
	if err := validateHubOrigin(origin); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return "", false
	}
	project := r.URL.Query().Get("project")
	if err := validateHubProjectDir(project); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("project query parameter: %v", err),
			Code:  "BAD_REQUEST",
		})
		return "", false
	}
	return HubProjectKey(origin, project), true
}

// --- hub lifecycle (socket-only control) ---

// StartHub binds the network control plane and starts serving it (plan 031
// D13) without persisting anything. Used by the autostart path (the file it
// would write is the file it just read) and by tests.
func (s *Server) StartHub(cfg HubConfig) error {
	return s.startHub(cfg, false)
}

// startHub is StartHub plus the F13 commit rule. It is safe to call on a
// running hub:
//
//   - Same configured listen address → the live config and token are updated in
//     place and the listener is kept. This is what makes a repeated
//     `prox hub start` with no flags a no-op rather than a rebind that would
//     race itself for its own port.
//   - Different address → the NEW listener is bound FIRST. If that fails the
//     previous listener is still serving and the error is returned, which is
//     the rollback D14 asks for; only on success is the old server swapped out
//     and shut down.
//
// persist is the F13 fix: `prox hub start` used to write ~/.prox/hub.yaml and
// THEN ask the daemon to bind it, so a failed rebind left the old listener
// serving while the file described an address nothing was listening on. Now
// validate → bind → commit is one operation and the file is written only after
// the daemon is actually serving the config it describes. The daemon is also
// the single writer, which is what lets the whole sequence sit under one
// lifecycle mutex (F14).
func (s *Server) startHub(cfg HubConfig, persist bool) error {
	// F14: start, stop, and token rotation are serialized against each other.
	// Without this, a `hub start` that read the token before taking hubMu could
	// write a stale value back over a rotation that landed in between, silently
	// re-admitting a credential the operator had just revoked.
	s.hubLifecycleMu.Lock()
	defer s.hubLifecycleMu.Unlock()

	cfg, err := NormalizeHubConfig(cfg)
	if err != nil {
		return err
	}
	if cfg.Auth == HubAuthToken && cfg.Token == "" {
		token, err := EnsureHubToken()
		if err != nil {
			return fmt.Errorf("hub token: %w", err)
		}
		cfg.Token = token
	}
	if err := s.checkHubReconfigureAllowed(cfg); err != nil {
		return err
	}

	// Fast path: already serving this exact address. Update config/token under
	// the lock and return — no rebind, no listener churn.
	s.hubMu.Lock()
	if s.hubServer != nil && s.hubCfg.Listen == cfg.Listen {
		s.hubCfg = cfg
		s.hubToken = cfg.Token
		s.hubMu.Unlock()
		if err := s.commitHubConfig(cfg, persist); err != nil {
			return err
		}
		s.logger.Info("hub mode reconfigured in place", "listen", cfg.Listen, "domain", cfg.Domain)
		return nil
	}
	s.hubMu.Unlock()

	// Bind before touching the running hub, so a bind failure leaves the
	// previous listener untouched (D14 rollback) and hub.yaml unwritten (F13).
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("binding hub control plane on %s: %w", cfg.Listen, err)
	}
	if err := s.commitHubConfig(cfg, persist); err != nil {
		_ = ln.Close()
		return err
	}

	srv := &http.Server{
		Handler:     s.newHubRouter(),
		ReadTimeout: 15 * time.Second,
		// WriteTimeout stays 0: the requests stream is SSE and the tunnel
		// upgrade (C4) is long-lived, exactly as on the socket server.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	s.hubMu.Lock()
	oldSrv := s.hubServer
	s.hubCfg = cfg
	s.hubToken = cfg.Token
	s.hubListener = ln
	s.hubServer = srv
	s.hubStartedAt = time.Now()
	s.hubMu.Unlock()

	// Shut the replaced server down OUTSIDE the lock: Shutdown waits for
	// in-flight handlers, and those handlers read the token under hubMu.
	if oldSrv != nil {
		go shutdownHubServer(oldSrv)
	}

	go func() {
		if err := srv.Serve(ln); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			s.logger.Error("hub control plane server error", "error", err)
		}
	}()

	s.logger.Info("hub mode started",
		"listen", ln.Addr().String(),
		"domain", cfg.Domain,
		"https_port", cfg.HTTPSPort,
		"http_port", cfg.HTTPPort,
		"auth", cfg.Auth,
	)
	return nil
}

// commitHubConfig writes the configuration the daemon has just proven it can
// serve (plan 031 F13). The Token is never part of the file (HubConfig.Token is
// yaml:"-"); it lives in ~/.prox/hub.token.
func (s *Server) commitHubConfig(cfg HubConfig, persist bool) error {
	if !persist {
		return nil
	}
	if err := SaveHubConfig(cfg); err != nil {
		return fmt.Errorf("saving the hub config after binding %s: %w", cfg.Listen, err)
	}
	return nil
}

// checkHubReconfigureAllowed refuses a reconfiguration that would change facts
// the EXISTING remote registrations were built from (plan 031 F13).
//
// Domain and the two data-plane ports are baked into every remote route at
// register time (D4): the hostname is "<service>.<domain>" and the route is
// keyed by "<hostname>:<port>". Swapping them on a running hub would leave the
// routes serving the old domain and ports while `prox hub status` reported the
// new ones — status describing a config the routes do not use, which is exactly
// what the finding says not to leave behind.
//
// The chosen arm is REJECT rather than "drop and re-register": dropping every
// publisher's registration to make a flag take effect is a destructive surprise
// for an operator who typed `prox hub start --https-port 8443` meaning to adjust
// one thing. `prox hub stop` first is one extra command and says what it does.
// Changing only the LISTEN address (or the auth mode) is unaffected — those do
// not appear in any route.
func (s *Server) checkHubReconfigureAllowed(cfg HubConfig) error {
	s.hubMu.RLock()
	running := s.hubServer != nil
	current := s.hubCfg
	s.hubMu.RUnlock()
	if !running {
		return nil
	}
	if current.Domain == cfg.Domain && current.HTTPSPort == cfg.HTTPSPort && current.HTTPPort == cfg.HTTPPort {
		return nil
	}
	if s.registry == nil {
		return nil
	}
	publishers := len(s.registry.RemoteProjectKeys())
	if publishers == 0 {
		return nil
	}
	return hubConfigErrorf(
		"cannot change the hub domain or data-plane ports while %d publisher(s) are registered "+
			"(their hostnames and ports were fixed at register time): run 'prox hub stop' first, "+
			"then 'prox hub start' with the new settings and let the publishers re-register",
		publishers)
}

// StopHub closes the network control plane and removes every remote
// registration it was serving (plan 031 §4.2), then schedules the ordinary
// empty-daemon shutdown check — which now CAN fire, since hub mode is off. A
// daemon still serving local projects keeps running; one that was only a hub
// exits after the usual grace.
//
// Lock discipline: the server and listener are copied out under hubMu and the
// lock is released before Shutdown (which waits for handlers that themselves
// take hubMu) and before removeProject (which takes lifecycleMu, above hubMu in
// the lock order).
func (s *Server) StopHub() {
	// F14: serialized against start and token rotation, so a stop can never
	// interleave with a start that is mid-bind.
	s.hubLifecycleMu.Lock()
	defer s.hubLifecycleMu.Unlock()

	s.hubMu.Lock()
	srv := s.hubServer
	ln := s.hubListener
	s.hubServer = nil
	s.hubListener = nil
	s.hubCfg = HubConfig{}
	s.hubToken = ""
	s.hubStartedAt = time.Time{}
	s.hubMu.Unlock()

	if srv != nil {
		shutdownHubServer(srv)
	} else if ln != nil {
		_ = ln.Close()
	}
	if srv == nil && ln == nil {
		return // hub mode was not on
	}

	// Tunnels go BEFORE the registrations they belong to, and before
	// lifecycleMu is taken by removeProject: the session mutex is a leaf in the
	// lock order and a session close is I/O (plan 031 D17). Closing them here
	// also means a publisher learns its tunnel is gone immediately rather than
	// discovering it on the next request.
	s.closeAllTunnels()

	// Remote registrations outlive nothing: their publishers reach this daemon
	// only through the listener that just closed.
	if s.registry != nil {
		for _, key := range s.registry.RemoteProjectKeys() {
			removed, ports := s.removeProject(key)
			s.logger.Info("removed remote registration on hub stop",
				"project", key, "removed_hostnames", removed, "closed_ports", ports)
		}
	}
	s.logger.Info("hub mode stopped")
	s.scheduleShutdownWhenEmpty()
}

// shutdownHubServer gracefully stops a hub control-plane server, bounded so a
// wedged handler cannot hold up a rebind or a daemon exit forever.
//
// The Close on timeout is plan 031 F15. Shutdown WAITS for in-flight handlers,
// and the hub's longest-lived handler is an SSE request subscription that a
// publisher holds open indefinitely — so a rebind's Shutdown reliably times
// out, and discarding that error left the replaced server's handlers (and the
// connections under them) alive for as long as the subscriber cared to hold
// them. Close ends them.
//
// It does NOT end the tunnels, which is the point: net/http neither tracks nor
// closes HIJACKED connections in Shutdown or Close, so the yamux sessions
// attached through the replaced server keep running across a rebind exactly as
// D14 intends — a publisher does not have to reconnect because the operator
// moved the control plane to another address.
func shutdownHubServer(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), constants.HubShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

// HubListenAddr returns the address the control plane is ACTUALLY bound to, or
// "" when hub mode is off. It is exported because `--listen host:0` binds an
// ephemeral port that only the listener knows — and because a data-plane port
// of 0 already means "no listener", so tests must not overload it to mean
// "ephemeral" (plan 031 P15/D14).
func (s *Server) HubListenAddr() string {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	if s.hubListener == nil {
		return ""
	}
	return s.hubListener.Addr().String()
}

// hubEnabled reports whether hub mode is on. scheduleShutdownWhenEmpty consults
// it: a hub daemon with no registrations is idle, not unwanted (D13).
func (s *Server) hubEnabled() bool {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	return s.hubServer != nil
}

// hubAuthSnapshot returns the auth mode and token the middleware should apply
// to the request it is handling. Read per request so a rotation takes effect
// immediately (D18).
func (s *Server) hubAuthSnapshot() (mode, token string) {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	if s.hubCfg.Auth == HubAuthNone {
		return HubAuthNone, ""
	}
	return HubAuthToken, s.hubToken
}

// hubRuntimeView is a handler's read-only copy of hub state, taken under the
// lock and used after releasing it.
type hubRuntimeView struct {
	enabled bool
	cfg     HubConfig
}

// hubConfigSnapshot copies the running hub's configuration for a handler to
// read without holding the lock.
func (s *Server) hubConfigSnapshot() hubRuntimeView {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	return hubRuntimeView{enabled: s.hubServer != nil, cfg: s.hubCfg}
}

// hubStatus renders the hub HOST's view (plan 031 D20). Enabled is false with
// everything else zero when hub mode is off.
func (s *Server) hubStatus() HubStatus {
	s.hubMu.RLock()
	enabled := s.hubServer != nil
	cfg := s.hubCfg
	startedAt := s.hubStartedAt
	listen := ""
	if s.hubListener != nil {
		listen = s.hubListener.Addr().String()
	}
	s.hubMu.RUnlock()

	if !enabled {
		return HubStatus{Enabled: false, Publishers: []HubPublisher{}}
	}
	status := HubStatus{
		Enabled:    true,
		Domain:     cfg.Domain,
		Listen:     listen,
		HTTPSPort:  cfg.HTTPSPort,
		HTTPPort:   cfg.HTTPPort,
		Auth:       cfg.Auth,
		StartedAt:  startedAt,
		Publishers: []HubPublisher{},
	}
	if s.registry != nil {
		status.Publishers = s.registry.RemotePublishers()
	}
	return status
}

// setHubToken replaces the token the running hub accepts, with no grace: an
// established connection authenticated at connect time and survives, but the
// next call carrying the old token gets 401 (plan 031 D18). A no-op when hub
// mode is off (the next StartHub reads the rotated file).
func (s *Server) setHubToken(token string) {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubServer == nil {
		return
	}
	s.hubToken = token
}

// --- socket-only hub/* handlers ---

// handleHubStart turns hub mode on (or reconfigures it).
//
// The daemon owns validate → bind → commit as ONE operation (plan 031 F13). A
// request carrying a config proposes it: the daemon normalizes it, binds it,
// and only then writes ~/.prox/hub.yaml, so a failed rebind can never leave the
// file describing an address nothing is listening on. A request with no config
// (an empty body) means "start from the file as it stands", which is the
// no-flags `prox hub start` and the shape autostart uses.
func (s *Server) handleHubStart(w http.ResponseWriter, r *http.Request) {
	var req HubStartRequest
	if r.Body != nil {
		// A body is optional; an empty one decodes to the zero request.
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlRequestBytes))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("invalid request body: %v", err),
				Code:  "BAD_REQUEST",
			})
			return
		}
	}

	var cfg HubConfig
	persist := req.Config != nil
	if req.Config != nil {
		cfg = *req.Config
	} else {
		loaded, err := LoadHubConfig()
		if err != nil {
			code := "HUB_CONFIG_INVALID"
			if errors.Is(err, os.ErrNotExist) {
				code = "HUB_NOT_CONFIGURED"
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("reading hub config: %v", err),
				Code:  code,
			})
			return
		}
		cfg = loaded
	}

	if err := s.startHub(cfg, persist); err != nil {
		status, code := http.StatusInternalServerError, "HUB_BIND_FAILED"
		var cfgErr *hubConfigError
		if errors.As(err, &cfgErr) {
			status, code = http.StatusBadRequest, "HUB_CONFIG_INVALID"
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error(), Code: code})
		return
	}
	writeJSON(w, http.StatusOK, s.hubStatus())
}

// handleHubStop turns hub mode off. Idempotent: stopping a hub that is not
// running reports the (disabled) status rather than an error.
func (s *Server) handleHubStop(w http.ResponseWriter, r *http.Request) {
	s.StopHub()
	writeJSON(w, http.StatusOK, s.hubStatus())
}

func (s *Server) handleHubStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hubStatus())
}

// handleHubRotateToken writes a fresh token and makes the running hub accept
// only it (D18). The daemon is the single writer, so the CLI never has to race
// it for the file.
func (s *Server) handleHubRotateToken(w http.ResponseWriter, r *http.Request) {
	// F14: the write and the in-memory swap happen under the same lifecycle
	// mutex a concurrent `hub start` takes, so a start can no longer read the
	// pre-rotation token and write it back over the new one.
	s.hubLifecycleMu.Lock()
	defer s.hubLifecycleMu.Unlock()

	token, err := RotateHubToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: err.Error(),
			Code:  "HUB_TOKEN_FAILED",
		})
		return
	}
	s.setHubToken(token)
	s.logger.Info("hub token rotated")
	writeJSON(w, http.StatusOK, HubTokenResponse{Token: token, Path: HubTokenPath()})
}
