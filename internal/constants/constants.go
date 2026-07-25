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

	// MaxProxyRequests is the maximum number of proxy requests that can be requested
	// to prevent memory exhaustion (DoS protection)
	MaxProxyRequests = 1000
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

	// DefaultProxyRequestBufferSize is the default number of proxy requests to keep in memory
	DefaultProxyRequestBufferSize = 1000
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
