package integration

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type StatusResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ConfigFile    string `json:"config_file,omitempty"`
	APIVersion    string `json:"api_version"`
}

type ProcessInfo struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	PID      int    `json:"pid"`
	Restarts int    `json:"restarts"`
}

type ProcessListResponse struct {
	Processes []ProcessInfo `json:"processes"`
}

func TestUpCommand_StartsProcesses(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	// Wait for API to be ready
	waitForAPI(t, testAPIAddr, 10*time.Second)

	// Give processes time to start
	time.Sleep(500 * time.Millisecond)

	// Verify status endpoint
	resp, err := http.Get(testAPIAddr + "/api/v1/status")
	requireNoError(t, err, "failed to get status")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("failed to decode status: %v", err)
	}

	if status.Status != "running" {
		t.Errorf("expected status 'running', got '%s'", status.Status)
	}
}

func TestUpCommand_ProcessList(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Get process list
	resp, err := http.Get(testAPIAddr + "/api/v1/processes")
	requireNoError(t, err, "failed to get processes")
	defer resp.Body.Close()

	var result ProcessListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode processes: %v", err)
	}

	if len(result.Processes) != 2 {
		t.Errorf("expected 2 processes, got %d", len(result.Processes))
	}

	// Find the long-running process
	found := false
	for _, p := range result.Processes {
		if p.Name == "long" {
			found = true
			if p.Status != "running" {
				t.Errorf("expected long process to be running, got '%s'", p.Status)
			}
		}
	}
	if !found {
		t.Error("long process not found")
	}
}

func TestUpCommand_GracefulShutdown(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Request shutdown via API
	err := stopProx(t, testAPIAddr)
	requireNoError(t, err, "failed to request shutdown")

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited - check that it was graceful
		if err != nil {
			t.Logf("process exited with error (may be expected): %v", err)
		}
	case <-time.After(15 * time.Second):
		killProx(cmd)
		t.Fatal("process did not shut down within timeout")
	}
}

// TestStopCommand_WaitsForCleanExit drives the real `prox stop` CLI against a
// foreground daemon: it must wait for the outcome, exit 0, print a stopped
// summary, and the daemon's state + PID files must be gone afterward (#36, D4).
func TestStopCommand_WaitsForCleanExit(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	out, exitCode := runProx(t, binary, "stop", "-c", configPath("integration"))
	if exitCode != 0 {
		t.Fatalf("prox stop exited %d, want 0\noutput:\n%s", exitCode, out)
	}
	if !strings.Contains(out, "Stopped") {
		t.Errorf("expected a stopped summary, got:\n%s", out)
	}

	// The foreground daemon must have exited cleanly as part of the waited stop.
	if err := waitCmdExit(t, cmd, 10*time.Second); err != nil {
		t.Errorf("foreground prox up should exit 0 on a clean stop, got %v", err)
	}

	// State + PID files must be cleaned up.
	root := projectRoot(t)
	for _, name := range []string{".prox/prox.state", ".prox/prox.pid"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be gone after stop, stat err=%v", name, err)
		}
	}
}

// TestStopCommand_AsyncPostReturnsImmediately confirms the legacy async POST
// /shutdown (no wait param) still returns an immediate 200 while the daemon tears
// down in the background.
func TestStopCommand_AsyncPostReturnsImmediately(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	start := time.Now()
	if err := stopProx(t, testAPIAddr); err != nil {
		t.Fatalf("async shutdown POST failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("async POST /shutdown should return promptly, took %v", elapsed)
	}

	if err := waitCmdExit(t, cmd, 15*time.Second); err != nil {
		t.Errorf("foreground prox up should exit 0 after an async clean stop, got %v", err)
	}
}

// TestStopCommand_DoubleStopNoPanic runs `prox stop` twice against the same
// daemon. The daemon must not panic (regression for the double-close bug), and
// the second invocation must exit sanely (a waited result, a connection-refused
// unknown-outcome path, or a not-running message) rather than crashing.
func TestStopCommand_DoubleStopNoPanic(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	prox := startProxWithOutput(t, binary, "up", "-c", configPath("integration"))
	defer killProx(prox.cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Fire the first stop in the background; it waits for the drain. Capture its
	// output and exit code — the first stop should observe the clean verdict.
	var out1 string
	var exit1 int
	firstDone := make(chan struct{})
	go func() {
		out1, exit1 = runProx(t, binary, "stop", "-c", configPath("integration"))
		close(firstDone)
	}()

	// A moment later, fire a second stop that races/follows the first.
	time.Sleep(200 * time.Millisecond)
	out2, exit2 := runProx(t, binary, "stop", "-c", configPath("integration"))

	select {
	case <-firstDone:
	case <-time.After(20 * time.Second):
		t.Fatal("first prox stop did not finish")
	}

	// The daemon does a clean stop (reapable processes), so the foreground exits 0
	// regardless of how many stop clients connected.
	if err := waitCmdExit(t, prox.cmd, 15*time.Second); err != nil {
		t.Errorf("daemon should exit 0 on a clean double stop, got %v", err)
	}

	// The daemon must not have panicked.
	if daemonOut := prox.Output(); strings.Contains(daemonOut, "panic:") {
		t.Errorf("daemon panicked during double stop:\n%s", daemonOut)
	}

	// The first stop reached the live daemon and should have seen the clean verdict:
	// exit 0 with a stopped summary.
	if exit1 != 0 {
		t.Errorf("first prox stop exited %d, want 0\noutput:\n%s", exit1, out1)
	}
	if !strings.Contains(out1, "Stopped") {
		t.Errorf("first prox stop should print a stopped summary, got:\n%s", out1)
	}

	// The second stop races the shutdown: depending on the window it either joins
	// the latched clean verdict (exit 0, stopped summary), hits a
	// connection-refused / unknown-outcome path (exit 1), or finds the daemon
	// already gone (a not-running style message). Assert it landed on one of those
	// recognized, non-panicking outcomes rather than only checking for panic text.
	if strings.Contains(out2, "panic:") {
		t.Errorf("second prox stop panicked:\n%s", out2)
	}
	if exit2 != 0 && exit2 != 1 {
		t.Errorf("second prox stop exited %d, want 0 or 1\noutput:\n%s", exit2, out2)
	}
	recognized := strings.Contains(out2, "Stopped") ||
		strings.Contains(out2, "Shutdown initiated") ||
		strings.Contains(out2, "outcome unknown") ||
		strings.Contains(out2, "not running") ||
		strings.Contains(out2, "connection refused")
	if !recognized {
		t.Errorf("second prox stop produced an unrecognized outcome (exit %d):\n%s", exit2, out2)
	}
}

func TestUpCommand_SpecificProcesses(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	// Start only the 'long' process
	cmd := startProx(t, binary, "up", "-c", configPath("integration"), "long")
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	// Get process list
	resp, err := http.Get(testAPIAddr + "/api/v1/processes")
	requireNoError(t, err, "failed to get processes")
	defer resp.Body.Close()

	var result ProcessListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode processes: %v", err)
	}

	// Should only have 1 process running
	runningCount := 0
	for _, p := range result.Processes {
		if p.Status == "running" {
			runningCount++
			if p.Name != "long" {
				t.Errorf("unexpected running process: %s", p.Name)
			}
		}
	}
	if runningCount != 1 {
		t.Errorf("expected 1 running process, got %d", runningCount)
	}
}

// TestUpCommand_GrandchildOutputCapture verifies that output from grandchild
// processes (like Python spawned via shell) is captured during graceful shutdown.
// This is the key feature that manual pipes (vs cmd.StdoutPipe) enables.
func TestUpCommand_GrandchildOutputCapture(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	// Use a different API address for this test to avoid port conflicts
	grandchildAPIAddr := "http://127.0.0.1:15556"

	prox := startProxWithOutput(t, binary, "up", "-c", configPath("grandchild"))
	defer killProx(prox.cmd)

	// Wait for API to be ready
	waitForAPI(t, grandchildAPIAddr, 10*time.Second)

	// Wait until the grandchild's startup marker is visible in the SAME
	// captured-output surface the post-exit assertions read (the terminal
	// echo subscription starts after process launch, so the logs API is not
	// an equivalent surface). This 15s startup deadline is independent of
	// the 15s shutdown wait below.
	waitForOutputContains(t, prox, "PROCESS_STARTED_PID=", 15*time.Second)

	// Request graceful shutdown via API
	err := stopProx(t, grandchildAPIAddr)
	requireNoError(t, err, "failed to request shutdown")

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- prox.cmd.Wait()
	}()

	select {
	case <-done:
		// Process exited
	case <-time.After(15 * time.Second):
		killProx(prox.cmd)
		t.Fatal("process did not shut down within timeout")
	}

	// Verify the output contains the grandchild's shutdown messages
	output := prox.Output()

	// The Python script prints these distinctive markers during shutdown
	expectedMarkers := []string{
		"PROCESS_STARTED_PID=",
		"GRACEFUL_SHUTDOWN_START",
		"GRACEFUL_SHUTDOWN_COMPLETE",
	}

	for _, marker := range expectedMarkers {
		if !strings.Contains(output, marker) {
			t.Errorf("expected output to contain %q, but it didn't.\nOutput:\n%s", marker, output)
		}
	}
}

// TestUpDetach_EarlyDeathReportsFailure exercises the real D2 parent
// wait-and-report path end to end: `prox up -d` with an unreadable config makes
// the detached child exit before it becomes ready, so the parent must block,
// detect the early death, print a failure with the child's log tail, and exit
// non-zero — no lingering daemon. This is the cheap, self-cleaning half of D2
// (the never-ready/timeout half is covered by unit-level fakes with injectable
// timings in internal/cli).
func TestUpDetach_EarlyDeathReportsFailure(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	// Isolated working dir so the child's .prox state never touches the repo
	// root and is auto-removed with the temp dir.
	dir := t.TempDir()
	badCfg := filepath.Join(dir, "does-not-exist.yaml")

	cmd := exec.Command(binary, "up", "-d", "-c", badCfg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected a non-zero exit, got err=%v\nOutput:\n%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d\nOutput:\n%s", ee.ExitCode(), out)
	}
	if !strings.Contains(string(out), "failed to start") {
		t.Errorf("expected failure diagnostics mentioning 'failed to start', got:\n%s", out)
	}
}

// TestUpTUI_NonInteractiveRefusesToStart pins the --tui non-TTY guard (plan 018
// C2). A piped invocation cannot drive a full-screen TUI, so `prox up --tui` must
// refuse BEFORE anything starts: non-zero exit, the guard's message, and — the
// part that matters — no process launched and no .prox state left behind. Run
// under `go test` the child's stdout/stderr are pipes, which is exactly the
// non-interactive shape a CI runner or `| tee` produces.
func TestUpTUI_NonInteractiveRefusesToStart(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	// Isolated working dir: nothing here touches the repo root's .prox, and the
	// process the config would have launched leaves a marker file we can look for.
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched.marker")
	cfg := filepath.Join(dir, "prox.yaml")
	cfgBody := "api:\n  host: 127.0.0.1\n\nprocesses:\n  marker:\n    cmd: touch " + marker + " && sleep 30\n"
	if err := os.WriteFile(cfg, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()

	// The guard runs before any startup work, so this is near-instant; the budget
	// is generous only to absorb a loaded CI machine.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		killProx(cmd)
		t.Fatal("prox up --tui did not exit; the non-TTY guard should refuse immediately")
	}

	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected a non-zero exit, got err=%v\nOutput:\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "--tui requires an interactive terminal") {
		t.Errorf("expected the interactive-terminal guard message, got:\n%s", out)
	}

	// Nothing may have started: no marker from the configured process, and no
	// state directory (the guard fires before EnsureStateDir).
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("--tui guard must refuse before starting processes, but the marker exists (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".prox")); !os.IsNotExist(err) {
		t.Errorf("--tui guard must refuse before any state is written, but .prox exists (stat err=%v)", err)
	}
}
