package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChild is an in-memory daemonChild so the D2 poll/wait/report logic can be
// tested without spawning a real process. Wait() blocks until the child is made
// to exit (either explicitly or by a signal registered in exitOn).
type fakeChild struct {
	pid    int
	done   chan error
	once   sync.Once
	exitOn map[syscall.Signal]bool
	exitBy error

	mu      sync.Mutex
	signals []os.Signal
}

func newFakeChild(pid int) *fakeChild {
	return &fakeChild{pid: pid, done: make(chan error, 1), exitOn: map[syscall.Signal]bool{}}
}

func (f *fakeChild) Pid() int    { return f.pid }
func (f *fakeChild) Wait() error { return <-f.done }

func (f *fakeChild) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.signals = append(f.signals, sig)
	f.mu.Unlock()
	if s, ok := sig.(syscall.Signal); ok && f.exitOn[s] {
		f.exit(f.exitBy)
	}
	return nil
}

func (f *fakeChild) exit(err error) { f.once.Do(func() { f.done <- err }) }

func (f *fakeChild) sentSignals() []os.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]os.Signal(nil), f.signals...)
}

// fastStartupOps returns ops with instant sleeps and tiny deadlines/grace so the
// timeout path resolves in milliseconds. Defaults model a never-ready child (no
// state, health down); individual tests override fields.
func fastStartupOps() daemonStartupOps {
	return daemonStartupOps{
		loadState: func(string) (*daemon.State, error) { return nil, errors.New("no state") },
		healthOK:  func(string) bool { return false },
		logTail:   func(string, int, int) string { return "" },
		// Default: nothing failed within the settle window (plan 027 C13).
		// Tests that care about the post-readiness verdict override it.
		settle: func(string) (settleVerdict, error) { return settleVerdict{}, nil },
		// Default: a sealed session with nothing to say (plan 028 A2), so a test
		// about anything else sees no extra output.
		fetchWarnings: func(string) ([]domain.Warning, bool, error) { return nil, true, nil },
		sleep:         func(time.Duration) {},

		readyTimeout:    40 * time.Millisecond,
		pollInterval:    time.Millisecond,
		killGrace:       30 * time.Millisecond,
		warningsTimeout: 40 * time.Millisecond,
		warningsPoll:    time.Millisecond,
	}
}

// TestAwaitDaemonStartup_Success: state file with the child's PID plus a healthy
// /health probe → nil (exit 0), and the running child is never signaled.
func TestAwaitDaemonStartup_Success(t *testing.T) {
	child := newFakeChild(4242)
	// Release the (never-exiting) Wait goroutine at test end.
	t.Cleanup(func() { child.exit(nil) })

	ops := fastStartupOps()
	ops.loadState = func(string) (*daemon.State, error) {
		return &daemon.State{PID: 4242, Host: "127.0.0.1", Port: 12345}, nil
	}
	healthCalls := 0
	ops.healthOK = func(addr string) bool {
		healthCalls++
		assert.Equal(t, "127.0.0.1:12345", addr)
		return true
	}

	addr, err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:12345", addr,
		"the resolved address must be returned: the settle check that follows has no other way to reach the daemon")
	assert.Equal(t, 1, healthCalls, "health should be probed once")
	assert.Empty(t, child.sentSignals(), "a ready child must not be signaled")
}

// readyStartupOps returns fastStartupOps for a child that becomes ready
// immediately, so a test can concentrate on what happens AFTER readiness.
func readyStartupOps(pid int) daemonStartupOps {
	ops := fastStartupOps()
	ops.loadState = func(string) (*daemon.State, error) {
		return &daemon.State{PID: pid, Host: "127.0.0.1", Port: 12345}, nil
	}
	ops.healthOK = func(string) bool { return true }
	return ops
}

// TestStartDetachedDaemon_SuccessPrintsHeadlineAfterSettle: a ready daemon whose
// processes survive the settle window exits 0 and prints the started line — and
// prints it only ONCE the settle step has run, which is the ordering the whole
// commit turns on.
func TestStartDetachedDaemon_SuccessPrintsHeadlineAfterSettle(t *testing.T) {
	child := newFakeChild(5000)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5000)
	settled := false
	var gotAddr string
	ops.settle = func(addr string) (settleVerdict, error) {
		settled = true
		gotAddr = addr
		return settleVerdict{}, nil
	}

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.NoError(t, err)
	assert.True(t, settled, "the settle step must run before success is declared")
	assert.Equal(t, "127.0.0.1:12345", gotAddr)
	assert.Contains(t, stdout, "prox started (pid 5000, api http://127.0.0.1:12345)")
	assert.Empty(t, stderr)
}

// TestStartDetachedDaemon_CrashedProcessExitsNonZero is the #94 contract itself:
// the daemon is up, a process is crashed, and `prox up -d` must NOT report
// success. The success headline must be absent (printing it and then exiting
// non-zero is the same lie in a new place), the crashed process must be named
// with the same sentence `prox status` uses, and `prox down` must be offered —
// because a non-zero exit here does not mean nothing started.
func TestStartDetachedDaemon_CrashedProcessExitsNonZero(t *testing.T) {
	child := newFakeChild(5001)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5001)
	ops.settle = func(string) (settleVerdict, error) {
		return settleVerdict{crashed: []string{"web", "worker"}}, nil
	}

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.Error(t, err)
	assert.Equal(t, "2 process(es) crashed", err.Error(),
		"the sentinel must be the one `prox status` returns, not a second vocabulary")
	assert.NotContains(t, stdout, "prox started", "no success headline may precede a non-zero exit")
	assert.Contains(t, stderr, "Crashed: web, worker — check 'prox logs web'.")
	assert.Contains(t, stderr, "prox down")
	assert.Contains(t, stderr, "pid 5001", "the failure still has to say what IS running")
}

// TestStartDetachedDaemon_BlockedProcessExitsNonZero: blocked shares the exit
// contract and the formatter with crashed, so the two paths cannot drift.
func TestStartDetachedDaemon_BlockedProcessExitsNonZero(t *testing.T) {
	child := newFakeChild(5002)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5002)
	ops.settle = func(string) (settleVerdict, error) {
		return settleVerdict{blocked: []settleProcess{{Name: "gated", Status: "blocked", BlockedOn: []string{"pg"}}}}, nil
	}

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.Error(t, err)
	assert.Equal(t, "1 process(es) blocked on failed dependencies", err.Error())
	assert.NotContains(t, stdout, "prox started")
	assert.Contains(t, stderr, "Blocked: gated(pg)")
}

// TestStartDetachedDaemon_UnverifiableStateKeepsExitCode: when the VERIFICATION
// fails (transport error, daemon shut down mid-poll, malformed body), readiness
// was still established — so the command keeps its pre-existing exit code and
// says once that the state could not be confirmed. A flaky follow-up request
// must never invent a failure.
func TestStartDetachedDaemon_UnverifiableStateKeepsExitCode(t *testing.T) {
	child := newFakeChild(5003)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5003)
	ops.settle = func(string) (settleVerdict, error) {
		return settleVerdict{}, errors.New("connection refused")
	}

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.NoError(t, err, "a failed verification must not turn a ready daemon into a failed start")
	assert.Contains(t, stdout, "prox started (pid 5003")
	assert.Contains(t, stderr, "could not confirm process state")
	assert.Equal(t, 1, strings.Count(stderr, "Warning:"), "exactly one warning, not one per poll")
}

// TestStartDetachedDaemon_NeverReadySkipsSettle: a daemon that never became
// ready fails on readiness alone. The settle step must not run at all — there
// is nothing to ask, and asking would only replace a precise diagnostic with a
// transport error.
func TestStartDetachedDaemon_NeverReadySkipsSettle(t *testing.T) {
	child := newFakeChild(5004)
	child.exitOn[syscall.SIGTERM] = true

	ops := fastStartupOps()
	settled := false
	ops.settle = func(string) (settleVerdict, error) {
		settled = true
		return settleVerdict{}, nil
	}

	var err error
	captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")
	assert.False(t, settled, "readiness failed; there is nothing to settle")
}

// TestStartDetachedDaemon_WarningsReachTheParentsStderr: a `-d` child's stdout
// and stderr are .prox/prox.log, so anything it printed is invisible to the
// person who typed `prox up -d`. The parent reads the warnings back over the
// child's own API and prints them itself — AFTER the success headline, since a
// warning is advisory and must not displace the outcome (plan 028 A2).
func TestStartDetachedDaemon_WarningsReachTheParentsStderr(t *testing.T) {
	child := newFakeChild(5100)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5100)
	var fetchedAddr string
	ops.fetchWarnings = func(addr string) ([]domain.Warning, bool, error) {
		fetchedAddr = addr
		return []domain.Warning{{
			Code: domain.WarningCodeMkcertCAUntrusted, Message: "the CA is not installed.", Hint: "Run 'mkcert -install'.",
		}}, true, nil
	}

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.NoError(t, err, "a warning must never turn a working start into a failed one")
	assert.Equal(t, "127.0.0.1:12345", fetchedAddr, "warnings come from the daemon that just became ready")
	assert.Contains(t, stdout, "prox started (pid 5100")
	assert.Equal(t, "Warning: the CA is not installed.\n         Run 'mkcert -install'.\n", stderr)
}

// TestStartDetachedDaemon_WarningsPrintOnAFailedStartToo: the process verdict
// and the advisories answer different questions, so a non-zero start still
// reports what the daemon had to say.
func TestStartDetachedDaemon_WarningsPrintOnAFailedStartToo(t *testing.T) {
	child := newFakeChild(5101)
	t.Cleanup(func() { child.exit(nil) })

	ops := readyStartupOps(5101)
	ops.settle = func(string) (settleVerdict, error) {
		return settleVerdict{crashed: []string{"web"}}, nil
	}
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		return []domain.Warning{{Code: "c", Message: "advisory"}}, true, nil
	}

	var err error
	_, stderr := captureOutput(t, func() {
		err = startDetachedDaemon(child, t.TempDir(), ops)
	})

	require.Error(t, err)
	assert.Contains(t, stderr, "Crashed: web")
	assert.Contains(t, stderr, "Warning: advisory")
	assert.Less(t, strings.Index(stderr, "Crashed: web"), strings.Index(stderr, "Warning: advisory"),
		"the failure is still the headline; the advisory follows it")
}

// TestAwaitDaemonWarnings_PollsUntilSealed is the race the completion latch
// exists for, at the unit level: the child is serving /status (so readiness and
// the settle window are long past) while an asynchronous producer is still
// running. A single fetch at that instant would return nothing.
func TestAwaitDaemonWarnings_PollsUntilSealed(t *testing.T) {
	ops := fastStartupOps()
	// Generous, because this test is about the POLLING, not the budget: the
	// default 40ms deadline is real wall-clock time, so a scheduling hiccup
	// between fetches could expire it and end the loop early, making the
	// assertion below fail for a reason that has nothing to do with the latch
	// (CodeRabbit, PR #110). ops.sleep is a no-op, so a large budget costs
	// nothing.
	ops.warningsTimeout = time.Minute
	fetches := 0
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		fetches++
		if fetches < 3 {
			return nil, false, nil // producer still running
		}
		return []domain.Warning{{Code: "late", Message: "sealed after the settle window"}}, true, nil
	}

	got := awaitDaemonWarnings("127.0.0.1:1", ops)

	require.Len(t, got, 1)
	assert.Equal(t, "sealed after the settle window", got[0].Message)
	assert.Equal(t, 3, fetches, "it polls until the latch flips, then stops")
}

// TestAwaitDaemonWarnings_SealedImmediatelyCostsOneFetch: the common case (no
// asynchronous producer, so the session sealed before it ever served /status)
// must not pay the polling budget.
func TestAwaitDaemonWarnings_SealedImmediatelyCostsOneFetch(t *testing.T) {
	ops := fastStartupOps()
	fetches := 0
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		fetches++
		return nil, true, nil
	}

	assert.Nil(t, awaitDaemonWarnings("127.0.0.1:1", ops))
	assert.Equal(t, 1, fetches)
}

// TestAwaitDaemonWarnings_NeverSealedPrintsWhatItHas: if the latch never flips
// the parent gives up on time and reports the best answer it saw. A warning must
// never delay startup meaningfully, and never fail it.
func TestAwaitDaemonWarnings_NeverSealedPrintsWhatItHas(t *testing.T) {
	ops := fastStartupOps()
	// A real (short) sleep here, not the no-op fake: the deadline is wall-clock,
	// so a no-op sleep would spin the poll loop for the whole budget.
	ops.sleep = time.Sleep
	ops.warningsTimeout = 30 * time.Millisecond
	ops.warningsPoll = 5 * time.Millisecond
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		return []domain.Warning{{Code: "partial", Message: "seen but never sealed"}}, false, nil
	}

	start := time.Now()
	got := awaitDaemonWarnings("127.0.0.1:1", ops)

	require.Len(t, got, 1)
	assert.Equal(t, "seen but never sealed", got[0].Message)
	assert.Less(t, time.Since(start), 2*time.Second, "the wait is bounded by warningsTimeout")
}

// TestAwaitDaemonWarnings_FetchErrorsAreSurvivable: the daemon is up (readiness
// proved it), so a flaky status read is a reason to retry and then say nothing —
// never a reason to fail the start.
func TestAwaitDaemonWarnings_FetchErrorsAreSurvivable(t *testing.T) {
	ops := fastStartupOps()
	ops.sleep = time.Sleep
	ops.warningsTimeout = 30 * time.Millisecond
	ops.warningsPoll = 5 * time.Millisecond
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		return nil, false, errors.New("connection refused")
	}

	assert.Nil(t, awaitDaemonWarnings("127.0.0.1:1", ops))

	// A transient error followed by a sealed answer still yields the warning.
	calls := 0
	ops.fetchWarnings = func(string) ([]domain.Warning, bool, error) {
		calls++
		if calls == 1 {
			return nil, false, errors.New("connection refused")
		}
		return []domain.Warning{{Code: "c", Message: "arrived on the retry"}}, true, nil
	}
	got := awaitDaemonWarnings("127.0.0.1:1", ops)
	require.Len(t, got, 1)
	assert.Equal(t, "arrived on the retry", got[0].Message)
}

// TestAwaitDaemonWarnings_NoFetcherIsANoOp guards the unit-test ops literals
// (and any future caller) that never wire a fetcher.
func TestAwaitDaemonWarnings_NoFetcherIsANoOp(t *testing.T) {
	ops := fastStartupOps()
	ops.fetchWarnings = nil
	assert.Nil(t, awaitDaemonWarnings("127.0.0.1:1", ops))
}

// TestAwaitDaemonStartup_EarlyDeath: the child exits (non-zero) before becoming
// ready → error, no signals (nothing to kill).
func TestAwaitDaemonStartup_EarlyDeath(t *testing.T) {
	child := newFakeChild(4243)
	child.exit(&exitStatusError{code: 1}) // dead before we start polling

	tailUsed := false
	var gotPid int
	ops := fastStartupOps()
	ops.logTail = func(_ string, pid int, _ int) string {
		tailUsed = true
		gotPid = pid
		return "boom: config invalid"
	}

	_, err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start")
	assert.True(t, tailUsed, "log tail should be gathered for diagnostics")
	assert.Equal(t, child.Pid(), gotPid, "log tail must be scoped to the failed child's own pid")
	assert.Empty(t, child.sentSignals(), "a dead child must not be signaled")
}

// TestAwaitDaemonStartup_NeverReadyTimeout: child stays alive but never writes
// state / answers health → SIGTERM then, after the grace, SIGKILL (escalation),
// and an error is returned.
func TestAwaitDaemonStartup_NeverReadyTimeout(t *testing.T) {
	child := newFakeChild(4244)
	// Only SIGKILL makes it exit → forces the escalation path.
	child.exitOn[syscall.SIGKILL] = true

	ops := fastStartupOps()

	start := time.Now()
	_, err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")
	assert.GreaterOrEqual(t, time.Since(start), ops.readyTimeout, "must poll until the deadline")

	sigs := child.sentSignals()
	require.Len(t, sigs, 2, "expected SIGTERM then SIGKILL, got %v", sigs)
	assert.Equal(t, syscall.SIGTERM, sigs[0])
	assert.Equal(t, syscall.SIGKILL, sigs[1])
}

// TestAwaitDaemonStartup_TermSuffices: a hung child that exits on SIGTERM is not
// escalated to SIGKILL.
func TestAwaitDaemonStartup_TermSuffices(t *testing.T) {
	child := newFakeChild(4245)
	child.exitOn[syscall.SIGTERM] = true

	_, err := awaitDaemonStartup(child, t.TempDir(), fastStartupOps())
	require.Error(t, err)

	sigs := child.sentSignals()
	require.Len(t, sigs, 1, "expected only SIGTERM, got %v", sigs)
	assert.Equal(t, syscall.SIGTERM, sigs[0])
}

// TestAwaitDaemonStartup_StaleStateIgnored: a pre-existing state file with the
// WRONG PID (a previous run's leftover) must not satisfy the poll. Health is
// never probed, and the child is timed out and killed.
func TestAwaitDaemonStartup_StaleStateIgnored(t *testing.T) {
	child := newFakeChild(4246)
	child.exitOn[syscall.SIGKILL] = true
	child.exitOn[syscall.SIGTERM] = true

	ops := fastStartupOps()
	ops.loadState = func(string) (*daemon.State, error) {
		// Stale: belongs to some other/previous process, not our child.
		return &daemon.State{PID: 9999, Host: "127.0.0.1", Port: 22222}, nil
	}
	healthCalls := 0
	ops.healthOK = func(string) bool { healthCalls++; return true }

	_, err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")
	assert.Zero(t, healthCalls, "stale state (wrong PID) must not advance to the health probe")
	assert.NotEmpty(t, child.sentSignals(), "a never-ready child must be signaled")
}

// exitStatusError is a stand-in for an *exec.ExitError so early-death diagnostics
// render a non-zero status without spawning a real process.
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string { return "exit status " + strconv.Itoa(e.code) }

// --- bounded log diagnostics (plan 027 C16, M3) -------------------------------

// writeDaemonLog writes content as dir's .prox/prox.log and returns dir.
func writeDaemonLog(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := daemon.LogPath(dir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return dir
}

// TestReadLogTail_ReadsAtMostTheLimit is the M3 regression at its source.
// .prox/prox.log is never truncated, so reading it whole (os.ReadFile, as this
// did) makes the memory cost of REPORTING a startup failure a function of every
// run that ever wrote to that project's log.
func TestReadLogTail_ReadsAtMostTheLimit(t *testing.T) {
	var b strings.Builder
	for b.Len() < 3*daemonLogReadLimit {
		b.WriteString("a log line that is here purely to make the file large\n")
	}
	full := b.String()
	dir := writeDaemonLog(t, full)

	got, err := readLogTail(daemon.LogPath(dir), daemonLogReadLimit)
	require.NoError(t, err)

	require.LessOrEqual(t, len(got), daemonLogReadLimit,
		"read more than the byte cap from a %d-byte log", len(full))
	require.NotEmpty(t, got)
	assert.True(t, strings.HasSuffix(full, got), "the window must be the END of the file")
	// The window opens mid-line; that fragment must be dropped rather than
	// handed on as if it were a whole line (a half-written run marker read as a
	// damaged one would be one we manufactured ourselves).
	assert.Equal(t, byte('\n'), full[len(full)-len(got)-1],
		"the returned window must start at a line boundary")
}

// TestReadLogTail_SmallFileIsReadWhole keeps the ordinary case exact: a log
// under the cap is returned byte for byte, first line included.
func TestReadLogTail_SmallFileIsReadWhole(t *testing.T) {
	const content = "--- run 2026-08-15T09:00:00Z pid=100 ---\nboom\n"
	dir := writeDaemonLog(t, content)

	got, err := readLogTail(daemon.LogPath(dir), daemonLogReadLimit)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

// TestDaemonLogTail_CapsLinesAndSaysSo pins the other half of M3: a run that
// logged more than the cap is reported as capped. Printing its last N lines
// under a bare "Last lines of ..." heading would present a fragment of the run
// as the whole of it -- the same class of quiet misstatement #99 was about.
func TestDaemonLogTail_CapsLinesAndSaysSo(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- run 2026-08-15T09:00:00Z pid=4242 ---\n")
	for i := 0; i < daemon.MaxRunTailLines+50; i++ {
		b.WriteString("chatty line " + strconv.Itoa(i) + "\n")
	}
	dir := writeDaemonLog(t, b.String())

	got := daemonLogTail(dir, 4242, daemonLogFallbackLines)
	lines := strings.Split(got, "\n")

	assert.Contains(t, got, "earlier lines of this run omitted",
		"a capped tail must say it was capped")
	assert.Len(t, lines, daemon.MaxRunTailLines+1, "notice plus exactly the capped line count")
	assert.Equal(t, "chatty line "+strconv.Itoa(daemon.MaxRunTailLines+49), lines[len(lines)-1],
		"the NEWEST lines are the ones worth keeping")
}

// TestDaemonLogTail_ShortRunIsVerbatim: nothing above is allowed to change the
// ordinary case, which is a short run reported exactly as it was logged.
func TestDaemonLogTail_ShortRunIsVerbatim(t *testing.T) {
	dir := writeDaemonLog(t, "--- run 2026-08-15T09:00:00Z pid=4242 ---\nboom: config invalid\n")

	got := daemonLogTail(dir, 4242, daemonLogFallbackLines)
	assert.Equal(t, "boom: config invalid", got)
	assert.NotContains(t, got, "omitted")
}

// TestDaemonLogTail_DamagedCurrentMarkerFallsBack is M4 seen from the CLI: the
// current run crashed partway through writing its marker and an OLDER run had
// the same pid. The scoped tail must refuse rather than hand the old run's
// output to a message that calls it this run's; the fallback that takes over
// claims nothing about which run it came from.
func TestDaemonLogTail_DamagedCurrentMarkerFallsBack(t *testing.T) {
	dir := writeDaemonLog(t, ""+
		"--- run 2026-08-15T08:00:00Z pid=4242 ---\n"+
		"OLD RUN OUTPUT\n"+
		"--- run 2026-08-15T09:00:00Z pid=42")

	got := daemonLogTail(dir, 4242, daemonLogFallbackLines)
	// The fallback (last n lines) still shows the file's end, torn marker and
	// all -- what it must NOT do is present the old segment as scoped to pid.
	assert.Contains(t, got, "--- run 2026-08-15T09:00:00Z pid=42",
		"the fallback shows the raw end of the log")
	assert.Contains(t, got, "--- run 2026-08-15T08:00:00Z pid=4242 ---",
		"the fallback is unscoped: it includes the old marker line itself")
}

// TestDaemonLogTail_MissingLogIsEmpty keeps the no-log case at "".
func TestDaemonLogTail_MissingLogIsEmpty(t *testing.T) {
	assert.Empty(t, daemonLogTail(t.TempDir(), 4242, daemonLogFallbackLines))
}
