package integration

import (
	"encoding/json"
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
func depStatusRunner(t *testing.T, binary, config string) (tmpDir string, runStatus func(extra ...string) (string, string, int)) {
	t.Helper()
	tmpDir = t.TempDir()

	configPath := filepath.Join(tmpDir, "prox.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	up := exec.Command(binary, "up", "-d", "--no-proxy", "-c", configPath)
	up.Dir = tmpDir
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("start daemon: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(binary, "stop", "-c", configPath)
		stop.Dir = tmpDir
		_, _ = stop.CombinedOutput()
		time.Sleep(300 * time.Millisecond)
	})

	waitForStateFile(t, filepath.Join(tmpDir, ".prox", "prox.state"), 10*time.Second)

	runStatus = func(extra ...string) (string, string, int) {
		t.Helper()
		args := append([]string{"status", "-c", configPath}, extra...)
		cmd := exec.Command(binary, args...)
		cmd.Dir = tmpDir
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), exitCodeOf(t, err)
	}
	return tmpDir, runStatus
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

// pollStatusJSON polls `prox status --json` until match returns true or the
// deadline elapses, returning the last decoded payload.
func pollStatusJSON(t *testing.T, runStatus func(...string) (string, string, int), timeout time.Duration, match func(statusJSONPayload) bool) statusJSONPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last statusJSONPayload
	for time.Now().Before(deadline) {
		stdout, _, _ := runStatus("--json")
		var p statusJSONPayload
		if json.Unmarshal([]byte(stdout), &p) == nil {
			last = p
			if match(p) {
				return p
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return last
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
		p := pollStatusJSON(t, runStatus, 15*time.Second, func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "migrate")
			return ok && s == "completed"
		})
		if s, _, _, ok := procState(p, "migrate"); !ok || s != "completed" {
			t.Fatalf("task migrate never completed; got %q (ok=%v)", s, ok)
		}
		stdout, stderr, code := runStatus()
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
		pollStatusJSON(t, runStatus, 15*time.Second, func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "migrate")
			return ok && s == "crashed"
		})
		stdout, stderr, code := runStatus()
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

		p := pollStatusJSON(t, runStatus, 15*time.Second, func(p statusJSONPayload) bool {
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

		stdout, stderr, code := runStatus()
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

		p := pollStatusJSON(t, runStatus, 15*time.Second, func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "web")
			return ok && s == "running"
		})
		if ds, ok := depState(p, "flaky"); !ok || ds != "warned" {
			t.Fatalf("dependency flaky state = %q (ok=%v), want warned", ds, ok)
		}
		stdout, stderr, code := runStatus()
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

		pollStatusJSON(t, runStatus, 15*time.Second, func(p statusJSONPayload) bool {
			cs, _, _, cok := procState(p, "crasher")
			gs, _, _, gok := procState(p, "gated")
			return cok && cs == "crashed" && gok && gs == "blocked"
		})
		stdout, stderr, code := runStatus()
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

	// A slow dependency (ready after ~5s via a start command) is served while
	// resolving: `up -d` returns promptly and status observes gated waiting on
	// slow (with waiting_on in JSON) comfortably inside the 5s window, then
	// converges to running once the dependency is ready. The waiting observation
	// stays in JSON so it never races the convergence the way a second table-mode
	// request could; the STATUS-column `waiting(slow)` decoration is pinned by the
	// statusField unit test instead.
	t.Run("detached slow dependency: waiting then converges", func(t *testing.T) {
		tmpDir := t.TempDir()
		marker := filepath.Join(tmpDir, "ready.marker")
		configPath := filepath.Join(tmpDir, "prox.yaml")
		config := fmt.Sprintf(`
processes:
  gated:
    cmd: "sleep 300"
    depends_on: [slow]
dependencies:
  slow:
    check:
      cmd: "test -f %s"
      timeout: 30s
      interval: 200ms
    start: "sh -c 'sleep 5; touch %s'"
    on_failure: fail
`, marker, marker)
		if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		start := time.Now()
		up := exec.Command(binary, "up", "-d", "--no-proxy", "-c", configPath)
		up.Dir = tmpDir
		if out, err := up.CombinedOutput(); err != nil {
			t.Fatalf("start daemon: %v\noutput: %s", err, out)
		}
		elapsed := time.Since(start)
		t.Cleanup(func() {
			stop := exec.Command(binary, "stop", "-c", configPath)
			stop.Dir = tmpDir
			_, _ = stop.CombinedOutput()
			time.Sleep(300 * time.Millisecond)
		})
		waitForStateFile(t, filepath.Join(tmpDir, ".prox", "prox.state"), 10*time.Second)
		// Detached start returns without waiting for the ~5s dependency.
		if elapsed > 3*time.Second {
			t.Errorf("`up -d` took %v; it must return promptly while the dependency resolves", elapsed)
		}

		runStatus := func(extra ...string) (string, string, int) {
			t.Helper()
			args := append([]string{"status", "-c", configPath}, extra...)
			cmd := exec.Command(binary, args...)
			cmd.Dir = tmpDir
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			return stdout.String(), stderr.String(), exitCodeOf(t, err)
		}

		// Observe gated waiting on slow via JSON, comfortably inside the ~5s start
		// window (poll deadline 4s < 5s so the observation cannot race convergence).
		waiting := pollStatusJSON(t, runStatus, 4*time.Second, func(p statusJSONPayload) bool {
			s, w, _, ok := procState(p, "gated")
			return ok && s == "waiting" && len(w) == 1 && w[0] == "slow"
		})
		if s, w, _, ok := procState(waiting, "gated"); !ok || s != "waiting" || len(w) != 1 || w[0] != "slow" {
			t.Fatalf("gated never observed waiting on slow; status=%q waiting_on=%v", s, w)
		}

		// Then it converges to running once the dependency becomes ready.
		converged := pollStatusJSON(t, runStatus, 30*time.Second, func(p statusJSONPayload) bool {
			s, _, _, ok := procState(p, "gated")
			return ok && s == "running"
		})
		if s, _, _, ok := procState(converged, "gated"); !ok || s != "running" {
			t.Fatalf("gated never converged to running; got %q", s)
		}
		_, _, code := runStatus()
		if code != 0 {
			t.Errorf("exit = %d, want 0 once the dependency is healthy", code)
		}
	})
}
