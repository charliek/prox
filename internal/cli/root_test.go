package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAPIAddrFromConfig(t *testing.T) {
	t.Run("returns address from config with custom port", func(t *testing.T) {
		// Create temp config file
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(configPath, []byte(`
api:
  port: 5552
  host: 127.0.0.1
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		app := &App{configPath: configPath}
		addr := app.loadAPIAddrFromConfig()

		if addr != "http://127.0.0.1:5552" {
			t.Errorf("expected http://127.0.0.1:5552, got %s", addr)
		}
	})

	t.Run("returns address with default port when not specified", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(configPath, []byte(`
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		app := &App{configPath: configPath}
		addr := app.loadAPIAddrFromConfig()

		if addr != "http://127.0.0.1:5555" {
			t.Errorf("expected http://127.0.0.1:5555, got %s", addr)
		}
	})

	t.Run("returns empty string when config not found", func(t *testing.T) {
		app := &App{configPath: "/nonexistent/prox.yaml"}
		addr := app.loadAPIAddrFromConfig()

		if addr != "" {
			t.Errorf("expected empty string, got %s", addr)
		}
	})

	t.Run("uses custom host from config", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(configPath, []byte(`
api:
  port: 8080
  host: 0.0.0.0
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		app := &App{configPath: configPath}
		addr := app.loadAPIAddrFromConfig()

		if addr != "http://0.0.0.0:8080" {
			t.Errorf("expected http://0.0.0.0:8080, got %s", addr)
		}
	})
}

func TestParseGlobalFlags_AddrExplicitlySet(t *testing.T) {
	t.Run("sets apiAddrExplicitlySet when --addr provided", func(t *testing.T) {
		app := NewApp()
		app.parseGlobalFlags([]string{"--addr", "http://localhost:9999", "status"})

		if !app.apiAddrExplicitlySet {
			t.Error("expected apiAddrExplicitlySet to be true")
		}
		if app.apiAddr != "http://localhost:9999" {
			t.Errorf("expected apiAddr to be http://localhost:9999, got %s", app.apiAddr)
		}
	})

	t.Run("apiAddrExplicitlySet false when --addr not provided", func(t *testing.T) {
		app := NewApp()
		app.parseGlobalFlags([]string{"status"})

		if app.apiAddrExplicitlySet {
			t.Error("expected apiAddrExplicitlySet to be false")
		}
	})
}
