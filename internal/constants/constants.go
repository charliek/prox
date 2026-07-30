// Package constants provides shared configuration values used across the prox application.
package constants

import (
	"path/filepath"
	"time"
)

// Configuration file defaults
const (
	// DefaultConfigFile is the default configuration filename
	DefaultConfigFile = "prox.yaml"

	// DefaultAPIHost is the default host for the API server
	DefaultAPIHost = "127.0.0.1"

	// DefaultAPIPort is the default port for the API server
	DefaultAPIPort = 5555

	// DefaultAPIAddress is the default API address for client connections
	DefaultAPIAddress = "http://127.0.0.1:5555"

	// DefaultProxyPort is the default port for the HTTPS reverse proxy
	DefaultProxyPort = 6789

	// DefaultCertsDir is the default directory for storing certificates
	DefaultCertsDir = "~/.prox/certs"
)

// Timeout and duration defaults
const (
	// DefaultRequestTimeout is the default timeout for API requests
	DefaultRequestTimeout = 30 * time.Second

	// DefaultShutdownTimeout is the default timeout for graceful shutdown
	DefaultShutdownTimeout = 10 * time.Second

	// KillGrace is the window reserved at the tail of a process's shutdown
	// budget for the SIGKILL escalation and post-kill group-liveness
	// verification. It is carved out of ShutdownTimeout: the graceful (SIGTERM)
	// phase runs until (deadline - KillGrace), after which the group is
	// SIGKILLed and given up to KillGrace more to disappear before Stop reports
	// the group could not be reaped.
	//
	// It also doubles as the configuration-time minimum for shutdown_timeout /
	// stop_timeout: a configured budget must leave more than KillGrace on the
	// table for the escalation to mean anything.
	KillGrace = 2 * time.Second

	// MaxStopTimeout is the configuration-time maximum for shutdown_timeout /
	// stop_timeout. It keeps the static ceilings that wrap the configured
	// value (API route timeout, CLI client timeout, TUI restart timeout)
	// boundable.
	MaxStopTimeout = 10 * time.Minute

	// LifecycleTimeoutCeiling is the static hang-protection ceiling for a
	// lifecycle operation (process start/stop/restart) that the daemon may
	// legitimately hold open for up to a configured stop budget. It sits one
	// minute above MaxStopTimeout so a valid long stop is never cut off by the
	// wrapping layer; the supervisor is the authoritative bound. Shared by the
	// three consumers named in MaxStopTimeout's comment: the API lifecycle
	// route timeout, the CLI lifecycle http.Client timeout, and the TUI restart
	// timeout (#35, D2).
	LifecycleTimeoutCeiling = MaxStopTimeout + time.Minute

	// DaemonStatusProbeTimeout bounds the /status probe against a possibly
	// draining old daemon during version-skew recovery (D1). The proxyd client's
	// 30s default is far too long for an interactive `prox up`.
	DaemonStatusProbeTimeout = 2 * time.Second

	// DaemonHealthProbeTimeout bounds a single /health probe used while polling
	// for an old daemon's socket to stop answering during a version-skew heal.
	DaemonHealthProbeTimeout = 1 * time.Second

	// DaemonProxyProbeTimeout bounds the live /health probe `prox status` issues
	// against the shared daemon to report proxy health (D5). Short so a downed
	// daemon never makes `prox status` sit out a long timeout.
	DaemonProxyProbeTimeout = 500 * time.Millisecond

	// ProxyStatusProbeCacheTTL caches the shared-proxy health probe result so a
	// polled `prox status` (TUIs/agents) does not pay the probe timeout on every
	// call; a downed daemon is re-probed at most once per TTL (D5).
	ProxyStatusProbeCacheTTL = 2 * time.Second

	// ForwarderHealAfterDown is how long the SSE forwarder's reconnect must have
	// failed continuously before it fires a self-heal (re-ensure a daemon of this
	// version + re-register this project) from inside its reconnect loop (D6b).
	// Injectable in tests so the heal path never waits out real wall-clock.
	ForwarderHealAfterDown = 15 * time.Second

	// ForwarderHealMinInterval is the minimum spacing between forwarder heal
	// attempts, damping churn against a flapping daemon (D6b). Injectable in tests.
	ForwarderHealMinInterval = 30 * time.Second

	// DeadRouteProbeMinInterval is the minimum spacing between on-502 dead-owner
	// liveness probes for a single project (#74). When a route's backend
	// transport fails, the daemon probes the owning `prox up` process's liveness
	// and reaps the registration if it is dead, converging a crashed project's
	// routes in ~one request instead of waiting up to a full
	// stalePIDCheckInterval (30s) for the periodic sweep. The single-in-flight
	// probe state machine bounds concurrency (one chain per project); this
	// interval bounds frequency, damping a 502 storm into at most one probe per
	// interval while still guaranteeing a trailing probe for a 502 suppressed
	// inside the window. Injectable in tests.
	DeadRouteProbeMinInterval = 1 * time.Second

	// DefaultDependencyCheckTimeout is the default readiness-probe budget for a
	// dependencies: entry whose check block omits `timeout` (plan 013 D1). The
	// resolver treats it as the overall window a dependency's check may keep
	// failing before the dependency is declared not-ready. Unlike a stop budget,
	// an explicit zero is NOT special here -- validation requires timeout >=
	// interval, so a dependency can never be configured with a zero timeout.
	DefaultDependencyCheckTimeout = 30 * time.Second

	// DefaultDependencyCheckInterval is the default spacing between readiness
	// probes for a dependencies: entry whose check block omits `interval` (plan
	// 013 D1). Validation requires interval > 0 and timeout >= interval.
	DefaultDependencyCheckInterval = 1 * time.Second

	// DefaultTaskTimeout is the default run budget for a tasks: entry whose
	// `timeout` is unset (plan 013 D1). It is DISTINCT from an explicit
	// `timeout: 0`, which means "no limit" -- the config layer models the
	// difference with a has-limit flag (see config.TaskConfig.ToDomain and
	// domain.TaskConfig.HasTimeout), so unset resolves to this default while an
	// explicit zero resolves to unbounded.
	DefaultTaskTimeout = 60 * time.Second

	// InFlightStaleAfter is how long a request record may sit in-flight before
	// it is reported as stale (D8, #53). Stale does NOT mean broken: SSE
	// streams, WebSocket upgrades, and large transfers can legitimately stay
	// in-flight this long while completely healthy. It means "completion
	// unknown" — the record's completion event (published when the response
	// body finishes) may have been lost (subscriber channel overrun, process
	// crash mid-request, etc.), so the true outcome can no longer be inferred
	// from this record alone. 5 minutes comfortably outlasts ordinary
	// request/response cycles while still surfacing genuinely stuck rows in a
	// timely way.
	InFlightStaleAfter = 5 * time.Minute
)

// SSE (Server-Sent Events) timing
const (
	// SSEHeartbeatInterval is how often an idle SSE stream (StreamLogs,
	// StreamProxyRequests) writes a ": ping" comment to keep the connection
	// observably alive and to surface a dead client via a failed write. The
	// server's http.Server.WriteTimeout is deliberately 0 for these routes (a
	// long-lived stream cannot carry a fixed connection-lifetime deadline), so
	// the heartbeat's per-write deadline (SSEWriteTimeout) is the only bound on
	// a stalled write.
	SSEHeartbeatInterval = 15 * time.Second

	// SSEWriteTimeout is the per-write deadline for an SSE write (connect
	// comment, data event, or heartbeat), set via
	// http.ResponseController.SetWriteDeadline before each write. It stands in
	// for the connection-lifetime WriteTimeout the SSE routes forgo.
	SSEWriteTimeout = 10 * time.Second

	// SSEReadTimeout is the CLI SSE client's read deadline: if no byte (data or
	// heartbeat) arrives within this window, the connection is declared dead.
	// Invariant: SSEReadTimeout > 3*SSEHeartbeatInterval, so the client
	// tolerates at least 3 consecutive missed heartbeats (a transient stall,
	// not just a single delayed tick) before giving up.
	SSEReadTimeout = 60 * time.Second
)

// Stream reconnection
//
// These bound the generic SSE reconnect runner in internal/stream, which every
// prox stream consumer (TUI attach streams, CLI --follow commands) drives. They
// are deliberately NOT shared with the proxyd forwarder's own reconnect loop:
// the forwarder predates this package and keeps its local constants so the two
// can be retuned independently.
const (
	// StreamReconnectBaseBackoff is the wait before the first reconnect
	// attempt after a stream ends, and the value the backoff resets to once an
	// attempt has survived StreamReconnectFlapThreshold.
	StreamReconnectBaseBackoff = 500 * time.Millisecond

	// StreamReconnectMaxBackoff caps the exponential reconnect backoff. The
	// wait doubles per failed cycle (500ms, 1s, 2s, 4s) and is then clamped
	// here, so a server that is down for a long time is re-probed at a steady
	// 5s rather than drifting toward minutes.
	StreamReconnectMaxBackoff = 5 * time.Second

	// StreamReconnectFlapThreshold is the minimum lifetime an attempt must
	// reach before its end counts as a recovery (i.e. resets the backoff to
	// StreamReconnectBaseBackoff). A connect that dies instantly is a flap, not
	// a recovery: without the guard, a crash-looping server that accepts and
	// immediately EOFs would reset the backoff on every cycle and be hammered
	// at the base rate forever. This mirrors the proxyd forwarder's
	// streamFlapThreshold precedent (see internal/proxyd/forwarder.go), but is
	// a SEPARATE knob by plan decision — the forwarder keeps its own constant
	// and does not read this one.
	StreamReconnectFlapThreshold = 1 * time.Second
)

// Log configuration
const (
	// DefaultLogLimit is the default number of log lines to return
	DefaultLogLimit = 100

	// MaxLogLines is the maximum number of log lines that can be requested
	// to prevent memory exhaustion (DoS protection)
	MaxLogLines = 10000
)

// Proxy request configuration
const (
	// DefaultProxyRequestLimit is the default number of proxy requests to return
	DefaultProxyRequestLimit = 100

	// MaxProxyRequests is the maximum number of proxy requests a single
	// API/daemon call may ask for — the `?limit=` clamp shared by the project
	// API's parseProxyRequestParams and the daemon socket's /api/v1/requests
	// (DoS protection) — AND, by the definition of
	// DefaultProxyRequestBufferSize below, the proxy request ring size. The two
	// are deliberately the same number: see that constant for the invariant.
	//
	// Raised from 1000 to 5000 (plan 018, D9). Ring memory is bounded not by
	// this count but by ProxyRequestDetailWindow, which limits how many of
	// those records keep their captured bodies.
	MaxProxyRequests = 5000

	// TUIRequestsSyncLimit is how many of the newest proxy requests the TUI
	// fetches in its initial sync, and the page size it uses to scroll further
	// back. It is deliberately SMALLER than the ring (MaxProxyRequests): a TUI
	// needs a screenful plus comfortable scroll depth, not the whole ring, and
	// paging the remainder on demand through the BeforeID cursor keeps startup
	// cost independent of how deep retention goes. It doubles as that scroll-back
	// PAGE size. It must not exceed the TUI's own display ring
	// (tui.maxRequestHistory, which is defined from MaxProxyRequests — the TUI
	// may hold the whole server ring once the user has paged through it), or the
	// snapshot it just paid to fetch would be trimmed on arrival.
	//
	// Equal to ProxyRequestDetailWindow today by coincidence, not by contract:
	// one is a client fetch size, the other a server retention policy. Retune
	// either without touching the other.
	TUIRequestsSyncLimit = 1000

	// ProxyRequestDetailWindow is how many of the NEWEST records in a proxy
	// request ring keep their captured BODY data (plan 018, D9b). When a record
	// falls outside this window the ring drops its inline body bytes and
	// unlinks its spilled body files, keeping the record itself plus its
	// captured HEADERS; a later body fetch for it reports unavailable_reason
	// "evicted", exactly as a disk-budget-evicted body does. This is what
	// bounds ring memory: 5000 records of metadata and headers is cheap, 5000
	// records of bodies (up to the 64KB inline threshold each, twice per
	// record) is not.
	ProxyRequestDetailWindow = 1000
)

// Buffer sizes
const (
	// DefaultLogBufferSize is the default size for log buffers
	DefaultLogBufferSize = 1000

	// DefaultSubscriptionBuffer is the default size for subscription buffers
	DefaultSubscriptionBuffer = 100

	// ScannerBufferSize is the initial buffer size for log line scanning
	ScannerBufferSize = 64 * 1024 // 64KB

	// ScannerMaxBufferSize is the maximum buffer size for log line scanning
	ScannerMaxBufferSize = 1024 * 1024 // 1MB

	// DefaultProxyRequestBufferSize is the default number of proxy requests to
	// keep in memory. It is DEFINED as MaxProxyRequests rather than merely
	// happening to equal it, because the shared-daemon forwarder backfills a
	// full daemon ring in ONE fetch of MaxProxyRequests records
	// (proxyd.backfillSnapshot): if the ring ever grew past the per-call limit
	// clamp, a reconnect could no longer restore the whole ring and the two
	// numbers would have silently drifted apart.
	//
	// Only the newest ProxyRequestDetailWindow of these records keep their
	// captured body data; the rest keep metadata and headers only.
	DefaultProxyRequestBufferSize = MaxProxyRequests
)

// Request capture configuration
const (
	// DefaultCaptureMaxBodySize is the maximum body size to capture per request/response (1MB)
	DefaultCaptureMaxBodySize = 1 * 1024 * 1024

	// DefaultCaptureInlineThreshold is the maximum body size to store inline in memory (64KB)
	// Bodies larger than this are stored on disk
	DefaultCaptureInlineThreshold = 64 * 1024

	// DefaultCaptureDiskBudget is the default daemon-wide (and standalone
	// per-project) ceiling on the TOTAL bytes of spilled capture body files on
	// disk (#69). Only bodies larger than DefaultCaptureInlineThreshold spill to
	// disk; once their combined size would exceed this budget the capture
	// accountant evicts the oldest record groups (oldest-first FIFO by first-spill
	// time) until it fits. The daemon folds every registered capture-enabled
	// project's budget into a single effective bound as the min of each
	// project's budget-or-default (an unset project contributes this default,
	// so one project can never raise another's bound); raising the effective
	// bound above this default requires every capture-enabled project to opt in
	// with an explicit proxy.capture.disk_budget above it (see
	// Registry.EffectiveCaptureDiskBudget). 1GiB.
	DefaultCaptureDiskBudget = 1 * 1024 * 1024 * 1024

	// CaptureDirectory is the directory name for storing captured body files
	CaptureDirectory = ".prox/capture"

	// MaxDecodedBodySize caps the number of bytes produced when decoding a
	// content-encoded (e.g. gzip) captured body at serve time. Exceeding the cap
	// is treated as a decode failure so a highly-compressible payload (zip bomb)
	// cannot exhaust memory. 10MB.
	MaxDecodedBodySize = 10 * 1024 * 1024

	// MaxSSEEventSize bounds a single SSE event the forwarder buffers from the
	// daemon's request stream. It is derived from DefaultCaptureMaxBodySize
	// rather than hard-coded: the 64KB inline threshold does NOT bound an event,
	// because on a capture disk-write failure both the request and response body
	// fall back to inline storage up to the max body size (see capture.go), so a
	// single record can carry two max-size bodies base64-encoded (~1.4MB each),
	// and headers are unbounded by the body threshold. The cap is twice the
	// base64-expanded max body size plus 1MB of slack for headers and JSON
	// framing (~3.7MB). An event exceeding it is skipped and the stream
	// continues. base64 std-encoding expands n bytes to ((n+2)/3)*4.
	MaxSSEEventSize = 2*(((DefaultCaptureMaxBodySize+2)/3)*4) + 1024*1024
)

// DaemonCaptureDir returns the daemon's shared capture directory
// (homeDir/.prox/capture) given a home directory. Kept here so both the api and
// proxyd packages can derive the path without an import cycle.
func DaemonCaptureDir(homeDir string) string {
	return filepath.Join(homeDir, CaptureDirectory)
}

// Proxy timeouts
const (
	// DefaultProxyBackendTimeout is the timeout for backend connections
	DefaultProxyBackendTimeout = 30 * time.Second

	// DefaultProxyReadHeaderTimeout is the timeout for reading request headers only.
	// Unlike ReadTimeout/WriteTimeout, this does not set a deadline on the full
	// connection lifetime, allowing long-lived connections (WebSocket, SSE) to
	// remain open indefinitely.
	DefaultProxyReadHeaderTimeout = 10 * time.Second

	// DefaultProxyIdleTimeout is the timeout for idle connections
	DefaultProxyIdleTimeout = 120 * time.Second

	// DefaultProxyDialTimeout is the timeout for dialing backend connections
	DefaultProxyDialTimeout = 30 * time.Second

	// DefaultProxyKeepAlive is the keep-alive duration for backend connections
	DefaultProxyKeepAlive = 30 * time.Second

	// DefaultProxyIdleConnTimeout is the timeout for idle connections in the transport
	DefaultProxyIdleConnTimeout = 90 * time.Second

	// DefaultProxyMaxIdleConns is the maximum number of idle connections
	DefaultProxyMaxIdleConns = 100
)

// File permissions
const (
	// FilePermissionDefault is the default permission for regular files (0644)
	FilePermissionDefault = 0644

	// DirPermissionPrivate is the permission for private directories (0700)
	DirPermissionPrivate = 0700

	// FilePermissionPrivate is the permission for sensitive files like tokens and keys (0600)
	FilePermissionPrivate = 0600
)

// ANSI color codes for terminal output
var (
	// ProcessColors are the colors used for process names in terminal output
	ProcessColors = []string{
		"\033[36m", // cyan
		"\033[33m", // yellow
		"\033[32m", // green
		"\033[35m", // magenta
		"\033[34m", // blue
		"\033[31m", // red
	}

	// ColorReset resets the terminal color
	ColorReset = "\033[0m"

	// ColorBrightRed is used for stderr output
	ColorBrightRed = "\033[91m"

	// HTTP status code colors
	ColorStatusSuccess  = "\033[32m" // green (2xx)
	ColorStatusRedirect = "\033[36m" // cyan (3xx)
	ColorStatusClient   = "\033[33m" // yellow (4xx)
	ColorStatusServer   = "\033[31m" // red (5xx)
)
