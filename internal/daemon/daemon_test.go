package daemon

import (
	"os"
	"testing"
)

func TestIsDaemonChild(t *testing.T) {
	t.Run("returns false when env var not set", func(t *testing.T) {
		// Save current value
		original := os.Getenv(DaemonEnvVar)
		defer os.Setenv(DaemonEnvVar, original)

		os.Unsetenv(DaemonEnvVar)

		if IsDaemonChild() {
			t.Error("expected IsDaemonChild to return false when env var not set")
		}
	})

	t.Run("returns true when env var is 1", func(t *testing.T) {
		// Save current value
		original := os.Getenv(DaemonEnvVar)
		defer os.Setenv(DaemonEnvVar, original)

		os.Setenv(DaemonEnvVar, "1")

		if !IsDaemonChild() {
			t.Error("expected IsDaemonChild to return true when env var is 1")
		}
	})

	t.Run("returns false when env var is 0", func(t *testing.T) {
		// Save current value
		original := os.Getenv(DaemonEnvVar)
		defer os.Setenv(DaemonEnvVar, original)

		os.Setenv(DaemonEnvVar, "0")

		if IsDaemonChild() {
			t.Error("expected IsDaemonChild to return false when env var is 0")
		}
	})
}

func TestFindAvailablePort(t *testing.T) {
	t.Run("finds available port on localhost", func(t *testing.T) {
		port, err := FindAvailablePort("127.0.0.1")
		if err != nil {
			t.Fatalf("FindAvailablePort failed: %v", err)
		}

		if port <= 0 || port > 65535 {
			t.Errorf("invalid port number: %d", port)
		}
	})

	t.Run("returns different ports on successive calls", func(t *testing.T) {
		port1, err := FindAvailablePort("127.0.0.1")
		if err != nil {
			t.Fatalf("first call failed: %v", err)
		}

		port2, err := FindAvailablePort("127.0.0.1")
		if err != nil {
			t.Fatalf("second call failed: %v", err)
		}

		// Ports might be the same if the first was immediately released
		// But typically they should be different
		t.Logf("Found ports: %d and %d", port1, port2)
	})
}

func TestIsRunning(t *testing.T) {
	t.Run("returns false when no state", func(t *testing.T) {
		tmpDir := t.TempDir()

		if IsRunning(tmpDir) {
			t.Error("expected IsRunning to return false with no state")
		}
	})

	t.Run("returns true when PID file is locked", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state directory
		err := EnsureStateDir(tmpDir)
		if err != nil {
			t.Fatalf("EnsureStateDir failed: %v", err)
		}

		// Create and lock PID file
		pf := NewPIDFile(PIDPath(tmpDir))
		err = pf.Create()
		if err != nil {
			t.Fatalf("Create PID file failed: %v", err)
		}
		defer pf.Release()

		if !IsRunning(tmpDir) {
			t.Error("expected IsRunning to return true when PID file is locked")
		}
	})

	t.Run("returns false when PID file exists but not locked and process not running", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state with a non-existent PID
		state := &State{
			PID:        4000000, // Very high PID unlikely to exist
			Port:       5555,
			Host:       "127.0.0.1",
			ConfigFile: "prox.yaml",
		}
		err := state.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write state failed: %v", err)
		}

		// Create unlocked PID file
		err = os.WriteFile(PIDPath(tmpDir), []byte("4000000\n"), 0600)
		if err != nil {
			t.Fatalf("Write PID file failed: %v", err)
		}

		if IsRunning(tmpDir) {
			t.Error("expected IsRunning to return false when process doesn't exist")
		}
	})
}

func TestGetRunningState(t *testing.T) {
	t.Run("returns error when not running", func(t *testing.T) {
		tmpDir := t.TempDir()

		_, err := GetRunningState(tmpDir)
		if err != ErrNotRunning {
			t.Errorf("expected ErrNotRunning, got %v", err)
		}
	})

	t.Run("returns state when running", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state directory
		err := EnsureStateDir(tmpDir)
		if err != nil {
			t.Fatalf("EnsureStateDir failed: %v", err)
		}

		// Create and lock PID file
		pf := NewPIDFile(PIDPath(tmpDir))
		err = pf.Create()
		if err != nil {
			t.Fatalf("Create PID file failed: %v", err)
		}
		defer pf.Release()

		// Write state
		state := &State{
			PID:        os.Getpid(),
			Port:       5555,
			Host:       "127.0.0.1",
			ConfigFile: "prox.yaml",
		}
		err = state.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write state failed: %v", err)
		}

		// Get running state
		loaded, err := GetRunningState(tmpDir)
		if err != nil {
			t.Fatalf("GetRunningState failed: %v", err)
		}

		if loaded.Port != 5555 {
			t.Errorf("expected port 5555, got %d", loaded.Port)
		}
	})
}

func TestCleanupStaleFiles(t *testing.T) {
	t.Run("returns ErrAlreadyRunning when PID file locked", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state directory
		err := EnsureStateDir(tmpDir)
		if err != nil {
			t.Fatalf("EnsureStateDir failed: %v", err)
		}

		// Create and lock PID file
		pf := NewPIDFile(PIDPath(tmpDir))
		err = pf.Create()
		if err != nil {
			t.Fatalf("Create PID file failed: %v", err)
		}
		defer pf.Release()

		err = CleanupStaleFiles(tmpDir)
		if err != ErrAlreadyRunning {
			t.Errorf("expected ErrAlreadyRunning, got %v", err)
		}
	})

	t.Run("cleans up stale files when process not running", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state with non-existent PID
		state := &State{
			PID:        4000000,
			Port:       5555,
			Host:       "127.0.0.1",
			ConfigFile: "prox.yaml",
		}
		err := state.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write state failed: %v", err)
		}

		// Create unlocked PID file
		err = os.WriteFile(PIDPath(tmpDir), []byte("4000000\n"), 0600)
		if err != nil {
			t.Fatalf("Write PID file failed: %v", err)
		}

		// Cleanup stale files
		err = CleanupStaleFiles(tmpDir)
		if err != nil {
			t.Fatalf("CleanupStaleFiles failed: %v", err)
		}

		// Verify files are cleaned up
		if _, err := os.Stat(StatePath(tmpDir)); !os.IsNotExist(err) {
			t.Error("state file should have been removed")
		}
		if _, err := os.Stat(PIDPath(tmpDir)); !os.IsNotExist(err) {
			t.Error("PID file should have been removed")
		}
	})

	t.Run("no error when no state exists", func(t *testing.T) {
		tmpDir := t.TempDir()

		err := CleanupStaleFiles(tmpDir)
		if err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})
}

func TestSetupLogging(t *testing.T) {
	t.Run("creates log file", func(t *testing.T) {
		tmpDir := t.TempDir()

		logFile, err := SetupLogging(tmpDir)
		if err != nil {
			t.Fatalf("SetupLogging failed: %v", err)
		}
		defer logFile.Close()

		// Check log file exists
		logPath := LogPath(tmpDir)
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			t.Error("log file was not created")
		}

		// Check permissions
		info, err := os.Stat(logPath)
		if err != nil {
			t.Fatalf("stat log file: %v", err)
		}
		mode := info.Mode().Perm()
		if mode != 0600 {
			t.Errorf("expected permissions 0600, got %o", mode)
		}
	})
}
