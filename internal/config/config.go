package config

import (
	"fmt"
	"os"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"gopkg.in/yaml.v3"
)

// Config represents the top-level prox configuration
type Config struct {
	API       APIConfig                `yaml:"api"`
	EnvFile   string                   `yaml:"env_file"`
	Processes map[string]ProcessConfig `yaml:"processes"`
	Proxy     *ProxyConfig             `yaml:"proxy,omitempty"`
	Services  map[string]ServiceConfig `yaml:"services,omitempty"`
	Certs     *CertsConfig             `yaml:"certs,omitempty"`
}

// ProxyConfig defines the HTTPS reverse proxy configuration
type ProxyConfig struct {
	Enabled   bool   `yaml:"enabled"`
	HTTPSPort int    `yaml:"https_port"`
	Domain    string `yaml:"domain"`
}

// ServiceConfig represents a service routing configuration that can be either
// a simple port number or an expanded form with additional options
type ServiceConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

// CertsConfig defines certificate configuration
type CertsConfig struct {
	Dir          string `yaml:"dir"`
	AutoGenerate bool   `yaml:"auto_generate"`
}

// APIConfig defines the HTTP API configuration
type APIConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
	Auth *bool  `yaml:"auth,omitempty"` // nil = auto-determine based on host
}

// ProcessConfig represents a process configuration that can be either
// a simple string command or an expanded form with additional options
type ProcessConfig struct {
	Cmd         string             `yaml:"cmd"`
	Env         map[string]string  `yaml:"env"`
	EnvFile     string             `yaml:"env_file"`
	Healthcheck *HealthcheckConfig `yaml:"healthcheck"`
}

// HealthcheckConfig defines health check configuration in YAML
type HealthcheckConfig struct {
	Cmd         string `yaml:"cmd"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	Retries     int    `yaml:"retries"`
	StartPeriod string `yaml:"start_period"`
}

// rawConfig is used for initial YAML parsing to handle the flexible process/service format
type rawConfig struct {
	API       APIConfig              `yaml:"api"`
	EnvFile   string                 `yaml:"env_file"`
	Processes map[string]interface{} `yaml:"processes"`
	Proxy     *ProxyConfig           `yaml:"proxy,omitempty"`
	Services  map[string]interface{} `yaml:"services,omitempty"`
	Certs     *CertsConfig           `yaml:"certs,omitempty"`
}

// Load reads and parses a configuration file
func Load(path string) (*Config, error) {
	// First check if file exists
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", domain.ErrConfigNotFound, path)
		}
		return nil, fmt.Errorf("checking config file: %w", err)
	}

	// Check file permissions for security
	if err := CheckFilePermissions(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	return Parse(data)
}

// Parse parses configuration from YAML bytes
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}

	config := &Config{
		API:       raw.API,
		EnvFile:   raw.EnvFile,
		Processes: make(map[string]ProcessConfig),
		Proxy:     raw.Proxy,
		Services:  make(map[string]ServiceConfig),
		Certs:     raw.Certs,
	}

	// Apply defaults
	if config.API.Port == 0 {
		config.API.Port = constants.DefaultAPIPort
	}
	if config.API.Host == "" {
		config.API.Host = constants.DefaultAPIHost
	}

	// Parse processes (can be string or expanded form)
	for name, value := range raw.Processes {
		proc, err := parseProcessConfig(name, value)
		if err != nil {
			return nil, fmt.Errorf("process %q: %w", name, err)
		}
		config.Processes[name] = proc
	}

	// Parse services (can be int port or expanded form)
	for name, value := range raw.Services {
		svc, err := parseServiceConfig(name, value)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		config.Services[name] = svc
	}

	// Apply proxy defaults
	if config.Proxy != nil {
		if config.Proxy.HTTPSPort == 0 {
			config.Proxy.HTTPSPort = constants.DefaultProxyPort
		}
	}

	// Apply certs defaults
	if config.Certs == nil && config.Proxy != nil {
		config.Certs = &CertsConfig{
			AutoGenerate: true, // Default to auto-generating certs
		}
	}
	if config.Certs != nil {
		if config.Certs.Dir == "" {
			config.Certs.Dir = constants.DefaultCertsDir
		}
	}

	if err := Validate(config); err != nil {
		return nil, err
	}

	return config, nil
}

// parseProcessConfig handles both simple and expanded process definitions
func parseProcessConfig(name string, value interface{}) (ProcessConfig, error) {
	switch v := value.(type) {
	case string:
		// Simple form: web: npm run dev
		return ProcessConfig{Cmd: v}, nil
	case map[string]interface{}:
		// Expanded form: re-marshal and unmarshal to struct
		data, err := yaml.Marshal(v)
		if err != nil {
			return ProcessConfig{}, fmt.Errorf("marshaling process config: %w", err)
		}
		var proc ProcessConfig
		if err := yaml.Unmarshal(data, &proc); err != nil {
			return ProcessConfig{}, fmt.Errorf("unmarshaling process config: %w", err)
		}
		return proc, nil
	default:
		return ProcessConfig{}, fmt.Errorf("invalid process configuration type: %T", value)
	}
}

// parseServiceConfig handles both simple (port only) and expanded service definitions
func parseServiceConfig(name string, value interface{}) (ServiceConfig, error) {
	switch v := value.(type) {
	case int:
		// Simple form: app: 3000
		return ServiceConfig{Port: v, Host: "localhost"}, nil
	case float64:
		// YAML may parse integers as float64
		return ServiceConfig{Port: int(v), Host: "localhost"}, nil
	case map[string]interface{}:
		// Expanded form: re-marshal and unmarshal to struct
		data, err := yaml.Marshal(v)
		if err != nil {
			return ServiceConfig{}, fmt.Errorf("marshaling service config: %w", err)
		}
		var svc ServiceConfig
		if err := yaml.Unmarshal(data, &svc); err != nil {
			return ServiceConfig{}, fmt.Errorf("unmarshaling service config: %w", err)
		}
		// Apply default host if not specified
		if svc.Host == "" {
			svc.Host = "localhost"
		}
		return svc, nil
	default:
		return ServiceConfig{}, fmt.Errorf("invalid service configuration type: %T", value)
	}
}

// ToDomainProcesses converts config processes to domain ProcessConfig slice
func (c *Config) ToDomainProcesses() []domain.ProcessConfig {
	processes := make([]domain.ProcessConfig, 0, len(c.Processes))
	for name, proc := range c.Processes {
		domainProc := domain.ProcessConfig{
			Name:    name,
			Cmd:     proc.Cmd,
			Env:     proc.Env,
			EnvFile: proc.EnvFile,
		}
		if proc.Healthcheck != nil {
			hc := &domain.HealthConfig{
				Cmd:     proc.Healthcheck.Cmd,
				Retries: proc.Healthcheck.Retries,
			}
			if proc.Healthcheck.Interval != "" {
				if d, err := time.ParseDuration(proc.Healthcheck.Interval); err == nil {
					hc.Interval = d
				}
			}
			if proc.Healthcheck.Timeout != "" {
				if d, err := time.ParseDuration(proc.Healthcheck.Timeout); err == nil {
					hc.Timeout = d
				}
			}
			if proc.Healthcheck.StartPeriod != "" {
				if d, err := time.ParseDuration(proc.Healthcheck.StartPeriod); err == nil {
					hc.StartPeriod = d
				}
			}
			domainProc.Healthcheck = hc
		}
		processes = append(processes, domainProc)
	}
	return processes
}
