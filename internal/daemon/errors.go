package daemon

import "errors"

var (
	// ErrStateNotFound is returned when no state file exists
	ErrStateNotFound = errors.New("state file not found")
	// ErrAlreadyRunning is returned when prox is already running
	ErrAlreadyRunning = errors.New("prox is already running")
	// ErrNotRunning is returned when prox is not running
	ErrNotRunning = errors.New("prox is not running")
	// ErrPIDFileLocked is returned when the PID file is locked by another process
	ErrPIDFileLocked = errors.New("PID file is locked by another process")
)
