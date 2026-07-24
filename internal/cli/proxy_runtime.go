package cli

import (
	"context"
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
	// begun; C5 only sets it.
	shutdownLatch atomic.Bool

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

// Mode returns the current proxy mode.
func (r *proxyRuntime) Mode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
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
	resp := &api.ProxyStatusResponse{
		Mode:                mode,
		ConsecutiveFailures: r.consecutiveFailures.Load(),
		BackfillFailures:    r.backfillFailures.Load(),
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
			resp.HealState = "healthy"
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
