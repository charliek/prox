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
)

// Timeout and duration defaults
const (
	// DefaultRequestTimeout is the default timeout for API requests
	DefaultRequestTimeout = 30 * time.Second

	// DefaultShutdownTimeout is the default timeout for graceful shutdown
	DefaultShutdownTimeout = 10 * time.Second
)

// Log configuration
const (
	// DefaultLogLimit is the default number of log lines to return
	DefaultLogLimit = 100

	// MaxLogLines is the maximum number of log lines that can be requested
	// to prevent memory exhaustion (DoS protection)
	MaxLogLines = 10000
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
)
