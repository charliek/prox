package config

import (
	"fmt"
	"os"
	"sort"
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
	// Dependencies are external readiness gates (plan 013 D1): each is probed
	// (tcp/url/cmd) until ready, optionally after running a start command.
	// Dependencies are dependency-graph roots -- they carry no depends_on.
	Dependencies map[string]DependencyConfig `yaml:"dependencies,omitempty"`
	// Tasks are run-to-completion commands (plan 013 D1) that may depend on
	// dependencies and other tasks. See TaskConfig.
	Tasks map[string]TaskConfig `yaml:"tasks,omitempty"`
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

// CaptureEffectivelyEnabled reports whether capture is actually on for this
// project (plan 012 D1, C4): the proxy itself must be enabled AND its capture
// config -- always materialized by Parse whenever a proxy: block exists, see
// materializeCapture -- must have Enabled == true. Capture is gated on the
// proxy being enabled at every use site (the register wire, the CLI hint);
// this is the single place that encodes the gate. Safe on a nil receiver (no
// proxy configured at all -> false).
func (p *ProxyConfig) CaptureEffectivelyEnabled() bool {
	if p == nil || !p.Enabled {
		return false
	}
	return p.Capture != nil && p.Capture.Enabled
}

// CaptureConfig defines request/response capture settings. It is always the
// MATERIALIZED form: Parse builds one (via materializeCapture) whenever a
// proxy: block exists at all, even with no capture: block or an empty one, so
// a project's capture policy is never nil to check once cfg.Proxy is non-nil.
// Its own raw YAML parsing lives on rawCaptureConfig, which carries the
// Enabled tri-state Parse needs to distinguish "absent" from "explicit false"
// (plan 012 D1, C4); CaptureConfig no longer doubles as its own raw type.
type CaptureConfig struct {
	// Enabled defaults to true whenever a proxy: block exists (capture-by-
	// default, plan 012 D1): materializeCapture sets it unless the config
	// explicitly says `enabled: false`. Effective capture also requires the
	// proxy itself to be enabled -- see ProxyConfig.CaptureEffectivelyEnabled.
	Enabled     bool   `yaml:"enabled"`
	MaxBodySize string `yaml:"max_body_size"` // e.g., "1MB", "512KB"
	// DiskBudget is the ceiling on the TOTAL bytes of spilled capture body files
	// on disk (#69), e.g. "512MB", "2GB". Empty means "use the default"
	// (constants.DefaultCaptureDiskBudget, 1GiB). In the shared daemon an explicit
	// value can only LOWER the daemon-wide effective bound, never raise it above
	// the default; note it may legitimately be smaller than max_body_size (a
	// single spilled body is then the oldest-and-only group and is evicted by the
	// same loop). Parsed with ParseSize.
	DiskBudget string `yaml:"disk_budget"`
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
	// DependsOn lists dependencies: and/or tasks: names that must be ready
	// before this process starts (plan 013 D1). Process names are NOT valid
	// targets; validation rejects them. Unlike dependency/task blocks (which
	// reject unknown keys), a process's other fields still follow the lenient
	// re-marshal parse path, so depends_on is simply another known field here.
	DependsOn []string `yaml:"depends_on"`
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
	Enabled   *bool             `yaml:"enabled,omitempty"`
	HTTPPort  int               `yaml:"http_port"`
	HTTPSPort int               `yaml:"https_port"`
	Domain    string            `yaml:"domain"`
	Capture   *rawCaptureConfig `yaml:"capture,omitempty"`
}

// rawCaptureConfig is the raw YAML parse shape for a proxy's capture: block
// (plan 012 D1, C4). It mirrors rawProxyConfig's Enabled tri-state pattern:
// nil means "key absent" (materializeCapture defaults it to true, the
// capture-by-default flip), so an explicit `enabled: false` can be told apart
// from an absent key. The remaining fields are absorbed straight from what
// used to live directly on CaptureConfig, which stops doubling as its own raw
// parse type.
type rawCaptureConfig struct {
	Enabled     *bool  `yaml:"enabled,omitempty"`
	MaxBodySize string `yaml:"max_body_size"`
	DiskBudget  string `yaml:"disk_budget"`
}

// materializeCapture builds the CaptureConfig for a proxy block (plan 012 D1,
// C4). Enabled defaults to true whenever a proxy: block exists at all --
// including when raw is nil (no capture: block) or every field on it is zero
// (an empty capture: block) -- so capture is on by default the moment a
// project turns the proxy on. An explicit `enabled: false` (or `true`)
// survives untouched. Whether capture is EFFECTIVELY on also requires the
// proxy itself to be enabled; that gate lives in
// ProxyConfig.CaptureEffectivelyEnabled, applied at use sites, not here --
// materialization always happens so cfg.Proxy.Capture is never nil once
// cfg.Proxy is non-nil.
func materializeCapture(raw *rawCaptureConfig) *CaptureConfig {
	cfg := &CaptureConfig{Enabled: true}
	if raw == nil {
		return cfg
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	cfg.MaxBodySize = raw.MaxBodySize
	cfg.DiskBudget = raw.DiskBudget
	return cfg
}

// rawConfig is used for initial YAML parsing to handle the flexible process/service format
type rawConfig struct {
	API       APIConfig              `yaml:"api"`
	EnvFile   string                 `yaml:"env_file"`
	Processes map[string]interface{} `yaml:"processes"`
	Proxy     *rawProxyConfig        `yaml:"proxy,omitempty"`
	Services  map[string]interface{} `yaml:"services,omitempty"`
	Certs     *CertsConfig           `yaml:"certs,omitempty"`
	// Dependencies and Tasks are held as raw maps so parseDependencies /
	// parseTasks can reject unknown keys precisely (plan 013 D1, C1): the
	// re-marshal parse path used for processes/services silently DROPS unknown
	// fields, which would let a typo'd key pass unnoticed. The strict parsers
	// inspect the raw map form directly to name the offending key.
	Dependencies    map[string]interface{} `yaml:"dependencies,omitempty"`
	Tasks           map[string]interface{} `yaml:"tasks,omitempty"`
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
		Dependencies:    make(map[string]DependencyConfig),
		Tasks:           make(map[string]TaskConfig),
		Certs:           raw.Certs,
		ShutdownTimeout: raw.ShutdownTimeout,
	}
	if raw.Proxy != nil {
		config.Proxy = &ProxyConfig{
			HTTPPort:  raw.Proxy.HTTPPort,
			HTTPSPort: raw.Proxy.HTTPSPort,
			Domain:    raw.Proxy.Domain,
			// Materialized unconditionally -- even with no capture: block at all --
			// so capture is on by default the moment a proxy: block exists (plan 012
			// D1, C4). See materializeCapture.
			Capture: materializeCapture(raw.Proxy.Capture),
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

	// Parse dependencies and tasks with STRICT unknown-key rejection (plan 013
	// D1, C1). These are structural errors (malformed shape / unknown keys):
	// they must be reported before semantic Validate runs, because a
	// half-formed check block cannot be meaningfully validated. Collected across
	// both blocks and sorted so the report is deterministic regardless of Go's
	// map iteration order.
	//
	// Accepted limitation (plan 013 D1, C1): the strict parsers inspect the
	// generic map[string]interface{} form, which yaml.v3 has already produced --
	// so a YAML anchor/alias or a `<<` merge key that resolves to a key already
	// present is collapsed into one map entry BEFORE we see it, and a duplicate
	// logical key can slip past unknown-key/duplicate detection. Detecting that
	// would require re-parsing at the yaml.Node level. We accept it: this is the
	// same behavior the legacy processes:/services: re-marshal path has always
	// had, prox config is local developer-authored input (not a security
	// boundary), and the failure mode is a last-write-wins merge, not a crash.
	var structuralErrs []string
	structuralErrs = append(structuralErrs, parseDependencies(raw.Dependencies, config.Dependencies)...)
	structuralErrs = append(structuralErrs, parseTasks(raw.Tasks, config.Tasks)...)
	if len(structuralErrs) > 0 {
		sort.Strings(structuralErrs)
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidConfig, strings.Join(structuralErrs, "; "))
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
