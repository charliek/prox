package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAPIAddrFromConfig(t *testing.T) {
	// Save original configPath and restore after test
	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	t.Run("returns address from config with custom port", func(t *testing.T) {
		// Create temp config file
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
api:
  port: 5552
  host: 127.0.0.1
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "http://127.0.0.1:5552" {
			t.Errorf("expected http://127.0.0.1:5552, got %s", addr)
		}
	})

	t.Run("returns empty when port not specified (dynamic)", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "" {
			t.Errorf("expected empty string for dynamic port, got %s", addr)
		}
	})

	t.Run("returns empty string when config not found", func(t *testing.T) {
		configPath = "/nonexistent/prox.yaml"
		addr := loadAPIAddrFromConfig()

		if addr != "" {
			t.Errorf("expected empty string, got %s", addr)
		}
	})

	t.Run("uses custom host from config", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
api:
  port: 8080
  host: 0.0.0.0
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "http://0.0.0.0:8080" {
			t.Errorf("expected http://0.0.0.0:8080, got %s", addr)
		}
	})
}

func TestGetProcessNames(t *testing.T) {
	// Save original configPath and restore after test
	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	t.Run("returns process names from config", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
processes:
  web: npm run dev
  api: go run ./cmd/api
  worker: python worker.py
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		names := getProcessNames()

		if len(names) != 3 {
			t.Errorf("expected 3 process names, got %d", len(names))
		}

		// Check that all expected names are present
		nameSet := make(map[string]bool)
		for _, name := range names {
			nameSet[name] = true
		}

		expected := []string{"web", "api", "worker"}
		for _, exp := range expected {
			if !nameSet[exp] {
				t.Errorf("expected process name %q not found", exp)
			}
		}
	})

	t.Run("returns nil when config not found", func(t *testing.T) {
		configPath = "/nonexistent/prox.yaml"
		names := getProcessNames()

		if names != nil {
			t.Errorf("expected nil, got %v", names)
		}
	})
}

// TestClientCommandsDiscoveryAllowlist pins the discovery allowlist contract:
// every command whose handler reaches the daemon API through the shared apiAddr
// global (the NewClient(apiAddr) call sites in this package, plus attach, which
// falls back to apiAddr when --addr is set) must be in clientCommands, or it
// silently talks to the :5555 default and breaks against dynamic-port daemons.
// 'start' (#41) and 'requests' (#43) both shipped with exactly that gap; when
// adding a new client command, add it to clientCommands AND to this list.
func TestClientCommandsDiscoveryAllowlist(t *testing.T) {
	// The enumerated apiAddr-consuming commands. downCmd reuses runStop, and
	// attachCmd honors apiAddr when explicitly set, so both belong here.
	apiAddrCommands := []string{
		"status",   // runStatus
		"logs",     // runLogs
		"stop",     // runStop
		"down",     // RunE: runStop
		"start",    // runStartProcess
		"restart",  // runRestart
		"attach",   // runAttach (apiAddr fallback + state discovery)
		"requests", // runRequests
	}

	for _, name := range apiAddrCommands {
		if !clientCommands[name] {
			t.Errorf("command %q performs client calls via apiAddr but is missing from the clientCommands discovery allowlist", name)
		}
	}

	// Every allowlist entry must be a real registered subcommand, so a renamed
	// or removed command can't leave a stale entry silently matching nothing.
	registered := make(map[string]bool)
	for _, cmd := range rootCmd.Commands() {
		registered[cmd.Name()] = true
	}
	for name := range clientCommands {
		if !registered[name] {
			t.Errorf("clientCommands allowlist entry %q is not a registered command", name)
		}
	}

	if len(clientCommands) != len(apiAddrCommands) {
		t.Errorf("clientCommands has %d entries but %d apiAddr-consuming commands are enumerated here; keep the allowlist and this test in sync",
			len(clientCommands), len(apiAddrCommands))
	}
}
