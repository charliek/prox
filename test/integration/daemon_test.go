package integration

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every detached launch below goes through f.StartDetached, which records the
// DAEMON's identity (the child `up -d` leaves behind, not the launcher that has
// already exited) and registers the waited teardown for it. The teardown these
// tests used to hand-roll -- a fire-and-forget POST /api/v1/shutdown followed by
// `time.Sleep(500ms)` -- recovered nothing when the daemon did not answer, and
// the 500ms sleep was a guess rather than a wait. See leakguard_test.go.

// detachedFixtureConfig is the trivial config most of these tests need: one
// long-lived process and nothing else. The `api:` block is dropped by the
// fixture, so each daemon binds a dynamic port and reports it in its own private
// .prox/prox.state.
const detachedFixtureConfig = `
processes:
  test: "sleep 60"
`

func TestDaemonMode_StartsInBackground(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, `
processes:
  test: "while true; do echo hello; sleep 1; done"
`)

	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)

	// Output should indicate daemon started
	if !strings.Contains(run.Output(), "prox started (pid") {
		t.Errorf("expected daemon start message, got: %s", run.Output())
	}

	// The state file exists by construction (the launcher waits for it), and
	// Addr reads the port back out of it.
	waitForStateFile(t, filepath.Join(run.StateDir(), daemonStateFileName), 10*time.Second)

	// Verify API is accessible
	waitForAPI(t, run.Addr(), apiReadyTimeout)
}

func TestDaemonMode_CreatesStateFile(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, detachedFixtureConfig)

	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)

	statePath := filepath.Join(run.StateDir(), daemonStateFileName)
	waitForStateFile(t, statePath, 10*time.Second)

	// Verify state file exists and read it
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	var state struct {
		PID        int    `json:"pid"`
		Port       int    `json:"port"`
		Host       string `json:"host"`
		ConfigFile string `json:"config_file"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	if state.PID == 0 {
		t.Error("state PID is 0")
	}
	if state.Port == 0 {
		t.Error("state Port is 0")
	}
	if state.Host == "" {
		t.Error("state Host is empty")
	}

	// The recorded pid is the DAEMON, not the launcher that started it: `up -d`
	// forks and exits, so the two are never the same process.
	if daemonPID := run.DaemonIdentity().PID; state.PID != daemonPID {
		t.Errorf("state PID %d should be the daemon the harness tracks (%d)", state.PID, daemonPID)
	}

	// Verify PID file exists
	pidPath := filepath.Join(run.StateDir(), "prox.pid")
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("PID file not found: %v", err)
	}

	pidStr := strings.TrimSpace(string(pidData))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("invalid PID in file: %v", err)
	}

	if pid != state.PID {
		t.Errorf("PID mismatch: file has %d, state has %d", pid, state.PID)
	}
}

func TestDaemonMode_RejectsSecondInstance(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, detachedFixtureConfig)

	// Start first daemon
	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)
	waitForStateFile(t, filepath.Join(run.StateDir(), daemonStateFileName), 10*time.Second)

	// Try to start second daemon - should fail
	output, code := f.Run(t, binary, "up", "-d", "-c", f.configPath)

	if code == 0 {
		t.Fatalf("expected second daemon to fail, but it succeeded\noutput: %s", output)
	}

	// Should mention already running
	if !strings.Contains(output, "already running") {
		t.Errorf("expected 'already running' error, got: %s", output)
	}
}

func TestDaemonMode_GracefulShutdown(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, detachedFixtureConfig)

	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)
	statePath := filepath.Join(run.StateDir(), daemonStateFileName)
	waitForStateFile(t, statePath, 10*time.Second)

	// Stop the daemon and WAIT for it to report the outcome. The bare POST this
	// used to send returns as soon as the daemon has been asked, so the file
	// assertions below were racing its cleanup rather than checking it.
	run.Shutdown(t)

	// Wait for shutdown - poll for state file removal
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify state file is cleaned up
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should have been removed after shutdown")
	}

	// Verify PID file is cleaned up
	pidPath := filepath.Join(run.StateDir(), "prox.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should have been removed after shutdown")
	}
}

func TestDaemonMode_DynamicPort(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	// Config WITHOUT api.port - should use dynamic port.
	f := newInlineFixture(t, detachedFixtureConfig)

	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)

	statePath := filepath.Join(run.StateDir(), daemonStateFileName)
	waitForStateFile(t, statePath, 10*time.Second)

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Port should be assigned dynamically (not the default 5555)
	if state.Port == 0 {
		t.Error("expected dynamic port to be assigned")
	}

	// Verify API is accessible on the dynamic port
	waitForAPI(t, run.Addr(), apiReadyTimeout)

	t.Logf("Daemon using dynamic port: %d", state.Port)
}

func TestDaemonMode_ConfiguredPort(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	// Config WITH a specific api.port. The number used to be hardcoded (16666),
	// which is the shared-resource shape the rest of this suite just removed:
	// two runs on one machine, or a developer's own service on that port, and
	// the daemon fails to bind. What is under test is that a CONFIGURED port is
	// honoured, not which number it is, so an ephemeral port reserved and
	// released here is just as good a subject.
	//
	// Releasing the reservation does leave a window in which something else can
	// take the port before the daemon binds it -- the bind-and-close TOCTOU that
	// produced a real failure in this plan's baseline measurement
	// (TestInFlight_EndToEnd, "bind: address already in use"). So, like every
	// other dynamic-port consumer here, this retries on that specific error
	// rather than reporting it as a broken assertion about configured ports.
	var (
		port int
		run  *proxRun
		f    *proxFixture
	)
	for attempt := 1; ; attempt++ {
		var reservation net.Listener
		port, reservation = freePort(t)
		if err := reservation.Close(); err != nil {
			t.Fatalf("release reserved api port %d: %v", port, err)
		}
		f = newInlineFixture(t, detachedFixtureConfig, withAPIPort(port))

		var err error
		run, err = f.TryStartDetached(t, binary, "up", "-d", "-c", f.configPath)
		if err == nil {
			break
		}
		if attempt >= freePortAttempts || !isAddrInUse(err) {
			t.Fatalf("start daemon on configured port %d: %v", port, err)
		}
		t.Logf("configured port %d was taken between reservation and bind; retrying", port)
	}

	statePath := filepath.Join(run.StateDir(), daemonStateFileName)
	waitForStateFile(t, statePath, 10*time.Second)

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Port should be the configured value
	if state.Port != port {
		t.Errorf("expected configured port %d, got %d", port, state.Port)
	}

	// Verify API is accessible on the configured port
	waitForAPI(t, "http://127.0.0.1:"+strconv.Itoa(port), apiReadyTimeout)
}

func TestDaemonMode_CLIAutoDiscovery(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, detachedFixtureConfig)

	run := f.StartDetached(t, binary, "up", "-d", "-c", f.configPath)

	// Wait for API to be ready before running CLI command
	waitForAPI(t, run.Addr(), apiReadyTimeout)

	// Run status command without specifying --addr
	// It should auto-discover the API address from .prox/prox.state
	statusOutput, code := f.Run(t, binary, "status", "-c", f.configPath)
	if code != 0 {
		t.Fatalf("status command failed (exit %d)\noutput: %s", code, statusOutput)
	}

	// Should show running status
	if !strings.Contains(statusOutput, "running") {
		t.Errorf("expected 'running' in status output, got: %s", statusOutput)
	}

	// No cleanup here: the `prox stop` this test used to fire and forget asserted
	// nothing, and the harness now stops the daemon with a waited shutdown.
}
