package integration

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is plan 027 C6b: the innermost of the three layers that keep a
// wedged test from burning a CI runner.
//
//	C8 (already landed)  job-level timeout-minutes, and `go test -timeout 20m`.
//	                     The outermost net. When it fires, the diagnostic is
//	                     "the package took too long" -- which names neither the
//	                     test nor the goroutine that stopped.
//	C6  (helpers_test.go) per-WAIT budgets, deadline-threaded, so no single poll
//	                     can outlive its caller.
//	C6b (here)           per-TEST budget, one for the body and a separate one for
//	                     the cleanup phase. A test that is genuinely deadlocked --
//	                     blocked on a channel, a mutex, an unbounded read --
//	                     never reaches a polling helper at all, so C6 cannot see
//	                     it. This can: it fires on wall clock, dumps EVERY
//	                     goroutine, and names the test and the phase.
//
// The stack dump is the point. Without it the only artifact of a hang is a
// package-level timeout twenty minutes later, and the reader has to guess.

// defaultTestBudget is the watchdog budget every test gets unless it asks for
// more.
//
// Derived from measurement, not taste. The slowest LEGITIMATE test in this
// package is TestAPI_SSEHeartbeatsKeepIdleStreamsAlive, measured at 35.4s on
// the plan 027 C6 tree: it deliberately idles through two 15s SSE heartbeat
// intervals, so its runtime is a property of the contract it tests and cannot
// be tuned down. Everything else finishes in under 11s. 110s is ~3x the
// slowest, which is enough headroom to absorb a two-core CI runner under
// `-race` and still small enough that a wedged test reports in under two
// minutes instead of at the 20-minute package timeout.
//
// A test may raise its own budget (with a comment saying why). None may
// disable it: startTest is the only way to install one, and the hygiene
// meta-test fails any test that does not.
const defaultTestBudget = 110 * time.Second

// teardownBudget is the watchdog budget the CLEANUP phase gets, separately from
// the body's.
//
// One budget for both phases was a defect in the safety mechanism itself
// (CodeRabbit, PR #108). t.Cleanup runs LIFO and startTest registers first, so
// its disarm runs LAST: every daemon teardown registered later ran while the
// watchdog was still counting down the BODY's budget, against whatever remained
// of it. A test that spent 40s of 110s and then met a wedged daemon crossed the
// budget INSIDE cleanup, and the watchdog -- the thing that exists to turn one
// bad test into one legible failure -- panicked the timer goroutine and took
// every remaining test in the package with it.
//
// Derived from what a legitimate teardown can cost, not from taste. One daemon's
// worst honest path through proxRun.teardown is stopDaemon's 45s waited shutdown
// + 20s exit wait + two 5s signal graces (75s), then launcherExitBudget (5s) and
// Kill's killReapTimeout (10s): ~90s. The most daemons any one test tears down is
// two (reap_orphans_test.go), so 180s is the worst case where BOTH are wedged at
// once. Overshooting is cheap here -- see reportWatchdog: expiry in this phase
// reports and fails the RUN rather than aborting the process, so a generous
// budget costs nothing and a tight one would manufacture failures.
const teardownBudget = 180 * time.Second

// startTest installs a per-test watchdog and returns the deadline every polling
// helper in this test's BODY must be bounded by.
//
// The returned deadline is also recorded, keyed by test name, so `within` can
// enforce that bound WITHOUT every helper call having to thread it by hand. The
// registry is the stronger guarantee of the two: a returned value can be
// forgotten at any one of ~150 call sites, whereas a helper that goes through
// `within` cannot outlive its test's budget even if the test never touched the
// return value. When the test moves into cleanup the registry entry moves with
// it, so a helper called from a t.Cleanup is bounded by the teardown budget
// rather than by a body deadline that may already have passed.
func startTest(t *testing.T, budget time.Duration) time.Time {
	t.Helper()

	// t.Context() is cancelled JUST BEFORE the first cleanup function runs
	// (testing.T.Context), and it is the only hook the testing package offers
	// for that transition: a cleanup registered here could only ever run LAST,
	// which is far too late to disarm anything.
	w := armWatchdog(t.Name(), t.Context().Done(), budget, teardownBudget, reportWatchdog)
	t.Cleanup(w.disarm)
	return w.bodyDeadline
}

// watchdogPhase names which half of a test's life the watchdog is currently
// bounding. They are budgeted separately because they fail differently: a body
// past its budget is deadlocked and cannot be allowed to continue, whereas a
// teardown past its budget has already run the test it belongs to and must not
// be allowed to take the REST of the package down with it.
type watchdogPhase string

const (
	phaseBody     watchdogPhase = "test body"
	phaseTeardown watchdogPhase = "cleanup"
)

// watchdogReport is everything an expiry can state honestly: which test, which
// phase, the budget that phase actually had, and the instant it ran out.
type watchdogReport struct {
	Name     string
	Phase    watchdogPhase
	Budget   time.Duration
	Deadline time.Time
}

// watchdog is one test's two-phase timer.
//
// report is a field rather than a direct call to reportWatchdog so that the
// mechanism can be tested without panicking the run that is testing it; see
// TestWatchdog_CleanupPhaseIsBudgetedSeparately.
type watchdog struct {
	name         string
	bodyDone     <-chan struct{}
	teardown     time.Duration
	report       func(watchdogReport)
	bodyDeadline time.Time
	stopped      chan struct{}

	mu       sync.Mutex
	timer    *time.Timer
	phase    watchdogPhase
	budget   time.Duration
	deadline time.Time
	disarmed bool
}

// armWatchdog starts a watchdog on the body budget, which becomes the teardown
// budget as soon as bodyDone is closed.
func armWatchdog(name string, bodyDone <-chan struct{}, body, teardown time.Duration, report func(watchdogReport)) *watchdog {
	w := &watchdog{
		name:     name,
		bodyDone: bodyDone,
		teardown: teardown,
		report:   report,
		stopped:  make(chan struct{}),
		phase:    phaseBody,
		budget:   body,
		deadline: time.Now().Add(body),
	}
	w.bodyDeadline = w.deadline
	testDeadlines.set(name, w.deadline)
	w.timer = time.AfterFunc(body, w.fire)
	go w.awaitTeardown()
	return w
}

// awaitTeardown re-budgets the watchdog the moment the test body returns.
func (w *watchdog) awaitTeardown() {
	select {
	case <-w.bodyDone:
		w.enterTeardown()
	case <-w.stopped:
	}
}

// fire is what the timer runs when a phase's budget expires.
//
// It re-checks the phase itself rather than trusting awaitTeardown to have got
// there first. That goroutine wakes on a channel close and in practice runs
// long before any cleanup can consume seconds, but "in practice" is not a bound:
// if the scheduler had not run it yet, reporting the BODY's budget against a
// test that has already returned is exactly the false alarm this split exists to
// remove. The check here is synchronous with the expiry, so it cannot lose that
// race.
func (w *watchdog) fire() {
	w.mu.Lock()
	if w.disarmed {
		w.mu.Unlock()
		return
	}
	if w.phase == phaseBody && w.bodyFinished() {
		w.enterTeardownLocked()
		w.mu.Unlock()
		return
	}
	rep := watchdogReport{Name: w.name, Phase: w.phase, Budget: w.budget, Deadline: w.deadline}
	w.mu.Unlock()

	w.report(rep)
}

// bodyFinished reports whether the test body has returned.
func (w *watchdog) bodyFinished() bool {
	select {
	case <-w.bodyDone:
		return true
	default:
		return false
	}
}

func (w *watchdog) enterTeardown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disarmed || w.phase != phaseBody {
		return
	}
	w.enterTeardownLocked()
}

// enterTeardownLocked re-arms the timer on the teardown budget and moves the
// deadline registry with it, so a `within` call made from a t.Cleanup is bounded
// by the phase it is actually running in.
func (w *watchdog) enterTeardownLocked() {
	w.phase = phaseTeardown
	w.budget = w.teardown
	w.deadline = time.Now().Add(w.teardown)
	testDeadlines.set(w.name, w.deadline)
	w.timer.Reset(w.teardown)
}

// disarm stops the watchdog for good. It is the LAST cleanup to run (registered
// first), which is precisely why it cannot be the thing that bounds teardown.
func (w *watchdog) disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disarmed {
		return
	}
	w.disarmed = true
	close(w.stopped)
	w.timer.Stop()
	testDeadlines.clear(w.name)
}

// teardownOverran records that some test's CLEANUP phase blew its budget, so
// that TestMain can fail a run whose tests all "passed" while a teardown hung.
var teardownOverran atomic.Bool

// reportWatchdog dumps every goroutine's stack, naming the test AND the phase,
// and then ends the run -- but only for the test body.
//
// For the BODY, aborting is the correct outcome and not a judgement call: a test
// past its budget is deadlocked, which is a failure however it is eventually
// reported. The only question is whether it is reported now, with the stacks
// that explain it, or in twenty minutes with a package name and nothing else.
//
// Killing the process does not leak daemons. Every daemon this run started is
// in the cross-run ledger (leakguard_test.go) before it can be leaked, and the
// NEXT run sweeps ledgers whose owner is gone -- which is exactly the case a
// violent exit produces. Reaping here instead would mean running a 45s waited
// shutdown per daemon from inside a process that is, by hypothesis, wedged.
//
// For CLEANUP the verdict is deliberately weaker, and this is the half that
// CodeRabbit's finding turns on. The test it belongs to has already run; the
// process is NOT known to be unusable; and the cleanup in question is most often
// a daemon teardown that is slow rather than stuck. Panicking there converts one
// slow teardown into a whole-package abort -- the exact opposite of what a
// watchdog is for. So the stacks are dumped (which is the diagnostic that was
// missing) and the run is marked failed, while every remaining test still gets
// to report its own result. A cleanup that is genuinely stuck FOREVER is left to
// the outer nets: `go test -timeout 20m` and the job's timeout-minutes.
func reportWatchdog(rep watchdogReport) {
	bar := strings.Repeat("=", 72)
	verdict := "It is deadlocked; every goroutine follows."
	if rep.Phase == phaseTeardown {
		verdict = "Its cleanup is wedged; every goroutine follows. The run will fail, but the " +
			"remaining tests are left alone -- a slow teardown must not abort the package."
	}
	fmt.Fprintf(os.Stderr, "\n%s\nprox integration WATCHDOG: test %s exceeded its %v %s budget\n"+
		"(deadline was %s, now %s). %s\n%s\n%s\n%s\n",
		bar, rep.Name, rep.Budget, rep.Phase,
		rep.Deadline.Format(clockFormat), time.Now().Format(clockFormat),
		verdict, bar, allGoroutineStacks(), bar)

	if rep.Phase == phaseTeardown {
		teardownOverran.Store(true)
		return
	}

	// A panic from this timer goroutine is unrecoverable by design: `go test`
	// reports the package as FAILED and the message names the test. os.Exit
	// would be quieter but would also skip the runtime's own final flush.
	panic(fmt.Sprintf("prox integration watchdog: test %s exceeded its %v %s budget",
		rep.Name, rep.Budget, rep.Phase))
}

// allGoroutineStacks renders every goroutine's stack, growing the buffer until
// the dump fits. runtime.Stack silently TRUNCATES to the buffer it is given,
// and a truncated dump of a deadlock is worse than useless: the goroutine that
// explains the hang is as likely as not the one that got cut off.
func allGoroutineStacks() []byte {
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < size {
			return buf[:n]
		}
		if size >= 1<<27 {
			return buf[:n]
		}
	}
}

// testDeadlines maps a running test's name to the deadline its watchdog set.
//
// Keyed by name rather than by *testing.T because the lookup has to work for
// SUBTESTS too: a subtest that installs no watchdog of its own is still bounded
// by its parent's, and `testDeadline` finds that by walking "Parent/Child" back
// up to "Parent".
var testDeadlines = &deadlineRegistry{byName: map[string]time.Time{}}

type deadlineRegistry struct {
	mu     sync.Mutex
	byName map[string]time.Time
}

func (r *deadlineRegistry) set(name string, deadline time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[name] = deadline
}

func (r *deadlineRegistry) clear(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byName, name)
}

func (r *deadlineRegistry) lookup(name string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byName[name]
	return d, ok
}

// testDeadline returns the watchdog deadline bounding t: its own if it
// installed one, otherwise the nearest ancestor's.
//
// A test with no watchdog anywhere up the chain reports false, and `within`
// then falls back to the bare role budget. That is a deliberately quiet
// fallback -- helpers must keep working -- and it is why the enforcement lives
// in the hygiene meta-test instead, where a missing watchdog is a named,
// actionable failure rather than a silently weaker bound.
func testDeadline(t *testing.T) (time.Time, bool) {
	name := t.Name()
	for {
		if d, ok := testDeadlines.lookup(name); ok {
			return d, true
		}
		slash := strings.LastIndex(name, "/")
		if slash < 0 {
			return time.Time{}, false
		}
		name = name[:slash]
	}
}

// --- the watchdog's own tests --------------------------------------------------

// fakeWatchdogBudgets are the budgets the two tests below run their fake
// watchdogs on. Milliseconds rather than the real minute-scale constants: what
// is under test is the PHASE machinery, and the wall-clock cost of proving it
// should be a second, not three minutes.
const (
	fakeBodyBudget     = 200 * time.Millisecond
	fakeTeardownBudget = 600 * time.Millisecond
	// fakeReportBudget bounds the wait for a report that must arrive. Far longer
	// than the budgets above, because it is a bound on a FAILURE (the report
	// never came), not a measurement of how quickly it should.
	fakeReportBudget = 10 * time.Second
)

// TestWatchdog_CleanupPhaseIsBudgetedSeparately is the regression test for the
// defect CodeRabbit found in the watchdog itself (PR #108).
//
// t.Cleanup runs LIFO and startTest registers first, so its disarm runs LAST:
// every daemon teardown a test registers runs while the watchdog is still armed.
// With ONE budget for the whole test, cleanup ran against whatever was left of
// the body's -- so a test that had spent most of its budget and then met a
// wedged daemon (stopDaemon can legitimately take ~75s) crossed the line inside
// cleanup, and the abort took every remaining test in the package with it.
//
// Reintroduce that by deleting the phase transition (fire's bodyFinished check
// and awaitTeardown) and this fails on its first assertion: the report arrives
// on the body's budget, ~200ms into a cleanup phase that had barely started.
func TestWatchdog_CleanupPhaseIsBudgetedSeparately(t *testing.T) {
	startTest(t, defaultTestBudget)

	reports := make(chan watchdogReport, 4)
	bodyDone := make(chan struct{})
	w := armWatchdog(t.Name()+"/fake", bodyDone, fakeBodyBudget, fakeTeardownBudget,
		func(rep watchdogReport) { reports <- rep })
	defer w.disarm()

	// The body returns almost immediately. Everything after this instant is the
	// cleanup phase -- which is where every daemon teardown in this package runs.
	time.Sleep(20 * time.Millisecond)
	close(bodyDone)

	// The body's budget must never fire against a test that has already
	// returned, however little of it was left.
	select {
	case rep := <-reports:
		t.Fatalf("the watchdog fired during cleanup on the %q budget (%v): a slow teardown would "+
			"abort the whole package", rep.Phase, rep.Budget)
	case <-time.After(fakeBodyBudget + 300*time.Millisecond):
	}

	// The deadline registry moved with the phase, so a helper called from a
	// t.Cleanup is bounded by the phase it is actually running in rather than by
	// a body deadline that has already passed.
	if d, ok := testDeadlines.lookup(w.name); !ok || !d.After(w.bodyDeadline) {
		t.Fatalf("cleanup deadline = %v (found=%v); want one later than the body deadline %v",
			d.Format(clockFormat), ok, w.bodyDeadline.Format(clockFormat))
	}

	// A wedged cleanup is still REPORTED -- the point is to re-budget the
	// watchdog, not to stop watching.
	select {
	case rep := <-reports:
		if rep.Phase != phaseTeardown {
			t.Errorf("phase = %q, want %q", rep.Phase, phaseTeardown)
		}
		if rep.Budget != fakeTeardownBudget {
			t.Errorf("reported budget = %v, want the teardown budget %v", rep.Budget, fakeTeardownBudget)
		}
	case <-time.After(fakeReportBudget):
		t.Fatal("a wedged cleanup was never reported: the watchdog stopped watching instead of re-budgeting")
	}
}

// TestWatchdog_BodyBudgetStillReportsTheBody is the other half: splitting the
// phases must not weaken the original guarantee. A test that is still RUNNING
// when its budget expires is deadlocked, and the report must say so -- naming
// the body phase and the body's budget, which is what makes reportWatchdog end
// the run rather than merely mark it.
func TestWatchdog_BodyBudgetStillReportsTheBody(t *testing.T) {
	startTest(t, defaultTestBudget)

	reports := make(chan watchdogReport, 4)
	// Never closed: the body of this fake test never returns.
	bodyDone := make(chan struct{})
	w := armWatchdog(t.Name()+"/fake", bodyDone, fakeBodyBudget, fakeTeardownBudget,
		func(rep watchdogReport) { reports <- rep })
	defer w.disarm()

	select {
	case rep := <-reports:
		if rep.Phase != phaseBody {
			t.Errorf("phase = %q, want %q", rep.Phase, phaseBody)
		}
		if rep.Budget != fakeBodyBudget {
			t.Errorf("reported budget = %v, want the body budget %v", rep.Budget, fakeBodyBudget)
		}
	case <-time.After(fakeReportBudget):
		t.Fatal("a deadlocked test body was never reported")
	}
}
