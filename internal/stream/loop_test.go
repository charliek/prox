package stream

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sentinel errors the scripted attempts below return; the tests' Classify maps
// them onto the three Classifications.
var (
	errTransient = errors.New("transient: stream dropped")
	errUnavail   = errors.New("unavailable: nothing is listening")
	errTerminal  = errors.New("terminal: version skew")
)

// classifyBySentinel is the policy every test shares: the sentinels above map
// onto their namesake Classification, anything else is transient.
func classifyBySentinel(err error) Classification {
	switch {
	case errors.Is(err, errTerminal):
		return ClassTerminal
	case errors.Is(err, errUnavail):
		return ClassUnavailable
	default:
		return ClassTransient
	}
}

// fakeClock is the injected Now. Attempts advance it explicitly to script their
// own lifetime, which is what drives the flap guard without wall-clock waits.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeTimer is the injected After: it records every backoff duration the loop
// asks for. It fires instantly by default (so the loop never costs wall-clock
// time); with block set, the returned channel never fires, which is how the
// cancel-mid-backoff and probe-short-circuit tests pin those paths. armed
// receives a signal per after() call, letting a test synchronize on "the loop
// has reached the backoff wait" rather than racing the scheduler for it — a
// cancel issued after that signal, with block set, can only be honored by the
// backoff select's ctx branch.
type fakeTimer struct {
	mu    sync.Mutex
	waits []time.Duration
	block bool
	armed chan struct{}
}

func (f *fakeTimer) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	block := f.block
	f.mu.Unlock()

	if f.armed != nil {
		select {
		case f.armed <- struct{}{}:
		default:
		}
	}

	ch := make(chan time.Time, 1)
	if !block {
		ch <- time.Time{}
	}
	return ch
}

func (f *fakeTimer) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

// recorder collects the reported statuses.
type recorder struct {
	mu       sync.Mutex
	statuses []Status
}

func (r *recorder) add(s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, s)
}

func (r *recorder) all() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Status(nil), r.statuses...)
}

func (r *recorder) states() []State {
	all := r.all()
	out := make([]State, 0, len(all))
	for _, s := range all {
		out = append(out, s.State)
	}
	return out
}

// harness bundles the scaffolding every loop test shares: a fake clock that
// only moves when a test advances it, a timer that records each backoff, a
// status recorder, and the Loop itself.
type harness struct {
	t     *testing.T
	clk   *fakeClock
	timer *fakeTimer
	rec   *recorder
	loop  *Loop

	// attempt is the scripted connect-and-consume body; a test assigns it
	// after newHarness returns. Routing through a field rather than passing
	// the closure into newHarness means the closure can reach h.loop (to call
	// Probe from inside an attempt) with no forward declaration.
	attempt func(ctx context.Context, markSynced func()) error

	// attempts counts calls to attempt, so scripts can branch on the attempt
	// number (1-based: the first attempt sees h.attempts == 1). Written only
	// from the goroutine running Run.
	attempts int
}

// newHarness wires the shared Config — sentinel policy, status recorder, fake
// clock, recording timer — around a Loop. Tests that need a different policy
// (nil Classify, nil Attempt, nil OnStatus) build their Config directly.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clk:   newClock(),
		timer: &fakeTimer{armed: make(chan struct{}, 8)},
		rec:   &recorder{},
	}
	h.loop = NewLoop(Config{
		Attempt: func(ctx context.Context, markSynced func()) error {
			h.attempts++
			return h.attempt(ctx, markSynced)
		},
		Classify: classifyBySentinel,
		OnStatus: h.rec.add,
		Now:      h.clk.now,
		After:    h.timer.after,
	})
	return h
}

// run drives the loop to completion, then pins the dedup invariant on every
// harness test. Tests that run the loop in their own goroutine call
// assertNoConsecutiveDuplicates directly once it has returned.
func (h *harness) run(ctx context.Context) {
	h.loop.Run(ctx)
	h.assertNoConsecutiveDuplicates()
}

// waitForState blocks until the recorder has seen at least n statuses with
// state s, failing the test after 2s. Polling is the only synchronization the
// public API offers for "the loop has emitted X"; the interval is tiny and
// the deadline generous, so this cannot flake under CI load without a real
// hang.
func (h *harness) waitForState(s State, n int) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, st := range h.rec.states() {
			if st == s {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("did not observe %d × %v within 2s (saw %v)", n, s, h.rec.states())
}

// assertNoConsecutiveDuplicates pins the OnStatus dedup invariant: the loop
// never reports the same State back to back with the same error.
func (h *harness) assertNoConsecutiveDuplicates() {
	h.t.Helper()
	statuses := h.rec.all()
	for i := 1; i < len(statuses); i++ {
		assert.Falsef(h.t, sameStatus(statuses[i-1], statuses[i]),
			"consecutive duplicate status at index %d: %v", i, statuses[i].State)
	}
}

// TestRun_BackoffDoublesAndCaps pins the reconnect schedule: the wait starts at
// the base backoff and doubles per failed cycle until it clamps at the max
// (500ms, 1s, 2s, 4s, 5s, 5s). The clock never advances, so every attempt is a
// flap and none of them resets the backoff.
func TestRun_BackoffDoublesAndCaps(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts > 6 {
			return errTerminal
		}
		return errTransient
	}
	h.run(context.Background())

	require.Equal(t, 7, h.attempts)
	assert.Equal(t, []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		5 * time.Second,
		5 * time.Second,
	}, h.timer.recorded())
}

// TestRun_FlapGuardControlsBackoffReset pins the flap guard borrowed from the
// proxyd forwarder: an attempt that ends before the flap threshold leaves the
// backoff growing, while one that survives the threshold resets it to base.
func TestRun_FlapGuardControlsBackoffReset(t *testing.T) {
	// Scripted attempt lifetimes: two instant flaps, one long-lived stream
	// (>= the threshold, so it counts as a recovery), then another flap.
	lifetimes := []time.Duration{0, 0, constants.StreamReconnectFlapThreshold, 0}

	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts > len(lifetimes) {
			return errTerminal
		}
		h.clk.advance(lifetimes[h.attempts-1])
		return errTransient
	}
	h.run(context.Background())

	require.Equal(t, len(lifetimes)+1, h.attempts, "one terminal attempt follows the scripted lifetimes")
	assert.Equal(t, []time.Duration{
		500 * time.Millisecond, // flap: backoff stays at base, then doubles
		time.Second,            // flap: no reset, the doubled wait applies
		500 * time.Millisecond, // survived the threshold: reset to base
		time.Second,            // flap again: doubling resumes
	}, h.timer.recorded())
}

// TestRun_MarkSyncedTransitionsToOK pins Connecting -> OK on markSynced, and
// Reconnecting -> Syncing on the retry.
func TestRun_MarkSyncedTransitionsToOK(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, markSynced func()) error {
		if h.attempts > 1 {
			return errTerminal
		}
		markSynced()
		return errTransient
	}
	h.run(context.Background())

	assert.Equal(t, []State{
		StateConnecting,
		StateOK,
		StateReconnecting,
		StateSyncing,
		StateClosed,
	}, h.rec.states())
}

// TestRun_WithoutMarkSyncedStaysSyncing pins that an attempt which never
// synchronizes never reports OK: the loop stays in Connecting/Syncing.
func TestRun_WithoutMarkSyncedStaysSyncing(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts > 2 {
			return errTerminal
		}
		return errTransient
	}
	h.run(context.Background())

	assert.Equal(t, []State{
		StateConnecting,
		StateReconnecting,
		StateSyncing,
		StateReconnecting,
		StateSyncing,
		StateClosed,
	}, h.rec.states())
	assert.NotContains(t, h.rec.states(), StateOK)
}

// TestRun_MarkSyncedDeduplicates pins that repeated markSynced calls within one
// attempt collapse into a single OK status.
func TestRun_MarkSyncedDeduplicates(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, markSynced func()) error {
		if h.attempts > 1 {
			return errTerminal
		}
		for i := 0; i < 5; i++ {
			markSynced()
		}
		return errTransient
	}
	h.run(context.Background())

	// Exactly one OK, despite five markSynced calls.
	assert.Equal(t, []State{
		StateConnecting,
		StateOK,
		StateReconnecting,
		StateSyncing,
		StateClosed,
	}, h.rec.states())
}

// TestRun_LateMarkSyncedIgnored pins the attempt-epoch guard: a markSynced
// closure captured by one attempt is inert once that attempt has returned, so a
// straggler cannot flip a later attempt's state to OK.
func TestRun_LateMarkSyncedIgnored(t *testing.T) {
	var stale func()

	h := newHarness(t)
	h.attempt = func(_ context.Context, markSynced func()) error {
		if h.attempts > 1 {
			stale() // belongs to the finished attempt: must be a no-op
			return errTerminal
		}
		stale = markSynced // captured, deliberately not called yet
		return errTransient
	}
	h.run(context.Background())

	assert.NotContains(t, h.rec.states(), StateOK)
}

// TestRun_UnavailableParksThenProbeRetries pins that an Unavailable
// classification arms no timer at all and that a Probe delivered while the loop
// is running wakes exactly one retry.
func TestRun_UnavailableParksThenProbeRetries(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts > 1 {
			return errTerminal
		}
		// Two probes from inside the attempt coalesce into a single wake.
		h.loop.Probe()
		h.loop.Probe()
		return errUnavail
	}
	h.run(context.Background())

	require.Equal(t, 2, h.attempts)
	assert.Empty(t, h.timer.recorded(), "a parked loop must not arm a backoff timer")
	assert.Equal(t, []State{
		StateConnecting,
		StateUnavailable,
		StateSyncing,
		StateClosed,
	}, h.rec.states())
}

// TestRun_UnavailableParksUntilProbeOrCancel pins the parking behaviour from the
// outside: no retry happens on its own, a Probe releases exactly one, and
// cancelling the context unparks the loop with a Closed status.
func TestRun_UnavailableParksUntilProbeOrCancel(t *testing.T) {
	entered := make(chan int, 8)

	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		entered <- h.attempts
		return errUnavail
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.loop.Run(ctx)
	}()

	require.Equal(t, 1, <-entered)
	// Negative assertion window: this is not a backoff wait (the loop arms no
	// timer while parked), just time for a wrong implementation to misbehave.
	select {
	case n := <-entered:
		t.Fatalf("parked loop retried on its own (attempt %d)", n)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, h.timer.recorded())

	h.loop.Probe()
	require.Equal(t, 2, <-entered)

	// Cancel only after the SECOND Unavailable emission (the loop emits it
	// right before re-parking; the intervening Syncing defeats dedup), so the
	// loop is provably at-or-inside the parked select — an implementation
	// that cannot cancel while parked hangs and fails the guard below (codex
	// review: cancelling right after attempt 2 returned could be satisfied by
	// the post-attempt ctx check instead).
	h.waitForState(StateUnavailable, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the parked loop's context was cancelled")
	}

	statuses := h.rec.all()
	require.NotEmpty(t, statuses)
	last := statuses[len(statuses)-1]
	assert.Equal(t, StateClosed, last.State)
	assert.ErrorIs(t, last.Err, context.Canceled)
	h.assertNoConsecutiveDuplicates()
}

// TestProbe_Coalesces pins the capacity-1 probe channel: repeated probes
// collapse into a single pending wake.
func TestProbe_Coalesces(t *testing.T) {
	loop := NewLoop(Config{})
	loop.Probe()
	loop.Probe()
	loop.Probe()
	assert.Len(t, loop.probe, 1)
}

// TestRun_ProbeShortCircuitsBackoff pins that a probe delivered during a normal
// (transient) backoff skips the remaining wait. The injected timer never fires,
// so the loop can only proceed via the probe.
func TestRun_ProbeShortCircuitsBackoff(t *testing.T) {
	h := newHarness(t)
	h.timer.block = true
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts > 1 {
			return errTerminal
		}
		h.loop.Probe()
		return errTransient
	}
	h.run(context.Background())

	require.Equal(t, 2, h.attempts)
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, h.timer.recorded(),
		"the timer is still armed; the probe only wins the race against it")
}

// TestRun_TerminalClosesLoop pins that a terminal classification ends Run with a
// Closed status carrying the error, without arming a backoff.
func TestRun_TerminalClosesLoop(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error { return errTerminal }
	h.run(context.Background())

	require.Equal(t, 1, h.attempts)
	assert.Empty(t, h.timer.recorded())
	require.Equal(t, []State{StateConnecting, StateClosed}, h.rec.states())
	assert.ErrorIs(t, h.rec.all()[1].Err, errTerminal)
}

// TestRun_ContextCancelDuringBackoff pins prompt shutdown while the loop is
// waiting out a backoff: the injected timer never fires, so only cancellation
// can end Run.
func TestRun_ContextCancelDuringBackoff(t *testing.T) {
	entered := make(chan struct{}, 4)

	h := newHarness(t)
	h.timer.block = true
	h.attempt = func(_ context.Context, _ func()) error {
		entered <- struct{}{}
		return errTransient
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.loop.Run(ctx)
	}()

	<-entered
	// Cancel only once the loop has ARMED the backoff timer: with block set
	// the timer never fires, so from here the backoff select's ctx branch is
	// the only possible exit — an implementation missing that branch hangs
	// and fails the 2s guard below (codex review: cancelling right after the
	// attempt returned could be satisfied by the post-attempt ctx check,
	// leaving the select branch unpinned).
	<-h.timer.armed
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation mid-backoff")
	}

	statuses := h.rec.all()
	require.NotEmpty(t, statuses)
	last := statuses[len(statuses)-1]
	assert.Equal(t, StateClosed, last.State)
	assert.ErrorIs(t, last.Err, context.Canceled)
	h.assertNoConsecutiveDuplicates()
}

// TestRun_CancelWithTimerAlreadyFired pins that cancellation observed around a
// ready backoff timer never launches another attempt: the instant-fire timer
// makes the select's timer branch permanently ready, and the cancel lands
// before the loop leaves the attempt, so whichever check the loop takes next
// (post-attempt or top-of-loop after a timer win) must end in Closed with no
// second attempt.
func TestRun_CancelWithTimerAlreadyFired(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.attempt = func(_ context.Context, _ func()) error {
		cancel()
		return errTransient
	}
	h.run(ctx)

	require.Equal(t, 1, h.attempts, "no attempt may start after cancellation")
	statuses := h.rec.all()
	require.NotEmpty(t, statuses)
	last := statuses[len(statuses)-1]
	assert.Equal(t, StateClosed, last.State)
	assert.ErrorIs(t, last.Err, context.Canceled)
}

// TestRun_CleanAttemptEndReconnects pins the clean-stream-end contract: an
// attempt returning nil (server closed the stream without error, ctx still
// live) is a transient end — the loop reports Reconnecting with a nil error
// and retries, rather than treating the nil as terminal.
func TestRun_CleanAttemptEndReconnects(t *testing.T) {
	h := newHarness(t)
	h.attempt = func(_ context.Context, _ func()) error {
		if h.attempts == 1 {
			return nil
		}
		return errTerminal
	}
	h.run(context.Background())

	require.Equal(t, 2, h.attempts, "a clean end must be followed by a retry")
	assert.Equal(t, []time.Duration{500 * time.Millisecond}, h.timer.recorded())

	states := h.rec.states()
	require.Contains(t, states, StateReconnecting)
	for _, s := range h.rec.all() {
		if s.State == StateReconnecting {
			assert.NoError(t, s.Err, "clean end reports Reconnecting with nil error")
		}
	}
}

// TestRun_ContextCancelDuringAttempt pins prompt shutdown while an attempt is
// in flight: the attempt honors ctx, and the loop closes without retrying.
func TestRun_ContextCancelDuringAttempt(t *testing.T) {
	entered := make(chan struct{}, 4)

	h := newHarness(t)
	h.attempt = func(actx context.Context, _ func()) error {
		entered <- struct{}{}
		<-actx.Done()
		return actx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.loop.Run(ctx)
	}()

	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation mid-attempt")
	}

	assert.Equal(t, 1, h.attempts)
	assert.Empty(t, h.timer.recorded())
	require.Equal(t, []State{StateConnecting, StateClosed}, h.rec.states())
	assert.ErrorIs(t, h.rec.all()[1].Err, context.Canceled)
}

// TestRun_NilClassifyTreatsEverythingTransient pins the documented default
// policy. It builds its own Config: the harness always wires
// classifyBySentinel, which is exactly what this test must omit.
func TestRun_NilClassifyTreatsEverythingTransient(t *testing.T) {
	timer := &fakeTimer{}
	rec := &recorder{}
	attempts := 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	NewLoop(Config{
		Attempt: func(_ context.Context, _ func()) error {
			attempts++
			if attempts >= 3 {
				cancel() // the only way out when nothing is terminal
			}
			return errTerminal // would be terminal under classifyBySentinel
		},
		OnStatus: rec.add,
		Now:      newClock().now,
		After:    timer.after,
	}).Run(ctx)

	assert.Equal(t, 3, attempts)
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second}, timer.recorded())
	statuses := rec.all()
	last := statuses[len(statuses)-1]
	assert.Equal(t, StateClosed, last.State)
	assert.ErrorIs(t, last.Err, context.Canceled)
}

// TestRun_NilAttemptClosesWithError pins that a misconfigured Loop reports a
// terminal status instead of panicking in the consumer's goroutine.
func TestRun_NilAttemptClosesWithError(t *testing.T) {
	rec := &recorder{}
	Run(context.Background(), Config{OnStatus: rec.add})

	require.Equal(t, []State{StateClosed}, rec.states())
	assert.ErrorIs(t, rec.all()[0].Err, errNoAttempt)
}

// TestRun_NilOnStatusIsSafe pins that a Loop without a status sink still runs.
func TestRun_NilOnStatusIsSafe(t *testing.T) {
	attempts := 0
	Run(context.Background(), Config{
		Attempt: func(_ context.Context, markSynced func()) error {
			attempts++
			markSynced()
			return errTerminal
		},
		Classify: classifyBySentinel,
		Now:      newClock().now,
		After:    (&fakeTimer{}).after,
	})
	assert.Equal(t, 1, attempts)
}

// TestState_String keeps the rendered names stable for logs and status lines.
func TestState_String(t *testing.T) {
	assert.Equal(t, "connecting", StateConnecting.String())
	assert.Equal(t, "syncing", StateSyncing.String())
	assert.Equal(t, "ok", StateOK.String())
	assert.Equal(t, "reconnecting", StateReconnecting.String())
	assert.Equal(t, "unavailable", StateUnavailable.String())
	assert.Equal(t, "closed", StateClosed.String())
	assert.Equal(t, "unknown", State(99).String())
}
