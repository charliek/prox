package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// StateDirName is the name of the directory storing runtime state
	StateDirName = ".prox"
	// StateFileName is the name of the state file
	StateFileName = "prox.state"
	// PIDFileName is the name of the PID file
	PIDFileName = "prox.pid"
	// LogFileName is the name of the daemon log file
	LogFileName = "prox.log"
)

// State holds the runtime state of a running prox instance.
//
// State is not safe for concurrent use. Callers should ensure that
// Read and Write operations on the same state file are not performed
// concurrently. In typical usage, the daemon writes state once at startup
// and clients read it, so concurrent access is not expected.
type State struct {
	PID        int       `json:"pid"`
	Port       int       `json:"port"`
	Host       string    `json:"host"`
	StartedAt  time.Time `json:"started_at"`
	ConfigFile string    `json:"config_file"`
}

// Write writes the state to the state file in the given directory
func (s *State) Write(dir string) error {
	if s.PID <= 0 {
		return fmt.Errorf("invalid PID: %d", s.PID)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("invalid port: %d", s.Port)
	}
	if s.Host == "" {
		return fmt.Errorf("host cannot be empty")
	}
	if s.ConfigFile == "" {
		return fmt.Errorf("config file cannot be empty")
	}

	stateDir := filepath.Join(dir, StateDirName)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	statePath := filepath.Join(stateDir, StateFileName)
	f, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening state file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing state file: %w", err)
	}

	return nil
}

// LoadState reads the state from the state file in the given directory
func LoadState(dir string) (*State, error) {
	statePath := filepath.Join(dir, StateDirName, StateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrStateNotFound
		}
		return nil, fmt.Errorf("reading state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshaling state: %w", err)
	}

	return &state, nil
}

// RemoveState removes the state file from the given directory
func RemoveState(dir string) error {
	statePath := filepath.Join(dir, StateDirName, StateFileName)
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing state file: %w", err)
	}
	return nil
}

// StateDir returns the path to the .prox directory in the given directory.
// If dir is empty, uses the current working directory.
// If the working directory cannot be determined, falls back to a relative path.
func StateDir(dir string) string {
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			// Fall back to relative path rather than creating at root
			return StateDirName
		}
	}
	return filepath.Join(dir, StateDirName)
}

// StatePath returns the full path to the state file
func StatePath(dir string) string {
	return filepath.Join(StateDir(dir), StateFileName)
}

// PIDPath returns the full path to the PID file
func PIDPath(dir string) string {
	return filepath.Join(StateDir(dir), PIDFileName)
}

// LogPath returns the full path to the daemon log file
func LogPath(dir string) string {
	return filepath.Join(StateDir(dir), LogFileName)
}

// EnsureStateDir creates the .prox directory if it doesn't exist
func EnsureStateDir(dir string) error {
	stateDir := StateDir(dir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	return nil
}

// CleanupStateDir removes all state files from the .prox directory
func CleanupStateDir(dir string) error {
	stateDir := StateDir(dir)

	// Remove state file
	statePath := filepath.Join(stateDir, StateFileName)
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing state file: %w", err)
	}

	// Remove PID file
	pidPath := filepath.Join(stateDir, PIDFileName)
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing PID file: %w", err)
	}

	// Note: We don't remove the log file - it may be useful for debugging

	return nil
}
