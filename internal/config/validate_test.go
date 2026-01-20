package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		cfg := &Config{
			API: APIConfig{Port: 5555, Host: "127.0.0.1"},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("invalid port fails", func(t *testing.T) {
		cfg := &Config{
			API: APIConfig{Port: 99999},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("negative port fails", func(t *testing.T) {
		cfg := &Config{
			API: APIConfig{Port: -1},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("empty processes fails", func(t *testing.T) {
		cfg := &Config{
			API:       APIConfig{Port: 5555},
			Processes: map[string]ProcessConfig{},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one process")
	})

	t.Run("missing cmd fails", func(t *testing.T) {
		cfg := &Config{
			API: APIConfig{Port: 5555},
			Processes: map[string]ProcessConfig{
				"web": {Env: map[string]string{"PORT": "3000"}},
			},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cmd")
	})

	t.Run("healthcheck without cmd fails", func(t *testing.T) {
		cfg := &Config{
			API: APIConfig{Port: 5555},
			Processes: map[string]ProcessConfig{
				"web": {
					Cmd:         "npm run dev",
					Healthcheck: &HealthcheckConfig{Interval: "10s"},
				},
			},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "healthcheck.cmd")
	})
}

func TestValidateProcessName(t *testing.T) {
	t.Run("valid names", func(t *testing.T) {
		validNames := []string{"web", "api", "worker-1", "my_service"}
		for _, name := range validNames {
			err := ValidateProcessName(name)
			assert.NoError(t, err, "name %q should be valid", name)
		}
	})

	t.Run("empty name fails", func(t *testing.T) {
		err := ValidateProcessName("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("name with space fails", func(t *testing.T) {
		err := ValidateProcessName("my service")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitespace")
	})

	t.Run("name with slash fails", func(t *testing.T) {
		err := ValidateProcessName("my/service")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "path")
	})
}
