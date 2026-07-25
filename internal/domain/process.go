package domain

import "time"

// ProcessState represents the current state of a process.
// Processes transition through these states during their lifecycle.
type ProcessState string

const (
	// ProcessStateRunning indicates the process is actively running
	ProcessStateRunning ProcessState = "running"
	// ProcessStateStopped indicates the process has been stopped (either by user or natural exit)
	ProcessStateStopped ProcessState = "stopped"
	// ProcessStateStarting indicates the process is in the process of starting up
	ProcessStateStarting ProcessState = "starting"
	// ProcessStateStopping indicates the process is in the process of shutting down
	ProcessStateStopping ProcessState = "stopping"
	// ProcessStateCrashed indicates the process exited unexpectedly or failed to start
	ProcessStateCrashed ProcessState = "crashed"
	// ProcessStateWaiting indicates a gated process whose depends_on targets are
	// still being resolved before it may launch (plan 013 D4). It is a limbo
	// state: NOT running (no PID/instance, monitor not started) and NOT stopped
	// (the process is scheduled and will still launch, block, or be stopped). A
	// waiting process has never been handed to the runner, so it carries no
	// process instance.
	ProcessStateWaiting ProcessState = "waiting"
	// ProcessStateBlocked is the terminal state a gated process reaches when a
	// required depends_on target failed (dependency exhausted its budget with
	// on_failure=fail); the process never launches (plan 013 D4). It is terminal
	// like stopped/crashed: no PID, no instance. The blocking target names are
	// recorded on the ManagedProcess (see ManagedProcess.BlockedBy) for status
	// surfacing (C5).
	ProcessStateBlocked ProcessState = "blocked"
	// ProcessStateCompleted is the terminal success state of a run-to-completion
	// task (plan 013 D4). The constant lands here with the rest of the plan-013
	// state enum; task execution that drives a process/task into it is C4.
	ProcessStateCompleted ProcessState = "completed"
)

// String returns the string representation of ProcessState
func (s ProcessState) String() string {
	return string(s)
}

// IsRunning returns true if the process is in a running state
func (s ProcessState) IsRunning() bool {
	return s == ProcessStateRunning
}

// IsStopped returns true if the process is in a terminal not-running state:
// stopped, crashed, blocked (a gated process that will never launch, plan 013
// D4), or completed (a finished task, plan 013 D4). Callers use it as the "no
// live process / report PID 0" predicate. A waiting process is deliberately
// EXCLUDED -- it is neither running nor stopped but scheduled to launch.
func (s ProcessState) IsStopped() bool {
	return s == ProcessStateStopped || s == ProcessStateCrashed ||
		s == ProcessStateBlocked || s == ProcessStateCompleted
}

// ProcessConfig defines the configuration for a single process
type ProcessConfig struct {
	Name        string
	Cmd         string
	Env         map[string]string
	EnvFile     string
	Healthcheck *HealthConfig
	// StopTimeout is this process's own configured SIGTERM->SIGKILL
	// escalation budget (config.ProcessConfig.StopTimeout, parsed). Zero
	// means unset: the effective budget resolution (proc > global > default)
	// happens in supervisor.createManagedProcess, not here.
	StopTimeout time.Duration
	// DependsOn lists the dependencies and/or tasks that must be ready before
	// this process starts (plan 013 D1). Entries reference dependencies: or
	// tasks: names only -- never other processes (validation enforces this).
	// The supervisor wiring that consumes this lands in a later commit.
	DependsOn []string
}

// ProcessInfo represents the runtime state of a process
type ProcessInfo struct {
	Name          string            `json:"name"`
	State         ProcessState      `json:"status"`
	PID           int               `json:"pid"`
	StartedAt     time.Time         `json:"started_at,omitempty"`
	RestartCount  int               `json:"restarts"`
	Health        HealthStatus      `json:"health"`
	HealthDetails *HealthState      `json:"healthcheck,omitempty"`
	Cmd           string            `json:"cmd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	// StopTimeout is the effective SIGTERM->SIGKILL escalation budget in force
	// for this process (per-process stop_timeout, else global shutdown_timeout,
	// else the default). Surfaced so operators can see the budget governing a
	// stop/restart. Zero only for a process built outside createManagedProcess.
	StopTimeout time.Duration `json:"-"`
}

// UptimeSeconds returns the number of seconds the process has been running
func (p ProcessInfo) UptimeSeconds() int64 {
	if p.StartedAt.IsZero() {
		return 0
	}
	return int64(time.Since(p.StartedAt).Seconds())
}
