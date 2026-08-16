package domain

import "time"

// HealthStatus represents the health state of a process
type HealthStatus string

// The four health values are mutually exclusive and, together, keep "we never
// checked" distinct from "we checked and could not tell" (#100). Before this
// distinction existed, a process with no `healthcheck:` block reported
// `unknown`, which reads as a check that ran and failed to reach a verdict.
const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	// HealthStatusUnknown means a healthcheck IS configured but has not reported
	// a verdict yet: the process is stopped/crashed/not yet launched, or it is
	// running but still inside its start_period.
	HealthStatusUnknown HealthStatus = "unknown"
	// HealthStatusNone means no healthcheck is configured for this process, so
	// prox never had anything to run. `prox status` renders it as "-" (the same
	// convention as an absent PID) rather than as a failed check.
	HealthStatusNone HealthStatus = "none"
)

// String returns the string representation of HealthStatus
func (s HealthStatus) String() string {
	return string(s)
}

// HealthConfig defines health check configuration
type HealthConfig struct {
	Cmd         string        `yaml:"cmd"`
	Interval    time.Duration `yaml:"interval"`
	Timeout     time.Duration `yaml:"timeout"`
	Retries     int           `yaml:"retries"`
	StartPeriod time.Duration `yaml:"start_period"`
}

// WithDefaults returns a copy of the config with default values applied
func (c HealthConfig) WithDefaults() HealthConfig {
	result := c
	if result.Interval == 0 {
		result.Interval = 10 * time.Second
	}
	if result.Timeout == 0 {
		result.Timeout = 5 * time.Second
	}
	if result.Retries == 0 {
		result.Retries = 3
	}
	if result.StartPeriod == 0 {
		result.StartPeriod = 30 * time.Second
	}
	return result
}

// HealthState represents the current health check state. It is only reported
// for a process that HAS a healthcheck configured; a process without one
// carries HealthStatusNone and no HealthState at all.
type HealthState struct {
	// Enabled reports whether the health check loop is currently running for
	// this process. It is false while a configured check is dormant -- the
	// process is stopped or has not been launched yet -- in which case Status is
	// HealthStatusUnknown. It used to be hardcoded true by the checker's only
	// constructor, which made it a field that could never disagree with its own
	// presence (#100 panel finding).
	Enabled             bool         `json:"enabled"`
	Status              HealthStatus `json:"status"`
	LastCheck           time.Time    `json:"last_check,omitempty"`
	LastOutput          string       `json:"last_output,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
}
