package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the predicate -----------------------------------------------------------

// TestDeadStack_Predicate pins the ONE decision this feature makes: when is a
// foreground session worth tearing down?
//
// Both halves of the predicate are load-bearing and each has a case here that
// fails without it:
//
//   - drop "no live process" and the live+crashed row fires, killing a
//     developer's working stack because one of three processes died.
//   - drop "some terminal failure" and the completed-only row fires (a task
//     config that SUCCEEDED) and the stopped-only row fires (a user who stopped
//     everything through the API, or a `prox up web` that started one process
//     out of several and then stopped it).
func TestDeadStack_Predicate(t *testing.T) {
	proc := func(name string, state domain.ProcessState) domain.ProcessInfo {
		return domain.ProcessInfo{Name: name, State: state}
	}

	cases := []struct {
		name  string
		procs []domain.ProcessInfo
		want  bool
		why   string
	}{
		{
			name: "empty",
			want: false,
			why:  "an empty config has nothing to fail; `prox up` may legitimately run only the proxy and API",
		},
		{
			name:  "all running",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateRunning), proc("api", domain.ProcessStateRunning)},
			want:  false,
		},
		{
			name:  "live plus crashed",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateRunning), proc("ghost", domain.ProcessStateCrashed)},
			want:  false,
			why:   "a PARTIAL crash must never tear down the processes that are still serving",
		},
		{
			name:  "starting plus crashed",
			procs: []domain.ProcessInfo{proc("slow", domain.ProcessStateStarting), proc("ghost", domain.ProcessStateCrashed)},
			want:  false,
			why:   "starting is live: the stack has not finished coming up",
		},
		{
			name:  "waiting plus crashed",
			procs: []domain.ProcessInfo{proc("gated", domain.ProcessStateWaiting), proc("ghost", domain.ProcessStateCrashed)},
			want:  false,
			why:   "waiting is limbo, not death: the gated process is still scheduled to launch",
		},
		{
			name:  "stopping plus crashed",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateStopping), proc("ghost", domain.ProcessStateCrashed)},
			want:  false,
			why:   "stopping is still live; the session is already ending on its own terms",
		},
		{
			name:  "crashed only",
			procs: []domain.ProcessInfo{proc("ghost", domain.ProcessStateCrashed)},
			want:  true,
			why:   "the #96 case: a typo in cmd: leaves a terminal supervising nothing",
		},
		{
			name:  "blocked only",
			procs: []domain.ProcessInfo{proc("gated", domain.ProcessStateBlocked)},
			want:  true,
			why:   "blocked is terminal: a failed required dependency means it will never launch",
		},
		{
			name:  "completed only",
			procs: []domain.ProcessInfo{proc("migrate", domain.ProcessStateCompleted)},
			want:  false,
			why:   "completed is terminal SUCCESS; a task config that finished is not a failure",
		},
		{
			name:  "stopped only",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateStopped)},
			want:  false,
			why:   "everything stopped through the API is an INTENT, and a never-started process is stopped too",
		},
		{
			name:  "completed plus crashed",
			procs: []domain.ProcessInfo{proc("migrate", domain.ProcessStateCompleted), proc("ghost", domain.ProcessStateCrashed)},
			want:  true,
			why:   "one success does not cancel a failure once nothing is live",
		},
		{
			name:  "stopped plus crashed",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateStopped), proc("ghost", domain.ProcessStateCrashed)},
			want:  true,
		},
		{
			name:  "stopped plus completed",
			procs: []domain.ProcessInfo{proc("web", domain.ProcessStateStopped), proc("migrate", domain.ProcessStateCompleted)},
			want:  false,
			why:   "nothing failed, so nothing to report and nothing to exit non-zero about",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deadStack(tc.procs), tc.why)
		})
	}
}

// TestSettleProcessesFromInfo_CarriesNameStatusAndBlockedOn pins the adapter
// that lets this path reuse the settle evaluator and formatter instead of
// growing a second way to say "crashed".
func TestSettleProcessesFromInfo_CarriesNameStatusAndBlockedOn(t *testing.T) {
	got := settleProcessesFromInfo([]domain.ProcessInfo{
		{Name: "ghost", State: domain.ProcessStateCrashed},
		{Name: "gated", State: domain.ProcessStateBlocked, BlockedOn: []string{"pg"}},
	})
	require.Len(t, got, 2)
	assert.Equal(t, settleProcess{Name: "ghost", Status: "crashed"}, got[0])
	assert.Equal(t, settleProcess{Name: "gated", Status: "blocked", BlockedOn: []string{"pg"}}, got[1])

	// And the evaluator built on it renders the same verdict `prox up -d` would.
	v := evaluateProcessSettle(got)
	assert.Equal(t, []string{"ghost"}, v.crashed)
	require.Len(t, v.blocked, 1)
	assert.Equal(t, "gated", v.blocked[0].Name)
	assert.Error(t, v.err())
}

// --- the watcher -------------------------------------------------------------

// fakeStack is a deadStackSource whose snapshot a test sets directly, with the
// same coalescing capacity-1 change bus the real supervisor has (so a test can
// reproduce a dropped wake) and a subscriber count so unsubscription can be
// asserted rather than assumed.
type fakeStack struct {
	mu     sync.Mutex
	procs  []domain.ProcessInfo
	subs   map[string]chan struct{}
	nextID int
	closed bool
	// reads counts Processes() calls, so a test can prove the watcher really
	// POLLED the window rather than trusting one sample.
	reads int
}

func newFakeStack(procs ...domain.ProcessInfo) *fakeStack {
	return &fakeStack{procs: procs, subs: map[string]chan struct{}{}}
}

func (f *fakeStack) Processes() []domain.ProcessInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	out := make([]domain.ProcessInfo, len(f.procs))
	copy(out, f.procs)
	return out
}

func (f *fakeStack) Subscribe() (string, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{}, 1)
	if f.closed {
		close(ch)
		return "closed", ch
	}
	f.nextID++
	id := string(rune('a' + f.nextID))
	f.subs[id] = ch
	return id, ch
}

func (f *fakeStack) Unsubscribe(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.subs[id]
	if !ok {
		return
	}
	delete(f.subs, id)
	close(ch)
}

// set replaces the snapshot and wakes every subscriber, exactly as a real
// transition does.
func (f *fakeStack) set(procs ...domain.ProcessInfo) {
	f.mu.Lock()
	f.procs = procs
	subs := make([]chan struct{}, 0, len(f.subs))
	for _, ch := range f.subs {
		subs = append(subs, ch)
	}
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // the latch is already set; this is the real bus's behavior
		}
	}
}

// setQuietly replaces the snapshot WITHOUT a wake — the dropped-transition case
// the coalescing latch makes possible, and the reason the watcher polls its
// window instead of waiting for another wake.
func (f *fakeStack) setQuietly(procs ...domain.ProcessInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs = procs
}

// closeEvents is Supervisor.CloseEvents: every subscriber channel closes and
// later subscribers get an already-closed one.
func (f *fakeStack) closeEvents() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for id, ch := range f.subs {
		delete(f.subs, id)
		close(ch)
	}
}

func (f *fakeStack) subscriberCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *fakeStack) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// watcher timings for the unit tests: the same state machine as production, two
// orders of magnitude faster.
const (
	testDeadStackWindow   = 100 * time.Millisecond
	testDeadStackInterval = 10 * time.Millisecond
	// testDeadStackWait bounds "did it fire" / "did it stay quiet" assertions.
	// Comfortably more than the window, so a pass is not a scheduling accident.
	testDeadStackWait = 3 * time.Second
)

// startTestWatcher starts a watcher over f and returns it plus a channel closed
// when it triggers.
func startTestWatcher(f *fakeStack, triggered chan struct{}) (*deadStackWatcher, <-chan struct{}) {
	fired := make(chan struct{})
	var once sync.Once
	trigger := func(onWin func()) {
		once.Do(func() {
			// Mirrors shutdownCoordinator.TriggerWith: onWin runs only for the
			// call that actually wins the trigger, and it runs BEFORE the
			// channel closes. A fake that latched outside the once would hide
			// exactly the race this ordering exists to close.
			if onWin != nil {
				onWin()
			}
			close(fired)
			// The real coordinator's Trigger closes the channel the watcher is
			// also selecting on; mirror that so the fake cannot pass on a
			// friendlier lifecycle than production's.
			if triggered != nil {
				close(triggered)
			}
		})
	}
	w := startDeadStackWatcherWithTiming(f, trigger, triggered, testDeadStackWindow, testDeadStackInterval)
	return w, fired
}

// awaitReads blocks until the watcher has taken at least n snapshots, so a test
// that wants to change the world "after the watcher has looked" can say so
// instead of guessing with a sleep.
func awaitReads(t *testing.T, f *fakeStack, n int) {
	t.Helper()
	deadline := time.Now().Add(testDeadStackWait)
	for time.Now().Before(deadline) {
		if f.readCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the watcher took fewer than %d snapshots within %s", n, testDeadStackWait)
}

// TestDeadStackWatcher_FiresOnAnAlreadyDeadSnapshot is the case a wake-driven
// watcher gets wrong.
//
// The stack is dead BEFORE the watcher exists, and no transition ever happens
// afterwards, so no wake is ever delivered. Subscribe is a change latch, not a
// replay stream — a watcher that waited for a wake before its first look would
// hang here forever, which is precisely the shape of the mid-run TUI-failure
// fallback (the stack may have been dead for minutes by the time plain mode
// takes over) and of a stack that dies during a slow startup.
func TestDeadStackWatcher_FiresOnAnAlreadyDeadSnapshot(t *testing.T) {
	f := newFakeStack(
		domain.ProcessInfo{Name: "ghost", State: domain.ProcessStateCrashed},
		domain.ProcessInfo{Name: "migrate", State: domain.ProcessStateCompleted},
	)
	w, fired := startTestWatcher(f, make(chan struct{}))
	defer w.stop()

	select {
	case <-fired:
	case <-time.After(testDeadStackWait):
		t.Fatalf("the watcher never fired against a stack that was already dead when it started")
	}

	w.stop()
	verdict, ok := w.latchedVerdict()
	require.True(t, ok, "a watcher that triggered must latch the reason, or runUp cannot exit non-zero")
	assert.Equal(t, []string{"ghost"}, verdict.crashed)
	assert.Empty(t, verdict.blocked)
	assert.Error(t, verdict.err(), "the latched verdict is what makes `prox up` exit non-zero")

	// It polled the window rather than deciding on a single sample.
	assert.Greater(t, f.readCount(), 1, "the window must be polled, not sampled once")
	// And it let go of its subscription.
	assert.Zero(t, f.subscriberCount(), "the watcher must unsubscribe when it returns")
}

// TestDeadStackWatcher_DoesNotFireWhenTheConditionBreaksMidWindow is the other
// half: a stack that recovers inside the settle window is not a dead stack.
//
// The recovery is delivered WITHOUT a wake (setQuietly), which is the case that
// justifies polling: the real change bus is a capacity-1 coalescing latch, so a
// transition arriving while a wake is already pending is dropped, and a purely
// wake-driven watcher would fire on the stale sample it had.
func TestDeadStackWatcher_DoesNotFireWhenTheConditionBreaksMidWindow(t *testing.T) {
	f := newFakeStack(domain.ProcessInfo{Name: "flaky", State: domain.ProcessStateCrashed})
	w, fired := startTestWatcher(f, make(chan struct{}))
	defer w.stop()

	// Inside the window (which only starts once the watcher's first snapshot
	// sees the dead stack), somebody runs `prox start flaky`.
	time.Sleep(testDeadStackInterval * 2)
	f.setQuietly(domain.ProcessInfo{Name: "flaky", State: domain.ProcessStateRunning})

	select {
	case <-fired:
		t.Fatalf("the watcher tore the session down for a stack that recovered inside the window")
	case <-time.After(testDeadStackWindow * 5):
	}

	w.stop()
	_, ok := w.latchedVerdict()
	assert.False(t, ok, "no fire means no verdict, so `prox up` keeps its existing exit code")
}

// TestDeadStackWatcher_FiresAfterALaterDeath covers the ordinary path: the
// stack is healthy when the watcher starts, and dies later. The abandoned
// window must not leave the watcher deaf — it goes back to waiting on wakes.
func TestDeadStackWatcher_FiresAfterALaterDeath(t *testing.T) {
	f := newFakeStack(domain.ProcessInfo{Name: "web", State: domain.ProcessStateRunning})
	w, fired := startTestWatcher(f, make(chan struct{}))
	defer w.stop()

	select {
	case <-fired:
		t.Fatalf("the watcher fired against a running stack")
	case <-time.After(testDeadStackWindow * 2):
	}

	f.set(domain.ProcessInfo{Name: "web", State: domain.ProcessStateCrashed})
	select {
	case <-fired:
	case <-time.After(testDeadStackWait):
		t.Fatalf("the watcher missed a death that happened after it started")
	}

	w.stop()
	verdict, ok := w.latchedVerdict()
	require.True(t, ok)
	assert.Equal(t, []string{"web"}, verdict.crashed)
}

// TestDeadStackWatcher_ClosedChangeBusStopsWatching: CloseEvents means the
// supervisor is going away (Supervisor.Stop runs it from a defer). That is
// "stop watching", never "fire" — the session is already ending, and its exit
// code is not this watcher's to decide.
func TestDeadStackWatcher_ClosedChangeBusStopsWatching(t *testing.T) {
	f := newFakeStack(domain.ProcessInfo{Name: "web", State: domain.ProcessStateRunning})
	w, fired := startTestWatcher(f, make(chan struct{}))
	defer w.stop()

	// The bus closes while the stack still looks alive; then everything dies,
	// as it does during teardown. No wake can be delivered on a closed bus.
	//
	// The wait is not incidental: without it the watcher's very first snapshot
	// can land after the crash below, and the test would be asserting about a
	// dead stack it never meant to create.
	awaitReads(t, f, 1)
	f.closeEvents()
	f.setQuietly(domain.ProcessInfo{Name: "web", State: domain.ProcessStateCrashed})

	select {
	case <-fired:
		t.Fatalf("the watcher fired off a closed change bus")
	case <-time.After(testDeadStackWindow * 3):
	}

	w.stop()
	_, ok := w.latchedVerdict()
	assert.False(t, ok)
}

// TestDeadStackWatcher_IntentionalShutdownLatchesNothing: a Ctrl-C or POST
// /shutdown closes the coordinator's trigger channel. Everything is about to
// become stopped, so the watcher must let go without latching — an intentional
// shutdown still exits 0.
func TestDeadStackWatcher_IntentionalShutdownLatchesNothing(t *testing.T) {
	f := newFakeStack(domain.ProcessInfo{Name: "ghost", State: domain.ProcessStateCrashed})
	triggered := make(chan struct{})
	close(triggered) // shutdown is already under way before the watcher looks

	fired := make(chan struct{})
	var once sync.Once
	w := startDeadStackWatcherWithTiming(f, func(onWin func()) {
		once.Do(func() {
			if onWin != nil {
				onWin()
			}
			close(fired)
		})
	}, triggered, testDeadStackWindow, testDeadStackInterval)
	defer w.stop()

	select {
	case <-fired:
		t.Fatalf("the watcher fired during a shutdown somebody else requested")
	case <-time.After(testDeadStackWindow * 3):
	}

	w.stop()
	_, ok := w.latchedVerdict()
	assert.False(t, ok, "an intentional shutdown must keep `prox up` at exit 0")
}

// TestDeadStackWatcher_StopIsIdempotentAndUnsubscribes: runUp calls stop()
// before performShutdown and the watcher may also have stopped itself by then.
func TestDeadStackWatcher_StopIsIdempotentAndUnsubscribes(t *testing.T) {
	f := newFakeStack(domain.ProcessInfo{Name: "web", State: domain.ProcessStateRunning})
	w, _ := startTestWatcher(f, make(chan struct{}))

	w.stop()
	w.stop()
	assert.Zero(t, f.subscriberCount(), "stop must unsubscribe from the change bus")
}
