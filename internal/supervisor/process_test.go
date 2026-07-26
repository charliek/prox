package supervisor

import (
	"context"
	"errors"
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
	assert.Equal(t, domain.HealthStatusUnknown, info.Health)
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
