package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPlainSupervisor builds a supervisor over a single "web" process backed by
// runner, with no config reload wired (ConfigPath unset). It does not start the
// supervisor.
func newPlainSupervisor(t *testing.T, runner ProcessRunner) *Supervisor {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })
	cfg := &config.Config{
		API: config.APIConfig{Port: 5561, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{
			"web": {Cmd: "irrelevant", StopTimeout: "5s"},
		},
	}
	return New(cfg, logMgr, runner, DefaultSupervisorConfig())
}

// TestLaunchGate_NilGateAllowsStart: a directly-constructed ManagedProcess has no
// launchGate installed, so Start must proceed exactly as before (#32/#36, D2
// back-compat).
func TestLaunchGate_NilGateAllowsStart(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	mp := newFakeManagedProcess(t, runner)

	require.Nil(t, mp.launchGate, "direct construction must install no gate")
	require.NoError(t, mp.Start(context.Background()), "a nil gate must allow the start (back-compat)")
	assert.Equal(t, domain.ProcessStateRunning, mp.State())
	assert.Equal(t, 1, runner.count())
}

// TestLaunchGate_StartRefusedWhenGateClosed: with the gate closed, startWithConfig
// (via Start, the nil-pending path) refuses with ErrShutdownInProgress, applies no
// state change, and never reaches the runner. Reopening the gate lets the same
// process start (#32/#36, D2).
func TestLaunchGate_StartRefusedWhenGateClosed(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	mp := newFakeManagedProcess(t, runner)

	var launchable atomic.Bool // zero value: gate closed
	mp.launchGate = func() error {
		if !launchable.Load() {
			return domain.ErrShutdownInProgress
		}
		return nil
	}

	err := mp.Start(context.Background())
	assert.ErrorIs(t, err, domain.ErrShutdownInProgress, "a closed gate must refuse the start")
	assert.Equal(t, domain.ProcessStateStopped, mp.State(), "state must be unchanged on refusal")
	assert.Equal(t, 0, runner.count(), "the runner must never launch on refusal")

	launchable.Store(true)
	require.NoError(t, mp.Start(context.Background()), "an open gate must allow the start")
	assert.Equal(t, domain.ProcessStateRunning, mp.State())
	assert.Equal(t, 1, runner.count())
}

// TestLaunchGate_StartAndRestartRefusedAfterStop: once a full Supervisor.Stop has
// completed, both StartProcess and RestartProcess refuse with
// ErrShutdownInProgress (the observable contract; here the #41 supervisor-state
// pre-check fires first -- the gate-path-specific refusal is proven separately by
// TestLaunchGate_RestartRefusedAfterStopBeganNoSwap).
func TestLaunchGate_StartAndRestartRefusedAfterStop(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	sup := newPlainSupervisor(t, runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.NoError(t, sup.Stop(context.Background()))

	assert.ErrorIs(t, sup.StartProcess(context.Background(), "web"), domain.ErrShutdownInProgress,
		"StartProcess must refuse after shutdown")
	assert.ErrorIs(t, sup.RestartProcess(context.Background(), "web"), domain.ErrShutdownInProgress,
		"RestartProcess must refuse after shutdown")
}

// TestLaunchGate_RestartRefusedAfterStopBeganNoSwap drives the gate path
// specifically (past the #41 pre-check): a restart is parked in the unlocked gap
// between its stop half and its start half while a full Supervisor.Stop flips the
// gate closed. When released, the start half hits the closed gate and is refused
// BEFORE the pending-config swap -- so no swap is applied and no replacement
// launches (#32/#36, D2).
func TestLaunchGate_RestartRefusedAfterStopBeganNoSwap(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.Equal(t, "echo v1", runner.lastConfig().Cmd)

	mp := getManagedProcess(t, sup, "web")

	// Park the restart between its stop and start halves so we can flip the gate
	// before the start half runs.
	reached := make(chan struct{})
	release := make(chan struct{})
	mp.restartStartBarrier = func() {
		close(reached)
		<-release
	}

	// Edit the file so a NON-refused restart would swap in v2; the refusal must
	// leave this un-applied.
	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v2\"\n")

	restartErr := make(chan error, 1)
	go func() { restartErr <- sup.RestartProcess(context.Background(), "web") }()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not reach the start-half barrier within timeout")
	}

	// The process is already stopped (stop half done), so Supervisor.Stop's own
	// per-process stop returns promptly; Stop flips the gate closed and completes.
	require.NoError(t, sup.Stop(context.Background()))

	close(release)

	err = recvErr(t, restartErr, "RestartProcess")
	assert.ErrorIs(t, err, domain.ErrShutdownInProgress, "the gate must refuse the start half after Stop began")

	assert.Equal(t, "echo v1", mp.Config().Cmd, "a refused launch must NOT apply the pending config swap")
	assert.Equal(t, 1, runner.count(), "no replacement may launch")
	assert.Equal(t, "echo v1", runner.lastConfig().Cmd, "the runner must never see the v2 config")
}

// TestLaunchGate_CheckUseWindowReplacementReaped exercises the deliberately-weak
// D2 invariant: a launcher that read the gate open just before Supervisor.Stop
// closed it still launches (the check/use window is real), but that replacement is
// reaped before Stop returns. The runner's onStart hook parks the restart's start
// half INSIDE runner.Start (past the gate check, p.mu held) while a real
// Supervisor.Stop flips the gate and queues its own reaping stop behind this
// launch (#32/#36, D2).
func TestLaunchGate_CheckUseWindowReplacementReaped(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	sup := newPlainSupervisor(t, runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp := getManagedProcess(t, sup, "web")

	stopErr := make(chan error, 1)
	runner.onStart = func(call int) {
		// call 1 is the restart's start half (the replacement). onStart runs inside
		// runner.Start, so the gate has already authorized this launch and p.mu is
		// held here.
		if call != 1 {
			return
		}
		// Kick off a full shutdown. It flips the gate closed under s.mu, then blocks
		// trying to stop "web" on p.mu (held by this very launch).
		go func() { stopErr <- sup.Stop(context.Background()) }()
		// Wait until Stop has closed the gate, proving the flip lands while this
		// launcher is already past the gate check (the check/use window).
		deadline := time.Now().Add(5 * time.Second)
		for sup.launchable.Load() {
			if time.Now().After(deadline) {
				t.Error("Supervisor.Stop did not close the launch gate within timeout")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}

	// The restart's start half launches the replacement in the window.
	require.NoError(t, sup.RestartProcess(context.Background(), "web"),
		"the replacement launches -- the gate was open when checked")
	require.Equal(t, 2, runner.count(), "the replacement must have launched despite the concurrent flip")

	// Supervisor.Stop's stop goroutine, queued on p.mu behind the launch, reaps the
	// replacement before Stop returns: clean outcome, no live process.
	require.NoError(t, recvErr(t, stopErr, "Supervisor.Stop"))
	assert.Equal(t, domain.ProcessStateStopped, mp.State(), "the replacement must be reaped before Stop returns")

	replacement := runner.last()
	assert.True(t, replacement.sawSignal(sigterm), "the reaping stop must have signalled the replacement")
}

// TestLaunchGate_LaunchableResetOnStopStartCycle: the gate is open after Start,
// closed after Stop, and reopened by a fresh Start so the process starts normally
// (supervisor stop->start cycles happen in tests) (#32/#36, D2).
func TestLaunchGate_LaunchableResetOnStopStartCycle(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	sup := newPlainSupervisor(t, runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.True(t, sup.launchable.Load(), "gate must be open after Start")

	require.NoError(t, sup.Stop(context.Background()))
	require.False(t, sup.launchable.Load(), "gate must be closed after Stop")

	res, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.True(t, sup.launchable.Load(), "gate must reopen on a re-start")
	assert.Contains(t, res.Started, "web", "the process must start normally after the cycle")
	assert.Equal(t, domain.ProcessStateRunning, getManagedProcess(t, sup, "web").State())
}
