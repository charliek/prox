package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ----------------------------------------------------------------

// converge drives EXACTLY the pattern the processes SSE handler will use: take a
// snapshot via Processes(), and re-snapshot every time the dirty latch wakes,
// until the snapshot satisfies want.
//
// Every convergence assertion in this file is written this way on purpose. The
// latch coalesces by design, so the number of wakes for a given burst is not a
// contract; what IS the contract is that the FINAL state is always observable to
// a subscriber that keeps re-snapshotting. Snapshotting BEFORE blocking is also
// what makes the loop lost-wake-free: the latch is level, so a change that
// landed before we blocked either shows in the snapshot or has left the latch
// set.
func converge(t *testing.T, sup *Supervisor, ch <-chan struct{}, want func([]domain.ProcessInfo) bool) []domain.ProcessInfo {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		snap := sup.Processes()
		if want(snap) {
			return snap
		}
		select {
		case _, ok := <-ch:
			if !ok {
				// The bus closed (supervisor stopped). Take one final snapshot before
				// giving up: the close is the end of the stream, not a state change.
				snap = sup.Processes()
				if want(snap) {
					return snap
				}
				t.Fatalf("change bus closed before the expected state was observable; last snapshot: %+v", snap)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the expected state; last snapshot: %+v", snap)
		}
	}
}

// stateIs builds a converge predicate matching one process's state.
func stateIs(name string, want domain.ProcessState) func([]domain.ProcessInfo) bool {
	return func(snap []domain.ProcessInfo) bool {
		for _, p := range snap {
			if p.Name == name {
				return p.State == want
			}
		}
		return false
	}
}

// drain clears a pending latch so the next assertion is about the NEXT change.
func drain(ch <-chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

// requireWake blocks until the dirty latch is set. Used after a SYNCHRONOUS
// transition (the supervisor call has already returned, so the notify has
// already happened) or after triggering an asynchronous one, to pin that the
// change actually reached the bus -- converge alone would be satisfied by a
// snapshot taken without any wake at all.
func requireWake(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-ch:
		require.True(t, ok, "the change bus closed instead of waking")
	case <-time.After(5 * time.Second):
		t.Fatal("no wake arrived on the change bus")
	}
}

// isClosed reports whether ch is closed, without blocking.
func isClosed(t *testing.T, ch <-chan struct{}) bool {
	t.Helper()
	select {
	case _, ok := <-ch:
		return !ok
	case <-time.After(2 * time.Second):
		return false
	}
}

// --- lifecycle --------------------------------------------------------------

// Subscribe/Unsubscribe are symmetric: the subscriber count returns to its
// baseline, the channel is closed on unsubscribe, and a repeated or unknown
// Unsubscribe is a no-op.
func TestChangeBus_SubscribeUnsubscribeCount(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{}, "3s")
	require.Equal(t, 0, sup.SubscriberCount(), "baseline")

	id1, ch1 := sup.Subscribe()
	id2, ch2 := sup.Subscribe()
	require.NotEqual(t, id1, id2, "subscription ids must be unique")
	require.Equal(t, 2, sup.SubscriberCount())

	sup.Unsubscribe(id1)
	assert.Equal(t, 1, sup.SubscriberCount())
	assert.True(t, isClosed(t, ch1), "unsubscribe must close the subscriber channel")

	// Idempotent: a repeated unsubscribe and an unknown id are both no-ops.
	sup.Unsubscribe(id1)
	sup.Unsubscribe("sup-does-not-exist")
	assert.Equal(t, 1, sup.SubscriberCount())

	sup.Unsubscribe(id2)
	assert.Equal(t, 0, sup.SubscriberCount(), "count must return to baseline")
	assert.True(t, isClosed(t, ch2))
}

// CloseEvents latches: every live subscriber is closed exactly once, the count
// drops to zero, a second Close is a no-op, and a Subscribe AFTER the close
// returns an already-closed channel (so a stream racing shutdown ends at once).
func TestChangeBus_CloseLatchesAndIsIdempotent(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{}, "3s")

	_, ch1 := sup.Subscribe()
	_, ch2 := sup.Subscribe()

	sup.CloseEvents()
	assert.Equal(t, 0, sup.SubscriberCount())
	assert.True(t, isClosed(t, ch1))
	assert.True(t, isClosed(t, ch2))

	// Idempotent (and must not double-close a channel, which would panic).
	sup.CloseEvents()
	sup.CloseEvents()

	id3, ch3 := sup.Subscribe()
	assert.True(t, isClosed(t, ch3), "Subscribe after Close must return a closed channel")
	assert.Equal(t, 0, sup.SubscriberCount(), "a post-close Subscribe must not be registered")
	sup.Unsubscribe(id3) // no-op, must not panic

	// A notify after the close is harmless.
	sup.notifyChange()
}

// A goroutine blocked on its subscriber channel unblocks when the supervisor
// stops: Supervisor.Stop latches the bus closed on every path, which is what
// lets the (timeout-free) SSE route return before the API server is shut down.
func TestChangeBus_StopUnblocksBlockedSubscriber(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"alpha": newGracefulFake(9101),
	}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	// Drain the start-time wakes so the goroutine below really blocks.
	_, ch := sup.Subscribe()

	unblocked := make(chan bool, 1)
	go func() {
		for {
			_, ok := <-ch
			if !ok {
				unblocked <- true
				return
			}
		}
	}()

	require.NoError(t, sup.Stop(context.Background()))

	select {
	case <-unblocked:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber blocked on the change bus was not released by Supervisor.Stop")
	}
}

// Stop closes the bus even on its not-running early return: a shutdown must
// always release stream subscribers, whether or not there was anything to stop.
func TestChangeBus_StopClosesBusWhenNotRunning(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{}, "3s")

	_, ch := sup.Subscribe()
	require.NoError(t, sup.Stop(context.Background()), "a not-running Stop returns nil")
	assert.True(t, isClosed(t, ch), "the not-running Stop path must still close the bus")
}

// --- coalescing -------------------------------------------------------------

// A burst of notifications never blocks the emitter and collapses into a single
// pending wake (capacity-1 level latch), including when the subscriber is not
// draining at all.
func TestChangeBus_BurstNeverBlocksEmitterAndCoalesces(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{}, "3s")
	_, ch := sup.Subscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			sup.notifyChange()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifyChange blocked on a subscriber that never drained")
	}

	assert.Equal(t, 1, len(ch), "a burst must coalesce into exactly one pending wake")
	<-ch
	assert.Equal(t, 0, len(ch), "the latch must be empty after a drain")
}

// N rapid REAL transitions: the subscriber wakes at least once and, after the
// burst, its wake-driven snapshot converges on the final state -- the property
// the SSE stream depends on (convergence, not event counts).
func TestChangeBus_RapidTransitionsConvergeOnFinalState(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"svc": newGracefulFake(9201),
	}, "3s")

	_, ch := sup.Subscribe()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	var wakes atomic.Int64
	stopReader := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				wakes.Add(1)
				// Re-entrancy: snapshot from the wake handler, exactly as the SSE
				// handler will.
				_ = sup.Processes()
			case <-stopReader:
				return
			}
		}
	}()

	for i := 0; i < 5; i++ {
		require.NoError(t, sup.StopProcess(context.Background(), "svc"))
		require.NoError(t, sup.StartProcess(context.Background(), "svc"))
	}
	// Final transition: leave it stopped so the converged state is unambiguous.
	require.NoError(t, sup.StopProcess(context.Background(), "svc"))

	close(stopReader)
	wg.Wait()
	assert.GreaterOrEqual(t, wakes.Load(), int64(1), "a burst of transitions must wake the subscriber at least once")

	// Drain whatever the latch holds and converge on the final state.
	snap := converge(t, sup, ch, stateIs("svc", domain.ProcessStateStopped))
	require.Equal(t, sup.Processes(), snap,
		"a subscriber-driven snapshot must equal a direct Processes() read")
}

// --- convergence per transition type ----------------------------------------

// start -> running, stop -> stopped, start again, and a restart-count bump are
// each observable to a wake-driven subscriber.
func TestChangeBus_ConvergesOnStartStopAndRestartCount(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"svc": newGracefulFake(9301),
	}, "3s")

	_, ch := sup.Subscribe()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	converge(t, sup, ch, stateIs("svc", domain.ProcessStateRunning))

	// Each of these calls commits its state before returning, so a wake MUST
	// already be latched by the time the call returns.
	drain(ch)
	require.NoError(t, sup.StopProcess(context.Background(), "svc"))
	requireWake(t, ch)
	converge(t, sup, ch, stateIs("svc", domain.ProcessStateStopped))

	drain(ch)
	require.NoError(t, sup.StartProcess(context.Background(), "svc"))
	requireWake(t, ch)
	converge(t, sup, ch, stateIs("svc", domain.ProcessStateRunning))

	drain(ch)
	require.NoError(t, sup.RestartProcess(context.Background(), "svc"))
	requireWake(t, ch)
	snap := converge(t, sup, ch, func(snap []domain.ProcessInfo) bool {
		for _, p := range snap {
			if p.Name == "svc" {
				return p.State == domain.ProcessStateRunning && p.RestartCount == 1
			}
		}
		return false
	})
	require.Equal(t, sup.Processes(), snap)
}

// A natural (unexpected) leader exit commits crashed inside the monitor, with no
// supervisor-level event of its own -- the path that was previously invisible to
// subscribers.
func TestChangeBus_ConvergesOnNaturalExit(t *testing.T) {
	fake := newGracefulFake(9401)
	sup := newStopSupervisor(t, map[string]*fakeProcess{"svc": fake}, "3s")

	_, ch := sup.Subscribe()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	converge(t, sup, ch, stateIs("svc", domain.ProcessStateRunning))

	// The leader exits on its own (and its group is gone): the monitor commits
	// crashed for a plain process, rc=0 included. No supervisor-level event
	// accompanies it, so the monitor's own notify is the only thing that can wake
	// the bus here.
	drain(ch)
	fake.setAlive(false)
	fake.exitLeader(nil)
	requireWake(t, ch)

	snap := converge(t, sup, ch, stateIs("svc", domain.ProcessStateCrashed))
	require.Equal(t, sup.Processes(), snap)
}

// A health STATUS flip fires the injected transition callback -- once per change,
// not once per check -- and fires it with h.mu released (the callback below reads
// the checker's own state, which would deadlock if the lock were still held).
func TestChangeBus_HealthTransitionFiresCallbackOnChangeOnly(t *testing.T) {
	var calls atomic.Int64
	var statuses sync.Map

	cfg := domain.HealthConfig{
		Cmd:         "false", // always fails
		Interval:    20 * time.Millisecond,
		Timeout:     time.Second,
		Retries:     1,
		StartPeriod: time.Millisecond,
	}

	var checker *HealthChecker
	checker = NewHealthChecker("svc", cfg, func() {
		calls.Add(1)
		// Re-entrancy check: reading State() from the callback must not deadlock,
		// which pins that the callback fires AFTER h.mu is released.
		statuses.Store(string(checker.State().Status), true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker.Start(ctx)
	t.Cleanup(checker.Stop)

	require.Eventually(t, func() bool { return checker.Status() == domain.HealthStatusUnhealthy },
		3*time.Second, 5*time.Millisecond, "the checker never went unhealthy")

	// Let several more (identically failing) checks run: the status does not
	// change, so no further callbacks may fire.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int64(1), calls.Load(),
		"the transition callback must fire once per status CHANGE, not once per check")

	_, ok := statuses.Load(string(domain.HealthStatusUnhealthy))
	assert.True(t, ok, "the callback must observe the committed status")
}

// A health flip on a live process reaches the bus. The flip is TEST-triggered
// (the check command tests for a file the test creates) rather than merely
// time-triggered, so the wake can be required rather than raced against.
func TestChangeBus_ConvergesOnHealthFlip(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})
	t.Cleanup(func() { logMgr.Close() })

	healthyMarker := filepath.Join(t.TempDir(), "healthy")

	cfg := makeTestConfig(map[string]string{"svc": "sleep 30"})
	proc := cfg.Processes["svc"]
	proc.Healthcheck = &config.HealthcheckConfig{
		Cmd:         "test -f " + healthyMarker,
		Interval:    "20ms",
		Timeout:     "2s",
		Retries:     1,
		StartPeriod: "1ms",
	}
	cfg.Processes["svc"] = proc

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())
	_, ch := sup.Subscribe()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	healthIs := func(want domain.HealthStatus) func([]domain.ProcessInfo) bool {
		return func(snap []domain.ProcessInfo) bool {
			for _, p := range snap {
				if p.Name == "svc" {
					return p.Health == want
				}
			}
			return false
		}
	}

	// unknown -> unhealthy (the marker does not exist yet).
	converge(t, sup, ch, healthIs(domain.HealthStatusUnhealthy))

	// unhealthy -> healthy, triggered by the test.
	drain(ch)
	require.NoError(t, os.WriteFile(healthyMarker, []byte("ok"), 0o600))
	requireWake(t, ch)

	snap := converge(t, sup, ch, healthIs(domain.HealthStatusHealthy))
	require.Equal(t, sup.Processes(), snap)
}

// A gated process's waiting -> running transition (WaitingOn is
// ProcessInfo-visible) reaches the bus at both ends of the dependency gate.
func TestChangeBus_ConvergesOnWaitingThenRunning(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober, nil)
	t.Cleanup(func() { logMgr.Close() })

	_, ch := sup.Subscribe()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	waiting := converge(t, sup, ch, stateIs("web", domain.ProcessStateWaiting))
	for _, p := range waiting {
		if p.Name == "web" {
			assert.Equal(t, []string{"db"}, p.WaitingOn, "WaitingOn must be visible in the converged snapshot")
		}
	}

	drain(ch)
	prober.release("db")
	requireWake(t, ch)
	snap := converge(t, sup, ch, stateIs("web", domain.ProcessStateRunning))
	require.Equal(t, sup.Processes(), snap)
}

// --- deadlock / re-entrancy -------------------------------------------------

// The re-entrancy pattern the SSE handler will use: several subscribers block on
// their latch and call Processes() (s.mu -> p.mu -> h.mu) straight from the wake
// handler while lifecycle transitions run concurrently. Under -race this pins
// both the lock discipline (no notify fires under a supervisor/process lock) and
// the absence of data races on the bus itself.
func TestChangeBus_SnapshotFromWakeHandlerUnderChurn(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"a": newGracefulFake(9501),
		"b": newGracefulFake(9502),
	}, "3s")

	const subscribers = 4
	var wg sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		_, ch := sup.Subscribe()
		wg.Add(1)
		go func(ch <-chan struct{}) {
			defer wg.Done()
			for {
				_, ok := <-ch
				if !ok {
					// Final snapshot after end-of-stream, mirroring the handler's exit.
					_ = sup.Processes()
					return
				}
				_ = sup.Processes()
			}
		}(ch)
	}

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	var churn sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		churn.Add(1)
		go func(name string) {
			defer churn.Done()
			for i := 0; i < 5; i++ {
				_ = sup.StopProcess(context.Background(), name)
				_ = sup.StartProcess(context.Background(), name)
			}
		}(name)
	}
	churn.Wait()

	// Stop closes the bus, which is what releases every subscriber goroutine.
	_ = sup.Stop(context.Background())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wake-handler subscribers did not drain after the supervisor stopped (possible deadlock)")
	}
	assert.Equal(t, 0, sup.SubscriberCount())
}

// TestChangeBus_FilteredStartWakesForDormantTaskRegistration pins the codex C10
// finding: the supervisor_start emit fires before process/task registration, so
// a pre-start subscriber woken by it could snapshot an incomplete process set
// and — when the filter schedules nothing further — never wake again. The
// post-registration notify closes that window: a subscriber that keeps
// re-snapshotting must observe the dormant task entry.
func TestChangeBus_FilteredStartWakesForDormantTaskRegistration(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })

	cfg := &config.Config{
		API:       config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{"web": {Cmd: "sleep 60"}},
		Tasks:     map[string]config.TaskConfig{"migrate": {Cmd: "sleep 60"}},
	}
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	_, ch := sup.Subscribe()

	// Filtered start naming only the process: the task is registered (dormant)
	// but never scheduled, so registration is the LAST visible change.
	_, err := sup.StartProcesses(context.Background(), []string{"web"})
	require.NoError(t, err)
	t.Cleanup(func() { stopSup(t, sup) })

	snap := converge(t, sup, ch, func(snap []domain.ProcessInfo) bool {
		var sawTask bool
		for _, p := range snap {
			if p.Name == "migrate" {
				sawTask = true
			}
		}
		return sawTask
	})
	require.Equal(t, sup.Processes(), snap)
}
