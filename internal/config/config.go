package config

import (
	"fmt"
	"os"
	"time"

	"github.com/charliek/prox/internal/domain"
	"gopkg.in/yaml.v3"
)

// Config represents the top-level prox configuration
type Config struct {
	API       APIConfig                `yaml:"api"`
	EnvFile   string                   `yaml:"env_file"`
	Processes map[string]ProcessConfig `yaml:"processes"`
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

// rawConfig is used for initial YAML parsing to handle the flexible process format
type rawConfig struct {
	API       APIConfig              `yaml:"api"`
	EnvFile   string                 `yaml:"env_file"`
	Processes map[string]interface{} `yaml:"processes"`
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
	}

	// Apply defaults
	if config.API.Port == 0 {
		config.API.Port = 5555
	}
	if config.API.Host == "" {
		config.API.Host = "127.0.0.1"
	}

	// Parse processes (can be string or expanded form)
	for name, value := range raw.Processes {
		proc, err := parseProcessConfig(name, value)
		if err != nil {
			return nil, fmt.Errorf("process %q: %w", name, err)
		}
		config.Processes[name] = proc
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
