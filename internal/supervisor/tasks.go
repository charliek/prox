package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// This file is the TASK COORDINATOR (plan 013 D3 / C4). Tasks are
// run-to-completion ManagedProcess children (Kind=task) that execute ONCE per
// supervisor lifetime after their own depends_on targets are satisfied. It sits
// beside the dependency graph coordinator (coordinator.go): a process's
// demandTarget routes a TASK target here (demandTask), a DEPENDENCY target to the
// resolver.
//
// # Single-flight, once-per-lifetime
//
// The FIRST demand of a task creates a taskNode and launches ONE runTask
// goroutine; concurrent demanders join the node's done channel and receive the
// SAME terminal outcome. A settled node stays cached, so a dependent process's
// restart does NOT re-run a completed task (once per lifetime). A manual
// start/restart of the task replaces its node (rescheduleTask retires it)
// so it runs again.
//
// # Outcome mapping (what demandTarget sees)
//
//   - completed (natural exit 0)     -> ready  (DepStateHealthy)
//   - crashed  (non-zero/signal/     -> failed (DepStateFailed): dependents blocked
//               run-timeout kill/     naming this task
//               own-dep failed)
//   - stopped/stopping (user stop)   -> canceled: a torn-down run is not a failure
//   - shutdown / node reset          -> canceled
//
// # Shutdown
//
// runTask goroutines are tracked in s.coordWg and run under a coordCtx-derived
// context, so Stop -- which cancels coordCtx and closeTasks() before coordWg.Wait()
// -- unblocks a task waiting on its deps OR on its child's completion. The task's
// child (if launched) is a normal member of the managed set and is stopped by
// Supervisor.Stop's per-process loop after coordWg has quiesced.

// taskNode is the single-flight join point for one task-run episode. done is
// closed exactly once, by runTask, when the run reaches a terminal outcome;
// joiners select on it. A retired node (rescheduleTask/closeTasks) is dropped from the
// registry but its runTask still finishes and publishes to it, so any demander
// that already joined the retired node observes a (canceled) outcome -- mirroring
// the resolver's retired-node semantics.
type taskNode struct {
	gen    uint64
	cancel context.CancelFunc
	done   chan struct{}

	// pending/rerun carry a manual re-run's reloaded config and restart-bump intent
	// into the episode (plan 013 D3): executeTask applies them AFTER admission so
	// the fresh config governs the run and the restart count bumps exactly once for
	// the winning episode.
	pending *pendingConfig
	rerun   bool

	lock     sync.Mutex
	canceled bool
	settled  bool
	outcome  DepOutcome
}

// cancelNode marks the node retired and cancels its run context in one step
// (plan 013 fix 3), mirroring the resolver's depNode.cancelNode. Setting canceled
// BEFORE cancelling makes retirement and publication atomic: a finish that
// acquires the lock after this observes canceled and DEMOTES its verdict, so a
// joiner of a retired rerun never sees a stale healthy/failed result.
func (n *taskNode) cancelNode() {
	n.lock.Lock()
	n.canceled = true
	n.lock.Unlock()
	n.cancel()
}

// finish publishes the terminal outcome and closes done exactly once. If the node
// was retired (cancelNode) while runTask was computing, the verdict is DEMOTED to
// canceled so joiners re-demand the replacement (plan 013 fix 1/3) instead of
// launching dependents off a retired run's result.
func (n *taskNode) finish(o DepOutcome) {
	n.lock.Lock()
	if !n.settled {
		n.settled = true
		if n.canceled && o.State != DepStateCanceled {
			o = DepOutcome{State: DepStateCanceled, Err: context.Canceled}
		}
		n.outcome = o
	}
	n.lock.Unlock()
	close(n.done)
}

func (n *taskNode) outcomeSnapshot() DepOutcome {
	n.lock.Lock()
	defer n.lock.Unlock()
	return n.outcome
}

// demandTask starts (or joins) a task's run and returns its terminal outcome
// (plan 013 D3). Single-flight per task per supervisor lifetime: the first
// demander creates the node and launches runTask; concurrent demanders join and
// receive the same outcome; a demand after the run settled returns the cached
// outcome immediately (no re-run). ctx bounds only THIS caller's wait -- a caller
// whose ctx dies gets a canceled outcome while the shared run continues; to abort
// the run itself use rescheduleTask/closeTasks.
func (s *Supervisor) demandTask(ctx context.Context, name string) DepOutcome {
	s.taskMu.Lock()
	if s.taskClosed {
		s.taskMu.Unlock()
		return DepOutcome{State: DepStateCanceled, Err: context.Canceled}
	}
	mp := s.tasks[name]
	if mp == nil {
		s.taskMu.Unlock()
		// Unknown task: a programming error in the caller (only declared tasks are
		// demanded). Report a failure rather than blocking a dependent forever.
		return DepOutcome{State: DepStateFailed, Err: fmt.Errorf("unknown task %q", name)}
	}
	node := s.taskNodes[name]
	if node == nil {
		// A plain (dependent-driven) demand joins or starts the once-per-lifetime
		// run with no reload/rerun intent.
		node = s.startTaskEpisodeLocked(name, mp, nil, false)
		if node == nil {
			// beginWaiting refused admission: the task is unexpectedly active without a
			// tracked node. Unreachable on the normal path (an active task always has a
			// node). Report failed rather than canceled so the demander's re-demand loop
			// does not spin.
			s.taskMu.Unlock()
			return DepOutcome{State: DepStateFailed, Err: fmt.Errorf("task %q is busy", name)}
		}
	}
	done := node.done
	s.taskMu.Unlock()

	select {
	case <-done:
		return node.outcomeSnapshot()
	case <-ctx.Done():
		return DepOutcome{State: DepStateCanceled, Err: ctx.Err()}
	}
}

// startTaskEpisodeLocked creates a fresh node for the current generation and
// launches its runTask goroutine (plan 013 D3). Caller holds s.taskMu.
//
// The run's context is derived from coordCtx so shutdown cancels it; node.cancel
// additionally aborts just this run (rescheduleTask / a stop of a still-resolving
// task). The child process's LIFECYCLE context is s.ctx (long-lived), mirroring
// the gated launch path -- so the launched task is not tied to any request or to
// coordCtx's early shutdown cancel.
//
// coordWg.Add is safe here without racing coordWg.Wait because it is done under
// taskMu after the caller checked !taskClosed: closeTasks (which sets taskClosed)
// also runs under taskMu, and Stop runs closeTasks BEFORE coordWg.Wait, so any Add
// that observed the gate open completes its taskMu section before Wait can begin.
//
// Admission (beginWaiting) happens HERE, under taskMu, so the process's terminal->
// waiting transition and the node creation are ONE atomic step (plan 013 fix 2):
// a concurrent path can never both observe the process terminal and both admit a
// run. It returns nil (having transitioned nothing) when beginWaiting refuses --
// the process is already active. pending/rerun (a manual re-run) are carried onto
// the node for executeTask to apply after admission.
func (s *Supervisor) startTaskEpisodeLocked(name string, mp *ManagedProcess, pending *pendingConfig, rerun bool) *taskNode {
	base := s.coordCtx
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithCancel(base)
	gen, _, ok := mp.beginWaiting(cancel)
	if !ok {
		cancel()
		return nil
	}
	node := &taskNode{
		gen:     gen,
		cancel:  cancel,
		done:    make(chan struct{}),
		pending: pending,
		rerun:   rerun,
	}
	s.taskNodes[name] = node
	s.coordWg.Add(1)
	go s.runTask(mp, node, runCtx, s.ctx)
	return node
}

// runTask executes one task-run episode and publishes its terminal outcome.
func (s *Supervisor) runTask(mp *ManagedProcess, node *taskNode, runCtx, supCtx context.Context) {
	defer s.coordWg.Done()
	node.finish(s.executeTask(mp, node, runCtx, supCtx))
}

// executeTask resolves a task's own depends_on, launches it in task mode, and
// waits for it to reach a terminal state (plan 013 D3). See taskOutcome for the
// state->outcome mapping.
func (s *Supervisor) executeTask(mp *ManagedProcess, node *taskNode, runCtx, supCtx context.Context) DepOutcome {
	// Admission (beginWaiting) already happened atomically in startTaskEpisodeLocked
	// under taskMu; node.gen is the launch generation captured there.
	gen := node.gen

	// A manual re-run's reloaded config and restart-bump apply now, after admission
	// (the task is in waiting, no run in flight) and BEFORE the launch, so the fresh
	// cmd/env/timeout governs this run and the restart count bumps exactly once for
	// this (winning) episode (plan 013 D3).
	if node.pending != nil {
		mp.applyTaskReload(node.pending)
	}
	if node.rerun {
		mp.bumpRestart()
	}

	// 1. Resolve the task's OWN depends_on (dependencies and/or other tasks) FIRST,
	//    reusing the same demandTarget fan-out a gated process uses. A task target
	//    recurses into demandTask (single-flight); config validation guarantees
	//    acyclicity among tasks, so this terminates.
	targets := mp.Config().DependsOn
	if canceled, blockedBy := s.demandTargets(runCtx, targets, s.classifyDependency); canceled {
		// Shutdown / stop / reset in progress: leave the waiting state for the
		// stop path (Supervisor.Stop settles a still-waiting task to stopped).
		return DepOutcome{State: DepStateCanceled, Err: context.Canceled}
	} else if len(blockedBy) > 0 {
		if mp.markBlocked(blockedBy, gen) {
			s.SystemLog("task %s blocked: dependency not ready: %s", mp.Name(), strings.Join(blockedBy, ", "))
		}
		return DepOutcome{State: DepStateFailed, Err: fmt.Errorf("task %q blocked: %s", mp.Name(), strings.Join(blockedBy, ", "))}
	}

	// 2. Launch the task child in task mode under the generation guard.
	inst, err := mp.startTask(supCtx, gen)
	if err != nil {
		switch {
		case errors.Is(err, errLaunchSuperseded), errors.Is(err, domain.ErrShutdownInProgress),
			errors.Is(err, domain.ErrProcessAlreadyRunning):
			// Stopped/restarted/superseded out from under us, or the gate closed:
			// nothing launched, the stop path owns the state. Not a failure.
			return DepOutcome{State: DepStateCanceled, Err: err}
		default:
			// A genuine launch failure (e.g. a surviving previous group, or the
			// runner failed): settle crashed (retaining current for reap) exactly as
			// the gated path, and report a failure so dependents are blocked.
			mp.failWaitingLaunch(gen)
			mp.logf(domain.StreamStderr, "task failed to start: %v", err)
			return DepOutcome{State: DepStateFailed, Err: err}
		}
	}
	s.emit(SupervisorEvent{
		Type:      EventTypeProcessStarted,
		Process:   mp.Name(),
		Timestamp: time.Now(),
		Info:      mp.Info(),
	})

	if inst == nil {
		// Defensive: startTask reported success but no instance; read whatever state
		// was committed.
		return taskOutcome(mp.State())
	}

	// 3. Wait for the run to reach a terminal state. The run-budget timer watches
	//    inst.exited (process EXIT, closed by the monitor BEFORE output draining),
	//    not inst.done (state commit, which a slow grandchild drain can delay past
	//    the budget) -- so a naturally-completed rc=0 task is never misclassified as
	//    a timeout crash (plan 013 fix 6). The exit-vs-timeout race is decided by a
	//    single atomic claim on the instance: if the exit was claimed first, the
	//    timer is a no-op (completed wins, no timeout log); if the timer wins, it
	//    escalate-kills and the monitor defers its natural-exit commit to stopTask
	//    (crashed). Escalation (SIGTERM->SIGKILL under stop_timeout + KillGrace) is
	//    EXCLUDED from the run budget.
	timeout, hasTimeout := mp.taskBudget()
	var timerC <-chan time.Time
	if hasTimeout {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timerC = t.C
	}

	select {
	case <-inst.exited:
		// The process exited on its own; the monitor is committing completed/crashed
		// (or a concurrent stop is committing stopped). Wait for the commit, then map.
	case <-timerC:
		// Run budget elapsed. Claim the timeout; if the process already exited and
		// claimed first, this is a no-op and the natural verdict wins (no timeout log).
		if inst.claimTerminal(claimTimeout) {
			mp.logf(domain.StreamStderr, "task timed out after %s", timeout)
			stopCtx, cancel := context.WithTimeout(supCtx, mp.StopTimeout())
			_ = mp.stopTask(stopCtx)
			cancel()
		}
	case <-runCtx.Done():
		// Shutdown / targeted stop / reset while the child runs: report canceled and
		// leave the child to the stop path (Supervisor.Stop stops it after coordWg
		// quiesces).
		return DepOutcome{State: DepStateCanceled, Err: runCtx.Err()}
	}
	// Wait for the terminal state to be committed (monitor/Stop close done after
	// the commit) before mapping the outcome.
	<-inst.done
	return taskOutcome(mp.State())
}

// taskOutcome maps a task child's terminal state to the outcome a demander sees
// (plan 013 D3). Stopping is folded into canceled: it can only appear while a
// stop is tearing the run down, which is never a readiness failure.
func taskOutcome(state domain.ProcessState) DepOutcome {
	switch state {
	case domain.ProcessStateCompleted:
		return DepOutcome{State: DepStateHealthy}
	case domain.ProcessStateStopped, domain.ProcessStateStopping:
		return DepOutcome{State: DepStateCanceled, Err: context.Canceled}
	default:
		// crashed, blocked, or (defensively) anything else: the task did not
		// complete, so dependents are blocked naming it.
		return DepOutcome{State: DepStateFailed, Err: fmt.Errorf("task did not complete (final state: %s)", state)}
	}
}

// demandTaskAsync schedules a task demand on its own coordWg-tracked goroutine
// (plan 013 D3), used for the initial bare-up/subset demands and for a manual
// re-run. It refuses (returns false) once the supervisor is no longer running, so
// a demand racing shutdown never adds to coordWg after Stop began waiting: the
// Add happens under s.mu with a state check, and Stop flips state to "stopping"
// under s.mu before it coordWg.Wait()s.
func (s *Supervisor) demandTaskAsync(name string) bool {
	s.mu.Lock()
	if s.state != "running" {
		s.mu.Unlock()
		return false
	}
	ctx := s.coordCtx
	if ctx == nil {
		ctx = context.Background()
	}
	s.coordWg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.coordWg.Done()
		s.demandTask(ctx, name)
	}()
	return true
}

// rescheduleTask atomically replaces a task's node with a fresh run episode for a
// manual re-run (plan 013 fix 2/3). Under taskMu it retires the current node
// (cancelNode -- so any joiner of the retired node's done observes a demoted
// canceled outcome and re-demands the replacement) and launches a fresh episode
// carrying the reload. The caller (rerunTask) holds rerunMu and has already
// STOPPED any running child, so retiring the node here never orphans a child; a
// dependent that was waiting re-demands the new node via the demandTarget
// retirement loop. Returns an already-active error only if admission is somehow
// refused (unreachable on the normal path given rerunMu + the prior stop).
func (s *Supervisor) rescheduleTask(mp *ManagedProcess, pending *pendingConfig, rerun bool) error {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.taskClosed {
		return domain.ErrShutdownInProgress
	}
	name := mp.Name()
	if node := s.taskNodes[name]; node != nil {
		node.cancelNode()
		delete(s.taskNodes, name)
	}
	if node := s.startTaskEpisodeLocked(name, mp, pending, rerun); node == nil {
		// beginWaiting refused: the process is unexpectedly active. Defensive only.
		return domain.ErrProcessAlreadyRunning
	}
	return nil
}

// closeTasks refuses new task demands and cancels every in-flight run (plan 013
// D3), the task-coordinator analog of Resolver.Close. coordCtx's cancel already
// unblocks the runs (their contexts derive from it); this additionally flips the
// closed flag so a demand arriving during shutdown returns canceled without
// starting work. cancelNode retires each node so a joiner observes a demoted
// canceled outcome. Idempotent (RefuseLaunches and Stop both call it).
func (s *Supervisor) closeTasks() {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.taskClosed {
		return
	}
	s.taskClosed = true
	for _, node := range s.taskNodes {
		node.cancelNode()
	}
}

// rerunTask handles a manual StartProcess/RestartProcess of a task (plan 013 D3).
// A task runs once per lifetime, so a manual (re)run resets its once-flag and
// re-demands it; the run is scheduled asynchronously (the task runs to completion
// in the background) and the call returns promptly, mirroring a gated start.
//
// isRestart distinguishes the two entry points for a task that is currently
// RUNNING: a plain start of a running task is refused (already running), while a
// restart re-runs it fail-before-stop -- it reloads the task config and
// re-resolves ITS deps BEFORE stopping the current run, so a dependency that has
// gone unready (or a bad reload) leaves the running task untouched. For a task in
// a terminal state (completed/crashed/stopped) both entry points simply reload
// and re-run.
func (s *Supervisor) rerunTask(ctx context.Context, mp *ManagedProcess, supCtx context.Context, isRestart bool) error {
	// Serialize manual re-runs so two concurrent ones cannot both admit an episode
	// (plan 013 fix 2): the loser observes the fresh waiting/running state below and
	// returns a clean already-active error.
	s.rerunMu.Lock()
	defer s.rerunMu.Unlock()

	switch mp.State() {
	case domain.ProcessStateWaiting:
		return domain.ErrProcessAlreadyWaiting
	case domain.ProcessStateStarting, domain.ProcessStateStopping:
		return domain.ErrProcessAlreadyRunning
	case domain.ProcessStateRunning:
		if !isRestart {
			return domain.ErrProcessAlreadyRunning
		}
	}

	// Reload the task config (edited cmd/env/timeout applies on the re-run; a
	// removed task is the 409-analog ErrProcessNotInConfig). Nil when reload is
	// disabled (ConfigPath unset). Fail-before-stop: any error returns here with
	// the current run untouched.
	pending, err := s.prepareReloadTask(mp.Name())
	if err != nil {
		return err
	}

	if mp.State() == domain.ProcessStateRunning {
		// Restart of a RUNNING task: install the reload's classification view +
		// resolver defs and re-resolve its deps (D6/fix 4/5) BEFORE the stop; abort
		// untouched on any failure (fail-before-stop).
		targets, isDep := s.prepareRestartTargets(pending, mp.Config().DependsOn)
		if err := s.reresolveTargets(targets, isDep); err != nil {
			return err
		}
		// Deps satisfied: stop the current run like any child. A surviving group
		// aborts the re-run (the running task is not shadowed by a fresh one). This
		// STOP precedes rescheduleTask's node retirement, so retiring the node there
		// never orphans a live child.
		stopCtx, cancel := context.WithTimeout(ctx, mp.StopTimeout())
		err := mp.Stop(stopCtx)
		cancel()
		if err != nil && !errors.Is(err, domain.ErrProcessNotRunning) {
			return err
		}
	} else if pending != nil {
		// Terminal task: install the reload's classification view + resolver defs so
		// the fresh run resolves against the reloaded config (D6/fix 4/5 parity).
		s.applyReloadGraph(pending)
	}

	// Atomically replace the node with a fresh episode. The reloaded config swap and
	// restart-count bump happen inside the episode (carried on the node), so only
	// this winning re-run applies them.
	return s.rescheduleTask(mp, pending, true)
}
