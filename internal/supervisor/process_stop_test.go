package supervisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeManagedProcess builds a ManagedProcess wired to the given runner with
// a throwaway log manager. loadEnv is left nil so Start uses the stored env.
func newFakeManagedProcess(t *testing.T, runner ProcessRunner) *ManagedProcess {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })
	return NewManagedProcess(domain.ProcessConfig{
		Name: "fake",
		Cmd:  "irrelevant",
	}, nil, runner, logMgr)
}

// waitForState polls mp until it reaches want or timeout elapses.
func waitForState(t *testing.T, mp *ManagedProcess, want domain.ProcessState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mp.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process did not reach state %q within %v (last: %q)", want, timeout, mp.State())
}

// firstIndexOf returns the index of the first recorded signal equal to sig, or
// -1 if never seen.
func firstIndexOf(sigs []sigRecord, sig os.Signal) int {
	for i, r := range sigs {
		if r.sig == sig {
			return i
		}
	}
	return -1
}

// TestManagedProcess_Stop_GracefulGroupDeath (A7-ish): a group that dies
// promptly on SIGTERM -- before the graceful deadline -- is reaped without ever
// receiving SIGKILL; Stop returns nil and the process ends stopped.
func TestManagedProcess_Stop_GracefulGroupDeath(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	mp := newFakeManagedProcess(t, runner)

	require.NoError(t, mp.Start(context.Background()))
	require.Equal(t, domain.ProcessStateRunning, mp.State())
	fp := runner.last()

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mp.Stop(stopCtx))

	assert.Equal(t, domain.ProcessStateStopped, mp.State())
	assert.True(t, fp.sawSignal(sigterm), "SIGTERM should have been sent")
	assert.False(t, fp.sawSignal(sigkill), "SIGKILL must NOT be sent when the group dies gracefully")
}

// TestManagedProcess_Stop_StubbornGroupEscalatesToSIGKILL: a group that ignores
// SIGTERM but dies on SIGKILL is escalated and reaped; Stop returns nil, SIGKILL
// was sent after SIGTERM, and the process ends stopped. A short ctx keeps it
// fast (with KillGrace=2s the tiny budget drives the graceful deadline to now,
// so escalation is immediate).
func TestManagedProcess_Stop_StubbornGroupEscalatesToSIGKILL(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newStubbornFake(2000 + call) })
	mp := newFakeManagedProcess(t, runner)

	require.NoError(t, mp.Start(context.Background()))
	fp := runner.last()

	stopCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, mp.Stop(stopCtx))

	assert.Equal(t, domain.ProcessStateStopped, mp.State())

	sigs := fp.signalsReceived()
	termIdx := firstIndexOf(sigs, sigterm)
	killIdx := firstIndexOf(sigs, sigkill)
	require.GreaterOrEqual(t, termIdx, 0, "SIGTERM should have been sent")
	require.GreaterOrEqual(t, killIdx, 0, "SIGKILL should have been sent after graceful timeout")
	assert.Less(t, termIdx, killIdx, "SIGTERM must precede SIGKILL")
}

// TestManagedProcess_Stop_UnreapableGroupReturnsError (A6): a group whose
// liveness probe is stuck true (a surviving grandchild) cannot be reaped, so
// Stop returns ErrProcessGroupNotReaped, the process ends crashed, current is
// retained, and a second Stop re-attempts the reap rather than short-circuiting
// to ErrProcessNotRunning.
func TestManagedProcess_Stop_UnreapableGroupReturnsError(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newUnreapableFake(3000 + call) })
	mp := newFakeManagedProcess(t, runner)

	require.NoError(t, mp.Start(context.Background()))
	fp := runner.last()

	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := mp.Stop(stopCtx)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrProcessGroupNotReaped)
	assert.Contains(t, err.Error(), "fake", "error should name the process")
	assert.Equal(t, domain.ProcessStateCrashed, mp.State())

	// current must be retained so a later stop/restart can retry the reap.
	mp.mu.RLock()
	require.NotNil(t, mp.current, "current instance must be retained after a failed reap")
	mp.mu.RUnlock()

	termsAfterFirst := len(signalsOfType(fp, sigterm))

	// A second Stop must NOT short-circuit to ErrProcessNotRunning: the group is
	// still (fake-)alive, so it re-runs the reap and reports the same error.
	stopCtx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	err2 := mp.Stop(stopCtx2)
	require.Error(t, err2)
	assert.ErrorIs(t, err2, domain.ErrProcessGroupNotReaped)
	assert.NotErrorIs(t, err2, domain.ErrProcessNotRunning)
	assert.Greater(t, len(signalsOfType(fp, sigterm)), termsAfterFirst,
		"second Stop should re-attempt the reap (send SIGTERM again), not short-circuit")
}

// signalsOfType returns all recorded signals of the given type.
func signalsOfType(fp *fakeProcess, sig os.Signal) []sigRecord {
	var out []sigRecord
	for _, r := range fp.signalsReceived() {
		if r.sig == sig {
			out = append(out, r)
		}
	}
	return out
}

// TestManagedProcess_RestartClobberSafety (A9): a tight start->restart loop
// ends running with a non-nil PID and an advanced restart count, and no stale
// monitor from a prior run flips the live run to crashed. Run under -race, this
// is the key concurrency test.
func TestManagedProcess_RestartClobberSafety(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(4000 + call) })
	mp := newFakeManagedProcess(t, runner)

	require.NoError(t, mp.Start(context.Background()))

	const restarts = 10
	for i := 0; i < restarts; i++ {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := mp.Restart(stopCtx, context.Background(), nil)
		cancel()
		require.NoErrorf(t, err, "restart %d", i)
		require.Equalf(t, domain.ProcessStateRunning, mp.State(), "restart %d should leave the process running", i)
	}

	// Give any straggler monitor goroutines a chance to run their critical
	// sections; the generation guard must prevent them from mutating state.
	time.Sleep(100 * time.Millisecond)

	info := mp.Info()
	assert.Equal(t, domain.ProcessStateRunning, info.State, "no stale monitor may flip the live run to crashed")
	assert.Greater(t, info.PID, 0, "the live run must report a non-nil PID")
	assert.Equal(t, restarts, info.RestartCount)
	assert.Equal(t, runner.last().PID(), info.PID, "PID must belong to the most-recent run")

	// Clean up the final live run.
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, mp.Stop(stopCtx))
}

// TestManagedProcess_StartDuringStopRejected: while a Stop is mid-graceful
// (state=stopping, group still alive), a concurrent Start is rejected with
// ErrProcessAlreadyRunning rather than launching a duplicate.
func TestManagedProcess_StartDuringStopRejected(t *testing.T) {
	// The graceful death is gated on a channel the test controls so Stop stays
	// in its graceful phase until we release it -- fully deterministic.
	release := make(chan struct{})
	runner := newFakeRunner(func(call int) *fakeProcess {
		fp := newFakeProcess(5000 + call)
		fp.onSignal = func(fp *fakeProcess, sig os.Signal) {
			if sig == sigterm {
				go func() {
					<-release
					fp.setAlive(false)
					fp.exitLeader(nil)
				}()
			}
		}
		return fp
	})
	mp := newFakeManagedProcess(t, runner)

	require.NoError(t, mp.Start(context.Background()))

	stopDone := make(chan error, 1)
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopDone <- mp.Stop(stopCtx)
	}()

	// Wait until Stop has transitioned to stopping (SIGTERM sent, group still
	// alive because we haven't released it).
	waitForState(t, mp, domain.ProcessStateStopping, time.Second)

	// A concurrent Start while stopping must be rejected.
	err := mp.Start(context.Background())
	assert.ErrorIs(t, err, domain.ErrProcessAlreadyRunning)

	// Only one process was ever created (no duplicate launched).
	assert.Equal(t, 1, runner.count(), "no duplicate process should be started during Stop")

	// Release the graceful death and let Stop finish cleanly.
	close(release)
	require.NoError(t, <-stopDone)
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestComputeDeadlines_NoContextDeadlineUsesFallback: with no ctx deadline,
// computeDeadlines falls back to the shutdown timeout; graceful ends KillGrace
// before the fallback deadline and kill lands at it.
func TestComputeDeadlines_NoContextDeadlineUsesFallback(t *testing.T) {
	mp := newFakeManagedProcess(t, newFakeRunner(func(int) *fakeProcess { return newFakeProcess(1) }))

	before := time.Now()
	graceful, kill := mp.computeDeadlines(context.Background())

	assert.WithinDuration(t, before.Add(constants.DefaultShutdownTimeout-constants.KillGrace), graceful, 500*time.Millisecond)
	assert.WithinDuration(t, before.Add(constants.DefaultShutdownTimeout), kill, 500*time.Millisecond)
	assert.Equal(t, constants.KillGrace, kill.Sub(graceful), "kill deadline is always KillGrace after graceful")
}

// TestComputeDeadlines_NearExpiredCtxEscalatesImmediately: an already-expired
// ctx drives the graceful deadline to ~now so Stop escalates immediately, while
// the kill deadline still reserves a full KillGrace (its own timer, not ctx).
func TestComputeDeadlines_NearExpiredCtxEscalatesImmediately(t *testing.T) {
	mp := newFakeManagedProcess(t, newFakeRunner(func(int) *fakeProcess { return newFakeProcess(1) }))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	time.Sleep(40 * time.Millisecond) // let ctx expire

	before := time.Now()
	graceful, kill := mp.computeDeadlines(ctx)

	assert.WithinDuration(t, before, graceful, 50*time.Millisecond, "graceful deadline should be ~now for an expired ctx")
	assert.Equal(t, constants.KillGrace, kill.Sub(graceful), "kill deadline still reserves a full KillGrace")
}

// TestManagedProcess_Stop_NoDeadlineFallbackEscalates: Stop(context.Background())
// (no deadline) uses the shutdown-timeout fallback and still escalates to
// SIGKILL against a stubborn group, succeeding within a bounded time. The
// fallback is shrunk via the shutdownTimeout test seam so the graceful window
// is ~1s instead of the 10s production default.
func TestManagedProcess_Stop_NoDeadlineFallbackEscalates(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newStubbornFake(6000 + call) })
	mp := newFakeManagedProcess(t, runner)
	mp.shutdownTimeout = 3 * time.Second // graceful window == 3s - KillGrace(2s) == 1s

	require.NoError(t, mp.Start(context.Background()))
	fp := runner.last()

	start := time.Now()
	require.NoError(t, mp.Stop(context.Background()))
	elapsed := time.Since(start)

	assert.Equal(t, domain.ProcessStateStopped, mp.State())
	assert.True(t, fp.sawSignal(sigkill), "stubborn group should be SIGKILLed after the graceful window")
	assert.Less(t, elapsed, 3*time.Second, "escalation must use the ~1s fallback window, not the 10s default")
}

// TestManagedProcess_Stop_EscalationUpperBound is the plan §7 escalation-timing
// bound: a SIGTERM-ignoring process with a 3s stop budget must be SIGKILLed no
// earlier than ~1s (the graceful window = budget - KillGrace(2s)) and no later
// than ~3s+ε (the full budget) after the stop begins. It asserts the actual
// SIGKILL timestamp, not just that a SIGKILL happened.
func TestManagedProcess_Stop_EscalationUpperBound(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newStubbornFake(6500 + call) })
	mp := newFakeManagedProcess(t, runner)
	mp.shutdownTimeout = 3 * time.Second // graceful window == 3s - KillGrace(2s) == 1s

	require.NoError(t, mp.Start(context.Background()))
	fp := runner.last()

	start := time.Now()
	require.NoError(t, mp.Stop(context.Background()))

	sigs := fp.signalsReceived()
	killIdx := firstIndexOf(sigs, sigkill)
	require.GreaterOrEqual(t, killIdx, 0, "stubborn group must be SIGKILLed")
	killAt := sigs[killIdx].at.Sub(start)

	assert.GreaterOrEqual(t, killAt, 900*time.Millisecond, "SIGKILL must not fire before the ~1s graceful window elapses")
	assert.LessOrEqual(t, killAt, 3*time.Second+500*time.Millisecond, "SIGKILL must fire within the ~3s budget (+ε)")
}
