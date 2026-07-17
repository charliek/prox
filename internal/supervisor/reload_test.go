package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfigFile writes cfgYAML to dir/prox.yaml (0644 so config.Load's
// permission check passes) and returns the path. Used to seed and later edit
// the file a reload test drives.
func writeConfigFile(t *testing.T, dir, cfgYAML string) string {
	t.Helper()
	path := filepath.Join(dir, "prox.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfgYAML), 0644), "writing config")
	return path
}

// newReloadSupervisor writes cfgYAML, loads it, and builds a supervisor wired
// for config reload (ConfigPath set, ConfigDir = dir) backed by runner. It does
// not start the supervisor.
func newReloadSupervisor(t *testing.T, dir, cfgYAML string, runner ProcessRunner) *Supervisor {
	t.Helper()
	path := writeConfigFile(t, dir, cfgYAML)

	cfg, err := config.Load(path)
	require.NoError(t, err, "loading initial config")

	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })

	supCfg := DefaultSupervisorConfig()
	supCfg.ConfigDir = dir
	supCfg.ConfigPath = path
	return New(cfg, logMgr, runner, supCfg)
}

// getManagedProcess returns the live ManagedProcess for name (lock-held read).
func getManagedProcess(t *testing.T, sup *Supervisor, name string) *ManagedProcess {
	t.Helper()
	sup.mu.RLock()
	defer sup.mu.RUnlock()
	mp := sup.processes[name]
	require.NotNil(t, mp, "process %q should exist", name)
	return mp
}

// TestReload_RestartAndStartStartApplyChangedCmd covers the core #33 promise: a
// cmd edited in prox.yaml takes effect on `restart`, and equally on `stop` +
// `start` (the reload rule is unified across both API paths -- plan D3). The
// fake runner records the cmd it was launched with.
func TestReload_RestartAndStartStartApplyChangedCmd(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.Equal(t, "echo v1", runner.lastConfig().Cmd)

	// restart picks up the edit.
	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v2\"\n")
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))
	assert.Equal(t, "echo v2", runner.lastConfig().Cmd, "restart must launch the edited cmd")
	assert.Equal(t, "echo v2", getManagedProcess(t, sup, "web").Config().Cmd, "stored config must match the launched cmd")

	// stop + start also picks up the (next) edit.
	require.NoError(t, sup.StopProcess(context.Background(), "web"))
	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v3\"\n")
	require.NoError(t, sup.StartProcess(context.Background(), "web"))
	assert.Equal(t, "echo v3", runner.lastConfig().Cmd, "start after stop must launch the edited cmd")
	assert.Equal(t, "echo v3", getManagedProcess(t, sup, "web").Config().Cmd)
}

// TestReload_ConsecutiveRestartsPickUpLatestFile: two edit->restart cycles each
// pick up the latest file, proving reload is per-request (not a one-shot).
func TestReload_ConsecutiveRestartsPickUpLatestFile(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1100 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	for _, cmd := range []string{"echo v2", "echo v3"} {
		writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \""+cmd+"\"\n")
		require.NoError(t, sup.RestartProcess(context.Background(), "web"))
		assert.Equal(t, cmd, runner.lastConfig().Cmd)
	}
}

// TestReload_RestartAppliesChangedHealthcheck: a changed healthcheck in the file
// is applied on restart (the runner sees the new healthcheck on the launched
// config).
func TestReload_RestartAppliesChangedHealthcheck(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1200 + call) })
	sup := newReloadSupervisor(t, dir,
		"processes:\n  web:\n    cmd: \"echo hi\"\n    healthcheck:\n      cmd: \"true\"\n      interval: 30s\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.NotNil(t, runner.lastConfig().Healthcheck)
	require.Equal(t, "true", runner.lastConfig().Healthcheck.Cmd)

	writeConfigFile(t, dir,
		"processes:\n  web:\n    cmd: \"echo hi\"\n    healthcheck:\n      cmd: \"echo healthy\"\n      interval: 45s\n")
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))

	hc := runner.lastConfig().Healthcheck
	require.NotNil(t, hc, "restart must carry the reloaded healthcheck")
	assert.Equal(t, "echo healthy", hc.Cmd)
	assert.Equal(t, 45*time.Second, hc.Interval)

	require.NoError(t, sup.Stop(context.Background()))
}

// TestReload_RestartAppliesChangedStopTimeout: a changed stop_timeout in the file
// updates the effective stop budget after restart (mp.StopTimeout()).
func TestReload_RestartAppliesChangedStopTimeout(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1300 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo hi\"\n    stop_timeout: 5s\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	mp := getManagedProcess(t, sup, "web")
	require.Equal(t, 5*time.Second, mp.StopTimeout())

	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo hi\"\n    stop_timeout: 8s\n")
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))
	assert.Equal(t, 8*time.Second, mp.StopTimeout(), "restart must apply the reloaded stop_timeout")
}

// TestReload_RestartRebuildsEnvClosureForChangedPath: pointing env_file at a
// different path in the reloaded file rebuilds the loadEnv closure, so the
// replacement observes the NEW file's values (the runner records the launched
// env).
func TestReload_RestartRebuildsEnvClosureForChangedPath(t *testing.T) {
	dir := t.TempDir()
	envA := filepath.Join(dir, "a.env")
	envB := filepath.Join(dir, "b.env")
	require.NoError(t, os.WriteFile(envA, []byte("MYVAL=fromA\n"), 0644))
	require.NoError(t, os.WriteFile(envB, []byte("MYVAL=fromB\n"), 0644))

	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1400 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo hi\"\n    env_file: a.env\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.Equal(t, "fromA", runner.lastEnv()["MYVAL"])

	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo hi\"\n    env_file: b.env\n")
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))
	assert.Equal(t, "fromB", runner.lastEnv()["MYVAL"], "restart must reload env from the new env_file path")
}

// TestReload_FailuresLeaveProcessUntouched drives every fail-closed path: broken
// YAML, a validation failure in an UNRELATED process, a missing file, and the
// target removed from the file. Each must return the documented sentinel and
// leave the original process running with its original cmd (no re-launch).
func TestReload_FailuresLeaveProcessUntouched(t *testing.T) {
	const goodCmd = "echo v1"
	base := "processes:\n  web:\n    cmd: \"" + goodCmd + "\"\n  other:\n    cmd: \"echo other\"\n"

	cases := []struct {
		name     string
		mutate   func(t *testing.T, dir, path string)
		wantErr  error
		wantCode string
	}{
		{
			name:     "broken yaml",
			mutate:   func(t *testing.T, dir, path string) { writeConfigFile(t, dir, "processes: {: :\n") },
			wantErr:  domain.ErrConfigReloadFailed,
			wantCode: domain.ErrCodeConfigReloadFailed,
		},
		{
			name: "unrelated process validation failure",
			// `other`'s stop_timeout is below KillGrace -> whole-file validation
			// fails even though `web` (the restart target) is untouched.
			mutate: func(t *testing.T, dir, path string) {
				writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \""+goodCmd+"\"\n  other:\n    cmd: \"echo other\"\n    stop_timeout: 1s\n")
			},
			wantErr:  domain.ErrConfigReloadFailed,
			wantCode: domain.ErrCodeConfigReloadFailed,
		},
		{
			name:     "missing file",
			mutate:   func(t *testing.T, dir, path string) { require.NoError(t, os.Remove(path)) },
			wantErr:  domain.ErrConfigReloadFailed,
			wantCode: domain.ErrCodeConfigReloadFailed,
		},
		{
			name: "target removed from config",
			mutate: func(t *testing.T, dir, path string) {
				writeConfigFile(t, dir, "processes:\n  other:\n    cmd: \"echo other\"\n")
			},
			wantErr:  domain.ErrProcessNotInConfig,
			wantCode: domain.ErrCodeProcessNotInConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1500 + call) })
			sup := newReloadSupervisor(t, dir, base, runner)

			_, err := sup.Start(context.Background())
			require.NoError(t, err)
			launchesBefore := runner.count()

			path := filepath.Join(dir, "prox.yaml")
			tc.mutate(t, dir, path)

			err = sup.RestartProcess(context.Background(), "web")
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.wantCode, domain.ErrorCode(err), "error must map to the documented code")

			// The target stays running with its original cmd; nothing re-launched.
			mp := getManagedProcess(t, sup, "web")
			assert.Equal(t, domain.ProcessStateRunning, mp.State(), "process must stay running after a failed reload")
			assert.Equal(t, goodCmd, mp.Config().Cmd, "stored config must be unchanged")
			assert.Equal(t, launchesBefore, runner.count(), "a failed reload must not stop or re-launch the process")
		})
	}
}

// TestReload_PreflightEnvFailureRestartsNothing: a reloaded config that points at
// a nonexistent env_file must fail the restart BEFORE the stop -- the process is
// never stopped (env is preflighted in prepareReload). Uses a stubborn fake so a
// stop, had one happened, would be observable via a SIGTERM.
func TestReload_PreflightEnvFailureRestartsNothing(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newStubbornFake(1600 + call) })
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	fp := runner.last()

	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n    env_file: does-not-exist.env\n")
	err = sup.RestartProcess(context.Background(), "web")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrConfigReloadFailed)

	mp := getManagedProcess(t, sup, "web")
	assert.Equal(t, domain.ProcessStateRunning, mp.State())
	assert.Equal(t, 1, runner.count(), "the process must not be re-launched")
	assert.False(t, fp.sawSignal(sigterm), "the process must not even be signalled (preflight fails before the stop)")
}

// TestReload_RestartStopHalfUsesOldBudget_NextStopUsesNew is the plan §7 timing
// contract: raising stop_timeout via a restart does NOT lengthen that restart's
// own stop half (it uses the pre-swap budget); the new budget governs the next
// stop.
//
// The old (3s) and new (10s) budgets are set far apart so the stop-half SIGKILL
// timestamp discriminates with a wide, non-flaky margin: it must land within the
// OLD ~1s graceful window (asserted as a loose ceiling well under the new
// budget's ~8s window, so slow/-race CI cannot flake it), and the lower bound is
// the real discriminator that it waited a graceful window at all. The new budget
// governing the next stop is proven directly via StopTimeout() (StopProcess
// bounds its stop by exactly that value -- see
// TestSupervisor_StopProcess_UsesPerProcessBudget); the replacement is a graceful
// fake so this assertion needs no second multi-second escalation.
func TestReload_RestartStopHalfUsesOldBudget_NextStopUsesNew(t *testing.T) {
	dir := t.TempDir()
	// Instance 0 is stubborn (forces the stop-half SIGKILL we time); the
	// replacement is graceful so the follow-up stop is instant.
	runner := newFakeRunner(func(call int) *fakeProcess {
		if call == 0 {
			return newStubbornFake(1700)
		}
		return newGracefulFake(1700 + call)
	})
	// old budget 3s -> graceful window == 3s - KillGrace(2s) == 1s.
	sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n    stop_timeout: 3s\n", runner)

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	oldFP := runner.last() // stubborn instance the restart's stop half will kill

	// Raise the budget to 10s (graceful window would be ~8s if it applied here).
	writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v1\"\n    stop_timeout: 10s\n")

	start := time.Now()
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))

	killIdx := firstIndexOf(oldFP.signalsReceived(), sigkill)
	require.GreaterOrEqual(t, killIdx, 0, "the restart's stop half should SIGKILL the stubborn old instance")
	killAt := oldFP.signalsReceived()[killIdx].at.Sub(start)
	assert.Greater(t, killAt, 700*time.Millisecond, "SIGKILL must wait out the OLD ~1s graceful window (not fire instantly)")
	assert.Less(t, killAt, 5*time.Second, "stop half must use the OLD 3s budget, not the new 10s one (~8s window)")

	// The new budget is now in force and will bound the next stop.
	mp := getManagedProcess(t, sup, "web")
	require.Equal(t, 10*time.Second, mp.StopTimeout(), "the reloaded budget governs subsequent stops")

	// The replacement stops promptly on its own (graceful) -- exercising the
	// next-stop path without a second multi-second escalation.
	require.NotSame(t, oldFP, runner.last(), "restart should have launched a fresh instance")
	require.NoError(t, sup.StopProcess(context.Background(), "web"))
	assert.Equal(t, domain.ProcessStateStopped, mp.State())
}

// TestReload_SwapIsAtomicUnderConcurrentStart is the plan D3 deterministic
// interleaving test. Two (re)starts race to launch a stopped process: both
// StartProcess and RestartProcess funnel their launch through startWithConfig,
// which swaps the reloaded config in only inside the locked critical section,
// after the guards pass and before the runner launches. The runner's onStart
// barrier makes the ordering deterministic -- the first launcher holds the lock
// through swap+launch (the winner); the second parks on the lock and, once the
// winner finishes, sees the process running and returns ErrProcessAlreadyRunning
// WITHOUT applying its swap. Either way the running process's launched cmd must
// equal the stored config's cmd.
func TestReload_SwapIsAtomicUnderConcurrentStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		winnerCmd string
		loserCmd  string
	}{
		{name: "restart-config wins", winnerCmd: "cmd-A", loserCmd: "cmd-B"},
		{name: "start-config wins", winnerCmd: "cmd-B", loserCmd: "cmd-A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
			t.Cleanup(func() { logMgr.Close() })

			gateReached := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once

			runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1800 + call) })
			runner.onStart = func(call int) {
				// Only the winner reaches here (the loser never launches). Signal
				// that the winner holds the lock, then block until released so the
				// loser is forced to park on the process lock.
				once.Do(func() { close(gateReached) })
				<-release
			}

			mp := NewManagedProcess(domain.ProcessConfig{Name: "web", Cmd: "orig"}, nil, runner, logMgr)
			t.Cleanup(func() { _ = mp.Stop(context.Background()) })

			winner := &pendingConfig{
				config:      domain.ProcessConfig{Name: "web", Cmd: tc.winnerCmd},
				stopTimeout: 4 * time.Second,
			}
			loser := &pendingConfig{
				config:      domain.ProcessConfig{Name: "web", Cmd: tc.loserCmd},
				stopTimeout: 4 * time.Second,
			}

			winErr := make(chan error, 1)
			go func() { winErr <- mp.startWithConfig(context.Background(), winner) }()

			// Wait until the winner holds the lock at the runner barrier, then
			// launch the loser so it must contend for the (held) lock.
			<-gateReached
			loseErr := make(chan error, 1)
			go func() { loseErr <- mp.startWithConfig(context.Background(), loser) }()
			time.Sleep(50 * time.Millisecond) // let the loser reach the lock

			close(release)

			require.NoError(t, <-winErr, "the winner's start should succeed")
			require.ErrorIs(t, <-loseErr, domain.ErrProcessAlreadyRunning, "the loser must be refused")

			// Invariant: running process's launched cmd == stored config's cmd.
			assert.Equal(t, tc.winnerCmd, runner.lastConfig().Cmd, "the launched cmd must be the winner's")
			assert.Equal(t, tc.winnerCmd, mp.Config().Cmd, "the stored config must match the running process")
			assert.Equal(t, 1, runner.count(), "the loser must not launch a second instance")
		})
	}
}

// TestReload_ConcurrentStartVsRestart_ThroughSupervisor drives the real
// interleaving the unit-level swap test cannot: a concurrent
// Supervisor.StartProcess racing a Supervisor.RestartProcess through the PUBLIC
// methods, forcing both winners deterministically with fake-runner /
// stop barriers (no sleeps). In every outcome the running process's launched cmd
// must equal the stored Config().Cmd, and the loser must return
// ErrProcessAlreadyRunning without having applied its own reloaded config.
func TestReload_ConcurrentStartVsRestart_ThroughSupervisor(t *testing.T) {
	// (a) StartProcess wins the unlocked gap between the restart's stop and its
	// start half. The restart is held at the post-stop barrier (process already
	// Stopped) while a StartProcess launches; the restart's start half then finds
	// the process running and is refused, without swapping in its own config.
	t.Run("start wins the post-stop gap", func(t *testing.T) {
		dir := t.TempDir()
		// Graceful instances: instance 0 stops cleanly during the restart's stop
		// half; the StartProcess launches instance 1.
		runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(2000 + call) })
		sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v0\"\n", runner)

		_, err := sup.Start(context.Background())
		require.NoError(t, err)
		mp := getManagedProcess(t, sup, "web")

		gapReached := make(chan struct{})
		releaseRestart := make(chan struct{})
		mp.restartStartBarrier = func() {
			close(gapReached)
			<-releaseRestart
		}
		t.Cleanup(func() { _ = sup.Stop(context.Background()) })

		// The restart reloads "v-restart"; the start (below) reloads "v-start".
		writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v-restart\"\n")
		restartErr := make(chan error, 1)
		go func() { restartErr <- sup.RestartProcess(context.Background(), "web") }()

		// Wait until the restart has stopped instance 0 and parked before its
		// start half; the process is now Stopped and the lock is free.
		<-gapReached

		// A StartProcess reloads its own config and wins the gap.
		writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v-start\"\n")
		startErr := make(chan error, 1)
		go func() { startErr <- sup.StartProcess(context.Background(), "web") }()
		require.NoError(t, <-startErr, "the StartProcess should win the gap")

		// Release the restart; its start half must now be refused.
		close(releaseRestart)
		require.ErrorIs(t, <-restartErr, domain.ErrProcessAlreadyRunning,
			"the restart's start half must be refused once StartProcess won")

		assert.Equal(t, "echo v-start", runner.lastConfig().Cmd, "the running process must be the StartProcess's config")
		assert.Equal(t, "echo v-start", mp.Config().Cmd, "stored config must match the running process (restart did not swap)")
	})

	// (b) The restart wins. A concurrent StartProcess arrives while the restart's
	// stop half is still in flight (process Stopping) and is refused immediately,
	// without swapping; the restart then completes and launches its own config.
	t.Run("restart wins over a start during the stop", func(t *testing.T) {
		dir := t.TempDir()

		termReached := make(chan struct{})
		releaseTerm := make(chan struct{})
		// Instance 0 blocks inside its SIGTERM handler, holding the restart in the
		// Stopping state; the replacement (instance 1) is graceful.
		runner := newFakeRunner(func(call int) *fakeProcess {
			if call == 0 {
				fp := newFakeProcess(2050)
				fp.onSignal = func(fp *fakeProcess, sig os.Signal) {
					if sig == sigterm {
						close(termReached)
						<-releaseTerm
						fp.setAlive(false)
						fp.exitLeader(nil)
					}
				}
				return fp
			}
			return newGracefulFake(2050 + call)
		})
		sup := newReloadSupervisor(t, dir, "processes:\n  web:\n    cmd: \"echo v0\"\n", runner)

		_, err := sup.Start(context.Background())
		require.NoError(t, err)
		mp := getManagedProcess(t, sup, "web")
		t.Cleanup(func() { _ = sup.Stop(context.Background()) })

		writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v-restart\"\n")
		restartErr := make(chan error, 1)
		go func() { restartErr <- sup.RestartProcess(context.Background(), "web") }()

		// Wait until the restart's stop half has sent SIGTERM and is blocked in
		// the handler -- the process is Stopping and the lock is free.
		<-termReached

		// A StartProcess arriving now must be refused (Stopping), without swap.
		writeConfigFile(t, dir, "processes:\n  web:\n    cmd: \"echo v-start\"\n")
		require.ErrorIs(t, sup.StartProcess(context.Background(), "web"), domain.ErrProcessAlreadyRunning,
			"a start during the restart's stop must be refused")

		// Let the stop finish; the restart launches its own reloaded config.
		close(releaseTerm)
		require.NoError(t, <-restartErr, "the restart should complete")

		assert.Equal(t, "echo v-restart", runner.lastConfig().Cmd, "the running process must be the restart's config")
		assert.Equal(t, "echo v-restart", mp.Config().Cmd, "stored config must match (the losing start did not swap)")
	})
}

// TestReload_DisabledWhenConfigPathUnset: with no ConfigPath (a Supervisor built
// directly, as many tests do), restart/start do NOT re-read any file and reuse
// the stored config -- back-compat is preserved.
func TestReload_DisabledWhenConfigPathUnset(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{"web": "echo v1"})
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1900 + call) })
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig()) // no ConfigPath

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	pending, err := sup.prepareReload("web")
	require.NoError(t, err)
	assert.Nil(t, pending, "prepareReload must be a no-op when ConfigPath is unset")

	require.NoError(t, sup.RestartProcess(context.Background(), "web"))
	assert.Equal(t, "echo v1", runner.lastConfig().Cmd, "restart must reuse the stored config when reload is disabled")
}
