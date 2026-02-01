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
)

// Error codes for API responses
const (
	ErrCodeProcessNotFound       = "PROCESS_NOT_FOUND"
	ErrCodeProcessAlreadyRunning = "PROCESS_ALREADY_RUNNING"
	ErrCodeProcessNotRunning     = "PROCESS_NOT_RUNNING"
	ErrCodeInvalidPattern        = "INVALID_PATTERN"
	ErrCodeShutdownInProgress    = "SHUTDOWN_IN_PROGRESS"

	// Proxy-related error codes (API-only, no sentinel errors as they
	// are only used for HTTP response formatting in the API layer)
	ErrCodeProxyNotEnabled       = "PROXY_NOT_ENABLED"
	ErrCodeStreamingNotSupported = "STREAMING_NOT_SUPPORTED"
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
	default:
		return "INTERNAL_ERROR"
	}
}
