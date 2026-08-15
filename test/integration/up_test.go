package integration

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// "no running prox instance" is the outcome when the second stop lands
	// after the daemon finished tearing down and removed .prox/prox.state.
	// Before plan 020 C3 this window produced "connection refused" instead:
	// discovery fell back to the config file's pinned api.port and dialed a
	// dead address. With that fallback gone (the state file is the single
	// discovery source), the same window is now reported as what it actually
	// is. Both are correct "the daemon is already gone" outcomes.
	recognized := strings.Contains(out2, "Stopped") ||
		strings.Contains(out2, "Shutdown initiated") ||
		strings.Contains(out2, "outcome unknown") ||
		strings.Contains(out2, "not running") ||
		strings.Contains(out2, "no running prox instance") ||
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

// TestUpCommand_InstantCrashLogsAlwaysVisible is a regression test for F2 (plan
// 020 C1): a process that crashes immediately after `prox up` starts it (a
// typo'd cmd, a missing binary) must have its crash reason land in the
// terminal output EVERY time, not intermittently.
//
// Before the fix, the terminal log printer subscribed to the log manager
// inside a goroutine started well after sup.Start()/StartProcesses()
// (`go printLogs(logMgr)`, with printLogs calling Subscribe as its first
// line) -- racing the crashed process's log line. The process here (a bad
// binary run via `sh -c`) crashes almost instantly and is never restarted (a
// crashed process just sits crashed, there is no backoff loop that would
// eventually paper over a lost first line), so whether the line was lost came
// down to goroutine-scheduling luck at that single moment: roughly half the
// time the subscription did not exist yet and the line was silently dropped
// from the terminal (though still recoverable via `prox logs`).
//
// This loops N>=20 real, separate `prox up` invocations -- the race is a
// once-per-invocation window, not something repeated restarts inside one
// process would exercise -- and asserts the crash reason appears in EVERY
// one. A single-shot version of this test passes against the old code
// roughly half the time and proves nothing (verified manually: see the C1
// commit report).
func TestUpCommand_InstantCrashLogsAlwaysVisible(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	const iterations = 20
	const addr = "http://127.0.0.1:15558"
	const marker = "exited unexpectedly"

	for i := range iterations {
		prox := startProxWithOutput(t, binary, "up", "-c", configPath("instant_crash"))

		// Backstop the orderly shutdown below: startProxWithOutput registers no
		// cleanup of its own, and waitForAPI/t.Fatalf can abandon the loop before
		// stopProx runs, stranding a child that holds port 15558 and poisons every
		// later test (codex review finding). Kill is a no-op once the process has
		// already exited normally.
		t.Cleanup(func() {
			if prox.cmd.Process != nil {
				_ = prox.cmd.Process.Kill()
			}
		})

		// Confirms the daemon itself came up; the process crash happens
		// concurrently with (just after) supervisor start, so this does not run
		// past the window we're testing.
		waitForAPI(t, addr, 10*time.Second)

		// Give the crashed process's log line a short, bounded window to reach
		// the terminal. Bounded deliberately short: ghost is never restarted, so
		// nothing will ever produce the line later -- if it isn't here within the
		// window, it was lost.
		deadline := time.Now().Add(3 * time.Second)
		out := prox.Output()
		for time.Now().Before(deadline) && !strings.Contains(out, marker) {
			time.Sleep(20 * time.Millisecond)
			out = prox.Output()
		}

		// Shut down before asserting, so a failed iteration doesn't leak the
		// daemon (and its port) into the next one.
		_ = stopProx(t, addr)
		if err := waitCmdExit(t, prox.cmd, 15*time.Second); err != nil {
			t.Logf("iteration %d: prox up did not exit cleanly: %v", i, err)
		}

		if !strings.Contains(out, marker) || !strings.Contains(out, "ghost") {
			t.Fatalf("iteration %d: crash reason missing from terminal output; wanted \"ghost\" + %q, got:\n%s", i, marker, out)
		}
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

// --- plan 026 C3: TUI mode resolution, through the real CLI ------------------
//
// resolveTUIMode is a pure function with an exhaustive unit matrix
// (internal/cli/tui_mode_test.go). The tests below cover the two things that
// matrix structurally cannot reach:
//
//   - cobra wiring — that --no-tui is registered at all, that Changed("tui") is
//     read where "was it typed" belongs and the flag value where the value
//     belongs. Getting those two backwards is the regression this whole design
//     guards against, and it looks identical to a correct implementation from
//     inside a unit test.
//   - package-global flag bleed — useTUI/noTUI/detach are package vars, so two
//     cobra runs in one process share them. Only a subprocess per invocation
//     proves each command line on its own.
//
// NOTE: there is deliberately no pty-based `prox up -d` test here. It belongs
// in C7. The `-d` conflict regression can only manifest when resolution returns
// something other than plain, which requires AutoDefault: true — and C3 wires
// AutoDefault: false, so on a pty `up -d` resolves plain and such a test would
// pass *against* the bug rather than catching it.

// upTUIFixture writes a config with a single long-lived process into a fresh
// temp dir and returns (dir, configPath). Loopback api.host keeps the daemon
// auth-disabled so the shutdown call below needs no token.
func upTUIFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "prox.yaml")
	body := "api:\n  host: 127.0.0.1\n\nprocesses:\n  worker:\n    cmd: sleep 300\n"
	if err := os.WriteFile(cfg, []byte(body), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return dir, cfg
}

// shutdownDaemonIn stops a detached daemon started in dir by reading the port
// it recorded in .prox/prox.state and posting /shutdown. Best-effort cleanup:
// the assertions live in the tests, not here.
func shutdownDaemonIn(t *testing.T, dir string) {
	t.Helper()
	statePath := filepath.Join(dir, ".prox", "prox.state")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Logf("cleanup: no state file at %s: %v", statePath, err)
		return
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Logf("cleanup: unparseable state file: %v", err)
		return
	}
	addr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	req, _ := http.NewRequest("POST", addr+"/api/v1/shutdown", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil && resp != nil {
		resp.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)
}

// TestUpTUIFlags_Conflicts pins the two flag combinations that must be refused
// and the false-valued forms that must NOT be, through real process
// invocations. `-d --tui=false` is the load-bearing row: cobra reports
// Changed("tui") for it, so a conflict predicate that forgets to also check the
// parsed value breaks a command line that is valid today.
func TestUpTUIFlags_Conflicts(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	t.Run("--tui --no-tui is refused", func(t *testing.T) {
		dir, cfg := upTUIFixture(t)
		cmd := exec.Command(binary, "up", "--tui", "--no-tui", "-c", cfg)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()

		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("expected a non-zero exit, got err=%v\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "--tui and --no-tui are mutually exclusive") {
			t.Errorf("expected the --tui/--no-tui conflict message, got:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".prox")); !os.IsNotExist(err) {
			t.Errorf("the conflict must be reported before any state is written, but .prox exists (stat err=%v)", err)
		}
	})

	t.Run("-d --tui is refused", func(t *testing.T) {
		dir, cfg := upTUIFixture(t)
		cmd := exec.Command(binary, "up", "-d", "--tui", "-c", cfg)
		cmd.Dir = dir
		// No cleanup needed: the conflict is reported before daemonization, so
		// there is no child to stop and no state file to find.
		out, err := cmd.CombinedOutput()

		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("expected a non-zero exit, got err=%v\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "--tui and --detach are mutually exclusive") {
			t.Errorf("expected the --tui/--detach conflict message, got:\n%s", out)
		}
	})

	t.Run("-d --tui=false is accepted", func(t *testing.T) {
		dir, cfg := upTUIFixture(t)
		cmd := exec.Command(binary, "up", "-d", "--tui=false", "-c", cfg)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		defer shutdownDaemonIn(t, dir)

		if err != nil {
			t.Fatalf("`up -d --tui=false` must succeed (Changed(\"tui\") is true for it, but no TUI was asserted): %v\noutput:\n%s", err, out)
		}
		if strings.Contains(string(out), "mutually exclusive") {
			t.Errorf("no conflict may be reported for --tui=false, got:\n%s", out)
		}
		if !strings.Contains(string(out), "prox started (pid") {
			t.Errorf("expected the daemon readiness line, got:\n%s", out)
		}
	})

	t.Run("-d --no-tui is accepted", func(t *testing.T) {
		dir, cfg := upTUIFixture(t)
		cmd := exec.Command(binary, "up", "-d", "--no-tui", "-c", cfg)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		defer shutdownDaemonIn(t, dir)

		if err != nil {
			t.Fatalf("`up -d --no-tui` must succeed: %v\noutput:\n%s", err, out)
		}
		if !strings.Contains(string(out), "prox started (pid") {
			t.Errorf("expected the daemon readiness line, got:\n%s", out)
		}
	})

	t.Run("plain -d still succeeds", func(t *testing.T) {
		dir, cfg := upTUIFixture(t)
		cmd := exec.Command(binary, "up", "-d", "-c", cfg)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		defer shutdownDaemonIn(t, dir)

		if err != nil {
			t.Fatalf("`up -d` must succeed with no flag-conflict error: %v\noutput:\n%s", err, out)
		}
		if strings.Contains(string(out), "mutually exclusive") {
			t.Errorf("a bare `up -d` must never report a flag conflict, got:\n%s", out)
		}
		if !strings.Contains(string(out), "prox started (pid") {
			t.Errorf("expected the daemon readiness line, got:\n%s", out)
		}
	})
}

// TestUpNoTUI_ForegroundStreamsPlainLogs proves `--no-tui` is a registered flag
// that a foreground `prox up` accepts and then behaves exactly as it does
// without it. Under `go test` stdio is pipes, so this run is non-interactive by
// construction and resolution would yield plain anyway -- the point here is the
// cobra registration and that the flag is not rejected, which an unregistered
// flag would fail instantly and loudly.
func TestUpNoTUI_ForegroundStreamsPlainLogs(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir, cfg := upTUIFixture(t)

	cmd := exec.Command(binary, "up", "--no-tui", "-c", cfg)
	cmd.Dir = dir
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}
	defer killProx(cmd)

	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(buf.String(), "API server: ") {
		if time.Now().After(deadline) {
			t.Fatalf("`up --no-tui` did not reach the startup preamble; output:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Contains(buf.String(), "unknown flag") {
		t.Fatalf("--no-tui must be a registered flag; output:\n%s", buf.String())
	}
}

// TestUpTUIEnv_UnrecognizedValueWarns pins the PROX_TUI failure mode a user is
// most likely to hit: a typo'd value is not silently obeyed and not silently
// ignored. It warns, names the offending value, and the run continues normally.
// (C4 moves this warning onto the TUI-visible path; the wording assertion here
// is deliberately loose so that move does not have to touch this test.)
func TestUpTUIEnv_UnrecognizedValueWarns(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir, cfg := upTUIFixture(t)

	cmd := exec.Command(binary, "up", "-c", cfg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PROX_TUI=banana")
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}
	defer killProx(cmd)

	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(buf.String(), "API server: ") {
		if time.Now().After(deadline) {
			t.Fatalf("`up` with a bad PROX_TUI did not start; output:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "PROX_TUI") || !strings.Contains(buf.String(), "banana") {
		t.Errorf("expected a warning naming PROX_TUI and the rejected value, got:\n%s", buf.String())
	}
}

// TestUpTUIEnv_RecognizedValueIsSilent is the other half: PROX_TUI=1 under
// pipes must fall back to plain streaming SILENTLY. A standing shell preference
// is not a per-invocation assertion, so it must neither error (that would
// booby-trap every piped `prox up` in that shell) nor print a note (that would
// pollute CI output forever for a mode change nobody asked about).
func TestUpTUIEnv_RecognizedValueIsSilent(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir, cfg := upTUIFixture(t)

	cmd := exec.Command(binary, "up", "-c", cfg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PROX_TUI=1")
	buf := &syncBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}
	defer killProx(cmd)

	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(buf.String(), "API server: ") {
		if time.Now().After(deadline) {
			t.Fatalf("`PROX_TUI=1 up` under pipes must stream plain logs, not fail; output:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Contains(buf.String(), "PROX_TUI") {
		t.Errorf("a recognized PROX_TUI value must fall back silently, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "requires an interactive terminal") {
		t.Errorf("PROX_TUI=1 is a preference, not an assertion; it must never hard-error, got:\n%s", buf.String())
	}
}
