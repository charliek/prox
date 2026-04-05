package proxyd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charliek/prox/internal/constants"
)

const (
	// DaemonDirName is the directory under $HOME for daemon state.
	DaemonDirName = ".prox"
	// DaemonSocketName is the Unix socket filename.
	DaemonSocketName = "proxy.sock"
	// DaemonPIDFileName is the daemon PID filename.
	DaemonPIDFileName = "proxy.pid"
	// DaemonStateFileName is the daemon state filename.
	DaemonStateFileName = "proxy.state"
	// DaemonLogFileName is the daemon log filename.
	DaemonLogFileName = "proxy.log"
)

// DaemonState holds the runtime state of the proxy daemon.
type DaemonState struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// DaemonDir returns the path to the ~/.prox directory.
func DaemonDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return DaemonDirName
	}
	return filepath.Join(home, DaemonDirName)
}

// SocketPath returns the path to the daemon Unix socket.
func SocketPath() string {
	return filepath.Join(DaemonDir(), DaemonSocketName)
}

// DaemonPIDPath returns the path to the daemon PID file.
func DaemonPIDPath() string {
	return filepath.Join(DaemonDir(), DaemonPIDFileName)
}

// DaemonStatePath returns the path to the daemon state file.
func DaemonStatePath() string {
	return filepath.Join(DaemonDir(), DaemonStateFileName)
}

// DaemonLogPath returns the path to the daemon log file.
func DaemonLogPath() string {
	return filepath.Join(DaemonDir(), DaemonLogFileName)
}

// EnsureDaemonDir creates the ~/.prox directory if it doesn't exist.
// Returns an error if the directory cannot be created (e.g., sandboxed environment).
func EnsureDaemonDir() error {
	dir := DaemonDir()
	if err := os.MkdirAll(dir, constants.DirPermissionPrivate); err != nil {
		return fmt.Errorf("cannot create daemon directory %s: %w", dir, err)
	}
	return nil
}

// WriteDaemonState writes the daemon state to ~/.prox/proxy.state.
func WriteDaemonState(state *DaemonState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling daemon state: %w", err)
	}

	statePath := DaemonStatePath()
	f, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, constants.FilePermissionPrivate)
	if err != nil {
		return fmt.Errorf("opening daemon state file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing daemon state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing daemon state file: %w", err)
	}
	return nil
}

// LoadDaemonState reads the daemon state from ~/.prox/proxy.state.
func LoadDaemonState() (*DaemonState, error) {
	data, err := os.ReadFile(DaemonStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("daemon state not found")
		}
		return nil, fmt.Errorf("reading daemon state: %w", err)
	}

	var state DaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshaling daemon state: %w", err)
	}
	return &state, nil
}

// CleanupDaemonState removes daemon state files (state, PID, socket).
func CleanupDaemonState() {
	os.Remove(DaemonStatePath())
	os.Remove(DaemonPIDPath())
	os.Remove(SocketPath())
}
