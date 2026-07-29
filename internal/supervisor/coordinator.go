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
// depends_on targets. A DEPENDENCY target resolves via the resolver; a TASK
// target resolves via the task coordinator (tasks.go) -- see demandTarget.
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
	deps := s.domainDependencies(s.config)
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

// domainDependencies converts a config's dependencies: block to the resolver's
// domain form (plan 013 D4/D6). An individual dependency whose config fails to
// convert (already rejected by validation, so unreachable on the normal path) is
// logged and skipped rather than failing the whole conversion. Shared by
// buildDefaultResolver (up-time) and prepareReload (the fresh set carried on a
// pending config for the D6 restart refresh).
func (s *Supervisor) domainDependencies(cfg *config.Config) map[string]domain.DependencyConfig {
	deps := make(map[string]domain.DependencyConfig, len(cfg.Dependencies))
	for name, dc := range cfg.Dependencies {
		dd, err := dc.ToDomain(name)
		if err != nil {
			s.systemErrorf("dependency %q: invalid config, skipped: %v", name, err)
			continue
		}
		deps[name] = dd
	}
	return deps
}

// prepareRestartTargets selects the targets a gated restart / task re-run
// re-resolves against, installing the reload's classification view and resolver
// definitions first (plan 013 D6, fix 4/5). With a pending reload it calls
// applyReloadGraph (replaces the effective view, Redefines changed/new
// dependencies, prunes removed/migrated ones) and governs by the reload's
// depends_on; without one it keeps the current targets. Classification always
// goes through s.classifyDependency, which reads the (now fresh) effective view.
func (s *Supervisor) prepareRestartTargets(pending *pendingConfig, current []string) ([]string, func(string) bool) {
	if pending == nil {
		return current, s.classifyDependency
	}
	s.applyReloadGraph(pending)
	return pending.config.DependsOn, s.classifyDependency
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
	// Admission committed the waiting state (beginWaiting), which is
	// ProcessInfo-visible via State and WaitingOn. Lock discipline: admitGated took
	// and RELEASED s.mu (and p.mu inside it) before returning, so this fires with no
	// lock held -- it deliberately is NOT inside admitGated, which notifies under
	// s.mu.
	s.notifyChange()
	targets := mp.Config().DependsOn
	go s.orchestrate(mp, targets, gen, procCtx, procCancel, supCtx, nil)
	return nil
}

// orchestrate resolves a gated process's dependency targets and then launches or
// blocks it (plan 013 D4). See the file header for the model. gen is the launch
// generation captured for this episode; it is carried to the final gate so a
// superseding stop/restart refuses a stale launch. pending, when non-nil, is a
// reload applied atomically at the launch gate (plan 013 D6): a blocked/stopped
// gated (re)start reloads the child's config just like the ungated path.
func (s *Supervisor) orchestrate(mp *ManagedProcess, targets []string, gen uint64, procCtx context.Context, procCancel context.CancelFunc, supCtx context.Context, pending *pendingConfig) {
	defer s.coordWg.Done()
	// The wait is over once we return: cancel the per-process context so its
	// Demand joins release and it does not leak. A launched process runs under
	// supCtx, not procCtx, so this never disturbs it.
	defer procCancel()

	// Resolve targets concurrently and aggregate in declaration order.
	canceled, blockedBy := s.demandTargets(procCtx, targets, s.classifyDependency)

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
			// waiting -> blocked, with BlockedOn now populated. Lock discipline:
			// markBlocked released p.mu before returning and orchestrate holds no
			// supervisor lock.
			s.notifyChange()
		}
		return
	}

	// All targets satisfied -> launch through the normal start path, under the
	// generation guard so a stop/restart that superseded us refuses the launch.
	if err := mp.startGated(supCtx, pending, gen); err != nil {
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
			if mp.failWaitingLaunch(gen) {
				// waiting -> crashed, committed AFTER the launch attempt's own notify.
				// Lock discipline: failWaitingLaunch released p.mu before returning.
				s.notifyChange()
			}
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

// demandTargets resolves a set of depends_on targets concurrently and aggregates
// the outcome in declaration order (plan 013 D4/D3). Targets are independent
// roots, so their demands run in parallel. It returns whether ANY target's
// resolution was canceled -- shutdown or a per-process stop, which is NOT a
// failure and takes precedence: the stopping path owns the terminal state, so the
// caller neither launches nor blocks -- and the not-ready targets (the blockers)
// in declaration order. Shared by orchestrate (a gated process's depends_on) and
// executeTask (a task's own depends_on).
func (s *Supervisor) demandTargets(ctx context.Context, targets []string, isDep func(string) bool) (canceled bool, blockedBy []string) {
	outcomes := make([]DepOutcome, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			outcomes[i] = s.demandTarget(ctx, target, isDep)
		}(i, target)
	}
	wg.Wait()

	for i, target := range targets {
		switch o := outcomes[i]; {
		case o.Canceled():
			canceled = true
		case !o.Ready():
			blockedBy = append(blockedBy, target)
		}
	}
	return canceled, blockedBy
}

// demandTarget resolves a single depends_on target to a readiness outcome (plan
// 013 D4/D3). A DEPENDENCY target is demanded from the resolver; a TASK target is
// demanded from the task coordinator (tasks.go), which runs the task once per
// supervisor lifetime and resolves it to completed -> ready or crashed/blocked ->
// failed. Config validation guarantees a target is either a dependency or a task.
//
// isDep classifies the name; classifyDependency reads the effective view, which a
// reload replaces (plan 013 D6, fix 4/5).
//
// Retirement re-demand (plan 013 fix 1): a canceled outcome is AMBIGUOUS. It
// means either (a) the DEMANDER's own wait ended (shutdown / a stop of this
// process) -- in which case the stopping path owns the state and we return the
// cancel -- or (b) the TARGET's node/generation was RETIRED mid-flight (a task
// re-run retiring its node, a dependency Redefine/ApplyGraph cancelling a
// generation) while this demander is still live. In case (b) returning canceled
// would strand the demander forever (e.g. a process waiting on a task that is
// being re-run). So: while our OWN ctx is still live, a canceled outcome means the
// target was retired -- re-demand, joining the replacement node / fresh
// generation. The loop blocks on each new in-flight resolution, so it cannot spin
// (a genuinely-gone target resolves to failed/unknown, not canceled), and it is
// bounded by ctx: once the demander's own wait is canceled, the cancel propagates.
func (s *Supervisor) demandTarget(ctx context.Context, name string, isDep func(string) bool) DepOutcome {
	for {
		var out DepOutcome
		if isDep(name) {
			out = s.resolver.Demand(ctx, name)
		} else {
			out = s.demandTask(ctx, name)
		}
		if !out.Canceled() || ctx.Err() != nil {
			return out
		}
		// Target retired while our episode is still current: re-demand.
	}
}

// classifyDependency is the depends_on classifier: a name is a dependency iff it
// is in the supervisor's effective classification view (plan 013 D4, fix 4/5),
// otherwise it is a task. The view is replaced on a successful reload
// (applyReloadGraph), so this reflects CURRENT config -- a reloaded/rerun episode
// classifies its targets correctly. Validation guarantees every target is one or
// the other. Falls back to the up-time config before the first Start (view nil).
func (s *Supervisor) classifyDependency(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	eff := s.effective
	if eff == nil {
		_, ok := s.config.Dependencies[name]
		return ok
	}
	_, ok := eff.deps[name]
	return ok
}

// taskNameSet returns the set of task names in a config, for a reload's fresh
// classification view (plan 013 fix 4/5).
func taskNameSet(cfg *config.Config) map[string]struct{} {
	set := make(map[string]struct{}, len(cfg.Tasks))
	for name := range cfg.Tasks {
		set[name] = struct{}{}
	}
	return set
}

// buildEffectiveGraph snapshots a config's dependency and task name sets for the
// classification view (plan 013 fix 4/5).
func buildEffectiveGraph(cfg *config.Config) *effectiveGraph {
	eff := &effectiveGraph{
		deps:  make(map[string]struct{}, len(cfg.Dependencies)),
		tasks: make(map[string]struct{}, len(cfg.Tasks)),
	}
	for name := range cfg.Dependencies {
		eff.deps[name] = struct{}{}
	}
	for name := range cfg.Tasks {
		eff.tasks[name] = struct{}{}
	}
	return eff
}

// applyReloadGraph installs a reload's classification view and refreshes the
// resolver to match (plan 013 fix 4/5). It REPLACES (never unions) the effective
// view from the reload's fresh dependency/task sets, then refreshes the resolver
// atomically via ApplyGraph: redefine every changed/new dependency and drop any
// name absent from the fresh set so a removed or migrated (dependency->task) name
// is forgotten -- all under one resolver lock (plan 013 D5 fix). Ordering: swap
// the view first so a concurrent classification never sees a dependency the
// resolver has already dropped.
func (s *Supervisor) applyReloadGraph(pending *pendingConfig) {
	if pending == nil {
		return
	}
	eff := &effectiveGraph{
		deps:  make(map[string]struct{}, len(pending.freshDeps)),
		tasks: make(map[string]struct{}, len(pending.freshTasks)),
	}
	for name := range pending.freshDeps {
		eff.deps[name] = struct{}{}
	}
	for name := range pending.freshTasks {
		eff.tasks[name] = struct{}{}
	}
	s.mu.Lock()
	s.effective = eff
	resolver := s.resolver
	s.mu.Unlock()

	if resolver == nil {
		return
	}
	// Install the fresh dependency set atomically (plan 013 D5 fix): a single
	// ApplyGraph under one resolver lock replaces the old redefine-loop +
	// RetainOnly, so a concurrent StatusSnapshots never sees a half-applied mix.
	resolver.ApplyGraph(pending.freshDeps)
}

// startProcessGated handles a manual StartProcess (and the non-running
// RestartProcess) for a gated process (plan 013 D4/D6). It RELOADS the child's
// config and refreshes the dependency graph, matching ungated StartProcess (which
// reloads via the pendingConfig machinery): a blocked/stopped gated (re)start must
// pick up an edited dependency definition AND the child's changed cmd/env, not
// keep the stale resolver/effective-graph and stored config. The live D6 gap this
// closes: a process blocked on a mis-configured dependency stayed blocked with the
// OLD definition after the user fixed prox.yaml and ran `prox restart`.
//
// Sequence (matching ungated fail-before-anything semantics):
//   - prepareReload is a PURE read: an invalid/removed/env-broken file errors here
//     with NOTHING changed (the process stays blocked, the resolver untouched);
//   - admitGated is the atomic state gate BEFORE any graph/config mutation, so an
//     active process is refused (already-running/waiting) without churning the
//     resolver or swapping a live child's config;
//   - only once admitted do we applyReloadGraph (redefine/add/prune deps + replace
//     the effective view) and re-resolve the previously-failed/warned targets;
//   - the child's own cmd/env/depends_on swap is applied ATOMICALLY at the launch
//     gate via the pending threaded into orchestrate.
//
// BlockedBy must be read BEFORE admission (beginWaiting clears it). Targets are
// Reset generation-conditionally via resetUnreadyTarget (FAILED or WARNED, plan
// D2); classification uses the effective view so a reload-added dependency is
// handled. Tasks re-run through their own coordinator, so only dependency targets
// are Reset here.
func (s *Supervisor) startProcessGated(mp *ManagedProcess) error {
	blocked := mp.BlockedBy()

	// Reload first (pure read, fail-before-anything).
	pending, err := s.prepareReload(mp.Name())
	if err != nil {
		return err
	}

	// Atomic admission before any mutation; a refused start leaves the graph and
	// stored config untouched.
	procCtx, procCancel, supCtx, gen, err := s.admitGated(mp)
	if err != nil {
		return err
	}
	// Admission committed the waiting state (see scheduleGated's note). Lock
	// discipline: admitGated released s.mu (and p.mu) before returning.
	s.notifyChange()

	// Admitted (the child is now waiting). Refresh the resolver + effective view
	// from the reload, then re-resolve the previously-unready targets fresh.
	if pending != nil {
		s.applyReloadGraph(pending)
	}
	for _, target := range blocked {
		if s.classifyDependency(target) {
			s.resetUnreadyTarget(target)
		}
	}
	targets := mp.Config().DependsOn
	if pending != nil {
		targets = pending.config.DependsOn
	}
	go s.orchestrate(mp, targets, gen, procCtx, procCancel, supCtx, pending)
	return nil
}

// resetUnreadyTarget Resets a dependency target whose cached outcome is not
// healthy -- FAILED or WARNED -- so an explicit re-demand (restart / start-on-
// blocked) re-probes it fresh (plan 013 D2/D4). WARNED is included per plan D2's
// "a warned/failed dependency is re-resolved fresh the next time something demands
// it": otherwise a dependency that warned while its backing resource was booting
// stays cached-warned forever and a later restart accepts the stale outcome
// without re-probing. HEALTHY stays cached (a restart never re-probes a healthy
// dependency). The reset is generation-conditional (finding 4): snapshotting the
// generation and passing it to ResetIfGeneration prevents an unconditional Reset
// from cancelling a NEWER generation another process just started resolving for
// the same shared target -- which would strand that process in waiting forever.
func (s *Supervisor) resetUnreadyTarget(target string) {
	if snap, ok := s.resolver.Snapshot(target); ok &&
		(snap.State == DepStateFailed || snap.State == DepStateWarned) {
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

	// Re-resolve every target BEFORE the stop, against the reload's fresh
	// dependency definitions (plan 013 D6): otherwise re-resolution runs against
	// the resolver's up-time definitions and a reload that added or redefined a
	// dependency would not be seen. A failed re-resolution returns with the running
	// process untouched.
	targets, isDep := s.prepareRestartTargets(pending, mp.Config().DependsOn)
	if err := s.reresolveTargets(targets, isDep); err != nil {
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
// Resets any target whose current resolver outcome is FAILED or WARNED so it
// re-resolves under a new generation (plan D2: a warned/failed dependency is
// re-probed the next time something demands it, so a stale warn does not persist);
// only a HEALTHY target keeps its cached outcome and returns instantly (a restart
// does not re-probe a healthy dependency). It returns an error if any target is
// not ready (or its resolution was canceled), so the caller can abort the restart
// with the running process untouched.
//
// The demand uses coordCtx (shutdown-cancelable) rather than the request context
// so a short API deadline does not spuriously cut a dependency's readiness budget
// short; the dependency's own timeout still bounds the wait.
func (s *Supervisor) reresolveTargets(targets []string, isDep func(string) bool) error {
	s.mu.RLock()
	coordCtx := s.coordCtx
	s.mu.RUnlock()
	if coordCtx == nil {
		coordCtx = context.Background()
	}

	for _, target := range targets {
		if isDep(target) {
			s.resetUnreadyTarget(target)
		}
	}
	for _, target := range targets {
		outcome := s.demandTarget(coordCtx, target, isDep)
		if outcome.Canceled() {
			return fmt.Errorf("restart aborted: dependency %q resolution canceled: %w", target, outcome.Err)
		}
		if !outcome.Ready() {
			return fmt.Errorf("restart aborted: dependency %q not ready: %w", target, outcome.Err)
		}
	}
	return nil
}
