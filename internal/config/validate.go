package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/charliek/prox/internal/domain"
)

// domainRegex validates domain format (basic DNS name validation)
var domainRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

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

	// Validate proxy config if present
	if config.Proxy != nil {
		if config.Proxy.HTTPSPort <= 0 || config.Proxy.HTTPSPort > 65535 {
			errs = append(errs, fmt.Sprintf("proxy.https_port: must be between 1 and 65535, got %d", config.Proxy.HTTPSPort))
		}
		if config.Proxy.Enabled && config.Proxy.Domain == "" {
			errs = append(errs, "proxy.domain: required when proxy is enabled")
		}
		if config.Proxy.Domain != "" && !domainRegex.MatchString(config.Proxy.Domain) {
			errs = append(errs, fmt.Sprintf("proxy.domain: invalid domain format %q", config.Proxy.Domain))
		}
	}

	// Validate certs config if present
	if config.Certs != nil {
		if config.Certs.Dir == "" {
			errs = append(errs, "certs.dir: directory path is required")
		}
	}

	// Validate services config if present
	for name, svc := range config.Services {
		if svc.Port <= 0 || svc.Port > 65535 {
			errs = append(errs, fmt.Sprintf("services.%s.port: must be between 1 and 65535, got %d", name, svc.Port))
		}
		if err := validateServiceName(name); err != nil {
			errs = append(errs, fmt.Sprintf("services.%s: %s", name, err.Error()))
		}
		if err := validateHost(svc.Host); err != nil {
			errs = append(errs, fmt.Sprintf("services.%s.host: %s", name, err.Error()))
		}
	}

	// Validate that services require proxy to be enabled
	if len(config.Services) > 0 && (config.Proxy == nil || !config.Proxy.Enabled) {
		errs = append(errs, "services: proxy must be enabled when services are defined")
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrInvalidConfig, strings.Join(errs, "; "))
	}

	return nil
}

// validateServiceName checks if a service name is valid as a subdomain
func validateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	// Service names become subdomains, so they must be valid DNS labels
	// - Only lowercase alphanumeric and hyphens
	// - Cannot start or end with hyphen
	// - Max 63 characters
	if len(name) > 63 {
		return fmt.Errorf("service name too long (max 63 characters)")
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("service name cannot start or end with hyphen")
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("service name can only contain lowercase letters, numbers, and hyphens")
		}
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

// hostnameRegex validates hostname format (excluding IP addresses)
var hostnameRegex = regexp.MustCompile(`^(localhost|[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*)$`)

// validateHost checks if a host is a valid hostname or IP address
func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("host cannot be empty")
	}
	// First check if it's a valid IP address (handles both IPv4 and IPv6)
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	// Otherwise validate as hostname
	if !hostnameRegex.MatchString(host) {
		return fmt.Errorf("invalid host format %q", host)
	}
	return nil
}
