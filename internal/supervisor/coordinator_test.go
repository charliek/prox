package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- controllable prober for coordinator tests ------------------------------
//
// coordProber drives each dependency target's readiness by mode, keyed on the
// check Target. "healthy" passes on the first probe (so the start: command never
// runs and no polling timers are needed); "fail" never passes (the resolver
// exhausts its short budget and terminates failed/warned); "block" blocks until
// the test releases the target (or the probe's ctx is canceled), modelling a
// slow dependency. Every probe is counted per target so a test can assert an
// out-of-scope dependency was never probed.
type coordProber struct {
	mu       sync.Mutex
	modes    map[string]string
	released map[string]chan struct{}
	probed   map[string]int
}

func newCoordProber() *coordProber {
	return &coordProber{
		modes:    map[string]string{},
		released: map[string]chan struct{}{},
		probed:   map[string]int{},
	}
}

func (p *coordProber) Probe(ctx context.Context, check domain.DependencyCheck) error {
	p.mu.Lock()
	p.probed[check.Target]++
	mode := p.modes[check.Target]
	rel := p.released[check.Target]
	p.mu.Unlock()

	switch mode {
	case "healthy":
		return nil
	case "block":
		select {
		case <-rel:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		return errNotReady
	}
}

func (p *coordProber) set(target, mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modes[target] = mode
	if _, ok := p.released[target]; !ok {
		p.released[target] = make(chan struct{})
	}
}

// release flips a "block" target to healthy by unblocking its pending probe.
func (p *coordProber) release(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.released[target]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

func (p *coordProber) probeCount(target string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probed[target]
}

// --- recording start runner (down-never-invokes-start) ----------------------

type recordingStartRunner struct{ calls int32 }

func (r *recordingStartRunner) Run(ctx context.Context, name, cmd string) error {
	atomic.AddInt32(&r.calls, 1)
	return nil
}

func (r *recordingStartRunner) count() int { return int(atomic.LoadInt32(&r.calls)) }

// --- test supervisor builder ------------------------------------------------

type depSpec struct {
	onFailure domain.FailurePolicy
	timeout   time.Duration
	interval  time.Duration
	start     string
}

// gatedSupervisor builds a supervisor whose processes carry the given depends_on
// lists and whose dependency resolver is driven by prober (with optional start
// runner). The domain dependency configs come from deps; config.Dependencies is
// populated too so demandTarget classifies the targets as dependencies. The
// runner hands out graceful fakes so Stop reaps cleanly.
func gatedSupervisor(t *testing.T, procDeps map[string][]string, deps map[string]depSpec, prober Prober, startRunner StartRunner) (*Supervisor, *fakeRunner, *logs.Manager) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})

	cfg := &config.Config{
		API:          config.APIConfig{Port: 5555, Host: "127.0.0.1"},
		Processes:    map[string]config.ProcessConfig{},
		Dependencies: map[string]config.DependencyConfig{},
		Tasks:        map[string]config.TaskConfig{},
	}
	for name, on := range procDeps {
		cfg.Processes[name] = config.ProcessConfig{Cmd: "sleep 60", DependsOn: on}
	}
	for name := range deps {
		cfg.Dependencies[name] = config.DependencyConfig{}
	}

	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(1000 + call) })
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
		// A large per-attempt cap lets a "block" probe stay blocked until the test
		// releases it (or the resolution is canceled), instead of being cycled by
		// the default 2s cap; a "fail" dep's own short check timeout still bounds it.
		opts := []ResolverOption{WithProber(prober), WithClock(realClock{}), WithAttemptCap(60 * time.Second)}
		if startRunner != nil {
			opts = append(opts, WithStartRunner(startRunner))
		}
		return NewResolver(depMap, "", nil, nil, opts...)
	}

	return sup, runner, logMgr
}

func waitState(t *testing.T, sup *Supervisor, name string, want domain.ProcessState) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		info, err := sup.Process(name)
		return err == nil && info.State == want
	}, 3*time.Second, 5*time.Millisecond, "process %q never reached state %q", name, want)
}

func stopSup(t *testing.T, sup *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = sup.Stop(ctx)
}

// --- tests ------------------------------------------------------------------

// A gated process waits until its (blocking) dependency becomes healthy, then
// launches. Until release it is in the waiting state and no process was created.
func TestCoordinator_GatedWaitsThenLaunchesHealthy(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	// Still gated: waiting, nothing launched.
	waitState(t, sup, "web", domain.ProcessStateWaiting)
	assert.Equal(t, 0, runner.count(), "no process should launch while the dependency is unresolved")
	info, _ := sup.Process("web")
	assert.Equal(t, 0, info.PID)

	prober.release("db")

	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.count())
}

// A required target that never becomes ready (on_failure=fail) leaves the
// process blocked, with the blocking targets recorded in declaration order.
func TestCoordinator_BlockedRecordsFailedTargetsInOrder(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "fail")
	prober.set("cache", "fail")
	prober.set("queue", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		// declaration order is queue, db, cache; only db and cache fail.
		map[string][]string{"web": {"queue", "db", "cache"}},
		map[string]depSpec{
			"queue": {},
			"db":    {timeout: 40 * time.Millisecond},
			"cache": {timeout: 40 * time.Millisecond},
		},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateBlocked)
	assert.Equal(t, 0, runner.count(), "a blocked process never launches")

	mp := sup.processes["web"]
	assert.Equal(t, []string{"db", "cache"}, mp.BlockedBy(), "blocking targets recorded in declaration order")
}

// A warned target (budget exhausted, on_failure=warn) counts as satisfied, so
// its dependents proceed to launch.
func TestCoordinator_WarnedTargetLaunches(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "fail")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {onFailure: domain.FailurePolicyWarn, timeout: 40 * time.Millisecond}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.count())
}

// Ungated processes are launched synchronously by Start exactly as before, even
// when a resolver is present: they never enter the waiting state.
func TestCoordinator_UngatedLaunchesImmediately(t *testing.T) {
	prober := newCoordProber()
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": nil},
		map[string]depSpec{},
		prober, nil)
	defer logMgr.Close()

	result, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	// Start already launched it (synchronous ungated path).
	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateRunning, info.State)
	assert.Equal(t, 1, runner.count())
	assert.Contains(t, result.Started, "web")
}

// Start returns promptly while a slow dependency is still resolving: the process
// is still waiting at the moment Start returns, proving Start did not block on
// resolution.
func TestCoordinator_StartReturnsPromptlyWhileResolving(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober, nil)
	defer logMgr.Close()

	start := time.Now()
	_, err := sup.Start(context.Background())
	elapsed := time.Since(start)
	require.NoError(t, err)
	defer stopSup(t, sup)

	assert.Less(t, elapsed, 500*time.Millisecond, "Start must not wait on dependency resolution")
	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateWaiting, info.State, "process still waiting when Start returned")
	assert.Equal(t, 0, runner.count())

	prober.release("db")
	waitState(t, sup, "web", domain.ProcessStateRunning)
}

// Shutdown mid-wait cancels orchestration, quiesces the coordinator before Stop
// returns, and leaves the process stopped (never blocked/crashed).
func TestCoordinator_ShutdownMidWaitEndsStopped(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	waitState(t, sup, "web", domain.ProcessStateWaiting)

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sup.Stop(stopCtx))

	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateStopped, info.State, "a shutdown-canceled wait ends stopped, not blocked")
	assert.Equal(t, 0, runner.count(), "the process was never launched")
}

// StopProcess of a waiting process, raced against the dependency becoming
// healthy: the process must end in a terminal state, never launched-and-left-
// running, and the stop must not hang. Race-gated.
func TestCoordinator_StopWaitingRace(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		prober := newCoordProber()
		prober.set("db", "block")
		sup, runner, logMgr := gatedSupervisor(t,
			map[string][]string{"web": {"db"}},
			map[string]depSpec{"db": {timeout: 30 * time.Second}},
			prober, nil)

		_, err := sup.Start(context.Background())
		require.NoError(t, err)
		waitState(t, sup, "web", domain.ProcessStateWaiting)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = sup.StopProcess(ctx, "web")
		}()
		go func() {
			defer wg.Done()
			prober.release("db")
		}()
		wg.Wait()

		// Whichever won, the process must settle terminally and the runner must
		// have produced at most one instance. Then a full stop must reap it.
		stopSup(t, sup)
		info, _ := sup.Process("web")
		assert.Truef(t, info.State.IsStopped(), "iter %d: expected terminal state, got %s", iter, info.State)
		assert.LessOrEqualf(t, runner.count(), 1, "iter %d: at most one instance", iter)
		if fp := runner.last(); fp != nil {
			alive, _ := fp.GroupAlive()
			assert.Falsef(t, alive, "iter %d: launched instance must be stopped", iter)
		}
		logMgr.Close()
	}
}

// Restart of a running gated process re-resolves its targets BEFORE stopping the
// running instance: a failed re-resolution leaves the running instance
// untouched; a successful one restarts.
func TestCoordinator_RestartFailBeforeStop(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 40 * time.Millisecond}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateRunning)
	firstPID := runner.last().PID()

	// Failed re-resolution: invalidate the cached healthy outcome (simulating the
	// dependency dropping) and make it re-resolve to failed. Restart must abort
	// with the running instance untouched.
	prober.set("db", "fail")
	sup.resolver.Reset("db")
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	err = sup.RestartProcess(rctx, "web")
	require.Error(t, err, "restart must fail when a target re-resolves unready")
	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateRunning, info.State, "running instance untouched on failed re-resolution")
	assert.Equal(t, firstPID, info.PID)
	assert.Equal(t, 1, runner.count(), "no new instance launched")

	// Successful re-resolution: the dependency is healthy again. reresolveTargets
	// itself Resets the now-failed "db" node (exercising the reset-only-failed
	// branch) and re-resolves it healthy, so restart swaps in a fresh instance.
	prober.set("db", "healthy")
	require.NoError(t, sup.RestartProcess(rctx, "web"))
	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 2, runner.count(), "a new instance launched on successful restart")
}

// A subset start (StartProcesses) resolves only the started process's dependency
// closure; a dependency belonging solely to an unstarted process is never
// probed.
func TestCoordinator_SubsetStartResolvesOnlyClosure(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	prober.set("cache", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{
			"web":    {"db"},
			"worker": {"cache"},
		},
		map[string]depSpec{"db": {}, "cache": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.StartProcesses(context.Background(), []string{"web"})
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.count())

	// worker was not in the subset, so its dependency must never have been probed.
	assert.Greater(t, prober.probeCount("db"), 0, "the started process's dependency was probed")
	assert.Equal(t, 0, prober.probeCount("cache"), "an unstarted process's dependency must not be probed")
	_, err = sup.Process("worker")
	assert.ErrorIs(t, err, domain.ErrProcessNotFound, "unstarted process is not registered")
}

// Down (Supervisor.Stop) never invokes a dependency's start: command. With a
// healthy dependency the resolver never runs start anyway; the up->down cycle
// leaves the recorder at zero, proving the shutdown path does not touch deps.
func TestCoordinator_DownNeverInvokesDependencyStart(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	rec := &recordingStartRunner{}
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {start: "docker compose up -d"}},
		prober, rec)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	waitState(t, sup, "web", domain.ProcessStateRunning)

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, sup.Stop(stopCtx))

	assert.Equal(t, 0, rec.count(), "down must never invoke a dependency start command")
}

// A stale coordinator completion (an earlier launch generation) must never
// launch after a stop/restart/re-demand superseded it: startGated refuses the
// launch and mutates no state.
func TestCoordinator_StaleGenerationDoesNotLaunch(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	defer logMgr.Close()
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(2000 + call) })
	mp := NewManagedProcess(domain.ProcessConfig{Name: "web", Cmd: "sleep 60", DependsOn: []string{"db"}}, nil, runner, logMgr)

	// Begin an orchestration episode, capturing its generation, then supersede it
	// (as a stop/restart/re-demand would) by bumping the launch generation.
	staleGen, _, ok := mp.beginWaiting(func() {})
	require.True(t, ok, "a stopped process is admissible")
	mp.waitGen.Add(1) // supersede the captured episode

	err := mp.startGated(context.Background(), staleGen)
	require.ErrorIs(t, err, errLaunchSuperseded)
	assert.Equal(t, 0, runner.count(), "a superseded generation must not launch")
	assert.Equal(t, domain.ProcessStateWaiting, mp.State(), "state unchanged by the refused launch")
}

// Manual StartProcess on a waiting gated process is a no-op error
// (ErrProcessAlreadyWaiting); the pending wait is undisturbed and the process
// still launches when its dependency becomes healthy.
func TestCoordinator_ManualStartOnWaitingIsNoop(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "block")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 30 * time.Second}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateWaiting)
	err = sup.StartProcess(context.Background(), "web")
	assert.ErrorIs(t, err, domain.ErrProcessAlreadyWaiting, "starting a waiting process is a no-op")

	prober.release("db")
	waitState(t, sup, "web", domain.ProcessStateRunning)
}

// Manual StartProcess on a blocked gated process re-demands its targets (Resets
// the failed ones) and launches once they are satisfied.
func TestCoordinator_ManualStartOnBlockedReDemands(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "fail")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {timeout: 40 * time.Millisecond}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateBlocked)
	assert.Equal(t, []string{"db"}, sup.processes["web"].BlockedBy())

	prober.set("db", "healthy")
	require.NoError(t, sup.StartProcess(context.Background(), "web"))
	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.Equal(t, 1, runner.count())
}

// Finding 1: a manual start of a RUNNING gated process must be refused, never
// flip the live process to waiting. (Admission's conditional beginWaiting is only
// admissible from a non-active state.)
func TestCoordinator_StartOnRunningGatedRefused(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning)

	err = sup.StartProcess(context.Background(), "web")
	assert.ErrorIs(t, err, domain.ErrProcessAlreadyRunning)
	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateRunning, info.State, "a running process must not be flipped to waiting")
	assert.Equal(t, 1, runner.count(), "no second instance")
}

// Finding 1 (race): concurrent manual starts of a stopped gated process admit
// EXACTLY ONE episode -- never two orchestrate goroutines, never a duplicate
// launch. Race-gated.
func TestCoordinator_ConcurrentStartsAdmitOneEpisode(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning) // initial launch (count 1)

	require.NoError(t, sup.StopProcess(context.Background(), "web"))
	waitState(t, sup, "web", domain.ProcessStateStopped)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = sup.StartProcess(context.Background(), "web")
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, e := range errs {
		if e == nil {
			admitted++
		} else {
			assert.True(t, isAlreadyActive(e), "refusals must be already-running/waiting, got %v", e)
		}
	}
	assert.Equal(t, 1, admitted, "exactly one concurrent start admits an episode")

	waitState(t, sup, "web", domain.ProcessStateRunning)
	assert.LessOrEqual(t, runner.count(), 2, "at most the initial + one re-launch, never a duplicate")
}

func isAlreadyActive(err error) bool {
	return errors.Is(err, domain.ErrProcessAlreadyRunning) || errors.Is(err, domain.ErrProcessAlreadyWaiting)
}

// Finding 2: a stale ManagedProcess pointer (superseded by a stop->start cycle)
// is refused by admission rather than orchestrating an untracked duplicate absent
// from s.processes.
func TestCoordinator_StalePointerRefused(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	waitState(t, sup, "web", domain.ProcessStateRunning)
	staleMp := sup.processes["web"]

	stopSup(t, sup)
	_, err = sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning)
	require.NotSame(t, staleMp, sup.processes["web"], "a fresh run rebuilt the managed process")

	err = sup.scheduleGated(staleMp)
	assert.ErrorIs(t, err, domain.ErrProcessNotFound, "a stale pointer must not be orchestrated")
}

// Finding 3: a gated launch refused by a surviving previous group must settle the
// process in crashed (retaining the live group for reaping), NOT strand it in
// waiting where Stop's no-instance shortcut would skip the orphaned group.
func TestCoordinator_LaunchRefusedBySurvivingGroupGoesCrashed(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, runner, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	waitState(t, sup, "web", domain.ProcessStateRunning)

	// Simulate an unexpected leader exit whose GROUP survives (a stubborn
	// grandchild): the process crashes but its group stays alive.
	fp := runner.last()
	fp.exitLeader(nil) // leader gone; alive flag stays true
	waitState(t, sup, "web", domain.ProcessStateCrashed)

	// A manual start now schedules an episode; the launch is refused by the
	// surviving group. It must settle crashed, not remain waiting.
	require.NoError(t, sup.StartProcess(context.Background(), "web"))
	require.Eventuallyf(t, func() bool {
		info, _ := sup.Process("web")
		return info.State == domain.ProcessStateCrashed
	}, 3*time.Second, 5*time.Millisecond, "refused gated launch must settle crashed, not stay waiting")

	// Stop must walk the real group path (send SIGTERM), proving the group is not
	// skipped by a waiting shortcut. The graceful fake dies on SIGTERM.
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, sup.Stop(stopCtx))
	assert.True(t, fp.sawSignal(sigterm), "Stop must signal the surviving group, not skip it")
}

// Finding 4: ResetIfGeneration is a no-op once the node has moved to a newer
// generation, so a stale reset can never cancel a fresh resolution another
// demander just started.
func TestResolver_ResetIfGenerationGuardsStaleReset(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	depMap := map[string]domain.DependencyConfig{
		"db": {Name: "db", Check: domain.DependencyCheck{Kind: domain.CheckKindTCP, Target: "db", Timeout: time.Second, Interval: 10 * time.Millisecond}, OnFailure: domain.FailurePolicyFail},
	}
	r := NewResolver(depMap, "", nil, nil, WithProber(prober), WithClock(realClock{}))
	defer r.Close()

	require.True(t, r.Demand(context.Background(), "db").Ready())
	snap, ok := r.Snapshot("db")
	require.True(t, ok)
	gen0 := snap.Gen

	// A concurrent path resets and re-resolves under a new generation.
	r.Reset("db")
	require.True(t, r.Demand(context.Background(), "db").Ready())
	snap, _ = r.Snapshot("db")
	require.Greater(t, snap.Gen, gen0, "a fresh generation is in place")

	// A stale reset carrying gen0 must be refused; the current generation survives.
	assert.False(t, r.ResetIfGeneration("db", gen0), "stale-generation reset is a no-op")
	after, ok := r.Snapshot("db")
	require.True(t, ok)
	assert.Equal(t, snap.Gen, after.Gen, "the newer generation was not cancelled")

	// A reset carrying the current generation does act.
	assert.True(t, r.ResetIfGeneration("db", after.Gen))
}

// Finding 4 (coordinator level): a blocked process's re-demand Resets its failed
// target generation-conditionally, so it cannot strand another process that has
// since re-resolved the same shared target.
func TestCoordinator_ReDemandDoesNotStrandSharedTarget(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "fail")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}, "worker": {"db"}},
		map[string]depSpec{"db": {timeout: 40 * time.Millisecond}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)

	waitState(t, sup, "web", domain.ProcessStateBlocked)
	waitState(t, sup, "worker", domain.ProcessStateBlocked)
	snap, ok := sup.resolver.Snapshot("db")
	require.True(t, ok)
	staleGen := snap.Gen

	// worker re-demands with a now-healthy dependency -> a new generation resolves.
	prober.set("db", "healthy")
	require.NoError(t, sup.StartProcess(context.Background(), "worker"))
	waitState(t, sup, "worker", domain.ProcessStateRunning)

	// A stale reset of the shared target at the old (failed) generation must be a
	// no-op, leaving worker's fresh healthy resolution intact.
	assert.False(t, sup.resolver.ResetIfGeneration("db", staleGen))
	info, _ := sup.Process("worker")
	assert.Equal(t, domain.ProcessStateRunning, info.State, "worker must not be stranded by a stale reset")
}

// Finding 5: once RefuseLaunches has closed the gate, gated admission refuses a
// fresh start with ErrShutdownInProgress and leaves the process state unchanged
// (never parked in waiting with a resolver that is already closed).
func TestCoordinator_RefuseLaunchesBlocksGatedAdmission(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning)
	require.NoError(t, sup.StopProcess(context.Background(), "web"))
	waitState(t, sup, "web", domain.ProcessStateStopped)

	sup.RefuseLaunches()

	err = sup.StartProcess(context.Background(), "web")
	assert.ErrorIs(t, err, domain.ErrShutdownInProgress)
	info, _ := sup.Process("web")
	assert.Equal(t, domain.ProcessStateStopped, info.State, "a refused gated start leaves state unchanged")
}

// Finding 6: a successful restart of a RUNNING gated process emits a
// process_started event, matching the ungated restart path.
func TestCoordinator_RunningGatedRestartEmitsStarted(t *testing.T) {
	prober := newCoordProber()
	prober.set("db", "healthy")
	sup, _, logMgr := gatedSupervisor(t,
		map[string][]string{"web": {"db"}},
		map[string]depSpec{"db": {}},
		prober, nil)
	defer logMgr.Close()

	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	defer stopSup(t, sup)
	waitState(t, sup, "web", domain.ProcessStateRunning)

	// Subscribe AFTER the initial launch so we observe only the restart's event.
	events := sup.Subscribe()
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	require.NoError(t, sup.RestartProcess(rctx, "web"))

	require.Eventuallyf(t, func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Type == EventTypeProcessStarted && ev.Process == "web" {
					return true
				}
			default:
				return false
			}
		}
	}, 3*time.Second, 5*time.Millisecond, "running gated restart must emit process_started")
}
