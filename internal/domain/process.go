package domain

import "time"

// ProcessKind discriminates a managed child's run mode (plan 013 D3). A plain
// process is a long-running service supervised for the whole `prox up`
// lifetime; a task is a run-to-completion command that executes once per
// supervisor lifetime after its depends_on targets are satisfied. Both are
// ManagedProcess children (they share env loading, logging, process-group
// handling, and stop escalation); only the exit mapping and scheduling differ.
type ProcessKind string

const (
	// ProcessKindProcess is a long-running supervised service. It is the zero
	// value's meaning: an unset Kind is treated as a process.
	ProcessKindProcess ProcessKind = "process"
	// ProcessKindTask is a run-to-completion command (plan 013 D3). A natural
	// exit 0 drives it to ProcessStateCompleted; any other exit drives it to
	// ProcessStateCrashed.
	ProcessKindTask ProcessKind = "task"
)

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

// IsTerminalFailure reports whether the state means the process definitively
// FAILED. The set is exactly two states, and widening it is a behavior change
// to the exit codes of `prox up`, `up -d`, `start` and `restart`:
//
//   - crashed -- exited unexpectedly, or failed to start. Terminal: prox has
//     no restart/backoff policy anywhere (there is no RestartPolicy in the
//     tree), so a crashed process STAYS crashed and this predicate cannot
//     false-positive on one that is merely mid-relaunch.
//   - blocked -- a gated process that a failed required dependency will never
//     let launch.
//
// Everything else is NOT a failure: starting and stopping are transient,
// waiting is limbo (the process is still scheduled to launch), stopped is
// deliberate, running is the goal, and completed is a task's terminal
// SUCCESS.
//
// Do NOT reach for IsStopped when you mean this: it collapses stopped,
// crashed, blocked AND completed together, so a task that finished cleanly
// would be reported as a failed start.
func (s ProcessState) IsTerminalFailure() bool {
	return s == ProcessStateCrashed || s == ProcessStateBlocked
}

// IsLive reports whether the state can still change on its own: running,
// starting, stopping, or waiting. Against today's 8 states this predicate and
// IsStopped are arithmetically opposite, but they are kept as two separate
// functions on purpose: they answer different questions -- "can this still
// change on its own?" versus "should PID be reported as 0?" -- and a 9th
// state added later is not guaranteed to answer both the same way. Defining
// IsLive as "not IsStopped" would make that future drift silent.
func (s ProcessState) IsLive() bool {
	return s == ProcessStateRunning || s == ProcessStateStarting ||
		s == ProcessStateStopping || s == ProcessStateWaiting
}

// AllProcessStates returns the canonical enumeration of every ProcessState.
// It exists so tests of the predicates above (and any future one) can be
// genuinely exhaustive: a table keyed by a literal count like
// require.Len(cases, 8) does not fail when a 9th state is added, because
// nothing enumerates the enum against that literal. Iterating this slice
// does. Returns a fresh slice each call so callers cannot mutate a
// package-level array.
func AllProcessStates() []ProcessState {
	return []ProcessState{
		ProcessStateRunning,
		ProcessStateStopped,
		ProcessStateStarting,
		ProcessStateStopping,
		ProcessStateCrashed,
		ProcessStateWaiting,
		ProcessStateBlocked,
		ProcessStateCompleted,
	}
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
	// Kind is the child's run mode (plan 013 D3). The zero value ("") means a
	// plain process; ProcessKindTask marks a run-to-completion task. Tasks carry
	// the run-budget fields below.
	Kind ProcessKind
	// TaskTimeout is a task's run budget; meaningful only when Kind is
	// ProcessKindTask and TaskHasTimeout is true (plan 013 D3). See
	// domain.TaskConfig.Timeout/HasTimeout.
	TaskTimeout time.Duration
	// TaskHasTimeout distinguishes a time-bounded task from an explicitly
	// unbounded one (timeout: 0). Only meaningful when Kind is ProcessKindTask.
	TaskHasTimeout bool
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
	// Kind is the child's run mode (plan 013 D3): ProcessKindProcess or
	// ProcessKindTask. Info() always reports a concrete value (never ""), so
	// status/API consumers can render tasks distinctly (surfacing lands in C5).
	Kind ProcessKind `json:"kind,omitempty"`
	// EndedAt freezes the reference time for UptimeSeconds when set (plan 013
	// D3). It is populated for a completed task so its uptime reflects how long
	// the task RAN (completion time - start time) rather than continuing to tick
	// after it finished. Zero for a live process, whose uptime keeps counting.
	EndedAt time.Time `json:"-"`
	// StopTimeout is the effective SIGTERM->SIGKILL escalation budget in force
	// for this process (per-process stop_timeout, else global shutdown_timeout,
	// else the default). Surfaced so operators can see the budget governing a
	// stop/restart. Zero only for a process built outside createManagedProcess.
	StopTimeout time.Duration `json:"-"`
	// WaitingOn lists the depends_on targets this process is gated on while it is
	// in the waiting state, in declaration order (plan 013 D5). A gated process
	// leaves waiting only once ALL its targets are satisfied, so while waiting it
	// is genuinely still gated on this set; it is empty in every other state.
	WaitingOn []string `json:"waiting_on,omitempty"`
	// BlockedOn lists the depends_on targets that failed and left this process in
	// the blocked state, in declaration order (plan 013 D5). Mirrors
	// ManagedProcess.BlockedBy; empty in every other state.
	BlockedOn []string `json:"blocked_on,omitempty"`
}

// UptimeSeconds returns the number of seconds the process has been running. For
// a terminal task whose EndedAt is frozen (plan 013 D3) it reports the run
// duration (EndedAt - StartedAt); otherwise it counts up to now.
func (p ProcessInfo) UptimeSeconds() int64 {
	if p.StartedAt.IsZero() {
		return 0
	}
	end := time.Now()
	if !p.EndedAt.IsZero() {
		end = p.EndedAt
	}
	return int64(end.Sub(p.StartedAt).Seconds())
}
