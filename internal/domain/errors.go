package domain

import (
	"errors"
	"fmt"
	"strings"
)

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

// ProcessStopFailure names one process whose whole-supervisor stop did not
// complete cleanly, carrying the error that process's Stop returned. Err wraps a
// sentinel (e.g. ErrProcessGroupNotReaped) so callers can classify it with
// errors.Is through the aggregate below (#36, D3).
type ProcessStopFailure struct {
	Name string
	Err  error
}

// ProcessStopError aggregates the per-process failures from a single
// Supervisor.Stop. It is returned (as a non-nil *ProcessStopError) only when at
// least one process failed to stop cleanly; a clean stop -- or a stop with
// nothing to do -- returns a nil error. Failures are sorted by process name for
// stable output.
//
// It implements Unwrap() []error so errors.Is/errors.As see through the
// aggregate to each failure's wrapped sentinel: errors.Is(err,
// ErrProcessGroupNotReaped) is true when any survivor could not be reaped, and
// errors.As(err, &*ProcessStopError) extracts the typed aggregate for
// serialization (#36, D3).
type ProcessStopError struct {
	Failures []ProcessStopFailure
}

// Error renders a readable multi-process message naming each survivor and its
// underlying error.
func (e *ProcessStopError) Error() string {
	if len(e.Failures) == 0 {
		// Defensive: a ProcessStopError is only constructed with failures, but
		// never render an empty, misleading "0 processes" message.
		return "process stop failed"
	}
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s: %v", f.Name, f.Err)
	}
	if len(parts) == 1 {
		return "process failed to stop cleanly: " + parts[0]
	}
	return fmt.Sprintf("%d processes failed to stop cleanly: %s", len(parts), strings.Join(parts, "; "))
}

// Unwrap exposes each failure's error so errors.Is/errors.As traverse the
// aggregate (Go 1.20 multi-error unwrap).
func (e *ProcessStopError) Unwrap() []error {
	errs := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		errs[i] = f.Err
	}
	return errs
}

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
	// ErrCodeCursorGone marks a before_id cursor whose anchor record is
	// unknown, evicted, or out of the request's filter scope (e.g. a
	// different project). 410, not 404: the cursor once referred to a real
	// position that has since aged out of the ring, so pollers should
	// restart pagination without a cursor rather than retry the same one
	// (D12, #50).
	ErrCodeCursorGone = "CURSOR_GONE"
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
