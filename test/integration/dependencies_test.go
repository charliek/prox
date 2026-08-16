package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// depStatusRunner starts a `prox up -d --no-proxy` daemon for the given config
// in a fresh temp dir, waits for its state file, registers teardown, and returns
// a closure that runs `prox status` (table or --json) in the project dir,
// returning stdout, stderr, and exit code separately. Mirrors status_test.go's
// crashed-child harness (plan 013 D5).
func depStatusRunner(t *testing.T, binary, config string) (tmpDir string, runStatus statusRunner) {
	t.Helper()
	tmpDir = t.TempDir()

	configPath := filepath.Join(tmpDir, "prox.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Not runCLI: several of these configs describe a process or task that is
	// MEANT to fail, and since plan 027 C13 (#94) `up -d` exits non-zero when a
	// process reaches a terminal-failed state inside the settle window. That
	// non-zero exit means "the daemon is up, its processes are not" -- the
	// daemon this runner then queries is running either way.
	startDaemonAllowingProcessFailure(t, binary, tmpDir, "start daemon", "up", "-d", "--no-proxy", "-c", configPath)
	t.Cleanup(func() {
		stopCLIQuietly(binary, tmpDir, "stop", "-c", configPath)
		time.Sleep(300 * time.Millisecond)
	})

	waitForStateFile(t, filepath.Join(tmpDir, ".prox", "prox.state"), within(t, stateFileTimeout))

	return tmpDir, runStatusIn(t, binary, tmpDir, configPath)
}

// runCLI runs one prox CLI invocation to completion in dir, bounded by the CLI
// ceiling, and fails the test with what it printed if it does not exit 0.
func runCLI(t *testing.T, binary, dir, what string, args ...string) string {
	t.Helper()

	ctx, cancel := boundedContext(within(t, cliCommandTimeout), cliCommandTimeout)
	defer cancel()

	cmd := boundedCommand(ctx, dir, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\noutput: %s", what, err, out)
	}
	return string(out)
}

// startDaemonAllowingProcessFailure is runCLI for a `prox up -d` whose config
// deliberately contains a process or task that fails.
//
// Since plan 027 C13 (#94) the launcher reports the resulting STATE, not just
// that the daemon accepted the request: a process that reaches crashed (or
// blocked) inside the settle window makes `up -d` exit 1 even though the daemon
// itself is up and stays up. runCLI would fail such a test on the very
// condition it was written to produce. Exit 0 and exit 1 are therefore both
// accepted here; anything else (a signal, a config error, a failure to launch
// at all) still fails the test, and the output is returned for inspection.
func startDaemonAllowingProcessFailure(t *testing.T, binary, dir, what string, args ...string) string {
	t.Helper()

	ctx, cancel := boundedContext(within(t, cliCommandTimeout), cliCommandTimeout)
	defer cancel()

	cmd := boundedCommand(ctx, dir, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("%s: %v\noutput: %s", what, err, out)
		}
		if ee.ExitCode() != 1 {
			t.Fatalf("%s: unexpected exit code %d\noutput: %s", what, ee.ExitCode(), out)
		}
	}
	return string(out)
}

// stopCLIQuietly runs a best-effort teardown command, bounded, ignoring its
// outcome. Bounded because a cleanup that hangs hangs the test that registered
// it, and t.Cleanup is exactly where an unbounded wait is hardest to see;
// t-free because it also runs after a test has already failed.
func stopCLIQuietly(binary, dir string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), cliCommandTimeout)
	defer cancel()

	cmd := boundedCommand(ctx, dir, binary, args...)
	_, _ = cmd.CombinedOutput()
}

// statusJSONPayload is the subset of `prox status --json` this suite asserts on.
type statusJSONPayload struct {
	Processes []struct {
		Name      string   `json:"name"`
		Status    string   `json:"status"`
		Kind      string   `json:"kind"`
		WaitingOn []string `json:"waiting_on"`
		BlockedOn []string `json:"blocked_on"`
	} `json:"processes"`
	Status struct {
		Dependencies []struct {
			Name         string `json:"name"`
			State        string `json:"state"`
			Check        string `json:"check"`
			LastError    string `json:"last_error"`
			StartInvoked bool   `json:"start_invoked"`
		} `json:"dependencies"`
	} `json:"status"`
}

// statusRunner runs `prox status` with extra args and returns stdout, stderr
// and the exit code.
//
// It takes a deadline because the command it runs is a real subprocess talking
// to a real daemon over HTTP: without one it is unbounded, and an unbounded
// step inside a bounded poll loop makes the loop's bound a decoration. The
// deadline (rather than a duration) is what lets awaitStatusJSON hand each
// invocation only what is left of the loop's own budget.
type statusRunner func(deadline time.Time, extra ...string) (string, string, int)

// runStatusIn builds the statusRunner for a project directory: `prox status
// -c <config>` run in dir, bounded by the shorter of the caller's deadline and
// the CLI ceiling.
func runStatusIn(t *testing.T, binary, dir, configPath string) statusRunner {
	return func(deadline time.Time, extra ...string) (string, string, int) {
		t.Helper()
		ctx, cancel := boundedContext(deadline, cliCommandTimeout)
		defer cancel()

		args := append([]string{"status", "-c", configPath}, extra...)
		cmd := boundedCommand(ctx, dir, binary, args...)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), exitCodeOf(t, err)
	}
}

// pollStatusJSON polls `prox status --json` until match returns true, and fails
// the test if the deadline arrives first.
//
// Two defects, both fixed here (plan 027 C6):
//
//   - it shelled out with a bare cmd.Run() and NO timeout, so one hung `prox
//     status` hung the loop that existed to bound it;
//   - on expiry it RETURNED THE LAST PAYLOAD instead of failing. Every caller
//     then re-checked the same predicate and reported something like "gated
//     never converged to running; got \"waiting\"" -- a statement about process
//     state, for what was actually a timeout. The real failure surfaced as a
//     confusing downstream assertion, one level away from the thing that
//     actually went wrong.
func pollStatusJSON(t *testing.T, runStatus statusRunner, deadline time.Time, match func(statusJSONPayload) bool) statusJSONPayload {
	t.Helper()
	p, err := awaitStatusJSON(runStatus, deadline, match)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// awaitStatusJSON is pollStatusJSON without the t.Fatal, so bounding_test.go
// can point it at a subprocess that never exits and assert that it comes back
// anyway -- and that the elapsed time it claims is the elapsed time that really
// passed.
func awaitStatusJSON(runStatus statusRunner, deadline time.Time, match func(statusJSONPayload) bool) (statusJSONPayload, error) {
	start := time.Now()
	var last statusJSONPayload
	var decoded bool
	for time.Now().Before(deadline) {
		// The loop's own deadline goes to each invocation, which bounds itself
		// by the SHORTER of that and the CLI ceiling -- so a stalled `prox
		// status` cannot push the loop past its own deadline.
		stdout, _, _ := runStatus(deadline, "--json")

		var p statusJSONPayload
		if json.Unmarshal([]byte(stdout), &p) == nil {
			last, decoded = p, true
			if match(p) {
				return p, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !decoded {
		return last, fmt.Errorf("`prox status --json` never produced a decodable payload %s", waitedFor(start, deadline))
	}
	return last, fmt.Errorf("`prox status --json` never satisfied the predicate %s; last payload: %+v",
		waitedFor(start, deadline), last)
}

func procState(p statusJSONPayload, name string) (status string, waitingOn, blockedOn []string, ok bool) {
	for _, pr := range p.Processes {
		if pr.Name == name {
			return pr.Status, pr.WaitingOn, pr.BlockedOn, true
		}
	}
	return "", nil, nil, false
}

func depState(p statusJSONPayload, name string) (state string, ok bool) {
	for _, d := range p.Status.Dependencies {
		if d.Name == name {
			return d.State, true
		}
	}
	return "", false
}

// TestDependenciesStatus exercises the plan 013 D5 status/exit surface end to end
// against the real binary. Subtests share one built binary but each runs its own
// isolated --no-proxy daemon.
func TestDependenciesStatus(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)
	binary := buildBinary(t)

	// Completed task → exit 0 (the rc=0 retirement flagship): a run-to-completion
	// task that exits 0 must NOT trip the exit contract.
	t.Run("completed task exits 0", func(t *testing.T) {
		_, runStatus := depStatusRunner(t, binary, `
processes:
  keeper: "sleep 300"
tasks:
  migrate:
    cmd: "sh -c 'exit 0'"
    timeout: 30s
`)
		p := pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "migrate")
			return ok && s == "completed"
		})
		if s, _, _, ok := procState(p, "migrate"); !ok || s != "completed" {
			t.Fatalf("task migrate never completed; got %q (ok=%v)", s, ok)
		}
		stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
		if code != 0 {
			t.Fatalf("exit = %d, want 0 for a completed task; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "migrate") || !strings.Contains(stdout, "completed") {
			t.Errorf("stdout missing the completed task row; got:\n%s", stdout)
		}
	})

	// Crashed task → exit 1: a task that exits non-zero lands crashed and trips
	// the same crashed-exit contract a process does.
	t.Run("crashed task exits 1", func(t *testing.T) {
		_, runStatus := depStatusRunner(t, binary, `
processes:
  keeper: "sleep 300"
tasks:
  migrate:
    cmd: "sh -c 'exit 1'"
    timeout: 30s
`)
		pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "migrate")
			return ok && s == "crashed"
		})
		stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
		if code != 1 {
			t.Fatalf("exit = %d, want 1 for a crashed task; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "Crashed: migrate") {
			t.Errorf("stdout missing the Crashed line; got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "Error: 1 process(es) crashed") {
			t.Errorf("stderr missing the crashed sentinel; got:\n%s", stderr)
		}
	})

	// Blocked process → exit 1 with the Blocked line, the failed dependency in the
	// Dependencies section, and blocked_on in JSON. The dependency's check is a
	// deterministically-failing command (`false` always exits non-zero) with a
	// tiny budget so it exhausts to failed (fail policy) without depending on a
	// port that a racing test could reuse.
	t.Run("blocked process exits 1", func(t *testing.T) {
		_, runStatus := depStatusRunner(t, binary, `
processes:
  gated:
    cmd: "sleep 300"
    depends_on: [pg]
dependencies:
  pg:
    check:
      cmd: "false"
      timeout: 1s
      interval: 200ms
    on_failure: fail
`)

		p := pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "gated")
			return ok && s == "blocked"
		})
		_, _, blockedOn, ok := procState(p, "gated")
		if !ok || len(blockedOn) != 1 || blockedOn[0] != "pg" {
			t.Fatalf("gated blocked_on = %v (ok=%v), want [pg]", blockedOn, ok)
		}
		if ds, ok := depState(p, "pg"); !ok || ds != "failed" {
			t.Fatalf("dependency pg state = %q (ok=%v), want failed", ds, ok)
		}

		stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
		if code != 1 {
			t.Fatalf("exit = %d, want 1 for a blocked process; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "Blocked: gated(pg)") {
			t.Errorf("stdout missing the Blocked line; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, "Dependencies:") || !strings.Contains(stdout, "pg") || !strings.Contains(stdout, "failed") {
			t.Errorf("stdout missing the Dependencies section; got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "Error: 1 process(es) blocked on failed dependencies") {
			t.Errorf("stderr missing the blocked sentinel; got:\n%s", stderr)
		}
	})

	// Warn dependency never becomes healthy → its dependents still run and status
	// exits 0; the warned dependency is visible in --json.
	t.Run("warn dependency lets dependents run, exit 0", func(t *testing.T) {
		_, runStatus := depStatusRunner(t, binary, `
processes:
  web:
    cmd: "sleep 300"
    depends_on: [flaky]
dependencies:
  flaky:
    check:
      cmd: "false"
      timeout: 1s
      interval: 200ms
    on_failure: warn
`)

		p := pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "web")
			return ok && s == "running"
		})
		if ds, ok := depState(p, "flaky"); !ok || ds != "warned" {
			t.Fatalf("dependency flaky state = %q (ok=%v), want warned", ds, ok)
		}
		stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
		if code != 0 {
			t.Fatalf("exit = %d, want 0 when only a warn dependency failed; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "warned") {
			t.Errorf("stdout missing the warned dependency; got:\n%s", stdout)
		}
	})

	// Combined crashed + blocked: both table lines print, but the crashed error
	// wins the primary sentinel (precedence crashed > blocked).
	t.Run("crashed and blocked coexist, crashed wins", func(t *testing.T) {
		_, runStatus := depStatusRunner(t, binary, `
processes:
  crasher: "sh -c 'exit 3'"
  gated:
    cmd: "sleep 300"
    depends_on: [pg]
dependencies:
  pg:
    check:
      cmd: "false"
      timeout: 1s
      interval: 200ms
    on_failure: fail
`)

		pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			cs, _, _, cok := procState(p, "crasher")
			gs, _, _, gok := procState(p, "gated")
			return cok && cs == "crashed" && gok && gs == "blocked"
		})
		stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "Crashed: crasher") {
			t.Errorf("stdout missing the Crashed line; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, "Blocked: gated(pg)") {
			t.Errorf("stdout missing the Blocked line; got:\n%s", stdout)
		}
		// Crashed outranks blocked for the single primary sentinel.
		if !strings.Contains(stderr, "Error: 1 process(es) crashed") {
			t.Errorf("stderr should carry the crashed sentinel (precedence); got:\n%s", stderr)
		}
		if strings.Contains(stderr, "blocked on failed dependencies") {
			t.Errorf("stderr should NOT carry the blocked sentinel when crashed wins; got:\n%s", stderr)
		}
	})

	// A slow dependency is held unready by a three-marker file barrier until
	// the test has itself observed `waiting`, so that observation can never
	// race convergence: `up -d` returns promptly while the dependency's start
	// command blocks waiting for a release marker the test controls; the test
	// observes gated waiting on slow (with waiting_on in JSON); the test then
	// writes the release marker, which lets the start command touch the
	// readiness marker the dependency's check polls for; and gated converges
	// to running once the check passes. The STATUS-column `waiting(slow)`
	// decoration is pinned by the statusField unit test instead.
	t.Run("detached slow dependency: waiting then converges", func(t *testing.T) {
		tmpDir := t.TempDir()
		invoked := filepath.Join(tmpDir, "invoked.marker")
		release := filepath.Join(tmpDir, "release.marker")
		ready := filepath.Join(tmpDir, "ready.marker")
		configPath := filepath.Join(tmpDir, "prox.yaml")
		config := fmt.Sprintf(`
processes:
  gated:
    cmd: "sleep 300"
    depends_on: [slow]
dependencies:
  slow:
    check:
      cmd: "test -f '%s'"
      # Generous: the barrier holds the dependency unready until the test has
      # observed waiting, so the budget must dominate the poll deadlines above
      # it, not model a realistic startup time.
      timeout: 60s
      interval: 200ms
    start: "sh -c 'touch \"%s\"; while [ ! -f \"%s\" ]; do sleep 0.1; done; touch \"%s\"'"
    on_failure: fail
`, ready, invoked, release, ready)
		if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		start := time.Now()
		runCLI(t, binary, tmpDir, "start daemon", "up", "-d", "--no-proxy", "-c", configPath)
		elapsed := time.Since(start)
		t.Cleanup(func() {
			// Self-release the barrier so the start helper's loop exits even if
			// the test failed before the release step or `stop` cannot reach the
			// daemon — otherwise the loop would spin on unfailingly at 10Hz.
			_ = os.WriteFile(release, nil, 0644)
			stopCLIQuietly(binary, tmpDir, "stop", "-c", configPath)
			time.Sleep(300 * time.Millisecond)
		})
		waitForStateFile(t, filepath.Join(tmpDir, ".prox", "prox.state"), within(t, stateFileTimeout))
		// Detached start returns without waiting for the dependency.
		if elapsed > 5*time.Second {
			t.Errorf("`up -d` took %v; it must return promptly while the dependency resolves", elapsed)
		}

		// The dependency's start command touches this the moment it actually
		// launches, proving the start: command ran (not just that it was
		// scheduled).
		waitForStateFile(t, invoked, within(t, stateFileTimeout))

		runStatus := runStatusIn(t, binary, tmpDir, configPath)

		// Observe gated waiting on slow via JSON. The dependency cannot become
		// ready before the test writes the release marker below, so this
		// observation is race-free by construction; the deadline is generous
		// headroom, not a load-bearing bound.
		waiting := pollStatusJSON(t, runStatus, within(t, processStateTimeout), func(p statusJSONPayload) bool {
			s, w, _, ok := procState(p, "gated")
			return ok && s == "waiting" && len(w) == 1 && w[0] == "slow"
		})
		if s, w, _, ok := procState(waiting, "gated"); !ok || s != "waiting" || len(w) != 1 || w[0] != "slow" {
			t.Fatalf("gated never observed waiting on slow; status=%q waiting_on=%v", s, w)
		}

		// Release the barrier: the start helper touches the ready marker and
		// the 200ms check poll converges.
		if err := os.WriteFile(release, nil, 0644); err != nil {
			t.Fatalf("write release marker: %v", err)
		}

		// Then it converges to running once the dependency becomes ready.
		converged := pollStatusJSON(t, runStatus, within(t, dependencyReadyTimeout), func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "gated")
			return ok && s == "running"
		})
		if s, _, _, ok := procState(converged, "gated"); !ok || s != "running" {
			t.Fatalf("gated never converged to running; got %q", s)
		}
		_, _, code := runStatus(within(t, cliCommandTimeout))
		if code != 0 {
			t.Errorf("exit = %d, want 0 once the dependency is healthy", code)
		}
	})
}
