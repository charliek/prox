package cli

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
)

// Proxy modes reported in the `prox status` proxy block (D5).
const (
	proxyModeShared     = "shared"
	proxyModeStandalone = "standalone"
	proxyModeDisabled   = "disabled"
)

// heal_state values reported in the `prox status` proxy block. C5 reported only
// healthy/""; C6 (D6b) refines the unreachable case to distinguish an in-progress
// heal from a busy different-version daemon.
const (
	healStateHealthy         = "healthy"
	healStateHealing         = "healing"
	healStateVersionMismatch = "version_mismatch"
)

// proxyRuntime is the single source of truth for this project's proxy path,
// shared by the `prox up` runtime, the SSE forwarder, and the API handlers
// (D5/D6). It owns:
//
//   - the mode (shared/standalone/disabled);
//   - the active daemon client (mutex-guarded; C6 swaps it on heal, and
//     performShutdown reads the CURRENT client for deregister);
//   - the original register request (stored for C6's re-register);
//   - the local forwarding request manager (source of the dropped-events count);
//   - forwarder connection state as atomics (following the lifecycleEpoch
//     precedent): consecutive reconnect failures, last successful connect time,
//     and backfill-failure count;
//   - a shutdown latch set before deregister so a C6 heal cannot re-register the
//     project mid-teardown.
//
// It implements proxyd.ForwarderStatusSink (the forwarder writes state through
// it) and api.ProxyStatusProvider (GET /status reads the proxy block from it).
type proxyRuntime struct {
	mu       sync.Mutex
	mode     string
	client   *proxyd.Client
	register proxyd.RegisterRequest
	localRM  *proxy.RequestManager

	// shutdownLatch is set (via MarkShuttingDown) before the deregister stage of
	// performShutdown. C6's heal callback consumes it to no-op once teardown has
	// begun.
	shutdownLatch atomic.Bool

	// healMu serializes a heal against shutdown's client read (FIX 3). heal holds
	// it for its whole body; performShutdown acquires it (clientAfterHealBarrier)
	// AFTER latching, so a heal that began before the latch finishes its client
	// swap before shutdown reads the client — the deregister then goes through the
	// HEALED client. A heal that starts after the latch no-ops (its first action
	// under healMu re-checks IsShuttingDown). It never nests inside mu.
	healMu sync.Mutex

	// forwarderCancel cancels the SSE forwarder's context. performShutdown calls
	// it (via CancelForwarder) BEFORE deregister (D6c) so the forwarder loop stops
	// and cannot fire a heal that re-registers the project mid-teardown. It lives
	// on a context derived from runUp's — separate so cancelling the forwarder
	// never disturbs the supervisor. nil until the shared-mode forwarder launches.
	forwarderCancel context.CancelFunc

	// captureEnabled is this project's EFFECTIVE capture state — the proxy is
	// actually running for this session AND capture is enabled in its config
	// (see resolveProxyRuntimeState). Reported in the status block so a client
	// can explain an empty request list. Guarded by mu.
	captureEnabled bool

	// warnings is this session's warning sink (plan 028 A2). The shared daemon
	// returns user-facing advisories on the register response — on the FIRST
	// register (tryDaemonProxy) and on every self-heal re-register (heal) — and
	// the daemon's own stdout/stderr are /dev/null, so this sink is the only way
	// they reach the user. Guarded by mu; nil is a usable no-op sink (every
	// warningSink method is nil-receiver safe), which is what unit-test runtimes
	// get.
	warnings *warningSink

	// healState overrides the derived heal_state while the shared daemon is
	// unreachable (D6b): "" (down, no heal attempted yet), "healing" (heal
	// attempts failing), or "version_mismatch" (a busy different-version daemon).
	// Guarded by mu. When the daemon is reachable, ProxyStatus reports "healthy"
	// regardless of this field.
	healState string

	// Forwarder state atomics (D5).
	consecutiveFailures atomic.Int64
	lastConnectedAt     atomic.Int64 // unix nanos; 0 == never connected
	backfillFailures    atomic.Int64

	// Probe cache (D5): the shared-daemon /health probe is cached for probeTTL so
	// a polled status does not pay the probe timeout on every call, and a downed
	// daemon is re-probed at most once per TTL. Guarded by probeMu (separate from
	// mu so a probe never contends with client/state access).
	probeMu         sync.Mutex
	probeCache      proxyProbeResult
	probeValidUntil time.Time

	// Injectable seams for tests (default to production impls).
	prober   func() (reachable bool, version string)
	probeTTL time.Duration
	now      func() time.Time
}

type proxyProbeResult struct {
	reachable bool
	version   string
}

// newProxyRuntime creates a runtime defaulting to disabled mode with production
// probe wiring. Mode/client/register/localRM are set as the proxy path resolves.
func newProxyRuntime() *proxyRuntime {
	r := &proxyRuntime{
		mode:     proxyModeDisabled,
		probeTTL: constants.ProxyStatusProbeCacheTTL,
		now:      time.Now,
	}
	r.prober = r.defaultProber
	return r
}

// SetMode records the resolved proxy mode.
func (r *proxyRuntime) SetMode(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
}

// SetCaptureEnabled records whether request/response capture is effectively on
// for this session. runUp calls it once, from the same resolved runtime state
// that decides whether to start the proxy at all.
func (r *proxyRuntime) SetCaptureEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captureEnabled = enabled
}

// SetWarningSink injects the session's warning sink (plan 028 A2). Called once
// from runUp before the proxy path resolves, so both register arms — the first
// one and every heal re-register — land in the same collection.
func (r *proxyRuntime) SetWarningSink(s *warningSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = s
}

// WarningSink returns the session's warning sink, or nil when none was injected
// (a nil sink is a no-op, not a crash).
func (r *proxyRuntime) WarningSink() *warningSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.warnings
}

// Mode returns the current proxy mode.
func (r *proxyRuntime) Mode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
}

// CaptureEnabled returns the effective capture state recorded by
// SetCaptureEnabled (false until resolved).
func (r *proxyRuntime) CaptureEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.captureEnabled
}

// SetClient stores the active daemon client. C6 calls this again on heal.
func (r *proxyRuntime) SetClient(c *proxyd.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = c
}

// Client returns the active daemon client (nil in standalone/disabled mode).
// performShutdown reads through this so a C6-healed client is used for
// deregister rather than the one captured at startup.
func (r *proxyRuntime) Client() *proxyd.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

// clientAfterHealBarrier blocks until any in-flight heal completes (its client
// swap has landed), then returns the current client. performShutdown calls it
// AFTER MarkShuttingDown()+CancelForwarder() so a heal that began before the latch
// finishes fully and the deregister goes through the HEALED client; a heal that
// starts after the latch no-ops under the same mutex (FIX 3).
func (r *proxyRuntime) clientAfterHealBarrier() *proxyd.Client {
	r.healMu.Lock()
	defer r.healMu.Unlock()
	return r.Client()
}

// SetRegisterRequest stores the original registration request (C6 re-registers
// with it after a heal).
func (r *proxyRuntime) SetRegisterRequest(req proxyd.RegisterRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.register = req
}

// RegisterRequest returns the stored registration request.
func (r *proxyRuntime) RegisterRequest() proxyd.RegisterRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.register
}

// SetLocalRequestManager stores the project's local forwarding request manager,
// the source of the dropped-events count in the status block.
func (r *proxyRuntime) SetLocalRequestManager(rm *proxy.RequestManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.localRM = rm
}

func (r *proxyRuntime) localRequestManager() *proxy.RequestManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.localRM
}

// MarkShuttingDown latches the shutdown flag before deregister (D6c).
func (r *proxyRuntime) MarkShuttingDown() {
	r.shutdownLatch.Store(true)
}

// IsShuttingDown reports whether teardown has begun.
func (r *proxyRuntime) IsShuttingDown() bool {
	return r.shutdownLatch.Load()
}

// SetForwarderCancel records the SSE forwarder's cancel func so performShutdown
// can stop the forwarder before deregister (D6c).
func (r *proxyRuntime) SetForwarderCancel(cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forwarderCancel = cancel
}

// CancelForwarder cancels the forwarder's context if one was launched (no-op
// otherwise). Called before deregister so no heal can fire mid-teardown (D6c).
func (r *proxyRuntime) CancelForwarder() {
	r.mu.Lock()
	cancel := r.forwarderCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setHealState records the heal state machine's current view (D6b).
func (r *proxyRuntime) setHealState(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healState = state
}

func (r *proxyRuntime) getHealState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.healState
}

// --- proxyd.ForwarderStatusSink ---

// ForwarderConnected resets the failure counter and records the connect time.
// A down→connected transition (there were prior failures) logs one line.
func (r *proxyRuntime) ForwarderConnected() {
	prev := r.consecutiveFailures.Swap(0)
	r.lastConnectedAt.Store(r.now().UnixNano())
	if prev > 0 {
		log.Printf("prox: reconnected to shared proxy daemon after %d failed attempt(s)", prev)
	}
}

// ForwarderConnectFailed increments the failure counter. The connected→down
// transition (first failure of a run) logs one line; subsequent failures are
// silent so a persistently down daemon does not flood the log.
func (r *proxyRuntime) ForwarderConnectFailed(err error) {
	if r.consecutiveFailures.Add(1) == 1 {
		log.Printf("prox: lost connection to shared proxy daemon: %v (proxied request inspection paused; retrying)", err)
	}
}

// ForwarderBackfillFailed increments the backfill-failure counter (the forwarder
// already logs the single backfill warning).
func (r *proxyRuntime) ForwarderBackfillFailed() {
	r.backfillFailures.Add(1)
}

// --- api.ProxyStatusProvider ---

// ProxyStatus builds the proxy block for GET /status (D5). In shared mode it
// live-probes the daemon's health (cached); standalone/disabled modes report
// mode only, with no probe.
func (r *proxyRuntime) ProxyStatus() *api.ProxyStatusResponse {
	mode := r.Mode()
	// Always non-nil from a current daemon: nil is reserved for the wire case
	// of an OLDER daemon that predates the field (see CaptureEnabled's doc), so
	// a live runtime must state its answer even when that answer is false.
	capture := r.CaptureEnabled()
	resp := &api.ProxyStatusResponse{
		Mode:                mode,
		ConsecutiveFailures: r.consecutiveFailures.Load(),
		BackfillFailures:    r.backfillFailures.Load(),
		CaptureEnabled:      &capture,
	}
	if ns := r.lastConnectedAt.Load(); ns > 0 {
		t := time.Unix(0, ns)
		resp.LastConnectedAt = &t
	}
	if rm := r.localRequestManager(); rm != nil {
		resp.DroppedEvents = rm.DroppedEvents()
	}

	if mode == proxyModeShared {
		reachable, version := r.probeDaemon()
		resp.DaemonReachable = reachable
		resp.DaemonVersion = version
		if reachable {
			resp.HealState = healStateHealthy
		} else {
			// Unreachable: surface the heal state machine's current view — ""
			// until the first heal attempt, then "healing"/"version_mismatch" (D6b).
			resp.HealState = r.getHealState()
		}
	}

	return resp
}

// probeDaemon returns the shared daemon's reachability and version, cached for
// probeTTL. The cache is keyed on monotonic time (now()) so repeated status
// polls within the TTL reuse one probe result rather than each paying the probe
// timeout.
func (r *proxyRuntime) probeDaemon() (bool, string) {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()

	if now := r.now(); now.Before(r.probeValidUntil) {
		return r.probeCache.reachable, r.probeCache.version
	}

	reachable, version := r.prober()
	r.probeCache = proxyProbeResult{reachable: reachable, version: version}
	r.probeValidUntil = r.now().Add(r.probeTTL)
	return reachable, version
}

// invalidateProbeCache forces the next ProxyStatus to re-probe rather than reuse
// a cached result. Called after a heal swaps in a fresh client so status reflects
// the new daemon immediately instead of waiting out the cache TTL.
func (r *proxyRuntime) invalidateProbeCache() {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	r.probeValidUntil = time.Time{}
}

// --- D6b self-heal ---

// healOps bundles the injectable operations proxyRuntime.heal needs so it can be
// unit-tested against a fake daemon without real sockets or wall-clock waits
// (mirrors skewOps / registerRetryOps). defaultHealOps wires the production impls.
type healOps struct {
	// ensureRunning starts/connects a daemon of this process's version (the heal
	// wraps it in ensureRunningWithRetry for the socket-removed-before-PID-lock
	// window).
	ensureRunning func() (*proxyd.Client, error)
	sleep         func(time.Duration)
	retryDelay    time.Duration
}

func defaultHealOps() healOps {
	return healOps{
		ensureRunning: proxyd.EnsureRunning,
		sleep:         time.Sleep,
		retryDelay:    1 * time.Second,
	}
}

// heal is the forwarder's self-heal callback (D6b), invoked INLINE from the
// forwarder reconnect loop after the shared daemon has been unreachable past the
// heal threshold. It re-ensures a daemon of this version and re-registers this
// project's stored request against it, swapping in the fresh client on success so
// performShutdown later deregisters through the healed client (D6c). It returns
// true only when the project is registered on a healthy daemon — the forwarder
// then reconnects to it on the same socket.
//
// Behavior:
//   - shutting down (latch set): no-op, return false — never re-register a project
//     that is tearing down (D6c);
//   - VersionMismatchError from EnsureRunning: a live daemon of another version is
//     holding the ports; NEVER restart a busy daemon — set heal_state
//     "version_mismatch" and keep waiting (heal retried after healMinInterval);
//   - any other ensure/register error: set heal_state "healing", count the attempt
//     (the forwarder keeps retrying forever);
//   - success: swap the client, reset failure state, set heal_state "healthy",
//     invalidate the probe cache, log one prominent line.
func (r *proxyRuntime) heal(ops healOps) bool {
	// Hold healMu for the WHOLE body so shutdown's client read (clientAfterHealBarrier)
	// blocks until an in-flight heal's client swap has landed (FIX 3).
	r.healMu.Lock()
	defer r.healMu.Unlock()

	// First action under the lock: a shutdown that latched before we acquired healMu
	// owns teardown now — no-op. A shutdown latching AFTER this check is blocked on
	// healMu until we return, so its client read observes our swap.
	if r.IsShuttingDown() {
		return false
	}

	client, err := ensureRunningWithRetry(ops.ensureRunning, ops.sleep, ops.retryDelay)
	if err != nil {
		var vme *proxyd.VersionMismatchError
		if errors.As(err, &vme) {
			r.setHealState(healStateVersionMismatch)
			return false
		}
		r.setHealState(healStateHealing)
		return false
	}

	resp, err := client.Register(r.RegisterRequest())
	if err != nil {
		// A busy different-version daemon holding the ports re-checks versions at
		// register time and returns VERSION_MISMATCH: surface it as version_mismatch
		// (like the EnsureRunning VersionMismatchError path), NOT healing (FIX 5).
		if isVersionMismatchError(err) {
			r.setHealState(healStateVersionMismatch)
			return false
		}
		r.setHealState(healStateHealing)
		return false
	}

	r.SetClient(client)
	r.consecutiveFailures.Store(0)
	r.setHealState(healStateHealthy)
	r.invalidateProbeCache()
	log.Printf("prox: shared proxy daemon healed — re-registered %d domain(s) with a fresh daemon", len(resp.Registered))

	// The re-register carries the SAME advisories the first one did (plan 028
	// A2): a fresh daemon re-runs the checks behind them, so a heal is often the
	// first time a project hears about, say, an untrusted mkcert CA. Everything
	// but len(resp.Registered) used to be discarded here.
	//
	// This runs on the FORWARDER's goroutine, not runUp's, so the preamble is
	// off limits (it is unsynchronized by construction). Only the sink — which
	// is mutex-guarded and feeds GET /status — plus the stdlib logger, which is
	// routed through the stdio sink and therefore reaches a TUI's log pane or
	// plain stderr. Add's return means each advisory is announced once, not once
	// per heal.
	for _, w := range r.WarningSink().Add(resp.Warnings...) {
		for _, line := range formatWarning(w) {
			log.Print(line)
		}
	}
	return true
}

// defaultProber issues a live /health probe against the active daemon client
// with a short timeout (D5). reachable is true iff the probe answered.
func (r *proxyRuntime) defaultProber() (bool, string) {
	client := r.Client()
	if client == nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.DaemonProxyProbeTimeout)
	defer cancel()
	version, err := client.HealthWithContext(ctx)
	if err != nil {
		return false, ""
	}
	return true, version
}
