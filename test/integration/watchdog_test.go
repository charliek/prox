package integration

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
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
//	C6b (here)           per-TEST budget. A test that is genuinely deadlocked --
//	                     blocked on a channel, a mutex, an unbounded read --
//	                     never reaches a polling helper at all, so C6 cannot see
//	                     it. This can: it fires on wall clock, dumps EVERY
//	                     goroutine, and names the test.
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

// startTest installs a per-test watchdog and returns the deadline every polling
// helper in this test must be bounded by.
//
// The returned deadline is also recorded, keyed by test name, so `within` can
// enforce that bound WITHOUT every helper call having to thread it by hand. The
// registry is the stronger guarantee of the two: a returned value can be
// forgotten at any one of ~150 call sites, whereas a helper that goes through
// `within` cannot outlive its test's budget even if the test never touched the
// return value.
func startTest(t *testing.T, budget time.Duration) time.Time {
	t.Helper()

	deadline := time.Now().Add(budget)
	name := t.Name()
	testDeadlines.set(name, deadline)

	timer := time.AfterFunc(budget, func() { abortOnWatchdog(name, budget, deadline) })
	t.Cleanup(func() {
		timer.Stop()
		testDeadlines.clear(name)
	})
	return deadline
}

// abortOnWatchdog dumps every goroutine's stack, naming the test, and then ends
// the run.
//
// Aborting is the correct outcome and not a judgement call: a test past its
// budget is deadlocked, which is a failure however it is eventually reported.
// The only question is whether it is reported now, with the stacks that explain
// it, or in twenty minutes with a package name and nothing else.
//
// Killing the process does not leak daemons. Every daemon this run started is
// in the cross-run ledger (leakguard_test.go) before it can be leaked, and the
// NEXT run sweeps ledgers whose owner is gone -- which is exactly the case a
// violent exit produces. Reaping here instead would mean running a 45s waited
// shutdown per daemon from inside a process that is, by hypothesis, wedged.
func abortOnWatchdog(name string, budget time.Duration, deadline time.Time) {
	bar := strings.Repeat("=", 72)
	fmt.Fprintf(os.Stderr, "\n%s\nprox integration WATCHDOG: test %s exceeded its %v budget\n"+
		"(deadline was %s, now %s). It is deadlocked; every goroutine follows.\n%s\n%s\n%s\n",
		bar, name, budget,
		deadline.Format(clockFormat), time.Now().Format(clockFormat),
		bar, allGoroutineStacks(), bar)

	// A panic from this timer goroutine is unrecoverable by design: `go test`
	// reports the package as FAILED and the message names the test. os.Exit
	// would be quieter but would also skip the runtime's own final flush.
	panic(fmt.Sprintf("prox integration watchdog: test %s exceeded its %v budget", name, budget))
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
