//go:build darwin || linux

package daemon

import (
	"os"
	"os/exec"
	"testing"
)

// deadPIDLocal runs a process to completion and returns its (now reaped) PID —
// guaranteed not to name a live process. Mirrors the proxyd package helper but
// kept local to daemon.
func deadPIDLocal(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	return cmd.Process.Pid
}

func TestProcessStartTime_Self(t *testing.T) {
	first, ok := ProcessStartTime(os.Getpid())
	if !ok {
		t.Fatal("ProcessStartTime(self) ok = false, want true")
	}
	if first <= 0 {
		t.Errorf("ProcessStartTime(self) = %d, want > 0", first)
	}
	// A process's start token is a fixed generation discriminator — stable
	// across reads for the life of the process.
	second, ok := ProcessStartTime(os.Getpid())
	if !ok || second != first {
		t.Errorf("second read = (%d, %v), want (%d, true) — token must be stable", second, ok, first)
	}
}

func TestProcessStartTime_DeadPID(t *testing.T) {
	if _, ok := ProcessStartTime(deadPIDLocal(t)); ok {
		t.Error("ProcessStartTime(dead) ok = true, want false")
	}
}

func TestProcessStartTime_NonPositive(t *testing.T) {
	if _, ok := ProcessStartTime(0); ok {
		t.Error("ProcessStartTime(0) ok = true, want false")
	}
	if _, ok := ProcessStartTime(-1); ok {
		t.Error("ProcessStartTime(-1) ok = true, want false")
	}
}

func TestIsProcessAlive(t *testing.T) {
	self := os.Getpid()
	token, ok := ProcessStartTime(self)
	if !ok {
		t.Fatal("could not read self start token")
	}

	if !IsProcessAlive(self, token) {
		t.Error("IsProcessAlive(self, matching token) = false, want true")
	}
	if IsProcessAlive(self, token+1) {
		t.Error("IsProcessAlive(self, mismatched token) = true, want false")
	}
	// startTime == 0 falls back to bare ProcessExists, which is true for self.
	if !IsProcessAlive(self, 0) {
		t.Error("IsProcessAlive(self, 0) = false, want true (bare-PID fallback)")
	}
	// A dead PID is dead regardless of the stored token.
	if IsProcessAlive(deadPIDLocal(t), token) {
		t.Error("IsProcessAlive(dead, token) = true, want false")
	}
	// The "current token unreadable -> alive" fallback in IsProcessAlive cannot
	// be forced deterministically for a live PID on darwin/linux without a
	// contrived seam; it is covered by inspection.
}
