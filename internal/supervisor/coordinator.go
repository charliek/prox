package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
)

// This file is the dependency GRAPH COORDINATOR (plan 013 C3 / D4). It sits on
// top of the resolver engine (deps.go) and gates PROCESS launches on their
// depends_on targets. Task execution (tasks-as-children) is C4; here a task
// target is a clearly-marked seam (see demandTarget).
//
// # Model
//
// A process with a non-empty depends_on is "gated". Supervisor.Start does NOT
// launch it directly; it registers it in the waiting state and hands it to this
// coordinator, which runs ONE background goroutine per gated process (orchestrate).
// That goroutine Demands each dependency target, and then:
//
//   - all targets ready (healthy or warned -- warned counts as satisfied) ->
//     launch the process via the normal start path, under a generation guard;
//   - a required target failed (on_failure=fail; canceled does NOT count as a
//     failure) -> mark the process blocked, recording the blocking targets in
//     declaration order;
//   - the wait was canceled (shutdown or a per-process stop) -> return without
//     launching or blocking; the stopping path settles the terminal state.
//
// Start returns promptly -- it never waits on resolution -- so the API is
// serving while dependencies resolve (constraint: Start must not block).
//
// # Shutdown
//
// RefuseLaunches / Stop cancel coordCtx and Close the resolver; Stop then waits
// on coordWg so every orchestrate goroutine has quiesced before the process set
// is stopped. A gated process whose wait is canceled by shutdown ends stopped
// (via its per-process Stop), never blocked/crashed.

// errLaunchSuperseded reports that a gated launch was refused at the final gate
// because a stop/restart/re-demand bumped the process's launch generation after
// the coordinator captured it. It is an internal control signal, never surfaced.
var errLaunchSuperseded = errors.New("gated launch superseded")

// buildDefaultResolver builds the production dependency resolver from the
// supervisor's config (plan 013 D4). It converts each configured dependency to
// its domain form and wires the real check/start seams. An individual
// dependency whose config fails to convert (already rejected by config
// validation at load, so unreachable on the normal path) is logged and skipped
// rather than failing the whole run. It is the default s.newResolver; tests
// replace that seam to inject scripted seams.
func (s *Supervisor) buildDefaultResolver() *Resolver {
	deps := make(map[string]domain.DependencyConfig, len(s.config.Dependencies))
	for name, dc := range s.config.Dependencies {
		dd, err := dc.ToDomain(name)
		if err != nil {
			s.systemErrorf("dependency %q: invalid config, skipped: %v", name, err)
			continue
		}
		deps[name] = dd
	}
	// Dependency start:/cmd: seams run under the same environment overlay a
	// process gets from the top-level env_file (see NewResolver). A load failure
	// degrades to the bare os environment rather than failing startup.
	var overlay map[string]string
	if env, err := config.LoadProcessEnv(s.config.EnvFile, "", nil, s.supConfig.ConfigDir); err != nil {
		s.systemErrorf("dependency resolver: could not load global env overlay: %v", err)
	} else {
		overlay = env
	}
	return NewResolver(deps, s.supConfig.ConfigDir, overlay, s.SystemLog)
}

// admitGated ATOMICALLY admits a gated process into a fresh orchestration
// episode (plan 013 D4, findings 1/2/5). Under s.mu it:
//
//   - refuses (ErrShutdownInProgress) when the supervisor is not accepting
//     launches -- state != running OR the launch gate is closed (RefuseLaunches /
//     Stop) -- so a start racing shutdown never parks a process in waiting that
//     nothing will ever resolve (finding 5);
//   - refuses (ErrProcessNotFound) when mp is no longer the tracked pointer for
//     its name, so a stale request cannot orchestrate an untracked duplicate
//     absent from s.processes (finding 2);
//   - conditionally transitions the process to waiting via beginWaiting, refusing
//     already-active/already-waiting states with the matching error (finding 1);
//   - registers the episode with coordWg INSIDE this s.mu critical section, so
//     Stop -- which flips state under s.mu before it coordWg.Wait()s -- can never
//     let its Wait pass before a just-admitted episode is counted (finding 2).
//
// The per-process wait context is derived from the validated coordCtx here (not a
// pre-read snapshot) so it always belongs to the current run.
func (s *Supervisor) admitGated(mp *ManagedProcess) (procCtx context.Context, procCancel context.CancelFunc, supCtx context.Context, gen uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != "running" || !s.launchable.Load() {
		return nil, nil, nil, 0, domain.ErrShutdownInProgress
	}
	if s.processes[mp.Name()] != mp {
		return nil, nil, nil, 0, domain.ErrProcessNotFound
	}

	base := s.coordCtx
	if base == nil {
		base = context.Background()
	}
	pctx, cancel := context.WithCancel(base)
	g, prev, ok := mp.beginWaiting(cancel)
	if !ok {
		cancel()
		if prev == domain.ProcessStateWaiting {
			return nil, nil, nil, 0, domain.ErrProcessAlreadyWaiting
		}
		return nil, nil, nil, 0, domain.ErrProcessAlreadyRunning
	}
	s.coordWg.Add(1)
	return pctx, cancel, s.ctx, g, nil
}

// scheduleGated admits a gated process (admitGated) and, on success, launches its
// orchestration goroutine (plan 013 D4). It is the single entry point for
// starting (or re-demanding) a gated process; it returns the admission error
// unchanged so callers surface already-running/already-waiting/shutdown as usual.
func (s *Supervisor) scheduleGated(mp *ManagedProcess) error {
	procCtx, procCancel, supCtx, gen, err := s.admitGated(mp)
	if err != nil {
		return err
	}
	targets := mp.Config().DependsOn
	go s.orchestrate(mp, targets, gen, procCtx, procCancel, supCtx)
	return nil
}

// orchestrate resolves a gated process's dependency targets and then launches or
// blocks it (plan 013 D4). See the file header for the model. gen is the launch
// generation captured for this episode; it is carried to the final gate so a
// superseding stop/restart refuses a stale launch.
func (s *Supervisor) orchestrate(mp *ManagedProcess, targets []string, gen uint64, procCtx context.Context, procCancel context.CancelFunc, supCtx context.Context) {
	defer s.coordWg.Done()
	// The wait is over once we return: cancel the per-process context so its
	// Demand joins release and it does not leak. A launched process runs under
	// supCtx, not procCtx, so this never disturbs it.
	defer procCancel()

	// Resolve targets concurrently -- dependencies are independent roots, so there
	// is no reason to serialize their probes -- and aggregate in declaration order.
	outcomes := make([]DepOutcome, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			outcomes[i] = s.demandTarget(procCtx, target)
		}(i, target)
	}
	wg.Wait()

	canceled := false
	var blockedBy []string
	for i, target := range targets {
		switch o := outcomes[i]; {
		case o.Canceled():
			// A canceled resolution (shutdown or per-process stop) is NOT a failure
			// and takes precedence over any blocking: the stopping path owns the
			// terminal state, so we neither launch nor block.
			canceled = true
		case !o.Ready():
			blockedBy = append(blockedBy, target)
		}
	}

	if canceled {
		// A canceled wait means shutdown, a per-process stop, or a superseding
		// re-demand is in progress; that path OWNS settling the terminal state
		// (Stop's waiting-case -> stopped, or the new episode). We deliberately
		// leave the state untouched here. This relies on findings 1/2/5 having
		// closed every window where a wait is canceled yet nothing settles it: a
		// stop bumps the generation and settles stopped; shutdown quiesces coordWg
		// then stops the still-waiting process; admission is refused once shutdown
		// began, so no orphan episode is ever left resolving with no settler.
		return
	}

	if len(blockedBy) > 0 {
		if mp.markBlocked(blockedBy, gen) {
			s.SystemLog("%s blocked: dependency not ready: %s", mp.Name(), strings.Join(blockedBy, ", "))
		}
		return
	}

	// All targets satisfied -> launch through the normal start path, under the
	// generation guard so a stop/restart that superseded us refuses the launch.
	if err := mp.startGated(supCtx, gen); err != nil {
		switch {
		case errors.Is(err, errLaunchSuperseded), errors.Is(err, domain.ErrShutdownInProgress),
			errors.Is(err, domain.ErrProcessAlreadyRunning):
			// Stopped/restarted/superseded out from under us, or shutdown closed the
			// gate: nothing to launch, and the stopping path owns the state.
			return
		default:
			// A genuine launch failure (e.g. a surviving previous group refused the
			// launch with ErrProcessGroupNotReaped, or the runner failed). Settle the
			// process in crashed rather than leaving it stranded in waiting: a
			// waiting process is skipped by Stop's no-instance shortcut, so a live
			// unreaped group would be orphaned and the ledger could be dropped
			// (finding 3). failWaitingLaunch retains p.current so Stop's crashed path
			// reaps the group. It no-ops when the runner path already committed
			// crashed, or when a superseding episode moved the process on.
			mp.failWaitingLaunch(gen)
			s.logManager.Write(domain.LogEntry{
				Timestamp: time.Now(),
				Process:   mp.Name(),
				Stream:    domain.StreamStderr,
				Line:      fmt.Sprintf("Failed to start: %v", err),
			})
			return
		}
	}
	s.emit(SupervisorEvent{
		Type:      EventTypeProcessStarted,
		Process:   mp.Name(),
		Timestamp: time.Now(),
		Info:      mp.Info(),
	})
}

// demandTarget resolves a single depends_on target to a readiness outcome (plan
// 013 D4).
//
// C3 supports DEPENDENCY targets only: they are demanded from the resolver. TASK
// targets are the C4 seam -- task execution does not exist yet, so a task target
// cannot be satisfied. Rather than hang the dependent process in waiting forever,
// it is reported as a readiness failure so the process becomes blocked with a
// clear reason. Config validation guarantees a target is either a dependency or a
// task, so the else branch is always a task.
//
// TODO(plan 013 C4): route task targets through the task coordinator (tasks run
// as children and resolve to completed/failed) instead of this stub.
func (s *Supervisor) demandTarget(ctx context.Context, name string) DepOutcome {
	if _, ok := s.config.Dependencies[name]; ok {
		return s.resolver.Demand(ctx, name)
	}
	return DepOutcome{
		State: DepStateFailed,
		Err:   fmt.Errorf("task dependency %q not yet startable (task execution lands in a later change)", name),
	}
}

// startProcessGated handles a manual StartProcess for a gated process (plan 013
// D4). The authoritative state check is admission (scheduleGated -> admitGated,
// atomic under s.mu+p.mu): running/starting/stopping -> ErrProcessAlreadyRunning,
// waiting -> ErrProcessAlreadyWaiting, and stopped/crashed/blocked/completed ->
// a fresh orchestration episode ("scheduled", returns nil).
//
// The one extra step for a blocked process is re-demanding its failed targets:
// their previously-failed outcomes are Reset (generation-conditionally, see
// resetFailedTarget) so the next Demand re-resolves them; healthy/warned targets
// keep their cached (instant) outcomes. BlockedBy must be read here, BEFORE
// admission, because beginWaiting clears it. When the process is not actually
// blocked (a concurrent transition), BlockedBy is empty and this is a no-op; the
// authoritative admission still maps the correct error.
func (s *Supervisor) startProcessGated(mp *ManagedProcess) error {
	for _, target := range mp.BlockedBy() {
		if _, ok := s.config.Dependencies[target]; ok {
			s.resetFailedTarget(target)
		}
	}
	return s.scheduleGated(mp)
}

// resetFailedTarget Resets a dependency target only if it is STILL in the failed
// generation the caller observes now (plan 013 D4, finding 4). Snapshotting the
// generation and passing it to ResetIfGeneration prevents an unconditional Reset
// from cancelling a NEWER generation another process just started resolving for
// the same shared target -- which would strand that process in waiting forever.
func (s *Supervisor) resetFailedTarget(target string) {
	if snap, ok := s.resolver.Snapshot(target); ok && snap.State == DepStateFailed {
		s.resolver.ResetIfGeneration(target, snap.Gen)
	}
}

// restartProcessGated handles a manual RestartProcess for a gated process (plan
// 013 D4), upholding the fail-before-stop guarantee: it reloads config and
// re-resolves ALL dependency targets BEFORE touching the running instance. Only
// when every target is satisfied does it stop+swap+start. Any resolution failure
// (or a reload failure) leaves the running process untouched and returns the
// error.
//
// A non-running gated process (waiting/blocked/stopped) has nothing to stop, so a
// restart is equivalent to a fresh (re-)demand -- it routes through
// startProcessGated, which schedules a fresh orchestration episode.
func (s *Supervisor) restartProcessGated(ctx context.Context, mp *ManagedProcess, supCtx context.Context) error {
	if mp.State() != domain.ProcessStateRunning {
		return s.startProcessGated(mp)
	}

	// Reload config first (fail-before-stop): an invalid file / removed process /
	// missing env_file fails here with the running process untouched.
	pending, err := s.prepareReload(mp.Name())
	if err != nil {
		return err
	}

	// Re-resolve every target BEFORE the stop. A failed re-resolution returns with
	// the running process untouched. When a reload changed depends_on, the fresh
	// config's targets govern (pending carries them); otherwise the current ones.
	targets := mp.Config().DependsOn
	if pending != nil {
		targets = pending.config.DependsOn
	}
	if err := s.reresolveTargets(targets); err != nil {
		return err
	}

	// All satisfied: supersede any stale orchestration, then stop+swap+start. The
	// stop half is bounded by the process's own (pre-swap) stop budget, mirroring
	// the ungated restart path.
	mp.waitGen.Add(1)
	restartCtx, cancel := context.WithTimeout(ctx, mp.StopTimeout())
	defer cancel()
	if err := mp.Restart(restartCtx, supCtx, pending); err != nil {
		return err
	}
	// Emit the same process_started event the ungated restart path emits (finding
	// 6): without this a successful running gated restart was silent to subscribers.
	s.emit(SupervisorEvent{
		Type:      EventTypeProcessStarted,
		Process:   mp.Name(),
		Timestamp: time.Now(),
		Info:      mp.Info(),
	})
	return nil
}

// reresolveTargets demands a restart's targets fresh (plan 013 D4). It first
// Resets any target whose current resolver outcome is FAILED so it re-resolves
// under a new generation; healthy/warned targets keep their cached outcome and
// return instantly (a cached healthy outcome is accepted -- restart does not
// re-probe a healthy dependency). It returns an error if any target is not ready
// (or its resolution was canceled), so the caller can abort the restart with the
// running process untouched.
//
// The demand uses coordCtx (shutdown-cancelable) rather than the request context
// so a short API deadline does not spuriously cut a dependency's readiness budget
// short; the dependency's own timeout still bounds the wait.
func (s *Supervisor) reresolveTargets(targets []string) error {
	s.mu.RLock()
	coordCtx := s.coordCtx
	s.mu.RUnlock()
	if coordCtx == nil {
		coordCtx = context.Background()
	}

	for _, target := range targets {
		s.resetFailedTarget(target)
	}
	for _, target := range targets {
		outcome := s.demandTarget(coordCtx, target)
		if outcome.Canceled() {
			return fmt.Errorf("restart aborted: dependency %q resolution canceled: %w", target, outcome.Err)
		}
		if !outcome.Ready() {
			return fmt.Errorf("restart aborted: dependency %q not ready: %w", target, outcome.Err)
		}
	}
	return nil
}
