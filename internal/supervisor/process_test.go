package supervisor

import (
	"context"
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

	err = mp.Restart(restartCtx)
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
