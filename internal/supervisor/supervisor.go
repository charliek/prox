package supervisor

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

// SupervisorConfig holds configuration for the supervisor
type SupervisorConfig struct {
	ShutdownTimeout time.Duration
	ConfigDir       string // Directory containing the config file (for resolving relative paths)
}

// DefaultSupervisorConfig returns default configuration
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		ShutdownTimeout: 10 * time.Second,
	}
}

// Supervisor manages multiple processes.
// It coordinates starting, stopping, and monitoring all configured processes.
type Supervisor struct {
	mu sync.RWMutex

	// config holds the loaded process configuration
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
func (s *Supervisor) createManagedProcess(name string, procConfig config.ProcessConfig) (*ManagedProcess, error) {
	// Load environment for this process
	env, err := config.LoadProcessEnv(s.config.EnvFile, procConfig.EnvFile, procConfig.Env, s.supConfig.ConfigDir)
	if err != nil {
		s.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   name,
			Stream:    domain.StreamStderr,
			Line:      fmt.Sprintf("Failed to load environment: %v", err),
		})
		return nil, fmt.Errorf("failed to load environment: %w", err)
	}

	domainConfig := domain.ProcessConfig{
		Name:    name,
		Cmd:     procConfig.Cmd,
		Env:     env,
		EnvFile: procConfig.EnvFile,
	}
	if procConfig.Healthcheck != nil {
		domainConfig.Healthcheck = &domain.HealthConfig{
			Cmd: procConfig.Healthcheck.Cmd,
		}
	}

	return NewManagedProcess(domainConfig, env, s.runner, s.logManager), nil
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

	// Create timeout context for shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, s.supConfig.ShutdownTimeout)
	defer cancel()

	// Stop all processes concurrently
	var wg sync.WaitGroup
	for _, mp := range processes {
		wg.Add(1)
		go func(mp *ManagedProcess) {
			defer wg.Done()
			if err := mp.Stop(shutdownCtx); err != nil {
				s.logManager.Write(domain.LogEntry{
					Timestamp: time.Now(),
					Process:   mp.Name(),
					Stream:    domain.StreamStderr,
					Line:      fmt.Sprintf("Error stopping: %v", err),
				})
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
	s.mu.RUnlock()

	if !ok {
		return domain.ErrProcessNotFound
	}

	// Use supervisor context for the process lifecycle.
	// The passed ctx is only used for the API request timeout, but the process
	// should continue running after the request completes.
	err := mp.Start(supCtx)
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

	// Create timeout context
	stopCtx, cancel := context.WithTimeout(ctx, s.supConfig.ShutdownTimeout)
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
	s.mu.RUnlock()

	if !ok {
		return domain.ErrProcessNotFound
	}

	// Create timeout context
	restartCtx, cancel := context.WithTimeout(ctx, s.supConfig.ShutdownTimeout)
	defer cancel()

	err := mp.Restart(restartCtx)
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
	Started []string            // Names of processes that started successfully
	Failed  map[string]error    // Names and errors of processes that failed to start
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
