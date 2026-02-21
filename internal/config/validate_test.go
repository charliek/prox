package config

import (
	"strings"
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

func TestValidateProxy(t *testing.T) {
	baseConfig := func() *Config {
		return &Config{
			API: APIConfig{Port: 5555, Host: "127.0.0.1"},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
			Certs: &CertsConfig{
				Dir:          "/tmp/certs",
				AutoGenerate: true,
			},
		}
	}

	t.Run("valid proxy config passes", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("proxy enabled without domain fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.domain")
	})

	t.Run("proxy disabled without domain passes", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   false,
			HTTPSPort: 6789,
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("invalid proxy port fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 99999,
			Domain:    "local.myapp.dev",
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.https_port")
	})

	t.Run("proxy port 0 fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 0,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one of http_port or https_port")
	})

	t.Run("proxy port -1 fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: -1,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.https_port: must be between 0 and 65535")
	})

	t.Run("proxy port 65536 fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 65536,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.https_port: must be between 0 and 65535")
	})

	t.Run("HTTP only proxy is valid", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:  true,
			HTTPPort: 6788,
			Domain:   "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("dual stack proxy is valid", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPPort:  6788,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("invalid HTTP port fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:  true,
			HTTPPort: 70000,
			Domain:   "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.http_port")
	})

	t.Run("negative HTTP port fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:  true,
			HTTPPort: -1,
			Domain:   "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy.http_port")
	})

	t.Run("HTTP only requires no certs", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:  true,
			HTTPPort: 6788,
			Domain:   "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		// Explicitly remove certs - should be valid for HTTP only
		cfg.Certs = nil
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("HTTPS requires certs", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		// No certs config - should fail for HTTPS
		cfg.Certs = nil
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certs: certificate configuration required")
	})

	t.Run("services without proxy fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "proxy must be enabled")
	})

	t.Run("invalid service port fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled: true,
			Domain:  "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 0, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "services.app.port")
	})

	t.Run("service port 65536 fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled: true,
			Domain:  "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 65536, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "services.app.port")
	})

	t.Run("service port 65535 passes", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 65535, Host: "localhost"},
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})
}

func TestValidateServiceName(t *testing.T) {
	t.Run("valid service names", func(t *testing.T) {
		validNames := []string{"app", "api", "my-service", "web123", "a1b2c3"}
		for _, name := range validNames {
			err := validateServiceName(name)
			assert.NoError(t, err, "name %q should be valid", name)
		}
	})

	t.Run("empty name fails", func(t *testing.T) {
		err := validateServiceName("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("name with uppercase fails", func(t *testing.T) {
		err := validateServiceName("MyService")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "lowercase")
	})

	t.Run("name starting with hyphen fails", func(t *testing.T) {
		err := validateServiceName("-app")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hyphen")
	})

	t.Run("name ending with hyphen fails", func(t *testing.T) {
		err := validateServiceName("app-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hyphen")
	})

	t.Run("name too long fails", func(t *testing.T) {
		longName := strings.Repeat("a", 64)
		err := validateServiceName(longName)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too long")
	})

	t.Run("name with underscore fails", func(t *testing.T) {
		err := validateServiceName("my_service")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "lowercase letters, numbers, and hyphens")
	})
}

func TestValidateHost(t *testing.T) {
	t.Run("valid hosts", func(t *testing.T) {
		validHosts := []string{"localhost", "127.0.0.1", "192.168.1.1", "example.com", "my-server.local"}
		for _, host := range validHosts {
			err := validateHost(host)
			assert.NoError(t, err, "host %q should be valid", host)
		}
	})

	t.Run("empty host fails", func(t *testing.T) {
		err := validateHost("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("invalid host format fails", func(t *testing.T) {
		invalidHosts := []string{"host:8080", "http://localhost", "my_server", "host name"}
		for _, host := range invalidHosts {
			err := validateHost(host)
			require.Error(t, err, "host %q should be invalid", host)
			assert.Contains(t, err.Error(), "invalid host format")
		}
	})
}

func TestValidateDomain(t *testing.T) {
	baseConfig := func() *Config {
		return &Config{
			API: APIConfig{Port: 5555, Host: "127.0.0.1"},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
			Certs: &CertsConfig{
				Dir:          "/tmp/certs",
				AutoGenerate: true,
			},
		}
	}

	t.Run("valid domain formats pass", func(t *testing.T) {
		validDomains := []string{"local.dev", "my-app.local.dev", "example.com", "sub.domain.co.uk"}
		for _, domain := range validDomains {
			cfg := baseConfig()
			cfg.Proxy = &ProxyConfig{
				Enabled:   true,
				HTTPSPort: 6789,
				Domain:    domain,
			}
			cfg.Services = map[string]ServiceConfig{
				"app": {Port: 3000, Host: "localhost"},
			}
			err := Validate(cfg)
			assert.NoError(t, err, "domain %q should be valid", domain)
		}
	})

	t.Run("invalid domain format fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "invalid domain with spaces",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid domain format")
	})

	t.Run("domain starting with hyphen fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "-invalid.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid domain format")
	})
}

func TestValidateCertsConfig(t *testing.T) {
	baseConfig := func() *Config {
		return &Config{
			API: APIConfig{Port: 5555, Host: "127.0.0.1"},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
			Certs: &CertsConfig{
				Dir:          "/tmp/certs",
				AutoGenerate: true,
			},
		}
	}

	t.Run("valid certs config passes", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Certs = &CertsConfig{
			Dir:          "/path/to/certs",
			AutoGenerate: true,
		}
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("empty certs dir fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Certs = &CertsConfig{
			Dir:          "",
			AutoGenerate: true,
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certs.dir")
	})
}

func TestValidateServiceHost(t *testing.T) {
	baseConfig := func() *Config {
		return &Config{
			API: APIConfig{Port: 5555, Host: "127.0.0.1"},
			Processes: map[string]ProcessConfig{
				"web": {Cmd: "npm run dev"},
			},
			Certs: &CertsConfig{
				Dir:          "/tmp/certs",
				AutoGenerate: true,
			},
		}
	}

	t.Run("empty service host fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: ""},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "services.app.host")
	})

	t.Run("invalid service host format fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Proxy = &ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.dev",
		}
		cfg.Services = map[string]ServiceConfig{
			"app": {Port: 3000, Host: "http://localhost"},
		}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "services.app.host")
	})
}
