package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startFakeProcessInSupervisor builds a ManagedProcess backed by a fresh fake
// runner, sets its effective stop budget to the given value (via the promoted
// shutdownTimeout field), starts it, and registers it on the supervisor. It
// returns the process and its runner so a test can inspect the fake's recorded
// signals afterwards.
func startFakeProcessInSupervisor(t *testing.T, sup *Supervisor, name string, budget time.Duration, factory func(call int) *fakeProcess) (*ManagedProcess, *fakeRunner) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })

	runner := newFakeRunner(factory)
	mp := NewManagedProcess(domain.ProcessConfig{Name: name, Cmd: "irrelevant"}, nil, runner, logMgr)
	mp.shutdownTimeout = budget
	require.NoError(t, mp.Start(context.Background()))

	sup.mu.Lock()
	sup.processes[name] = mp
	sup.mu.Unlock()
	return mp, runner
}

// TestSupervisor_MaxStopBudget covers the two contracts of MaxStopBudget: an
// empty supervisor reports constants.DefaultShutdownTimeout, and a populated one
// reports the maximum effective budget over its live processes (used to size the
// daemon's outer shutdown deadline so hot-reloaded budgets are respected).
func TestSupervisor_MaxStopBudget(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	t.Run("empty supervisor returns the default", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())
		assert.Equal(t, constants.DefaultShutdownTimeout, sup.MaxStopBudget())
	})

	t.Run("mixed budgets return the maximum", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mpA := NewManagedProcess(domain.ProcessConfig{Name: "a"}, nil, nil, logMgr)
		mpA.shutdownTimeout = 5 * time.Second
		mpB := NewManagedProcess(domain.ProcessConfig{Name: "b"}, nil, nil, logMgr)
		mpB.shutdownTimeout = 8 * time.Second

		sup.mu.Lock()
		sup.processes["a"] = mpA
		sup.processes["b"] = mpB
		sup.mu.Unlock()

		assert.Equal(t, 8*time.Second, sup.MaxStopBudget())
	})
}

// TestSupervisor_Stop_MixedBudgetsPerProcess proves the bulk Stop path gives each
// process its own deadline rather than one shared deadline: two stubborn
// processes with different budgets are SIGKILLed at times matching their own
// (budget - KillGrace) graceful windows, not at a single common instant.
func TestSupervisor_Stop_MixedBudgetsPerProcess(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	// Bring the supervisor to the running state (no config processes) so Stop
	// does not short-circuit; then inject the fakes.
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	// fast: graceful window = 2.5s - 2s = 0.5s -> SIGKILL ~0.5s
	// slow: graceful window = 4s   - 2s = 2s   -> SIGKILL ~2s
	_, fastRunner := startFakeProcessInSupervisor(t, sup, "fast", 2500*time.Millisecond,
		func(call int) *fakeProcess { return newStubbornFake(7000 + call) })
	_, slowRunner := startFakeProcessInSupervisor(t, sup, "slow", 4*time.Second,
		func(call int) *fakeProcess { return newStubbornFake(8000 + call) })

	start := time.Now()
	require.NoError(t, sup.Stop(context.Background()))

	fastFP := fastRunner.last()
	slowFP := slowRunner.last()

	fastKill := firstIndexOf(fastFP.signalsReceived(), sigkill)
	slowKill := firstIndexOf(slowFP.signalsReceived(), sigkill)
	require.GreaterOrEqual(t, fastKill, 0, "fast process should have been SIGKILLed")
	require.GreaterOrEqual(t, slowKill, 0, "slow process should have been SIGKILLed")

	fastAt := fastFP.signalsReceived()[fastKill].at.Sub(start)
	slowAt := slowFP.signalsReceived()[slowKill].at.Sub(start)

	// Each escalates on its own budget. If Stop used one shared deadline, both
	// would escalate together; instead fast lands well before slow.
	assert.Less(t, fastAt, 1500*time.Millisecond, "fast (2.5s budget) should escalate ~0.5s after stop begins")
	assert.Greater(t, slowAt, 1500*time.Millisecond, "slow (4s budget) should escalate ~2s after stop begins")
	assert.Less(t, slowAt, 3500*time.Millisecond, "slow should still escalate within its own budget")
	assert.Less(t, fastAt, slowAt, "per-process deadlines: fast must escalate before slow")
}

// TestSupervisor_StopProcess_UsesPerProcessBudget verifies StopProcess bounds the
// stop by the target's own effective budget (mp.StopTimeout), not the global
// default: a 3s-budget stubborn process escalates to SIGKILL ~1s after the stop
// begins (graceful window = 3s - KillGrace(2s) = 1s).
func TestSupervisor_StopProcess_UsesPerProcessBudget(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	_, runner := startFakeProcessInSupervisor(t, sup, "svc", 3*time.Second,
		func(call int) *fakeProcess { return newStubbornFake(9000 + call) })

	start := time.Now()
	require.NoError(t, sup.StopProcess(context.Background(), "svc"))
	elapsed := time.Since(start)

	fp := runner.last()
	killIdx := firstIndexOf(fp.signalsReceived(), sigkill)
	require.GreaterOrEqual(t, killIdx, 0, "stubborn process should be SIGKILLed")
	killAt := fp.signalsReceived()[killIdx].at.Sub(start)

	assert.Greater(t, killAt, 700*time.Millisecond, "SIGKILL must wait out the ~1s graceful window")
	assert.Less(t, elapsed, 3*time.Second, "must use the 3s per-process budget, not the 10s default")
}

// TestSupervisor_RestartProcess_UsesPerProcessBudget verifies the stop half of a
// restart is bounded by the target's own effective budget: a 3s-budget stubborn
// process escalates ~1s into the restart, then a fresh instance is started.
func TestSupervisor_RestartProcess_UsesPerProcessBudget(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp, runner := startFakeProcessInSupervisor(t, sup, "svc", 3*time.Second,
		func(call int) *fakeProcess { return newStubbornFake(9500 + call) })

	// Capture the first instance's fake before the restart replaces current.
	firstFP := runner.last()

	start := time.Now()
	require.NoError(t, sup.RestartProcess(context.Background(), "svc"))
	elapsed := time.Since(start)

	killIdx := firstIndexOf(firstFP.signalsReceived(), sigkill)
	require.GreaterOrEqual(t, killIdx, 0, "stubborn process should be SIGKILLed during the stop half")
	killAt := firstFP.signalsReceived()[killIdx].at.Sub(start)

	assert.Greater(t, killAt, 700*time.Millisecond, "SIGKILL must wait out the ~1s graceful window")
	assert.Less(t, elapsed, 3*time.Second, "stop half must use the 3s per-process budget, not the 10s default")
	assert.Equal(t, domain.ProcessStateRunning, mp.State(), "restart should leave a fresh instance running")
	assert.Equal(t, 2, runner.count(), "restart should have started a second instance")
}
