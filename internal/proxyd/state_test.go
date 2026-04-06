package proxyd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaemonPaths(t *testing.T) {
	// These should not panic and should return non-empty strings
	if p := DaemonDir(); p == "" {
		t.Error("DaemonDir() returned empty string")
	}
	if p := SocketPath(); p == "" {
		t.Error("SocketPath() returned empty string")
	}
	if p := DaemonPIDPath(); p == "" {
		t.Error("DaemonPIDPath() returned empty string")
	}
	if p := DaemonStatePath(); p == "" {
		t.Error("DaemonStatePath() returned empty string")
	}
	if p := DaemonLogPath(); p == "" {
		t.Error("DaemonLogPath() returned empty string")
	}
}

func TestWriteAndLoadDaemonState(t *testing.T) {
	// Override the daemon dir to a temp directory for testing
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Ensure the .prox directory exists
	if err := EnsureDaemonDir(); err != nil {
		t.Fatalf("EnsureDaemonDir: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	state := &DaemonState{
		PID:       12345,
		Version:   "test-version",
		StartedAt: now,
	}

	if err := WriteDaemonState(state); err != nil {
		t.Fatalf("WriteDaemonState: %v", err)
	}

	loaded, err := LoadDaemonState()
	if err != nil {
		t.Fatalf("LoadDaemonState: %v", err)
	}

	if loaded.PID != 12345 {
		t.Errorf("PID = %d, want 12345", loaded.PID)
	}
	if loaded.Version != "test-version" {
		t.Errorf("Version = %q, want %q", loaded.Version, "test-version")
	}
	if !loaded.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", loaded.StartedAt, now)
	}
}

func TestLoadDaemonState_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := LoadDaemonState()
	if err == nil {
		t.Error("expected error for missing state, got nil")
	}
}

func TestEnsureDaemonDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if err := EnsureDaemonDir(); err != nil {
		t.Fatalf("EnsureDaemonDir: %v", err)
	}

	info, err := os.Stat(filepath.Join(tmpDir, DaemonDirName))
	if err != nil {
		t.Fatalf("stat daemon dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("daemon dir is not a directory")
	}
}

func TestCleanupDaemonState(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if err := EnsureDaemonDir(); err != nil {
		t.Fatalf("EnsureDaemonDir: %v", err)
	}

	// Create some files
	state := &DaemonState{PID: 1, Version: "v", StartedAt: time.Now()}
	if err := WriteDaemonState(state); err != nil {
		t.Fatalf("WriteDaemonState: %v", err)
	}

	// Write a dummy socket and PID file
	os.WriteFile(SocketPath(), []byte("dummy"), 0600)
	os.WriteFile(DaemonPIDPath(), []byte("1"), 0600)

	CleanupDaemonState()

	for _, path := range []string{DaemonStatePath(), DaemonPIDPath(), SocketPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("file %s should have been removed", path)
		}
	}
}
