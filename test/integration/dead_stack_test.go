package integration

import (
	"strings"
	"testing"
	"time"
)

// This file covers the dead-stack exit end to end (plan 028 C3, #96).
//
// A foreground `prox up` used to wait forever on a shutdown trigger that only a
// signal, POST /shutdown or a TUI quit ever closes. A typo in `cmd:` — the most
// common first-run mistake there is — therefore left the user holding a
// terminal that supervised nothing, silently, until they thought to press
// Ctrl-C. The unit tests in internal/cli pin the DECISION (dead_stack_test.go);
// these pin the WIRING against the real binary, including the two things that
// most matter about a teardown rule: that it does NOT fire on a partial crash,
// and that it does NOT exist in the detached daemon child.
//
// Everything here runs in plain (non-TUI) mode by construction: under `go test`
// the child's stdout is a pipe, so TUI resolution degrades to plain log
// streaming, which is the mode this feature belongs to. The TUI's own answer to
// a dead stack — a persistent banner, and no auto-exit — is a separate commit.

// deadStackBadCmd is a command that cannot be exec'd at all, so the supervisor
// marks the process crashed the instant it tries to start it: a typo in `cmd:`,
// reached deterministically and with no sleep for a slow runner to race.
const deadStackBadCmd = "/no/such/binary-xyz"

// deadStackSettleGrace is comfortably longer than the watcher's 500ms
// confirmation window (internal/cli/process_settle.go), so "it had not exited"
// means the rule did not fire rather than that it had not got round to it yet.
const deadStackSettleGrace = 3 * time.Second

// TestDeadStack_ForegroundUpExitsWhenEveryProcessIsDead is the #96 regression
// test: the whole stack dies, so the session ends by itself, non-zero, saying
// what died.
//
// It is written as WaitExit rather than a poll for a reason: against the
// unfixed code `prox up` never exits at all, so this test hangs until the
// budget kills it — which is exactly the user-visible bug, reproduced.
func TestDeadStack_ForegroundUpExitsWhenEveryProcessIsDead(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, `
processes:
  ghost:
    cmd: "`+deadStackBadCmd+`"
`)

	run := f.Start(t, binary, "up", "--no-proxy", "-c", f.configPath)

	if err := run.WaitExit(t, within(t, processExitTimeout)); err == nil {
		t.Fatalf("`prox up` exited 0 with every process dead; output:\n%s", run.Output())
	}
	if code := run.ExitCode(t); code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, run.Output())
	}

	out := run.Output()
	for _, want := range []string{
		"Crashed: ghost",                // named, in `prox status`'s own words
		"prox logs ghost",               // where the reason is
		"No processes are left running", // why the terminal came back
		"1 process(es) crashed",         /* the same sentinel every other command returns */
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

// TestDeadStack_PartialCrashKeepsRunning is the half that decides whether this
// feature is safe to ship: one process of two dies and the other keeps serving,
// so the session must NOT be torn down. Tearing a developer's whole stack down
// because one process crashed would be a far worse bug than the one #96 fixes.
//
// It also pins the other end of the contract: an INTENTIONAL shutdown of a
// session that contains a crashed process still exits 0, because the watcher
// latches nothing when somebody else requested the shutdown.
func TestDeadStack_PartialCrashKeepsRunning(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, `
processes:
  ghost:
    cmd: "`+deadStackBadCmd+`"
  keeper:
    cmd: "sleep 300"
`)

	run := f.Start(t, binary, "up", "--no-proxy", "-c", f.configPath)
	waitForAPI(t, run.Addr(), within(t, apiReadyTimeout))

	// Prove the interesting state really occurred — one crashed, one alive —
	// so this cannot pass vacuously against a config where nothing failed.
	waitForProcessState(t, run.Addr(), "ghost", "crashed", within(t, processStateTimeout))
	waitForProcessState(t, run.Addr(), "keeper", "running", within(t, processStateTimeout))

	if run.awaitExit(deadStackSettleGrace) {
		t.Fatalf("`prox up` tore the session down over a PARTIAL crash; output:\n%s", run.Output())
	}
	// Still serving, not merely still resident.
	waitForProcessState(t, run.Addr(), "keeper", "running", within(t, processStateTimeout))

	// Now end it the way the user would. keeper stops cleanly and the crash was
	// never this shutdown's doing, so the command exits 0.
	if err := stopProx(t, run.Addr()); err != nil {
		t.Fatalf("shutdown request failed: %v\noutput:\n%s", err, run.Output())
	}
	if err := run.WaitExit(t, within(t, processExitTimeout)); err != nil {
		t.Fatalf("an intentional shutdown exited non-zero (%v); output:\n%s", err, run.Output())
	}
}

// TestDeadStack_DetachedDaemonSurvivesAnAllCrashedConfig is the anti-regression
// test for the gating rule.
//
// `--detach` short-circuits TUI resolution to plain mode, so the detached
// daemon child runs the very same wait block a foreground `prox up` does. A
// dead-stack watcher there would kill the daemon moments after `prox up -d`
// printed "The daemon is still running; stop it with 'prox down'", taking the
// API and the crash logs the user needs with it. `prox up -d` already reports
// the crash at settle time and exits non-zero; the daemon staying up is the
// contract.
func TestDeadStack_DetachedDaemonSurvivesAnAllCrashedConfig(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, `
processes:
  ghost:
    cmd: "`+deadStackBadCmd+`"
`)

	// TryStartDetached, not StartDetached: a non-zero `up -d` is CORRECT here
	// (the settle check reports the crash), and the handle it returns still owns
	// the teardown of the daemon that is deliberately left running.
	run, err := f.TryStartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	if err == nil {
		t.Fatalf("`prox up -d` exited 0 with a crashed process; output:\n%s", run.Output())
	}
	if out := run.Output(); !strings.Contains(out, "prox down") {
		t.Errorf("`up -d` did not point at 'prox down'; got:\n%s", out)
	}

	// Well past the watcher's window: a daemon child that ran one would already
	// have shut itself down.
	time.Sleep(deadStackSettleGrace)

	// The daemon is still there and still answering.
	waitForAPI(t, run.Addr(), within(t, apiReadyTimeout))
	statusOut, statusCode := f.Run(t, binary, "status", "-c", f.configPath)
	if !strings.Contains(statusOut, "crashed") {
		t.Errorf("`prox status` did not report the crashed process; exit=%d output:\n%s", statusCode, statusOut)
	}

	// And `prox down`, the remedy `up -d` advertised, has something to stop.
	downOut, downCode := f.Run(t, binary, "down", "-c", f.configPath)
	if downCode != 0 {
		t.Fatalf("`prox down` exit = %d, want 0; output:\n%s", downCode, downOut)
	}
}
