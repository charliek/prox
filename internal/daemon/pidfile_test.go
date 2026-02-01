package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPIDFile_Create(t *testing.T) {
	t.Run("creates and locks file", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		pf := NewPIDFile(pidPath)
		err := pf.Create()
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		defer pf.Release()

		// File should exist
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			t.Fatal("PID file was not created")
		}

		// File should contain our PID
		data, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatalf("reading PID file: %v", err)
		}

		pid := os.Getpid()
		expected := pid
		actual, err := ReadPID(pidPath)
		if err != nil {
			t.Fatalf("ReadPID failed: %v", err)
		}
		if actual != expected {
			t.Errorf("PID mismatch: got %d, want %d (raw: %s)", actual, expected, string(data))
		}
	})

	t.Run("detects locked file", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		// Create first PID file
		pf1 := NewPIDFile(pidPath)
		err := pf1.Create()
		if err != nil {
			t.Fatalf("First Create failed: %v", err)
		}
		defer pf1.Release()

		// Try to create second PID file - should fail
		pf2 := NewPIDFile(pidPath)
		err = pf2.Create()
		if err != ErrPIDFileLocked {
			t.Errorf("expected ErrPIDFileLocked, got %v", err)
		}
	})
}

func TestPIDFile_Release(t *testing.T) {
	t.Run("unlocks and removes file", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		pf := NewPIDFile(pidPath)
		err := pf.Create()
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		// Release
		err = pf.Release()
		if err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		// File should be removed
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Error("PID file should have been removed")
		}

		// Should be able to create new PID file
		pf2 := NewPIDFile(pidPath)
		err = pf2.Create()
		if err != nil {
			t.Errorf("should be able to create after release: %v", err)
		}
		pf2.Release()
	})

	t.Run("release is idempotent", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		pf := NewPIDFile(pidPath)
		err := pf.Create()
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		// Multiple releases should not error
		err = pf.Release()
		if err != nil {
			t.Errorf("first Release failed: %v", err)
		}

		err = pf.Release()
		if err != nil {
			t.Errorf("second Release failed: %v", err)
		}
	})
}

func TestIsLocked(t *testing.T) {
	t.Run("returns true when locked", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		pf := NewPIDFile(pidPath)
		err := pf.Create()
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		defer pf.Release()

		if !IsLocked(pidPath) {
			t.Error("expected IsLocked to return true")
		}
	})

	t.Run("returns false when not locked", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		// Create file without locking
		err := os.WriteFile(pidPath, []byte("12345\n"), 0600)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		if IsLocked(pidPath) {
			t.Error("expected IsLocked to return false for unlocked file")
		}
	})

	t.Run("returns false when file doesn't exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "nonexistent.pid")

		if IsLocked(pidPath) {
			t.Error("expected IsLocked to return false for non-existent file")
		}
	})
}

func TestReadPID(t *testing.T) {
	t.Run("reads valid PID", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		err := os.WriteFile(pidPath, []byte("12345\n"), 0600)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		pid, err := ReadPID(pidPath)
		if err != nil {
			t.Fatalf("ReadPID failed: %v", err)
		}
		if pid != 12345 {
			t.Errorf("expected 12345, got %d", pid)
		}
	})

	t.Run("handles whitespace", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		err := os.WriteFile(pidPath, []byte("  12345 \n\n"), 0600)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		pid, err := ReadPID(pidPath)
		if err != nil {
			t.Fatalf("ReadPID failed: %v", err)
		}
		if pid != 12345 {
			t.Errorf("expected 12345, got %d", pid)
		}
	})

	t.Run("error on non-existent file", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "nonexistent.pid")

		_, err := ReadPID(pidPath)
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("error on invalid content", func(t *testing.T) {
		tmpDir := t.TempDir()
		pidPath := filepath.Join(tmpDir, "test.pid")

		err := os.WriteFile(pidPath, []byte("not-a-number\n"), 0600)
		if err != nil {
			t.Fatalf("creating file: %v", err)
		}

		_, err = ReadPID(pidPath)
		if err == nil {
			t.Error("expected error for invalid content")
		}
	})
}

func TestProcessExists(t *testing.T) {
	t.Run("returns true for current process", func(t *testing.T) {
		pid := os.Getpid()
		if !ProcessExists(pid) {
			t.Error("expected ProcessExists to return true for current process")
		}
	})

	t.Run("returns false for non-existent process", func(t *testing.T) {
		// Use a very large PID that's unlikely to exist
		// Note: This is not a perfect test as PIDs can wrap around
		pid := 4000000
		if ProcessExists(pid) {
			t.Error("expected ProcessExists to return false for non-existent process")
		}
	})
}
