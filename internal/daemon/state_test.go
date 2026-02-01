package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestState_Write_Validation(t *testing.T) {
	t.Run("port validation", func(t *testing.T) {
		tests := []struct {
			name    string
			port    int
			wantErr bool
		}{
			{"valid min port", 1, false},
			{"valid mid port", 8080, false},
			{"valid max port", 65535, false},
			{"invalid zero port", 0, true},
			{"invalid negative port", -1, true},
			{"invalid too high port", 65536, true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				state := &State{PID: 1, Port: tt.port, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
				err := state.Write(tmpDir)

				if tt.wantErr && err == nil {
					t.Errorf("expected error for port %d, got nil", tt.port)
				}
				if !tt.wantErr && err != nil {
					t.Errorf("unexpected error for port %d: %v", tt.port, err)
				}
			})
		}
	})

	t.Run("PID validation", func(t *testing.T) {
		tmpDir := t.TempDir()
		state := &State{PID: 0, Port: 5555, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
		err := state.Write(tmpDir)
		if err == nil {
			t.Error("expected error for zero PID")
		}

		state = &State{PID: -1, Port: 5555, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
		err = state.Write(tmpDir)
		if err == nil {
			t.Error("expected error for negative PID")
		}
	})

	t.Run("host validation", func(t *testing.T) {
		tmpDir := t.TempDir()
		state := &State{PID: 1, Port: 5555, Host: "", ConfigFile: "prox.yaml"}
		err := state.Write(tmpDir)
		if err == nil {
			t.Error("expected error for empty host")
		}
	})

	t.Run("config file validation", func(t *testing.T) {
		tmpDir := t.TempDir()
		state := &State{PID: 1, Port: 5555, Host: "127.0.0.1", ConfigFile: ""}
		err := state.Write(tmpDir)
		if err == nil {
			t.Error("expected error for empty config file")
		}
	})
}

func TestState_WriteAndLoad(t *testing.T) {
	t.Run("round-trip serialization", func(t *testing.T) {
		tmpDir := t.TempDir()

		original := &State{
			PID:        12345,
			Port:       5555,
			Host:       "127.0.0.1",
			StartedAt:  time.Now().Truncate(time.Second), // Truncate for comparison
			ConfigFile: "prox.yaml",
		}

		// Write state
		err := original.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Verify state file exists
		statePath := filepath.Join(tmpDir, StateDirName, StateFileName)
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			t.Fatal("state file was not created")
		}

		// Load state
		loaded, err := LoadState(tmpDir)
		if err != nil {
			t.Fatalf("LoadState failed: %v", err)
		}

		// Compare
		if loaded.PID != original.PID {
			t.Errorf("PID mismatch: got %d, want %d", loaded.PID, original.PID)
		}
		if loaded.Port != original.Port {
			t.Errorf("Port mismatch: got %d, want %d", loaded.Port, original.Port)
		}
		if loaded.Host != original.Host {
			t.Errorf("Host mismatch: got %s, want %s", loaded.Host, original.Host)
		}
		if loaded.ConfigFile != original.ConfigFile {
			t.Errorf("ConfigFile mismatch: got %s, want %s", loaded.ConfigFile, original.ConfigFile)
		}
		// Compare time with some tolerance
		if loaded.StartedAt.Unix() != original.StartedAt.Unix() {
			t.Errorf("StartedAt mismatch: got %v, want %v", loaded.StartedAt, original.StartedAt)
		}
	})

	t.Run("creates state directory if not exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		stateDir := filepath.Join(tmpDir, StateDirName)

		// Ensure state dir doesn't exist
		if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
			t.Fatal("state dir should not exist initially")
		}

		state := &State{PID: 1, Port: 5555, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
		err := state.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Now state dir should exist
		if _, err := os.Stat(stateDir); os.IsNotExist(err) {
			t.Fatal("state dir should have been created")
		}
	})
}

func TestLoadState_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	_, err := LoadState(tmpDir)
	if err != ErrStateNotFound {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestStateDir(t *testing.T) {
	t.Run("returns correct path with dir", func(t *testing.T) {
		dir := "/some/path"
		expected := "/some/path/.prox"
		result := StateDir(dir)
		if result != expected {
			t.Errorf("expected %s, got %s", expected, result)
		}
	})

	t.Run("uses cwd when dir is empty", func(t *testing.T) {
		cwd, _ := os.Getwd()
		expected := filepath.Join(cwd, StateDirName)
		result := StateDir("")
		if result != expected {
			t.Errorf("expected %s, got %s", expected, result)
		}
	})
}

func TestStatePath(t *testing.T) {
	dir := "/project"
	expected := "/project/.prox/prox.state"
	result := StatePath(dir)
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestPIDPath(t *testing.T) {
	dir := "/project"
	expected := "/project/.prox/prox.pid"
	result := PIDPath(dir)
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestLogPath(t *testing.T) {
	dir := "/project"
	expected := "/project/.prox/prox.log"
	result := LogPath(dir)
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestRemoveState(t *testing.T) {
	t.Run("removes existing state file", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create state
		state := &State{PID: 1, Port: 5555, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
		err := state.Write(tmpDir)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Remove state
		err = RemoveState(tmpDir)
		if err != nil {
			t.Fatalf("RemoveState failed: %v", err)
		}

		// Verify removal
		_, err = LoadState(tmpDir)
		if err != ErrStateNotFound {
			t.Errorf("expected ErrStateNotFound after removal, got %v", err)
		}
	})

	t.Run("no error when state file doesn't exist", func(t *testing.T) {
		tmpDir := t.TempDir()

		err := RemoveState(tmpDir)
		if err != nil {
			t.Errorf("expected no error for non-existent file, got %v", err)
		}
	})
}

func TestEnsureStateDir(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, StateDirName)

	// Ensure state dir doesn't exist initially
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("state dir should not exist initially")
	}

	err := EnsureStateDir(tmpDir)
	if err != nil {
		t.Fatalf("EnsureStateDir failed: %v", err)
	}

	// Check directory was created
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("state dir was not created: %v", err)
	}

	if !info.IsDir() {
		t.Fatal("state dir is not a directory")
	}

	// Check permissions (should be 0700)
	mode := info.Mode().Perm()
	if mode != 0700 {
		t.Errorf("expected permissions 0700, got %o", mode)
	}
}

func TestCleanupStateDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create state and PID file
	state := &State{PID: 1, Port: 5555, Host: "127.0.0.1", ConfigFile: "prox.yaml"}
	err := state.Write(tmpDir)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	pidPath := PIDPath(tmpDir)
	err = os.WriteFile(pidPath, []byte("12345\n"), 0600)
	if err != nil {
		t.Fatalf("creating PID file failed: %v", err)
	}

	// Cleanup
	err = CleanupStateDir(tmpDir)
	if err != nil {
		t.Fatalf("CleanupStateDir failed: %v", err)
	}

	// Verify state file removed
	if _, err := os.Stat(StatePath(tmpDir)); !os.IsNotExist(err) {
		t.Error("state file should have been removed")
	}

	// Verify PID file removed
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should have been removed")
	}

	// Verify .prox directory still exists (we don't remove it)
	if _, err := os.Stat(StateDir(tmpDir)); os.IsNotExist(err) {
		t.Error(".prox directory should still exist")
	}
}
