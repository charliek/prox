package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

// outputDrainTimeout is the maximum time to wait for output readers to finish
// after a process exits. This allows grandchild processes to complete their
// final writes before we stop reading.
const outputDrainTimeout = 5 * time.Second

// groupPollInterval is how often Stop probes the process group's liveness
// while waiting for a graceful (SIGTERM) or forced (SIGKILL) shutdown.
const groupPollInterval = 100 * time.Millisecond

// processInstance represents a single run of a managed process. Every Start
// creates a fresh instance; ManagedProcess.current points at the live (or
// most-recent) one. Keeping per-run state on an instance -- rather than flat
// fields on ManagedProcess -- lets a stale monitor from a previous run close
// only its own done channel and never clobber a newer run's state (see the
// generation guard in monitor and the finalization gate in Stop).
type processInstance struct {
	proc     Process
	done     chan struct{}
	doneOnce sync.Once
	cancel   context.CancelFunc
	outputWg sync.WaitGroup

	// exited is closed by monitor IMMEDIATELY after inst.proc.Wait() returns --
	// BEFORE output draining (plan 013 D3, fix 6). It signals process EXIT, which
	// can precede the monitor's state commit (done) by up to outputDrainTimeout
	// when a grandchild holds a pipe open. The task run-budget timer watches this,
	// not done, so a slow drain of a naturally-completed rc=0 task is not
	// misclassified as a run-timeout crash. Closed exactly once via exitedOnce.
	exited     chan struct{}
	exitedOnce sync.Once
	// claim is the single terminal-outcome commit point for a task run (plan 013
	// D3, fix 6). The monitor CAS(none->exit)es it right after Wait; the task
	// run-budget timer CAS(none->timeout)es it on fire. Whoever wins owns the
	// terminal verdict, so a completed exit and a run-timeout can never both
	// commit -- if the exit was claimed first, the timer is a no-op (completed
	// wins, no timeout log); if the timer claimed first, the monitor defers its
	// natural-exit commit to stopTask (which commits crashed).
	claim atomic.Int32
}

// terminalClaim is the winner of a task run's terminal-outcome race (fix 6).
type terminalClaim int32

const (
	claimNone    terminalClaim = 0
	claimExit    terminalClaim = 1 // the process exited on its own (monitor)
	claimTimeout terminalClaim = 2 // the run budget elapsed (task coordinator)
)

// claimTerminal attempts to claim the terminal outcome for a task run, returning
// whether this caller won. Only the winner acts on the verdict.
func (i *processInstance) claimTerminal(c terminalClaim) bool {
	return i.claim.CompareAndSwap(int32(claimNone), int32(c))
}

// markExited closes the exited channel exactly once (called by monitor right
// after Wait returns, before draining).
func (i *processInstance) markExited() {
	i.exitedOnce.Do(func() { close(i.exited) })
}

// closeDone closes the instance's done channel exactly once.
func (i *processInstance) closeDone() {
	i.doneOnce.Do(func() { close(i.done) })
}

// pendingConfig carries a freshly-reloaded process runtime (domain config, env
// loader closure, the env map that closure produced, and effective stop budget)
// to be swapped into a ManagedProcess atomically inside startWithConfig's locked
// critical section (#33, D3). The supervisor builds it via prepareReload before
// any stop; a nil pending means "no reload -- keep the stored config".
type pendingConfig struct {
	config  domain.ProcessConfig
	loadEnv func() (map[string]string, error)
	// env is the exact map prepareReload's preflight already loaded and
	// validated from disk, BEFORE the stop. startWithConfig applies it as-is for
	// this run rather than re-invoking loadEnv, so an env file deleted between
	// the preflight and the launch cannot cause downtime (the fail-before-stop
	// promise) and the running process gets exactly what was validated. The
	// loadEnv closure is still stored for any future non-reload start, preserving
	// the #30 fresh-read-on-every-start semantics.
	env         map[string]string
	stopTimeout time.Duration
	// freshDeps carries the reload's dependency definitions (converted to domain
	// form) so the gated restart path can refresh the resolver against a reload
	// that added or redefined a dependency, and classify depends_on targets
	// against the fresh set (plan 013 D6). nil when reload is disabled.
	freshDeps map[string]domain.DependencyConfig
	// freshTasks is the reload's set of task names, used with freshDeps to REPLACE
	// the supervisor's effective classification view on a successful reload so
	// depends_on targets classify against current config, not the immutable
	// startup snapshot (plan 013 fix 4/5). nil when reload is disabled.
	freshTasks map[string]struct{}
}

// stopEpisode carries the verdict of a single in-flight Stop to any concurrent
// secondary Stop that joins while the process is Stopping. Exactly one episode
// is installed at every state->Stopping transition (both the normal entry and
// the crashed-retry fall-through) and resolved exactly once, via
// resolveStopEpisodeLocked, IN THE SAME p.mu critical section that commits the
// terminal state verdict.
//
// Invariant: any goroutine that observes the terminal state (Stopped/Crashed)
// under p.mu also observes the episode resolved -- state commit and episode
// resolution are one atomic step. A Stop that arrives after the commit is by
// definition NOT concurrent with the episode; it correctly sees
// ErrProcessNotRunning (clean case) or starts a legitimate fresh retry episode
// (surviving-group case). That boundary is expected behavior, not divergence.
//
// resolved (guarded by p.mu) makes resolution idempotent so the deferred panic
// backstop is a no-op once the explicit in-critical-section resolution has run.
// done is closed AFTER err is written, so a waiter that reads err after <-done
// needs no lock (the write happens-before the close). Episodes are distinct
// objects per transition, so a waiter that captured episode N never observes
// episode N+1's verdict (#32, D1).
type stopEpisode struct {
	done     chan struct{}
	err      error
	resolved bool
}

// ManagedProcess handles the lifecycle of a single process
type ManagedProcess struct {
	mu sync.RWMutex

	// name is the process's identity, set once at construction and never
	// mutated (a rename requires `prox up`, not a restart). It is read lock-free
	// for log attribution (logf/readOutput) and error messages so a config swap
	// under mu.Lock cannot race those reads, and so an old run's monitor stays
	// correctly attributed across a swap (#33, D3).
	name string

	// kind is the child's run mode (plan 013 D3): a plain process or a
	// run-to-completion task. Set once at construction from the domain config's
	// Kind (defaulted to process) and never mutated -- a reload swaps cmd/env,
	// not the kind. Read lock-free like name; task-mode branches in monitor and
	// stop key off it.
	kind domain.ProcessKind

	config     domain.ProcessConfig
	env        map[string]string
	runner     ProcessRunner
	logManager *logs.Manager

	// completedAt freezes a task's uptime at the moment it reaches
	// ProcessStateCompleted (plan 013 D3). Info reports the frozen run duration
	// (completedAt - startedAt) rather than letting uptime tick past completion.
	// Guarded by p.mu; zero until a task completes.
	completedAt time.Time

	// taskTimeout / taskHasTimeout are a task's run budget (plan 013 D3),
	// swappable on a manual re-run's reload. Meaningful only when kind is
	// ProcessKindTask. Guarded by p.mu; read via taskBudget.
	taskTimeout    time.Duration
	taskHasTimeout bool

	// loadEnv, if set, is called at the top of every Start to (re)load the
	// process's environment from disk (env_file(s)) merged with inline env.
	// When nil, Start uses the stored env as-is (back-compat for tests that
	// construct ManagedProcess directly via NewManagedProcess).
	loadEnv func() (map[string]string, error)

	state        domain.ProcessState
	startedAt    time.Time
	restartCount int

	// current is the live (or most-recent) run of this process. It is retained
	// even after the run stops/crashes so a surviving process group stays
	// reapable by a later Stop and so Info can report the last PID.
	//
	// Invariant: a non-nil current always has a non-nil proc (a failed
	// runner.Start is never published as current).
	current *processInstance

	// Health checker for the current run.
	healthChecker *HealthChecker

	// restartStartBarrier is a test-only seam (nil in production). When set, it
	// is invoked inside Restart after the stop half completes and before the
	// replacement's start half runs, so an interleaving test can deterministically
	// hold a restart open in the unlocked gap between its stop and start and race
	// a concurrent StartProcess into it. Read once under the lock in Restart.
	restartStartBarrier func()

	// episode is the in-flight stop's verdict carrier, installed under mu at every
	// state->Stopping transition and cleared by finishStopEpisode when that stop
	// resolves. A secondary Stop joining while state==Stopping captures it under
	// mu and waits on it for the primary's verdict (#32, D1). nil when no stop is
	// in flight.
	episode *stopEpisode

	// launchGate, when set, is the supervisor's launch gate closure (#32/#36, D2).
	// startWithConfig invokes it inside its p.mu critical section, after the state
	// and surviving-group guards and BEFORE the pending-config swap; a non-nil
	// error (ErrShutdownInProgress) aborts the launch with no swap applied and the
	// state unchanged. supervisor.createManagedProcess injects it; nil (direct
	// NewManagedProcess construction in tests) means "always allow".
	launchGate func() error

	// onLaunched, when set, is invoked by startWithConfig AFTER a successful launch
	// and AFTER p.mu is released. It persists the supervisor's orphan-reaping
	// ledger (see Supervisor.persistChildren / orphans.go). It is deliberately
	// called outside the p.mu critical section: persistChildren takes s.mu, and the
	// verified s.mu -> p.mu order would AB-BA if it ran while p.mu was held.
	// supervisor.createManagedProcess injects it; nil (direct construction in
	// tests) means "no persistence". Set once before publication, so it is read
	// lock-free (like launchGate).
	onLaunched func()

	// onChange, when set, wakes the supervisor's process-change bus (plan 017 C10).
	// It carries no payload -- a subscriber re-reads Processes() -- and is invoked
	// ONLY with p.mu released (see notifyChange), because a woken subscriber calls
	// straight back into Processes(), whose s.mu -> p.mu order would AB-BA against a
	// notify fired under p.mu. supervisor.createManagedProcess/createManagedTask
	// inject it; nil (direct construction in tests) means "no bus". Set once before
	// publication, so it is read lock-free (like launchGate/onLaunched).
	onChange func()

	// stopBarrier is a test-only seam (nil in production). Stop invokes it,
	// unlocked, at interleaving-sensitive points identified by phase:
	//
	//   - "primary-installed": in the primary path just after the stop episode is
	//     installed and the lock released (before any signalling/verdict work), so
	//     a test can hold the primary open with its episode published.
	//   - "secondary-joined": in the Stopping branch just after a secondary
	//     captured the in-flight episode and released the lock (before it waits on
	//     the verdict), so a test can confirm the join happened.
	//   - "finalizing": in the primary path inside the finalization window, after
	//     the monitor-drain wait and BEFORE the authoritative verdict/state commit,
	//     so a test can hold a verdict late and prove a concurrent secondary
	//     waiter outlives exactly this tail (the window stopVerdictMargin covers).
	//   - "verdict-committed": in the primary path just after the terminal state
	//     and episode were committed atomically, to probe that boundary.
	//
	// Together they make concurrent-stop interleavings deterministic without
	// sleeps (#32), mirroring the restartStartBarrier seam pattern.
	stopBarrier func(phase string)

	// shutdownTimeout is this process's effective stop budget: the
	// no-context-deadline fallback in computeDeadlines. supervisor.
	// createManagedProcess resolves and sets it (per-process stop_timeout,
	// else global shutdown_timeout, else constants.DefaultShutdownTimeout)
	// before the ManagedProcess is published, so production code never sees
	// zero here. Tests that construct a ManagedProcess directly (bypassing
	// createManagedProcess) may still set it directly as a seam to shrink the
	// fallback window; production code and computeDeadlines both go through
	// the locked StopTimeout() accessor.
	shutdownTimeout time.Duration

	// --- gated-launch orchestration (plan 013 D4) ---
	//
	// A process with a non-empty depends_on is "gated": Supervisor.Start does not
	// launch it directly but registers it in state `waiting` and hands it to the
	// graph coordinator (see coordinator.go), which resolves its dependency
	// targets in a background goroutine and then drives it to running (all
	// satisfied) or blocked (a required target failed). These fields carry the
	// per-process state that orchestration needs.

	// waitGen is the launch generation for gated orchestration. A coordinator
	// goroutine captures it when it begins resolving; the launch is committed only
	// if it still matches at the final gate (startWithConfigLocked, under p.mu),
	// so a stop/restart/re-demand that bumped it supersedes a stale completion
	// (constraint: an atomic checked at the final gate). Every mutation happens
	// under p.mu; the final-gate read is under p.mu too, but the field stays an
	// atomic to match the launch-gate's lock-free-read discipline.
	waitGen atomic.Uint64

	// waitCancel cancels THIS process's pending dependency wait (the coordinator's
	// per-process context). StopProcess of a waiting process calls it to unblock
	// the coordinator's Demand joins without aborting the shared dependency
	// resolution other processes depend on. Guarded by p.mu; nil when no wait is
	// in flight.
	waitCancel context.CancelFunc

	// blockedBy records, in declaration order, the depends_on targets that failed
	// and left this process blocked (plan 013 D4). Stored for status surfacing in
	// C5; read via BlockedBy. Guarded by p.mu.
	blockedBy []string
}

// NewManagedProcess creates a new managed process
func NewManagedProcess(config domain.ProcessConfig, env map[string]string, runner ProcessRunner, logManager *logs.Manager) *ManagedProcess {
	kind := config.Kind
	if kind == "" {
		kind = domain.ProcessKindProcess
	}
	return &ManagedProcess{
		name:           config.Name,
		kind:           kind,
		config:         config,
		env:            env,
		runner:         runner,
		logManager:     logManager,
		state:          domain.ProcessStateStopped,
		taskTimeout:    config.TaskTimeout,
		taskHasTimeout: config.TaskHasTimeout,
	}
}

// notifyChange wakes the supervisor's change bus for this process (plan 017 C10).
//
// LOCK DISCIPLINE: the caller MUST hold neither p.mu nor any supervisor lock --
// every call site below documents which lock it has just released. A nil hook
// (directly constructed test process) is a no-op.
func (p *ManagedProcess) notifyChange() {
	if p.onChange != nil {
		p.onChange()
	}
}

// isTask reports whether this managed child runs in task mode (plan 013 D3).
// kind is immutable, so no lock is taken.
func (p *ManagedProcess) isTask() bool { return p.kind == domain.ProcessKindTask }

// taskBudget returns a task's run budget (duration, hasLimit). Meaningful only
// for a task (plan 013 D3).
func (p *ManagedProcess) taskBudget() (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.taskTimeout, p.taskHasTimeout
}

// Name returns the process name. Immutable, so no lock is taken.
func (p *ManagedProcess) Name() string {
	return p.name
}

// Config returns a deep copy of the process configuration. It takes the read
// lock (config is swappable on a reload -- #33, D3) and clones the reference
// fields (Healthcheck pointer, Env map, DependsOn slice) so callers cannot
// observe a torn value or mutate the live config.
func (p *ManagedProcess) Config() domain.ProcessConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cfg := p.config
	if p.config.Healthcheck != nil {
		hc := *p.config.Healthcheck
		cfg.Healthcheck = &hc
	}
	// maps.Clone / slices.Clone return nil for a nil input, preserving the
	// nil-vs-empty distinction.
	cfg.Env = maps.Clone(p.config.Env)
	cfg.DependsOn = slices.Clone(p.config.DependsOn)
	return cfg
}

// Info returns the current process info
func (p *ManagedProcess) Info() domain.ProcessInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	info := domain.ProcessInfo{
		Name:         p.config.Name,
		State:        p.state,
		Kind:         p.kind,
		RestartCount: p.restartCount,
		// Health is resolved from p.config below; this is only a placeholder so
		// the field is never left as the empty string.
		Health:      domain.HealthStatusNone,
		Cmd:         p.config.Cmd,
		Env:         p.env,
		StopTimeout: p.shutdownTimeout,
	}

	// PID is only meaningful while the process is actively running/starting/
	// stopping. A stopped/crashed/blocked/completed process reports 0 even though
	// current is retained for reap/retry. A waiting process reports 0 too: on a
	// RE-RUN of a completed/crashed task, current still holds the reaped prior
	// instance, so guarding on IsStopped alone would surface that dead PID while
	// the task waits on its dependencies again (plan 013 D5 fix).
	if !p.state.IsStopped() && p.state != domain.ProcessStateWaiting &&
		p.current != nil && p.current.proc != nil {
		info.PID = p.current.proc.PID()
	}

	if !p.startedAt.IsZero() {
		info.StartedAt = p.startedAt
	}

	// A completed task freezes its uptime at completion (plan 013 D3): report
	// the frozen end so UptimeSeconds reflects the run duration, not now-start.
	if p.state == domain.ProcessStateCompleted && !p.completedAt.IsZero() {
		info.EndedAt = p.completedAt
	}

	// Gated-launch surfacing (plan 013 D5). While waiting, report the full
	// depends_on set (declaration order) as WaitingOn: a gated process leaves
	// waiting only when every target is satisfied, so it is still gated on the
	// set. When blocked, report the recorded blocking targets. Both are empty in
	// every other state.
	switch p.state {
	case domain.ProcessStateWaiting:
		info.WaitingOn = slices.Clone(p.config.DependsOn)
	case domain.ProcessStateBlocked:
		info.BlockedOn = slices.Clone(p.blockedBy)
	}

	// Health is derived from the CONFIG, not from whether a checker object
	// happens to exist right now (#100). p.healthChecker is created only at
	// launch and discarded on stop, so keying on it conflates three distinct
	// situations; only the config can tell "never configured" from "configured
	// but not reporting".
	switch {
	case p.config.Healthcheck == nil || p.config.Healthcheck.Cmd == "":
		// No healthcheck configured: nothing was ever run, so there is no verdict
		// to be unsure about. No HealthDetails either -- there is no check to
		// describe.
		info.Health = domain.HealthStatusNone
	case p.healthChecker == nil:
		// Configured, but no live checker: stopped, or not yet launched. The check
		// exists and simply has not reported, which is exactly what unknown means.
		info.Health = domain.HealthStatusUnknown
		info.HealthDetails = &domain.HealthState{
			Enabled: false,
			Status:  domain.HealthStatusUnknown,
		}
	default:
		state := p.healthChecker.State()
		info.Health = state.Status
		info.HealthDetails = &state
	}

	return info
}

// State returns the current state
func (p *ManagedProcess) State() domain.ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// BlockedBy returns the depends_on targets that failed and left this process
// blocked, in declaration order (plan 013 D4). Empty unless the process is in
// the blocked state. The slice is cloned so callers cannot mutate live state.
func (p *ManagedProcess) BlockedBy() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.blockedBy)
}

// gated reports whether this process has a non-empty depends_on and must go
// through the graph coordinator rather than launching directly (plan 013 D4).
func (p *ManagedProcess) gated() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.config.DependsOn) > 0
}

// beginWaiting CONDITIONALLY starts a gated orchestration episode (plan 013 D4).
// It admits a new episode only from a non-active state -- stopped, crashed,
// blocked, or completed -- and, atomically under p.mu, bumps the launch
// generation, moves the process to waiting, clears prior blocked reasons, and
// stores the per-process wait cancel so StopProcess can unblock this process's
// Demand joins. It returns (gen, true) on admission.
//
// From an ACTIVE state (running/starting/stopping) or an already-waiting one it
// makes NO change and returns (prev, false) so the caller can map the correct
// already-running / already-waiting error. Making admission conditional and
// atomic with the state read is what guarantees EXACTLY ONE episode per process:
// two concurrent starts that both observed `stopped` cannot both flip the
// process to waiting -- the first admits, the second sees `waiting` and is
// refused (findings 1/2).
func (p *ManagedProcess) beginWaiting(cancel context.CancelFunc) (uint64, domain.ProcessState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case domain.ProcessStateStopped, domain.ProcessStateCrashed,
		domain.ProcessStateBlocked, domain.ProcessStateCompleted:
		// Admissible: fall through and start a fresh episode.
	default:
		return 0, p.state, false
	}
	gen := p.waitGen.Add(1)
	p.state = domain.ProcessStateWaiting
	p.blockedBy = nil
	p.waitCancel = cancel
	p.startedAt = time.Time{}
	return gen, domain.ProcessStateWaiting, true
}

// failWaitingLaunch settles a gated process into crashed after its launch failed
// for a genuine reason -- most importantly a surviving previous group that the
// launch guard refused (ErrProcessGroupNotReaped) -- but only if this episode is
// still current AND the process is still waiting, so a superseded or
// already-stopped episode is never clobbered (plan 013 D4, finding 3). It
// deliberately RETAINS p.current: a surviving group must stay reapable by Stop's
// crashed path. Without this, a refused gated launch would strand the process in
// waiting, where Stop's no-instance shortcut skips the live group and shutdown
// could drop the orphan ledger while the group survives.
func (p *ManagedProcess) failWaitingLaunch(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitGen.Load() != gen || p.state != domain.ProcessStateWaiting {
		return false
	}
	p.state = domain.ProcessStateCrashed
	p.waitCancel = nil
	return true
}

// markBlocked moves the process to the terminal blocked state, recording the
// failed targets, but only if gen still matches the current launch generation --
// a stop/restart/re-demand that bumped the generation supersedes this stale
// completion (returns false, no state change). Plan 013 D4.
func (p *ManagedProcess) markBlocked(targets []string, gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitGen.Load() != gen {
		return false
	}
	p.state = domain.ProcessStateBlocked
	p.blockedBy = slices.Clone(targets)
	p.waitCancel = nil
	return true
}

// StopTimeout returns the effective per-process stop budget used as the
// no-context-deadline fallback in computeDeadlines.
func (p *ManagedProcess) StopTimeout() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.shutdownTimeout
}

// childRecord returns a ledger record for this process's currently-running
// group, and ok=false when there is nothing live to record. Only a process in
// the Running state with a live current instance is recorded: its leader is
// still alive, so its start token is readable and the reap's up-front identity
// check can positively match it on the next startup. A Stopping/Stopped/Crashed
// process is deliberately excluded -- its leader has been (or is being) reaped,
// so it could never be positively identified anyway.
//
// The start token comes from Process.StartToken (captured at spawn), so no
// per-process syscall runs here and the recorded token cannot be a reused PID's.
func (p *ManagedProcess) childRecord() (ChildRecord, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state != domain.ProcessStateRunning || p.current == nil || p.current.proc == nil {
		return ChildRecord{}, false
	}
	pid := p.current.proc.PID()
	pgid := p.current.proc.PGID()
	if pid <= 0 {
		return ChildRecord{}, false
	}
	// The start token is the one CAPTURED AT SPAWN (Process.StartToken), not re-read
	// here: the monitor's Wait() reaps the leader (freeing its PID) BEFORE it takes
	// p.mu to leave Running, so state can be Running while the PID is already
	// reusable. Re-reading daemon.ProcessStartTime(pid) here could therefore stamp
	// an unrelated reused-PID process's token onto this record, which the reaper
	// would later positively match and signal (codex review).
	return ChildRecord{Name: p.name, PID: pid, PGID: pgid, StartToken: p.current.proc.StartToken()}, true
}

// Start starts the process with its currently-stored config.
func (p *ManagedProcess) Start(ctx context.Context) error {
	return p.startWithConfig(ctx, nil)
}

// startWithConfig starts the process (see startWithConfigLocked for the launch
// semantics) and, on success, persists the orphan-reaping ledger via onLaunched.
//
// The persist is invoked HERE -- after startWithConfigLocked has returned and
// fully released p.mu -- rather than inside the locked section, because
// persistChildren takes s.mu and the verified s.mu -> p.mu order would AB-BA if
// it ran while p.mu was held. Every launch path funnels through this wrapper
// (Start, Restart, and the supervisor's direct startWithConfig calls), so no
// path can forget to persist and a future path inherits it for free.
func (p *ManagedProcess) startWithConfig(ctx context.Context, pending *pendingConfig) error {
	return p.startWithConfigGen(ctx, pending, nil)
}

// startGated launches a gated process whose dependency wait completed (plan 013
// D4). It is the coordinator's launch path: expectGen is the generation the
// coordinator captured when it began resolving; the launch commits only if it
// still matches at the final gate, so a stop/restart/re-demand that superseded
// this completion refuses the launch (errLaunchSuperseded) rather than racing a
// stale process into existence. pending, when non-nil, is a reload swapped in
// ATOMICALLY at the launch gate (plan 013 D6) so a blocked/stopped gated (re)start
// picks up the child's edited cmd/env/stop budget exactly as the ungated path
// does; nil launches with the stored config.
func (p *ManagedProcess) startGated(ctx context.Context, pending *pendingConfig, expectGen uint64) error {
	return p.startWithConfigGen(ctx, pending, &expectGen)
}

// startTask launches a task child whose own depends_on wait completed (plan 013
// D3). Like startGated it enforces the launch generation expectGen at the final
// gate, so a stop/restart/re-demand that superseded this run refuses the launch
// (errLaunchSuperseded). On success it returns the launched instance so the task
// coordinator can watch its exit (inst.exited, pre-drain) for the run-budget race
// and its done channel for the committed terminal state.
func (p *ManagedProcess) startTask(ctx context.Context, expectGen uint64) (*processInstance, error) {
	if err := p.startWithConfigGen(ctx, nil, &expectGen); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current, nil
}

// applyTaskReload swaps a freshly-reloaded task runtime (cmd/env/stop budget/run
// budget) into a NON-running task before a manual re-run (plan 013 D3). It is
// safe to mutate the stored config directly here -- the caller guarantees the
// task is in a terminal state (completed/crashed/stopped), so no run observes a
// torn config. The env loader closure is stored for the fresh-read-on-launch at
// the next start.
func (p *ManagedProcess) applyTaskReload(pending *pendingConfig) {
	if pending == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = pending.config
	p.loadEnv = pending.loadEnv
	p.shutdownTimeout = pending.stopTimeout
	p.taskTimeout = pending.config.TaskTimeout
	p.taskHasTimeout = pending.config.TaskHasTimeout
}

// bumpRestart increments the restart count for a manual task re-run (plan 013
// D3), reusing the same counter surfaced for processes.
func (p *ManagedProcess) bumpRestart() {
	p.mu.Lock()
	p.restartCount++
	p.mu.Unlock()
	// RestartCount is ProcessInfo-visible. Lock discipline: p.mu released above;
	// the caller (executeTask) holds no process/supervisor lock either. The other
	// bump (Restart) is bracketed by the stop half's and start half's own notifies,
	// so it needs no site of its own.
	p.notifyChange()
}

// startWithConfigGen is the shared launch wrapper: it runs the locked launch
// (optionally reloading pending config and/or enforcing a launch generation) and
// persists the orphan-reaping ledger via onLaunched AFTER p.mu is released (see
// startWithConfigLocked's comment on the s.mu -> p.mu ordering).
func (p *ManagedProcess) startWithConfigGen(ctx context.Context, pending *pendingConfig, expectGen *uint64) error {
	err := p.startWithConfigLocked(ctx, pending, expectGen)
	if err == nil && p.onLaunched != nil {
		p.onLaunched()
	}
	// Wake the change bus for every launch attempt. Lock discipline: p.mu is fully
	// released by startWithConfigLocked before this runs (the same reason onLaunched
	// is invoked here). Fired on the error path too, because a failed launch can
	// still have committed a visible state (Crashed on an env-reload or runner
	// failure); a refusal that changed nothing merely costs one spurious wake, which
	// the level latch coalesces away.
	p.notifyChange()
	return err
}

// startWithConfigLocked starts the process, optionally swapping in a
// freshly-reloaded config first. When pending is non-nil the swap (config, env
// loader, effective stop budget) happens inside this locked critical section,
// AFTER both early-return guards (already-running state check,
// surviving-previous-group check) pass and BEFORE the runner launches -- so a
// concurrent Start that wins the race starts the old config and the loser
// returns ErrProcessAlreadyRunning WITHOUT applying the swap. The stored config
// therefore always matches the running process (#33, D3).
//
// If the runner (or the post-swap env reload) fails AFTER the swap was applied,
// the new config stays in place (state Crashed; the next start uses it) -- "the
// file is the truth".
func (p *ManagedProcess) startWithConfigLocked(ctx context.Context, pending *pendingConfig, expectGen *uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Generation guard (plan 013 D4). For a coordinator-driven gated launch,
	// commit only if the process is still on the generation the coordinator
	// captured when it began resolving. A stop/restart/re-demand that superseded
	// this completion bumped waitGen (under p.mu), so this stale launch is refused
	// with no state change -- checked under p.mu, an atomic read only, before any
	// state guard so it can never race a launch into a stopped/blocked process
	// that a concurrent stop already settled. Ungated launches pass expectGen=nil.
	if expectGen != nil && p.waitGen.Load() != *expectGen {
		return errLaunchSuperseded
	}

	// Reject starting over an active run. `stopping` is included so a Start
	// racing an in-flight Stop is refused rather than launching a duplicate
	// while the previous group is still being reaped. The loser exits here
	// before any swap, keeping the stored config consistent with the winner.
	if p.state == domain.ProcessStateRunning ||
		p.state == domain.ProcessStateStarting ||
		p.state == domain.ProcessStateStopping {
		return domain.ErrProcessAlreadyRunning
	}

	// Never launch a duplicate over a surviving previous group. If the prior
	// run's group is still alive (e.g. a prior Stop failed to reap it), refuse
	// rather than orphan it -- the caller must stop it first. Checked before the
	// swap so a refusal here also leaves the stored config untouched.
	if p.current != nil && p.current.proc != nil {
		if alive, _ := p.current.proc.GroupAlive(); alive {
			return fmt.Errorf("%w: %s (previous instance still running; stop it first)",
				domain.ErrProcessGroupNotReaped, p.name)
		}
	}

	// Launch gate (#32/#36, D2). Called here -- inside the p.mu critical section,
	// after the state and surviving-group guards, before the pending-config swap
	// and the runner launch -- so a refusal applies no swap, leaves the state
	// unchanged, and returns the gate's error (ErrShutdownInProgress). The closure
	// reads only an atomic (see createManagedProcess); it must NOT take s.mu, whose
	// s.mu -> p.mu order would AB-BA here.
	//
	// Invariant (D2, deliberately weak): a launcher that OBSERVES the gate closed
	// never launches. A launcher that read the gate open just before Supervisor.Stop
	// flipped it may still launch while holding p.mu -- but that is harmless: Stop
	// flips the gate under s.mu before it spawns the per-process stop goroutines, so
	// this process's stop goroutine, queued on p.mu behind this very launch, reaps
	// the replacement before Supervisor.Stop returns. No replacement outlives
	// Supervisor.Stop. The stronger "cannot launch after Stop began" is NOT claimed;
	// a full launch lease (RWMutex) was considered and rejected -- a new lock class
	// and ordering rules to exclude a transient the invariant already renders
	// harmless (no orphan survives daemon exit).
	if p.launchGate != nil {
		if err := p.launchGate(); err != nil {
			return err
		}
	}

	// Commit point: both guards passed, so this call is the one that launches.
	// Swap in the reloaded runtime now (config, env loader, effective stop
	// budget) -- from here on a failure keeps the new config active.
	if pending != nil {
		p.config = pending.config
		p.loadEnv = pending.loadEnv
		p.shutdownTimeout = pending.stopTimeout
	}

	// Apply the environment for this run.
	//
	//   - Reload path (pending != nil): use the snapshot prepareReload already
	//     loaded AND validated from disk before the stop. Re-reading here would
	//     reopen a window where an env file deleted between preflight and launch
	//     causes downtime, breaking the fail-before-stop promise; it would also
	//     risk applying something other than what was validated. No error path is
	//     needed -- the load already succeeded.
	//   - Non-reload path (up-time start, or ConfigPath unset): read from disk now
	//     via the stored closure so a plain start still picks up current env-file
	//     contents (#30 fresh-read-on-every-start). This must happen before any
	//     per-instance state is created so a failure here leaves no dangling
	//     instance behind.
	if pending != nil {
		p.env = pending.env
	} else if p.loadEnv != nil {
		env, err := p.loadEnv()
		if err != nil {
			p.state = domain.ProcessStateCrashed
			p.logf(domain.StreamStderr, "failed to reload environment: %v", err)
			return fmt.Errorf("%w: %v", domain.ErrEnvReloadFailed, err)
		}
		p.env = env
	}

	p.state = domain.ProcessStateStarting

	// Build a fresh instance for this run.
	inst := &processInstance{done: make(chan struct{}), exited: make(chan struct{})}
	processCtx, cancel := context.WithCancel(ctx)
	inst.cancel = cancel

	proc, err := p.runner.Start(processCtx, p.config, p.env)
	if err != nil {
		p.state = domain.ProcessStateCrashed
		inst.cancel()
		inst.closeDone()
		// Do not publish the failed instance: p.current is left unchanged (the
		// prior dead instance, or nil), preserving the "non-nil current => non-nil
		// proc" invariant.
		return err
	}

	inst.proc = proc
	p.current = inst
	p.startedAt = time.Now()
	p.state = domain.ProcessStateRunning

	// Start output readers, tracked on this instance's WaitGroup.
	inst.outputWg.Add(2)
	go func() {
		defer inst.outputWg.Done()
		p.readOutput(proc.Stdout(), domain.StreamStdout)
	}()
	go func() {
		defer inst.outputWg.Done()
		p.readOutput(proc.Stderr(), domain.StreamStderr)
	}()

	// Start health checker if configured
	if p.config.Healthcheck != nil && p.config.Healthcheck.Cmd != "" {
		// The transition callback is stored here (under p.mu) but only ever FIRED
		// from the checker's own goroutine with h.mu released, so wiring it inside
		// this critical section is safe.
		p.healthChecker = NewHealthChecker(p.config.Name, *p.config.Healthcheck, p.notifyChange)
		p.healthChecker.Start(processCtx)
	}

	// Monitor this specific instance.
	go p.monitor(inst)

	return nil
}

// computeDeadlines derives the graceful (SIGTERM) and kill (SIGKILL/verify)
// deadlines for a Stop from the caller's context.
//
//   - The per-process stop budget (shutdownTimeout, or
//     constants.DefaultShutdownTimeout when unset) is the graceful+kill window.
//     It is the fallback when ctx carries no deadline, AND an upper bound when
//     ctx carries a LATER one: Supervisor.Stop deliberately grants each goroutine
//     StopTimeout + stopVerdictMargin so a stop joining an in-flight primary as a
//     secondary can outlive the primary's finalization window, but that oversized
//     ctx must not stretch THIS process's own escalation past its stop_timeout
//     (#35). An EARLIER ctx deadline (e.g. a short API request) is still honored.
//   - KillGrace is reserved at the tail for the SIGKILL phase, so the graceful
//     phase ends at (deadline - KillGrace).
//   - If the budget is already spent (remaining <= 0) or too small to reserve
//     the kill grace (remaining <= KillGrace), the graceful deadline is "now"
//     and Stop escalates immediately.
//   - killDeadline is always gracefulDeadline + KillGrace. The SIGKILL phase
//     times against killDeadline (its own timer, not ctx) so a near-expired ctx
//     cannot cut the kill/verify short.
func (p *ManagedProcess) computeDeadlines(ctx context.Context) (gracefulDeadline, killDeadline time.Time) {
	now := time.Now()

	// Read the per-process budget exactly ONCE and use it for both the
	// no-deadline fallback and the later-deadline cap below, so there is no
	// double-read window. The budget in force when this stop began governs
	// escalation; a caller ctx with a LATER deadline is only an outer bound and
	// cannot stretch escalation past stop_timeout (#35). A config swap cannot
	// occur mid-stop -- startWithConfig only swaps while a start is permitted, and
	// this stop has already set state=Stopping, which blocks startWithConfig -- so
	// this value is stable for the whole stop; it is exactly the budget the
	// running (or replacement) process was launched under, the correct governor.
	budget := p.StopTimeout()
	if budget <= 0 {
		budget = constants.DefaultShutdownTimeout
	}
	budgetDeadline := now.Add(budget)

	dl, ok := ctx.Deadline()
	if !ok || dl.After(budgetDeadline) {
		dl = budgetDeadline
	}

	// If the budget is already spent (remaining <= 0) or too small to reserve
	// the kill grace (remaining <= KillGrace), escalate immediately -- the
	// former is a subset of the latter since KillGrace is positive.
	if remaining := time.Until(dl); remaining <= constants.KillGrace {
		gracefulDeadline = now
	} else {
		gracefulDeadline = dl.Add(-constants.KillGrace)
	}
	killDeadline = gracefulDeadline.Add(constants.KillGrace)
	return gracefulDeadline, killDeadline
}

// Stop stops the process gracefully, escalating to SIGKILL if the group does
// not exit within the graceful window, and reports an error only if the group
// truly survives. See design doc D3. A clean reap settles the process in
// stopped.
func (p *ManagedProcess) Stop(ctx context.Context) error {
	return p.stop(ctx, domain.ProcessStateStopped)
}

// stopTask stops a task's run and settles it in CRASHED rather than stopped
// (plan 013 D3). It is the run-timeout escalation path: the run budget already
// expired, so the child is signalled (SIGTERM->SIGKILL) exactly like a normal
// stop -- reusing the group-reap machinery -- but the terminal verdict is
// crashed, since a timed-out task did not complete.
func (p *ManagedProcess) stopTask(ctx context.Context) error {
	return p.stop(ctx, domain.ProcessStateCrashed)
}

// stop is the shared stop implementation. cleanState is the terminal state
// committed when the group is confirmed reaped (stopped for a normal stop,
// crashed for a task run-timeout). A surviving group always commits crashed
// regardless; the error paths are unchanged.
func (p *ManagedProcess) stop(ctx context.Context, cleanState domain.ProcessState) (retErr error) {
	// Wake the change bus once this stop has settled, whichever of the many exits
	// it takes (waiting->stopped, blocked->stopped, the early no-ops, and the final
	// verdict commit). Registered as the FIRST defer so it runs LAST -- after the
	// deferred episode backstop below, hence with p.mu released. Not every path
	// changes state; a spurious wake just costs the subscriber one re-snapshot.
	defer p.notifyChange()

	p.mu.Lock()

	switch p.state {
	case domain.ProcessStateCompleted:
		// A finished task (plan 013 D3). Normally there is nothing to reap -- a
		// stop/down of a completed task is a no-op success, and it must NOT be
		// reclassified to stopped. But if a grandchild outlived the leader's exit
		// its group may still be alive; fall through to reap it (retry path),
		// mirroring the stopped/crashed surviving-group case below.
		if p.current == nil || p.current.proc == nil {
			p.mu.Unlock()
			return nil
		}
		if alive, _ := p.current.proc.GroupAlive(); !alive {
			p.mu.Unlock()
			return nil
		}
		// Surviving group: fall through to reap it.
	case domain.ProcessStateWaiting:
		// Gated process whose dependency wait is still pending (or just resolved
		// but not yet launched). Supersede any resolved-but-unlaunched coordinator
		// completion (bump the generation so its final gate refuses), cancel this
		// process's wait so the coordinator's Demand joins unblock, and settle in
		// stopped. There is no process group to reap. Plan 013 D4.
		p.waitGen.Add(1)
		cancel := p.waitCancel
		p.waitCancel = nil
		p.state = domain.ProcessStateStopped
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		// A real transition happened (the scheduled launch was canceled), so this
		// is a clean stop, not an ErrProcessNotRunning no-op.
		return nil
	case domain.ProcessStateBlocked:
		// Terminal gated state, no wait in flight and no group. Settle in stopped
		// so a later start can re-schedule it. Plan 013 D4.
		p.state = domain.ProcessStateStopped
		p.blockedBy = nil
		p.mu.Unlock()
		return nil
	case domain.ProcessStateStopped, domain.ProcessStateCrashed:
		// Nothing running, unless a group survived a prior stop / unexpected
		// leader exit -- in which case fall through and reap it (retry path),
		// installing a fresh episode below.
		if p.current == nil || p.current.proc == nil {
			p.mu.Unlock()
			return domain.ErrProcessNotRunning
		}
		if alive, _ := p.current.proc.GroupAlive(); !alive {
			p.mu.Unlock()
			return domain.ErrProcessNotRunning
		}
		// Surviving group: fall through to reap it.
	case domain.ProcessStateStopping:
		// A stop is already in flight. Wait for the PRIMARY's published verdict
		// via the stop episode -- NOT merely the run monitor's leader-reaped
		// signal -- so two concurrent Stops on the same process return the same
		// result when both caller contexts stay live through episode completion
		// (#32, D1). A caller whose context dies first still gets ctx.Err().
		ep := p.episode
		inst := p.current
		barrier := p.stopBarrier
		p.mu.Unlock()

		if ep == nil {
			// Defensive: a Stopping state should always carry an episode (every
			// transition installs one), so with the atomic commit+resolution above
			// this branch is unreachable. If it ever isn't, wait on the run monitor
			// and then take a fresh authoritative probe -- mirroring the primary's
			// verdict logic -- rather than blindly reporting success.
			if inst == nil || inst.proc == nil {
				return nil
			}
			select {
			case <-inst.done:
				if alive, _ := inst.proc.GroupAlive(); alive {
					return fmt.Errorf("%w: %s", domain.ErrProcessGroupNotReaped, p.name)
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if barrier != nil {
			barrier("secondary-joined")
		}

		select {
		case <-ep.done:
			// The err write happens-before close(ep.done), so no lock is needed.
			return ep.err
		case <-ctx.Done():
			// Simultaneous readiness: re-check the episode non-blocking and prefer
			// a published verdict over ctx.Err(), so a real result is never
			// dropped when done and ctx fire at once (#32, D1).
			select {
			case <-ep.done:
				return ep.err
			default:
				return ctx.Err()
			}
		}
	}

	// Primary path: commit the Stopping transition and install a fresh episode to
	// carry this call's verdict to any secondary waiter. The episode is resolved
	// in-line with each terminal-state commit below (early-exit and the final
	// verdict), inside the SAME p.mu critical section, so the two are observed
	// atomically. The deferred backstop only fires if a recovered panic skipped
	// that resolution (#32, D1).
	p.state = domain.ProcessStateStopping
	ep := &stopEpisode{done: make(chan struct{})}
	p.episode = ep

	inst := p.current
	healthChecker := p.healthChecker
	p.healthChecker = nil
	barrier := p.stopBarrier
	p.mu.Unlock()

	// The Stopping transition is itself ProcessInfo-visible and can precede the
	// terminal verdict by the whole stop budget, so surface it now rather than
	// letting the stream jump straight from running to stopped. Lock discipline:
	// p.mu released immediately above.
	p.notifyChange()

	defer func() { p.backstopStopEpisode(ep, inst, retErr) }()

	if barrier != nil {
		barrier("primary-installed")
	}

	// Stop the health checker for this run.
	if healthChecker != nil {
		healthChecker.Stop()
	}

	if inst == nil || inst.proc == nil {
		p.mu.Lock()
		p.state = cleanState
		p.resolveStopEpisodeLocked(ep, nil)
		p.mu.Unlock()
		return nil
	}

	gracefulDeadline, killDeadline := p.computeDeadlines(ctx)

	// Send SIGTERM to the whole group.
	if err := inst.proc.Signal(sigterm); err != nil {
		p.logf(domain.StreamStderr, "SIGTERM failed (process may have already exited): %v", err)
	}

	// Graceful phase: poll group liveness until it is gone or the graceful
	// deadline passes. Escalation is purely time-based -- we deliberately do
	// NOT key off done/output-drain, so a slow-but-graceful grandchild that
	// takes longer than the poll interval is not killed early.
	gone := waitGroupGone(inst.proc, gracefulDeadline)

	// Escalation: if still alive at the graceful deadline, SIGKILL the group
	// and poll again until it is gone or the kill deadline passes.
	if !gone {
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "system",
			Stream:    domain.StreamStdout,
			Line:      fmt.Sprintf("sending SIGKILL to %s (graceful shutdown timed out)", p.name),
		})
		if err := inst.proc.Signal(sigkill); err != nil {
			p.logf(domain.StreamStderr, "SIGKILL failed: %v", err)
		}
		// Poll until the group is gone or the kill deadline passes. The return
		// value is intentionally discarded: the authoritative verdict below
		// re-probes after the finalization gate has reaped the leader (a zombie
		// leader keeps the group "alive" until reaped). This call exists for its
		// bounded wait, giving the group time to disappear after SIGKILL.
		waitGroupGone(inst.proc, killDeadline)
	}

	// Finalization gate (fixes the restart-clobber race): wait for this run's
	// monitor to finish its critical section and reap the leader before any
	// replacement can start. The cap is a safety net; after SIGKILL the leader
	// is dead and done closes promptly.
	select {
	case <-inst.done:
	case <-time.After(outputDrainTimeout + time.Second):
	}

	// Test seam: park inside the finalization window, AFTER the monitor-drain
	// wait and BEFORE the authoritative verdict/state commit, so a test can hold a
	// primary's verdict late and prove a concurrent secondary waiter (sized by
	// Supervisor.Stop's stopVerdictMargin) outlives exactly this tail (#32/#36).
	if barrier != nil {
		barrier("finalizing")
	}

	// Verdict: a fresh probe now that the leader has been reaped is
	// authoritative.
	alive, _ := inst.proc.GroupAlive()

	var verdict error
	if alive {
		verdict = fmt.Errorf("%w: %s", domain.ErrProcessGroupNotReaped, p.name)
	}

	p.mu.Lock()
	if p.current == inst {
		if alive {
			// Group survived: keep current so a later stop/restart can retry.
			p.state = domain.ProcessStateCrashed
		} else {
			p.state = cleanState
		}
	}
	if inst.cancel != nil {
		inst.cancel()
	}
	// Resolve the episode in the SAME critical section that commits the terminal
	// state, so any goroutine observing the state under p.mu also observes the
	// published verdict -- no torn window between commit and resolution.
	p.resolveStopEpisodeLocked(ep, verdict)
	p.mu.Unlock()

	if barrier != nil {
		barrier("verdict-committed")
	}

	return verdict
}

// resolveStopEpisodeLocked resolves a stop episode exactly once. The caller MUST
// hold p.mu, and MUST call it in the same critical section that commits the
// terminal state verdict, so the two are observed atomically (see stopEpisode's
// invariant). It records the verdict, detaches the episode (only if p.episode
// still points at ep -- a later Stop may already have installed a newer one),
// and closes ep.done so waiters observe the result. Idempotent: a second call
// (e.g. the deferred panic backstop after the explicit resolution) is a no-op.
// Writing err before closing done gives waiters a lock-free read.
func (p *ManagedProcess) resolveStopEpisodeLocked(ep *stopEpisode, err error) {
	if ep.resolved {
		return
	}
	ep.resolved = true
	ep.err = err
	if p.episode == ep {
		p.episode = nil
	}
	close(ep.done)
}

// backstopStopEpisode is the deferred panic backstop for a primary Stop. On every
// normal exit the episode is already resolved in-line with the state commit, so
// this is a no-op. It only acts when a (recovered) panic skipped the explicit
// resolution: it publishes a verdict so waiters never hang and -- if the process
// is still stranded mid-stop (state Stopping, this run still current) -- moves it
// to Crashed so the group stays reapable by a retry. Unrecovered panics kill the
// process anyway; this is belt-and-braces for recovered ones.
func (p *ManagedProcess) backstopStopEpisode(ep *stopEpisode, inst *processInstance, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep.resolved {
		return
	}
	if p.state == domain.ProcessStateStopping && p.current == inst {
		p.state = domain.ProcessStateCrashed
	}
	if err == nil {
		err = fmt.Errorf("%w: %s (stop aborted before verdict)", domain.ErrProcessGroupNotReaped, p.name)
	}
	p.resolveStopEpisodeLocked(ep, err)
}

// waitGroupGone polls proc's group liveness on groupPollInterval until the
// group is gone (GroupAlive returns false) or deadline passes. A probe error
// is treated conservatively as "still alive", so a transient probe failure
// never masquerades as a successful reap. It returns true iff the group is gone.
func waitGroupGone(proc Process, deadline time.Time) bool {
	ticker := time.NewTicker(groupPollInterval)
	defer ticker.Stop()

	for {
		if alive, _ := proc.GroupAlive(); !alive {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		<-ticker.C
	}
}

// Restart restarts the process.
//
// stopCtx and startCtx are intentionally distinct: stopCtx bounds the graceful
// stop of the current instance (typically a short-lived, request/timeout
// context), while startCtx becomes the lifecycle context for the replacement
// instance's process/health-checker and must be long-lived (e.g. a
// supervisor's own context). Using a request-scoped context for startCtx
// would cancel the replacement's health checker and lifecycle context as
// soon as the request context is cancelled/expires (see RestartProcess).
//
// If Stop fails (e.g. the old group could not be reaped) Restart aborts before
// Start, so a surviving group is never shadowed by a replacement.
//
// pending, when non-nil, carries a freshly-reloaded config swapped in atomically
// by the replacement's start half (see startWithConfig); nil restarts with the
// stored config. The stop half runs before the swap, so it uses the OLD stored
// stop budget -- a raised stop_timeout only governs the next stop (#33, D3).
func (p *ManagedProcess) Restart(stopCtx, startCtx context.Context, pending *pendingConfig) error {
	if err := p.Stop(stopCtx); err != nil && err != domain.ErrProcessNotRunning {
		return err
	}

	p.mu.Lock()
	p.restartCount++
	barrier := p.restartStartBarrier
	p.mu.Unlock()

	// Test-only seam: hold the restart in the unlocked gap between its stop and
	// its start half so an interleaving test can race a concurrent start in.
	if barrier != nil {
		barrier()
	}

	return p.startWithConfig(startCtx, pending)
}

// monitor watches a single instance for exit. It owns draining that instance's
// output and closing its done channel, but only the live instance
// (generation guard) may mutate ManagedProcess state.
func (p *ManagedProcess) monitor(inst *processInstance) {
	err := inst.proc.Wait()

	// Claim the terminal outcome and signal EXIT before draining (plan 013 D3,
	// fix 6). claimedExit is false only when a task's run-budget timer already
	// claimed a timeout; in that case the natural-exit commit below is skipped so
	// stopTask (already escalating) owns the crashed verdict. markExited unblocks
	// the run-budget timer's exit race immediately, well before a slow grandchild
	// drain would otherwise settle done.
	claimedExit := inst.claimTerminal(claimExit)
	inst.markExited()

	// Tear down THIS instance's context the instant the leader is gone (#107).
	// Before this, the only cancels were the launch-failure cleanup and stop(),
	// so a process that crashed on its own left its health checker ticking --
	// re-executing the user's check command against a dead pid, with whatever
	// side effects that command has, until some later Stop happened to arrive
	// (and reporting `"healthcheck": {"enabled": true}` the whole time).
	//
	// WHY HERE, before the output-drain wait, and not at the terminal-state
	// commit below: the drain can take up to outputDrainTimeout (5s) when a
	// grandchild is holding the pipe open, and cancelling at the commit point
	// would keep the checker shelling out for that entire window. From the
	// instant inst.proc.Wait() returns, the leader is gone and the health
	// checker is meaningless.
	//
	// WHY IT IS SAFE:
	//   - The only consumers of processCtx are healthChecker.Start and
	//     runner.Start; the production ExecRunner explicitly ignores its ctx
	//     (lifecycle is driven via Signal, see runner.go), so cancelling kills
	//     nothing.
	//   - Output draining is tracked on inst.outputWg, NOT on the context, so
	//     this cannot truncate log capture -- the drain wait below is unaffected.
	//   - Teardown is by inst.proc.Signal(...) in stop(). Cancelling sends no
	//     signal and disturbs no pgid.
	//   - Cancelling is NOT the same as releasing p.current: the instance (and
	//     therefore the pgid) is deliberately retained below, so a surviving
	//     group stays reapable by a later Stop. Only the health loop stops.
	//   - It touches only this instance's context. A restart installs a fresh
	//     HealthChecker on a fresh processCtx, so a stale monitor cancels only
	//     its own dead checker.
	//   - stop()'s later inst.cancel() becomes a no-op; context.CancelFunc is
	//     idempotent by contract.
	if inst.cancel != nil {
		inst.cancel()
	}

	// Wait for this instance's output readers to finish draining pipes with a
	// timeout. With manual pipes (not cmd.StdoutPipe), the pipes stay open
	// until all processes (including grandchildren) close them, so graceful
	// shutdown messages from child processes are captured -- but a grandchild
	// holding the pipe open must not block us forever.
	outputDone := make(chan struct{})
	go func() {
		inst.outputWg.Wait()
		close(outputDone)
	}()

	drainTimedOut := false
	select {
	case <-outputDone:
		// Output readers finished normally
	case <-time.After(outputDrainTimeout):
		drainTimedOut = true
	}

	exitCode := exitCodeFromWaitErr(err)

	p.mu.Lock()
	// prevState is sampled under the lock so the natural-exit commit below can be
	// detected exactly (see the notify after the unlock).
	prevState := p.state
	// Generation guard: a stale monitor -- one whose instance has already been
	// replaced by a newer Start -- must touch nothing shared and emit no
	// (misleading) logs (drain-timeout notice or exit code) attributed to a run
	// that is no longer current. It only closes its own done below.
	if p.current == inst {
		if drainTimedOut {
			p.logf(domain.StreamStderr, "output capture timed out (some logs may be missing)")
		}
		switch p.state {
		case domain.ProcessStateStopping:
			// Stop owns the transition to stopped; here we only record the exit
			// code. Stop's finalization gate normally waits on inst.done, so
			// this log precedes Stop's final verdict.
			p.logf(domain.StreamStdout, "stopped (rc=%d)", exitCode)
		case domain.ProcessStateRunning, domain.ProcessStateStarting:
			// A leader exit not driven by Stop. For a TASK (plan 013 D3), a natural
			// exit 0 is success -> completed (uptime frozen here); any non-zero or
			// signal exit -> crashed. A plain process ALWAYS crashes on a leader exit,
			// rc=0 included (a service is not supposed to exit) -- this asymmetry is
			// deliberately gated on kind so plain-process semantics are byte-for-byte
			// unchanged. Surviving grandchildren (a group that outlives the leader)
			// go through the existing orphan-reaping machinery via the retained
			// p.current below and Stop's surviving-group path.
			switch {
			case !claimedExit:
				// A task run-budget timer claimed a timeout before this exit was
				// recorded (fix 6): stopTask owns the crashed verdict, so do NOT commit
				// a natural-exit state here. Reachable only for a task.
			case p.kind == domain.ProcessKindTask && exitCode == 0:
				p.state = domain.ProcessStateCompleted
				p.completedAt = time.Now()
				p.logf(domain.StreamStdout, "task completed (rc=0)")
			default:
				p.state = domain.ProcessStateCrashed
				p.logf(domain.StreamStderr, "exited unexpectedly (rc=%d)", exitCode)
			}
		default:
			// State is already terminal (stopped/crashed) -- Stop committed a
			// verdict before this monitor ran (only reachable if Stop's
			// finalization gate timed out instead of receiving inst.done). Do
			// NOT override it: Stop owns the terminal state.
		}
	}
	// Deliberately do NOT null p.current: retaining it keeps the pgid reapable
	// for a retry Stop. Note this is untouched by the instance-context cancel
	// above (#107) -- cancelling the context is NOT releasing the instance: it
	// stops the health loop and nothing else, so the retained pgid stays exactly
	// as reapable as it was before.
	stateChanged := p.state != prevState
	p.mu.Unlock()

	// Natural exit / task completion: this is the only path that commits
	// completed or crashed without any supervisor-level emit, so it must wake the
	// bus itself. Lock discipline: p.mu released immediately above; monitor holds
	// nothing else. Gated on an actual transition so the far more common
	// stop-driven exits (where Stop owns the verdict and its own notify) do not
	// double-wake.
	if stateChanged {
		p.notifyChange()
	}

	inst.closeDone()
}

// exitCodeFromWaitErr derives an exit code from a Wait() error. Signal
// termination is reported as the negative signal number (e.g. -15 for SIGTERM).
func exitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return -int(status.Signal())
			}
			return status.ExitStatus()
		}
		return exitErr.ExitCode()
	}
	return 1 // Generic error
}

// logf writes a formatted log line attributed to this process on the given
// stream. It is the per-process mirror of Supervisor.SystemLog.
func (p *ManagedProcess) logf(stream domain.Stream, format string, args ...interface{}) {
	p.logManager.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   p.name,
		Stream:    stream,
		Line:      fmt.Sprintf(format, args...),
	})
}

// readOutput reads from a stream and writes to the log manager
func (p *ManagedProcess) readOutput(r interface{}, stream domain.Stream) {
	reader, ok := r.(interface{ Read([]byte) (int, error) })
	if !ok || reader == nil {
		return
	}

	scanner := bufio.NewScanner(reader)
	// Increase buffer size for long lines
	scanner.Buffer(make([]byte, constants.ScannerBufferSize), constants.ScannerMaxBufferSize)

	for scanner.Scan() {
		line := scanner.Text()
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.name,
			Stream:    stream,
			Line:      line,
		})
	}

	// Log any scanner errors (e.g., I/O errors during output capture)
	if err := scanner.Err(); err != nil {
		p.logf(domain.StreamStderr, "output reader error: %v", err)
	}
}
