package integration

import (
	"strings"
	"testing"
)

// This file covers the three truthfulness contracts from #95 (plan 027 C11) at
// the real CLI boundary, where the messages are actually read:
//
//   - the "Is prox running?" hint appears ONLY when nothing is listening;
//   - an unknown process name is NAMED, with a working alternative;
//   - `prox logs <typo>` is an error, not silent success.
//
// The unit tests in internal/cli pin the classification itself; these pin what
// a user sees, which is the part that was wrong.

// errorsFixtureConfig is a project with two long-lived processes, so a name can
// be mistyped in a way that has an obvious intended target ("wbe" -> "web").
const errorsFixtureConfig = `
processes:
  web: "sh -c 'echo web-ready; sleep 300'"
  worker: "sh -c 'sleep 300'"
`

// unreachableAddr is a loopback address nothing can be listening on: port 1 is
// privileged and unused, so a dial there is refused immediately. That makes it
// a deterministic stand-in for "no daemon", with none of the allocate-and-hope
// raciness of releasing a real ephemeral port and betting nothing takes it.
const unreachableAddr = "http://127.0.0.1:1"

// TestCLIErrors_LiveDaemonNeverClaimsProxIsDown is the #95 regression test: a
// daemon that just answered must never be reported as possibly absent.
func TestCLIErrors_LiveDaemonNeverClaimsProxIsDown(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, errorsFixtureConfig)
	run := f.Start(t, binary, "up", "--no-proxy", "-c", f.configPath)
	waitForAPI(t, run.Addr(), within(t, apiReadyTimeout))
	waitForProcessState(t, run.Addr(), "web", "running", within(t, processStateTimeout))

	t.Run("stop a process that is not running", func(t *testing.T) {
		if out, code := f.Run(t, binary, "stop", "web"); code != 0 {
			t.Fatalf("first stop should succeed, got exit %d:\n%s", code, out)
		}

		out, code := f.Run(t, binary, "stop", "web")
		if code == 0 {
			t.Errorf("stopping an already-stopped process should fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "PROCESS_NOT_RUNNING") {
			t.Errorf("expected the daemon's own answer, got:\n%s", out)
		}
		assertNoDaemonDownHint(t, out)
	})

	t.Run("start an unknown process", func(t *testing.T) {
		out, code := f.Run(t, binary, "start", "wbe")
		if code == 0 {
			t.Errorf("starting an unknown process should fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, `"wbe"`) {
			t.Errorf("the error must name the process that was asked for, got:\n%s", out)
		}
		if !strings.Contains(out, `Did you mean "web"?`) {
			t.Errorf("the error must name a process that exists, got:\n%s", out)
		}
		assertNoDaemonDownHint(t, out)
	})

	t.Run("restart an unknown process lists the real names", func(t *testing.T) {
		out, code := f.Run(t, binary, "restart", "zzzzzzzz")
		if code == 0 {
			t.Errorf("restarting an unknown process should fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "Known processes:") {
			t.Errorf("a name nothing resembles must list the valid ones, got:\n%s", out)
		}
		for _, name := range []string{"web", "worker"} {
			if !strings.Contains(out, name) {
				t.Errorf("expected %q in the list of valid names, got:\n%s", name, out)
			}
		}
		assertNoDaemonDownHint(t, out)
	})

	t.Run("logs for an unknown process", func(t *testing.T) {
		// The whole defect: this used to print nothing and exit 0, which reads
		// exactly like "that process has logged nothing".
		out, code := f.Run(t, binary, "logs", "wbe")
		if code == 0 {
			t.Errorf("a mistyped process name must not exit 0:\n%s", out)
		}
		if !strings.Contains(out, `unknown process "wbe"`) {
			t.Errorf("the error must name the process that was asked for, got:\n%s", out)
		}
		if !strings.Contains(out, `Did you mean "web"?`) {
			t.Errorf("the error must name a process that exists, got:\n%s", out)
		}
		assertNoDaemonDownHint(t, out)
	})

	t.Run("logs for an unknown process via --process", func(t *testing.T) {
		out, code := f.Run(t, binary, "logs", "--process", "web,wrker")
		if code == 0 {
			t.Errorf("a mistyped name in a comma-separated filter must not exit 0:\n%s", out)
		}
		if !strings.Contains(out, `unknown process "wrker"`) {
			t.Errorf("the error must name the offending element, got:\n%s", out)
		}
	})

	t.Run("logs for an unknown process with --follow", func(t *testing.T) {
		out, code := f.Run(t, binary, "logs", "--follow", "wbe")
		if code == 0 {
			t.Errorf("--follow must validate the name too, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, `unknown process "wbe"`) {
			t.Errorf("the error must name the process that was asked for, got:\n%s", out)
		}
	})

	t.Run("logs for a real process still works", func(t *testing.T) {
		// The other half of the contract: only a wrong NAME is an error. A real
		// process still prints its logs and exits 0.
		out, code := f.Run(t, binary, "logs", "worker")
		if code != 0 {
			t.Errorf("a valid process name must still exit 0, got %d:\n%s", code, out)
		}
	})
}

// TestCLIErrors_UnreachableDaemonKeepsTheHint is the other side of the same
// change, and the reason it had to be positive detection rather than a blanket
// removal: when nothing IS listening, "Try 'prox up' first" is exactly the
// action that works, and it must still be printed.
func TestCLIErrors_UnreachableDaemonKeepsTheHint(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	// A project directory with a config but no daemon. --addr is what makes the
	// dial happen at all: without it a client command stops at discovery (no
	// .prox/prox.state), which has its own, already-truthful message.
	f := newInlineFixture(t, errorsFixtureConfig)

	for _, args := range [][]string{
		{"status", "--addr", unreachableAddr},
		{"logs", "--addr", unreachableAddr},
		{"stop", "web", "--addr", unreachableAddr},
		{"start", "web", "--addr", unreachableAddr},
	} {
		out, code := f.Run(t, binary, args...)
		if code == 0 {
			t.Errorf("prox %s against a dead address should fail, got exit 0:\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(out, "Is prox running") {
			t.Errorf("prox %s must keep the hint when nothing is listening, got:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestCLIErrors_NoStateFileExplainsItself pins the message a client command
// gives with no daemon and no --addr: it never reaches the hint at all, because
// discovery stops first — and what it says is already actionable.
func TestCLIErrors_NoStateFileExplainsItself(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, errorsFixtureConfig)

	out, code := f.Run(t, binary, "logs", "web")
	if code == 0 {
		t.Errorf("expected a non-zero exit with no daemon, got 0:\n%s", out)
	}
	if !strings.Contains(out, "no running prox instance found in this directory") {
		t.Errorf("expected the discovery error, got:\n%s", out)
	}
}

// assertNoDaemonDownHint fails when output claims prox might not be running.
// Every caller has just been answered BY the daemon, so the claim is provably
// false there.
func assertNoDaemonDownHint(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "Is prox running") {
		t.Errorf("the daemon answered this command; it must not be told prox may be down:\n%s", out)
	}
}
