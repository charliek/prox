package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests cover plan 020 C3: every client command verifies that the prox
// answering on the address it discovered from .prox/prox.state actually belongs
// to the directory it was run from.
//
// The false-refusal cases matter more than the refusal cases. A wrong refusal
// locks a user out of their own project, and the paths a daemon and a client
// record for "the same" directory or config legitimately differ in FORM all the
// time: `-c $(pwd)/prox.yaml` vs a bare `prox status`, a symlinked project
// root, /tmp vs /private/tmp on Darwin, prox.yml vs prox.yaml.

// runProxIn runs a prox subcommand to completion with cwd set to dir (NOT the
// repo root, unlike runProx) and returns its combined output and exit code.
// exec.Cmd{Dir: ...} leaves $PWD alone, so the child's os.Getwd() reports the
// SYMLINK-RESOLVED path — which is precisely the divergence os.SameFile has to
// absorb, and which every coding agent and script produces.
func runProxIn(t *testing.T, binary, dir string, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to run prox %v in %s: %v", args, dir, err)
		}
	}
	return string(out), exitCode
}

// startDaemonIn starts a detached prox in dir with the given extra args and
// returns the API address it bound. It registers a best-effort shutdown so a
// failing assertion never strands a daemon.
func startDaemonIn(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()

	full := append([]string{"up", "-d"}, args...)
	cmd := exec.Command(binary, full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon in %s: %v\noutput: %s", dir, err, out)
	}

	statePath := filepath.Join(dir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)
	addr := "http://" + net.JoinHostPort(readStateHost(t, statePath), strconv.Itoa(readStatePort(t, statePath)))
	waitForAPI(t, addr, 10*time.Second)

	t.Cleanup(func() { _ = stopProx(t, addr) })
	return addr
}

// projectState is the subset of daemon.State these tests read, decoded without
// importing the internal package (matching the convention in this directory).
type projectState struct {
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	Host       string `json:"host"`
	ConfigFile string `json:"config_file"`
	StartedAt  string `json:"started_at"`
}

func readState(t *testing.T, path string) projectState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading state file %s: %v", path, err)
	}
	var st projectState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parsing state file %s: %v", path, err)
	}
	return st
}

func readStatePort(t *testing.T, path string) int { return readState(t, path).Port }

func readStateHost(t *testing.T, path string) string {
	t.Helper()
	host := readState(t, path).Host
	if host == "" {
		host = "127.0.0.1"
	}
	return host
}

// writeState writes a hand-built .prox/prox.state into dir. Tests use it to
// forge the stale/cross-project state files that C3 exists to catch.
func writeState(t *testing.T, dir string, st projectState) {
	t.Helper()
	stateDir := filepath.Join(dir, ".prox")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("creating %s: %v", stateDir, err)
	}
	if st.StartedAt == "" {
		st.StartedAt = time.Now().Format(time.RFC3339Nano)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("marshaling state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "prox.state"), data, 0600); err != nil {
		t.Fatalf("writing state: %v", err)
	}
}

// writeProjectConfig writes a minimal, dynamic-port prox.yaml (or prox.yml)
// into dir and returns its path.
func writeProjectConfig(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "processes:\n  sleeper:\n    cmd: \"while true; do sleep 1; done\"\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// statusPID fetches the daemon's own pid from its state file, so a refusal test
// can assert the OWNER SURVIVED rather than merely that the caller exited 1.
func daemonPID(t *testing.T, dir string) int {
	t.Helper()
	return readState(t, filepath.Join(dir, ".prox", "prox.state")).PID
}

// processPID reads a supervised process's pid straight from the owning
// daemon's API. Guarding the daemon pid alone is not enough: a `restart` or
// `stop` that reached the wrong project would replace or kill the CHILD while
// leaving the daemon untouched. Returns 0 when the process is absent or has no
// pid (i.e. not running), which is itself a detectable change.
func processPID(t *testing.T, addr, name string) int {
	t.Helper()

	resp, err := http.Get(addr + "/api/v1/processes")
	if err != nil {
		t.Fatalf("fetching processes from %s: %v", addr, err)
	}
	defer resp.Body.Close()

	var list ProcessListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decoding processes from %s: %v", addr, err)
	}
	for _, p := range list.Processes {
		if p.Name == name {
			return p.PID
		}
	}
	return 0
}

// TestOwnership_StaleStateRefusesAndOwnerSurvives is the destructive case that
// motivated C3. Project B's state file points at the port project A's prox is
// listening on (a stale file, or two projects pinning one api.port). Every
// client command run from B must refuse, name A as the owner, and — the real
// assertion — leave A's daemon and processes untouched.
func TestOwnership_StaleStateRefusesAndOwnerSurvives(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	ownerDir := t.TempDir()
	writeProjectConfig(t, ownerDir, "prox.yaml")
	ownerAddr := startDaemonIn(t, binary, ownerDir)
	ownerPID := daemonPID(t, ownerDir)
	ownerState := readState(t, filepath.Join(ownerDir, ".prox", "prox.state"))

	// The SUPERVISED CHILD's pid, not just the daemon's. `restart sleeper` and
	// `stop sleeper` reaching the wrong project would replace or kill this
	// process while leaving the daemon itself perfectly alive, so asserting only
	// on the daemon pid would miss exactly the damage those two commands do
	// (codex review finding).
	sleeperPID := processPID(t, ownerAddr, "sleeper")
	if sleeperPID <= 0 {
		t.Fatalf("owner's sleeper process has no pid to guard (got %d)", sleeperPID)
	}

	// Project B: a state file forged to point at A's port.
	otherDir := t.TempDir()
	writeProjectConfig(t, otherDir, "prox.yaml")
	writeState(t, otherDir, projectState{
		PID:        ownerState.PID,
		Port:       ownerState.Port,
		Host:       ownerState.Host,
		ConfigFile: filepath.Join(otherDir, "prox.yaml"),
	})

	for _, args := range [][]string{{"status"}, {"down"}, {"restart", "sleeper"}, {"logs"}, {"stop", "sleeper"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, code := runProxIn(t, binary, otherDir, args...)
			if code == 0 {
				t.Fatalf("prox %v from a foreign project must fail, got exit 0:\n%s", args, out)
			}
			if !strings.Contains(out, "not running for this project") {
				t.Errorf("prox %v should refuse with an ownership message, got:\n%s", args, out)
			}
			if !strings.Contains(out, "--addr "+ownerAddr) {
				t.Errorf("prox %v should offer '--addr %s' as the escape hatch, got:\n%s", args, ownerAddr, out)
			}

			// The owner must be completely unaffected.
			if !processAlive(ownerPID) {
				t.Fatalf("prox %v from a foreign project KILLED the owning daemon (pid %d)", args, ownerPID)
			}
			// ...and its supervised child must be the SAME process, neither
			// killed (stop) nor replaced by a restart.
			if got := processPID(t, ownerAddr, "sleeper"); got != sleeperPID {
				t.Fatalf("prox %v from a foreign project disturbed the owner's supervised process: pid %d -> %d",
					args, sleeperPID, got)
			}
		})
	}

	// The owner's own view is still healthy and its state file untouched.
	out, code := runProxIn(t, binary, ownerDir, "status")
	if code != 0 {
		t.Fatalf("the owning project's own status broke: exit %d\n%s", code, out)
	}
	if got := readState(t, filepath.Join(ownerDir, ".prox", "prox.state")); got.PID != ownerPID {
		t.Errorf("owner state file changed: pid %d -> %d", ownerPID, got.PID)
	}
}

// TestOwnership_SharedConfigTwoRootsCannotControlEachOther is the false-ALLOW
// guard. Two project roots started from ONE config file (`prox up -c
// ../shared/prox.yaml`) report identical config_file values, so a config-path
// identity basis would let each control the other. The project directory is
// authoritative, so B must still be refused.
func TestOwnership_SharedConfigTwoRootsCannotControlEachOther(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	sharedDir := t.TempDir()
	sharedCfg := writeProjectConfig(t, sharedDir, "prox.yaml")

	ownerDir := t.TempDir()
	startDaemonIn(t, binary, ownerDir, "-c", sharedCfg)
	ownerPID := daemonPID(t, ownerDir)
	ownerState := readState(t, filepath.Join(ownerDir, ".prox", "prox.state"))

	// B: same shared config, state file pointing at A's port.
	otherDir := t.TempDir()
	writeState(t, otherDir, projectState{
		PID:        ownerState.PID,
		Port:       ownerState.Port,
		Host:       ownerState.Host,
		ConfigFile: sharedCfg, // identical to A's — a config comparison would ALLOW
	})

	out, code := runProxIn(t, binary, otherDir, "-c", sharedCfg, "down")
	if code == 0 {
		t.Fatalf("a second root sharing one config must not be able to stop the first:\n%s", out)
	}
	if !strings.Contains(out, "not running for this project") {
		t.Errorf("expected an ownership refusal, got:\n%s", out)
	}
	if !processAlive(ownerPID) {
		t.Fatalf("the shared-config sibling STOPPED the owning daemon (pid %d)", ownerPID)
	}
}

// TestOwnership_ExplicitConfigFormsAllowed is the plainest false-refusal case:
// the daemon was started with an absolute `-c $(pwd)/prox.yaml`, the client is
// run with no -c at all. Those two never see the same config string, which is
// exactly why identity is the project directory and not the config path.
func TestOwnership_ExplicitConfigFormsAllowed(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := writeProjectConfig(t, dir, "prox.yaml")

	startDaemonIn(t, binary, dir, "-c", cfg)

	for _, args := range [][]string{{"status"}, {"logs"}, {"status", "--json"}} {
		out, code := runProxIn(t, binary, dir, args...)
		if code != 0 {
			t.Fatalf("prox %v in its own project must succeed, got exit %d:\n%s", args, code, out)
		}
	}
}

// TestOwnership_ProxYmlAllowed pins that a project using prox.yml rather than
// prox.yaml is not refused. The daemon must be told about it explicitly (-c),
// the client is not — another config-form divergence the project-directory
// basis makes irrelevant.
func TestOwnership_ProxYmlAllowed(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	cfg := writeProjectConfig(t, dir, "prox.yml")

	startDaemonIn(t, binary, dir, "-c", cfg)

	out, code := runProxIn(t, binary, dir, "status")
	if code != 0 {
		t.Fatalf("a prox.yml project must not be refused, got exit %d:\n%s", code, out)
	}
}

// TestOwnership_SymlinkedProjectRootAllowed reaches the very same project
// through a symlinked root. The daemon records one path form, the client's cwd
// resolves to another; os.SameFile must collapse them.
func TestOwnership_SymlinkedProjectRootAllowed(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	real := t.TempDir()
	writeProjectConfig(t, real, "prox.yaml")
	startDaemonIn(t, binary, real)

	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out, code := runProxIn(t, binary, link, "status")
	if code != 0 {
		t.Fatalf("reaching the project through a symlinked root must not be refused, got exit %d:\n%s", code, out)
	}
}

// TestOwnership_TmpVsPrivateTmpAllowed is the Darwin /tmp -> /private/tmp case.
// The daemon is started from a shell-style $PWD of /tmp/... while the client is
// run via exec.Cmd{Dir: ...}, whose os.Getwd() reports /private/tmp/...
func TestOwnership_TmpVsPrivateTmpAllowed(t *testing.T) {
	skipShort(t)
	if runtime.GOOS != "darwin" {
		t.Skip("/tmp is a symlink to /private/tmp only on Darwin")
	}

	binary := buildBinary(t)
	dir, err := os.MkdirTemp("/tmp", "prox-c3-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	writeProjectConfig(t, dir, "prox.yaml")

	// Start the daemon with $PWD pinned to the /tmp form, the way a shell would.
	cmd := exec.Command(binary, "up", "-d")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PWD="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting daemon: %v\n%s", err, out)
	}
	statePath := filepath.Join(dir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)
	addr := "http://" + net.JoinHostPort(readStateHost(t, statePath), strconv.Itoa(readStatePort(t, statePath)))
	waitForAPI(t, addr, 10*time.Second)
	t.Cleanup(func() { _ = stopProx(t, addr) })

	// The client gets no $PWD, so its cwd resolves to /private/tmp/...
	out, code := runProxIn(t, binary, dir, "status")
	if code != 0 {
		t.Fatalf("/tmp vs /private/tmp must not be refused, got exit %d:\n%s", code, out)
	}
}

// TestOwnership_ExplicitAddrBypassesEveryClientCommand pins --addr as a real
// escape hatch for the whole clientCommands allowlist. It is what the ownership
// refusal tells users to run, so it must work from a directory with no state of
// its own — including for `attach`, which used to check local daemon state and
// bail with "prox is not running" BEFORE it ever looked at --addr.
func TestOwnership_ExplicitAddrBypassesEveryClientCommand(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	ownerDir := t.TempDir()
	writeProjectConfig(t, ownerDir, "prox.yaml")
	addr := startDaemonIn(t, binary, ownerDir)

	// A neutral directory: no prox.yaml, no .prox, nothing to discover.
	elsewhere := t.TempDir()

	// Every allowlisted command except attach (a TUI; covered below) and down
	// (destructive; covered in its own test). Ordered so each lifecycle call is
	// legal against the state the previous one left behind.
	//
	// `requests` is expected to fail: this project runs without a proxy, so the
	// daemon answers PROXY_NOT_ENABLED. That IS the proof --addr worked — the
	// command reached the daemon and got a daemon-side verdict rather than a
	// discovery error — so it is asserted on the message, not the exit code.
	cases := []struct {
		args      []string
		wantExit0 bool
	}{
		{[]string{"status"}, true},
		{[]string{"logs"}, true},
		{[]string{"requests"}, false},
		{[]string{"stop", "sleeper"}, true},
		{[]string{"start", "sleeper"}, true},
		{[]string{"restart", "sleeper"}, true},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			full := append(append([]string{}, tc.args...), "--addr", addr)
			out, code := runProxIn(t, binary, elsewhere, full...)
			if tc.wantExit0 && code != 0 {
				t.Errorf("prox %v --addr should succeed, got exit %d:\n%s", tc.args, code, out)
			}
			if strings.Contains(out, "no running prox instance") || strings.Contains(out, "not running for this project") {
				t.Errorf("prox %v --addr must not consult local state, got:\n%s", tc.args, out)
			}
		})
	}

	// attach: without a TTY the TUI cannot run, but it must get PAST discovery
	// and the daemon handshake first. The old code returned "prox is not
	// running" from daemon.GetRunningState before consulting --addr at all;
	// that specific message must be gone.
	t.Run("attach", func(t *testing.T) {
		out, _ := runProxIn(t, binary, elsewhere, "attach", "--addr", addr)
		if strings.Contains(out, "prox is not running") {
			t.Errorf("attach --addr must not fail on local daemon state, got:\n%s", out)
		}
		if strings.Contains(out, "no running prox instance") {
			t.Errorf("attach --addr must bypass discovery, got:\n%s", out)
		}
	})
}

// TestOwnership_ExplicitAddrDownFromForeignDir completes the escape hatch: the
// refusal message tells the user to re-run with --addr, and they will typically
// do that from the directory they were refused in — which has its own (stale)
// state file. `prox down --addr` must not then wait on THAT directory's files
// and turn a clean remote stop into a bogus "shutdown incomplete".
func TestOwnership_ExplicitAddrDownFromForeignDir(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)

	ownerDir := t.TempDir()
	writeProjectConfig(t, ownerDir, "prox.yaml")
	addr := startDaemonIn(t, binary, ownerDir)
	ownerState := readState(t, filepath.Join(ownerDir, ".prox", "prox.state"))

	otherDir := t.TempDir()
	writeProjectConfig(t, otherDir, "prox.yaml")
	writeState(t, otherDir, projectState{
		PID:        ownerState.PID,
		Port:       ownerState.Port,
		Host:       ownerState.Host,
		ConfigFile: filepath.Join(otherDir, "prox.yaml"),
	})

	start := time.Now()
	out, code := runProxIn(t, binary, otherDir, "down", "--addr", addr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("down --addr from a foreign dir should exit 0, got %d after %s:\n%s", code, elapsed, out)
	}
	if elapsed >= 10*time.Second {
		t.Errorf("down --addr took %s; it must not wait out this directory's unrelated state files", elapsed)
	}
	if processAlive(ownerState.PID) {
		// Give the daemon a beat to finish exiting before declaring failure.
		time.Sleep(time.Second)
		if processAlive(ownerState.PID) {
			t.Errorf("down --addr did not stop the targeted daemon (pid %d):\n%s", ownerState.PID, out)
		}
	}
}

// TestOwnership_UnrelatedServiceIsNamedNotAProx pins the message boundary: a
// squatter on the recorded port must be reported as "not a prox", not as "no
// running prox instance" — the latter sends the user off debugging their own
// project instead of the service holding the port.
func TestOwnership_UnrelatedServiceIsNamedNotAProx(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	writeProjectConfig(t, dir, "prox.yaml")

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "<html>definitely not a prox</html>")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	writeState(t, dir, projectState{
		PID:        os.Getpid(),
		Port:       ln.Addr().(*net.TCPAddr).Port,
		Host:       "127.0.0.1",
		ConfigFile: filepath.Join(dir, "prox.yaml"),
	})

	out, code := runProxIn(t, binary, dir, "status")
	if code == 0 {
		t.Fatalf("a squatting service must not be treated as this project's prox:\n%s", out)
	}
	if !strings.Contains(out, "not a prox") {
		t.Errorf("expected a 'not a prox' message, got:\n%s", out)
	}
	if strings.Contains(out, "no running prox instance") {
		t.Errorf("must not degrade to 'no running prox instance', got:\n%s", out)
	}
}

// TestOwnership_DeadPIDStateFile covers part C. A state file whose recorded PID
// is dead is NOT grounds to skip the probe (the port may have been reassigned
// to a live prox), but when nothing answers there the user gets the ordinary
// "not running" outcome rather than a raw dial error.
func TestOwnership_DeadPIDStateFile(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	writeProjectConfig(t, dir, "prox.yaml")

	// A port that is (almost certainly) free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	writeState(t, dir, projectState{
		PID:        999999, // not a live process
		Port:       port,
		Host:       "127.0.0.1",
		ConfigFile: filepath.Join(dir, "prox.yaml"),
	})

	out, code := runProxIn(t, binary, dir, "status")
	if code == 0 {
		t.Fatalf("a stale state file must not resolve, got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "no running prox instance") {
		t.Errorf("expected the ordinary 'not running' outcome, got:\n%s", out)
	}
	if !strings.Contains(out, strconv.Itoa(port)) {
		t.Errorf("expected the address it tried to be named, got:\n%s", out)
	}
}

// TestOwnership_StatusReportsProjectDir pins the wire contract the whole check
// rests on: GET /status reports project_dir, and it is the directory the daemon
// wrote .prox/prox.state into.
func TestOwnership_StatusReportsProjectDir(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	dir := t.TempDir()
	writeProjectConfig(t, dir, "prox.yaml")
	addr := startDaemonIn(t, binary, dir)

	resp, err := http.Get(addr + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var status struct {
		APIVersion string `json:"api_version"`
		ProjectDir string `json:"project_dir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if status.APIVersion == "" {
		t.Error("api_version must be present: it is the positive marker clients use to tell a prox from a squatter")
	}
	if status.ProjectDir == "" {
		t.Fatal("project_dir must be reported")
	}

	daemonDir, err := os.Stat(status.ProjectDir)
	if err != nil {
		t.Fatalf("stat %q: %v", status.ProjectDir, err)
	}
	wantDir, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %q: %v", dir, err)
	}
	if !os.SameFile(daemonDir, wantDir) {
		t.Errorf("project_dir %q is not the project directory %q", status.ProjectDir, dir)
	}
}
