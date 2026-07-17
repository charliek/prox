package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// ShutdownTimeout is the default SIGTERM->SIGKILL escalation budget for
	// every process that does not set its own stop_timeout. Empty means
	// "use constants.DefaultShutdownTimeout". See stopBudgetOptions.
	ShutdownTimeout string `yaml:"shutdown_timeout"`
}

// ProxyConfig defines the HTTP/HTTPS reverse proxy configuration
type ProxyConfig struct {
	Enabled   bool           `yaml:"enabled"`
	HTTPPort  int            `yaml:"http_port"`
	HTTPSPort int            `yaml:"https_port"`
	Domain    string         `yaml:"domain"`
	Capture   *CaptureConfig `yaml:"capture,omitempty"`
}

// CaptureConfig defines request/response capture settings
type CaptureConfig struct {
	Enabled     bool   `yaml:"enabled"`
	MaxBodySize string `yaml:"max_body_size"` // e.g., "1MB", "512KB"
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
	// StopTimeout overrides Config.ShutdownTimeout for this process. Empty
	// means "use the global shutdown_timeout, else the constant default".
	// See stopBudgetOptions.
	StopTimeout string `yaml:"stop_timeout"`
}

// HealthcheckConfig defines health check configuration in YAML
type HealthcheckConfig struct {
	Cmd         string `yaml:"cmd"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	Retries     int    `yaml:"retries"`
	StartPeriod string `yaml:"start_period"`
}

type rawProxyConfig struct {
	Enabled   *bool          `yaml:"enabled,omitempty"`
	HTTPPort  int            `yaml:"http_port"`
	HTTPSPort int            `yaml:"https_port"`
	Domain    string         `yaml:"domain"`
	Capture   *CaptureConfig `yaml:"capture,omitempty"`
}

// rawConfig is used for initial YAML parsing to handle the flexible process/service format
type rawConfig struct {
	API             APIConfig              `yaml:"api"`
	EnvFile         string                 `yaml:"env_file"`
	Processes       map[string]interface{} `yaml:"processes"`
	Proxy           *rawProxyConfig        `yaml:"proxy,omitempty"`
	Services        map[string]interface{} `yaml:"services,omitempty"`
	Certs           *CertsConfig           `yaml:"certs,omitempty"`
	ShutdownTimeout string                 `yaml:"shutdown_timeout"`
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
		API:             raw.API,
		EnvFile:         raw.EnvFile,
		Processes:       make(map[string]ProcessConfig),
		Services:        make(map[string]ServiceConfig),
		Certs:           raw.Certs,
		ShutdownTimeout: raw.ShutdownTimeout,
	}
	if raw.Proxy != nil {
		config.Proxy = &ProxyConfig{
			HTTPPort:  raw.Proxy.HTTPPort,
			HTTPSPort: raw.Proxy.HTTPSPort,
			Domain:    raw.Proxy.Domain,
			Capture:   raw.Proxy.Capture,
		}
		if raw.Proxy.Enabled != nil {
			config.Proxy.Enabled = *raw.Proxy.Enabled
		}
	}

	// Apply defaults
	// Note: API.Port == 0 means dynamic assignment (handled by cli/up.go).
	// Users can still set api.port explicitly in their config if needed.
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

	// Apply proxy defaults and auto-enable logic
	if config.Proxy != nil {
		// Auto-enable proxy if either port is set and enabled was not explicitly set.
		if raw.Proxy.Enabled == nil && (config.Proxy.HTTPPort > 0 || config.Proxy.HTTPSPort > 0) {
			config.Proxy.Enabled = true
		}

		// For backwards compatibility: if proxy is enabled but no ports set,
		// default to HTTPS only (original behavior)
		if config.Proxy.Enabled && config.Proxy.HTTPPort == 0 && config.Proxy.HTTPSPort == 0 {
			config.Proxy.HTTPSPort = constants.DefaultProxyPort
		}
	}

	// Apply certs defaults only if HTTPS is being used
	if config.Certs == nil && config.Proxy != nil && config.Proxy.Enabled && config.Proxy.HTTPSPort > 0 {
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

// durationFieldOptions configures parseFieldDuration's per-field validation
// policy. The zero value (all fields false/zero) enforces only "well-formed,
// non-negative" -- callers opt into the extra behaviors they need.
type durationFieldOptions struct {
	// allowZero treats an explicitly-parsed zero duration (e.g. "0s") as
	// meaning "use the documented default" and returns it as zero without
	// applying min/max below. Healthcheck fields set this; the stop-budget
	// fields do not, so an explicit zero falls through to the min check and
	// is rejected like any other too-small value.
	allowZero bool
	// min, when nonzero, rejects any duration <= min. Ignored when zero.
	min time.Duration
	// max, when nonzero, rejects any duration > max. Ignored when zero.
	max time.Duration
}

// parseFieldDuration parses one duration field under the given policy. An
// empty string always leaves the value zero (unset) with no error -- what
// "zero" means (use a default, or absence) is up to the caller. A malformed
// or negative value always returns a clear, field-named error; min/max are
// enforced only when set in opts.
func parseFieldDuration(field, s string, opts durationFieldOptions) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", field, s)
	}
	// ParseDuration truncates a tiny negative (e.g. "-0.1ns") to 0, which would
	// otherwise slip past d < 0 and be silently accepted as zero. Check the
	// input's sign too so any negative value is rejected outright.
	if d < 0 || strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("%s: duration must not be negative (%q)", field, s)
	}
	if opts.allowZero && d == 0 {
		return 0, nil
	}
	if opts.min > 0 && d <= opts.min {
		return 0, fmt.Errorf("%s: must be greater than %s (%s is reserved for the SIGKILL escalation): %q",
			field, trimZeroDuration(opts.min), trimZeroDuration(opts.min), s)
	}
	if opts.max > 0 && d > opts.max {
		return 0, fmt.Errorf("%s: must not exceed %s: %q", field, trimZeroDuration(opts.max), s)
	}
	return d, nil
}

// trimZeroDuration renders a duration without the trailing zero-valued units
// time.Duration.String always includes (e.g. 10*time.Minute -> "10m0s", not
// "10m"). Only exercised on the fixed constants used as min/max (KillGrace,
// MaxStopTimeout) in error messages, so only whole hour/minute/second values
// need the shorter form; anything else falls back to the standard format.
func trimZeroDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d >= time.Second && d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return d.String()
	}
}

// healthDurationOptions is the validation policy for healthcheck duration
// fields (interval/timeout/start_period): zero means "use the documented
// default" and there is no upper cap. This preserves the pre-#35 behavior
// exactly (issue #31).
var healthDurationOptions = durationFieldOptions{allowZero: true}

// stopBudgetOptions is the validation policy shared by the top-level
// shutdown_timeout and per-process stop_timeout fields (#35, D1): no
// "zero means default" special case -- an explicit zero is just another
// too-small value -- and a bounded range of (KillGrace, MaxStopTimeout] so
// every static ceiling that wraps the configured value stays boundable.
var stopBudgetOptions = durationFieldOptions{
	min: constants.KillGrace,
	max: constants.MaxStopTimeout,
}

// parseHealthDuration parses one healthcheck duration field under
// healthDurationOptions. See parseFieldDuration.
func parseHealthDuration(field, s string) (time.Duration, error) {
	return parseFieldDuration(field, s, healthDurationOptions)
}

// ShutdownTimeoutDuration parses Config.ShutdownTimeout under
// stopBudgetOptions. Zero (empty string) means unset -- callers fall back to
// constants.DefaultShutdownTimeout. Validate has already confirmed the value
// is well-formed and in range for any config that went through Load/Parse; a
// parse error here is only reachable for a Config built directly (e.g. tests)
// without going through Validate.
func (c *Config) ShutdownTimeoutDuration() (time.Duration, error) {
	return parseFieldDuration("shutdown_timeout", c.ShutdownTimeout, stopBudgetOptions)
}

// StopTimeoutDuration parses ProcessConfig.StopTimeout under
// stopBudgetOptions. Zero (empty string) means unset -- callers fall back to
// the global shutdown_timeout, then constants.DefaultShutdownTimeout. See
// Config.ShutdownTimeoutDuration for the same Validate-already-checked note.
func (p *ProcessConfig) StopTimeoutDuration() (time.Duration, error) {
	return parseFieldDuration("stop_timeout", p.StopTimeout, stopBudgetOptions)
}

// ToDomain converts the YAML healthcheck config to the domain type, parsing the
// duration strings. Returns the first field error (parse or negative). Empty
// fields stay zero so WithDefaults applies the default.
func (hc *HealthcheckConfig) ToDomain() (*domain.HealthConfig, error) {
	interval, err := parseHealthDuration("interval", hc.Interval)
	if err != nil {
		return nil, err
	}
	timeout, err := parseHealthDuration("timeout", hc.Timeout)
	if err != nil {
		return nil, err
	}
	startPeriod, err := parseHealthDuration("start_period", hc.StartPeriod)
	if err != nil {
		return nil, err
	}
	return &domain.HealthConfig{
		Cmd:         hc.Cmd,
		Interval:    interval,
		Timeout:     timeout,
		Retries:     hc.Retries,
		StartPeriod: startPeriod,
	}, nil
}

// ParseSize parses a human-readable size string (e.g., "1MB", "512KB", "1024")
// into bytes. Supported suffixes: B, KB, MB, GB (case-insensitive).
// If no suffix is provided, the value is treated as bytes.
func ParseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	var multiplier int64
	suffix := ""

	// Find where the numeric part ends
	numEnd := 0
	for i, c := range s {
		if c < '0' || c > '9' {
			numEnd = i
			suffix = s[i:]
			break
		}
		numEnd = i + 1
	}

	if numEnd == 0 {
		return 0, fmt.Errorf("invalid size: %s", s)
	}

	numStr := s[:numEnd]
	value, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size number: %s", numStr)
	}

	if value < 0 {
		return 0, fmt.Errorf("size cannot be negative: %s", s)
	}

	switch strings.TrimSpace(suffix) {
	case "", "B":
		multiplier = 1
	case "KB", "K":
		multiplier = 1024
	case "MB", "M":
		multiplier = 1024 * 1024
	case "GB", "G":
		multiplier = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid size suffix: %s", suffix)
	}

	return value * multiplier, nil
}
