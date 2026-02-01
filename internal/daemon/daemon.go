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

// Daemonize re-executes the current process as a daemon.
//
// IMPORTANT: In the parent process, this function calls os.Exit(0) and never returns.
// Only the child process continues execution (where IsDaemonChild() returns true).
//
// The function:
//  1. Re-executes the current binary with the same arguments
//  2. Sets _PROX_DAEMON=1 environment variable to mark the child
//  3. Detaches the child from the terminal (new session)
//  4. Prints the child PID and exits the parent with status 0
//
// Note: There is a small race window where the parent exits before confirming
// the child successfully initialized. This is acceptable because:
//   - Client commands use discoverDaemon() which reads state and retries
//   - The window is very small (child starts immediately)
//   - A pipe-based confirmation would add significant complexity
func Daemonize() error {
	// Get the current executable path
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting executable path: %w", err)
	}

	// Prepare environment with daemon marker
	env := append(os.Environ(), DaemonEnvVar+"=1")

	// Create command with same args
	cmd := exec.Command(executable, os.Args[1:]...)
	cmd.Env = env

	// Detach from terminal - create new session
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Don't inherit stdin/stdout/stderr - daemon manages its own logging
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start the daemon process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting daemon process: %w", err)
	}

	// Return the child's PID
	fmt.Printf("prox started (pid %d)\n", cmd.Process.Pid)

	// Parent exits successfully
	os.Exit(0)

	return nil // Unreachable, but needed for compiler
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
