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

	t.Run("returns address with default port when not specified", func(t *testing.T) {
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

		if addr != "http://127.0.0.1:5555" {
			t.Errorf("expected http://127.0.0.1:5555, got %s", addr)
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
