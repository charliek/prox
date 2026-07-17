package domain

import "errors"

// Domain errors
var (
	ErrProcessNotFound       = errors.New("process not found")
	ErrProcessAlreadyRunning = errors.New("process already running")
	ErrProcessNotRunning     = errors.New("process not running")
	ErrInvalidPattern        = errors.New("invalid filter pattern")
	ErrShutdownInProgress    = errors.New("shutdown in progress")
	ErrConfigNotFound        = errors.New("config file not found")
	ErrInvalidConfig         = errors.New("invalid configuration")
	ErrEnvReloadFailed       = errors.New("environment reload failed")
	ErrProcessGroupNotReaped = errors.New("process group could not be terminated")
	// ErrConfigReloadFailed wraps any failure to re-read/validate prox.yaml on an
	// API-driven (re)start (missing/unreadable file, YAML syntax, validation
	// failure). The wrapping detail carries the underlying loader message (#33, D3).
	ErrConfigReloadFailed = errors.New("config reload failed")
	// ErrProcessNotInConfig is returned when an API-driven (re)start reloads the
	// config and the target process is no longer present in it (#33, D3). The
	// caller must run `prox up` to reconcile added/removed processes.
	ErrProcessNotInConfig = errors.New("no longer in config")
)

// Error codes for API responses
const (
	ErrCodeProcessNotFound       = "PROCESS_NOT_FOUND"
	ErrCodeProcessAlreadyRunning = "PROCESS_ALREADY_RUNNING"
	ErrCodeProcessNotRunning     = "PROCESS_NOT_RUNNING"
	ErrCodeInvalidPattern        = "INVALID_PATTERN"
	ErrCodeShutdownInProgress    = "SHUTDOWN_IN_PROGRESS"
	ErrCodeEnvReloadFailed       = "ENV_RELOAD_FAILED"
	ErrCodeProcessGroupNotReaped = "PROCESS_GROUP_NOT_REAPED"
	ErrCodeConfigReloadFailed    = "CONFIG_RELOAD_FAILED"
	ErrCodeProcessNotInConfig    = "PROCESS_NOT_IN_CONFIG"

	// Proxy-related error codes (API-only, no sentinel errors as they
	// are only used for HTTP response formatting in the API layer)
	ErrCodeProxyNotEnabled       = "PROXY_NOT_ENABLED"
	ErrCodeStreamingNotSupported = "STREAMING_NOT_SUPPORTED"
	ErrCodeRequestNotFound       = "REQUEST_NOT_FOUND"
	ErrCodeMissingRequestID      = "MISSING_REQUEST_ID"
)

// ErrorCode returns the API error code for a domain error
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrProcessNotFound):
		return ErrCodeProcessNotFound
	case errors.Is(err, ErrProcessAlreadyRunning):
		return ErrCodeProcessAlreadyRunning
	case errors.Is(err, ErrProcessNotRunning):
		return ErrCodeProcessNotRunning
	case errors.Is(err, ErrInvalidPattern):
		return ErrCodeInvalidPattern
	case errors.Is(err, ErrShutdownInProgress):
		return ErrCodeShutdownInProgress
	case errors.Is(err, ErrEnvReloadFailed):
		return ErrCodeEnvReloadFailed
	case errors.Is(err, ErrProcessGroupNotReaped):
		return ErrCodeProcessGroupNotReaped
	case errors.Is(err, ErrConfigReloadFailed):
		return ErrCodeConfigReloadFailed
	case errors.Is(err, ErrProcessNotInConfig):
		return ErrCodeProcessNotInConfig
	default:
		return "INTERNAL_ERROR"
	}
}
