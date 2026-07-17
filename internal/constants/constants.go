// Package constants provides shared configuration values used across the prox application.
package constants

import "time"

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

	// CaptureDirectory is the directory name for storing captured body files
	CaptureDirectory = ".prox/capture"
)

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
