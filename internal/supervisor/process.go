package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"os/exec"
	"sync"
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

	config     domain.ProcessConfig
	env        map[string]string
	runner     ProcessRunner
	logManager *logs.Manager

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

	// stopBarrier is a test-only seam (nil in production). Stop invokes it,
	// unlocked, at two interleaving-sensitive points identified by phase:
	//
	//   - "primary-installed": in the primary path just after the stop episode is
	//     installed and the lock released (before any signalling/verdict work), so
	//     a test can hold the primary open with its episode published.
	//   - "secondary-joined": in the Stopping branch just after a secondary
	//     captured the in-flight episode and released the lock (before it waits on
	//     the verdict), so a test can confirm the join happened.
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
}

// NewManagedProcess creates a new managed process
func NewManagedProcess(config domain.ProcessConfig, env map[string]string, runner ProcessRunner, logManager *logs.Manager) *ManagedProcess {
	return &ManagedProcess{
		name:       config.Name,
		config:     config,
		env:        env,
		runner:     runner,
		logManager: logManager,
		state:      domain.ProcessStateStopped,
	}
}

// Name returns the process name. Immutable, so no lock is taken.
func (p *ManagedProcess) Name() string {
	return p.name
}

// Config returns a deep copy of the process configuration. It takes the read
// lock (config is swappable on a reload -- #33, D3) and clones the reference
// fields (Healthcheck pointer, Env map) so callers cannot observe a torn value
// or mutate the live config.
func (p *ManagedProcess) Config() domain.ProcessConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cfg := p.config
	if p.config.Healthcheck != nil {
		hc := *p.config.Healthcheck
		cfg.Healthcheck = &hc
	}
	// maps.Clone returns nil for a nil map, preserving the nil-vs-empty distinction.
	cfg.Env = maps.Clone(p.config.Env)
	return cfg
}

// Info returns the current process info
func (p *ManagedProcess) Info() domain.ProcessInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	info := domain.ProcessInfo{
		Name:         p.config.Name,
		State:        p.state,
		RestartCount: p.restartCount,
		Health:       domain.HealthStatusUnknown,
		Cmd:          p.config.Cmd,
		Env:          p.env,
		StopTimeout:  p.shutdownTimeout,
	}

	// PID is only meaningful while the process is active. Once stopped or
	// crashed we report 0 even though current is retained for reap/retry.
	if !p.state.IsStopped() && p.current != nil && p.current.proc != nil {
		info.PID = p.current.proc.PID()
	}

	if !p.startedAt.IsZero() {
		info.StartedAt = p.startedAt
	}

	// Include health check state if checker exists
	if p.healthChecker != nil {
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

// StopTimeout returns the effective per-process stop budget used as the
// no-context-deadline fallback in computeDeadlines.
func (p *ManagedProcess) StopTimeout() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.shutdownTimeout
}

// Start starts the process with its currently-stored config.
func (p *ManagedProcess) Start(ctx context.Context) error {
	return p.startWithConfig(ctx, nil)
}

// startWithConfig starts the process, optionally swapping in a freshly-reloaded
// config first. When pending is non-nil the swap (config, env loader, effective
// stop budget) happens inside this locked critical section, AFTER both
// early-return guards (already-running state check, surviving-previous-group
// check) pass and BEFORE the runner launches -- so a concurrent Start that wins
// the race starts the old config and the loser returns ErrProcessAlreadyRunning
// WITHOUT applying the swap. The stored config therefore always matches the
// running process (#33, D3).
//
// If the runner (or the post-swap env reload) fails AFTER the swap was applied,
// the new config stays in place (state Crashed; the next start uses it) -- "the
// file is the truth".
func (p *ManagedProcess) startWithConfig(ctx context.Context, pending *pendingConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

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
	inst := &processInstance{done: make(chan struct{})}
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
		p.healthChecker = NewHealthChecker(p.config.Name, *p.config.Healthcheck)
		p.healthChecker.Start(processCtx)
	}

	// Monitor this specific instance.
	go p.monitor(inst)

	return nil
}

// computeDeadlines derives the graceful (SIGTERM) and kill (SIGKILL/verify)
// deadlines for a Stop from the caller's context.
//
//   - If ctx has no deadline, a fallback of shutdownTimeout (or
//     constants.DefaultShutdownTimeout when unset) is used.
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

	dl, ok := ctx.Deadline()
	if !ok {
		timeout := p.StopTimeout()
		if timeout <= 0 {
			timeout = constants.DefaultShutdownTimeout
		}
		dl = now.Add(timeout)
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
// truly survives. See design doc D3.
func (p *ManagedProcess) Stop(ctx context.Context) (retErr error) {
	p.mu.Lock()

	switch p.state {
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
		p.state = domain.ProcessStateStopped
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
			p.state = domain.ProcessStateStopped
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
			// Genuine unexpected exit (crash / natural exit not driven by Stop).
			p.state = domain.ProcessStateCrashed
			p.logf(domain.StreamStderr, "exited unexpectedly (rc=%d)", exitCode)
		default:
			// State is already terminal (stopped/crashed) -- Stop committed a
			// verdict before this monitor ran (only reachable if Stop's
			// finalization gate timed out instead of receiving inst.done). Do
			// NOT override it: Stop owns the terminal state.
		}
	}
	// Deliberately do NOT null p.current: retaining it keeps the pgid reapable
	// for a retry Stop.
	p.mu.Unlock()

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
