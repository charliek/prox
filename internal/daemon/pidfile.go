package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PIDFile manages a PID file with file locking.
//
// PIDFile is not safe for concurrent use. Callers must ensure that
// Create and Release are not called concurrently on the same instance.
type PIDFile struct {
	path string
	file *os.File
}

// NewPIDFile creates a new PIDFile manager for the given path
func NewPIDFile(path string) *PIDFile {
	return &PIDFile{path: path}
}

// Create creates and locks the PID file, writing the current process's PID.
// Returns ErrPIDFileLocked if another process holds the lock.
func (p *PIDFile) Create() error {
	// Open file for writing, create if not exists
	f, err := os.OpenFile(p.path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("opening PID file: %w", err)
	}

	// Try to acquire exclusive lock (non-blocking)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return ErrPIDFileLocked
		}
		return fmt.Errorf("locking PID file: %w", err)
	}

	// Truncate and write PID
	if err := f.Truncate(0); err != nil {
		p.releaseAndClose(f)
		return fmt.Errorf("truncating PID file: %w", err)
	}

	if _, err := f.Seek(0, 0); err != nil {
		p.releaseAndClose(f)
		return fmt.Errorf("seeking PID file: %w", err)
	}

	pid := os.Getpid()
	if _, err := fmt.Fprintf(f, "%d\n", pid); err != nil {
		p.releaseAndClose(f)
		return fmt.Errorf("writing PID: %w", err)
	}

	// Sync to ensure data is written
	if err := f.Sync(); err != nil {
		p.releaseAndClose(f)
		return fmt.Errorf("syncing PID file: %w", err)
	}

	p.file = f
	return nil
}

// Release unlocks and removes the PID file
func (p *PIDFile) Release() error {
	if p.file == nil {
		return nil
	}

	// Unlock - ignore error since we're cleaning up anyway
	_ = syscall.Flock(int(p.file.Fd()), syscall.LOCK_UN)

	// Close - ignore error since we're cleaning up anyway
	_ = p.file.Close()

	p.file = nil

	// Remove file
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing PID file: %w", err)
	}

	return nil
}

// releaseAndClose unlocks and closes the file without removing it.
// Warnings are written directly to stderr. In daemon mode, stderr is redirected
// to the log file, so these warnings will be captured there. This is acceptable
// because these warnings occur during cleanup after an error and are not critical.
func (p *PIDFile) releaseAndClose(f *os.File) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to unlock PID file: %v\n", err)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to close PID file: %v\n", err)
	}
}

// IsLocked checks if the PID file is locked by another process.
// Returns true if locked, false if not locked or file doesn't exist.
func IsLocked(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false // File doesn't exist or can't be opened
	}
	defer f.Close()

	// Try to acquire shared lock (non-blocking)
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err != nil {
		return true // Can't get lock, so it's held exclusively by another process
	}

	// Got the lock, release it
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// ReadPID reads the PID from a PID file
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading PID file: %w", err)
	}

	pidStr := strings.TrimSpace(string(data))

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("parsing PID: %w", err)
	}

	return pid, nil
}

// ProcessExists checks if a process with the given PID exists
func ProcessExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// On Unix, FindProcess always succeeds, so we need to send signal 0 to check.
	// If we get EPERM, the process exists but we don't have permission to signal it.
	err = process.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
