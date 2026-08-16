package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/prox/internal/daemon"
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

	ctx, cancel := cliContext(t)
	defer cancel()
	cmd := boundedCommand(ctx, f.dir, binary, args...)
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

// startOpt customizes a launch after its exec.Cmd has been built with this
// fixture's directory and the default stdio wiring, and before the process is
// actually started.
//
// It exists so that a test needing unusual stdio -- above all the pty tests,
// which must hand prox a real controlling terminal -- can supply that wiring
// without owning the process. Cmd.Wait stays in StartWith, called exactly once
// per launch: a test that starts its own process is a test that must wait for
// it, and that is precisely how the concurrent-Wait defect got in.
type startOpt func(t *testing.T, l *launch)

// launch is the mutable description of one prox launch, handed to each startOpt
// before the process starts.
type launch struct {
	// cmd is the command about to run. An opt may edit any of its fields --
	// Env and the three stdio streams are the ones that matter -- but must
	// never start or wait on it outside of start below.
	cmd *exec.Cmd
	// out is what proxRun.Output() returns. An opt that rewires stdio must
	// replace it with a buffer it actually feeds, since the default one will
	// then receive nothing (see startPTYRun in tui_pty_test.go).
	out *syncBuffer
	// start starts cmd. It is a field rather than a hardcoded cmd.Start because
	// only pty.StartWithSize allocates a terminal; an opt that replaces it may
	// also skip the test itself (a sandbox that refuses ptys is a skip, not a
	// failure). A returned error is reported by StartWith.
	start func(*exec.Cmd) error
}

// withEnv appends key=value entries to the launch's environment. A nil Env
// means "inherit", so it is materialized first rather than replaced -- a prox
// subprocess with an environment of exactly one variable behaves nothing like
// the one a user runs.
func withEnv(kv ...string) startOpt {
	return func(t *testing.T, l *launch) {
		if l.cmd.Env == nil {
			l.cmd.Env = os.Environ()
		}
		l.cmd.Env = append(l.cmd.Env, kv...)
	}
}

// withAPIPort puts an `api: {port: N}` block back into a config after rendering
// stripped it, for the one test whose subject IS that key. Everything else wants
// the dynamic port rendering gives it.
func withAPIPort(port int) fixtureOpt {
	return func(t *testing.T, f *proxFixture, body *yaml.Node) {
		t.Helper()
		dropMappingKey(body, "api")
		body.Content = append(body.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "api"},
			&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "port"},
				{Kind: yaml.ScalarNode, Value: strconv.Itoa(port)},
			}},
		)
	}
}

// Start launches prox in this fixture's directory and returns the run handle.
func (f *proxFixture) Start(t *testing.T, binary string, args ...string) *proxRun {
	t.Helper()
	return f.StartWith(t, binary, nil, args...)
}

// StartWith is Start with the launch customized by opts -- a different
// environment, different stdio, a different starter -- while the run handle it
// returns still owns the one and only Cmd.Wait for the process.
func (f *proxFixture) StartWith(t *testing.T, binary string, opts []startOpt, args ...string) *proxRun {
	t.Helper()

	cmd, l := buildLaunch(t, binary, f.dir, opts, args...)

	if err := l.start(cmd); err != nil {
		t.Fatalf("failed to start prox: %v", err)
	}

	r := &proxRun{
		t:         t,
		cmd:       cmd,
		dir:       f.dir,
		out:       l.out,
		exited:    make(chan struct{}),
		watchStop: make(chan struct{}),
	}
	// The ONE Wait for this process, started here and never called again from
	// anywhere else. waitErr is written before exited is closed, so every
	// reader that reads it after receiving from the channel is ordered behind
	// this write.
	go func() {
		r.waitErr = cmd.Wait()
		close(r.exited)
	}()

	// Registered after t.TempDir()'s own cleanup, so LIFO ordering tears the
	// daemon down before the directory it is running in is removed.
	t.Cleanup(r.teardown)

	// Resolve (and ledger) the daemon identity in the background, so that a run
	// nobody ever calls Addr on is still recorded before it can be leaked.
	go r.watchIdentity()

	return r
}

// StartDetached runs a DETACHING prox launch -- `up -d` -- to completion in this
// fixture's directory and returns a handle whose daemon identity is the child
// the launcher left behind.
//
// It exists so that no test has to hand-roll this. `prox up -d` forks a child
// and the parent CLI exits as soon as that child is ready (internal/cli/
// daemon_startup.go awaitDaemonStartup), so by the time any cleanup runs the
// launched process is a corpse and the daemon is a pid in .prox/prox.state.
// Every hand-rolled version of this in the suite therefore "cleaned up" by
// posting a fire-and-forget shutdown and sleeping 500ms, which recovers nothing
// when the daemon does not answer.
//
// The returned handle's Output() is the LAUNCHER's combined output (which is
// where "prox started (pid ...)" appears); the daemon's own output goes to
// .prox/prox.log.
func (f *proxFixture) StartDetached(t *testing.T, binary string, args ...string) *proxRun {
	t.Helper()
	return startDetachedIn(t, binary, f.dir, nil, args...)
}

// TryStartDetached is StartDetached for the one caller that must inspect a
// startup failure instead of failing the test on it: the configured-port test
// reserves an ephemeral port and can legitimately lose it to another process
// between releasing the reservation and the daemon's bind, and must retry
// rather than report that as a broken assertion about configured ports.
func (f *proxFixture) TryStartDetached(t *testing.T, binary string, args ...string) (*proxRun, error) {
	t.Helper()
	return tryStartDetachedIn(t, binary, f.dir, nil, args...)
}

// startDetachedIn is StartDetached for a directory that is not a proxFixture --
// the several tests that build their own project directory by hand and then
// start a daemon in it. They get the same two-identity teardown and the same
// ledger entry; nothing about leak-proofing should depend on how the config got
// written.
func startDetachedIn(t *testing.T, binary, dir string, opts []startOpt, args ...string) *proxRun {
	t.Helper()
	r, err := tryStartDetachedIn(t, binary, dir, opts, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return r
}

// tryStartDetachedIn is startDetachedIn that returns the startup failure rather
// than ending the test with it. Teardown and the ledger entry are registered
// either way, so a launcher that fails AFTER its daemon came up still cannot
// leak it.
func tryStartDetachedIn(t *testing.T, binary, dir string, opts []startOpt, args ...string) (*proxRun, error) {
	t.Helper()

	cmd, l := buildLaunch(t, binary, dir, opts, args...)

	r := &proxRun{
		t:         t,
		cmd:       cmd,
		dir:       dir,
		out:       l.out,
		exited:    make(chan struct{}),
		watchStop: make(chan struct{}),
		detached:  true,
	}
	// Registered BEFORE the launcher runs: if the daemon comes up and the
	// launcher then reports a failure, the daemon still gets torn down.
	t.Cleanup(r.teardown)

	// Start + Wait here is the one and only Wait for the launcher, and it is
	// safe to make it inline because the launcher is short-lived by construction
	// -- unlike a foreground run, there is nothing to observe while it lives.
	if err := l.start(cmd); err != nil {
		close(r.exited)
		return nil, fmt.Errorf("failed to start detached prox %v: %w", args, err)
	}
	r.waitErr = cmd.Wait()
	close(r.exited)
	if r.waitErr != nil {
		return nil, fmt.Errorf("failed to start detached prox %v: %w\noutput:\n%s", args, r.waitErr, r.Output())
	}

	// The launcher only exits 0 after the state file names its child AND that
	// child's /health answers, so this resolves on the first read.
	r.DaemonIdentity()
	return r, nil
}

// buildLaunch assembles the exec.Cmd and the launch description shared by every
// start in this harness: the working directory, the one combined output buffer,
// and whatever the opts change before the process runs.
func buildLaunch(t *testing.T, binary, dir string, opts []startOpt, args ...string) (*exec.Cmd, *launch) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	// One buffer for both streams: what a test asserts on is what the terminal
	// would have shown, and interleaving is what a pty gives anyway, so the two
	// wirings produce comparable output.
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

	l := &launch{cmd: cmd, out: out, start: (*exec.Cmd).Start}
	for _, opt := range opts {
		opt(t, l)
	}
	return cmd, l
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
// not reachable at all. TestProxRun_KillDuringWaitExitIsRaceFree holds that
// line: it reproduces the old shape and fails, under -race and on its own
// assertion, the moment a second Wait comes back.
//
// A run also carries the SECOND identity this harness needs: the daemon. For a
// foreground `prox up` the launched process is the daemon, so the two coincide;
// for a detached `up -d` the launcher exits immediately and the daemon is the
// child recorded in .prox/prox.state. Cleanup targets the daemon in both cases
// (see teardown), which is the only version of cleanup that means anything for a
// detached launch.
type proxRun struct {
	t         *testing.T
	cmd       *exec.Cmd
	dir       string        // the launch's working directory; .prox/ lives here
	out       *syncBuffer   // combined stdout+stderr, or the drained pty master
	exited    chan struct{} // closed by the single waiter goroutine
	waitErr   error         // written before exited is closed
	detached  bool          // true for `up -d`: cmd is the launcher, not the daemon
	watchStop chan struct{} // closed by teardown to stop watchIdentity

	mu      sync.Mutex
	ident   daemonIdentity // the daemon, memoized once the state file names it
	identOK bool

	stopOnce sync.Once
}

// WaitExit waits for the process to exit by deadline and returns the error from
// its single Cmd.Wait (nil on a clean exit). On expiry it kills the process and
// fails the test.
func (r *proxRun) WaitExit(t *testing.T, deadline time.Time) error {
	t.Helper()
	start := time.Now()
	select {
	case <-r.exited:
		return r.waitErr
	case <-time.After(time.Until(deadline)):
		r.Kill()
		t.Fatalf("process did not exit %s; output:\n%s", waitedFor(start, deadline), r.Output())
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

// Shutdown asks this run's daemon to stop and WAITS for it to report the
// outcome, failing the test if the request itself fails.
//
// Tests that assert on what a shutdown leaves behind use this rather than a bare
// POST: /api/v1/shutdown without ?wait=true returns as soon as the daemon has
// been ASKED (see the async-return test in up_test.go), so anything asserted
// straight after a bare 200 is a race against the daemon's own cleanup.
func (r *proxRun) Shutdown(t *testing.T) {
	t.Helper()
	id := r.DaemonIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), daemonShutdownBudget)
	defer cancel()
	if err := requestWaitedShutdown(ctx, id.Addr); err != nil {
		t.Fatalf("waited shutdown of %s failed: %v\noutput:\n%s", id, err, r.Output())
	}
}

// Output returns the combined stdout and stderr captured so far.
func (r *proxRun) Output() string {
	return r.out.String()
}

// Signal sends sig to the launched process, failing the test if it cannot.
//
// Tests reach for this instead of the exec.Cmd directly, so that the command
// stays the run handle's business -- the same reason Wait does.
func (r *proxRun) Signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if r.cmd.Process == nil {
		t.Fatalf("cannot signal %v: process was never started", sig)
	}
	if err := r.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("failed to signal %v: %v", sig, err)
	}
}

// StateDir returns this run's private .prox directory.
func (r *proxRun) StateDir() string {
	return filepath.Join(r.dir, daemonStateDirName)
}

// Addr returns the API base URL of this run's daemon, read from this run's own
// state file.
//
// The state file is the only race-free source. A bind-and-close probe for a
// "free" port would reintroduce exactly the TOCTOU this harness exists to
// remove; here prox picks the port, writes it down, and the test reads what
// prox actually chose. TestDaemonMode_DynamicPort already relies on this.
//
// The result is memoized (with the rest of the daemon identity) because the
// daemon deletes its state file on shutdown, so a late caller (a post-shutdown
// assertion, say) must still get the address rather than a timeout.
func (r *proxRun) Addr() string {
	r.t.Helper()
	return r.DaemonIdentity().Addr
}

// DaemonIdentity returns this run's daemon {pid, startToken, stateDir, addr},
// waiting up to apiReadyTimeout for the state file to name it, and failing the
// test if it never does.
func (r *proxRun) DaemonIdentity() daemonIdentity {
	r.t.Helper()

	statePath := filepath.Join(r.StateDir(), daemonStateFileName)
	start := time.Now()
	deadline := start.Add(apiReadyTimeout)
	for {
		if id, ok := r.tryDaemonIdentity(); ok {
			return id
		}

		// Report the real failure rather than waiting out the full budget and
		// then blaming the API: if prox is already gone it is never going to
		// write the file, and its output says why. A detached launcher exits by
		// design, so only a foreground run can fail this way.
		if !r.detached {
			select {
			case <-r.exited:
				r.t.Fatalf("prox exited (%v) before writing %s; output:\n%s", r.waitErr, statePath, r.Output())
			default:
			}
		}

		if !time.Now().Before(deadline) {
			r.t.Fatalf("prox did not write a usable %s %s; output:\n%s", statePath, waitedFor(start, deadline), r.Output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tryDaemonIdentity makes ONE non-blocking attempt to resolve the daemon
// identity, memoizing and ledgering it on first success.
//
// The ledger append happens here, at the single moment a daemon first exists to
// be leaked, so it covers every launch -- including one whose test never asks
// for the address.
func (r *proxRun) tryDaemonIdentity() (daemonIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.identOK {
		return r.ident, true
	}

	id, ok := identityFromStateDir(r.StateDir(), r.acceptDaemonPID)
	if !ok {
		return daemonIdentity{}, false
	}
	r.ident, r.identOK = id, true

	if err := packageLedger.record(id); err != nil {
		// Never fatal, and never t.Logf: this runs on a background goroutine
		// that may outlive the test. A ledger this run cannot write only costs
		// the NEXT run its chance to reap, which is strictly better than
		// failing a test over a temp-directory problem.
		fmt.Fprintf(os.Stderr, "prox integration: recording daemon %s in the run ledger: %v\n", id, err)
	}
	return id, true
}

// acceptDaemonPID decides whether a pid found in this run's state file is this
// launch's daemon.
//
// Foreground: the state pid IS the launched process, and requiring the match
// makes staleness unrepresentable. A fixture directory can host more than one
// daemon generation (reap_orphans_test.go starts a second prox in the first
// one's dir), and a generation killed with SIGKILL leaves its state file behind:
// daemon.State.Write truncates in place rather than renaming, so a read between
// generations can otherwise return the DEAD generation's port, which then fails
// to answer.
//
// Detached: the launcher is not the daemon and has already exited, so the pid to
// accept is a DIFFERENT, live one -- the child `up -d` waited for before exiting
// (it refuses to report success until state.PID equals that child, so a leftover
// file from an earlier generation cannot be what we read here).
func (r *proxRun) acceptDaemonPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	launcher := r.launcherPID()
	if !r.detached {
		return launcher != 0 && pid == launcher
	}
	return pid != launcher && daemon.ProcessExists(pid)
}

// watchIdentity resolves the daemon identity in the background until it appears,
// the process exits, teardown stops it, or the readiness budget expires.
//
// It must never touch *testing.T: it can still be running when the test that
// started it finishes, and a t.Logf from there panics the run.
func (r *proxRun) watchIdentity() {
	deadline := time.Now().Add(apiReadyTimeout)
	for {
		if _, ok := r.tryDaemonIdentity(); ok {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-r.exited:
			return
		case <-r.watchStop:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// teardown is the run's registered cleanup, and the ONLY teardown any test in
// this package should need: ask the daemon to stop and WAIT for the answer, wait
// for the launcher and the daemon to actually be gone, and escalate to signals
// only on expiry -- aimed at the daemon identity, re-verified immediately before
// each signal.
//
// It is idempotent and safe to reach concurrently with the signal handler and
// the end-of-run sweep: every step is guarded by a liveness check that a
// finished daemon fails.
func (r *proxRun) teardown() {
	r.stopOnce.Do(func() { close(r.watchStop) })

	// One non-blocking attempt only. A launch that never produced a state file
	// (a refused `up`, or the /bin/sh in the race-freedom test below) must not
	// make every cleanup pay the readiness budget.
	id, haveDaemon := r.tryDaemonIdentity()
	if !haveDaemon {
		// Nothing daemon-shaped ever appeared, so there is nothing to ask
		// politely and nothing but the launched process to clean up.
		r.Kill()
		return
	}

	// Ask, wait, and escalate against the DAEMON identity (leakguard_test.go).
	stopDaemon(id, r.t.Logf)

	// Then the launcher. For a foreground run it IS the daemon and has already
	// exited above, so this only absorbs the gap before wait4 reaps it; for a
	// detached run it exited before this handle was even returned. A launcher
	// still standing after all that gets the SIGKILL Kill has always sent.
	if !r.awaitExit(launcherExitBudget) {
		r.t.Logf("teardown: launcher pid %d outlived the daemon; killing it", r.launcherPID())
		r.Kill()
	}
}

// launcherPID is the pid of the process this handle started, or 0 if it never
// started. It is NOT necessarily the daemon -- see DaemonIdentity.
func (r *proxRun) launcherPID() int {
	if r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}

// awaitExit reports whether the launched process has exited within d.
func (r *proxRun) awaitExit(d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	select {
	case <-r.exited:
		return true
	case <-time.After(d):
		select {
		case <-r.exited:
			return true
		default:
			return false
		}
	}
}

// daemonStateDirName / daemonStateFileName mirror internal/daemon's constants,
// which this external test package deliberately does not import.
const (
	daemonStateDirName  = ".prox"
	daemonStateFileName = "prox.state"
)

// TestProxRun_KillDuringWaitExitIsRaceFree is the regression test for the
// defect this harness was built to remove: two goroutines calling Cmd.Wait on
// the same command.
//
// The old idiom put `defer killProx(cmd)` in the test and waited via
// waitCmdExit, which spawned its own `go cmd.Wait()`. Whenever a wait timed out
// -- i.e. whenever a test was ALREADY failing -- t.Fatalf ran the deferred
// killProx, whose second Cmd.Wait ran concurrently with the first. exec.Cmd.Wait
// is documented as not safe for concurrent use, and both calls write
// Cmd.ProcessState, so the real failure arrived buried under a race report.
//
// This test reproduces exactly that shape against the harness: a still-running
// process, one goroutine waiting and another killing at the same time. Against
// proxRun it is quiet, because the single waiter goroutine started at launch
// owns the Wait and both callers merely observe the channel it closes. Against
// the old pair it fails twice over -- `-race` reports the concurrent
// ProcessState access, and the two observers disagree, since whichever Wait
// loses gets "exec: Wait was already called" instead of the exit status.
func TestProxRun_KillDuringWaitExitIsRaceFree(t *testing.T) {
	startTest(t, defaultTestBudget)
	if runtime.GOOS == "windows" {
		t.Skip("the /bin/sh probe below is unix-only")
	}

	// The config is irrelevant here -- the launched process is a shell, not
	// prox -- but a fixture is what owns a private directory and the single
	// Wait, so this exercises the same code path every real test takes.
	f := newInlineFixture(t, "processes:\n  noop: \"true\"\n")
	run := f.StartWith(t, "/bin/sh", nil, "-c", "sleep 30")

	// Both calls must be in flight while the process is still running: that is
	// what puts a second Wait alongside a first one already blocked in wait4.
	var wg sync.WaitGroup
	var waitErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Generously bounded: this must observe the kill, not its own timeout,
		// so that no t.Fatalf is reached from this non-test goroutine.
		waitErr = run.WaitExit(t, within(t, processExitTimeout))
	}()
	go func() {
		defer wg.Done()
		run.Kill()
	}()
	wg.Wait()

	// One Wait means one result: a second observer sees the same error value,
	// not "exec: Wait was already called" and not a nil clobbered by a racing
	// writer.
	// Identity, not equivalence, is the assertion: every observer must be handed
	// the one stored result, so the values have to be the same error.
	if second := run.WaitExit(t, within(t, processExitTimeout)); second != waitErr {
		t.Fatalf("the two observers disagree: concurrent waiter got %v, later waiter got %v", waitErr, second)
	}
	if waitErr == nil {
		t.Fatalf("expected the killed process to report a non-nil wait error, got nil")
	}
	if strings.Contains(waitErr.Error(), "Wait was already called") {
		t.Fatalf("Cmd.Wait was called more than once: %v", waitErr)
	}
}

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
