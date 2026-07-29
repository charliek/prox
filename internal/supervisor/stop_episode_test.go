package supervisor

import (
	"context"
	"os"
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

// stopGate coordinates a concurrent-Stop interleaving deterministically (no
// sleeps) via the ManagedProcess.stopBarrier seam. The primary Stop parks at
// parkPhase (default "primary-installed": episode published, state Stopping)
// until releasePrimary is closed; every secondary reports its join on
// "secondary-joined". A test waits for the primary to park and for N secondaries
// to join, then releases the primary so its verdict resolves the shared episode
// (#32, D1). Setting parkPhase to "verdict-committed" parks the primary AFTER the
// terminal state and episode were committed atomically, to probe that boundary.
type stopGate struct {
	parkPhase      string
	primaryReached chan struct{}
	releasePrimary chan struct{}
	joined         chan struct{}
}

func newStopGate() *stopGate { return newStopGateAt("primary-installed") }

func newStopGateAt(phase string) *stopGate {
	return &stopGate{
		parkPhase:      phase,
		primaryReached: make(chan struct{}),
		releasePrimary: make(chan struct{}),
		joined:         make(chan struct{}, 16),
	}
}

func (g *stopGate) barrier(phase string) {
	switch phase {
	case g.parkPhase:
		close(g.primaryReached)
		<-g.releasePrimary
	case "secondary-joined":
		g.joined <- struct{}{}
	}
}

// awaitPrimary blocks until the primary Stop has installed its episode and
// parked at the barrier.
func (g *stopGate) awaitPrimary(t *testing.T) {
	t.Helper()
	select {
	case <-g.primaryReached:
	case <-time.After(5 * time.Second):
		t.Fatal("primary Stop did not install its episode within timeout")
	}
}

// awaitJoins blocks until n secondary Stops have captured the in-flight episode.
func (g *stopGate) awaitJoins(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-g.joined:
		case <-time.After(5 * time.Second):
			t.Fatalf("secondary %d did not join the episode within timeout", i)
		}
	}
}

// goStop runs mp.Stop(ctx) on a goroutine and returns a channel carrying its
// result.
func goStop(mp *ManagedProcess, ctx context.Context) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- mp.Stop(ctx) }()
	return ch
}

// recvErr reads a Stop result with a generous timeout so a hung waiter fails the
// test loudly instead of blocking forever.
func recvErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return within timeout", what)
		return nil
	}
}

// newFastUnreapableFake models a surviving grandchild whose verdict is reached
// without the SIGKILL kill-grace wall wait: the leader exits on SIGTERM (so the
// run monitor finishes and done closes) while GroupAlive reports the group gone
// to Stop's graceful poll (call 0) yet alive to the authoritative post-reap
// verdict re-probe (call 1+). The result is a deterministic, fast
// ErrProcessGroupNotReaped -- the exact corner the verdict re-probe exists to
// catch (#32).
func newFastUnreapableFake(pid int) *fakeProcess {
	fp := newFakeProcess(pid)
	fp.onSignal = func(fp *fakeProcess, sig os.Signal) {
		if sig == sigterm {
			fp.exitLeader(nil)
		}
	}
	fp.groupAliveFn = func(call int) (bool, error) { return call >= 1, nil }
	return fp
}

// TestStopEpisode_ConcurrentCleanSameVerdict: a primary and a secondary Stop of
// a cleanly-reaped group both return nil. The secondary joins the primary's
// episode (deterministically, via the barrier) and receives the published
// verdict rather than short-circuiting to ErrProcessNotRunning.
func TestStopEpisode_ConcurrentCleanSameVerdict(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t)

	secondary := goStop(mp, context.Background())
	gate.awaitJoins(t, 1)

	close(gate.releasePrimary)

	require.NoError(t, recvErr(t, primary, "primary Stop"))
	require.NoError(t, recvErr(t, secondary, "secondary Stop"))
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestStopEpisode_ConcurrentUnreapableSameVerdict: with a group that survives,
// both the primary and the secondary Stop return ErrProcessGroupNotReaped. The
// secondary observes the primary's verdict, not a spurious success (the #32 bug).
func TestStopEpisode_ConcurrentUnreapableSameVerdict(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newFastUnreapableFake(2000 + call) })
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t)

	secondary := goStop(mp, context.Background())
	gate.awaitJoins(t, 1)

	close(gate.releasePrimary)

	perr := recvErr(t, primary, "primary Stop")
	serr := recvErr(t, secondary, "secondary Stop")
	assert.ErrorIs(t, perr, domain.ErrProcessGroupNotReaped, "primary must report the surviving group")
	assert.ErrorIs(t, serr, domain.ErrProcessGroupNotReaped, "secondary must observe the same verdict")
	assert.Equal(t, domain.ProcessStateCrashed, mp.State())
}

// TestStopEpisode_ThreeConcurrentWaitersSameVerdict: three secondaries joining
// one in-flight primary all receive the identical (nil) verdict.
func TestStopEpisode_ThreeConcurrentWaitersSameVerdict(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(3000 + call) })
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t)

	const waiters = 3
	results := make([]<-chan error, waiters)
	for i := range results {
		results[i] = goStop(mp, context.Background())
	}
	gate.awaitJoins(t, waiters)

	close(gate.releasePrimary)

	require.NoError(t, recvErr(t, primary, "primary Stop"))
	for i, ch := range results {
		require.NoErrorf(t, recvErr(t, ch, "waiter Stop"), "waiter %d must get the same nil verdict", i)
	}
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestStopEpisode_CanceledSecondaryGetsCtxErr: a secondary whose context is
// canceled before the primary's verdict lands returns ctx.Err(), while the
// primary still delivers its own verdict to completion.
func TestStopEpisode_CanceledSecondaryGetsCtxErr(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(4000 + call) })
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t)

	secCtx, secCancel := context.WithCancel(context.Background())
	secondary := goStop(mp, secCtx)
	gate.awaitJoins(t, 1)

	// The primary is still parked, so the episode is unresolved: canceling the
	// secondary's context must make it return ctx.Err() (not the not-yet-published
	// verdict).
	secCancel()
	serr := recvErr(t, secondary, "secondary Stop")
	assert.ErrorIs(t, serr, context.Canceled, "canceled secondary must return ctx.Err()")

	// The primary is unaffected and delivers its own verdict.
	close(gate.releasePrimary)
	require.NoError(t, recvErr(t, primary, "primary Stop"))
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestStopEpisode_SimultaneousReadinessPrefersVerdict: when the episode is
// already resolved AND the caller's context is already canceled, the Stopping
// branch must prefer the published verdict over ctx.Err(). Looped so the runtime
// exercises both arms of the outer select.
func TestStopEpisode_SimultaneousReadinessPrefersVerdict(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(5000 + call) })
	mp := newFakeManagedProcess(t, runner)

	verdict := domain.ErrProcessGroupNotReaped
	ep := &stopEpisode{done: make(chan struct{}), err: verdict}
	close(ep.done)

	mp.mu.Lock()
	mp.state = domain.ProcessStateStopping
	mp.episode = ep
	mp.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: both ep.done and ctx.Done() are ready

	for i := 0; i < 500; i++ {
		err := mp.Stop(ctx)
		require.ErrorIs(t, err, domain.ErrProcessGroupNotReaped,
			"a ready episode must win over an already-canceled context (iter %d)", i)
	}
}

// TestStopEpisode_RetryEpisodeIsolation: a waiter that joined episode 1 (an
// unreapable group) observes episode 1's error; a fresh retry Stop installs a
// distinct episode 2 that -- with the group now killable -- resolves nil, and
// episode 2's waiters observe nil. A late/early waiter never sees the other
// episode's verdict.
func TestStopEpisode_RetryEpisodeIsolation(t *testing.T) {
	var killable atomic.Bool
	runner := newFakeRunner(func(call int) *fakeProcess {
		fp := newFakeProcess(6000 + call)
		fp.onSignal = func(fp *fakeProcess, sig os.Signal) {
			if sig == sigterm {
				fp.exitLeader(nil)
			}
		}
		// While unreapable: gone to the graceful poll, alive to the verdict.
		// Once killable: the group is reapable, so every probe reports gone.
		fp.groupAliveFn = func(call int) (bool, error) {
			if killable.Load() {
				return false, nil
			}
			return call >= 1, nil
		}
		return fp
	})
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	// --- Episode 1: unreapable -> error, with a waiter joined to it. ---
	gate1 := newStopGate()
	mp.stopBarrier = gate1.barrier

	primary1 := goStop(mp, context.Background())
	gate1.awaitPrimary(t)
	waiter1 := goStop(mp, context.Background())
	gate1.awaitJoins(t, 1)
	close(gate1.releasePrimary)

	assert.ErrorIs(t, recvErr(t, primary1, "episode 1 primary"), domain.ErrProcessGroupNotReaped)
	assert.ErrorIs(t, recvErr(t, waiter1, "episode 1 waiter"), domain.ErrProcessGroupNotReaped,
		"a waiter on episode 1 must get episode 1's error")
	require.Equal(t, domain.ProcessStateCrashed, mp.State())

	// --- Episode 2: retry with a now-killable group -> nil, distinct episode. ---
	gate2 := newStopGate()
	mp.stopBarrier = gate2.barrier

	primary2 := goStop(mp, context.Background())
	gate2.awaitPrimary(t)
	// The group has become reapable between episodes.
	killable.Store(true)
	waiter2a := goStop(mp, context.Background())
	waiter2b := goStop(mp, context.Background())
	gate2.awaitJoins(t, 2)
	close(gate2.releasePrimary)

	require.NoError(t, recvErr(t, primary2, "episode 2 primary"))
	require.NoError(t, recvErr(t, waiter2a, "episode 2 waiter a"), "episode 2 waiters must get nil")
	require.NoError(t, recvErr(t, waiter2b, "episode 2 waiter b"), "episode 2 waiters must get nil")
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestStopEpisode_EarlyExitPathResolvesEpisode: a Stop that enters the primary
// transition but finds no live instance (the early-exit path) must still resolve
// its episode -- a waiter joined to it gets nil, never a hang.
func TestStopEpisode_EarlyExitPathResolvesEpisode(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(7000 + call) })
	mp := newFakeManagedProcess(t, runner)

	// Construct the early-exit precondition directly: state past the guards but
	// with no instance to signal.
	mp.mu.Lock()
	mp.state = domain.ProcessStateRunning
	mp.current = nil
	mp.mu.Unlock()

	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t)
	secondary := goStop(mp, context.Background())
	gate.awaitJoins(t, 1)
	close(gate.releasePrimary)

	require.NoError(t, recvErr(t, primary, "early-exit primary"))
	require.NoError(t, recvErr(t, secondary, "early-exit waiter"),
		"a waiter on the early-exit episode must get the nil verdict")
	assert.Equal(t, domain.ProcessStateStopped, mp.State())

	mp.mu.RLock()
	assert.Nil(t, mp.episode, "the resolved episode must be detached")
	mp.mu.RUnlock()
}

// TestStopEpisode_PostCommitBoundaryCleanGetsNotRunning: a Stop that enters the
// window between the primary's atomic state+episode commit and its function
// return observes the committed terminal state -- for a cleanly-reaped group it
// correctly gets ErrProcessNotRunning. Deterministic proof the torn window
// (commit visible before episode resolved) is gone (#32, D1).
func TestStopEpisode_PostCommitBoundaryCleanGetsNotRunning(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(9000 + call) })
	mp := newFakeManagedProcess(t, runner)
	require.NoError(t, mp.Start(context.Background()))

	gate := newStopGateAt("verdict-committed")
	mp.stopBarrier = gate.barrier

	primary := goStop(mp, context.Background())
	gate.awaitPrimary(t) // parked AFTER state=Stopped + episode resolved, atomically

	late := goStop(mp, context.Background())
	assert.ErrorIs(t, recvErr(t, late, "post-commit Stop"), domain.ErrProcessNotRunning,
		"a Stop after the atomic commit is not concurrent with the episode: clean -> ErrProcessNotRunning")

	close(gate.releasePrimary)
	require.NoError(t, recvErr(t, primary, "primary Stop"))
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestStopEpisode_PostCommitUnreapableRetryIsolation: for a surviving group, the
// episode-N waiter observes episode N's error (published atomically at the
// commit), and a Stop entering the post-commit window becomes a legitimate fresh
// retry episode (N+1) with its own verdict -- never observing an unpublished N
// nor N's object (#32, D1).
func TestStopEpisode_PostCommitUnreapableRetryIsolation(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newFastUnreapableFake(9100 + call) })
	mp := newFakeManagedProcess(t, runner)
	// Small budget so the retry's real SIGKILL escalation is bounded by the kill
	// grace rather than the 10s default graceful window.
	mp.shutdownTimeout = 200 * time.Millisecond
	require.NoError(t, mp.Start(context.Background()))

	var installOnce, commitOnce sync.Once
	installReached := make(chan struct{})
	releaseInstall := make(chan struct{})
	commitReached := make(chan struct{})
	releaseCommit := make(chan struct{})
	joined := make(chan struct{}, 4)
	mp.stopBarrier = func(phase string) {
		switch phase {
		case "primary-installed":
			parked := false
			installOnce.Do(func() { parked = true; close(installReached) })
			if parked {
				<-releaseInstall
			}
		case "secondary-joined":
			joined <- struct{}{}
		case "verdict-committed":
			parked := false
			commitOnce.Do(func() { parked = true; close(commitReached) })
			if parked {
				<-releaseCommit
			}
		}
	}

	// Episode N: park the primary with its episode installed, join a waiter.
	primaryN := goStop(mp, context.Background())
	<-installReached
	waiterN := goStop(mp, context.Background())
	<-joined

	// Let episode N run to its unreapable verdict (state+episode committed
	// atomically), then park just past the commit.
	close(releaseInstall)
	<-commitReached

	assert.ErrorIs(t, recvErr(t, waiterN, "episode N waiter"), domain.ErrProcessGroupNotReaped,
		"episode N waiter must observe episode N's verdict, published atomically at the commit")

	// A Stop entering the post-commit window sees state Crashed + a live group, so
	// it starts a FRESH retry episode (N+1) and runs to its own verdict.
	retry := goStop(mp, context.Background())
	assert.ErrorIs(t, recvErr(t, retry, "retry Stop"), domain.ErrProcessGroupNotReaped,
		"post-commit Stop must produce its own fresh retry verdict")

	close(releaseCommit)
	assert.ErrorIs(t, recvErr(t, primaryN, "episode N primary"), domain.ErrProcessGroupNotReaped)
	assert.Equal(t, domain.ProcessStateCrashed, mp.State())
}

// TestStopEpisode_NilEpisodeFallbackProbe: the defensive ep==nil branch (a
// Stopping state with no installed episode) waits for the run monitor and then
// takes a fresh authoritative probe -- a surviving group yields the reap error, a
// reaped group yields nil -- rather than blindly reporting success (#32, D1).
func TestStopEpisode_NilEpisodeFallbackProbe(t *testing.T) {
	run := func(t *testing.T, groupAlive, wantErr bool) {
		runner := newFakeRunner(func(call int) *fakeProcess { return newFakeProcess(9200 + call) })
		mp := newFakeManagedProcess(t, runner)
		require.NoError(t, mp.Start(context.Background()))
		fp := runner.last()

		// Force the defensive precondition directly: Stopping with no episode.
		mp.mu.Lock()
		inst := mp.current
		mp.state = domain.ProcessStateStopping
		mp.episode = nil
		mp.mu.Unlock()

		// Simulate the run monitor having finished (done closed) with the group in
		// the desired final liveness.
		fp.setAlive(groupAlive)
		inst.closeDone()

		err := mp.Stop(context.Background())
		if wantErr {
			assert.ErrorIs(t, err, domain.ErrProcessGroupNotReaped, "surviving group must yield the reap error")
		} else {
			assert.NoError(t, err, "reaped group must yield nil")
		}

		// Release the still-blocked monitor goroutine so it does not leak.
		fp.exitLeader(nil)
	}

	t.Run("surviving group -> error", func(t *testing.T) { run(t, true, true) })
	t.Run("reaped group -> nil", func(t *testing.T) { run(t, false, false) })
}

// TestStopEpisode_StopProcessNoStoppedEventOnSurvivor: through the StopProcess
// path, an unreapable group makes BOTH the primary and the secondary caller
// return ErrProcessGroupNotReaped, so StopProcess's existing suppression emits no
// process_stopped event for either -- the verdicts are now consistent (#32, D1).
func TestStopEpisode_StopProcessNoStoppedEventOnSurvivor(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })

	cfg := &config.Config{
		API: config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{
			// A comfortable budget so the secondary's StopProcess context outlives
			// the (barrier-gated, fast) primary verdict.
			"svc": {Cmd: "irrelevant", StopTimeout: "5s"},
		},
	}
	runner := newFakeRunner(func(call int) *fakeProcess { return newFastUnreapableFake(8000 + call) })
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig())

	events := sup.subscribeEvents()
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp := getManagedProcess(t, sup, "svc")
	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	stop := func() <-chan error {
		ch := make(chan error, 1)
		go func() { ch <- sup.StopProcess(context.Background(), "svc") }()
		return ch
	}
	primary := stop()
	gate.awaitPrimary(t)
	secondary := stop()
	gate.awaitJoins(t, 1)
	close(gate.releasePrimary)

	assert.ErrorIs(t, recvErr(t, primary, "StopProcess primary"), domain.ErrProcessGroupNotReaped)
	assert.ErrorIs(t, recvErr(t, secondary, "StopProcess secondary"), domain.ErrProcessGroupNotReaped,
		"the secondary caller must also return the surviving-group error")

	// Both StopProcess calls have returned, so any process_stopped emit would
	// already be buffered: drain non-blocking and assert none was emitted.
	for {
		select {
		case e := <-events:
			assert.NotEqualf(t, EventTypeProcessStopped, e.Type,
				"no process_stopped event may be emitted for a surviving group (got %v)", e.Type)
		default:
			return
		}
	}
}
