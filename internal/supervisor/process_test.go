package supervisor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedProcess_StartStop(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "sleep 30",
	}, nil, runner, logMgr)

	t.Run("initial state is stopped", func(t *testing.T) {
		assert.Equal(t, domain.ProcessStateStopped, mp.State())
	})

	t.Run("start changes state to running", func(t *testing.T) {
		ctx := context.Background()
		err := mp.Start(ctx)
		require.NoError(t, err)

		assert.Equal(t, domain.ProcessStateRunning, mp.State())
		assert.Greater(t, mp.Info().PID, 0)
	})

	t.Run("cannot start while running", func(t *testing.T) {
		ctx := context.Background()
		err := mp.Start(ctx)
		assert.ErrorIs(t, err, domain.ErrProcessAlreadyRunning)
	})

	t.Run("stop changes state to stopped", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := mp.Stop(ctx)
		require.NoError(t, err)

		assert.Equal(t, domain.ProcessStateStopped, mp.State())
	})

	t.Run("cannot stop while stopped", func(t *testing.T) {
		ctx := context.Background()
		err := mp.Stop(ctx)
		assert.ErrorIs(t, err, domain.ErrProcessNotRunning)
	})
}

func TestManagedProcess_OutputCapture(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "echo stdout_message; echo stderr_message >&2",
	}, nil, runner, logMgr)

	ctx := context.Background()
	err := mp.Start(ctx)
	require.NoError(t, err)

	// Wait for process to finish
	time.Sleep(500 * time.Millisecond)

	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"test"}}, 0)

	var hasStdout, hasStderr bool
	for _, e := range entries {
		if e.Stream == domain.StreamStdout && e.Line == "stdout_message" {
			hasStdout = true
		}
		if e.Stream == domain.StreamStderr && e.Line == "stderr_message" {
			hasStderr = true
		}
	}

	assert.True(t, hasStdout, "stdout should be captured")
	assert.True(t, hasStderr, "stderr should be captured")
}

func TestManagedProcess_Restart(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "sleep 30",
	}, nil, runner, logMgr)

	ctx := context.Background()
	err := mp.Start(ctx)
	require.NoError(t, err)

	firstPID := mp.Info().PID
	assert.Equal(t, 0, mp.Info().RestartCount)

	// Restart
	restartCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = mp.Restart(restartCtx, context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, domain.ProcessStateRunning, mp.State())
	assert.NotEqual(t, firstPID, mp.Info().PID)
	assert.Equal(t, 1, mp.Info().RestartCount)

	// Cleanup
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	mp.Stop(stopCtx)
}

func TestManagedProcess_Info(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "echo hello",
	}, map[string]string{"FOO": "bar"}, runner, logMgr)

	info := mp.Info()
	assert.Equal(t, "test", info.Name)
	assert.Equal(t, "echo hello", info.Cmd)
	assert.Equal(t, "bar", info.Env["FOO"])
	assert.Equal(t, domain.ProcessStateStopped, info.State)
	// No healthcheck configured: "none", not "unknown" (#100). "unknown" reads as
	// a check that ran and could not reach a verdict; nothing was ever run here.
	assert.Equal(t, domain.HealthStatusNone, info.Health)
	assert.Nil(t, info.HealthDetails, "a process with no healthcheck must carry no health detail block")
}

// TestManagedProcess_Info_HealthSituations pins the three-way distinction from
// #100: Info() derives Health from the CONFIG, not from whether a checker
// object currently exists. Keying on p.healthChecker cannot tell "never
// configured" apart from "configured but stopped/crashed/not yet launched",
// which is exactly the conflation that made an unconfigured process report
// "unknown".
func TestManagedProcess_Info_HealthSituations(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	t.Run("no healthcheck configured reports none", func(t *testing.T) {
		mp := NewManagedProcess(domain.ProcessConfig{
			Name: "web",
			Cmd:  "sleep 30",
		}, nil, NewExecRunner(), logMgr)

		info := mp.Info()
		assert.Equal(t, domain.HealthStatusNone, info.Health)
		assert.Nil(t, info.HealthDetails)
	})

	// A healthcheck block whose Cmd is empty is the same as no healthcheck at
	// all: launch skips checker creation on the same condition, so nothing would
	// ever run.
	t.Run("healthcheck with empty cmd reports none", func(t *testing.T) {
		mp := NewManagedProcess(domain.ProcessConfig{
			Name:        "web",
			Cmd:         "sleep 30",
			Healthcheck: &domain.HealthConfig{},
		}, nil, NewExecRunner(), logMgr)

		assert.Equal(t, domain.HealthStatusNone, mp.Info().Health)
	})

	t.Run("configured but not yet launched reports unknown", func(t *testing.T) {
		mp := NewManagedProcess(domain.ProcessConfig{
			Name:        "web",
			Cmd:         "sleep 30",
			Healthcheck: &domain.HealthConfig{Cmd: "true"},
		}, nil, NewExecRunner(), logMgr)

		info := mp.Info()
		require.Equal(t, domain.ProcessStateStopped, info.State)
		assert.Equal(t, domain.HealthStatusUnknown, info.Health)
		// The detail block is present and says the loop is dormant, so a client can
		// tell "configured, waiting to run" from "configured and actively checking".
		require.NotNil(t, info.HealthDetails)
		assert.False(t, info.HealthDetails.Enabled)
		assert.Equal(t, domain.HealthStatusUnknown, info.HealthDetails.Status)
	})

	t.Run("configured but crashed reports unknown", func(t *testing.T) {
		mp := NewManagedProcess(domain.ProcessConfig{
			Name:        "web",
			Cmd:         "sleep 30",
			Healthcheck: &domain.HealthConfig{Cmd: "true"},
		}, nil, NewExecRunner(), logMgr)

		// A crash leaves the process terminal; a later Stop discards the checker.
		// Either way the configured check has reported nothing, so: unknown.
		mp.mu.Lock()
		mp.state = domain.ProcessStateCrashed
		mp.healthChecker = nil
		mp.mu.Unlock()

		info := mp.Info()
		assert.Equal(t, domain.ProcessStateCrashed, info.State)
		assert.Equal(t, domain.HealthStatusUnknown, info.Health,
			"a configured check on a crashed process must stay unknown, not collapse to none")
	})

	// A live checker owns the verdict, and reports Enabled=true only while its
	// loop is actually running.
	t.Run("live checker owns the status", func(t *testing.T) {
		mp := NewManagedProcess(domain.ProcessConfig{
			Name:        "web",
			Cmd:         "sleep 30",
			Healthcheck: &domain.HealthConfig{Cmd: "true"},
		}, nil, NewExecRunner(), logMgr)

		ctx, cancel := context.WithCancel(context.Background())
		// The explicit cancel below is the SUBJECT of this test, but it is not
		// reached if an assertion before it fails -- and then the checker's loop
		// would outlive the test, shelling out on its interval for as long as the
		// package keeps running. The deferred cancel is the backstop; cancelling
		// twice is a no-op.
		defer cancel()
		checker := NewHealthChecker("web", domain.HealthConfig{Cmd: "true"}, nil)
		checker.Start(ctx)
		mp.mu.Lock()
		mp.healthChecker = checker
		mp.mu.Unlock()

		info := mp.Info()
		assert.Equal(t, domain.HealthStatusUnknown, info.Health, "no check has completed yet")
		require.NotNil(t, info.HealthDetails)
		assert.True(t, info.HealthDetails.Enabled, "a running check loop reports enabled")

		// Cancelling the instance context stops the loop; Enabled must follow it
		// down rather than staying hardcoded true (#100 panel finding).
		cancel()
		assert.False(t, mp.Info().HealthDetails.Enabled)
	})
}

// healthyForeverConfig is a healthcheck whose start period is longer than any
// test run, so the checker's loop parks in its start-period select and never
// shells out. The tests below observe HealthDetails.Enabled -- i.e. whether the
// loop is alive at all -- so they need the loop RUNNING, not CHECKING, and this
// keeps them free of subprocess timing.
func healthyForeverConfig() *domain.HealthConfig {
	return &domain.HealthConfig{
		Cmd:         "true",
		Interval:    time.Hour,
		Timeout:     time.Minute,
		Retries:     1,
		StartPeriod: time.Hour,
	}
}

// newHealthCheckedFake builds a ManagedProcess with a healthcheck configured,
// wired to the given fake runner (mirrors newFakeManagedProcess).
func newHealthCheckedFake(t *testing.T, runner ProcessRunner) *ManagedProcess {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(func() { logMgr.Close() })
	return NewManagedProcess(domain.ProcessConfig{
		Name:        "web",
		Cmd:         "irrelevant",
		Healthcheck: healthyForeverConfig(),
	}, nil, runner, logMgr)
}

// TestManagedProcess_Monitor_NaturalExitStopsHealthChecker is the regression
// test for #107: when a process dies on its own (no Stop), the health checker
// must stop with it. Before the fix the instance context was cancelled ONLY by
// stop() and the launch-failure path, so a crashed process kept re-running the
// user's check command on its interval -- and reported enabled=true -- until
// some later Stop tore it down.
func TestManagedProcess_Monitor_NaturalExitStopsHealthChecker(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newFakeProcess(7100 + call) })
	mp := newHealthCheckedFake(t, runner)

	require.NoError(t, mp.Start(context.Background()))
	require.Equal(t, domain.ProcessStateRunning, mp.State())

	before := mp.Info()
	require.NotNil(t, before.HealthDetails)
	require.True(t, before.HealthDetails.Enabled, "a live run must report an enabled check loop")

	// The leader exits on its own -- no Stop, no signal. The group goes with it.
	fp := runner.last()
	fp.setAlive(false)
	fp.exitLeader(errors.New("boom"))

	waitForState(t, mp, domain.ProcessStateCrashed, 5*time.Second)

	after := mp.Info()
	require.NotNil(t, after.HealthDetails)
	assert.False(t, after.HealthDetails.Enabled,
		"a crashed process must not keep a health check loop running (#107)")
}

// TestManagedProcess_Monitor_HealthCheckerStopsBeforeOutputDrain pins the
// INSERTION POINT of the #107 fix, which is the part that actually matters:
// the cancel happens right after Wait() returns, BEFORE monitor's up-to-5s
// output-drain wait. This test holds a pipe open past the leader's exit (what a
// surviving grandchild does), so the drain is guaranteed to still be in
// progress, and asserts the checker is already down.
//
// If the cancel were moved to the terminal-state commit -- the other natural
// place for it -- this test fails: the checker would keep executing the user's
// command for the whole outputDrainTimeout window.
func TestManagedProcess_Monitor_HealthCheckerStopsBeforeOutputDrain(t *testing.T) {
	// A pipe whose write end the test holds: the stdout reader blocks in Read
	// until we close it, exactly like a grandchild that outlives the leader.
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	defer pr.Close()
	pipeClosed := false
	closePipe := func() {
		if !pipeClosed {
			pipeClosed = true
			pw.Close()
		}
	}
	defer closePipe()

	runner := newFakeRunner(func(call int) *fakeProcess {
		fp := newFakeProcess(7200 + call)
		fp.stdout = pr
		return fp
	})
	mp := newHealthCheckedFake(t, runner)

	require.NoError(t, mp.Start(context.Background()))
	require.True(t, mp.Info().HealthDetails.Enabled)

	fp := runner.last()
	fp.setAlive(false)
	start := time.Now()
	fp.exitLeader(nil)

	// The checker must go down promptly -- not after the drain window.
	require.Eventually(t, func() bool {
		return !mp.Info().HealthDetails.Enabled
	}, outputDrainTimeout/2, 5*time.Millisecond,
		"health checker must stop as soon as the leader exits, not wait out the output drain")
	elapsed := time.Since(start)

	// ...and prove the drain really was still pending when it did, so this is a
	// statement about ordering rather than about a fast machine: the terminal
	// state is only committed AFTER the drain, and the pipe is still open here.
	assert.Equal(t, domain.ProcessStateRunning, mp.State(),
		"the drain must still be in flight (state not yet committed) when the checker stops")
	assert.Less(t, elapsed, outputDrainTimeout,
		"checker stopped only after the full drain window")

	// Release the grandchild so the monitor finishes before the log manager is
	// torn down.
	closePipe()
	waitForState(t, mp, domain.ProcessStateCrashed, 5*time.Second)
}

// TestManagedProcess_Info_WaitingReportsNoPID pins the plan 013 D5 fix: a gated
// child re-run into the waiting state must report PID 0 (renders "-"), even
// though `current` still holds the reaped instance from its prior run. Guarding
// on IsStopped alone would surface that dead PID because waiting is not stopped.
func TestManagedProcess_Info_WaitingReportsNoPID(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name:      "migrate",
		Cmd:       "true",
		Kind:      domain.ProcessKindTask,
		DependsOn: []string{"db"},
	}, nil, NewExecRunner(), logMgr)

	// Simulate a completed prior run that left a reaped instance with a live PID.
	mp.current = &processInstance{proc: newFakeProcess(4321)}
	mp.state = domain.ProcessStateCompleted
	// A completed process would report PID 0 (IsStopped), but confirm the retained
	// instance really carries the stale PID so the waiting assertion is meaningful.
	require.Equal(t, 4321, mp.current.proc.PID())

	// Re-run: admit a fresh gated episode, moving the task to waiting while current
	// still points at the reaped prior instance.
	_, _, ok := mp.beginWaiting(func() {})
	require.True(t, ok, "completed task must admit a fresh waiting episode")
	require.Equal(t, domain.ProcessStateWaiting, mp.State())

	info := mp.Info()
	assert.Equal(t, domain.ProcessStateWaiting, info.State)
	assert.Equal(t, 0, info.PID, "a waiting re-run must report PID 0, not the reaped prior PID")
	assert.Equal(t, []string{"db"}, info.WaitingOn)
}

func TestManagedProcess_StopLogsExitCode(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "sleep 30",
	}, nil, runner, logMgr)

	ctx := context.Background()
	err := mp.Start(ctx)
	require.NoError(t, err)

	// Stop the process
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = mp.Stop(stopCtx)
	require.NoError(t, err)

	// Wait a moment for logs to be written
	time.Sleep(100 * time.Millisecond)

	// Check that the stopped message was logged with exit code
	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"test"}}, 0)

	var foundStoppedMessage bool
	for _, e := range entries {
		if e.Stream == domain.StreamStdout && e.Line == "stopped (rc=-15)" {
			foundStoppedMessage = true
			break
		}
	}

	assert.True(t, foundStoppedMessage, "should log 'stopped (rc=-15)' message when process is terminated by SIGTERM")
}

// TestManagedProcess_LoadEnvReloadsOnEveryStart verifies D1/A1c: a loadEnv
// closure is invoked at the top of every Start, so an edited env source
// (here a mutable closure over a variable, standing in for a rewritten
// env_file) is reflected on the very next Start after a Stop - not just
// once at process creation.
func TestManagedProcess_LoadEnvReloadsOnEveryStart(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "sleep 5",
	}, nil, runner, logMgr)

	current := "v1"
	mp.loadEnv = func() (map[string]string, error) {
		return map[string]string{"RELOAD_VAL": current}, nil
	}

	ctx := context.Background()
	require.NoError(t, mp.Start(ctx))
	assert.Equal(t, "v1", mp.Info().Env["RELOAD_VAL"])

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, mp.Stop(stopCtx))
	cancel()

	// Mutate the source the loader reads from, simulating an edited env_file.
	current = "v2"

	require.NoError(t, mp.Start(ctx))
	assert.Equal(t, "v2", mp.Info().Env["RELOAD_VAL"])

	// Cleanup
	stopCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	require.NoError(t, mp.Stop(stopCtx2))
}

// TestManagedProcess_LoadEnvFailureCrashesProcess verifies A2: when loadEnv
// fails, Start returns a typed error wrapping domain.ErrEnvReloadFailed, the
// state becomes crashed, no process is launched, and a subsequent Stop
// behaves sanely (ErrProcessNotRunning) rather than hanging or panicking on
// a dangling done channel.
func TestManagedProcess_LoadEnvFailureCrashesProcess(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "sleep 5",
	}, nil, runner, logMgr)

	loadErr := errors.New("env file missing")
	mp.loadEnv = func() (map[string]string, error) {
		return nil, loadErr
	}

	ctx := context.Background()
	err := mp.Start(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrEnvReloadFailed)
	assert.Contains(t, err.Error(), "env file missing")

	assert.Equal(t, domain.ProcessStateCrashed, mp.State())
	assert.Equal(t, 0, mp.Info().PID, "no process should have been launched")

	// A subsequent Stop should behave sanely rather than hang on a dangling
	// done channel or panic on a double-close.
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = mp.Stop(stopCtx)
	assert.ErrorIs(t, err, domain.ErrProcessNotRunning)
}

func TestManagedProcess_CrashLogsExitCode(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	mp := NewManagedProcess(domain.ProcessConfig{
		Name: "test",
		Cmd:  "exit 42",
	}, nil, runner, logMgr)

	ctx := context.Background()
	err := mp.Start(ctx)
	require.NoError(t, err)

	// Wait for process to exit on its own
	time.Sleep(500 * time.Millisecond)

	// Check that the crashed message was logged with exit code
	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"test"}}, 0)

	var foundCrashedMessage bool
	for _, e := range entries {
		if e.Stream == domain.StreamStderr && e.Line == "exited unexpectedly (rc=42)" {
			foundCrashedMessage = true
			break
		}
	}

	assert.True(t, foundCrashedMessage, "should log 'exited unexpectedly (rc=42)' message when process exits with error code")
}
