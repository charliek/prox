package proxyd

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startTestServer(t *testing.T) (*Server, *Client, string) {
	t.Helper()

	// Use a short temp dir to stay under Unix socket 104-byte path limit on macOS
	tmpDir, err := os.MkdirTemp("/tmp", "prox-ut-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "t.sock")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	server := NewServer(ServerConfig{
		SocketPath: socketPath,
		Logger:     logger,
		Version:    "test-version",
	})

	registry := NewRegistry()
	server.SetRegistry(registry)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	// Wait for socket to be available
	deadline := time.Now().Add(2 * time.Second)
	client := NewClient(socketPath)
	for time.Now().Before(deadline) {
		if _, err := client.Health(); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Cleanup(func() {
		server.Shutdown(t.Context())
	})

	return server, client, socketPath
}

func TestHealthEndpoint(t *testing.T) {
	_, client, _ := startTestServer(t)

	ver, err := client.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if ver != "test-version" {
		t.Errorf("version = %q, want %q", ver, "test-version")
	}
}

func TestStatusEndpoint(t *testing.T) {
	_, client, _ := startTestServer(t)

	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Version != "test-version" {
		t.Errorf("version = %q, want %q", status.Version, "test-version")
	}
	if status.PID == 0 {
		t.Error("PID should not be 0")
	}
	if status.RouteCount != 0 {
		t.Errorf("route count = %d, want 0", status.RouteCount)
	}
}

func TestRegisterAndRoutes(t *testing.T) {
	_, client, _ := startTestServer(t)

	resp, err := client.Register(RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
		},
		HTTPSPort: 443,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(resp.Registered) != 1 || resp.Registered[0] != "api.local.dev" {
		t.Errorf("registered = %v, want [api.local.dev]", resp.Registered)
	}

	// Check routes
	routes, err := client.Routes()
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	if routes[0].Hostname != "api.local.dev" {
		t.Errorf("hostname = %q, want %q", routes[0].Hostname, "api.local.dev")
	}

	// Check status
	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ProjectCount != 1 {
		t.Errorf("project count = %d, want 1", status.ProjectCount)
	}
	if status.RouteCount != 1 {
		t.Errorf("route count = %d, want 1", status.RouteCount)
	}
}

func TestRegisterVersionMismatch(t *testing.T) {
	_, client, _ := startTestServer(t)

	_, err := client.Register(RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "wrong-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
		},
		HTTPSPort: 443,
	})
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestDeregister(t *testing.T) {
	_, client, _ := startTestServer(t)

	// Register first
	_, err := client.Register(RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
		},
		HTTPSPort: 443,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Deregister
	err = client.Deregister(DeregisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	// Verify empty
	routes, err := client.Routes()
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(routes) != 0 {
		t.Errorf("got %d routes after deregister, want 0", len(routes))
	}
}

func TestShutdownProtected(t *testing.T) {
	_, client, _ := startTestServer(t)

	// Register a project
	_, err := client.Register(RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
		},
		HTTPSPort: 443,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Shutdown without force should fail
	err = client.Shutdown(false)
	if err == nil {
		t.Fatal("expected error for shutdown with active routes, got nil")
	}
	t.Logf("got expected error: %v", err)

	// Shutdown with force should succeed
	err = client.Shutdown(true)
	if err != nil {
		t.Fatalf("Shutdown(force): %v", err)
	}
}

func TestDomainConflictViaClient(t *testing.T) {
	_, client, _ := startTestServer(t)

	// Register project A
	_, err := client.Register(RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
		},
		HTTPSPort: 443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Register project B with same domain — should conflict
	_, err = client.Register(RegisterRequest{
		ProjectDir: "/test/project-b",
		PID:        os.Getpid(),
		Version:    "test-version",
		Domain:     "local.dev",
		Services: map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 4000},
		},
		HTTPSPort: 443,
	})
	if err == nil {
		t.Fatal("expected domain conflict error, got nil")
	}
	t.Logf("got expected error: %v", err)
}
