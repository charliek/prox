package supervisor

import (
	"bufio"
	"context"
	"fmt"
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

// ManagedProcess handles the lifecycle of a single process
type ManagedProcess struct {
	mu sync.RWMutex

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

	// shutdownTimeout overrides constants.DefaultShutdownTimeout for the
	// no-context-deadline fallback in computeDeadlines. It is a test seam only
	// (kept small so the no-deadline escalation path runs fast); production
	// code leaves it zero, which means "use constants.DefaultShutdownTimeout".
	shutdownTimeout time.Duration
}

// NewManagedProcess creates a new managed process
func NewManagedProcess(config domain.ProcessConfig, env map[string]string, runner ProcessRunner, logManager *logs.Manager) *ManagedProcess {
	return &ManagedProcess{
		config:     config,
		env:        env,
		runner:     runner,
		logManager: logManager,
		state:      domain.ProcessStateStopped,
	}
}

// Name returns the process name
func (p *ManagedProcess) Name() string {
	return p.config.Name
}

// Config returns the process configuration
func (p *ManagedProcess) Config() domain.ProcessConfig {
	return p.config
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

// Start starts the process
func (p *ManagedProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reject starting over an active run. `stopping` is included so a Start
	// racing an in-flight Stop is refused rather than launching a duplicate
	// while the previous group is still being reaped.
	if p.state == domain.ProcessStateRunning ||
		p.state == domain.ProcessStateStarting ||
		p.state == domain.ProcessStateStopping {
		return domain.ErrProcessAlreadyRunning
	}

	// Reload the environment from disk on every Start (covers both `start`
	// after `stop` and the replacement half of `restart`). This must happen
	// before any per-instance state is created so a failure here leaves no
	// dangling instance behind.
	if p.loadEnv != nil {
		env, err := p.loadEnv()
		if err != nil {
			p.state = domain.ProcessStateCrashed
			p.logf(domain.StreamStderr, "failed to reload environment: %v", err)
			return fmt.Errorf("%w: %v", domain.ErrEnvReloadFailed, err)
		}
		p.env = env
	}

	// Never launch a duplicate over a surviving previous group. If the prior
	// run's group is still alive (e.g. a prior Stop failed to reap it), refuse
	// rather than orphan it -- the caller must stop it first.
	if p.current != nil && p.current.proc != nil {
		if alive, _ := p.current.proc.GroupAlive(); alive {
			return fmt.Errorf("%w: %s (previous instance still running; stop it first)",
				domain.ErrProcessGroupNotReaped, p.config.Name)
		}
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
		timeout := p.shutdownTimeout
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
func (p *ManagedProcess) Stop(ctx context.Context) error {
	p.mu.Lock()

	switch p.state {
	case domain.ProcessStateStopped, domain.ProcessStateCrashed:
		// Nothing running, unless a group survived a prior stop / unexpected
		// leader exit -- in which case fall through and reap it (retry path).
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
		// A stop is already in flight; wait for it (or ctx) on the in-flight
		// run's done channel.
		//
		// Known limitation: inst.done closing means the run's monitor finished
		// (leader reaped), not that the PRIMARY Stop finished its group polling
		// and verdict. So a concurrent secondary Stop can return nil slightly
		// before the primary's verdict, and -- only in the doubly-rare case of
		// two concurrent Stops on the same process AND a genuinely unreapable
		// group -- can report success while the primary returns
		// ErrProcessGroupNotReaped. The common (killable) case is consistent:
		// both return nil. Serializing concurrent same-process stops (or
		// propagating the primary's result to waiters) is tracked as future
		// work; the primary always reaps/reports correctly.
		inst := p.current
		p.mu.Unlock()
		if inst == nil {
			return nil
		}
		select {
		case <-inst.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	p.state = domain.ProcessStateStopping
	inst := p.current
	healthChecker := p.healthChecker
	p.healthChecker = nil
	p.mu.Unlock()

	// Stop the health checker for this run.
	if healthChecker != nil {
		healthChecker.Stop()
	}

	if inst == nil || inst.proc == nil {
		p.mu.Lock()
		p.state = domain.ProcessStateStopped
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
			Line:      fmt.Sprintf("sending SIGKILL to %s (graceful shutdown timed out)", p.config.Name),
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
	p.mu.Unlock()

	if alive {
		return fmt.Errorf("%w: %s", domain.ErrProcessGroupNotReaped, p.config.Name)
	}
	return nil
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
func (p *ManagedProcess) Restart(stopCtx, startCtx context.Context) error {
	if err := p.Stop(stopCtx); err != nil && err != domain.ErrProcessNotRunning {
		return err
	}

	p.mu.Lock()
	p.restartCount++
	p.mu.Unlock()

	return p.Start(startCtx)
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

	select {
	case <-outputDone:
		// Output readers finished normally
	case <-time.After(outputDrainTimeout):
		p.logf(domain.StreamStderr, "output capture timed out (some logs may be missing)")
	}

	exitCode := exitCodeFromWaitErr(err)

	p.mu.Lock()
	// Generation guard: a stale monitor -- one whose instance has already been
	// replaced by a newer Start -- must touch nothing shared and emit no
	// (misleading) exit log. It only closes its own done below.
	if p.current == inst {
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
		Process:   p.config.Name,
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
			Process:   p.config.Name,
			Stream:    stream,
			Line:      line,
		})
	}

	// Log any scanner errors (e.g., I/O errors during output capture)
	if err := scanner.Err(); err != nil {
		p.logf(domain.StreamStderr, "output reader error: %v", err)
	}
}
