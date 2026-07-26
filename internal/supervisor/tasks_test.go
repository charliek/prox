package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingReader models a grandchild holding stdout open: Read blocks until
// release is closed, then reports EOF. It lets a test delay a task's output drain
// (and thus the monitor's state commit / inst.done) past the run budget while the
// leader has already exited (fix 6).
type blockingReader struct {
	release <-chan struct{}
	done    bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	<-r.release
	r.done = true
	return 0, io.EOF
}

// --- scripted runner keyed by name -----------------------------------------
//
// scriptedRunner hands out one fakeProcess per launch, keyed by the launched
// config's Name, so a task test can drive a specific task/process's exit
// independently. A task run BLOCKS (its Wait stays open) until the test calls
// completeTask/crashTask, modelling a run-to-completion child under test control.
type scriptedRunner struct {
	mu       sync.Mutex
	nextPID  int
	procs    map[string]*fakeProcess
	behavior map[string]func(*fakeProcess, os.Signal)
	stdout   map[string]io.Reader // optional per-name stdout (e.g. a blockingReader)
	starts   map[string]int
	order    []string
}

func newScriptedRunner() *scriptedRunner {
	return &scriptedRunner{
		nextPID:  6000,
		procs:    map[string]*fakeProcess{},
		behavior: map[string]func(*fakeProcess, os.Signal){},
		stdout:   map[string]io.Reader{},
		starts:   map[string]int{},
	}
}

func (r *scriptedRunner) Start(ctx context.Context, cfg domain.ProcessConfig, env map[string]string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPID++
	fp := newFakeProcess(r.nextPID)
	if b := r.behavior[cfg.Name]; b != nil {
		fp.onSignal = b
	} else {
		// Default: die gracefully on SIGTERM so a stop/shutdown/timeout reaps fast.
		fp.onSignal = gracefulOnTerm
	}
	if sr := r.stdout[cfg.Name]; sr != nil {
		fp.stdout = sr
	}
	r.procs[cfg.Name] = fp
	r.starts[cfg.Name]++
	r.order = append(r.order, cfg.Name)
	return fp, nil
}

func (r *scriptedRunner) proc(name string) *fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[name]
}

func (r *scriptedRunner) startCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[name]
}

func (r *scriptedRunner) launchOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// completeTask ends a running task with a natural exit 0 (its group goes away),
// driving the monitor to commit ProcessStateCompleted.
func completeTask(t *testing.T, r *scriptedRunner, name string) {
	t.Helper()
	fp := r.proc(name)
	require.NotNilf(t, fp, "task %q was never launched", name)
	fp.setAlive(false)
	fp.exitLeader(nil)
}

// crashTask ends a running task with a non-zero exit (its group goes away),
// driving the monitor to commit ProcessStateCrashed.
func crashTask(t *testing.T, r *scriptedRunner, name string) {
	t.Helper()
	fp := r.proc(name)
	require.NotNilf(t, fp, "task %q was never launched", name)
	fp.setAlive(false)
	fp.exitLeader(errors.New("boom")) // non-ExitError -> exit code 1 -> crashed
}

// --- task supervisor builder -----------------------------------------------

type taskSpec struct {
	dependsOn []string
	timeout   string // raw timeout string ("" default 60s, "0" unlimited, "100ms", ...)
}

// taskSupervisor builds a supervisor with the given processes (each with a
// depends_on list), tasks (depends_on + timeout), and dependencies (driven by
// prober), all launched through a scriptedRunner.
func taskSupervisor(t *testing.T, procDeps map[string][]string, tasks map[string]taskSpec, deps map[string]depSpec, prober Prober) (*Supervisor, *scriptedRunner, *logs.Manager) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 400})

	cfg := &config.Config{
		API:          config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes:    map[string]config.ProcessConfig{},
		Dependencies: map[string]config.DependencyConfig{},
		Tasks:        map[string]config.TaskConfig{},
	}
	for name, on := range procDeps {
		cfg.Processes[name] = config.ProcessConfig{Cmd: "sleep 60", DependsOn: on}
	}
	for name, spec := range tasks {
		cfg.Tasks[name] = config.TaskConfig{Cmd: "sleep 60", DependsOn: spec.dependsOn, Timeout: spec.timeout}
	}
	for name := range deps {
		cfg.Dependencies[name] = config.DependencyConfig{}
	}

	runner := newScriptedRunner()
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig())

	depMap := map[string]domain.DependencyConfig{}
	for name, spec := range deps {
		onFailure := spec.onFailure
		if onFailure == "" {
			onFailure = domain.FailurePolicyFail
		}
		timeout := spec.timeout
		if timeout == 0 {
			timeout = 2 * time.Second
		}
		interval := spec.interval
		if interval == 0 {
			interval = 10 * time.Millisecond
		}
		depMap[name] = domain.DependencyConfig{
			Name:      name,
			Check:     domain.DependencyCheck{Kind: domain.CheckKindTCP, Target: name, Timeout: timeout, Interval: interval},
			Start:     spec.start,
			OnFailure: onFailure,
		}
	}
	sup.newResolver = func() *Resolver {
		return NewResolver(depMap, "", nil, nil, WithProber(prober), WithClock(realClock{}), WithAttemptCap(60*time.Second))
	}
	return sup, runner, logMgr
}

// --- tests ------------------------------------------------------------------

// Exit 0 -> completed: terminal, PID suppressed, uptime frozen at completion.
func TestTask_ExitZeroCompletes(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	info, _ := sup.Process("migrate")
	assert.Equal(t, domain.ProcessKindTask, info.Kind)
	assert.Greater(t, info.PID, 0, "a running task reports its PID")

	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)

	info, _ = sup.Process("migrate")
	assert.True(t, info.State.IsStopped(), "completed is terminal")
	assert.Equal(t, 0, info.PID, "PID suppressed once completed")
	frozen := info.UptimeSeconds()
	time.Sleep(1100 * time.Millisecond)
	info2, _ := sup.Process("migrate")
	assert.Equal(t, frozen, info2.UptimeSeconds(), "uptime frozen at completion, does not keep ticking")
}

// Exit non-zero -> crashed (sticky terminal).
func TestTask_NonZeroExitCrashes(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	crashTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCrashed)
}

// Run-timeout -> escalated stop -> crashed, with a distinct "task timed out"
// log line.
func TestTask_TimeoutEscalatesToCrashed(t *testing.T) {
	sup, _, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {timeout: "120ms"}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	// Never completed: the run budget elapses and the coordinator escalate-kills it.
	waitState(t, sup, "migrate", domain.ProcessStateCrashed)

	entries, _, _ := logMgr.Query(domain.LogFilter{Processes: []string{"migrate"}}, 0)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Line, "task timed out after") {
			found = true
		}
	}
	assert.True(t, found, "a timed-out task logs a distinct timeout line")
}

// timeout: 0 means unlimited -- the task keeps running well past the default
// budget window and only ends when it exits on its own.
func TestTask_UnlimitedTimeoutDoesNotKill(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {timeout: "0"}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	// Stay running across a window; an unlimited task must not be killed.
	time.Sleep(400 * time.Millisecond)
	info, _ := sup.Process("migrate")
	assert.Equal(t, domain.ProcessStateRunning, info.State, "unlimited task keeps running")

	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
}

// A plain process whose leader exits 0 STILL crashes (task-mode mapping must not
// leak to plain processes).
func TestProcess_ExitZeroStillCrashes(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, map[string][]string{"svc": nil}, nil, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "svc", domain.ProcessStateRunning)
	fp := runner.proc("svc")
	fp.setAlive(false)
	fp.exitLeader(nil) // rc=0
	waitState(t, sup, "svc", domain.ProcessStateCrashed)
}

// A dependent process waits for a task, the task runs once, and two dependents
// join a single task run (single-flight).
func TestTask_DependentsWaitAndJoinSingleRun(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t,
		map[string][]string{"web": {"migrate"}, "worker": {"migrate"}},
		map[string]taskSpec{"migrate": {}},
		nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	// Both dependents wait on the task, neither launched yet.
	assert.Equal(t, domain.ProcessStateWaiting, mustState(t, sup, "web"))
	assert.Equal(t, domain.ProcessStateWaiting, mustState(t, sup, "worker"))
	assert.Equal(t, 0, runner.startCount("web"))

	completeTask(t, runner, "migrate")
	waitState(t, sup, "web", domain.ProcessStateRunning)
	waitState(t, sup, "worker", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.startCount("migrate"), "the task ran exactly once for both dependents")
}

// A dependent restart does NOT re-run a completed task; a manual task restart
// DOES re-run it, and dependents are unaffected.
func TestTask_RestartSemantics(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t,
		map[string][]string{"web": {"migrate"}},
		map[string]taskSpec{"migrate": {}},
		nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	completeTask(t, runner, "migrate")
	waitState(t, sup, "web", domain.ProcessStateRunning)
	require.Equal(t, 1, runner.startCount("migrate"))

	// A dependent restart re-resolves its targets; a completed task is cached
	// (once per lifetime), so it is NOT re-run.
	require.NoError(t, sup.RestartProcess(context.Background(), "web"))
	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.startCount("migrate"), "dependent restart must not re-run a completed task")

	// A manual task restart resets its once-flag and re-runs it; the dependent is
	// unaffected (still running).
	require.NoError(t, sup.RestartProcess(context.Background(), "migrate"))
	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	assert.Equal(t, 2, runner.startCount("migrate"), "manual task restart re-runs the task")
	assert.Equal(t, domain.ProcessStateRunning, mustState(t, sup, "web"), "the dependent is unaffected by a task re-run")
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
	info, _ := sup.Process("migrate")
	assert.Equal(t, 1, info.RestartCount, "a manual re-run bumps the restart count")
}

// A crashed task blocks its dependents, naming the task.
func TestTask_CrashBlocksDependent(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t,
		map[string][]string{"web": {"migrate"}},
		map[string]taskSpec{"migrate": {}},
		nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	crashTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCrashed)

	waitState(t, sup, "web", domain.ProcessStateBlocked)
	assert.Equal(t, []string{"migrate"}, sup.processes["web"].BlockedBy(), "dependent blocked, naming the crashed task")
}

// A task chain: taskB depends_on [taskA, dep]; taskA must complete and dep be
// healthy before taskB launches (ordering respected).
func TestTask_ChainOrdering(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, runner, logMgr := taskSupervisor(t, nil,
		map[string]taskSpec{
			"taskA": {},
			"taskB": {dependsOn: []string{"taskA", "db"}},
		},
		map[string]depSpec{"db": {}},
		prober)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "taskA", domain.ProcessStateRunning)
	// taskB is still waiting on taskA (which has not completed) -- not launched.
	assert.Equal(t, 0, runner.startCount("taskB"), "taskB must not launch before taskA completes")

	completeTask(t, runner, "taskA")
	waitState(t, sup, "taskB", domain.ProcessStateRunning)
	order := runner.launchOrder()
	assert.Less(t, indexOf(order, "taskA"), indexOf(order, "taskB"), "taskA launched before taskB")

	completeTask(t, runner, "taskB")
	waitState(t, sup, "taskB", domain.ProcessStateCompleted)
}

// Bare `prox up` runs a task that nothing depends on.
func TestTask_BareUpRunsUndependedTask(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"solo": {}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "solo", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.startCount("solo"))
	completeTask(t, runner, "solo")
	waitState(t, sup, "solo", domain.ProcessStateCompleted)
}

// A subset start naming a task runs it (and its closure) only; a task outside the
// subset and depended on by nothing started is not run.
func TestTask_SubsetStartRunsNamedTaskOnly(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil,
		map[string]taskSpec{"migrate": {}, "other": {}},
		nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.StartProcesses(context.Background(), []string{"migrate"})
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	assert.Equal(t, 0, runner.startCount("other"), "a task outside the subset (and undepended) must not run")
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
}

// Stopping a task mid-run settles it stopped (not crashed).
func TestTask_StopMidRunIsStopped(t *testing.T) {
	sup, _, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {timeout: "0"}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	require.NoError(t, sup.StopProcess(context.Background(), "migrate"))
	waitState(t, sup, "migrate", domain.ProcessStateStopped)
}

// Shutdown cancels a process waiting on a task cleanly (both end stopped, never
// blocked/crashed).
func TestTask_ShutdownCancelsWaiterCleanly(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block") // the task's own dependency never resolves
	sup, _, logMgr := taskSupervisor(t,
		map[string][]string{"web": {"migrate"}},
		map[string]taskSpec{"migrate": {dependsOn: []string{"db"}}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	waitState(t, sup, "migrate", domain.ProcessStateWaiting)
	waitState(t, sup, "web", domain.ProcessStateWaiting)

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sup.Stop(stopCtx))

	assert.Equal(t, domain.ProcessStateStopped, mustState(t, sup, "web"), "waiter ends stopped on shutdown")
	assert.Equal(t, domain.ProcessStateStopped, mustState(t, sup, "migrate"), "waiting task ends stopped on shutdown")
}

// Redefine swaps a changed dependency definition and Resets it so the next
// Demand re-resolves against the new definition; an unchanged Redefine is a
// no-op (no re-probe).
func TestResolver_RedefineSwapsDefinition(t *testing.T) {
	prober := newCoordProber()
	prober.set("old", "healthy")
	prober.set("new", "healthy")
	depMap := map[string]domain.DependencyConfig{
		"db": {Name: "db", Check: domain.DependencyCheck{Kind: domain.CheckKindCmd, Target: "old", Timeout: time.Second, Interval: 10 * time.Millisecond}, OnFailure: domain.FailurePolicyFail},
	}
	r := NewResolver(depMap, "", nil, nil, WithProber(prober), WithClock(realClock{}))
	defer r.Close()

	require.True(t, r.Demand(context.Background(), "db").Ready())
	require.Greater(t, prober.probeCount("old"), 0)

	// An identical Redefine is a no-op: no reset, cached outcome stands.
	assert.False(t, r.Redefine("db", depMap["db"]), "identical definition is a no-op")

	// A changed definition (new check target) swaps + resets; the next Demand
	// probes the NEW target.
	changed := depMap["db"]
	changed.Check.Target = "new"
	assert.True(t, r.Redefine("db", changed), "changed definition swaps and resets")
	require.True(t, r.Demand(context.Background(), "db").Ready())
	assert.Greater(t, prober.probeCount("new"), 0, "re-resolution used the new definition")
}

// A gated restart after a reload that changed a dependency's check target
// re-resolves against the NEW definition (plan 013 D6).
func TestReload_RestartRefreshesChangedDependency(t *testing.T) {
	dir := t.TempDir()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1500 + call) })
	oldYAML := "processes:\n  web:\n    cmd: \"sleep 60\"\n    depends_on: [db]\n" +
		"dependencies:\n  db:\n    check:\n      cmd: \"checkold\"\n      interval: 10ms\n      timeout: 2s\n"
	sup := newReloadSupervisor(t, dir, oldYAML, runner)

	prober := newCoordProber()
	prober.set("checkold", "healthy")
	prober.set("checknew", "healthy")
	sup.newResolver = func() *Resolver {
		return NewResolver(sup.domainDependencies(sup.config), dir, nil, nil,
			WithProber(prober), WithClock(realClock{}), WithAttemptCap(60*time.Second))
	}

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateRunning)
	require.Greater(t, prober.probeCount("checkold"), 0)
	oldCount := prober.probeCount("checkold")

	// Change the dependency's check target and restart the dependent: the restart
	// must resolve against the NEW definition, probing "checknew".
	newYAML := "processes:\n  web:\n    cmd: \"sleep 60\"\n    depends_on: [db]\n" +
		"dependencies:\n  db:\n    check:\n      cmd: \"checknew\"\n      interval: 10ms\n      timeout: 2s\n"
	writeConfigFile(t, dir, newYAML)

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	require.NoError(t, sup.RestartProcess(rctx, "web"))
	waitState(t, sup, "web", domain.ProcessStateRunning)

	assert.Greater(t, prober.probeCount("checknew"), 0, "restart re-resolved against the reloaded check target")
	assert.Equal(t, oldCount, prober.probeCount("checkold"), "the stale definition was not re-probed")
}

// Fix 1: a process waiting on a task, when the task is re-run mid-wait, must
// re-demand and complete against the NEW run (never strand in waiting).
func TestTask_Fix1_WaiterReDemandsRerunTarget(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t,
		map[string][]string{"web": {"migrate"}},
		map[string]taskSpec{"migrate": {timeout: "0"}},
		nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	waitState(t, sup, "web", domain.ProcessStateWaiting)

	// Re-run the task while web is still waiting on it. This retires web's joined
	// node; web must re-demand and join the fresh run rather than strand.
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	require.NoError(t, sup.RestartProcess(rctx, "migrate"))

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	require.Equal(t, 2, runner.startCount("migrate"), "the task re-ran")
	// web is still waiting (on the NEW run), not stranded/blocked.
	assert.Equal(t, domain.ProcessStateWaiting, mustState(t, sup, "web"))

	completeTask(t, runner, "migrate")
	waitState(t, sup, "web", domain.ProcessStateRunning)
}

// Fix 1 (dependency): a process waiting on a dependency, when the dependency is
// Redefined mid-demand (its node retired), must re-demand and resolve against the
// new definition rather than strand.
func TestTask_Fix1_WaiterReDemandsRedefinedDependency(t *testing.T) {
	prober := newCoordProber()
	prober.set("dbOld", "block")   // initial definition never resolves
	prober.set("dbNew", "healthy") // redefined target resolves
	depMap := map[string]domain.DependencyConfig{
		"db": {Name: "db", Check: domain.DependencyCheck{Kind: domain.CheckKindTCP, Target: "dbOld", Timeout: 30 * time.Second, Interval: 10 * time.Millisecond}, OnFailure: domain.FailurePolicyFail},
	}
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})
	defer logMgr.Close()
	cfg := &config.Config{
		API:          config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes:    map[string]config.ProcessConfig{"web": {Cmd: "sleep 60", DependsOn: []string{"db"}}},
		Dependencies: map[string]config.DependencyConfig{"db": {}},
		Tasks:        map[string]config.TaskConfig{},
	}
	runner := newScriptedRunner()
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig())
	sup.newResolver = func() *Resolver {
		return NewResolver(depMap, "", nil, nil, WithProber(prober), WithClock(realClock{}), WithAttemptCap(60*time.Second))
	}

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateWaiting)

	// Redefine db to the resolving target; web's in-flight demand is canceled and
	// must re-demand against the new generation.
	changed := depMap["db"]
	changed.Check.Target = "dbNew"
	require.True(t, sup.resolver.Redefine("db", changed))

	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Greater(t, prober.probeCount("dbNew"), 0, "web resolved against the redefined dependency")
}

// Fix 2: N concurrent manual starts of a completed task admit EXACTLY ONE re-run
// episode; the others get a clean already-active error. Race-gated.
func TestTask_Fix2_ConcurrentRerunsAdmitOne(t *testing.T) {
	sup, runner, logMgr := taskSupervisor(t, nil, map[string]taskSpec{"migrate": {timeout: "0"}}, nil, newCoordProber())
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = sup.StartProcess(context.Background(), "migrate")
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, e := range errs {
		if e == nil {
			admitted++
		} else {
			assert.Truef(t, isAlreadyActive(e), "refusals must be already-active, got %v", e)
		}
	}
	assert.Equal(t, 1, admitted, "exactly one concurrent re-run admits an episode")

	// StartProcess admits synchronously (beginWaiting under taskMu) but the LAUNCH
	// happens in the async runTask goroutine, so the started-count is only settled
	// once the re-run is actually running -- asserting it immediately after
	// wg.Wait races that goroutine (a pre-existing flake). Wait for the single
	// re-run to reach running FIRST, then assert exactly one re-run launched;
	// admitted==1 above already bounds it to a single episode.
	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	assert.Equal(t, 2, runner.startCount("migrate"), "exactly one re-run launched (initial + 1)")

	// The re-run settles terminally.
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
}

// Fix 4: a reload that ADDS a dependency and makes a task depend on it succeeds
// -- the rerun episode classifies the new name as a dependency (fresh view), not
// an unknown task.
func TestTask_Fix4_ReloadAddedDependencyClassifies(t *testing.T) {
	dir := t.TempDir()
	runner := newScriptedRunner()
	initialYAML := "processes:\n  keep:\n    cmd: \"sleep 60\"\n" +
		"tasks:\n  migrate:\n    cmd: \"sleep 60\"\n"
	sup := newReloadSupervisor(t, dir, initialYAML, runner)

	prober := newCoordProber()
	prober.set("cachecheck", "healthy")
	sup.newResolver = func() *Resolver {
		return NewResolver(sup.domainDependencies(sup.config), dir, nil, nil,
			WithProber(prober), WithClock(realClock{}), WithAttemptCap(60*time.Second))
	}

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)

	// Reload: add dependency cache and make migrate depend on it.
	newYAML := "processes:\n  keep:\n    cmd: \"sleep 60\"\n" +
		"dependencies:\n  cache:\n    check:\n      cmd: \"cachecheck\"\n      interval: 10ms\n      timeout: 2s\n" +
		"tasks:\n  migrate:\n    cmd: \"sleep 60\"\n    depends_on: [cache]\n"
	writeConfigFile(t, dir, newYAML)

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	require.NoError(t, sup.RestartProcess(rctx, "migrate"))

	// The rerun resolves the newly-added dependency (classified as a dep, not an
	// unknown task) and runs to completion.
	waitState(t, sup, "migrate", domain.ProcessStateRunning)
	assert.Greater(t, prober.probeCount("cachecheck"), 0, "the reload-added dependency was resolved")
	completeTask(t, runner, "migrate")
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
}

// Fix 5: applyReloadGraph REPLACES the classification view and prunes the
// resolver, so a name migrated dependency->task classifies as a task afterwards
// and the resolver forgets the stale definition.
func TestTask_Fix5_MigratedDependencyReclassifies(t *testing.T) {
	prober := newCoordProber()
	prober.set("svc", "healthy")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"svc"}},
		map[string]depSpec{"svc": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning)

	require.True(t, sup.classifyDependency("svc"), "svc starts as a dependency")
	_, ok := sup.resolver.Snapshot("svc")
	require.True(t, ok, "the resolver has an svc node")

	// Reload migrates svc from a dependency to a task.
	pending := &pendingConfig{
		freshDeps:  map[string]domain.DependencyConfig{},
		freshTasks: map[string]struct{}{"svc": {}},
	}
	sup.applyReloadGraph(pending)

	assert.False(t, sup.classifyDependency("svc"), "svc now classifies as a task")
	_, ok = sup.resolver.Snapshot("svc")
	assert.False(t, ok, "the resolver forgot the stale dependency definition")
}

// Fix 6: a task that exits rc=0 before its budget, but whose output drain is held
// open by a grandchild past the budget, must settle COMPLETED (not a run-timeout
// crash). The run-budget timer watches process EXIT, not the (drain-delayed)
// state commit.
func TestTask_Fix6_SlowDrainAfterExitCompletes(t *testing.T) {
	release := make(chan struct{})
	runner := newScriptedRunner()
	runner.stdout["migrate"] = &blockingReader{release: release}

	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})
	defer logMgr.Close()
	cfg := &config.Config{
		API:       config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{},
		Tasks:     map[string]config.TaskConfig{"migrate": {Cmd: "sleep 60", Timeout: "150ms"}},
	}
	sup := New(cfg, logMgr, runner, DefaultSupervisorConfig())
	sup.newResolver = func() *Resolver { return NewResolver(nil, "", nil, nil) }

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		stopSup(t, sup)
	}()

	waitState(t, sup, "migrate", domain.ProcessStateRunning)

	// Leader exits rc=0 well before the 150ms budget; the grandchild keeps stdout
	// open (drain blocked) past the budget.
	fp := runner.proc("migrate")
	fp.setAlive(false)
	fp.exitLeader(nil)

	// Past the budget while the drain is still blocked: must NOT be crashed.
	time.Sleep(300 * time.Millisecond)
	assert.NotEqual(t, domain.ProcessStateCrashed, mustState(t, sup, "migrate"),
		"a completed task with a slow drain must not be misclassified as a timeout crash")

	// Release the drain; the task settles completed.
	close(release)
	waitState(t, sup, "migrate", domain.ProcessStateCompleted)
}

// --- small helpers ----------------------------------------------------------

func mustState(t *testing.T, sup *Supervisor, name string) domain.ProcessState {
	t.Helper()
	info, err := sup.Process(name)
	require.NoError(t, err)
	return info.State
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}
