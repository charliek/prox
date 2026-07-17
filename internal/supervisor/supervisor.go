package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

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

	// ctx and cancel are used for coordinating graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc

	// eventMu protects eventSubs from concurrent access
	eventMu sync.RWMutex
	// eventSubs holds channels for subscribers to supervisor events
	eventSubs []chan SupervisorEvent
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

	return s
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

// Stop stops all processes and the supervisor
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state != "running" {
		s.mu.Unlock()
		return nil
	}
	s.state = "stopping"
	processes := make([]*ManagedProcess, 0, len(s.processes))
	for _, mp := range s.processes {
		processes = append(processes, mp)
	}
	s.mu.Unlock()

	// Stop all processes concurrently. Each process gets its own timeout context
	// sized from its effective stop budget (StopTimeout), so a per-process
	// stop_timeout is honored individually. This replaces the single shared
	// shutdownCtx that previously truncated every process to one common deadline
	// (#35, D2). The daemon's outer deadline is sized by up.go from
	// MaxStopBudget() so ctx here rarely bounds anything.
	var wg sync.WaitGroup
	for _, mp := range processes {
		wg.Add(1)
		go func(mp *ManagedProcess) {
			defer wg.Done()
			// Info() already snapshots the effective stop budget (StopTimeout),
			// so read it once here rather than taking the process lock a second
			// time via mp.StopTimeout().
			info := mp.Info()
			stopCtx, cancel := context.WithTimeout(ctx, info.StopTimeout)
			defer cancel()
			if info.PID > 0 {
				s.SystemLog("sending SIGTERM to %s (pid %d)", info.Name, info.PID)
			}
			if err := mp.Stop(stopCtx); err != nil && err != domain.ErrProcessNotRunning {
				s.logManager.Write(domain.LogEntry{
					Timestamp: time.Now(),
					Process:   mp.Name(),
					Stream:    domain.StreamStderr,
					Line:      fmt.Sprintf("Error stopping: %v", err),
				})
				// Full-instance stop is best-effort, but surface an
				// un-reapable group prominently so operators can see which
				// process leaked (D4). We do not abort the rest of shutdown.
				if errors.Is(err, domain.ErrProcessGroupNotReaped) {
					s.SystemLog("could not reap process group for %s", mp.Name())
				}
			}
			s.emit(SupervisorEvent{
				Type:      EventTypeProcessStopped,
				Process:   mp.Name(),
				Timestamp: time.Now(),
				Info:      mp.Info(),
			})
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

	return nil
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

	err := mp.Stop(stopCtx)
	if err == nil || err == domain.ErrProcessNotRunning {
		s.emit(SupervisorEvent{
			Type:      EventTypeProcessStopped,
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
