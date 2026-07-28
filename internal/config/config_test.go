package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_SimpleForm(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "testdata", "configs", "simple.yaml"))
	require.NoError(t, err)

	assert.Equal(t, 0, cfg.API.Port) // 0 means dynamic assignment
	assert.Equal(t, "127.0.0.1", cfg.API.Host)
	assert.Len(t, cfg.Processes, 3)

	assert.Equal(t, "npm run dev", cfg.Processes["web"].Cmd)
	assert.Equal(t, "go run ./cmd/server", cfg.Processes["api"].Cmd)
	assert.Equal(t, "python worker.py", cfg.Processes["worker"].Cmd)
}

func TestLoad_ExpandedForm(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "testdata", "configs", "expanded.yaml"))
	require.NoError(t, err)

	assert.Equal(t, 8080, cfg.API.Port)
	assert.Equal(t, "0.0.0.0", cfg.API.Host)
	assert.Equal(t, ".env", cfg.EnvFile)
	assert.Len(t, cfg.Processes, 2)

	// Simple form process
	assert.Equal(t, "npm run dev", cfg.Processes["web"].Cmd)

	// Expanded form process
	api := cfg.Processes["api"]
	assert.Equal(t, "go run ./cmd/server", api.Cmd)
	assert.Equal(t, "8080", api.Env["PORT"])
	assert.Equal(t, "true", api.Env["DEBUG"])

	// Healthcheck
	require.NotNil(t, api.Healthcheck)
	assert.Equal(t, "curl -f http://localhost:8080/health", api.Healthcheck.Cmd)
	assert.Equal(t, "10s", api.Healthcheck.Interval)
	assert.Equal(t, "5s", api.Healthcheck.Timeout)
	assert.Equal(t, 3, api.Healthcheck.Retries)
	assert.Equal(t, "30s", api.Healthcheck.StartPeriod)
}

func TestLoad_ValidationError_NoCmd(t *testing.T) {
	_, err := Load(filepath.Join("..", "..", "testdata", "configs", "invalid_no_cmd.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cmd")
	assert.Contains(t, err.Error(), "required")
}

func TestLoad_ValidationError_InvalidPort(t *testing.T) {
	_, err := Load(filepath.Join("..", "..", "testdata", "configs", "invalid_port.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port")
}

func TestLoad_ValidationError_NoProcesses(t *testing.T) {
	_, err := Load(filepath.Join("..", "..", "testdata", "configs", "empty_processes.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one process")
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("nonexistent.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestParse_InvalidYAML(t *testing.T) {
	_, err := Parse([]byte("invalid: yaml: content:"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing yaml")
}

func TestParse_HealthcheckInvalidDuration(t *testing.T) {
	yamlData := []byte(`
api:
  port: 5555
processes:
  api:
    cmd: go run ./cmd/server
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 3x
`)
	cfg, err := Parse(yamlData)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.True(t, errors.Is(err, domain.ErrInvalidConfig))
	assert.Contains(t, err.Error(), "processes.api.healthcheck.interval")
}

func TestHealthcheckConfig_ToDomain(t *testing.T) {
	t.Run("valid non-default durations populate all fields", func(t *testing.T) {
		hc := &HealthcheckConfig{
			Cmd:         "curl -f http://localhost/health",
			Interval:    "7s",
			Timeout:     "3s",
			Retries:     5,
			StartPeriod: "11s",
		}
		got, err := hc.ToDomain()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "curl -f http://localhost/health", got.Cmd)
		assert.Equal(t, 7*time.Second, got.Interval)
		assert.Equal(t, 3*time.Second, got.Timeout)
		assert.Equal(t, 5, got.Retries)
		assert.Equal(t, 11*time.Second, got.StartPeriod)
	})

	t.Run("empty durations convert to zero", func(t *testing.T) {
		hc := &HealthcheckConfig{Cmd: "true"}
		got, err := hc.ToDomain()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "true", got.Cmd)
		assert.Equal(t, time.Duration(0), got.Interval)
		assert.Equal(t, time.Duration(0), got.Timeout)
		assert.Equal(t, 0, got.Retries)
		assert.Equal(t, time.Duration(0), got.StartPeriod)
	})

	t.Run("explicit zero values convert to zero and default via WithDefaults", func(t *testing.T) {
		hc := &HealthcheckConfig{
			Cmd:         "true",
			Interval:    "0s",
			Timeout:     "0s",
			Retries:     0,
			StartPeriod: "0s",
		}
		got, err := hc.ToDomain()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, time.Duration(0), got.Interval)
		assert.Equal(t, time.Duration(0), got.Timeout)
		assert.Equal(t, 0, got.Retries)
		assert.Equal(t, time.Duration(0), got.StartPeriod)

		// Zero means "use the documented default" (D5).
		withDefaults := got.WithDefaults()
		assert.Equal(t, 10*time.Second, withDefaults.Interval)
		assert.Equal(t, 5*time.Second, withDefaults.Timeout)
		assert.Equal(t, 3, withDefaults.Retries)
		assert.Equal(t, 30*time.Second, withDefaults.StartPeriod)
	})

	t.Run("malformed duration returns error", func(t *testing.T) {
		hc := &HealthcheckConfig{Cmd: "true", Interval: "3x"}
		got, err := hc.ToDomain()
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "interval")
	})

	t.Run("negative duration returns error", func(t *testing.T) {
		hc := &HealthcheckConfig{Cmd: "true", Timeout: "-5s"}
		got, err := hc.ToDomain()
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "timeout")
		assert.Contains(t, err.Error(), "negative")
	})

	t.Run("tiny negative duration truncating to zero still returns error", func(t *testing.T) {
		// time.ParseDuration("-0.1ns") truncates to 0; the sign check must
		// still reject it rather than let WithDefaults silently substitute.
		hc := &HealthcheckConfig{Cmd: "true", Interval: "-0.1ns"}
		got, err := hc.ToDomain()
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "interval")
		assert.Contains(t, err.Error(), "negative")
	})
}

// TestShutdownTimeoutDuration_Matrix covers the #35/D1 validation policy for
// the top-level shutdown_timeout field: empty is unset (zero, no error);
// malformed/negative/zero/sub-KillGrace values are rejected uniformly; values
// in (KillGrace, MaxStopTimeout] are accepted, including the boundaries.
func TestShutdownTimeoutDuration_Matrix(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantDur   time.Duration
		wantErr   bool
		errSubstr string
	}{
		{name: "omitted is unset", value: "", wantDur: 0},
		{name: "valid value parses", value: "30s", wantDur: 30 * time.Second},
		{name: "malformed is rejected", value: "3x", wantErr: true, errSubstr: "invalid duration"},
		{name: "negative is rejected", value: "-5s", wantErr: true, errSubstr: "negative"},
		{name: "explicit zero is rejected", value: "0s", wantErr: true, errSubstr: "greater than 2s"},
		{name: "exactly KillGrace (2s) is rejected", value: "2s", wantErr: true, errSubstr: "greater than 2s"},
		{name: "just above KillGrace (2.5s) is accepted", value: "2.5s", wantDur: 2500 * time.Millisecond},
		{name: "exactly MaxStopTimeout (10m) is accepted", value: "10m", wantDur: 10 * time.Minute},
		{name: "above MaxStopTimeout is rejected", value: "10m1s", wantErr: true, errSubstr: "must not exceed 10m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ShutdownTimeout: tt.value}
			got, err := cfg.ShutdownTimeoutDuration()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "shutdown_timeout")
				assert.Contains(t, err.Error(), tt.errSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantDur, got)
		})
	}
}

// TestStopTimeoutDuration_Matrix mirrors TestShutdownTimeoutDuration_Matrix
// for the per-process stop_timeout field; same policy, different field name
// in the error.
func TestStopTimeoutDuration_Matrix(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantDur   time.Duration
		wantErr   bool
		errSubstr string
	}{
		{name: "omitted is unset", value: "", wantDur: 0},
		{name: "valid value parses", value: "45s", wantDur: 45 * time.Second},
		{name: "malformed is rejected", value: "bogus", wantErr: true, errSubstr: "invalid duration"},
		{name: "negative is rejected", value: "-1s", wantErr: true, errSubstr: "negative"},
		{name: "explicit zero is rejected", value: "0s", wantErr: true, errSubstr: "greater than 2s"},
		{name: "exactly KillGrace (2s) is rejected", value: "2s", wantErr: true, errSubstr: "greater than 2s"},
		{name: "just above KillGrace (2.5s) is accepted", value: "2.5s", wantDur: 2500 * time.Millisecond},
		{name: "exactly MaxStopTimeout (10m) is accepted", value: "10m", wantDur: 10 * time.Minute},
		{name: "above MaxStopTimeout is rejected", value: "10m1s", wantErr: true, errSubstr: "must not exceed 10m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &ProcessConfig{Cmd: "true", StopTimeout: tt.value}
			got, err := proc.StopTimeoutDuration()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "stop_timeout")
				assert.Contains(t, err.Error(), tt.errSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantDur, got)
		})
	}
}

// TestShutdownTimeoutDuration_ErrorMessageFormat pins the exact error text
// from the plan (D1): field-named, and states the KillGrace rationale.
func TestShutdownTimeoutDuration_ErrorMessageFormat(t *testing.T) {
	cfg := &Config{ShutdownTimeout: "1s"}
	_, err := cfg.ShutdownTimeoutDuration()
	require.Error(t, err)
	assert.Equal(t, `shutdown_timeout: must be greater than 2s (2s is reserved for the SIGKILL escalation): "1s"`, err.Error())
}

// TestParse_ShutdownAndStopTimeout verifies the new YAML fields round-trip
// through Parse (both top-level and per-process, expanded form).
func TestParse_ShutdownAndStopTimeout(t *testing.T) {
	yamlData := []byte(`
shutdown_timeout: 30s
processes:
  web:
    cmd: npm run dev
    stop_timeout: 45s
  worker: python worker.py
`)
	cfg, err := Parse(yamlData)
	require.NoError(t, err)
	assert.Equal(t, "30s", cfg.ShutdownTimeout)
	assert.Equal(t, "45s", cfg.Processes["web"].StopTimeout)
	assert.Equal(t, "", cfg.Processes["worker"].StopTimeout)
}

// TestLoad_ValidationError_ShutdownTimeout and its stop_timeout counterpart
// verify the bad values surface through the full Load path (Parse+Validate),
// field-named per the #31 pattern.
func TestLoad_ValidationError_ShutdownTimeout(t *testing.T) {
	yamlData := []byte(`
shutdown_timeout: 1s
processes:
  web: npm run dev
`)
	_, err := Parse(yamlData)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown_timeout")
	assert.Contains(t, err.Error(), "greater than 2s")
}

func TestLoad_ValidationError_StopTimeout(t *testing.T) {
	yamlData := []byte(`
processes:
  web:
    cmd: npm run dev
    stop_timeout: 15m
`)
	_, err := Parse(yamlData)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "processes.web.stop_timeout")
	assert.Contains(t, err.Error(), "must not exceed 10m")
}

// TestHealthcheckConfig_ToDomain_RegressionAfterStopBudgetParser: the parser
// generalization for #35 must not change healthcheck semantics (non-goal).
// Explicit zero stays accepted (use-default) and durations well past
// MaxStopTimeout are still accepted -- healthcheck fields have no cap.
func TestHealthcheckConfig_ToDomain_RegressionAfterStopBudgetParser(t *testing.T) {
	t.Run("explicit zero interval still accepted", func(t *testing.T) {
		hc := &HealthcheckConfig{Cmd: "true", Interval: "0s"}
		got, err := hc.ToDomain()
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), got.Interval)
	})

	t.Run("duration beyond MaxStopTimeout still accepted (no cap leak)", func(t *testing.T) {
		hc := &HealthcheckConfig{Cmd: "true", StartPeriod: "15m"}
		got, err := hc.ToDomain()
		require.NoError(t, err)
		assert.Equal(t, 15*time.Minute, got.StartPeriod)
	})
}

func TestParse_ProxyConfig(t *testing.T) {
	t.Run("parses full proxy config", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  https_port: 8443
  domain: local.myapp.dev

services:
  app: 3000
  api:
    port: 8000
    host: 127.0.0.1

certs:
  dir: /custom/certs
  auto_generate: true
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// Check proxy
		require.NotNil(t, cfg.Proxy)
		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 8443, cfg.Proxy.HTTPSPort)
		assert.Equal(t, "local.myapp.dev", cfg.Proxy.Domain)

		// Check services
		assert.Len(t, cfg.Services, 2)
		assert.Equal(t, 3000, cfg.Services["app"].Port)
		assert.Equal(t, "localhost", cfg.Services["app"].Host) // Default host
		assert.Equal(t, 8000, cfg.Services["api"].Port)
		assert.Equal(t, "127.0.0.1", cfg.Services["api"].Host)

		// Check certs
		require.NotNil(t, cfg.Certs)
		assert.Equal(t, "/custom/certs", cfg.Certs.Dir)
		assert.True(t, cfg.Certs.AutoGenerate)
	})

	t.Run("applies proxy defaults", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.test.dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		assert.Equal(t, 6789, cfg.Proxy.HTTPSPort) // Default port
		require.NotNil(t, cfg.Certs)
		assert.Equal(t, "~/.prox/certs", cfg.Certs.Dir) // Default certs dir
		assert.True(t, cfg.Certs.AutoGenerate)
	})

	t.Run("no proxy config is valid", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Nil(t, cfg.Proxy)
		assert.Empty(t, cfg.Services)
		assert.Nil(t, cfg.Certs)
	})

	t.Run("service with integer port as float64", func(t *testing.T) {
		// YAML parsers may parse integers as float64
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.Equal(t, 3000, cfg.Services["app"].Port)
	})

	t.Run("proxy auto-creates certs config", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// Certs config should be auto-created when proxy is enabled with HTTPS
		require.NotNil(t, cfg.Certs)
		assert.Equal(t, "~/.prox/certs", cfg.Certs.Dir)
		assert.True(t, cfg.Certs.AutoGenerate)
	})

	t.Run("parses HTTP port config", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  http_port: 6788
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// Check proxy auto-enabled and HTTP port set
		require.NotNil(t, cfg.Proxy)
		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 6788, cfg.Proxy.HTTPPort)
		assert.Equal(t, 0, cfg.Proxy.HTTPSPort) // No HTTPS

		// No certs config for HTTP only
		assert.Nil(t, cfg.Certs)
	})

	t.Run("parses dual stack proxy config", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  http_port: 6788
  https_port: 6789
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// Check both ports set and proxy enabled
		require.NotNil(t, cfg.Proxy)
		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 6788, cfg.Proxy.HTTPPort)
		assert.Equal(t, 6789, cfg.Proxy.HTTPSPort)

		// Certs config should be created for HTTPS
		require.NotNil(t, cfg.Certs)
		assert.True(t, cfg.Certs.AutoGenerate)
	})

	t.Run("proxy auto-enables when http_port set", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  http_port: 6788
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// Proxy should be auto-enabled
		require.NotNil(t, cfg.Proxy)
		assert.True(t, cfg.Proxy.Enabled)
	})

	t.Run("explicit enabled false is respected when port is set", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: false
  http_port: 6788
  domain: local.test.dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy)
		assert.False(t, cfg.Proxy.Enabled)
		assert.Equal(t, 6788, cfg.Proxy.HTTPPort)
		assert.Equal(t, 0, cfg.Proxy.HTTPSPort)
		assert.Nil(t, cfg.Certs)
	})

	t.Run("parses capture config", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  http_port: 6788
  domain: local.test.dev
  capture:
    enabled: true
    max_body_size: "2MB"

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy)
		require.NotNil(t, cfg.Proxy.Capture)
		assert.True(t, cfg.Proxy.Capture.Enabled)
		assert.Equal(t, "2MB", cfg.Proxy.Capture.MaxBodySize)
	})

	// TestParse_CaptureMaterialization pins the capture-by-default materialization
	// matrix (plan 012 D1, C4): Parse always builds cfg.Proxy.Capture whenever a
	// proxy: block exists at all -- with no capture: block, an empty one, or an
	// explicit enabled value -- defaulting Enabled to true and letting an explicit
	// `enabled: false` survive.
	t.Run("no capture block materializes Capture with Enabled true", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  http_port: 6788
  domain: local.test.dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy)
		require.NotNil(t, cfg.Proxy.Capture, "capture must be materialized even with no capture: block")
		assert.True(t, cfg.Proxy.Capture.Enabled)
	})

	t.Run("empty capture block materializes Enabled true", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  http_port: 6788
  domain: local.test.dev
  capture: {}
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy.Capture)
		assert.True(t, cfg.Proxy.Capture.Enabled)
	})

	t.Run("explicit enabled false survives", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  http_port: 6788
  domain: local.test.dev
  capture:
    enabled: false
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy.Capture)
		assert.False(t, cfg.Proxy.Capture.Enabled)
	})

	t.Run("explicit enabled true survives", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  http_port: 6788
  domain: local.test.dev
  capture:
    enabled: true
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy.Capture)
		assert.True(t, cfg.Proxy.Capture.Enabled)
	})

	t.Run("proxy disabled: capture materializes but is effectively off", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: false
  http_port: 6788
  domain: local.test.dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		require.NotNil(t, cfg.Proxy)
		require.NotNil(t, cfg.Proxy.Capture, "capture still materializes on a disabled proxy block")
		assert.True(t, cfg.Proxy.Capture.Enabled, "the capture config's own field still defaults on")
		assert.False(t, cfg.Proxy.CaptureEffectivelyEnabled(), "but effective capture is gated off by the disabled proxy")
	})

	t.Run("no proxy block at all: no capture materialized", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		assert.Nil(t, cfg.Proxy)
	})

	t.Run("HTTP only does not auto-create certs", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  http_port: 6788
  domain: local.test.dev

services:
  app: 3000
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)

		// No certs should be auto-created for HTTP only
		assert.Nil(t, cfg.Certs)
	})

	t.Run("loads HTTP only config from file", func(t *testing.T) {
		cfg, err := Load(filepath.Join("..", "..", "testdata", "configs", "http_only.yaml"))
		require.NoError(t, err)

		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 6788, cfg.Proxy.HTTPPort)
		assert.Equal(t, 0, cfg.Proxy.HTTPSPort)
		assert.Equal(t, "local.test.dev", cfg.Proxy.Domain)
		assert.Nil(t, cfg.Certs) // No certs for HTTP only
	})

	t.Run("loads dual stack config from file", func(t *testing.T) {
		cfg, err := Load(filepath.Join("..", "..", "testdata", "configs", "dual_stack.yaml"))
		require.NoError(t, err)

		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 6788, cfg.Proxy.HTTPPort)
		assert.Equal(t, 6789, cfg.Proxy.HTTPSPort)
		assert.Equal(t, "local.test.dev", cfg.Proxy.Domain)
		require.NotNil(t, cfg.Certs) // Certs auto-created for HTTPS
		assert.True(t, cfg.Certs.AutoGenerate)
	})
}

// --- Strict processes: / services: parsing (plan 016 W1) -------------------

// TestParse_NullHealthcheckMeansAbsent pins that a bare `healthcheck:` key
// (explicit YAML null, e.g. every subfield commented out) still parses as "no
// healthcheck" rather than tripping the strict must-be-a-mapping error.
func TestParse_NullHealthcheckMeansAbsent(t *testing.T) {
	cfg, err := Parse([]byte("processes:\n  web:\n    cmd: ./web\n    healthcheck:\n"))
	require.NoError(t, err)
	assert.Nil(t, cfg.Processes["web"].Healthcheck)
}

func TestParse_Processes_Errors(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantSub   string
		wantExact string
	}{
		{
			name: "typo'd process field",
			yaml: `
processes:
  web:
    cmd: ./web
    stop_timout: 5s
`,
			wantSub: `processes.web: unknown field "stop_timout"`,
		},
		{
			name: "unknown healthcheck subfield",
			yaml: `
processes:
  web:
    cmd: ./web
    healthcheck:
      cmd: ./check
      intervall: 10s
`,
			wantSub: `processes.web.healthcheck: unknown field "intervall"`,
		},
		{
			name: "healthcheck is not a mapping",
			yaml: `
processes:
  web:
    cmd: ./web
    healthcheck: ./check
`,
			// Exact match: the shape defect must yield exactly this one error,
			// not a second opaque re-marshal error for the same value.
			wantExact: `invalid configuration: processes.web.healthcheck: must be a mapping`,
		},
		{
			name: "process entry is a list",
			yaml: `
processes:
  web:
    - ./web
`,
			wantSub: "processes.web: must be a command string or a mapping",
		},
		{
			name: "process entry has a non-string key",
			yaml: `
processes:
  web:
    cmd: ./web
    3: oops
`,
			wantSub: "processes.web: must be a command string or a mapping",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			require.Error(t, err)
			if tc.wantExact != "" {
				assert.Equal(t, tc.wantExact, err.Error())
			} else {
				assert.Contains(t, err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParse_Services_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "typo'd service field",
			yaml: `
processes: {web: ./web}
proxy: {enabled: true, domain: local.test.dev}
services:
  app:
    prot: 3000
`,
			wantSub: `services.app: unknown field "prot"`,
		},
		{
			name: "service entry is a list",
			yaml: `
processes: {web: ./web}
proxy: {enabled: true, domain: local.test.dev}
services:
  app:
    - 3000
`,
			wantSub: "services.app: must be a port number or a mapping",
		},
		{
			name: "service entry is a string",
			yaml: `
processes: {web: ./web}
proxy: {enabled: true, domain: local.test.dev}
services:
  app: "3000"
`,
			wantSub: "services.app: must be a port number or a mapping",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestParse_StructuralErrors_BatchedDeterministically pins that unknown keys
// from every strict block (processes, services, dependencies, tasks) are
// reported TOGETHER, once each, in sorted order under ErrInvalidConfig -- not
// one fatal first error per block.
func TestParse_StructuralErrors_BatchedDeterministically(t *testing.T) {
	yaml := `
processes:
  web:
    cmd: ./web
    stop_timout: 5s
proxy: {enabled: true, domain: local.test.dev}
services:
  app:
    port: 3000
    prot: 8080
dependencies:
  redis:
    check:
      tcp: localhost:6379
    on_failur: warn
tasks:
  build:
    cmd: ./build
    timeut: 5s
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidConfig)
	assert.Equal(t,
		`invalid configuration: dependencies.redis: unknown field "on_failur"; `+
			`processes.web: unknown field "stop_timout"; `+
			`services.app: unknown field "prot"; tasks.build: unknown field "timeut"`,
		err.Error())

	_, err2 := Parse([]byte(yaml))
	require.Error(t, err2)
	assert.Equal(t, err.Error(), err2.Error(), "error string must not depend on map iteration order")
}

// TestParse_ServiceShorthand_Float64Port covers the float64 branch of
// parseServiceConfig for real: `3000.0` decodes to float64, unlike the plain
// `3000` case (which yaml.v3 decodes as int) exercised in TestParse_ProxyConfig.
func TestParse_ServiceShorthand_Float64Port(t *testing.T) {
	yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.test.dev

services:
  app: 3000.0
`
	cfg, err := Parse([]byte(yaml))
	require.NoError(t, err)
	assert.Equal(t, 3000, cfg.Services["app"].Port)
	assert.Equal(t, "localhost", cfg.Services["app"].Host)
}

func TestParseSize(t *testing.T) {
	t.Run("empty string returns zero", func(t *testing.T) {
		size, err := ParseSize("")
		require.NoError(t, err)
		assert.Equal(t, int64(0), size)
	})

	t.Run("plain bytes", func(t *testing.T) {
		size, err := ParseSize("1024")
		require.NoError(t, err)
		assert.Equal(t, int64(1024), size)
	})

	t.Run("B suffix", func(t *testing.T) {
		size, err := ParseSize("512B")
		require.NoError(t, err)
		assert.Equal(t, int64(512), size)
	})

	t.Run("KB suffix", func(t *testing.T) {
		size, err := ParseSize("512KB")
		require.NoError(t, err)
		assert.Equal(t, int64(512*1024), size)
	})

	t.Run("K suffix", func(t *testing.T) {
		size, err := ParseSize("64K")
		require.NoError(t, err)
		assert.Equal(t, int64(64*1024), size)
	})

	t.Run("MB suffix", func(t *testing.T) {
		size, err := ParseSize("1MB")
		require.NoError(t, err)
		assert.Equal(t, int64(1024*1024), size)
	})

	t.Run("M suffix", func(t *testing.T) {
		size, err := ParseSize("2M")
		require.NoError(t, err)
		assert.Equal(t, int64(2*1024*1024), size)
	})

	t.Run("GB suffix", func(t *testing.T) {
		size, err := ParseSize("1GB")
		require.NoError(t, err)
		assert.Equal(t, int64(1024*1024*1024), size)
	})

	t.Run("case insensitive", func(t *testing.T) {
		size, err := ParseSize("1mb")
		require.NoError(t, err)
		assert.Equal(t, int64(1024*1024), size)
	})

	t.Run("whitespace is trimmed", func(t *testing.T) {
		size, err := ParseSize("  512KB  ")
		require.NoError(t, err)
		assert.Equal(t, int64(512*1024), size)
	})

	t.Run("invalid suffix returns error", func(t *testing.T) {
		_, err := ParseSize("100XB")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid size suffix")
	})

	t.Run("non-numeric string returns error", func(t *testing.T) {
		_, err := ParseSize("notanumber")
		require.Error(t, err)
	})

	t.Run("zero is valid", func(t *testing.T) {
		size, err := ParseSize("0")
		require.NoError(t, err)
		assert.Equal(t, int64(0), size)
	})
}
