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
)

// String returns the string representation of ProcessState
func (s ProcessState) String() string {
	return string(s)
}

// IsRunning returns true if the process is in a running state
func (s ProcessState) IsRunning() bool {
	return s == ProcessStateRunning
}

// IsStopped returns true if the process is stopped or crashed
func (s ProcessState) IsStopped() bool {
	return s == ProcessStateStopped || s == ProcessStateCrashed
}

// ProcessConfig defines the configuration for a single process
type ProcessConfig struct {
	Name        string
	Cmd         string
	Env         map[string]string
	EnvFile     string
	Healthcheck *HealthConfig
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
}

// UptimeSeconds returns the number of seconds the process has been running
func (p ProcessInfo) UptimeSeconds() int64 {
	if p.StartedAt.IsZero() {
		return 0
	}
	return int64(time.Since(p.StartedAt).Seconds())
}
