package integration

// Plan 018 C9: PTY-driven end-to-end proof for the TUI shutdown
// non-negotiables that unit tests cannot exercise -- they need a real
// controlling terminal (bubbletea's WithAltScreen requires one) and a real
// isatty(stdin) probe.
//
//   - `prox up --tui` + 'q' stops the supervisor (unlike attach's 'q').
//   - `prox up --tui` + POST /shutdown?wait=true quits the TUI and stops.
//   - `prox up --tui` + SIGTERM quits the TUI and stops.
//   - `prox attach` + 'q' detaches only -- the daemon keeps running.
//   - `prox up --tui` with a non-terminal stdin refuses to start, even with
//     stdout attached to a real terminal (the piped-stdio guard test in
//     up_test.go covers the stdout half; this covers the stdin half).
//
// These complement the pure Update()-driven unit tests (client_model_test.go,
// base_behavior_test.go) by proving the same behavior holds through a real
// bubbletea program running against a real pty, the real CLI process, and the
// real shutdown coordinator.
//
// Every test here skips on windows and skips (not fails) if the sandbox
// refuses to allocate a pty -- some CI/agent sandboxes forbid it.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// tuiPTYWinsize is a generous, fixed terminal size for every PTY test in this
// file. bubbletea's initial WindowSizeMsg comes from the pty's ioctl-reported
// size; an unset (0x0) size would leave the client rendering nothing to grep
// for.
var tuiPTYWinsize = &pty.Winsize{Rows: 40, Cols: 120}

// tuiFixtureConfig writes a single-process fixture config to dir/prox.yaml
// and returns its path. The process is a bare "sleep 300": no shell, no trap
// needed -- SIGTERM alone reaps it immediately, so liveness checks around
// shutdown are unambiguous. api.host is loopback so the daemon runs
// auth-disabled and every HTTP call in this file needs no token.
func tuiFixtureConfig(t *testing.T, dir string) string {
	t.Helper()
	cfg := filepath.Join(dir, "prox.yaml")
	body := "api:\n  host: 127.0.0.1\n\nprocesses:\n  worker:\n    cmd: sleep 300\n"
	if err := os.WriteFile(cfg, []byte(body), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return cfg
}

// startPTY starts cmd with stdin/stdout/stderr all attached to one real pty
// (a genuine controlling terminal, sized via tuiPTYWinsize), and continuously
// drains the pty master into a returned buffer in the background. Draining
// must never stop: an undrained master fills its kernel buffer and blocks
// every subsequent write the child makes, wedging the whole test. Skips
// (rather than failing) the test if this sandbox refuses to allocate a pty.
// It pins TERM to a capable value -- see startPTYWithTERM.
func startPTY(t *testing.T, cmd *exec.Cmd) (*os.File, *syncBuffer) {
	t.Helper()
	return startPTYWithTERM(t, cmd, "xterm-256color")
}

// startPTYWithTERM is startPTY with an explicit TERM, for the tests that want
// an incapable terminal.
//
// TERM is pinned rather than inherited (plan 026 C3). Terminal hostability now
// requires a TERM that is set and not "dumb", and GitHub's ubuntu-latest and
// macos-latest runners -- where CI runs this whole suite, with no -short -- set
// no TERM at all. Inheriting would make every `--tui` test here pass on a
// developer's terminal and fail in CI, and would equally let a developer's
// TERM leak into a test that wants a specific one. So any inherited TERM is
// REMOVED first and the requested value appended: `os.Environ()` carrying
// TERM=xterm-256color plus a later TERM=dumb is resolved by exec to the last
// occurrence, which is subtle enough to be worth not relying on.
func startPTYWithTERM(t *testing.T, cmd *exec.Cmd, term string) (*os.File, *syncBuffer) {
	t.Helper()
	env := cmd.Env
	if env == nil {
		env = os.Environ() // nil means "inherit"; materialize it so we can edit
	}
	filtered := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TERM=") {
			filtered = append(filtered, kv)
		}
	}
	cmd.Env = append(filtered, "TERM="+term)

	ptmx, err := pty.StartWithSize(cmd, tuiPTYWinsize)
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	buf := &syncBuffer{}
	go func() {
		_, _ = io.Copy(buf, ptmx)
	}()
	return ptmx, buf
}

// waitForPTYContains polls buf (raw pty bytes -- ANSI/alt-screen control
// sequences and all) until it contains substr, or fails the test after
// timeout. Deliberately a raw substring search, not a line scan: styled text
// is wrapped in escape codes before/after, never split mid-word, so this is
// safe and much simpler than trying to parse the terminal stream.
func waitForPTYContains(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = buf.String()
		if strings.Contains(last, substr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last = buf.String(); strings.Contains(last, substr) {
		return
	}
	t.Fatalf("pty output did not contain %q within %v; captured output:\n%s", substr, timeout, last)
}

// waitForPTYAPIServerURL polls buf until the "API server: http://..." startup
// line appears -- printed via plain fmt.Printf in internal/cli/up.go BEFORE
// the alt-screen TUI takes over, so it always lands in the raw stream ahead
// of any bubbletea output -- and returns the bare URL (no auth annotation).
func waitForPTYAPIServerURL(t *testing.T, buf *syncBuffer, timeout time.Duration) string {
	t.Helper()
	const marker = "API server: "
	deadline := time.Now().Add(timeout)
	for {
		out := buf.String()
		if idx := strings.Index(out, marker); idx >= 0 {
			rest := out[idx+len(marker):]
			if end := strings.IndexAny(rest, " \r\n"); end >= 0 {
				return rest[:end]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("API server URL not found in pty output within %v; captured:\n%s", timeout, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeToPTY writes s to the pty master, i.e. simulates a keystroke arriving
// on the child's stdin.
func writeToPTY(t *testing.T, ptmx *os.File, s string) {
	t.Helper()
	if _, err := ptmx.Write([]byte(s)); err != nil {
		t.Fatalf("failed to write %q to pty: %v", s, err)
	}
}

// TestUpTUI_QuitStopsSupervisor pins the core non-negotiable of plan 018: `q`
// on `prox up --tui` stops the supervisor -- unlike `q` on `prox attach`,
// which only detaches (TestAttach_QuitDetachesLeavesDaemonRunning below).
func TestUpTUI_QuitStopsSupervisor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := tuiFixtureConfig(t, dir)

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir
	ptmx, buf := startPTY(t, cmd)
	defer ptmx.Close()
	defer killProx(cmd)

	addr := waitForPTYAPIServerURL(t, buf, 15*time.Second)
	waitForAPI(t, addr, 15*time.Second)
	pid := waitForProcessState(t, addr, "worker", "running", 10*time.Second).PID
	waitForPTYContains(t, buf, "worker", 15*time.Second)

	writeToPTY(t, ptmx, "q")

	if err := waitCmdExit(t, cmd, 15*time.Second); err != nil {
		t.Errorf("up --tui should exit 0 after 'q', got %v", err)
	}
	if !waitForPIDGone(pid, 10*time.Second) {
		t.Errorf("worker pid %d should be gone after q-triggered shutdown", pid)
	}
}

// TestUpTUI_ShutdownEndpointQuitsAndStops proves POST
// /api/v1/shutdown?wait=true quits the TUI (via ShutdownCh -> tea.Quit) and
// stops the supervisor, returning a waited, successful JSON body once the
// stop has actually landed -- no deadlock between the handler blocking on the
// coordinator and the coordinator's teardown needing the API server.
func TestUpTUI_ShutdownEndpointQuitsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := tuiFixtureConfig(t, dir)

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir
	ptmx, buf := startPTY(t, cmd)
	defer ptmx.Close()
	defer killProx(cmd)

	addr := waitForPTYAPIServerURL(t, buf, 15*time.Second)
	waitForAPI(t, addr, 15*time.Second)
	pid := waitForProcessState(t, addr, "worker", "running", 10*time.Second).PID
	waitForPTYContains(t, buf, "worker", 15*time.Second)

	req, err := http.NewRequest(http.MethodPost, addr+"/api/v1/shutdown?wait=true", nil)
	requireNoError(t, err, "building shutdown request")
	resp, err := http.DefaultClient.Do(req)
	requireNoError(t, err, "POST /api/v1/shutdown?wait=true")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from wait=true shutdown, got %d", resp.StatusCode)
	}
	var shutdownResp struct {
		Success bool `json:"success"`
		Waited  bool `json:"waited"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&shutdownResp); err != nil {
		t.Fatalf("failed to decode shutdown response: %v", err)
	}
	if !shutdownResp.Waited {
		t.Errorf("expected waited=true in the shutdown response, got %+v", shutdownResp)
	}
	if !shutdownResp.Success {
		t.Errorf("expected success=true in the shutdown response, got %+v", shutdownResp)
	}

	if err := waitCmdExit(t, cmd, 15*time.Second); err != nil {
		t.Errorf("up --tui should exit 0 after POST /shutdown?wait=true, got %v", err)
	}
	if !waitForPIDGone(pid, 10*time.Second) {
		t.Errorf("worker pid %d should be gone after the shutdown-endpoint stop", pid)
	}
}

// TestUpTUI_SigtermQuitsAndStops proves an external SIGTERM (Ctrl-C's
// non-interactive cousin -- e.g. a process manager stopping prox) quits the
// TUI and stops the supervisor exactly like 'q' or the shutdown endpoint.
func TestUpTUI_SigtermQuitsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := tuiFixtureConfig(t, dir)

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir
	ptmx, buf := startPTY(t, cmd)
	defer ptmx.Close()
	defer killProx(cmd)

	addr := waitForPTYAPIServerURL(t, buf, 15*time.Second)
	waitForAPI(t, addr, 15*time.Second)
	pid := waitForProcessState(t, addr, "worker", "running", 10*time.Second).PID
	waitForPTYContains(t, buf, "worker", 15*time.Second)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal SIGTERM: %v", err)
	}

	if err := waitCmdExit(t, cmd, 15*time.Second); err != nil {
		t.Errorf("up --tui should exit 0 after SIGTERM, got %v", err)
	}
	if !waitForPIDGone(pid, 10*time.Second) {
		t.Errorf("worker pid %d should be gone after the SIGTERM shutdown", pid)
	}
}

// TestAttach_QuitDetachesLeavesDaemonRunning is the counterpart to
// TestUpTUI_QuitStopsSupervisor: 'q' on `prox attach` only detaches the TUI --
// the daemon and its processes must survive, since attach supervises nothing
// (ShutdownCh is nil in the attach call site, internal/cli/commands.go).
func TestAttach_QuitDetachesLeavesDaemonRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := tuiFixtureConfig(t, dir)

	// Start the daemon (prox up -d) in the background.
	up := exec.Command(binary, "up", "-d", "-c", cfg)
	up.Dir = dir
	out, err := up.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(binary, "stop", "-c", cfg)
		stop.Dir = dir
		_, _ = stop.CombinedOutput()
	})

	statePath := filepath.Join(dir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)
	stateData, err := os.ReadFile(statePath)
	requireNoError(t, err, "reading state file")
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state file: %v", err)
	}
	addr := fmt.Sprintf("http://127.0.0.1:%d", state.Port)
	waitForAPI(t, addr, 10*time.Second)
	daemonPID := waitForProcessState(t, addr, "worker", "running", 10*time.Second).PID

	// Attach the client TUI under a PTY. No -c needed: attach discovers the
	// daemon from cwd's .prox/prox.state (internal/cli/commands.go runAttach).
	attachCmd := exec.Command(binary, "attach")
	attachCmd.Dir = dir
	ptmx, buf := startPTY(t, attachCmd)
	defer ptmx.Close()
	defer killProx(attachCmd)

	waitForPTYContains(t, buf, "worker", 15*time.Second)

	writeToPTY(t, ptmx, "q")

	if err := waitCmdExit(t, attachCmd, 15*time.Second); err != nil {
		t.Errorf("attach should exit 0 after 'q', got %v", err)
	}

	// The daemon must still be running: attach quitting is purely a detach.
	resp, err := http.Get(addr + "/api/v1/status")
	requireNoError(t, err, "GET /api/v1/status after attach quit")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected the daemon to still respond 200 after attach quit, got %d", resp.StatusCode)
	}
	if !processAlive(daemonPID) {
		t.Errorf("worker pid %d should still be alive -- attach quit must not stop the daemon's processes", daemonPID)
	}
}

// TestUpTUI_StdinDevNullRefused covers the isatty(stdin) half of the D6
// interactive-terminal guard (internal/cli/commands.go isInteractiveStdio):
// stdout is a real pty (interactive) but stdin is /dev/null (not a
// terminal), so `up --tui` must still refuse before starting anything.
// TestUpTUI_NonInteractiveRefusesToStart in up_test.go covers the piped
// stdout half; neither alone proves isInteractiveStdio checks BOTH streams.
func TestUpTUI_StdinDevNullRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched.marker")
	cfg := filepath.Join(dir, "prox.yaml")
	cfgBody := "api:\n  host: 127.0.0.1\n\nprocesses:\n  marker:\n    cmd: touch " + marker + " && sleep 30\n"
	if err := os.WriteFile(cfg, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	devNull, err := os.Open(os.DevNull)
	requireNoError(t, err, "opening /dev/null")
	defer devNull.Close()

	// Built directly from pty.Open rather than pty.Start: the convenience
	// starters assign the SAME tty to stdin/stdout/stderr, which is exactly
	// what this test must NOT do. Assign the slave to stdout/stderr only and
	// leave stdin as the already-opened /dev/null file.
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	defer ptyMaster.Close()

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir
	cmd.Stdin = devNull
	cmd.Stdout = ptySlave
	cmd.Stderr = ptySlave

	if err := cmd.Start(); err != nil {
		_ = ptySlave.Close()
		t.Skipf("PTY unavailable: %v", err)
	}
	_ = ptySlave.Close() // the child has its own dup; the parent doesn't need it
	defer killProx(cmd)

	buf := &syncBuffer{}
	go func() {
		_, _ = io.Copy(buf, ptyMaster)
	}()

	// The guard runs before any startup work, so this is near-instant; the
	// budget is generous only to absorb a loaded CI machine.
	if err := waitCmdExit(t, cmd, 15*time.Second); err == nil {
		t.Fatalf("expected a non-zero exit with stdin=/dev/null, got nil error; output:\n%s", buf.String())
	}

	if !strings.Contains(buf.String(), "--tui requires an interactive terminal") {
		t.Errorf("expected the interactive-terminal guard message, got:\n%s", buf.String())
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

// TestUpTUI_TermDumbRefused pins terminal-hostability condition 2 (plan 026
// C3): isatty says yes on a pty whose TERM is "dumb", but bubbletea has no
// capabilities to draw with there, so a REQUIRED TUI (an explicit `--tui`)
// must refuse. The message has to name TERM rather than reuse condition 1's
// "requires an interactive terminal" -- on a real pty that would be flatly
// untrue and would send the user hunting for the wrong problem.
//
// This is the negative case startPTY's TERM pin exists to keep separable.
func TestUpTUI_TermDumbRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY-driven TUI tests are unix-only")
	}
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched.marker")
	cfg := filepath.Join(dir, "prox.yaml")
	cfgBody := "api:\n  host: 127.0.0.1\n\nprocesses:\n  marker:\n    cmd: touch " + marker + " && sleep 30\n"
	if err := os.WriteFile(cfg, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cmd := exec.Command(binary, "up", "--tui", "-c", cfg)
	cmd.Dir = dir

	ptmx, buf := startPTYWithTERM(t, cmd, "dumb")
	defer ptmx.Close()
	defer killProx(cmd)

	if err := waitCmdExit(t, cmd, 15*time.Second); err == nil {
		t.Fatalf("expected a non-zero exit with TERM=dumb, got nil error; output:\n%s", buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "TERM=dumb") {
		t.Errorf("expected an error naming TERM, got:\n%s", out)
	}
	if strings.Contains(out, "requires an interactive terminal") {
		t.Errorf("stdin/stdout ARE a terminal here; condition 1's message would be misleading. Got:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the TERM guard must refuse before starting processes, but the marker exists (stat err=%v)", err)
	}
}
