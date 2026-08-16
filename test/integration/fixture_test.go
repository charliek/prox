package integration

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is the isolated integration harness (plan 027 workstream A).
//
// Before it, every helper that launched the real binary ran it with
// cmd.Dir = <repo root>. Every such test therefore shared ONE .prox/ state
// directory (PID file, state file, children ledger, daemon log) and ONE pinned
// API port, 15555. Two tests that overlapped by even a few milliseconds fought
// over the PID-file lock and the port, and the loser reported "API did not
// become ready within 20s" -- a message that describes neither the cause nor
// the test that caused it. Measured on unmodified main, four tests failed that
// way in a single `make test` run.
//
// proxFixture gives each test its own working directory, so its .prox/ is
// private by construction, and its rendered config carries no api: block, so
// prox allocates a dynamic API port and the test reads it back from that
// private state file (proxRun.Addr). Nothing is shared, so nothing can collide.

// testdataPrefix is how the checked-in fixture configs spell paths to the
// helper scripts: relative to the repo root, because that is where prox used
// to run. prox has no per-process `dir:` key -- processes inherit the daemon's
// cwd (internal/config/config.go) -- so once the daemon runs in a temp dir
// these have to become absolute or every process would fail to exec.
const testdataPrefix = "./testdata/"

// stubbornPortKey is the env var testdata/scripts/stubborn_grandchild.sh reads
// to decide which TCP port its deliberately-SIGTERM-ignoring python grandchild
// binds. It is pinned to 15561 in the checked-in config, which is exactly the
// port that strands itself across an interrupted test run (see CLAUDE.md).
// Rendering replaces it with a per-fixture port.
const stubbornPortKey = "STUBBORN_PORT"

// fixtureOpt customizes a fixture's config after the standard rendering rules
// have been applied and before it is written. It receives the top-level
// mapping node of the parsed document.
type fixtureOpt func(t *testing.T, f *proxFixture, body *yaml.Node)

// proxFixture is one test's private working directory: the cwd that every prox
// process and every prox CLI command belonging to that test runs in, and
// therefore the owner of a .prox/ state directory no other test can see.
type proxFixture struct {
	t          *testing.T
	dir        string // the prox process's cwd; .prox/ lives here
	configPath string // <dir>/prox.yaml

	mu           sync.Mutex
	stubbornPort int // allocated lazily, once, by StubbornPort
}

// newFixture renders testdata/configs/<name>.yaml into a fresh private
// directory and returns the fixture that owns it.
func newFixture(t *testing.T, name string, opts ...fixtureOpt) *proxFixture {
	t.Helper()

	src := filepath.Join(repoRoot(t), "testdata", "configs", name+".yaml")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture config %s: %v", src, err)
	}
	return newInlineFixture(t, string(raw), opts...)
}

// newInlineFixture materializes a config given as a string -- the primitive
// newFixture is built on -- applying the same rendering rules either way, so an
// inline config and a checked-in one behave identically.
func newInlineFixture(t *testing.T, doc string, opts ...fixtureOpt) *proxFixture {
	t.Helper()

	// EvalSymlinks is load-bearing, not tidiness. On macOS t.TempDir() hands
	// back /var/folders/..., while the daemon that runs there reports its own
	// os.Getwd() as the resolved /private/var/folders/... Any assertion that
	// compares a prox-reported path against fixture.dir would then fail on the
	// macOS leg only. internal/cli/root_test.go resolves temp dirs for the same
	// reason.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}

	f := &proxFixture{t: t, dir: dir, configPath: filepath.Join(dir, "prox.yaml")}
	f.writeConfig(t, doc, opts...)
	return f
}

// Rewrite replaces the fixture's config in place, at the same path, so reload
// and restart tests can mutate the config a running daemon is watching.
func (f *proxFixture) Rewrite(t *testing.T, doc string, opts ...fixtureOpt) {
	t.Helper()
	f.writeConfig(t, doc, opts...)
}

func (f *proxFixture) writeConfig(t *testing.T, doc string, opts ...fixtureOpt) {
	t.Helper()
	if err := os.WriteFile(f.configPath, f.render(t, doc, opts...), 0o644); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}
}

// render applies the two rendering rules to a config document.
//
// It edits the parsed YAML rather than the text, because a textual
// search-and-replace cannot tell an `api:` mapping from a process named `api`
// (testdata/configs/expanded.yaml has one), nor a path inside a command from
// the same characters inside a comment.
func (f *proxFixture) render(t *testing.T, doc string, opts ...fixtureOpt) []byte {
	t.Helper()

	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("parse fixture config: %v\n%s", err, doc)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		t.Fatalf("fixture config has no document node:\n%s", doc)
	}
	body := root.Content[0]
	if body.Kind != yaml.MappingNode {
		t.Fatalf("fixture config is not a mapping:\n%s", doc)
	}

	// Rule 1: drop api: entirely, so prox allocates a dynamic port
	// (internal/cli/up.go) instead of every fixture fighting over one pinned
	// one. The address comes back out of the private state file; see
	// proxRun.Addr.
	dropMappingKey(body, "api")

	// Rule 2: repoint script paths (and the stubborn listener's port) now that
	// the daemon's cwd is this fixture's directory rather than the repo root.
	f.rewriteValues(body)

	for _, opt := range opts {
		opt(t, f, body)
	}

	out, err := yaml.Marshal(body)
	if err != nil {
		t.Fatalf("render fixture config: %v", err)
	}
	return out
}

// dropMappingKey removes key and its value from a mapping node, if present.
func dropMappingKey(m *yaml.Node, key string) {
	if m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i:i], m.Content[i+2:]...)
			return
		}
	}
}

// rewriteValues walks the document and rewrites the scalars that only made
// sense when prox ran from the repo root.
func (f *proxFixture) rewriteValues(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == stubbornPortKey && v.Kind == yaml.ScalarNode {
				v.SetString(strconv.Itoa(f.StubbornPort()))
				v.Style = yaml.DoubleQuotedStyle
				continue
			}
			f.rewriteValues(v)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			f.rewriteValues(c)
		}
	case yaml.ScalarNode:
		if strings.Contains(n.Value, testdataPrefix) {
			n.SetString(strings.ReplaceAll(n.Value, testdataPrefix, filepath.Join(repoRoot(f.t), "testdata")+"/"))
		}
	}
}

// StubbornPort returns this fixture's port for the stubborn-grandchild
// scripts, allocating it once on first use.
//
// This one genuinely has to bind-and-close: the number is baked into a config
// file that a shell script binds seconds later, so there is nothing to hand a
// held listener to. The API port does NOT work this way -- see proxRun.Addr.
func (f *proxFixture) StubbornPort() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stubbornPort == 0 {
		port, reservation := freePort(f.t)
		if err := reservation.Close(); err != nil {
			f.t.Fatalf("release reserved stubborn port %d: %v", port, err)
		}
		f.stubbornPort = port
	}
	return f.stubbornPort
}

// Run runs a prox subcommand to completion IN THIS FIXTURE'S DIRECTORY and
// returns its combined output and exit code.
//
// Running CLI commands here is mandatory, not a convenience. Every client
// command discovers its API address from the cwd's .prox/prox.state
// (rootPersistentPreRunE -> discoverAPIAddress) and then probes the daemon to
// confirm it belongs to this directory. Run from anywhere else, a command
// either finds no state file at all or is refused as owned by another project.
func (f *proxFixture) Run(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Dir = f.dir
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to run prox %v: %v", args, err)
		}
	}
	return string(out), exitCode
}

// Start launches prox in this fixture's directory and returns the run handle.
func (f *proxFixture) Start(t *testing.T, binary string, args ...string) *proxRun {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Dir = f.dir
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}

	r := &proxRun{
		t:       t,
		cmd:     cmd,
		fixture: f,
		stdout:  stdout,
		stderr:  stderr,
		exited:  make(chan struct{}),
	}
	// The ONE Wait for this process, started here and never called again from
	// anywhere else. waitErr is written before exited is closed, so every
	// reader that reads it after receiving from the channel is ordered behind
	// this write.
	go func() {
		r.waitErr = cmd.Wait()
		close(r.exited)
	}()

	// Registered after t.TempDir()'s own cleanup, so LIFO ordering kills the
	// process before the directory it is running in is removed.
	t.Cleanup(r.Kill)

	return r
}

// proxRun is a launched prox process whose Cmd.Wait is owned by exactly one
// goroutine.
//
// The idiom it replaces had two: a `defer killProx(cmd)` in the test plus a
// waitCmdExit helper that spawned its own `go cmd.Wait()`. On a timeout
// waitCmdExit called t.Fatalf, which runs the deferred killProx -- a SECOND,
// concurrent Cmd.Wait on the same command. exec.Cmd.Wait is documented as not
// safe for concurrent use. Here the waiter is started once at launch and both
// WaitExit and Kill only OBSERVE the channel it closes, so a second Wait is
// not reachable at all.
type proxRun struct {
	t              *testing.T
	cmd            *exec.Cmd
	fixture        *proxFixture
	stdout, stderr *syncBuffer
	exited         chan struct{} // closed by the single waiter goroutine
	waitErr        error         // written before exited is closed

	mu   sync.Mutex
	addr string // memoized by Addr
}

// WaitExit waits for the process to exit within timeout and returns the error
// from its single Cmd.Wait (nil on a clean exit). On timeout it kills the
// process and fails the test.
func (r *proxRun) WaitExit(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-r.exited:
		return r.waitErr
	case <-time.After(timeout):
		r.Kill()
		t.Fatalf("process did not exit within %v; output:\n%s", timeout, r.Output())
		return nil // unreachable; t.Fatalf stops the test
	}
}

// Kill terminates the process if it is still running, then waits for the
// single waiter goroutine to reap it. Safe to call repeatedly and after the
// process has already exited.
func (r *proxRun) Kill() {
	select {
	case <-r.exited:
		return
	default:
	}

	// Signal-0 probe before signalling, mirroring killIfAlive: never send a
	// signal to a pid that has already been reaped and (vanishingly rarely)
	// reused by an unrelated process.
	if r.cmd.Process != nil && processAlive(r.cmd.Process.Pid) {
		_ = r.cmd.Process.Kill()
	}

	select {
	case <-r.exited:
	case <-time.After(killReapTimeout):
	}
}

// killReapTimeout bounds how long Kill waits for the reaper after SIGKILL.
// SIGKILL cannot be caught, so overshooting this means something is wrong with
// the wait, not with the process; the bound only keeps a cleanup from hanging
// the whole package.
const killReapTimeout = 10 * time.Second

// Output returns the combined stdout and stderr captured so far.
func (r *proxRun) Output() string {
	return r.stdout.String() + r.stderr.String()
}

// StateDir returns this run's private .prox directory.
func (r *proxRun) StateDir() string {
	return filepath.Join(r.fixture.dir, daemonStateDirName)
}

// Addr returns the API base URL, read from this run's own state file.
//
// The state file is the only race-free source. A bind-and-close probe for a
// "free" port would reintroduce exactly the TOCTOU this harness exists to
// remove; here prox picks the port, writes it down, and the test reads what
// prox actually chose. TestDaemonMode_DynamicPort already relies on this.
//
// The result is memoized because the daemon deletes its state file on
// shutdown, so a late caller (a post-shutdown assertion, say) must still get
// the address rather than a timeout.
func (r *proxRun) Addr() string {
	r.t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addr != "" {
		return r.addr
	}

	statePath := filepath.Join(r.StateDir(), daemonStateFileName)
	deadline := time.Now().Add(apiReadyTimeout)
	for {
		// The state file is written with O_TRUNC rather than a rename, so a
		// read can legitimately catch it half-written; treat any parse failure
		// or zero port as "not yet" and retry.
		if data, err := os.ReadFile(statePath); err == nil {
			var state struct {
				Port int    `json:"port"`
				Host string `json:"host"`
			}
			if json.Unmarshal(data, &state) == nil && state.Port > 0 {
				host := state.Host
				if host == "" {
					host = "127.0.0.1"
				}
				r.addr = "http://" + net.JoinHostPort(host, strconv.Itoa(state.Port))
				return r.addr
			}
		}

		// Report the real failure rather than waiting out the full budget and
		// then blaming the API: if prox is already gone it is never going to
		// write the file, and its output says why.
		select {
		case <-r.exited:
			r.t.Fatalf("prox exited (%v) before writing %s; output:\n%s", r.waitErr, statePath, r.Output())
		default:
		}

		if !time.Now().Before(deadline) {
			r.t.Fatalf("prox did not write %s within %v; output:\n%s", statePath, apiReadyTimeout, r.Output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// daemonStateDirName / daemonStateFileName mirror internal/daemon's constants,
// which this external test package deliberately does not import.
const (
	daemonStateDirName  = ".prox"
	daemonStateFileName = "prox.state"
)

// repoRoot returns the fully resolved repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(projectRoot(t))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("resolve repo root symlinks: %v", err)
	}
	return resolved
}
