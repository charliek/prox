package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// logLines fetches the most recent log lines for a process via
// GET /api/v1/logs. It returns nil (rather than failing the test) on a
// transient request/decode error so callers can poll it in a retry loop.
func logLines(t *testing.T, addr, process string) []string {
	t.Helper()

	url := fmt.Sprintf("%s/api/v1/logs?process=%s&lines=1000", addr, process)
	resp, err := http.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var parsed struct {
		Logs []struct {
			Line string `json:"line"`
		} `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil
	}

	lines := make([]string, len(parsed.Logs))
	for i, e := range parsed.Logs {
		lines[i] = e.Line
	}
	return lines
}

// waitForLogContains polls a process's logs until a line contains substr, or
// fails the test after timeout.
func waitForLogContains(t *testing.T, addr, process, substr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = logLines(t, addr, process)
		for _, l := range last {
			if strings.Contains(l, substr) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("process %q logs did not contain %q within %v; last lines: %v", process, substr, timeout, last)
}

// waitForMarkerValue polls a process's logs for a line containing prefix and
// returns the trimmed text following it, skipping any value equal to
// exclude. This is used both to capture a marker (exclude="") and, after a
// restart, to detect a genuinely *new* marker value such as a fresh
// GRANDCHILD_PID (exclude=<old value>). Fails the test if no matching,
// non-excluded marker appears within timeout.
func waitForMarkerValue(t *testing.T, addr, process, prefix, exclude string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range logLines(t, addr, process) {
			_, after, found := strings.Cut(l, prefix)
			if !found {
				continue
			}
			val := strings.TrimSpace(after)
			if val != "" && val != exclude {
				return val
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("marker %q (excluding %q) not found in %q logs within %v", prefix, exclude, process, timeout)
	return ""
}

// processAlive reports whether pid refers to a live process using a signal-0
// liveness probe (mirrors execProcess.GroupAlive's leader-only fallback).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// killIfAlive SIGKILLs pid iff it is still alive. Every stubborn-grandchild
// test below registers this via t.Cleanup for every PID it records, so a
// failed assertion (or, for the restart test, an intentionally-still-running
// replacement grandchild) can never leak an orphaned stubborn_listener.py
// process into CI. Guarded by a signal-0 probe first so it never signals a
// pid that has already been reaped (and, vanishingly rarely, reused by an
// unrelated process).
func killIfAlive(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// waitForPIDGone polls until pid is no longer alive, up to timeout. It returns
// true once the pid is gone (or was never alive) and false if it is still alive
// at the deadline, so each caller can craft its own context-specific failure
// message.
func waitForPIDGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// TestRestart_ReloadsEnvFile (A1): a process echoes a value sourced from its
// env_file in a loop; editing the file and restarting must cause the
// replacement to observe the new value -- proving env_file reload works
// end-to-end through the real binary and API (D1).
func TestRestart_ReloadsEnvFile(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	// The env file lives in its own temp dir -- it isn't the fixture's own
	// config, just a file env_file points at, so there's nothing here for
	// f.Rewrite to own.
	envDir := t.TempDir()
	envFile := filepath.Join(envDir, "worker.env")
	requireNoError(t, os.WriteFile(envFile, []byte("MYVAL=v1\n"), 0644), "writing initial env file")

	cfgContent := fmt.Sprintf(`processes:
  echoenv:
    cmd: 'while :; do echo "MYVAL=$MYVAL"; sleep 0.3; done'
    env_file: %q
`, envFile)
	f := newInlineFixture(t, cfgContent)
	run := f.Start(t, binary, "up", "-c", f.configPath)

	waitForAPI(t, run.Addr(), apiReadyTimeout)

	// Confirm the process is running with the initial value before mutating
	// the env file out from under it.
	waitForLogContains(t, run.Addr(), "echoenv", "MYVAL=v1", 5*time.Second)

	// Mutate the (mutable, t.TempDir-owned) env file -- never a committed
	// fixture -- and restart.
	requireNoError(t, os.WriteFile(envFile, []byte("MYVAL=v2\n"), 0644), "rewriting env file")

	status, errResp := restartProcess(t, run.Addr(), "echoenv")
	if status != http.StatusOK {
		t.Fatalf("restart failed: status=%d code=%s error=%s", status, errResp.Code, errResp.Error)
	}

	// The replacement instance must reload env_file from disk and observe
	// the new value.
	waitForLogContains(t, run.Addr(), "echoenv", "MYVAL=v2", 5*time.Second)
}

// printerConfig renders a config whose single `printer` process echoes
// "MARKER=<marker>" in a loop. No api: block -- the fixture harness drops it
// and allocates a dynamic port (see fixture_test.go). Used by the
// changed-cmd reload test to edit the launched command out from under a
// running process.
func printerConfig(marker string) string {
	return fmt.Sprintf(`processes:
  printer:
    cmd: 'while :; do echo "MARKER=%s"; sleep 0.3; done'
`, marker)
}

// TestRestart_ReloadsChangedCmd (#33): edit the launched command in prox.yaml
// and restart via the API; the replacement must run the NEW command. A second
// edit->restart cycle proves reload is per-request (consecutive reloads each
// pick up the latest file), exercising the real `prox up` + API path end to end.
func TestRestart_ReloadsChangedCmd(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, printerConfig("v1"))
	run := f.Start(t, binary, "up", "-c", f.configPath)

	waitForAPI(t, run.Addr(), apiReadyTimeout)
	waitForLogContains(t, run.Addr(), "printer", "MARKER=v1", 5*time.Second)

	// Two consecutive edit->restart cycles, each picking up the latest file.
	for _, marker := range []string{"v2", "v3"} {
		f.Rewrite(t, printerConfig(marker))

		status, errResp := restartProcess(t, run.Addr(), "printer")
		if status != http.StatusOK {
			t.Fatalf("restart (%s) failed: status=%d code=%s error=%s", marker, status, errResp.Code, errResp.Error)
		}
		waitForLogContains(t, run.Addr(), "printer", "MARKER="+marker, 5*time.Second)
	}
}

// TestRestart_RemovedProcessReturns409 (#33): removing the restart target from
// the config (while another process remains, so validation passes) must make a
// restart of the removed process fail with PROCESS_NOT_IN_CONFIG (HTTP 409) and
// leave the still-configured process running untouched.
func TestRestart_RemovedProcessReturns409(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	twoProcs := `processes:
  alpha:
    cmd: 'while :; do echo "ALPHA"; sleep 0.3; done'
  beta:
    cmd: 'while :; do echo "BETA"; sleep 0.3; done'
`
	f := newInlineFixture(t, twoProcs)
	run := f.Start(t, binary, "up", "-c", f.configPath)

	waitForAPI(t, run.Addr(), apiReadyTimeout)
	waitForLogContains(t, run.Addr(), "alpha", "ALPHA", 5*time.Second)

	// Snapshot alpha's identity so we can prove the failed restart didn't
	// stop-and-relaunch it (same PID, same restart count), not merely that
	// something named alpha ended up running.
	before := waitForProcessState(t, run.Addr(), "alpha", "running", 3*time.Second)

	// Remove alpha from the file (beta remains so the file still validates).
	onlyBeta := `processes:
  beta:
    cmd: 'while :; do echo "BETA"; sleep 0.3; done'
`
	f.Rewrite(t, onlyBeta)

	status, errResp := restartProcess(t, run.Addr(), "alpha")
	if status != http.StatusConflict {
		t.Fatalf("expected 409 restarting a removed process, got status=%d code=%s error=%s", status, errResp.Code, errResp.Error)
	}
	if errResp.Code != "PROCESS_NOT_IN_CONFIG" {
		t.Fatalf("expected code PROCESS_NOT_IN_CONFIG, got %q (error=%q)", errResp.Code, errResp.Error)
	}

	// alpha must still be running, and it must be the SAME instance: identical
	// PID and restart count, not a stop-and-relaunch.
	after := waitForProcessState(t, run.Addr(), "alpha", "running", 3*time.Second)
	if after.PID != before.PID || after.Restarts != before.Restarts {
		t.Fatalf("alpha was disturbed by the failed restart: pid %d->%d restarts %d->%d",
			before.PID, after.PID, before.Restarts, after.Restarts)
	}
}

// TestStop_KillsStubbornGrandchild (A3): the leader exits gracefully on
// SIGTERM but a backgrounded grandchild ignores it and holds a real TCP port.
// `stop` must return only once the grandchild is verified gone (escalating to
// SIGKILL after the graceful window). This test is inherently slow (~the
// graceful deadline, ~8s by default) because that escalation window is real.
func TestStop_KillsStubbornGrandchild(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newFixture(t, "stubborn")
	run := f.Start(t, binary, "up", "-c", f.configPath)

	var grandchildPID int
	t.Cleanup(func() { killIfAlive(grandchildPID) })

	waitForAPI(t, run.Addr(), apiReadyTimeout)

	pidStr := waitForMarkerValue(t, run.Addr(), "worker", "GRANDCHILD_PID=", "", 5*time.Second)
	pid, err := strconv.Atoi(pidStr)
	requireNoError(t, err, "parsing GRANDCHILD_PID")
	grandchildPID = pid

	waitForMarkerValue(t, run.Addr(), "worker", "LISTENING=", "", 5*time.Second)

	start := time.Now()
	status, errResp := stopProcess(t, run.Addr(), "worker")
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("stop failed: status=%d code=%s error=%s (after %v)", status, errResp.Code, errResp.Error, elapsed)
	}
	t.Logf("stop of stubborn worker took %v", elapsed)

	// Stop() only returns after its finalization gate + verdict, so the
	// grandchild should already be gone -- poll with a deadline as a
	// robustness net rather than asserting instantaneously.
	if !waitForPIDGone(grandchildPID, 15*time.Second) {
		t.Fatalf("grandchild pid %d still alive %v after stop returned (stop took %v)", grandchildPID, 15*time.Second, elapsed)
	}
}

// TestRestart_StubbornGrandchildPortRebinds (A4): same stubborn-grandchild
// scenario, but via restart. The old group must be fully reaped before the
// replacement starts, so the replacement's grandchild can rebind the same
// port (no EADDRINUSE) -- proof the old listener genuinely released it since
// SO_REUSEADDR is deliberately off in stubborn_listener.py.
func TestRestart_StubbornGrandchildPortRebinds(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newFixture(t, "stubborn")
	run := f.Start(t, binary, "up", "-c", f.configPath)

	var oldPID, newPID int
	t.Cleanup(func() {
		killIfAlive(newPID)
		killIfAlive(oldPID)
	})

	waitForAPI(t, run.Addr(), apiReadyTimeout)

	oldPIDStr := waitForMarkerValue(t, run.Addr(), "worker", "GRANDCHILD_PID=", "", 5*time.Second)
	oldPID, err := strconv.Atoi(oldPIDStr)
	requireNoError(t, err, "parsing initial GRANDCHILD_PID")
	waitForMarkerValue(t, run.Addr(), "worker", "LISTENING=", "", 5*time.Second)

	start := time.Now()
	status, errResp := restartProcess(t, run.Addr(), "worker")
	elapsed := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("restart failed: status=%d code=%s error=%s (after %v)", status, errResp.Code, errResp.Error, elapsed)
	}
	t.Logf("restart of stubborn worker took %v", elapsed)

	// The old grandchild must be gone -- restart's internal Stop only
	// launches the replacement once the old group was verified reaped.
	if processAlive(oldPID) {
		t.Fatalf("old grandchild pid %d still alive after restart returned", oldPID)
	}

	// A new, distinct grandchild must appear (this marker is only printed
	// after a successful bind+listen, so its mere presence proves the
	// rebind succeeded -- a failed rebind would crash the python script
	// before it ever printed GRANDCHILD_PID/LISTENING).
	newPIDStr := waitForMarkerValue(t, run.Addr(), "worker", "GRANDCHILD_PID=", oldPIDStr, 10*time.Second)
	newPID, err = strconv.Atoi(newPIDStr)
	requireNoError(t, err, "parsing new GRANDCHILD_PID")
	if newPID == oldPID {
		t.Fatalf("expected a new grandchild pid distinct from %d, got the same pid", oldPID)
	}

	listeningPort := waitForMarkerValue(t, run.Addr(), "worker", "LISTENING=", "", 5*time.Second)
	if wantPort := strconv.Itoa(f.StubbornPort()); listeningPort != wantPort {
		t.Fatalf("expected grandchild to listen on port %s, got %q", wantPort, listeningPort)
	}

	// The replacement should have settled into "running" (not crashed on
	// EADDRINUSE).
	waitForProcessState(t, run.Addr(), "worker", "running", 3*time.Second)
}

// TestFullStop_NoOrphanedGrandchild (A5): the same stubborn-grandchild
// scenario via a full-instance `prox stop` (POST /api/v1/shutdown). After the
// daemon process exits, no member of the original group -- in particular the
// grandchild holding the port -- may remain.
func TestFullStop_NoOrphanedGrandchild(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newFixture(t, "stubborn")
	run := f.Start(t, binary, "up", "-c", f.configPath)

	var grandchildPID int
	t.Cleanup(func() { killIfAlive(grandchildPID) })

	waitForAPI(t, run.Addr(), apiReadyTimeout)

	pidStr := waitForMarkerValue(t, run.Addr(), "worker", "GRANDCHILD_PID=", "", 5*time.Second)
	pid, err := strconv.Atoi(pidStr)
	requireNoError(t, err, "parsing GRANDCHILD_PID")
	grandchildPID = pid
	waitForMarkerValue(t, run.Addr(), "worker", "LISTENING=", "", 5*time.Second)

	requireNoError(t, stopProx(t, run.Addr()), "requesting full shutdown")

	// WaitExit blocks until the process exits, or fails the test (killing it
	// first) on timeout -- mirrors the goroutine + select this replaces. The
	// exit error itself is not asserted here, matching the original.
	_ = run.WaitExit(t, 20*time.Second)

	if !waitForPIDGone(grandchildPID, 5*time.Second) {
		t.Fatalf("grandchild pid %d still alive after full shutdown", grandchildPID)
	}
}
