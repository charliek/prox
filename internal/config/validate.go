package config

import (
	"fmt"
	"strings"

	"github.com/charliek/prox/internal/domain"
)

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Validate checks the configuration for errors
func Validate(config *Config) error {
	var errs []string

	// Validate API config
	if config.API.Port < 0 || config.API.Port > 65535 {
		errs = append(errs, fmt.Sprintf("api.port: must be between 0 and 65535, got %d", config.API.Port))
	}

	// Validate processes
	if len(config.Processes) == 0 {
		errs = append(errs, "processes: at least one process must be defined")
	}

	for name, proc := range config.Processes {
		if proc.Cmd == "" {
			errs = append(errs, fmt.Sprintf("processes.%s.cmd: command is required", name))
		}

		// Validate healthcheck if present
		if proc.Healthcheck != nil {
			if proc.Healthcheck.Cmd == "" {
				errs = append(errs, fmt.Sprintf("processes.%s.healthcheck.cmd: command is required", name))
			}
			if proc.Healthcheck.Retries < 0 {
				errs = append(errs, fmt.Sprintf("processes.%s.healthcheck.retries: must be non-negative", name))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrInvalidConfig, strings.Join(errs, "; "))
	}

	return nil
}

// ValidateProcessName checks if a process name is valid
func ValidateProcessName(name string) error {
	if name == "" {
		return &ValidationError{Field: "name", Message: "process name cannot be empty"}
	}
	if strings.ContainsAny(name, " \t\n/\\") {
		return &ValidationError{Field: "name", Message: "process name cannot contain whitespace or path separators"}
	}
	return nil
}
