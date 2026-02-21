package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_SimpleForm(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "testdata", "configs", "simple.yaml"))
	require.NoError(t, err)

	assert.Equal(t, 5555, cfg.API.Port)
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

func TestConfig_ToDomainProcesses(t *testing.T) {
	cfg := &Config{
		Processes: map[string]ProcessConfig{
			"web": {Cmd: "npm run dev"},
			"api": {
				Cmd: "go run ./cmd/server",
				Env: map[string]string{"PORT": "8080"},
				Healthcheck: &HealthcheckConfig{
					Cmd:      "curl -f http://localhost:8080/health",
					Interval: "10s",
					Timeout:  "5s",
					Retries:  3,
				},
			},
		},
	}

	procs := cfg.ToDomainProcesses()
	assert.Len(t, procs, 2)

	// Find api process
	var apiProc *struct {
		found bool
		cmd   string
		env   map[string]string
		hc    bool
	}
	for _, p := range procs {
		if p.Name == "api" {
			apiProc = &struct {
				found bool
				cmd   string
				env   map[string]string
				hc    bool
			}{
				found: true,
				cmd:   p.Cmd,
				env:   p.Env,
				hc:    p.Healthcheck != nil,
			}
			break
		}
	}

	require.NotNil(t, apiProc)
	assert.True(t, apiProc.found)
	assert.Equal(t, "go run ./cmd/server", apiProc.cmd)
	assert.Equal(t, "8080", apiProc.env["PORT"])
	assert.True(t, apiProc.hc)
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
