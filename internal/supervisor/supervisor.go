package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

// stopVerdictMargin is the slack added to each per-process stop budget when
// Supervisor.Stop sizes its goroutine contexts (and the outer bound up.go grants
// via StopWaitBound). A Supervisor.Stop joining an in-flight primary stop must
// outlive the primary's finalization window: the primary's verdict can land up
// to ~budget + outputDrainTimeout + 1s after the primary began (see the
// finalization gate in process.go). Sizing the secondary at budget +
// stopVerdictMargin keeps it alive through that tail so it aggregates the real
// PROCESS_GROUP_NOT_REAPED verdict instead of ctx-expiring first (#36, D3).
const stopVerdictMargin = outputDrainTimeout + 2*time.Second

// SupervisorConfig holds configuration for the supervisor
type SupervisorConfig struct {
	ShutdownTimeout time.Duration
	ConfigDir       string // Directory containing the config file (for resolving relative paths)
	// ConfigPath is the absolute path to prox.yaml. When set, an API-driven
	// (re)start re-reads and validates the file and applies the target
	// process's current config before launching it (#33, D3). Empty disables
	// reload entirely (tests that construct a Supervisor directly), preserving
	// back-compat: the up-time config is used as-is.
	ConfigPath string
	// StateDir is the absolute path to this project's .prox state directory. When
	// set, every successful launch persists the orphan-reaping ownership ledger
	// (<StateDir>/prox.children) so a later `prox up` can reap groups a SIGKILL'd
	// generation orphaned (plan 009 D11-D13, #59). Empty disables persistence
	// (tests that construct a Supervisor directly), a clean no-op.
	StateDir string
}

// DefaultSupervisorConfig returns default configuration
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		ShutdownTimeout: constants.DefaultShutdownTimeout,
	}
}

// Supervisor manages multiple processes.
// It coordinates starting, stopping, and monitoring all configured processes.
type Supervisor struct {
	mu sync.RWMutex

	// config holds the process configuration as loaded at `up` time. It is an
	// immutable snapshot: per-process API-driven reloads (#33, D3) re-read the
	// file into a fresh config used only for that process's swap and deliberately
	// do NOT update this field. Consequently a whole-supervisor Stop-then-Start
	// rebuilds every process from the ORIGINAL up-time config, not the latest
	// file (only reachable in tests today; a full-config hot reload is future
	// work).
	config *config.Config
	// supConfig holds supervisor-specific settings like shutdown timeout
	supConfig SupervisorConfig
	// processes maps process names to their managed process instances
	processes map[string]*ManagedProcess
	// runner handles the actual process execution (can be mocked for testing)
	runner ProcessRunner
	// logManager handles log collection and subscription
	logManager *logs.Manager

	// startedAt records when the supervisor was started
	startedAt time.Time
	// state is the current supervisor state: "stopped", "running", or "stopping"
	state string

	// launchable is the launch gate (#32/#36, D2): true while the supervisor is
	// running, flipped false at the top of Stop (under s.mu, alongside
	// state="stopping") before the per-process stop goroutines are created, and
	// reset true on every entry into "running" so a stop->start cycle reopens the
	// gate. createManagedProcess injects a launchGate closure that reads this flag
	// (atomic read only -- see the closure comment) and refuses a launch with
	// ErrShutdownInProgress once it is false. It is an atomic (not guarded by s.mu)
	// so the gate can be read inside startWithConfig's p.mu critical section without
	// taking s.mu, which would AB-BA against the verified s.mu -> p.mu order.
	launchable atomic.Bool

	// ctx and cancel are used for coordinating graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// eventMu protects eventSubs from concurrent access
	eventMu sync.RWMutex
	// eventSubs holds channels for subscribers to supervisor events
	eventSubs []chan SupervisorEvent

	// childrenMu serializes writes to the orphan-reaping ownership ledger
	// (<supConfig.StateDir>/prox.children). Holding it across BOTH the s.mu
	// snapshot and the file write gives last-writer-correctness under the parallel
	// launches: each serialized persist snapshots the CURRENT live set, so the
	// final write reflects reality even when starts race. Lock order is
	// childrenMu -> s.mu (persistChildren); no path takes them the other way.
	childrenMu sync.Mutex

	// bootMarker is this host's boot identity (plan 010 D7, #67), read ONCE at
	// construction and stamped into every ledger write so the next generation can
	// detect and safely discard a cross-boot ledger. Empty on Darwin/others (not
	// needed) and on a Linux boot_id read failure (degrades to markerless).
	bootMarker string
}

// SupervisorEvent represents a supervisor event
type SupervisorEvent struct {
	Type      EventType
	Process   string
	Timestamp time.Time
	Info      domain.ProcessInfo
}

// EventType defines the type of supervisor event
type EventType string

const (
	EventTypeProcessStarted  EventType = "process_started"
	EventTypeProcessStopped  EventType = "process_stopped"
	EventTypeProcessCrashed  EventType = "process_crashed"
	EventTypeSupervisorStart EventType = "supervisor_start"
	EventTypeSupervisorStop  EventType = "supervisor_stop"
)

// New creates a new supervisor
func New(cfg *config.Config, logManager *logs.Manager, runner ProcessRunner, supConfig SupervisorConfig) *Supervisor {
	if runner == nil {
		runner = NewExecRunner()
	}

	s := &Supervisor{
		config:     cfg,
		supConfig:  supConfig,
		processes:  make(map[string]*ManagedProcess),
		runner:     runner,
		logManager: logManager,
		state:      "stopped",
	}
	s.bootMarker = s.readBootMarker()

	return s
}

// readBootMarker reads this host's boot marker once at construction (plan 010 D7,
// #67). A Linux boot_id read failure is logged once and degrades to markerless
// behavior (an empty marker); Darwin/others never read and return "".
func (s *Supervisor) readBootMarker() string {
	marker, err := bootMarkerFor(runtime.GOOS, os.ReadFile)
	if err != nil {
		s.systemErrorf("WARNING: could not read boot marker (%v); orphan-reap ledger falls back to markerless behavior", err)
	}
	return marker
}

// ledgerBootMarker returns the marker persistChildren stamps on the ledger.
// If the construction-time read failed (empty marker on Linux), it retries
// the read here: a markerless ledger on Linux is DISCARDED unsignaled by the
// next generation (ledgerDisposition), so letting one transient init-time
// /proc read failure poison every ledger this generation writes would
// silently disable same-boot orphan reaping for the whole run. Called under
// childrenMu, so the lazy backfill of s.bootMarker does not race.
func (s *Supervisor) ledgerBootMarker() string {
	if s.bootMarker == "" && runtime.GOOS == "linux" {
		if marker, err := bootMarkerFor(runtime.GOOS, os.ReadFile); err == nil && marker != "" {
			s.bootMarker = marker
			s.systemErrorf("boot marker became readable; orphan-reap ledger is marker-guarded again")
		}
	}
	return s.bootMarker
}

// Start starts the supervisor and all configured processes
func (s *Supervisor) Start(ctx context.Context) (StartResult, error) {
	return s.startWithFilter(ctx, nil)
}

// StartProcesses starts only the specified processes
func (s *Supervisor) StartProcesses(ctx context.Context, names []string) (StartResult, error) {
	nameSet := make(map[string]bool)
	for _, name := range names {
		nameSet[name] = true
	}
	return s.startWithFilter(ctx, nameSet)
}

// startWithFilter is the common implementation for Start and StartProcesses.
// If filter is nil, all processes are started. Otherwise, only processes in the filter are started.
func (s *Supervisor) startWithFilter(ctx context.Context, filter map[string]bool) (StartResult, error) {
	result := StartResult{
		Failed: make(map[string]error),
	}

	s.mu.Lock()
	if s.state == "running" {
		s.mu.Unlock()
		return result, fmt.Errorf("supervisor already running")
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.state = "running"
	// Reopen the launch gate. This also covers a stop->start cycle (Stop flipped
	// it false): the fresh run must accept launches again (#32/#36, D2).
	s.launchable.Store(true)
	s.startedAt = time.Now()
	s.mu.Unlock()

	s.emit(SupervisorEvent{
		Type:      EventTypeSupervisorStart,
		Timestamp: time.Now(),
	})

	// Create managed processes
	for name, procConfig := range s.config.Processes {
		// Skip if filter is set and this process is not in it
		if filter != nil && !filter[name] {
			continue
		}

		mp, err := s.createManagedProcess(name, procConfig)
		if err != nil {
			result.Failed[name] = err
			continue
		}

		s.mu.Lock()
		s.processes[name] = mp
		s.mu.Unlock()
	}

	// Start all processes concurrently
	s.startProcessesConcurrently(&result)

	return result, nil
}

// createManagedProcess creates a new managed process from configuration.
//
// Environment loading is deferred to a closure invoked at the top of every
// Start (see ManagedProcess.loadEnv / D1): this avoids an eager load here
// that would (a) double-read the env files on the very first start and
// (b) prevent process creation entirely if the env file is transiently
// unreadable. A bad env file now fails loudly at Start instead.
func (s *Supervisor) createManagedProcess(name string, procConfig config.ProcessConfig) (*ManagedProcess, error) {
	domainConfig, loadEnv, effective, err := s.buildProcessRuntime(name, s.config, procConfig)
	if err != nil {
		return nil, err
	}

	mp := NewManagedProcess(domainConfig, nil, s.runner, s.logManager)
	// mp is not yet published to s.processes / any other goroutine, so these
	// direct field sets need no lock; production reads go through the locked
	// StopTimeout() accessor and the lock-held loadEnv read in Start.
	mp.loadEnv = loadEnv
	mp.shutdownTimeout = effective
	// Inject the launch gate (#32/#36, D2). ATOMIC READ ONLY: startWithConfig
	// calls this inside its p.mu critical section, so this closure must NOT take
	// s.mu -- the verified s.mu -> p.mu order would make an s.mu acquisition here
	// an AB-BA deadlock against callers already holding s.mu while reaching for
	// p.mu (Processes/Process/MaxStopBudget). Reading the atomic is sufficient.
	mp.launchGate = func() error {
		if !s.launchable.Load() {
			return domain.ErrShutdownInProgress
		}
		return nil
	}
	// Persist the orphan-reaping ledger after every successful launch (plan 009
	// D11-D13, #59). startWithConfig invokes this AFTER releasing p.mu, so
	// persistChildren's childrenMu -> s.mu ordering is safe. Wiring it here (on
	// every managed process) means all launch paths persist and a future one
	// cannot forget.
	mp.onLaunched = s.persistChildren

	return mp, nil
}

// buildProcessRuntime resolves the domain process config, the env-loader
// closure, and the effective stop budget for a process from the given config
// (the up-time config at creation, or a freshly reloaded one on an API-driven
// (re)start -- see prepareReload). The env closure captures cfg's global
// env_file plus the process's own env_file/inline env, so a path change in a
// reloaded file takes effect when it is invoked at the top of Start.
//
// Effective stop budget: the process's own stop_timeout, else cfg's global
// shutdown_timeout, else constants.DefaultShutdownTimeout (#35, D1).
func (s *Supervisor) buildProcessRuntime(name string, cfg *config.Config, procConfig config.ProcessConfig) (domain.ProcessConfig, func() (map[string]string, error), time.Duration, error) {
	globalEnvFile := cfg.EnvFile
	procEnvFile := procConfig.EnvFile
	inlineEnv := procConfig.Env
	configDir := s.supConfig.ConfigDir
	loadEnv := func() (map[string]string, error) {
		return config.LoadProcessEnv(globalEnvFile, procEnvFile, inlineEnv, configDir)
	}

	stopTimeout, err := procConfig.StopTimeoutDuration()
	if err != nil {
		return domain.ProcessConfig{}, nil, 0, fmt.Errorf("process %q: %w", name, err)
	}

	domainConfig := domain.ProcessConfig{
		Name:        name,
		Cmd:         procConfig.Cmd,
		EnvFile:     procConfig.EnvFile,
		StopTimeout: stopTimeout,
	}
	if procConfig.Healthcheck != nil {
		hc, err := procConfig.Healthcheck.ToDomain()
		if err != nil {
			return domain.ProcessConfig{}, nil, 0, fmt.Errorf("process %q healthcheck: %w", name, err)
		}
		domainConfig.Healthcheck = hc
	}

	effective := stopTimeout
	if effective == 0 {
		global, err := cfg.ShutdownTimeoutDuration()
		if err != nil {
			return domain.ProcessConfig{}, nil, 0, fmt.Errorf("process %q: %w", name, err)
		}
		effective = global
	}
	// Honor a SupervisorConfig-level default before the constant so directly
	// constructed supervisors (no shutdown_timeout in the file) get the timeout
	// they configured; up.go sets it from the same parsed config, so this is a
	// no-op on the normal path.
	if effective == 0 {
		effective = s.supConfig.ShutdownTimeout
	}
	if effective <= 0 {
		effective = constants.DefaultShutdownTimeout
	}

	return domainConfig, loadEnv, effective, nil
}

// prepareReload re-reads and validates the whole config file and resolves the
// pending runtime (domain config + env loader + effective stop budget) for the
// named process, to be swapped in atomically inside startWithConfig (#33, D3).
//
// It returns:
//   - (nil, nil) when ConfigPath is unset -- reload is disabled and the caller
//     (re)starts with the existing stored config (back-compat for tests that
//     construct a Supervisor directly);
//   - (nil, ErrConfigReloadFailed) for a missing/unreadable file, YAML syntax
//     error, validation failure (including an invalid UNRELATED section --
//     whole-file validation is intentional, fail-closed), or a preflight env
//     failure;
//   - (nil, ErrProcessNotInConfig) when the target was removed from the file;
//   - (pending, nil) otherwise.
//
// It touches no ManagedProcess state and performs no stop, so any error here
// leaves the running process completely untouched. The env loader is preflighted
// once (result discarded) so a missing new env_file fails before any stop.
func (s *Supervisor) prepareReload(name string) (*pendingConfig, error) {
	cfgPath := s.supConfig.ConfigPath
	if cfgPath == "" {
		return nil, nil
	}

	fresh, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrConfigReloadFailed, err)
	}

	procConfig, ok := fresh.Processes[name]
	if !ok {
		return nil, fmt.Errorf("process %q %w; run 'prox up' to reconcile", name, domain.ErrProcessNotInConfig)
	}

	domainConfig, loadEnv, effective, err := s.buildProcessRuntime(name, fresh, procConfig)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrConfigReloadFailed, err)
	}

	// Preflight the env load so a missing/unreadable new env_file fails here,
	// before any stop -- the running process stays untouched. The resulting map
	// is carried on the pending config and applied verbatim at launch (see
	// pendingConfig.env), so the same validated snapshot is used and no re-read
	// window is reopened after the stop.
	env, err := loadEnv()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrConfigReloadFailed, err)
	}

	return &pendingConfig{config: domainConfig, loadEnv: loadEnv, env: env, stopTimeout: effective}, nil
}

// startProcessesConcurrently starts all managed processes concurrently and updates the result.
func (s *Supervisor) startProcessesConcurrently(result *StartResult) {
	var wg sync.WaitGroup
	var resultMu sync.Mutex

	for name, mp := range s.processes {
		wg.Add(1)
		go func(name string, mp *ManagedProcess) {
			defer wg.Done()
			if err := mp.Start(s.ctx); err != nil {
				s.logManager.Write(domain.LogEntry{
					Timestamp: time.Now(),
					Process:   name,
					Stream:    domain.StreamStderr,
					Line:      fmt.Sprintf("Failed to start: %v", err),
				})
				resultMu.Lock()
				result.Failed[name] = err
				resultMu.Unlock()
			} else {
				s.emit(SupervisorEvent{
					Type:      EventTypeProcessStarted,
					Process:   name,
					Timestamp: time.Now(),
					Info:      mp.Info(),
				})
				resultMu.Lock()
				result.Started = append(result.Started, name)
				resultMu.Unlock()
			}
		}(name, mp)
	}
	wg.Wait()
}

// persistChildren rewrites the orphan-reaping ownership ledger to reflect every
// currently-live child (plan 009 D11-D13, #59). It is invoked by every
// successful startWithConfig (via ManagedProcess.onLaunched), OUTSIDE any p.mu
// critical section.
//
// It holds childrenMu across BOTH the s.mu snapshot and the file write so the
// serialized persists each capture the up-to-date live set -- the final write
// therefore reflects reality even when launches race (last-writer-correctness).
// The write itself is temp+rename (file-atomicity), so a concurrent LoadChildren
// never sees a torn ledger.
//
// Persistence-failure policy (best-effort, loud): a write failure is logged
// prominently and swallowed -- the just-started child is NOT failed or killed
// over an unwritable ledger.
func (s *Supervisor) persistChildren() {
	if s.supConfig.StateDir == "" {
		return
	}

	s.childrenMu.Lock()
	defer s.childrenMu.Unlock()

	recs := s.snapshotChildren()

	// Gate AFTER the snapshot, not before. Once launches are refused, Stop owns the
	// ledger (removes it after a clean reap, or RETAINS it on
	// ErrProcessGroupNotReaped). A launch that got past the launch gate and reaches
	// this callback late must NOT rewrite the ledger, or it could clobber a
	// retained ledger (dropping a surviving group) or recreate one after removal
	// (codex review). Stop flips launchable false BEFORE it stops any process, and
	// the p.mu the snapshot takes to read a process's (Crashed) state
	// synchronizes-with that flip -- so any snapshot that could OMIT a
	// being-stopped survivor is guaranteed to observe launchable==false here and
	// abort. Checking BEFORE the snapshot left a window where a callback read
	// launchable==true, then Stop flipped it and Crashed the survivor, then the
	// stale snapshot wrote an incomplete ledger.
	//
	// Accepted residual (plan §9, codex): this gate trades the clobber-a-retained-
	// ledger bug for a narrower completeness gap. If a callback snapshots BEFORE a
	// concurrent launch B, writes while launchable is still true, and B's own
	// callback then aborts at this gate during shutdown, a B that SURVIVES Stop can
	// be left out of the ledger. This is NOT a safety issue (it can only fail to
	// reap, never signal an innocent group) and is ultra-narrow (needs a
	// SIGKILL-surviving group AND an API launch landing exactly at shutdown); worst
	// case the operator kills that one group manually, as before #59. Fully closing
	// it (Stop authoritatively rewriting the survivor set) is a recommended
	// follow-up, bounded anyway by the pre-existing launch/stop race (#36 D4).
	if !s.launchable.Load() {
		return
	}

	if err := WriteChildren(s.supConfig.StateDir, s.ledgerBootMarker(), recs); err != nil {
		s.systemErrorf("ERROR: failed to persist child ownership ledger: %v", err)
	}
}

// systemErrorf writes an ERROR-level system log line to stderr. It is the stderr
// sibling of SystemLog, used for the best-effort ledger-persistence failures that
// are logged loudly but swallowed (see persistChildren / removeChildrenLedger).
func (s *Supervisor) systemErrorf(format string, args ...interface{}) {
	s.logManager.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "system",
		Stream:    domain.StreamStderr,
		Line:      fmt.Sprintf(format, args...),
	})
}

// snapshotChildren returns a ledger record for every currently-live child. It
// collects the managed-process pointers under s.mu (correct s.mu -> p.mu order),
// then reads each record via childRecord (which takes its own p.mu and reads the
// start token UNDER it, so the token matches the captured pid's generation), so
// no per-process syscall runs while s.mu is held.
func (s *Supervisor) snapshotChildren() []ChildRecord {
	s.mu.RLock()
	procs := make([]*ManagedProcess, 0, len(s.processes))
	for _, mp := range s.processes {
		procs = append(procs, mp)
	}
	s.mu.RUnlock()

	recs := make([]ChildRecord, 0, len(procs))
	for _, mp := range procs {
		if rec, ok := mp.childRecord(); ok {
			recs = append(recs, rec)
		}
	}
	return recs
}

// removeChildrenLedger deletes the ownership ledger under childrenMu. It is
// called from Stop ONLY when every group was confirmed reaped -- a clean
// shutdown leaves no orphans to reap, so the ledger must not linger and drive a
// spurious reap on the next startup. A no-op when persistence is disabled.
func (s *Supervisor) removeChildrenLedger() {
	if s.supConfig.StateDir == "" {
		return
	}
	s.childrenMu.Lock()
	defer s.childrenMu.Unlock()
	if err := RemoveChildren(s.supConfig.StateDir); err != nil {
		s.systemErrorf("ERROR: failed to remove child ownership ledger: %v", err)
	}
}

// stopEvent maps a per-process Stop result to the event that Supervisor.Stop and
// StopProcess both emit, so the event semantics stay uniform across the two paths
// by construction (#36, D3): a clean stop (or already-not-running) is
// process_stopped; a surviving group is process_crashed (state is already
// Crashed); any other error (ctx expiry/cancellation) is not proof of anything
// and emits no event (ok == false).
func stopEvent(err error) (EventType, bool) {
	switch {
	case err == nil || errors.Is(err, domain.ErrProcessNotRunning):
		return EventTypeProcessStopped, true
	case errors.Is(err, domain.ErrProcessGroupNotReaped):
		return EventTypeProcessCrashed, true
	default:
		return "", false
	}
}

// RefuseLaunches closes the launch gate ahead of Stop. Daemon shutdown runs an
// earlier teardown stage (proxyd deregister, up to a few seconds) BEFORE Stop
// flips the gate itself, and the API keeps serving through that stage; without
// this a StartProcess/RestartProcess arriving in that window would still launch a
// process the shutdown is about to orphan. Calling this at the very top of the
// shutdown sequence closes the gate immediately. It is just s.launchable.Store(
// false) -- idempotent with the identical flip Stop performs, so calling both is
// harmless, and it does NOT change supervisor state (still "running") so the
// read-only API stays fully answerable during the drain.
//
// Accepted residual: a restart already past its pre-checks can still stop its
// current process and is only refused at the start half -- fine, shutdown was
// about to stop that process anyway (#36, D4).
func (s *Supervisor) RefuseLaunches() {
	s.launchable.Store(false)
}

// Stop stops all processes and the supervisor.
//
// The signature stays the idiomatic Stop(ctx) error, but the concrete failure
// value is a *domain.ProcessStopError aggregating every process that did not stop
// cleanly (each failure wraps a sentinel such as ErrProcessGroupNotReaped, so
// errors.Is/errors.As see through the aggregate). It returns a literal nil --
// never a typed-nil *ProcessStopError -- when the stop is clean, and likewise nil
// from the not-running early return (nothing to do) (#36, D3).
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state != "running" {
		s.mu.Unlock()
		return nil
	}
	s.state = "stopping"
	// Close the launch gate before spawning the per-process stop goroutines, so a
	// launcher that observes it closed never launches, and a launcher already past
	// the gate (the check/use window) is reaped by this process's stop goroutine --
	// which is queued on p.mu behind that very launch -- before Stop returns (D2).
	s.launchable.Store(false)
	processes := make([]*ManagedProcess, 0, len(s.processes))
	for _, mp := range s.processes {
		processes = append(processes, mp)
	}
	s.mu.Unlock()

	// Stop all processes concurrently. Each process gets its own timeout context
	// sized from its effective stop budget (StopTimeout) plus stopVerdictMargin,
	// so a per-process stop_timeout is honored individually AND a stop joining an
	// in-flight primary as a secondary outlives the primary's finalization window
	// (#35, D2 / #36, D3). The daemon's outer deadline is sized by up.go from
	// StopWaitBound() so ctx here rarely bounds anything.
	var (
		wg       sync.WaitGroup
		failMu   sync.Mutex
		failures []domain.ProcessStopFailure
	)
	for _, mp := range processes {
		wg.Add(1)
		go func(mp *ManagedProcess) {
			defer wg.Done()
			// Info() already snapshots the effective stop budget (StopTimeout),
			// so read it once here rather than taking the process lock a second
			// time via mp.StopTimeout().
			info := mp.Info()
			stopCtx, cancel := context.WithTimeout(ctx, info.StopTimeout+stopVerdictMargin)
			defer cancel()
			if info.PID > 0 {
				s.SystemLog("sending SIGTERM to %s (pid %d)", info.Name, info.PID)
			}
			err := mp.Stop(stopCtx)
			if evt, ok := stopEvent(err); ok {
				s.emit(SupervisorEvent{
					Type:      evt,
					Process:   mp.Name(),
					Timestamp: time.Now(),
					Info:      mp.Info(),
				})
			}
			// A non-clean stop (anything but nil/ErrProcessNotRunning) is logged once
			// and recorded for the aggregate, whichever class it is. A surviving group
			// is additionally surfaced prominently (D4); its process_crashed event was
			// already emitted above (#36, D3).
			if err != nil && !errors.Is(err, domain.ErrProcessNotRunning) {
				s.logManager.Write(domain.LogEntry{
					Timestamp: time.Now(),
					Process:   mp.Name(),
					Stream:    domain.StreamStderr,
					Line:      fmt.Sprintf("Error stopping: %v", err),
				})
				if errors.Is(err, domain.ErrProcessGroupNotReaped) {
					s.SystemLog("could not reap process group for %s", mp.Name())
				}
				failMu.Lock()
				failures = append(failures, domain.ProcessStopFailure{Name: mp.Name(), Err: err})
				failMu.Unlock()
			}
		}(mp)
	}
	wg.Wait()

	s.mu.Lock()
	s.state = "stopped"
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	s.emit(SupervisorEvent{
		Type:      EventTypeSupervisorStop,
		Timestamp: time.Now(),
	})

	if len(failures) == 0 {
		// Every group was confirmed reaped -- there are no orphans for a later
		// `prox up` to reap, so drop the ownership ledger. It is RETAINED below
		// when a group survived (ErrProcessGroupNotReaped), so the next startup can
		// still reap whatever this stop could not.
		s.removeChildrenLedger()
		// Return a literal nil, never a typed-nil *ProcessStopError (which would be
		// a non-nil error interface).
		return nil
	}
	// Stable output: sort by process name after the concurrent collection.
	sort.Slice(failures, func(i, j int) bool { return failures[i].Name < failures[j].Name })
	return &domain.ProcessStopError{Failures: failures}
}

// Processes returns info for all processes
func (s *Supervisor) Processes() []domain.ProcessInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]domain.ProcessInfo, 0, len(s.processes))
	for _, mp := range s.processes {
		result = append(result, mp.Info())
	}

	// Sort by name for consistent ordering
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// Process returns info for a specific process
func (s *Supervisor) Process(name string) (domain.ProcessInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mp, ok := s.processes[name]
	if !ok {
		return domain.ProcessInfo{}, domain.ErrProcessNotFound
	}

	return mp.Info(), nil
}

// StartProcess starts a specific process
func (s *Supervisor) StartProcess(ctx context.Context, name string) error {
	s.mu.RLock()
	mp, ok := s.processes[name]
	supCtx := s.ctx // Use supervisor context for process lifecycle, not request context
	state := s.state
	s.mu.RUnlock()

	if !ok {
		return domain.ErrProcessNotFound
	}

	// Refuse to launch once shutdown has begun. An in-flight lifecycle request
	// can outlive the API server's shutdown window (Shutdown never force-closes
	// active connections), so without this gate a start could re-launch a child
	// after Supervisor.Stop finished reaping. Not a complete lease (a stop can
	// still begin after this check); full serialization is tracked with #32/#36.
	if state != "running" {
		return domain.ErrShutdownInProgress
	}

	// s.ctx is nil until Supervisor.Start() runs. Passing a nil context into
	// mp.Start -> context.WithCancel would panic; guard against being called
	// before the supervisor has started (unreachable via the normal API wiring,
	// which serves requests only after Start, but defensive).
	if supCtx == nil {
		return domain.ErrShutdownInProgress
	}

	// Re-read the config and prepare the fresh runtime BEFORE launching, so a
	// `stop`+`start` picks up a changed cmd/env/healthcheck/stop_timeout exactly
	// like `restart` does (#33, D3). Any reload failure leaves the process
	// untouched. pending is nil when ConfigPath is unset (reload disabled).
	pending, err := s.prepareReload(name)
	if err != nil {
		return err
	}

	// Use supervisor context for the process lifecycle.
	// The passed ctx is only used for the API request timeout, but the process
	// should continue running after the request completes. The pending config is
	// swapped in atomically inside the locked critical section (see
	// startWithConfig), so a concurrent Start that wins the race never leaves the
	// running process and stored config mismatched.
	err = mp.startWithConfig(supCtx, pending)
	if err == nil {
		s.emit(SupervisorEvent{
			Type:      EventTypeProcessStarted,
			Process:   name,
			Timestamp: time.Now(),
			Info:      mp.Info(),
		})
	}
	return err
}

// StopProcess stops a specific process
func (s *Supervisor) StopProcess(ctx context.Context, name string) error {
	s.mu.RLock()
	mp, ok := s.processes[name]
	s.mu.RUnlock()

	if !ok {
		return domain.ErrProcessNotFound
	}

	// Bound the stop by this process's own effective budget (a snapshot of the
	// currently-effective per-process value; a budget raised concurrently takes
	// effect from the next request). See mp.StopTimeout (#35, D2).
	stopCtx, cancel := context.WithTimeout(ctx, mp.StopTimeout())
	defer cancel()

	// Event semantics are uniform with Supervisor.Stop via the shared stopEvent
	// classifier (#36, D3): process_stopped on a clean/already-not-running stop,
	// process_crashed on a surviving group, no event on any other error.
	err := mp.Stop(stopCtx)
	if evt, ok := stopEvent(err); ok {
		s.emit(SupervisorEvent{
			Type:      evt,
			Process:   name,
			Timestamp: time.Now(),
			Info:      mp.Info(),
		})
	}
	return err
}

// RestartProcess restarts a specific process
func (s *Supervisor) RestartProcess(ctx context.Context, name string) error {
	s.mu.RLock()
	mp, ok := s.processes[name]
	supCtx := s.ctx // Use supervisor context for the replacement's lifecycle, not request context
	state := s.state
	s.mu.RUnlock()

	if !ok {
		return domain.ErrProcessNotFound
	}

	// Refuse to launch a replacement once shutdown has begun (same gate and
	// caveat as StartProcess).
	if state != "running" {
		return domain.ErrShutdownInProgress
	}

	// s.ctx is nil until Supervisor.Start() runs; the replacement is started on
	// it, so guard against a pre-start call that would panic in
	// context.WithCancel (defensive; unreachable via the normal API wiring).
	if supCtx == nil {
		return domain.ErrShutdownInProgress
	}

	// Re-read + validate the whole file and preflight the target's fresh env
	// BEFORE the stop, so an invalid file, a removed process, or a missing new
	// env_file fails with the running process untouched (#33, D3). pending is nil
	// when ConfigPath is unset (reload disabled).
	pending, err := s.prepareReload(name)
	if err != nil {
		return err
	}

	// Bound the stop half of the restart by this process's own effective budget
	// (snapshot of the currently-effective, pre-swap value: a raised stop_timeout
	// only governs from the next stop). Computed here, before mp.Restart runs the
	// stop, so the stop half observes the OLD budget (#35, D2 / #33, D3).
	restartCtx, cancel := context.WithTimeout(ctx, mp.StopTimeout())
	defer cancel()

	// Stop uses restartCtx (bounded by the request/shutdown timeout); the
	// replacement's Start uses supCtx so its process lifecycle and health checker
	// survive after this request's context is cancelled/expires (mirrors
	// StartProcess). The pending config is swapped in atomically inside the
	// replacement's locked critical section (see startWithConfig).
	err = mp.Restart(restartCtx, supCtx, pending)
	if err == nil {
		s.emit(SupervisorEvent{
			Type:      EventTypeProcessStarted,
			Process:   name,
			Timestamp: time.Now(),
			Info:      mp.Info(),
		})
	}
	return err
}

// MaxStopBudget returns the maximum effective stop budget (StopTimeout) over
// all current processes, or constants.DefaultShutdownTimeout when there are no
// processes. It is used at shutdown time to size the daemon's outer shutdown
// deadline so that hot-reloaded per-process budgets (plan D3, future work) are
// respected -- it reads live values rather than a snapshot taken at startup.
func (s *Supervisor) MaxStopBudget() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	maxBudget := time.Duration(0)
	for _, mp := range s.processes {
		if b := mp.StopTimeout(); b > maxBudget {
			maxBudget = b
		}
	}
	if maxBudget <= 0 {
		return constants.DefaultShutdownTimeout
	}
	return maxBudget
}

// StopWaitBound returns the deadline a caller should grant Supervisor.Stop: the
// largest live per-process stop budget plus stopVerdictMargin. The margin lets a
// per-process stop goroutine that joins an in-flight primary as a secondary
// outlive the primary's finalization window (see stopVerdictMargin). up.go sizes
// its supervisor teardown stage from this so it does not duplicate the constant
// (#36, D3).
func (s *Supervisor) StopWaitBound() time.Duration {
	return s.MaxStopBudget() + stopVerdictMargin
}

// Status returns supervisor status
func (s *Supervisor) Status() SupervisorStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return SupervisorStatus{
		State:     s.state,
		StartedAt: s.startedAt,
	}
}

// SupervisorStatus holds supervisor status information
type SupervisorStatus struct {
	State     string
	StartedAt time.Time
}

// StartResult contains information about process startup results
type StartResult struct {
	Started []string         // Names of processes that started successfully
	Failed  map[string]error // Names and errors of processes that failed to start
}

// HasFailures returns true if any processes failed to start
func (r StartResult) HasFailures() bool {
	return len(r.Failed) > 0
}

// AllStarted returns true if all processes started successfully
func (r StartResult) AllStarted() bool {
	return len(r.Failed) == 0
}

// UptimeSeconds returns seconds since supervisor started
func (st SupervisorStatus) UptimeSeconds() int64 {
	if st.StartedAt.IsZero() {
		return 0
	}
	return int64(time.Since(st.StartedAt).Seconds())
}

// Subscribe creates a channel for receiving supervisor events
func (s *Supervisor) Subscribe() <-chan SupervisorEvent {
	ch := make(chan SupervisorEvent, 100)

	s.eventMu.Lock()
	s.eventSubs = append(s.eventSubs, ch)
	s.eventMu.Unlock()

	return ch
}

// emit sends an event to all subscribers
func (s *Supervisor) emit(event SupervisorEvent) {
	s.eventMu.RLock()
	defer s.eventMu.RUnlock()

	for _, ch := range s.eventSubs {
		select {
		case ch <- event:
		default:
			// Channel full, skip
		}
	}
}

// SystemLog writes a system-level log message (displayed as coming from "system")
func (s *Supervisor) SystemLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	s.logManager.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "system",
		Stream:    domain.StreamStdout,
		Line:      msg,
	})
}

// Wait blocks until the supervisor stops or context is cancelled
func (s *Supervisor) Wait(ctx context.Context) error {
	s.mu.RLock()
	supCtx := s.ctx
	s.mu.RUnlock()

	if supCtx == nil {
		return nil
	}

	select {
	case <-supCtx.Done():
		return supCtx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
