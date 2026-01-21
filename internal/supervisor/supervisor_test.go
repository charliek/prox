package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestConfig(processes map[string]string) *config.Config {
	cfg := &config.Config{
		API: config.APIConfig{
			Port: 5555,
			Host: "127.0.0.1",
		},
		Processes: make(map[string]config.ProcessConfig),
	}
	for name, cmd := range processes {
		cfg.Processes[name] = config.ProcessConfig{Cmd: cmd}
	}
	return cfg
}

func TestSupervisor_StartStop(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{
		"test1": "sleep 30",
		"test2": "sleep 30",
	})

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)

	// Check all processes started
	processes := sup.Processes()
	assert.Len(t, processes, 2)

	for _, p := range processes {
		assert.Equal(t, "running", string(p.State))
	}

	// Check status
	status := sup.Status()
	assert.Equal(t, "running", status.State)
	assert.True(t, status.UptimeSeconds() >= 0)

	// Stop
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = sup.Stop(stopCtx)
	require.NoError(t, err)

	// Check all stopped
	processes = sup.Processes()
	for _, p := range processes {
		assert.True(t, p.State.IsStopped())
	}
}

func TestSupervisor_ProcessControl(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{
		"test": "sleep 30",
	})

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)

	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Stop(stopCtx)
	}()

	t.Run("get process info", func(t *testing.T) {
		info, err := sup.Process("test")
		require.NoError(t, err)
		assert.Equal(t, "test", info.Name)
		assert.Equal(t, "running", string(info.State))
	})

	t.Run("process not found", func(t *testing.T) {
		_, err := sup.Process("nonexistent")
		assert.ErrorIs(t, err, domain.ErrProcessNotFound)
	})

	t.Run("stop process", func(t *testing.T) {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := sup.StopProcess(stopCtx, "test")
		require.NoError(t, err)

		info, _ := sup.Process("test")
		assert.True(t, info.State.IsStopped())
	})

	t.Run("start process", func(t *testing.T) {
		err := sup.StartProcess(ctx, "test")
		require.NoError(t, err)

		info, _ := sup.Process("test")
		assert.Equal(t, "running", string(info.State))
	})

	t.Run("restart process", func(t *testing.T) {
		info1, _ := sup.Process("test")
		pid1 := info1.PID

		restartCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := sup.RestartProcess(restartCtx, "test")
		require.NoError(t, err)

		info2, _ := sup.Process("test")
		assert.NotEqual(t, pid1, info2.PID)
		assert.Equal(t, 1, info2.RestartCount)
	})
}

func TestSupervisor_Events(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{
		"test": "echo hello",
	})

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())
	events := sup.Subscribe()

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)

	// Should receive supervisor start event
	select {
	case e := <-events:
		assert.Equal(t, EventTypeSupervisorStart, e.Type)
	case <-time.After(time.Second):
		t.Fatal("expected supervisor start event")
	}

	// Should receive process started event
	select {
	case e := <-events:
		assert.Equal(t, EventTypeProcessStarted, e.Type)
		assert.Equal(t, "test", e.Process)
	case <-time.After(time.Second):
		t.Fatal("expected process started event")
	}

	// Stop
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sup.Stop(stopCtx)
}

func TestSupervisor_StartSelectedProcesses(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{
		"web":    "sleep 30",
		"api":    "sleep 30",
		"worker": "sleep 30",
	})

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.StartProcesses(ctx, []string{"web", "api"})
	require.NoError(t, err)

	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Stop(stopCtx)
	}()

	processes := sup.Processes()
	assert.Len(t, processes, 2)

	// web and api should be running, worker should not exist
	_, err = sup.Process("web")
	assert.NoError(t, err)

	_, err = sup.Process("api")
	assert.NoError(t, err)

	_, err = sup.Process("worker")
	assert.ErrorIs(t, err, domain.ErrProcessNotFound)
}

func TestSupervisor_SystemLog(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	sup.SystemLog("test message %d", 42)

	// Wait a moment for log to be written
	time.Sleep(50 * time.Millisecond)

	// Check the log was written as "system"
	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"system"}}, 0)

	var foundMessage bool
	for _, e := range entries {
		if e.Process == "system" && e.Line == "test message 42" {
			foundMessage = true
			break
		}
	}

	assert.True(t, foundMessage, "SystemLog should write log entry with process name 'system'")
}

func TestSupervisor_StopLogsSIGTERM(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{
		"test": "sleep 30",
	})

	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)

	// Stop
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sup.Stop(stopCtx)

	// Wait a moment for logs to be written
	time.Sleep(100 * time.Millisecond)

	// Check that "sending SIGTERM" message was logged
	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"system"}}, 0)

	var foundSIGTERMMessage bool
	for _, e := range entries {
		if e.Process == "system" && strings.HasPrefix(e.Line, "sending SIGTERM to test (pid ") {
			foundSIGTERMMessage = true
			break
		}
	}

	assert.True(t, foundSIGTERMMessage, "Stop should log 'sending SIGTERM to test (pid X)' message")
}
