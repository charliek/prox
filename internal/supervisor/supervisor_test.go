package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
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
	events := sup.subscribeEvents()

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

// TestSupervisor_CreateManagedProcess_HealthcheckTiming is the regression test
// for issue #31: createManagedProcess must propagate the full healthcheck
// timing (interval/timeout/retries/start_period), not just cmd, so configured
// timing takes effect instead of being silently replaced by WithDefaults.
func TestSupervisor_CreateManagedProcess_HealthcheckTiming(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	t.Run("non-default timing is honored, not WithDefaults", func(t *testing.T) {
		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{
			Cmd: "true",
			Healthcheck: &config.HealthcheckConfig{
				Cmd:         "true",
				Interval:    "7s",
				Timeout:     "3s",
				Retries:     5,
				StartPeriod: "11s",
			},
		})
		require.NoError(t, err)
		require.NotNil(t, mp)
		require.NotNil(t, mp.config.Healthcheck)

		assert.Equal(t, "true", mp.config.Healthcheck.Cmd)
		assert.Equal(t, 7*time.Second, mp.config.Healthcheck.Interval)
		assert.Equal(t, 3*time.Second, mp.config.Healthcheck.Timeout)
		assert.Equal(t, 5, mp.config.Healthcheck.Retries)
		assert.Equal(t, 11*time.Second, mp.config.Healthcheck.StartPeriod)

		// Explicitly NOT the WithDefaults values (10s/5s/3/30s).
		assert.NotEqual(t, 10*time.Second, mp.config.Healthcheck.Interval)
		assert.NotEqual(t, 5*time.Second, mp.config.Healthcheck.Timeout)
		assert.NotEqual(t, 3, mp.config.Healthcheck.Retries)
		assert.NotEqual(t, 30*time.Second, mp.config.Healthcheck.StartPeriod)
	})

	t.Run("cmd-only healthcheck creates with zero timing", func(t *testing.T) {
		mp, err := sup.createManagedProcess("svc2", config.ProcessConfig{
			Cmd:         "true",
			Healthcheck: &config.HealthcheckConfig{Cmd: "true"},
		})
		require.NoError(t, err)
		require.NotNil(t, mp)
		require.NotNil(t, mp.config.Healthcheck)

		assert.Equal(t, "true", mp.config.Healthcheck.Cmd)
		assert.Equal(t, time.Duration(0), mp.config.Healthcheck.Interval)
		assert.Equal(t, time.Duration(0), mp.config.Healthcheck.Timeout)
		assert.Equal(t, 0, mp.config.Healthcheck.Retries)
		assert.Equal(t, time.Duration(0), mp.config.Healthcheck.StartPeriod)
	})
}

// TestSupervisor_CreateManagedProcess_InvalidHealthcheckDuration verifies the
// defensive error path: a malformed duration surfaces a process-named error and
// no ManagedProcess (rather than silently defaulting).
func TestSupervisor_CreateManagedProcess_InvalidHealthcheckDuration(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

	mp, err := sup.createManagedProcess("svc", config.ProcessConfig{
		Cmd: "true",
		Healthcheck: &config.HealthcheckConfig{
			Cmd:      "true",
			Interval: "3x",
		},
	})
	require.Error(t, err)
	assert.Nil(t, mp)
	assert.Contains(t, err.Error(), "svc")
	assert.Contains(t, err.Error(), "interval")
}

// TestSupervisor_CreateManagedProcess_StopTimeoutResolution covers the
// #35/D1 effective stop-budget precedence: per-process stop_timeout beats the
// global shutdown_timeout, which beats constants.DefaultShutdownTimeout. The
// resolved value lands in the ManagedProcess's promoted shutdownTimeout field
// (read via StopTimeout()); domain.ProcessConfig.StopTimeout carries only the
// raw per-process value (0 = unset), never the resolved one.
func TestSupervisor_CreateManagedProcess_StopTimeoutResolution(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	t.Run("per-process stop_timeout overrides global shutdown_timeout", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		cfg.ShutdownTimeout = "20s"
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true", StopTimeout: "45s"})
		require.NoError(t, err)
		assert.Equal(t, 45*time.Second, mp.StopTimeout())
		assert.Equal(t, 45*time.Second, mp.config.StopTimeout)
	})

	t.Run("global shutdown_timeout used when process has none", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		cfg.ShutdownTimeout = "20s"
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true"})
		require.NoError(t, err)
		assert.Equal(t, 20*time.Second, mp.StopTimeout())
		assert.Equal(t, time.Duration(0), mp.config.StopTimeout, "raw per-process value stays unset")
	})

	t.Run("constant default used when neither is set", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true"})
		require.NoError(t, err)
		assert.Equal(t, constants.DefaultShutdownTimeout, mp.StopTimeout())
	})

	t.Run("SupervisorConfig.ShutdownTimeout is honored before the constant", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		supConfig := DefaultSupervisorConfig()
		supConfig.ShutdownTimeout = 25 * time.Second
		sup := New(cfg, logMgr, nil, supConfig)

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true"})
		require.NoError(t, err)
		assert.Equal(t, 25*time.Second, mp.StopTimeout(),
			"a directly-configured SupervisorConfig timeout must not be inert")
	})

	t.Run("malformed global shutdown_timeout surfaces a process-named error", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		cfg.ShutdownTimeout = "3x"
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true"})
		require.Error(t, err)
		assert.Nil(t, mp)
		assert.Contains(t, err.Error(), "svc")
		assert.Contains(t, err.Error(), "shutdown_timeout")
	})

	t.Run("malformed per-process stop_timeout surfaces a process-named error", func(t *testing.T) {
		cfg := makeTestConfig(map[string]string{})
		sup := New(cfg, logMgr, nil, DefaultSupervisorConfig())

		mp, err := sup.createManagedProcess("svc", config.ProcessConfig{Cmd: "true", StopTimeout: "1s"})
		require.Error(t, err)
		assert.Nil(t, mp)
		assert.Contains(t, err.Error(), "svc")
		assert.Contains(t, err.Error(), "stop_timeout")
	})
}

// TestDefaultSupervisorConfig_ShutdownTimeout pins DefaultSupervisorConfig's
// default to constants.DefaultShutdownTimeout instead of its own literal
// (#35).
func TestDefaultSupervisorConfig_ShutdownTimeout(t *testing.T) {
	assert.Equal(t, constants.DefaultShutdownTimeout, DefaultSupervisorConfig().ShutdownTimeout)
}

// TestSupervisor_RestartProcess_HealthCheckerSurvivesRequestContext is the
// regression test for the C2 restart-context bug: RestartProcess must start
// the replacement process on the supervisor's long-lived context, not on a
// context derived from the (request-scoped) ctx the caller passed in.
//
// Before the fix, Supervisor.RestartProcess built a single timeout context
// from the caller's ctx and threaded it through ManagedProcess.Restart into
// both Stop and Start. Start derives processCtx (and the health checker's
// context) from whatever context it's given, so once an HTTP handler
// returns and its request context is cancelled, the replacement's
// processCtx -- and therefore its health checker -- was torn down even
// though the process itself kept running. This test simulates exactly that:
// a cancelable "request" context that is cancelled the instant
// RestartProcess returns, then asserts the replacement's health checker
// keeps ticking (status becomes and remains "healthy") well past that
// cancellation.
//
// Health-check ticking is used as the observable here (rather than a new
// production accessor into the process's internal context) because it is
// the most direct symptom described by the bug report and is fully exposed
// via Supervisor.Process already.
func TestSupervisor_RestartProcess_HealthCheckerSurvivesRequestContext(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()

	runner := NewExecRunner()

	// Fast, deterministic health-check timing is driven through the real
	// config -> domain conversion path (Supervisor.createManagedProcess) -- the
	// same path production uses -- so the test exercises the actual wiring. We
	// register the resulting ManagedProcess on the supervisor, then drive
	// everything else through the real Supervisor.StartProcess/RestartProcess
	// API used in production.
	cfg := makeTestConfig(map[string]string{})
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig())

	ctx := context.Background()
	_, err := sup.Start(ctx)
	require.NoError(t, err)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Stop(stopCtx)
	}()

	mp, err := sup.createManagedProcess("test", config.ProcessConfig{
		Cmd: "sleep 30",
		Healthcheck: &config.HealthcheckConfig{
			Cmd:         "true", // always succeeds, cheap
			Interval:    "50ms",
			Timeout:     "1s",
			Retries:     1,
			StartPeriod: "50ms",
		},
	})
	require.NoError(t, err)

	sup.mu.Lock()
	sup.processes["test"] = mp
	sup.mu.Unlock()

	require.NoError(t, sup.StartProcess(ctx, "test"))

	// Establish the checker is healthy before restarting.
	waitForHealthCheckerHealthy(t, sup, "test", 2*time.Second)

	firstInfo, err := sup.Process("test")
	require.NoError(t, err)
	firstPID := firstInfo.PID

	// Simulate an HTTP handler: a cancelable request context that is
	// cancelled the instant the handler (RestartProcess) returns -- this is
	// exactly what net/http does once a response has been written.
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	err = sup.RestartProcess(requestCtx, "test")
	cancelRequest()
	require.NoError(t, err)

	info, err := sup.Process("test")
	require.NoError(t, err)
	assert.Equal(t, domain.ProcessStateRunning, info.State)
	assert.NotEqual(t, firstPID, info.PID, "restart should replace the process with a new PID")
	assert.Equal(t, 1, info.RestartCount)

	// The replacement's health checker must keep ticking even though the
	// request context that initiated the restart is now cancelled. Without
	// the C2 fix this never becomes healthy (status stays "unknown" forever
	// because the health checker's context was cancelled along with the
	// request context).
	waitForHealthCheckerHealthy(t, sup, "test", 2*time.Second)

	postCancelInfo, err := sup.Process("test")
	require.NoError(t, err)
	require.NotNil(t, postCancelInfo.HealthDetails)
	lastCheck1 := postCancelInfo.HealthDetails.LastCheck
	require.False(t, lastCheck1.IsZero())

	// Confirm the checker is still actively ticking rather than frozen on a
	// single successful check that happened to sneak in before teardown.
	time.Sleep(300 * time.Millisecond)
	laterInfo, err := sup.Process("test")
	require.NoError(t, err)
	require.NotNil(t, laterInfo.HealthDetails)
	assert.True(t, laterInfo.HealthDetails.LastCheck.After(lastCheck1),
		"health checker should still be ticking after the request context is cancelled")
	assert.Equal(t, domain.ProcessStateRunning, laterInfo.State)
}

// waitForHealthCheckerHealthy polls the supervisor until the named process
// reports domain.HealthStatusHealthy, failing the test if timeout elapses
// first.
func waitForHealthCheckerHealthy(t *testing.T, sup *Supervisor, name string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastStatus domain.HealthStatus
	for time.Now().Before(deadline) {
		info, err := sup.Process(name)
		require.NoError(t, err)
		lastStatus = info.Health
		if info.Health == domain.HealthStatusHealthy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %s did not become/remain healthy within %v (last health status: %q)", name, timeout, lastStatus)
}
