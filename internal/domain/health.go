package domain

import "time"

// HealthStatus represents the health state of a process
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusUnknown   HealthStatus = "unknown"
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

// HealthState represents the current health check state
type HealthState struct {
	Enabled             bool         `json:"enabled"`
	Status              HealthStatus `json:"status"`
	LastCheck           time.Time    `json:"last_check,omitempty"`
	LastOutput          string       `json:"last_output,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
}
