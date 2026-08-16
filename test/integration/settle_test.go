package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the post-start settle check end to end (plan 027 C13, #94):
// `prox up -d`, `prox start` and `prox restart` must report the resulting STATE
// of the processes they touched, not merely that the daemon accepted the
// request.
//
// The unit tests in internal/cli pin the DECISION (which states count as a
// terminal failure, over the whole domain.ProcessState enum) against fakes.
// These pin the WIRING against the real binary: that a process which execs and
// dies really does turn each of those three commands red, that the message
// names the process and offers the next step, and — the half that is easy to
// forget — that the ordinary, benign states do NOT.

// settleCrashedProcess is the failure this whole feature exists for: a command
// that launches (sh always launches) and then immediately dies. Distinct from a
// process prox cannot launch at all, which was already reported.
const settleCrashedProcess = "/no/such/binary-xyz"

// TestSettle_UpDetachCrashedProcessExitsNonZero: `prox up -d` for a project
// whose process dies on launch exits non-zero, names the process with the same
// sentence `prox status` uses, and points at `prox down` — because the daemon
// IS up, and the user needs to know that a non-zero exit here did not mean
// "nothing started".
//
// Before #94 this exited 0 and printed "prox started", while `prox status` a
// second later said crashed.
func TestSettle_UpDetachCrashedProcessExitsNonZero(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, fmt.Sprintf(`
processes:
  ghost:
    cmd: "%s"
  keeper:
    cmd: "sleep 300"
`, settleCrashedProcess))

	// TryStartDetached rather than StartDetached: a non-zero `up -d` is the
	// SUBJECT here, not a broken launch, and the handle it returns still owns
	// the teardown of the daemon that is (correctly) left running.
	run, err := f.TryStartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	if err == nil {
		t.Fatalf("`prox up -d` exited 0 with a crashed process; output:\n%s", run.Output())
	}
	if code := run.ExitCode(t); code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, run.Output())
	}

	out := run.Output()
	for _, want := range []string{
		"Crashed: ghost",        // names the process, in status's own words
		"prox logs ghost",       // where the reason is
		"prox down",             // the daemon is up; this is how to stop it
		"1 process(es) crashed", /* the same sentinel `prox status` returns */
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	// The success headline must be absent, not merely followed by a failure:
	// printing "prox started" and then exiting 1 is the same lie relocated.
	if strings.Contains(out, "prox started") {
		t.Errorf("a non-zero start printed the success headline; got:\n%s", out)
	}

	// The daemon really is still up — the exit code above is a statement about
	// the processes, and the user's `prox down` has something to stop.
	waitForAPI(t, run.Addr(), within(t, apiReadyTimeout))
	waitForProcessState(t, run.Addr(), "keeper", "running", within(t, processStateTimeout))
}

// TestSettle_UpDetachBenignStatesExitZero is the other half of the contract,
// and the one that decides whether this feature is usable at all: a healthy
// project must still exit 0.
//
// It deliberately mixes the states that are easy to mistake for failures — a
// process still coming up, and a task that has run to COMPLETION (completed is
// terminal, but it is terminal SUCCESS; a predicate built on
// ProcessState.IsStopped would have failed here).
func TestSettle_UpDetachBenignStatesExitZero(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, `
processes:
  web:
    cmd: "sleep 300"
  slow:
    cmd: "sh -c 'sleep 2; exec sleep 300'"
tasks:
  migrate:
    cmd: "true"
    timeout: 30s
`)

	// StartDetached fails the test on any non-zero exit, which is exactly the
	// assertion this test wants.
	run := f.StartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	if out := run.Output(); !strings.Contains(out, "prox started") {
		t.Errorf("a healthy start did not print the success headline; got:\n%s", out)
	}

	// Prove the interesting states really occurred, so this test cannot pass
	// vacuously against a config where everything just ran.
	waitForProcessState(t, run.Addr(), "migrate", "completed", within(t, processStateTimeout))
	waitForProcessState(t, run.Addr(), "slow", "running", within(t, processStateTimeout))
}

// TestSettle_UpDetachWaitingProcessExitsZero: a gated process whose dependency
// has not resolved yet is in `waiting` — limbo, not failure. It is still
// scheduled to launch, so `prox up -d` must return promptly and exit 0 rather
// than treating "not running yet" as "failed".
//
// (`blocked`, waiting's terminal sibling, is near-unobservable from `up -d`: a
// dependency has to exhaust a budget measured in tens of seconds before a
// gated process gets there, so a 500ms window will essentially never see it.
// It is covered by the unit tests and reachable from `prox start` against an
// already-failed dependency.)
func TestSettle_UpDetachWaitingProcessExitsZero(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	// A marker that is never created, so the dependency never becomes ready and
	// `gated` stays waiting for the whole test.
	marker := filepath.Join(t.TempDir(), "never-created")
	f := newInlineFixture(t, fmt.Sprintf(`
processes:
  gated:
    cmd: "sleep 300"
    depends_on: [pg]
dependencies:
  pg:
    check:
      cmd: "test -f %s"
      timeout: 60s
      interval: 200ms
    on_failure: fail
`, marker))

	run := f.StartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	if out := run.Output(); !strings.Contains(out, "prox started") {
		t.Errorf("a waiting process turned a start into a failure; got:\n%s", out)
	}
	waitForProcessState(t, run.Addr(), "gated", "waiting", within(t, processStateTimeout))
}

// TestSettle_StartProcessReportsTheResultingState covers `prox start` on both
// sides: a process that stays up exits 0, and one that dies right after launch
// exits non-zero and says so.
//
// The subject is specifically "launched, then died". A process prox cannot
// launch at all already failed the request itself; what `prox start` used to
// report as success was exec.Start returning, which says nothing about whether
// the thing is alive a moment later.
func TestSettle_StartProcessReportsTheResultingState(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f, crashMarker := newFlakyFixture(t)
	run := f.StartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	waitForProcessState(t, run.Addr(), "flaky", "running", within(t, processStateTimeout))

	// Benign: stop a healthy process and start it again. It survives the settle
	// window, so the command reports success and exits 0.
	if out, code := f.Run(t, binary, "stop", "steady", "-c", f.configPath); code != 0 {
		t.Fatalf("`prox stop steady` exit = %d; output:\n%s", code, out)
	}
	out, code := f.Run(t, binary, "start", "steady", "-c", f.configPath)
	if code != 0 {
		t.Fatalf("`prox start steady` exit = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Started process: steady") {
		t.Errorf("a successful start did not print its headline; got:\n%s", out)
	}

	// Arm the crash, then stop and start `flaky`: it now exits immediately.
	armCrash(t, crashMarker)
	if out, code := f.Run(t, binary, "stop", "flaky", "-c", f.configPath); code != 0 {
		t.Fatalf("`prox stop flaky` exit = %d; output:\n%s", code, out)
	}
	out, code = f.Run(t, binary, "start", "flaky", "-c", f.configPath)
	if code == 0 {
		t.Fatalf("`prox start flaky` exited 0 for a process that died on launch; output:\n%s", out)
	}
	assertCrashReported(t, out, "flaky", "Started process: flaky")
}

// TestSettle_RestartReportsTheResultingState is TestSettle_StartProcess... for
// `prox restart`, which had the identical defect: the daemon acked as soon as
// the relaunch had been issued, so restarting a process into a config that no
// longer works reported success.
func TestSettle_RestartReportsTheResultingState(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f, crashMarker := newFlakyFixture(t)
	run := f.StartDetached(t, binary, "up", "-d", "--no-proxy", "-c", f.configPath)
	waitForProcessState(t, run.Addr(), "flaky", "running", within(t, processStateTimeout))

	// Benign restart first: nothing is armed, so the process comes back up and
	// the command exits 0.
	out, code := f.Run(t, binary, "restart", "flaky", "-c", f.configPath)
	if code != 0 {
		t.Fatalf("`prox restart flaky` exit = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Restarted process: flaky") {
		t.Errorf("a successful restart did not print its headline; got:\n%s", out)
	}

	// Now arm the crash and restart again: same command, same process, opposite
	// verdict — which is only possible because the exit code follows the state
	// rather than the request.
	armCrash(t, crashMarker)
	out, code = f.Run(t, binary, "restart", "flaky", "-c", f.configPath)
	if code == 0 {
		t.Fatalf("`prox restart flaky` exited 0 for a process that died on launch; output:\n%s", out)
	}
	assertCrashReported(t, out, "flaky", "Restarted process: flaky")
}

// newFlakyFixture builds a project with one process whose behavior the test
// controls at runtime: `flaky` runs forever until a marker file exists, and
// exits immediately once it does.
//
// A process that is switched from healthy to crashing WITHOUT touching the
// config is what makes the start/restart tests honest: the same command against
// the same process must exit 0 before the marker and non-zero after it, so the
// exit code can only be following the resulting state.
func newFlakyFixture(t *testing.T) (f *proxFixture, crashMarker string) {
	t.Helper()

	// A directory of its own: the fixture's own directory does not exist until
	// newInlineFixture creates it, and the path has to be baked into the config
	// that creates it.
	crashMarker = filepath.Join(t.TempDir(), "crash-now")
	f = newInlineFixture(t, fmt.Sprintf(`
processes:
  flaky:
    cmd: "sh -c 'if [ -f %s ]; then exit 3; fi; exec sleep 300'"
  steady:
    cmd: "sleep 300"
`, crashMarker))
	return f, crashMarker
}

// armCrash creates the marker that makes `flaky` exit on its next launch.
func armCrash(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("arm the crash marker: %v", err)
	}
}

// assertCrashReported checks the shared shape of a failed start/restart: the
// crashed process is named in `prox status`'s own words, its logs are pointed
// at, and the success headline never printed.
func assertCrashReported(t *testing.T, out, name, headline string) {
	t.Helper()
	for _, want := range []string{
		"Crashed: " + name,
		"prox logs " + name,
		"1 process(es) crashed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, headline) {
		t.Errorf("a failed command printed its success headline %q; got:\n%s", headline, out)
	}
}
