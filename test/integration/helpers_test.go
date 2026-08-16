package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain scrubs PROX_TUI from this test process's environment before any test
// runs, so no `prox` subprocess started here can inherit a developer's own
// setting (codex review of plan 026 C7).
//
// PROX_TUI is a documented per-shell knob that outranks terminal capability, and
// nearly every test in this package launches the real binary with an inherited
// environment. An ambient PROX_TUI=1 makes the TUI-default tests pass even
// against a reverted default; an ambient PROX_TUI=0 makes them fail against
// correct code; an ambient garbage value adds a warning line to output several
// tests assert on. Tests that mean to exercise the variable set it explicitly on
// cmd.Env, which is unaffected by this. (The pty helpers filter it out of
// cmd.Env as well, so they do not silently depend on this.)
//
// It also installs the run-level halves of the leak guard (plan 027 C5,
// leakguard_test.go): a SIGINT/SIGTERM trap so an interrupted run still stops
// its daemons, a sweep of ledgers left by runs that died before they could, and
// an informational banner when a daemon is running in the repo root.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("PROX_TUI")

	installSignalTeardown(os.Stderr)
	warnOnRepoRootDaemon(os.Stderr)
	reapStaleLedgers(packageLedger.dir, os.Getpid(), os.Stderr)

	code := m.Run()

	// Anything this run started and did not stop is a teardown bug; kill it
	// before reporting, so a leak can never outlive the process that made it.
	if packageLedger.reapOwn(os.Stderr) > 0 && code == 0 {
		fmt.Fprintln(os.Stderr, "prox integration: the run leaked daemons that survived their tests' teardown")
	}
	packageLedger.remove()

	// The shared binary outlives every t.TempDir(), so it is removed here.
	if sharedBinary.dir != "" {
		_ = os.RemoveAll(sharedBinary.dir)
	}
	os.Exit(code)
}

// buildBinary returns the path to a prox binary built once for the whole
// package run.
//
// It used to build into t.TempDir(), i.e. once per test: 61 builds per run,
// each producing a BRAND NEW binary on disk. That is not free even with a warm
// build cache. On macOS the first execution of a freshly written binary costs
// 0.22-0.40s of code-signing/security evaluation, against 0.00s for a binary
// that has been run before (measured, plan 027 C4 gate), and 61 concurrent
// `go build` invocations also contend for the build cache lock while the ~1500
// unit tests in other packages run in parallel.
//
// Sharing one binary does NOT reintroduce the shared-resource problem this
// plan removes: the binary is read-only and identical for every test, whereas
// the resources that made this suite unreliable -- one .prox state directory
// and one API port -- were mutable and contended. The per-test isolation that
// matters lives in proxFixture, not here.
//
// The binary lives in a package-scoped temp dir removed by TestMain after
// m.Run(), since t.TempDir() is per-test and would delete it while other tests
// still need it.
func buildBinary(t *testing.T) string {
	t.Helper()

	sharedBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "prox-integration-bin-")
		if err != nil {
			sharedBinary.err = fmt.Errorf("creating shared binary dir: %w", err)
			return
		}
		sharedBinary.dir = dir

		// Bounded like everything else, but with its own budget: a cold build
		// cache legitimately takes far longer than any CLI invocation, and a
		// `go build` that never returns (a stuck cache lock, say) would
		// otherwise hang every test in the package behind one sync.Once.
		ctx, cancel := context.WithTimeout(context.Background(), binaryBuildTimeout)
		defer cancel()

		binary := filepath.Join(dir, "prox")
		cmd := boundedCommand(ctx, projectRoot(t), "go", "build", "-o", binary, "./cmd/prox")
		if out, err := cmd.CombinedOutput(); err != nil {
			sharedBinary.err = fmt.Errorf("building prox: %w\n%s", err, out)
			return
		}

		// Pay the first-exec cost once, here, rather than letting whichever
		// test happens to run first absorb it inside its own readiness budget.
		if out, err := boundedCommand(ctx, "", binary, "--version").CombinedOutput(); err != nil {
			sharedBinary.err = fmt.Errorf("warming prox binary: %w\n%s", err, out)
			return
		}
		sharedBinary.path = binary
	})

	if sharedBinary.err != nil {
		t.Fatalf("failed to build binary: %v", sharedBinary.err)
	}
	return sharedBinary.path
}

// sharedBinary holds the one prox build shared by every test in this package.
var sharedBinary struct {
	once sync.Once
	path string
	dir  string
	err  error
}

// --- timeout budgets, named by role -----------------------------------------
//
// One block, one place to look. Before plan 027 C6 this package carried ~150
// bare duration literals and three named constants, so the SAME wait was 3s in
// one test, 5s in another and 10s in a third, and nothing recorded which
// number was a considered budget and which was a guess someone bumped once to
// get past a flake.
//
// Every constant below is named for the ROLE it plays, never for the caller,
// because the role is what decides how long is reasonable. None of them is a
// performance assertion: every one bounds a wait for something that either
// happens or fails the test, so a generous budget costs only how long a real
// failure takes to REPORT.
//
// Generous, but bounded on purpose. Plan 026's lesson was that inflating
// budgets without bounding total cost turns a legible set of failures into one
// illegible package-timeout panic, which names the package and nothing else.
// The per-test watchdog (watchdog_test.go) is the other half of that bargain:
// these budgets say how long ONE wait may take, the watchdog says how long one
// TEST may take, and `within` below makes sure the first can never outlive the
// second.
const (
	// apiReadyTimeout: the daemon's API answers GET /api/v1/status.
	//
	// It was a fixed 10s, which does not survive `make test-race`: that builds
	// and runs every package concurrently with race instrumentation, so the
	// whole unit suite competes with these integration tests for cores and a
	// race-instrumented `prox up` regularly needs more than 10s to bind and
	// answer. The failure signature is a wave of "API did not become ready
	// within 10s" across unrelated tests, which reads exactly like a real
	// regression and is not one.
	apiReadyTimeout = 20 * time.Second

	// stateFileTimeout: a .prox/prox.state (or a marker file a fixture script
	// touches) appears or disappears on disk.
	stateFileTimeout = 15 * time.Second

	// processStateTimeout: a supervised process reaches an expected state.
	processStateTimeout = 15 * time.Second

	// logAppearTimeout: a log line or a marker emitted by a supervised process
	// becomes visible, whether through the API or in a captured stdout stream.
	logAppearTimeout = 15 * time.Second

	// streamFrameTimeout: one SSE frame arrives on an already-connected stream.
	// Streams are bounded frame by frame, never as a whole — see sseHTTPClient.
	streamFrameTimeout = 10 * time.Second

	// processExitTimeout: a launched prox process exits after being asked to
	// shut down.
	processExitTimeout = 30 * time.Second

	// pidGoneTimeout: a child or grandchild pid disappears.
	pidGoneTimeout = 15 * time.Second

	// cliCommandTimeout: one CLI invocation runs to completion. A ceiling on a
	// single `prox status`/`prox stop`, not a budget for a loop around it.
	cliCommandTimeout = 30 * time.Second

	// dependencyReadyTimeout: a declared dependency converges to ready and the
	// processes gated behind it start.
	//
	// Not in the role table plan 027 C6 started from, and deliberately so: this
	// waits on a real external-service check loop rather than on prox's own
	// bookkeeping, and it is the one place in the suite that legitimately took
	// 30s. Folding it into processStateTimeout would have LOWERED a live budget
	// to 15s, which is a flake, not a cleanup.
	dependencyReadyTimeout = 30 * time.Second

	// ptyWaitTimeout: anything observed through a real pty.
	//
	// It was 15s, which is not survivable under `go test ./...`: that runs every
	// package CONCURRENTLY, so the whole unit suite — including the deliberately
	// slow race and deadlock tests added by plan 026 — competes with these pty
	// tests for the same cores, and `prox up` can easily take longer than 15s to
	// get as far as printing its API URL. The failure signature is a wave of
	// "not found in pty output within 15s" across unrelated tests, which reads
	// exactly like a real regression and is not one; it also predates plan 026
	// (reproduced on the C6 tree). CI runs `go test -v ./...` on two-core hosted
	// runners, where the squeeze is worse than on a dev machine.
	ptyWaitTimeout = 25 * time.Second

	// pollRequestTimeout: ONE HTTP request to a local prox API. A ceiling, not a
	// budget — pollGetWithinDeadline shortens it to whatever is left of the
	// caller's own deadline.
	pollRequestTimeout = 5 * time.Second

	// commandKillGrace: how long Cmd.Wait may still take AFTER a bounded
	// command has been cancelled and its leader killed.
	//
	// This one is load-bearing and easy to miss. exec.CommandContext's default
	// cancel kills the LEADER only, and Cmd.Wait does not return until the
	// goroutines copying stdout/stderr see EOF — which never happens while any
	// orphaned descendant still holds the write end of those pipes. A `sh -c`
	// wrapper around a long sleep reproduces it exactly. Without Cmd.WaitDelay
	// the context deadline bounds the PROCESS and not the CALL, so a helper
	// that looks bounded hangs anyway; TestBounding_StalledSubprocess... caught
	// precisely that.
	commandKillGrace = 2 * time.Second

	// binaryBuildTimeout: the one-per-run `go build ./cmd/prox`. Its own budget
	// because a cold build cache is legitimately slow and nothing else in this
	// package compiles anything.
	binaryBuildTimeout = 5 * time.Minute
)

// cliContext bounds one CLI invocation made from a test body: the CLI ceiling,
// or whatever is left of the test's watchdog budget, whichever is shorter.
func cliContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return boundedContext(within(t, cliCommandTimeout), cliCommandTimeout)
}

// boundedCommand builds an exec.Cmd bounded end to end by ctx: cancellation
// kills the process, and WaitDelay bounds how long Wait may then spend waiting
// on pipes an orphaned descendant may still hold open.
//
// Every subprocess this package launches WITHOUT the proxRun harness goes
// through here, so "bounded" means the CALL returns, not merely that the child
// was signalled.
func boundedCommand(ctx context.Context, dir, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.WaitDelay = commandKillGrace
	return cmd
}

// within returns the instant a wait of role budget d must end by: d from now,
// or this test's watchdog deadline, whichever comes FIRST.
//
// This is why every polling helper in this package takes a `deadline time.Time`
// rather than a duration. A duration composes wrongly: three nested 15s waits
// inside a 20s test are three chances to blow past the test's own budget, and
// each one still reports "within 15s" on the way out. A deadline composes
// correctly, because a nested wait can only ever be handed what is LEFT.
func within(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	at := time.Now().Add(d)
	if td, ok := testDeadline(t); ok && td.Before(at) {
		return td
	}
	return at
}

// waitedFor renders the only two facts a timed-out wait can state honestly: how
// long it ACTUALLY ran, and the absolute instant it was bounded by.
//
// Reporting the nominal role budget instead is a lie waiting to happen. A
// deadline carries no original duration, and `within` hands a nested wait only
// the remainder of its caller's budget, so a message saying "did not happen
// within 15s" routinely describes a wait that in fact got 2s — and sends the
// reader looking for a 15s stall that never occurred.
func waitedFor(start, deadline time.Time) string {
	now := time.Now()
	return fmt.Sprintf("after %v (deadline %s, now %s)",
		now.Sub(start).Round(time.Millisecond),
		deadline.Format(clockFormat),
		now.Format(clockFormat))
}

// boundedContext returns a context bounded by BOTH a role ceiling and the
// caller's remaining budget, whichever is shorter, plus its cancel.
//
// It is the exec-side counterpart of pollGetWithinDeadline, and exists for the
// same reason: a step that starts just before a deadline must not be allowed to
// run its full ceiling PAST it, or the surrounding loop reports a timeout for a
// budget it silently doubled. An expired budget yields an already-cancelled
// context, so the caller's loop condition ends the wait rather than one last
// attempt sneaking in after the deadline.
func boundedContext(deadline time.Time, ceiling time.Duration) (context.Context, context.CancelFunc) {
	budget := min(time.Until(deadline), ceiling)
	if budget < 0 {
		budget = 0
	}
	return context.WithTimeout(context.Background(), budget)
}

// clockFormat is how absolute deadlines are printed: wall-clock time to the
// millisecond, which is what lines up against a log or another test's output.
const clockFormat = "15:04:05.000"

// pollInterval is how long every poll loop in this package sleeps between
// attempts.
const pollInterval = 100 * time.Millisecond

// apiClient bounds every single request this suite makes to a prox API.
//
// The naked http.Get these helpers used goes through http.DefaultClient, which
// has NO timeout: a server that accepts the connection and then stalls blocks
// the call indefinitely, so the helper sails past its own budget and the package
// eventually dies on go test's timeout instead of failing one assertion
// (CodeRabbit, PR #106). The per-request budget is deliberately much shorter
// than any surrounding poll loop, since every attempt is retried anyway.
//
// SSE endpoints must NOT use this client: a client-wide Timeout kills a healthy
// long-lived stream. See sseHTTPClient in push_data_plane_test.go.
var apiClient = &http.Client{Timeout: pollRequestTimeout}

// pollGetWithinDeadline issues one poll bounded by BOTH the per-request ceiling
// and the caller's remaining budget, returning a cancel the caller must run
// after closing the body.
//
// The client timeout alone bounds a stalled call but not the operation: a
// request that starts just before the deadline may still run the full ceiling
// past it, so a helper given a 5s budget could take ~10s and then report a
// timeout "within 5s" (CodeRabbit, PR #106). A timeout message that misstates
// its own budget is exactly the kind of misleading signal that makes these
// failures expensive to diagnose.
func pollGetWithinDeadline(url string, deadline time.Time) (*http.Response, context.CancelFunc, error) {
	budget := min(time.Until(deadline), pollRequestTimeout)
	if budget <= 0 {
		// The budget ran out between the caller's loop check and here. Do not
		// start a request at all: a very short one could still succeed and let
		// the helper report readiness AFTER its deadline (CodeRabbit, PR #106).
		// Returning the error lets the loop's own condition end the wait.
		return nil, nil, context.DeadlineExceeded
	}

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// waitForAPI waits, until deadline, for the API at addr to answer
// GET /api/v1/status, and fails the test if it never does.
func waitForAPI(t *testing.T, addr string, deadline time.Time) {
	t.Helper()
	if err := awaitAPI(addr, deadline); err != nil {
		t.Fatal(err)
	}
}

// awaitAPI is waitForAPI without a *testing.T, so that the bounding invariant
// can be asserted on rather than merely believed: bounding_test.go points this
// at a server that accepts and never answers and checks both that it returns
// and that the elapsed time it reports is the elapsed time that really passed.
func awaitAPI(addr string, deadline time.Time) error {
	start := time.Now()
	for time.Now().Before(deadline) {
		resp, cancel, err := pollGetWithinDeadline(addr+"/api/v1/status", deadline)
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			cancel()
			if ok {
				return nil
			}
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("API at %s did not become ready %s", addr, waitedFor(start, deadline))
}

// syncBuffer is a goroutine-safe bytes.Buffer: the exec copier goroutines
// write while tests poll Output() before the process has exited.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stopProx sends shutdown request to prox via API
func stopProx(t *testing.T, addr string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), pollRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/shutdown", nil)
	if err != nil {
		return err
	}

	// The bare (non-waited) shutdown returns as soon as the daemon has been
	// ASKED, so the one-request ceiling is the right bound here. The WAITED
	// variant is a different budget entirely — see requestWaitedShutdown.
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// restartProcess sends a POST /api/v1/processes/{name}/restart request and
// returns the HTTP status code and the parsed error response (empty Code/Error
// on success).
func restartProcess(t *testing.T, addr, name string) (int, ErrorResponse) {
	t.Helper()
	return postProcessAction(t, addr, name, "restart")
}

// stopProcess sends a POST /api/v1/processes/{name}/stop request and returns
// the HTTP status code and parsed error response (empty Code/Error on
// success).
func stopProcess(t *testing.T, addr, name string) (int, ErrorResponse) {
	t.Helper()
	return postProcessAction(t, addr, name, "stop")
}

// postProcessAction posts to /api/v1/processes/{name}/{action} (start, stop,
// restart) and returns the status code plus a decoded error body (zero value
// if the response wasn't an error payload).
//
// Bounded by processExitTimeout rather than apiClient's one-request ceiling,
// because these handlers do not return until the transition has HAPPENED: a
// stop sits through the whole SIGTERM grace (constants.DefaultShutdownTimeout,
// 10s) before SIGKILL, and a restart pays that and then starts. The stubborn-
// grandchild tests reach exactly that path, and a 5s ceiling turns a healthy
// 12s stop into "context deadline exceeded", which reads like a broken daemon.
func postProcessAction(t *testing.T, addr, name, action string) (int, ErrorResponse) {
	t.Helper()

	url := fmt.Sprintf("%s/api/v1/processes/%s/%s", addr, name, action)

	ctx, cancel := boundedContext(within(t, processExitTimeout), processExitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("failed to build POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ctxBoundClient.Do(req)
	if err != nil {
		t.Fatalf("failed to POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	var errResp ErrorResponse
	if resp.StatusCode != http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
	}
	return resp.StatusCode, errResp
}

// ErrorResponse mirrors internal/api.ErrorResponse for decoding error bodies
// in integration tests without importing the internal/api package.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// projectRoot returns the repo root (two dirs up from test/integration), where
// the daemon and CLI both run so they share one .prox state directory.
func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	return filepath.Join(wd, "..", "..")
}

// freePortAttempts bounds how many ports registerOnFreePort will try before
// giving up. Losing the reserve/bind race once is plausible; losing it three
// times running means the machine is out of ephemeral ports or something is
// scanning them, and neither is worth retrying through.
const freePortAttempts = 3

// registerOnFreePort reserves an ephemeral port, releases the reservation, and
// calls register(port) -- the call that makes the shared daemon actually bind
// it -- retrying with a fresh port if something else took it in between.
//
// It returns the port that was successfully bound.
func registerOnFreePort(t *testing.T, register func(port int) error) int {
	t.Helper()

	var lastErr error
	for attempt := 1; attempt <= freePortAttempts; attempt++ {
		port, reservation := freePort(t)
		if err := reservation.Close(); err != nil {
			t.Fatalf("release reserved port %d: %v", port, err)
		}
		err := register(port)
		if err == nil {
			return port
		}
		if !isAddrInUse(err) {
			t.Fatalf("register on port %d: %v", port, err)
		}
		lastErr = err
		t.Logf("port %d was taken between reservation and bind (attempt %d/%d): %v",
			port, attempt, freePortAttempts, err)
	}
	t.Fatalf("no free port survived reservation in %d attempts; last error: %v", freePortAttempts, lastErr)
	return 0 // unreachable; t.Fatalf stops the test
}

// isAddrInUse reports whether err is an EADDRINUSE, including when it arrived
// as text: these binds happen inside the proxy daemon and come back over its
// socket as a string, so the typed errno does not survive the trip.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use")
}

// waitForProcessState waits, until deadline, for a process to reach a specific
// state.
func waitForProcessState(t *testing.T, addr, name, expectedStatus string, deadline time.Time) ProcessInfo {
	t.Helper()

	start := time.Now()
	var lastStatus string
	for time.Now().Before(deadline) {
		resp, cancel, err := pollGetWithinDeadline(fmt.Sprintf("%s/api/v1/processes/%s", addr, name), deadline)
		if err == nil {
			var proc ProcessInfo
			matched := false
			if err := json.NewDecoder(resp.Body).Decode(&proc); err == nil {
				lastStatus = proc.Status
				matched = proc.Status == expectedStatus
			}
			resp.Body.Close()
			cancel()
			if matched {
				return proc
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("process %s did not reach state %q %s (last status: %q)",
		name, expectedStatus, waitedFor(start, deadline), lastStatus)
	return ProcessInfo{}
}

// requireNoError fails the test if err is not nil
func requireNoError(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// skipShort skips the test if -short flag is provided
func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
}

// waitForStateFile waits, until deadline, for path to exist.
func waitForStateFile(t *testing.T, path string, deadline time.Time) {
	t.Helper()

	start := time.Now()
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("state file %s not created %s", path, waitedFor(start, deadline))
}
