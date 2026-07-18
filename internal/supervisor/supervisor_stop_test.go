package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nameRunner hands out a pre-built fakeProcess per process name, so a
// multi-process Supervisor.Stop test can assign distinct stop behaviour (clean
// vs unreapable) to specific names regardless of the concurrent, map-ordered
// Start sequence (fakeRunner's call-index factory cannot bind behaviour to a
// name). One start per name is assumed (these tests never restart).
type nameRunner struct {
	mu    sync.Mutex
	fakes map[string]*fakeProcess
}

func newNameRunner(fakes map[string]*fakeProcess) *nameRunner {
	return &nameRunner{fakes: fakes}
}

func (r *nameRunner) Start(_ context.Context, cfg domain.ProcessConfig, _ map[string]string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fp, ok := r.fakes[cfg.Name]
	if !ok {
		panic("nameRunner: no fake configured for process " + cfg.Name)
	}
	return fp, nil
}

// newStopSupervisor builds a supervisor over the given named fakes (each keyed by
// process name), all sharing stopTimeout. It does not start the supervisor.
func newStopSupervisor(t *testing.T, fakes map[string]*fakeProcess, stopTimeout string) *Supervisor {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})
	t.Cleanup(func() { logMgr.Close() })
	cfg := &config.Config{
		API:       config.APIConfig{Port: 5556, Host: "127.0.0.1"},
		Processes: make(map[string]config.ProcessConfig, len(fakes)),
	}
	for name := range fakes {
		cfg.Processes[name] = config.ProcessConfig{Cmd: "irrelevant", StopTimeout: stopTimeout}
	}
	return New(cfg, logMgr, newNameRunner(fakes), DefaultSupervisorConfig())
}

// collectStopEvents drains sub non-blocking and returns the stop/crash events
// per process name. Callers invoke it only after the operation under test has
// returned (Supervisor.Stop / StopProcess emit synchronously before returning),
// so the drain observes a settled set with no race.
func collectStopEvents(sub <-chan SupervisorEvent) map[string][]EventType {
	out := make(map[string][]EventType)
	for {
		select {
		case e := <-sub:
			if e.Type == EventTypeProcessStopped || e.Type == EventTypeProcessCrashed {
				out[e.Process] = append(out[e.Process], e.Type)
			}
		default:
			return out
		}
	}
}

// TestSupervisorStop_AggregatesSingleSurvivor: three processes (clean,
// unreapable, clean) -> Stop returns a non-nil *ProcessStopError carrying exactly
// the one survivor; errors.Is sees ErrProcessGroupNotReaped through the aggregate
// and errors.As extracts the typed value (#36, D3).
func TestSupervisorStop_AggregatesSingleSurvivor(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"alpha": newGracefulFake(1001),
		"beta":  newFastUnreapableFake(1002),
		"gamma": newGracefulFake(1003),
	}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	err = sup.Stop(context.Background())
	require.Error(t, err, "a surviving group must make Stop return an error")

	var agg *domain.ProcessStopError
	require.ErrorAs(t, err, &agg, "the error must be a *ProcessStopError")
	require.Len(t, agg.Failures, 1, "exactly the one unreapable process must be reported")
	assert.Equal(t, "beta", agg.Failures[0].Name)
	assert.ErrorIs(t, err, domain.ErrProcessGroupNotReaped, "errors.Is must see through the aggregate")
	assert.ErrorIs(t, agg.Failures[0].Err, domain.ErrProcessGroupNotReaped)
}

// TestSupervisorStop_FailuresSortedByName (ordering stability): several survivors
// with names supplied out of order are reported sorted by name in the aggregate.
func TestSupervisorStop_FailuresSortedByName(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"zebra": newFastUnreapableFake(2001),
		"mango": newGracefulFake(2002),
		"alpha": newFastUnreapableFake(2003),
		"delta": newFastUnreapableFake(2004),
	}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	err = sup.Stop(context.Background())
	require.Error(t, err)

	var agg *domain.ProcessStopError
	require.ErrorAs(t, err, &agg)

	names := make([]string, len(agg.Failures))
	for i, f := range agg.Failures {
		names[i] = f.Name
	}
	assert.Equal(t, []string{"alpha", "delta", "zebra"}, names, "failures must be sorted by name")
}

// TestSupervisorStop_CleanReturnsNil: a fully clean stop returns a literal nil
// error (not a typed-nil *ProcessStopError).
func TestSupervisorStop_CleanReturnsNil(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"alpha": newGracefulFake(3001),
		"beta":  newGracefulFake(3002),
	}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	require.NoError(t, sup.Stop(context.Background()), "a clean stop must return nil")
}

// TestSupervisorStop_EmptyReturnsNil: a running supervisor with no processes
// stops cleanly with nil.
func TestSupervisorStop_EmptyReturnsNil(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	require.NoError(t, sup.Stop(context.Background()), "an empty stop must return nil")
}

// TestSupervisorStop_NotRunningReturnsNil: Stop on a never-started (not running)
// supervisor is a no-op returning nil.
func TestSupervisorStop_NotRunningReturnsNil(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"alpha": newGracefulFake(4001),
	}, "3s")

	require.NoError(t, sup.Stop(context.Background()), "a not-running Stop must return nil")
}

// TestSupervisorStop_CleanEmitsStopped: full-stop of clean processes emits
// process_stopped (and no process_crashed) for each.
func TestSupervisorStop_CleanEmitsStopped(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"alpha": newGracefulFake(5001),
		"beta":  newGracefulFake(5002),
	}, "3s")

	sub := sup.Subscribe()
	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.NoError(t, sup.Stop(context.Background()))

	ev := collectStopEvents(sub)
	assert.Equal(t, []EventType{EventTypeProcessStopped}, ev["alpha"])
	assert.Equal(t, []EventType{EventTypeProcessStopped}, ev["beta"])
}

// TestSupervisorStop_UnreapableEmitsCrashedNotStopped: full-stop with a surviving
// group emits process_crashed for that process and NO process_stopped for it,
// while the clean sibling still emits process_stopped (#36, D3).
func TestSupervisorStop_UnreapableEmitsCrashedNotStopped(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"good": newGracefulFake(6001),
		"bad":  newFastUnreapableFake(6002),
	}, "3s")

	sub := sup.Subscribe()
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	err = sup.Stop(context.Background())
	require.Error(t, err)

	ev := collectStopEvents(sub)
	assert.Equal(t, []EventType{EventTypeProcessCrashed}, ev["bad"],
		"a surviving group must emit process_crashed and no process_stopped")
	assert.Equal(t, []EventType{EventTypeProcessStopped}, ev["good"])
}

// TestSupervisorStop_StopProcessUnreapableEmitsCrashed: StopProcess on a surviving
// group now emits process_crashed (previously it emitted nothing) -- uniform with
// the full-stop path (#36, D3).
func TestSupervisorStop_StopProcessUnreapableEmitsCrashed(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"svc": newFastUnreapableFake(7001),
	}, "5s")

	sub := sup.Subscribe()
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	err = sup.StopProcess(context.Background(), "svc")
	assert.ErrorIs(t, err, domain.ErrProcessGroupNotReaped)

	ev := collectStopEvents(sub)
	assert.Equal(t, []EventType{EventTypeProcessCrashed}, ev["svc"],
		"StopProcess on a surviving group must emit process_crashed")
}

// TestSupervisorStop_CtxExpiredSecondaryNoEventStillReported: a full Supervisor.Stop
// that joins an in-flight primary as a secondary and whose context is canceled
// before the primary's verdict lands returns ctx.Err() for that process -- it
// emits NO event (a ctx error is not proof of anything) but still records the
// process in the aggregate so the failure is surfaced (#36, D3).
func TestSupervisorStop_CtxExpiredSecondaryNoEventStillReported(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"svc": newFastUnreapableFake(8001),
	}, "5s")

	sub := sup.Subscribe()
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp := getManagedProcess(t, sup, "svc")
	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	// In-flight primary StopProcess: parks with its episode installed (state
	// Stopping), before any signalling/verdict.
	primaryCh := make(chan error, 1)
	go func() { primaryCh <- sup.StopProcess(context.Background(), "svc") }()
	gate.awaitPrimary(t)

	// Supervisor.Stop joins as the secondary; cancel its context before the
	// primary is released so the secondary observes ctx.Done, not the verdict.
	stopCtx, stopCancel := context.WithCancel(context.Background())
	stopCh := make(chan error, 1)
	go func() { stopCh <- sup.Stop(stopCtx) }()
	gate.awaitJoins(t, 1)
	stopCancel()

	serr := recvErr(t, stopCh, "Supervisor.Stop")
	require.Error(t, serr, "the canceled secondary must surface as a failure")
	var agg *domain.ProcessStopError
	require.ErrorAs(t, serr, &agg)
	require.Len(t, agg.Failures, 1)
	assert.Equal(t, "svc", agg.Failures[0].Name)
	assert.ErrorIs(t, serr, context.Canceled, "the recorded error is the ctx cancellation")
	assert.NotErrorIs(t, serr, domain.ErrProcessGroupNotReaped,
		"a ctx cancellation is not a reap-failure verdict")

	// No event may be emitted for the ctx-expired secondary (drain while the
	// primary is still parked, so any emit would be its own, not present yet).
	ev := collectStopEvents(sub)
	assert.Empty(t, ev["svc"], "a ctx-expired secondary must emit no event")

	// Release the parked primary so its goroutine finishes and does not leak.
	close(gate.releasePrimary)
	assert.ErrorIs(t, recvErr(t, primaryCh, "primary StopProcess"), domain.ErrProcessGroupNotReaped)
}

// TestSupervisorStop_SecondaryOutlivesFinalizationWindow (the load-bearing overlap
// test): an in-flight primary StopProcess on a surviving group is parked at the
// "finalizing" seam -- INSIDE the finalization window, after the monitor drain and
// before the authoritative verdict -- exactly the tail stopVerdictMargin exists to
// cover. A full Supervisor.Stop joins as the secondary (its per-process context is
// StopTimeout + stopVerdictMargin) and must wait through that parked tail to
// observe and aggregate the true PROCESS_GROUP_NOT_REAPED.
//
// The per-process budget is overridden to a tiny 50ms, so the OLD sizing
// (StopTimeout alone) would have given the secondary a 50ms context that expires
// long before the parked verdict lands -- proving the margin is load-bearing and
// that the test exercises the finalization tail, not merely the pre-signalling
// window (#32/#36, D3).
func TestSupervisorStop_SecondaryOutlivesFinalizationWindow(t *testing.T) {
	// Config carries a valid budget (stop_timeout must exceed the reserved SIGKILL
	// grace); after Start we override the live per-process budget to 50ms directly
	// (the shutdownTimeout seam other stop tests use).
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"svc": newFastUnreapableFake(9001),
	}, "3s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp := getManagedProcess(t, sup, "svc")
	mp.mu.Lock()
	mp.shutdownTimeout = 50 * time.Millisecond
	mp.mu.Unlock()

	// Custom barrier: DO NOT park at "primary-installed" (let the primary flow
	// through signalling to the finalization gate); record the secondary's join;
	// park exactly once at "finalizing" so the primary's verdict lands late.
	var installOnce, finalizeOnce sync.Once
	installed := make(chan struct{})
	finalizeReached := make(chan struct{})
	releaseFinalize := make(chan struct{})
	joined := make(chan struct{}, 4)
	mp.stopBarrier = func(phase string) {
		switch phase {
		case "primary-installed":
			installOnce.Do(func() { close(installed) })
		case "secondary-joined":
			joined <- struct{}{}
		case "finalizing":
			parked := false
			finalizeOnce.Do(func() { parked = true; close(finalizeReached) })
			if parked {
				<-releaseFinalize
			}
		}
	}

	// Primary StopProcess installs the episode (state Stopping) and proceeds
	// toward the finalization gate.
	primaryCh := make(chan error, 1)
	go func() { primaryCh <- sup.StopProcess(context.Background(), "svc") }()
	<-installed

	// Supervisor.Stop joins as the secondary now that state is Stopping.
	stopCh := make(chan error, 1)
	go func() { stopCh <- sup.Stop(context.Background()) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("secondary did not join the episode within timeout")
	}

	// The primary reaches the finalization gate and parks there with its verdict
	// pending; the secondary is waiting on the episode.
	select {
	case <-finalizeReached:
	case <-time.After(5 * time.Second):
		t.Fatal("primary did not reach the finalization gate within timeout")
	}

	// Hold the parked verdict well past the 50ms old-sizing budget. Under the old
	// StopTimeout-only sizing the secondary's 50ms context is already done here;
	// under the new sizing (50ms + stopVerdictMargin ~= 7s) it is still waiting.
	time.Sleep(200 * time.Millisecond)
	close(releaseFinalize)

	assert.ErrorIs(t, recvErr(t, primaryCh, "primary StopProcess"), domain.ErrProcessGroupNotReaped)

	serr := recvErr(t, stopCh, "Supervisor.Stop")
	require.Error(t, serr, "the secondary must aggregate the surviving group's verdict")
	var agg *domain.ProcessStopError
	require.ErrorAs(t, serr, &agg)
	require.Len(t, agg.Failures, 1)
	assert.Equal(t, "svc", agg.Failures[0].Name)
	assert.ErrorIs(t, serr, domain.ErrProcessGroupNotReaped,
		"the secondary outlived the finalization tail and carried the true verdict")
}

// TestSupervisorStop_StatusReadableDuringDrain pins the property the C4 stage
// reorder exists for -- the daemon can answer read-only status while a stop is
// draining -- at the supervisor level (the full HTTP-level check lands in C5).
// A process's stop is parked mid-drain (state Stopping, s.mu released, p.mu not
// held at the "primary-installed" seam); Processes() and Status() must still
// answer promptly rather than block behind the parked stop. Deterministic via
// the stopBarrier seam (no sleeps).
func TestSupervisorStop_StatusReadableDuringDrain(t *testing.T) {
	sup := newStopSupervisor(t, map[string]*fakeProcess{
		"web": newGracefulFake(11001),
	}, "5s")

	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	mp := getManagedProcess(t, sup, "web")
	gate := newStopGate()
	mp.stopBarrier = gate.barrier

	stopCh := make(chan error, 1)
	go func() { stopCh <- sup.Stop(context.Background()) }()
	gate.awaitPrimary(t) // parked mid-stop: state Stopping, drain in progress

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		procs := sup.Processes()
		assert.Len(t, procs, 1, "Processes must still answer during the drain")
		st := sup.Status()
		assert.NotEmpty(t, st.State, "Status must still answer during the drain")
	}()
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Processes/Status blocked while a stop was parked mid-drain")
	}

	close(gate.releasePrimary)
	require.NoError(t, recvErr(t, stopCh, "Supervisor.Stop"), "the parked clean stop must finish nil")
}
