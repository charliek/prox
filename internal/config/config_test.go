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
