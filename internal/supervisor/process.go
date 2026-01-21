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

// ManagedProcess handles the lifecycle of a single process
type ManagedProcess struct {
	mu sync.RWMutex

	config     domain.ProcessConfig
	env        map[string]string
	runner     ProcessRunner
	logManager *logs.Manager

	state        domain.ProcessState
	process      Process
	startedAt    time.Time
	restartCount int

	// Health checker
	healthChecker *HealthChecker

	// Context for the current process instance
	cancel context.CancelFunc

	// Channel to signal when process exits
	done     chan struct{}
	doneOnce sync.Once // Ensures done channel is closed only once

	// outputWg tracks completion of output reader goroutines
	outputWg sync.WaitGroup
}

// NewManagedProcess creates a new managed process
func NewManagedProcess(config domain.ProcessConfig, env map[string]string, runner ProcessRunner, logManager *logs.Manager) *ManagedProcess {
	return &ManagedProcess{
		config:     config,
		env:        env,
		runner:     runner,
		logManager: logManager,
		state:      domain.ProcessStateStopped,
		done:       make(chan struct{}),
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

	if p.process != nil {
		info.PID = p.process.PID()
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

	if p.state == domain.ProcessStateRunning || p.state == domain.ProcessStateStarting {
		return domain.ErrProcessAlreadyRunning
	}

	p.state = domain.ProcessStateStarting

	// Create a new context for this process instance
	processCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	p.doneOnce = sync.Once{} // Reset for new process instance

	// Start the process
	proc, err := p.runner.Start(processCtx, p.config, p.env)
	if err != nil {
		p.state = domain.ProcessStateCrashed
		p.cancel = nil
		p.closeDone()
		return err
	}

	p.process = proc
	p.startedAt = time.Now()
	p.state = domain.ProcessStateRunning

	// Start output readers with WaitGroup tracking
	p.outputWg.Add(2)
	go func() {
		defer p.outputWg.Done()
		p.readOutput(proc.Stdout(), domain.StreamStdout)
	}()
	go func() {
		defer p.outputWg.Done()
		p.readOutput(proc.Stderr(), domain.StreamStderr)
	}()

	// Start health checker if configured
	if p.config.Healthcheck != nil && p.config.Healthcheck.Cmd != "" {
		p.healthChecker = NewHealthChecker(p.config.Name, *p.config.Healthcheck)
		p.healthChecker.Start(processCtx)
	}

	// Monitor the process
	go p.monitor()

	return nil
}

// Stop stops the process gracefully
func (p *ManagedProcess) Stop(ctx context.Context) error {
	p.mu.Lock()

	if p.state == domain.ProcessStateStopped || p.state == domain.ProcessStateCrashed {
		p.mu.Unlock()
		return domain.ErrProcessNotRunning
	}

	if p.state == domain.ProcessStateStopping {
		p.mu.Unlock()
		// Wait for existing stop to complete
		select {
		case <-p.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	p.state = domain.ProcessStateStopping
	proc := p.process
	cancel := p.cancel
	done := p.done
	healthChecker := p.healthChecker
	p.healthChecker = nil
	p.mu.Unlock()

	// Stop health checker if running
	if healthChecker != nil {
		healthChecker.Stop()
	}

	if proc == nil {
		return nil
	}

	// Send SIGTERM
	if err := proc.Signal(sigterm); err != nil {
		// Log the error - process might already be dead, but this is useful for debugging
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.config.Name,
			Stream:    domain.StreamStderr,
			Line:      "SIGTERM failed (process may have already exited): " + err.Error(),
		})
	}

	// Wait for graceful shutdown or timeout
	select {
	case <-done:
		// Process exited
	case <-ctx.Done():
		// Timeout - send SIGKILL
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "system",
			Stream:    domain.StreamStdout,
			Line:      fmt.Sprintf("sending SIGKILL to %s (graceful shutdown timed out)", p.config.Name),
		})
		if err := proc.Signal(sigkill); err != nil {
			p.logManager.Write(domain.LogEntry{
				Timestamp: time.Now(),
				Process:   p.config.Name,
				Stream:    domain.StreamStderr,
				Line:      "SIGKILL failed: " + err.Error(),
			})
		}
		// Wait a bit for SIGKILL
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}

	// Cancel context to clean up
	if cancel != nil {
		cancel()
	}

	return nil
}

// Restart restarts the process
func (p *ManagedProcess) Restart(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil && err != domain.ErrProcessNotRunning {
		return err
	}

	p.mu.Lock()
	p.restartCount++
	p.mu.Unlock()

	return p.Start(ctx)
}

// monitor watches for process exit
func (p *ManagedProcess) monitor() {
	proc := p.process
	if proc == nil {
		return
	}

	err := proc.Wait()

	// Wait for output readers to finish draining pipes with a timeout.
	// With manual pipes (not cmd.StdoutPipe), the pipes stay open until
	// all processes (including grandchildren) close them. This ensures
	// graceful shutdown messages from child processes are captured.
	// However, if a grandchild holds the pipe open indefinitely, we don't
	// want to block forever.
	outputDone := make(chan struct{})
	go func() {
		p.outputWg.Wait()
		close(outputDone)
	}()

	select {
	case <-outputDone:
		// Output readers finished normally
	case <-time.After(outputDrainTimeout):
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.config.Name,
			Stream:    domain.StreamStderr,
			Line:      "output capture timed out (some logs may be missing)",
		})
	}

	// Extract exit code from error
	// For signal termination, we use negative signal number (e.g., -15 for SIGTERM)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() {
					// Process was killed by signal - use negative signal number
					exitCode = -int(status.Signal())
				} else {
					exitCode = status.ExitStatus()
				}
			} else {
				exitCode = exitErr.ExitCode()
			}
		} else {
			exitCode = 1 // Generic error
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == domain.ProcessStateStopping {
		p.state = domain.ProcessStateStopped
		// Log the stopped message with exit code
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.config.Name,
			Stream:    domain.StreamStdout,
			Line:      fmt.Sprintf("stopped (rc=%d)", exitCode),
		})
	} else {
		// Unexpected exit
		p.state = domain.ProcessStateCrashed
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.config.Name,
			Stream:    domain.StreamStderr,
			Line:      fmt.Sprintf("exited unexpectedly (rc=%d)", exitCode),
		})
	}

	p.process = nil
	p.closeDone()
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
		p.logManager.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   p.config.Name,
			Stream:    domain.StreamStderr,
			Line:      fmt.Sprintf("output reader error: %v", err),
		})
	}
}

// closeDone safely closes the done channel using sync.Once to prevent double-close panic
func (p *ManagedProcess) closeDone() {
	p.doneOnce.Do(func() {
		close(p.done)
	})
}
