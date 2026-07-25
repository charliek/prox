package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// --- fake clock -------------------------------------------------------------
//
// fakeClock is a deterministic Clock for the resolver tests. Time never advances
// on its own; a test moves it with Advance or fires the resolver's pending timers
// explicitly. Every timer the resolver creates carries a monotonically-increasing
// registration sequence so a test can wait for the SPECIFIC timer the resolver
// arms after a probe is answered (the interval wait), rather than racing the
// transient per-attempt-cap timer that is stopped during the hand-off.
type fakeClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	seq    int
	timers []*fakeTimer
}

type fakeTimer struct {
	c       chan time.Time
	at      time.Time
	seq     int
	fired   bool
	stopped bool
	fc      *fakeClock
}

func newFakeClock() *fakeClock {
	fc := &fakeClock{now: time.Unix(0, 0)}
	fc.cond = sync.NewCond(&fc.mu)
	return fc
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) NewTimer(d time.Duration) Timer {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.seq++
	t := &fakeTimer{c: make(chan time.Time, 1), at: fc.now.Add(d), seq: fc.seq, fc: fc}
	if d <= 0 {
		t.fired = true
		t.c <- fc.now
	} else {
		fc.timers = append(fc.timers, t)
	}
	fc.cond.Broadcast()
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	fc := t.fc
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	fc.removeLocked(t)
	fc.cond.Broadcast()
	return true
}

func (fc *fakeClock) removeLocked(t *fakeTimer) {
	for i, x := range fc.timers {
		if x == t {
			fc.timers = append(fc.timers[:i], fc.timers[i+1:]...)
			return
		}
	}
}

// seqNow returns the current registration sequence (number of timers ever armed).
func (fc *fakeClock) seqNow() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.seq
}

// waitNew blocks until a timer registered strictly after `since` is pending.
func (fc *fakeClock) waitNew(since int) {
	fc.mu.Lock()
	for fc.seq <= since {
		fc.cond.Wait()
	}
	fc.mu.Unlock()
}

// waitPending blocks until at least n timers are pending.
func (fc *fakeClock) waitPending(n int) {
	fc.mu.Lock()
	for len(fc.timers) < n {
		fc.cond.Wait()
	}
	fc.mu.Unlock()
}

// fireEarliest waits for at least one pending timer, then advances now to the
// earliest timer's deadline and fires it (and anything else now due).
func (fc *fakeClock) fireEarliest() {
	fc.mu.Lock()
	for len(fc.timers) == 0 {
		fc.cond.Wait()
	}
	earliest := fc.timers[0].at
	for _, t := range fc.timers[1:] {
		if t.at.Before(earliest) {
			earliest = t.at
		}
	}
	fc.now = earliest
	fc.fireDueLocked()
	fc.mu.Unlock()
}

// Advance moves now forward by d and fires every timer that becomes due.
func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)
	fc.fireDueLocked()
	fc.mu.Unlock()
}

func (fc *fakeClock) fireDueLocked() {
	var live []*fakeTimer
	for _, t := range fc.timers {
		if !t.at.After(fc.now) {
			t.fired = true
			t.c <- fc.now
		} else {
			live = append(live, t)
		}
	}
	fc.timers = live
	fc.cond.Broadcast()
}

func (fc *fakeClock) elapsed() time.Duration { return fc.Now().Sub(time.Unix(0, 0)) }

// --- scripted prober --------------------------------------------------------
//
// scriptProber blocks on every Probe until the test supplies a result (or the
// attempt's ctx is canceled). Because probes are never instant, the resolver's
// per-attempt timer coexists with a genuinely-blocked probe, so the test drives
// each attempt explicitly.
type scriptProber struct {
	calls chan *probeCall
}

type probeCall struct {
	check  domain.DependencyCheck
	ctx    context.Context
	result chan error
}

func newScriptProber() *scriptProber {
	return &scriptProber{calls: make(chan *probeCall, 16)}
}

func (p *scriptProber) Probe(ctx context.Context, check domain.DependencyCheck) error {
	call := &probeCall{check: check, ctx: ctx, result: make(chan error, 1)}
	p.calls <- call
	select {
	case err := <-call.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *scriptProber) nextCall(t *testing.T) *probeCall {
	t.Helper()
	select {
	case c := <-p.calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a probe call")
		return nil
	}
}

func (p *scriptProber) assertNoCall(t *testing.T) {
	t.Helper()
	select {
	case c := <-p.calls:
		t.Fatalf("unexpected probe call: %+v", c.check)
	default:
	}
}

var errNotReady = errors.New("not ready")

// --- driver -----------------------------------------------------------------
//
// driver sequences the resolver's attempts deterministically: answer() responds
// to the pending probe and records the attempt-cap timer's sequence; tick() then
// waits for the interval timer armed AFTER that answer and fires it, advancing
// exactly one interval without ever grabbing the (now stopped) cap timer.
type driver struct {
	t        *testing.T
	prober   *scriptProber
	clk      *fakeClock
	baseline int
}

func newDriver(t *testing.T, prober *scriptProber, clk *fakeClock) *driver {
	return &driver{t: t, prober: prober, clk: clk}
}

func (d *driver) answer(err error) {
	d.t.Helper()
	call := d.prober.nextCall(d.t)
	d.baseline = d.clk.seqNow() // this attempt's cap timer is already armed
	call.result <- err
}

func (d *driver) tick() {
	d.t.Helper()
	d.clk.waitNew(d.baseline) // the interval timer armed after the answer
	d.clk.fireEarliest()
}

// --- fake start runner ------------------------------------------------------

type fakeStartRunner struct {
	calls int32
}

func (s *fakeStartRunner) Run(ctx context.Context, name, cmd string) error {
	atomic.AddInt32(&s.calls, 1)
	return nil
}

func (s *fakeStartRunner) count() int { return int(atomic.LoadInt32(&s.calls)) }

// errStartRunner always returns an error, simulating a non-zero start exit.
type errStartRunner struct{}

func (errStartRunner) Run(ctx context.Context, name, cmd string) error {
	return errors.New("exit status 1")
}

// --- helpers ----------------------------------------------------------------

func tcpCheck(timeout, interval time.Duration) domain.DependencyConfig {
	return domain.DependencyConfig{
		Name:      "dep",
		Check:     domain.DependencyCheck{Kind: domain.CheckKindTCP, Target: "127.0.0.1:1", Timeout: timeout, Interval: interval},
		OnFailure: domain.FailurePolicyFail,
	}
}

func newTestResolver(cfg domain.DependencyConfig, prober Prober, start StartRunner, clk Clock, log LogFunc) *Resolver {
	opts := []ResolverOption{WithProber(prober), WithClock(clk)}
	if start != nil {
		opts = append(opts, WithStartRunner(start))
	}
	return NewResolver(map[string]domain.DependencyConfig{cfg.Name: cfg}, "", nil, log, opts...)
}

func demand(r *Resolver, ctx context.Context, name string) <-chan DepOutcome {
	ch := make(chan DepOutcome, 1)
	go func() { ch <- r.Demand(ctx, name) }()
	return ch
}

func waitOutcome(t *testing.T, ch <-chan DepOutcome) DepOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for outcome")
		return DepOutcome{}
	}
}

// --- state machine tests ----------------------------------------------------

func TestResolverHealthyFirstTry(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	cfg.Start = "should-not-run"
	prober := newScriptProber()
	start := &fakeStartRunner{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, start, clk, nil)

	ch := demand(r, context.Background(), cfg.Name)
	prober.nextCall(t).result <- nil // initial check passes

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy", o.State)
	}
	if start.count() != 0 {
		t.Fatalf("start invoked %d times; must be 0 when initial check passes", start.count())
	}
	if snap, _ := r.Snapshot(cfg.Name); snap.StartInvoked {
		t.Fatal("snapshot reports StartInvoked; start must not run on healthy-first-try")
	}
}

func TestResolverStartOnceThenPollToHealthy(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	cfg.Start = "bring-it-up"
	prober := newScriptProber()
	start := &fakeStartRunner{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, start, clk, nil)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // initial fail -> start once -> polling
	d.tick()
	d.answer(errNotReady) // poll 1 fail
	d.tick()
	d.answer(nil) // poll 2 healthy

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy", o.State)
	}
	if start.count() != 1 {
		t.Fatalf("start invoked %d times; want exactly 1", start.count())
	}
}

func TestResolverStartFailsNonZeroKeepsPolling(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	cfg.Start = "exits-nonzero"
	prober := newScriptProber()
	logs := &logCapture{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, errStartRunner{}, clk, logs.log)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // initial fail -> start (fails) -> poll
	d.tick()
	d.answer(nil) // poll succeeds

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy (polling continues after non-zero start)", o.State)
	}
	if !logs.contains("start command failed") {
		t.Fatalf("expected a logged start failure; logs: %v", logs.lines())
	}
}

func TestResolverBudgetExhaustedFail(t *testing.T) {
	// timeout 3s, interval 1s -> attempts at t=0 (initial), t=1, t=2; the t=3
	// tick lands on the deadline and dispatches nothing -> terminal.
	cfg := tcpCheck(3*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // t=0
	d.tick()
	d.answer(errNotReady) // t=1
	d.tick()
	d.answer(errNotReady) // t=2
	d.tick()              // -> t=3 == deadline, terminal

	o := waitOutcome(t, ch)
	if o.State != DepStateFailed {
		t.Fatalf("state = %s, want failed", o.State)
	}
	if o.Err == nil {
		t.Fatal("failed outcome should carry the last error")
	}
}

func TestResolverBudgetExhaustedWarn(t *testing.T) {
	cfg := tcpCheck(3*time.Second, time.Second)
	cfg.OnFailure = domain.FailurePolicyWarn
	prober := newScriptProber()
	logs := &logCapture{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, logs.log)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady)
	d.tick()
	d.answer(errNotReady)
	d.tick()
	d.answer(errNotReady)
	d.tick()

	o := waitOutcome(t, ch)
	if o.State != DepStateWarned {
		t.Fatalf("state = %s, want warned", o.State)
	}
	if !o.Ready() {
		t.Fatal("warned outcome should be Ready() (coordinator proceeds)")
	}
	if !logs.contains("on_failure: warn") {
		t.Fatalf("expected a warn log; logs: %v", logs.lines())
	}
}

func TestResolverNoStartJustPolls(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second) // no Start
	prober := newScriptProber()
	start := &fakeStartRunner{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, start, clk, nil)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // initial fail
	d.tick()
	d.answer(errNotReady) // poll 1
	d.tick()
	d.answer(nil) // poll 2 healthy

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy", o.State)
	}
	if start.count() != 0 {
		t.Fatalf("no start configured but runner invoked %d times", start.count())
	}
}

// --- single-flight ----------------------------------------------------------

func TestResolverSingleFlight(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	cfg.Start = "up"
	prober := newScriptProber()
	start := &fakeStartRunner{}
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, start, clk, nil)
	d := newDriver(t, prober, clk)

	const n = 25
	outcomes := make([]<-chan DepOutcome, n)
	for i := range outcomes {
		outcomes[i] = demand(r, context.Background(), cfg.Name)
	}

	// Exactly one resolution runs: initial fail, start once, one poll -> healthy.
	d.answer(errNotReady)
	d.tick()
	d.answer(nil)

	for i := range outcomes {
		o := waitOutcome(t, outcomes[i])
		if o.State != DepStateHealthy {
			t.Fatalf("demander %d: state = %s, want healthy", i, o.State)
		}
	}
	if start.count() != 1 {
		t.Fatalf("start invoked %d times across %d demanders; want exactly 1", start.count(), n)
	}
	prober.assertNoCall(t)
}

// --- cancellation -----------------------------------------------------------

func TestResolverCancelMidPollIsCanceledNotFailed(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)

	ch := demand(r, context.Background(), cfg.Name)
	call := prober.nextCall(t)
	baseline := clk.seqNow() // the initial attempt's cap timer is armed
	call.result <- errNotReady

	// Wait until the resolution has parked on the interval timer, then cancel.
	clk.waitNew(baseline)
	r.Close()

	o := waitOutcome(t, ch)
	if o.State != DepStateCanceled {
		t.Fatalf("state = %s, want canceled (not failed)", o.State)
	}
	if !o.Canceled() {
		t.Fatal("Canceled() should be true")
	}
}

func TestResolverCallerCtxCancelDoesNotFailResolution(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)

	cctx, ccancel := context.WithCancel(context.Background())
	ch := demand(r, cctx, cfg.Name)
	call := prober.nextCall(t)
	baseline := clk.seqNow() // the initial attempt's cap timer is armed
	call.result <- errNotReady
	clk.waitNew(baseline) // park on the interval timer

	// Cancel only the caller's ctx: that caller unblocks canceled; the shared
	// resolution keeps running for a second demander.
	ccancel()
	o := waitOutcome(t, ch)
	if o.State != DepStateCanceled {
		t.Fatalf("caller-cancel state = %s, want canceled", o.State)
	}

	ch2 := demand(r, context.Background(), cfg.Name)
	clk.fireEarliest() // fire the interval timer the resolution is parked on
	prober.nextCall(t).result <- nil
	o2 := waitOutcome(t, ch2)
	if o2.State != DepStateHealthy {
		t.Fatalf("joined demander state = %s, want healthy (resolution survived caller cancel)", o2.State)
	}
}

// --- generations ------------------------------------------------------------

func TestResolverResetStartsFreshGeneration(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)

	ch1 := demand(r, context.Background(), cfg.Name)
	call := prober.nextCall(t)
	baseline := clk.seqNow() // gen 1's initial attempt cap timer is armed
	call.result <- errNotReady
	clk.waitNew(baseline) // gen 1 parked on the interval timer

	// Reset invalidates gen 1: its demander gets canceled, next Demand is fresh.
	r.Reset(cfg.Name)
	o1 := waitOutcome(t, ch1)
	if o1.State != DepStateCanceled {
		t.Fatalf("gen1 outcome = %s, want canceled after Reset", o1.State)
	}

	ch2 := demand(r, context.Background(), cfg.Name)
	prober.nextCall(t).result <- nil // gen 2 initial check passes
	o2 := waitOutcome(t, ch2)
	if o2.State != DepStateHealthy {
		t.Fatalf("gen2 outcome = %s, want healthy; gen1's canceled outcome must not leak", o2.State)
	}
}

// --- per-attempt bound ------------------------------------------------------

func TestResolverPerAttemptBoundDoesNotEatBudget(t *testing.T) {
	// Budget 30s, interval 1s, attempt cap 2s. A hung probe is canceled after 2s
	// (min(2s, remaining)) so it does not consume the whole budget: subsequent
	// attempts still run.
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)

	ch := demand(r, context.Background(), cfg.Name)

	// Initial attempt hangs (never answered). The attempt-cap timer (2s) is the
	// only pending timer while the probe blocks. Advancing 2s fires it, canceling
	// the probe via its attempt ctx -- scriptProber then returns ctx.Err() and the
	// resolution records a failure and moves on to polling (budget NOT the
	// resolution ctx, so this is not a canceled outcome).
	prober.nextCall(t)
	clk.waitPending(1) // the attempt-cap timer
	clk.Advance(2 * time.Second)

	// ~28s of budget remain. Drive one poll to a healthy result.
	clk.fireEarliest() // the interval timer
	prober.nextCall(t).result <- nil

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy; a single hung attempt must not exhaust the budget", o.State)
	}
	if el := clk.elapsed(); el >= 30*time.Second {
		t.Fatalf("elapsed %s reached the budget; the hung attempt ate it", el)
	}
}

// --- boundary ---------------------------------------------------------------

// TestResolverBoundarySuccessBeforeDeadlineWins documents and pins the boundary
// semantics: a check whose success is observed at an interval tick STRICTLY
// before the overall deadline wins (healthy).
func TestResolverBoundarySuccessBeforeDeadlineWins(t *testing.T) {
	// timeout 2500ms, interval 1s: ticks land at t=1s and t=2s, both < deadline
	// (2.5s). The success at t=2s is observed before the deadline -> healthy.
	cfg := tcpCheck(2500*time.Millisecond, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // t=0 initial fail
	d.tick()              // -> t=1
	d.answer(errNotReady) // t=1 fail
	d.tick()              // -> t=2 (< 2.5s deadline)
	d.answer(nil)         // t=2 success wins

	o := waitOutcome(t, ch)
	if o.State != DepStateHealthy {
		t.Fatalf("state = %s, want healthy (success before deadline wins)", o.State)
	}
}

// TestResolverBoundaryTickAtDeadlineTerminates pins the other side: when the
// next interval tick lands exactly at the deadline, no attempt is dispatched and
// the resolution terminates (failed).
func TestResolverBoundaryTickAtDeadlineTerminates(t *testing.T) {
	// timeout 2s, interval 1s: tick at t=1 (dispatch, fail), tick at t=2 ==
	// deadline (no dispatch) -> failed.
	cfg := tcpCheck(2*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)
	d := newDriver(t, prober, clk)

	ch := demand(r, context.Background(), cfg.Name)
	d.answer(errNotReady) // t=0 initial fail
	d.tick()              // -> t=1
	d.answer(errNotReady) // t=1 fail
	d.tick()              // -> t=2 == deadline, no dispatch

	o := waitOutcome(t, ch)
	if o.State != DepStateFailed {
		t.Fatalf("state = %s, want failed (tick at deadline dispatches nothing)", o.State)
	}
	prober.assertNoCall(t)
}

// --- unknown / closed -------------------------------------------------------

func TestResolverUnknownDependency(t *testing.T) {
	r := NewResolver(map[string]domain.DependencyConfig{}, "", nil, nil)
	o := r.Demand(context.Background(), "nope")
	if o.State != DepStateFailed {
		t.Fatalf("unknown dependency state = %s, want failed", o.State)
	}
}

func TestResolverClosedReturnsCanceled(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)
	r.Close()
	o := r.Demand(context.Background(), cfg.Name)
	if o.State != DepStateCanceled {
		t.Fatalf("state after Close = %s, want canceled", o.State)
	}
}

// --- adversarial-review regression tests ------------------------------------

// probeFunc adapts a function to the Prober interface for synchronous scripted
// probes.
type probeFunc func(ctx context.Context, check domain.DependencyCheck) error

func (f probeFunc) Probe(ctx context.Context, check domain.DependencyCheck) error {
	return f(ctx, check)
}

// TestResolverPublishAfterCancelDemotesToCanceled (Fix 1): a Reset that lands in
// the window between resolve computing a terminal verdict and publishing it must
// demote the published outcome to canceled, so the demander never sees the stale
// pre-cancel result. The testAfterResolve hook forces the interleaving
// deterministically.
func TestResolverPublishAfterCancelDemotesToCanceled(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)
	// After resolve returns Healthy but before it publishes, retire the node.
	r.testAfterResolve = func() { r.Reset(cfg.Name) }

	ch := demand(r, context.Background(), cfg.Name)
	prober.nextCall(t).result <- nil // initial check would pass -> Healthy

	o := waitOutcome(t, ch)
	if o.State != DepStateCanceled {
		t.Fatalf("state = %s, want canceled (retirement raced publication)", o.State)
	}
}

// TestResolverConcurrentDemandResetStress races many Demand+Reset pairs to shake
// out publish/cancel data races (run under -race). Every outcome must be a valid
// terminal, never a torn/empty state.
func TestResolverConcurrentDemandResetStress(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	// A prober that always succeeds immediately keeps resolutions short so many
	// generations churn.
	prober := probeFunc(func(context.Context, domain.DependencyCheck) error { return nil })
	r := newTestResolver(cfg, prober, nil, realClock{}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			o := r.Demand(context.Background(), cfg.Name)
			switch o.State {
			case DepStateHealthy, DepStateCanceled, DepStateFailed, DepStateWarned:
			default:
				t.Errorf("invalid outcome state %q", o.State)
			}
		}()
		go func() {
			defer wg.Done()
			r.Reset(cfg.Name)
		}()
	}
	wg.Wait()
}

// TestResolverCancelPreferredOverTimerTerminal (Fix 2): once the resolution is
// canceled, firing the interval timer it was parked on must still yield canceled,
// never a failed/warned terminal, regardless of which select branch wins.
func TestResolverCancelPreferredOverTimerTerminal(t *testing.T) {
	cfg := tcpCheck(2*time.Second, time.Second)
	prober := newScriptProber()
	clk := newFakeClock()
	r := newTestResolver(cfg, prober, nil, clk, nil)

	ch := demand(r, context.Background(), cfg.Name)
	call := prober.nextCall(t)
	baseline := clk.seqNow()
	call.result <- errNotReady // initial fail -> polling
	clk.waitNew(baseline)      // parked on the interval timer

	r.Close()          // cancel the resolution
	clk.fireEarliest() // fire the interval timer despite cancellation

	o := waitOutcome(t, ch)
	if o.State != DepStateCanceled {
		t.Fatalf("state = %s, want canceled (cancel must win over the timer terminal)", o.State)
	}
}

// TestResolverNilResultAtDeadlineDoesNotWin (Fix 3): a probe that returns nil but
// is observed at/after the deadline must NOT count as healthy.
func TestResolverNilResultAtDeadlineDoesNotWin(t *testing.T) {
	cfg := tcpCheck(time.Second, time.Second) // deadline at t=1
	clk := newFakeClock()
	var called int32
	prober := probeFunc(func(context.Context, domain.DependencyCheck) error {
		atomic.AddInt32(&called, 1)
		clk.Advance(time.Second) // jump the clock to the deadline before returning
		return nil               // "success" observed exactly at the deadline
	})
	r := newTestResolver(cfg, prober, nil, clk, nil)

	o := waitOutcome(t, demand(r, context.Background(), cfg.Name))
	if o.State != DepStateFailed {
		t.Fatalf("state = %s, want failed (nil at the deadline does not win)", o.State)
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Fatal("prober was never called")
	}
}

// TestResolverNilResultAfterCancelIsCanceled (Fix 3): a probe that returns nil
// concurrently with a cancellation must yield canceled, not healthy.
func TestResolverNilResultAfterCancelIsCanceled(t *testing.T) {
	cfg := tcpCheck(30*time.Second, time.Second)
	clk := newFakeClock()
	var r *Resolver
	prober := probeFunc(func(context.Context, domain.DependencyCheck) error {
		r.Close() // cancel the resolution before this success is accepted
		return nil
	})
	r = newTestResolver(cfg, prober, nil, clk, nil)

	o := waitOutcome(t, demand(r, context.Background(), cfg.Name))
	if o.State != DepStateCanceled {
		t.Fatalf("state = %s, want canceled (success after cancel is not healthy)", o.State)
	}
}

// steppingClock advances now by step on every Now() read; NewTimer is unused.
type steppingClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.now
	c.now = c.now.Add(c.step)
	return n
}

func (c *steppingClock) NewTimer(d time.Duration) Timer { return realClock{}.NewTimer(d) }

// TestResolverZeroBoundInitialGoesTerminalWithoutProbe (Fix 4): if the budget is
// already spent by the time the first attempt would dispatch (attemptFor <= 0),
// the resolver must terminate WITHOUT dispatching an unbounded probe.
func TestResolverZeroBoundInitialGoesTerminalWithoutProbe(t *testing.T) {
	cfg := tcpCheck(time.Second, time.Second)
	// step (2s) > timeout (1s): deadline is computed from the first Now(), and the
	// attemptFor's second Now() is already past it -> bound <= 0.
	clk := &steppingClock{step: 2 * time.Second}
	var called int32
	prober := probeFunc(func(context.Context, domain.DependencyCheck) error {
		atomic.AddInt32(&called, 1)
		return nil
	})
	r := newTestResolver(cfg, prober, nil, clk, nil)

	o := waitOutcome(t, demand(r, context.Background(), cfg.Name))
	if o.State != DepStateFailed {
		t.Fatalf("state = %s, want failed (zero-bound initial terminates)", o.State)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("prober was dispatched with a zero bound; it must be skipped")
	}
	if o.Err == nil {
		t.Fatal("failed outcome should carry a sentinel error")
	}
}

// --- log capture ------------------------------------------------------------

type logCapture struct {
	mu sync.Mutex
	ls []string
}

func (c *logCapture) log(format string, args ...interface{}) {
	c.mu.Lock()
	c.ls = append(c.ls, fmt.Sprintf(format, args...))
	c.mu.Unlock()
}

func (c *logCapture) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.ls {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (c *logCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ls...)
}
