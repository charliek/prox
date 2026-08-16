package integration

import (
	"strings"
	"testing"
	"time"
)

// This file covers the session warning channel end to end (plan 028 A2): an
// advisory raised where the user cannot see it — inside a `prox up -d` child,
// whose stdout and stderr are .prox/prox.log — has to reach the terminal of the
// person who typed the command.
//
// The unit tests in internal/cli pin the pieces (the sink, the two-line shape,
// the parent's poll loop against a fake fetcher). These pin the WIRING against
// the real binary, and above all the one thing no fake can prove: that the
// completion latch really does cover the window between the parent's settle wait
// and a producer that is still running.
//
// # HOW THE TIMING IS MADE REAL, AND THE ONE SYNTHETIC PART OF IT
//
// Plan 028's real asynchronous producer (the B2 DNS check) does not exist yet,
// and no producer that does exist can be asked to finish at a chosen instant. So
// the child is given one via PROX_TEST_STARTUP_WARNING (internal/cli/
// warnings.go): a warning that lands after a stated delay. Everything else is
// the production path — the real sink, the real GET /status, the real parent
// poll loop, the real rendering.
//
// The delay is what makes the test non-vacuous. The parent normally exits at
// (child ready) + a 500ms settle window; the warning does not exist until
// warningProducerDelay after the child starts, which is well past that. A parent
// that fetched status ONCE after settling — the implementation without the latch
// — would print nothing here.
// It must also stay BELOW internal/cli's warningProducerJoinTimeout (2s): past
// that the child seals without ever collecting this warning and the test fails
// outright rather than vacuously. Lowering that constant breaks this test, and
// this comment is the pointer that saves the next person a bisect (CodeRabbit
// review).
const warningProducerDelay = 1200 * time.Millisecond

// The advisory the hook produces. Deliberately distinctive so it cannot be
// confused with any other line prox prints.
const (
	warningHookMessage = "the synthetic startup check reported a problem."
	warningHookHint    = "Run 'prox doctor' (this is a test fixture)."
)

// warningHookEnv is the PROX_TEST_STARTUP_WARNING value: "<delay>|<code>|<message>|<hint>".
func warningHookEnv(delay time.Duration) string {
	return "PROX_TEST_STARTUP_WARNING=" + strings.Join([]string{
		delay.String(), "test_startup_warning", warningHookMessage, warningHookHint,
	}, "|")
}

const warningFixtureConfig = `
processes:
  web:
    cmd: "sleep 300"
`

// TestWarnings_UpDetachSurfacesAWarningSealedAfterSettling is the flagship: a
// warning that is not even produced until after `prox up -d` has finished
// waiting for readiness and watching the processes settle still reaches the
// launcher's terminal.
//
// It fails in exactly the way the feature would be broken: without the
// warnings_sealed latch (or with a parent that reads status once instead of
// polling it), the launcher exits before the warning exists and prints nothing.
func TestWarnings_UpDetachSurfacesAWarningSealedAfterSettling(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, warningFixtureConfig)

	started := time.Now()
	run := startDetachedIn(t, binary, f.dir, []startOpt{withEnv(warningHookEnv(warningProducerDelay))},
		"up", "-d", "--no-proxy", "-c", f.configPath)
	launcherTook := time.Since(started)

	out := run.Output()
	if !strings.Contains(out, "Warning: "+warningHookMessage) {
		t.Fatalf("the launcher never printed the daemon-side warning; output:\n%s", out)
	}
	if !strings.Contains(out, "         "+warningHookHint) {
		t.Errorf("the hint must ride along, indented under the message; output:\n%s", out)
	}

	// The headline still comes first: an advisory must not displace the outcome
	// the user is waiting for.
	headline := strings.Index(out, "prox started")
	warning := strings.Index(out, "Warning: "+warningHookMessage)
	if headline < 0 || headline > warning {
		t.Errorf("expected the success headline before the warning; output:\n%s", out)
	}

	// The timing assertion is what keeps this test honest. The launcher cannot
	// have printed that warning without waiting past the settle window for the
	// latch, because the warning did not exist until then.
	if launcherTook < warningProducerDelay {
		t.Errorf("launcher exited in %s, sooner than the %s producer delay: the warning it printed "+
			"cannot have come from the post-settle window this test exists to cover",
			launcherTook, warningProducerDelay)
	}

	// The daemon is up and holds the warning for later readers too: `prox status`
	// prints it, and — because an advisory is not a failure — still exits 0.
	waitForAPI(t, run.Addr(), within(t, apiReadyTimeout))
	statusOut, code := f.Run(t, binary, "status")
	if code != 0 {
		t.Errorf("`prox status` exit = %d, want 0: a warning must never change the exit code; output:\n%s",
			code, statusOut)
	}
	if !strings.Contains(statusOut, "Warning: "+warningHookMessage) {
		t.Errorf("`prox status` did not print the session warning; output:\n%s", statusOut)
	}
}

// TestWarnings_ForegroundUpPrintsThemOnStderr is the plain-mode half: a
// foreground session has no wire and no parent, so the warning has to appear on
// the session's own output, in the same two-line shape.
func TestWarnings_ForegroundUpPrintsThemOnStderr(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	f := newInlineFixture(t, warningFixtureConfig)

	// A short delay here: nothing is racing the render, this only has to prove
	// the end-of-startup report happens at all.
	run := f.StartWith(t, binary, []startOpt{withEnv(warningHookEnv(50 * time.Millisecond))},
		"up", "--no-tui", "--no-proxy", "-c", f.configPath)

	waitForRunOutputContains(t, run, "Warning: "+warningHookMessage, within(t, apiReadyTimeout))
	if out := run.Output(); !strings.Contains(out, "         "+warningHookHint) {
		t.Errorf("the hint must be printed under the message; output:\n%s", out)
	}
}
