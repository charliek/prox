package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatus_CrashedChildExits1 pins the #72 exit contract end to end against
// the real binary: a project whose only child exits immediately lands in the
// crashed state, and `prox status` then exits 1 (both table and JSON modes),
// names the crashed process, and surfaces the sentinel on stderr.
func TestStatus_CrashedChildExits1(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// A single child that exits quickly with a non-zero code. The supervisor
	// marks any non-Stop-driven exit as crashed (sticky, no auto-restart).
	configPath := filepath.Join(tmpDir, "prox.yaml")
	if err := os.WriteFile(configPath, []byte(`
processes:
  crasher: "sh -c 'exit 3'"
`), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start the daemon without the shared proxy so this test never touches the
	// machine-wide daemon socket.
	// The launcher itself now exits 1 for this config: since plan 027 C13 (#94)
	// `up -d` reports the resulting state, and this config's only process
	// crashes inside the settle window. The daemon is up regardless -- that is
	// precisely what the non-zero exit means here -- so the rest of the test
	// (which is about `prox status`) is unaffected.
	startDaemonAllowingProcessFailure(t, binary, tmpDir, "failed to start daemon", "up", "-d", "--no-proxy", "-c", configPath)

	// Always tear the daemon down so a wedged test never strands a daemon.
	// Registered before the readiness wait so a failed wait still cleans up.
	t.Cleanup(func() {
		stopCLIQuietly(binary, tmpDir, "stop", "-c", configPath)
		time.Sleep(300 * time.Millisecond)
	})

	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, within(t, stateFileTimeout))

	// runStatus runs `prox status` (with optional extra args) in the project
	// directory and returns its stdout, stderr, and exit code separately (the
	// `Error:` sentinel lands on stderr, so keeping the streams apart lets the
	// JSON on stdout parse cleanly).
	runStatus := runStatusIn(t, binary, tmpDir, configPath)

	// Poll `prox status --json` until the child reports crashed. Bounded
	// deadline, no fixed sleeps proportional to the crash timing.
	deadline := time.Now().Add(15 * time.Second)
	crashed := false
	for time.Now().Before(deadline) {
		stdout, _, _ := runStatus(within(t, cliCommandTimeout), "--json")
		var payload struct {
			Processes []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"processes"`
		}
		if json.Unmarshal([]byte(stdout), &payload) == nil {
			for _, p := range payload.Processes {
				if p.Name == "crasher" && p.Status == "crashed" {
					crashed = true
				}
			}
		}
		if crashed {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !crashed {
		t.Fatal("child never reached the crashed state within the deadline")
	}

	// Table mode: exit 1, names the crashed process on stdout, and the sentinel
	// appears on stderr at the real CLI boundary.
	stdout, stderr, code := runStatus(within(t, cliCommandTimeout))
	if code != 1 {
		t.Errorf("`prox status` exit code = %d, want 1; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Crashed: crasher") {
		t.Errorf("stdout missing the Crashed line; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Error: 1 process(es) crashed") {
		t.Errorf("stderr missing the Error sentinel; got:\n%s", stderr)
	}

	// JSON mode also exits 1.
	_, _, jsonCode := runStatus(within(t, cliCommandTimeout), "--json")
	if jsonCode != 1 {
		t.Errorf("`prox status --json` exit code = %d, want 1", jsonCode)
	}
}

// exitCodeOf extracts a process exit code from an *exec.Cmd run error (0 on nil,
// the real code on an ExitError, fatal otherwise).
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("command failed without an exit code: %v", err)
	return -1
}
