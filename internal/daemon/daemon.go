package daemon

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
)

const (
	// DaemonEnvVar is the environment variable used to detect daemon child process
	DaemonEnvVar = "_PROX_DAEMON"
)

// IsDaemonChild returns true if this process is a daemon child process
func IsDaemonChild() bool {
	return os.Getenv(DaemonEnvVar) == "1"
}

// buildDaemonCommand constructs (but does not start) the detached daemon-child
// command. It is split out from Daemonize so a test can pin the child protocol
// (executable, argv, _PROX_DAEMON=1 env marker, Setsid, nil stdio) without
// actually spawning a process.
//
// The child protocol here is byte-for-byte identical to prox's pre-D2 behavior:
// same executable, same os.Args[1:], _PROX_DAEMON=1 appended to the inherited
// environment, Setsid to detach from the controlling terminal, and nil stdio
// (the child redirects its own output to the daemon log via SetupLogging).
func buildDaemonCommand() (*exec.Cmd, error) {
	// Get the current executable path
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("getting executable path: %w", err)
	}

	// Create command with same args
	cmd := exec.Command(executable, os.Args[1:]...)

	// Prepare environment with daemon marker
	cmd.Env = append(os.Environ(), DaemonEnvVar+"=1")

	// Detach from terminal - create new session
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Don't inherit stdin/stdout/stderr - daemon manages its own logging
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	return cmd, nil
}

// Daemonize re-executes the current process as a detached daemon child and
// returns the started child command. Unlike its pre-D2 form, it no longer
// prints a success line or calls os.Exit(0): the caller (runUp's daemonize
// branch) owns wait-and-report so `prox up -d` only reports success once the
// child is actually ready (D2). The returned *exec.Cmd is the direct child, so
// the caller must Wait() on it (both to detect early death and to reap the
// zombie).
//
// The child continues execution where IsDaemonChild() returns true. The child
// protocol is unchanged — see buildDaemonCommand.
func Daemonize() (*exec.Cmd, error) {
	cmd, err := buildDaemonCommand()
	if err != nil {
		return nil, err
	}

	// Start the daemon process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting daemon process: %w", err)
	}

	return cmd, nil
}

// SetupLogging redirects stdout and stderr to the daemon log file.
// Should be called early in the daemon child process.
func SetupLogging(dir string) (*os.File, error) {
	if err := EnsureStateDir(dir); err != nil {
		return nil, err
	}

	logPath := LogPath(dir)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	// Write a run marker BEFORE redirecting stdout/stderr below, so a failure
	// diagnostic (internal/cli's printDaemonFailure) can tail only this run's
	// output rather than the whole never-truncated history of the log (#99).
	// This process's own pid is exactly the pid the parent's daemonChild
	// tracks: Daemonize starts this process directly (no double-fork), so
	// os.Getpid() here equals child.Pid() in the parent.
	if err := WriteRunMarker(logFile, os.Getpid()); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("writing run marker: %w", err)
	}

	// Redirect stdout and stderr
	os.Stdout = logFile
	os.Stderr = logFile

	return logFile, nil
}

// FindAvailablePort finds an available TCP port on the given host.
// Returns the port number or an error if no port could be found.
func FindAvailablePort(host string) (int, error) {
	// Use port 0 to let the OS assign an available port
	addr := net.JoinHostPort(host, "0")
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("finding available port: %w", err)
	}
	defer listener.Close()

	// Get the assigned port
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected address type: %T", listener.Addr())
	}

	return tcpAddr.Port, nil
}

// IsRunning checks if a prox instance is running in the given directory.
// Returns true if running, false otherwise.
//
// Note: This is a best-effort check. There is a small race window between
// checking the PID file lock and loading state where the process could stop.
// For authoritative checks, use PID file locking directly via NewPIDFile.
func IsRunning(dir string) bool {
	pidPath := PIDPath(dir)

	// First check if PID file is locked
	if IsLocked(pidPath) {
		return true
	}

	// If not locked, check if state file exists and process is running
	state, err := LoadState(dir)
	if err != nil {
		return false
	}

	return ProcessExists(state.PID)
}

// GetRunningState returns the state of a running prox instance, if any.
// Returns ErrNotRunning if no instance is running.
func GetRunningState(dir string) (*State, error) {
	if !IsRunning(dir) {
		return nil, ErrNotRunning
	}

	state, err := LoadState(dir)
	if err != nil {
		return nil, err
	}

	return state, nil
}

// CleanupStaleFiles removes stale state files if the process is not running.
// This handles crash recovery scenarios.
func CleanupStaleFiles(dir string) error {
	pidPath := PIDPath(dir)

	// If PID file is locked, process is running - don't cleanup
	if IsLocked(pidPath) {
		return ErrAlreadyRunning
	}

	// Check if state file exists
	state, err := LoadState(dir)
	if err != nil {
		if err == ErrStateNotFound {
			return nil // Nothing to clean up
		}
		return err
	}

	// If process is still running, don't cleanup
	if ProcessExists(state.PID) {
		return ErrAlreadyRunning
	}

	// Process is not running - clean up stale files
	return CleanupStateDir(dir)
}
