package api

import (
	"strings"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// sensitiveEnvPatterns contains patterns that indicate sensitive environment variables
var sensitiveEnvPatterns = []string{
	"PASSWORD",
	"SECRET",
	"KEY",
	"TOKEN",
	"CREDENTIAL",
	"PRIVATE",
	"AUTH",
	"API_KEY",
	"APIKEY",
	"ACCESS_KEY",
	"ACCESSKEY",
}

// StatusResponse represents the response for GET /status
type StatusResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ConfigFile    string `json:"config_file,omitempty"`
	APIVersion    string `json:"api_version"`
}

// ProcessListResponse represents the response for GET /processes
type ProcessListResponse struct {
	Processes []ProcessResponse `json:"processes"`
}

// ProcessResponse represents a single process in responses
type ProcessResponse struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Restarts      int    `json:"restarts"`
	Health        string `json:"health"`
}

// ProcessDetailResponse represents the response for GET /processes/{name}
type ProcessDetailResponse struct {
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	PID           int               `json:"pid"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Restarts      int               `json:"restarts"`
	Health        string            `json:"health"`
	Healthcheck   *HealthcheckInfo  `json:"healthcheck,omitempty"`
	Cmd           string            `json:"cmd"`
	Env           map[string]string `json:"env,omitempty"`
}

// HealthcheckInfo represents health check details
type HealthcheckInfo struct {
	Enabled             bool   `json:"enabled"`
	LastCheck           string `json:"last_check,omitempty"`
	LastOutput          string `json:"last_output,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// LogsResponse represents the response for GET /logs
type LogsResponse struct {
	Logs          []LogEntryResponse `json:"logs"`
	FilteredCount int                `json:"filtered_count"`
	TotalCount    int                `json:"total_count"`
}

// LogEntryResponse represents a single log entry
type LogEntryResponse struct {
	Timestamp string `json:"timestamp"`
	Process   string `json:"process"`
	Stream    string `json:"stream"`
	Line      string `json:"line"`
}

// SuccessResponse represents a simple success response
type SuccessResponse struct {
	Success bool `json:"success"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// ToProcessResponse converts domain.ProcessInfo to ProcessResponse
func ToProcessResponse(info domain.ProcessInfo) ProcessResponse {
	return ProcessResponse{
		Name:          info.Name,
		Status:        string(info.State),
		PID:           info.PID,
		UptimeSeconds: info.UptimeSeconds(),
		Restarts:      info.RestartCount,
		Health:        string(info.Health),
	}
}

// ToProcessDetailResponse converts domain.ProcessInfo to ProcessDetailResponse
func ToProcessDetailResponse(info domain.ProcessInfo) ProcessDetailResponse {
	resp := ProcessDetailResponse{
		Name:          info.Name,
		Status:        string(info.State),
		PID:           info.PID,
		UptimeSeconds: info.UptimeSeconds(),
		Restarts:      info.RestartCount,
		Health:        string(info.Health),
		Cmd:           info.Cmd,
		Env:           filterSensitiveEnv(info.Env),
	}

	if info.HealthDetails != nil {
		resp.Healthcheck = &HealthcheckInfo{
			Enabled:             info.HealthDetails.Enabled,
			LastOutput:          info.HealthDetails.LastOutput,
			ConsecutiveFailures: info.HealthDetails.ConsecutiveFailures,
		}
		if !info.HealthDetails.LastCheck.IsZero() {
			resp.Healthcheck.LastCheck = info.HealthDetails.LastCheck.Format(time.RFC3339)
		}
	}

	return resp
}

// filterSensitiveEnv filters out sensitive environment variables
// Variables matching sensitive patterns have their values replaced with "[REDACTED]"
func filterSensitiveEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}

	filtered := make(map[string]string, len(env))
	for key, value := range env {
		if isSensitiveEnvVar(key) {
			filtered[key] = "[REDACTED]"
		} else {
			filtered[key] = value
		}
	}
	return filtered
}

// isSensitiveEnvVar checks if an environment variable name matches sensitive patterns
func isSensitiveEnvVar(name string) bool {
	upperName := strings.ToUpper(name)
	for _, pattern := range sensitiveEnvPatterns {
		if strings.Contains(upperName, pattern) {
			return true
		}
	}
	return false
}

// ToLogEntryResponse converts domain.LogEntry to LogEntryResponse
func ToLogEntryResponse(entry domain.LogEntry) LogEntryResponse {
	return LogEntryResponse{
		Timestamp: entry.Timestamp.Format(time.RFC3339Nano),
		Process:   entry.Process,
		Stream:    string(entry.Stream),
		Line:      entry.Line,
	}
}
