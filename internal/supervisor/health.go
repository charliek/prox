package supervisor

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// HealthChecker runs periodic health checks for a process.
// It executes a configured command at regular intervals and tracks the health status.
type HealthChecker struct {
	mu sync.RWMutex

	// config holds the health check configuration (command, interval, timeout, etc.)
	config domain.HealthConfig
	// process is the name of the process being checked (for logging)
	process string

	// status is the current health status (unknown, healthy, or unhealthy)
	status domain.HealthStatus
	// lastCheck records when the last health check was performed
	lastCheck time.Time
	// lastOutput holds the output from the last health check command
	lastOutput string
	// consecutiveFailures counts sequential failed health checks
	consecutiveFailures int

	// ctx and cancel control the health check loop lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	// onTransition, when set, is invoked once per health STATUS change (plan 017
	// C10) so the supervisor's change bus wakes on a health flip -- Health is
	// ProcessInfo-visible. It carries no payload and is deliberately a bare
	// callback rather than a supervisor back-reference: the checker knows nothing
	// about the supervisor. It is fired with h.mu RELEASED (see runCheck), because
	// a woken subscriber calls back into Processes() -> Info() -> State(), which
	// takes h.mu. Set once at construction, so it is read lock-free.
	onTransition func()
}

// NewHealthChecker creates a new health checker. onTransition may be nil; when
// set it fires (lock-free) on each health status change -- see the field comment.
func NewHealthChecker(process string, config domain.HealthConfig, onTransition func()) *HealthChecker {
	// Apply defaults
	config = config.WithDefaults()

	return &HealthChecker{
		config:       config,
		process:      process,
		status:       domain.HealthStatusUnknown,
		onTransition: onTransition,
	}
}

// Start starts the health checker
func (h *HealthChecker) Start(ctx context.Context) {
	h.mu.Lock()
	h.ctx, h.cancel = context.WithCancel(ctx)
	h.mu.Unlock()

	go h.run()
}

// Stop stops the health checker
func (h *HealthChecker) Stop() {
	h.mu.Lock()
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Unlock()
}

// State returns the current health state
func (h *HealthChecker) State() domain.HealthState {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return domain.HealthState{
		Enabled:             true,
		Status:              h.status,
		LastCheck:           h.lastCheck,
		LastOutput:          h.lastOutput,
		ConsecutiveFailures: h.consecutiveFailures,
	}
}

// Status returns the current health status
func (h *HealthChecker) Status() domain.HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.status
}

// run is the main health check loop
func (h *HealthChecker) run() {
	h.mu.RLock()
	ctx := h.ctx
	h.mu.RUnlock()

	// Wait for start period
	select {
	case <-ctx.Done():
		return
	case <-time.After(h.config.StartPeriod):
	}

	// Run health checks at interval
	ticker := time.NewTicker(h.config.Interval)
	defer ticker.Stop()

	// Run first check immediately
	h.runCheck(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.runCheck(ctx)
		}
	}
}

// runCheck executes a single health check
func (h *HealthChecker) runCheck(ctx context.Context) {
	// Create timeout context
	checkCtx, cancel := context.WithTimeout(ctx, h.config.Timeout)
	defer cancel()

	// Run the command
	cmd := exec.CommandContext(checkCtx, "sh", "-c", h.config.Cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	h.mu.Lock()
	// prev is sampled under the lock alongside the commit below so the transition
	// test is exact; the callback itself fires after the unlock.
	prev := h.status

	h.lastCheck = time.Now()

	// Combine stdout and stderr for output
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	// Truncate output if too long
	if len(output) > 1000 {
		output = output[:1000] + "..."
	}
	h.lastOutput = output

	if err != nil {
		// Health check failed
		h.consecutiveFailures++
		if h.consecutiveFailures >= h.config.Retries {
			h.status = domain.HealthStatusUnhealthy
		}
	} else {
		// Health check passed
		h.consecutiveFailures = 0
		h.status = domain.HealthStatusHealthy
	}
	changed := h.status != prev
	h.mu.Unlock()

	// LOCK DISCIPLINE: h.mu is released above; the callback re-enters the
	// supervisor (Processes -> Info -> State), which takes p.mu and h.mu. Fired
	// only on a real status change, so a steady stream of passing checks does not
	// wake subscribers on every tick (LastCheck/LastOutput churn deliberately does
	// not notify).
	if changed && h.onTransition != nil {
		h.onTransition()
	}
}
