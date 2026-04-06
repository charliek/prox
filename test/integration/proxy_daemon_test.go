package integration

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
)

// startDaemonServer starts a daemon server on a Unix socket in a temp dir.
func startDaemonServer(t *testing.T) (*proxyd.Client, func()) {
	t.Helper()

	// Use a short temp dir path to stay under Unix socket 104-byte limit on macOS
	tmpDir, err := os.MkdirTemp("/tmp", "prox-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "d.sock")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	registry := proxyd.NewRegistry()
	requestMgr := proxy.NewRequestManager(100)
	server := proxyd.NewServer(proxyd.ServerConfig{
		SocketPath: socketPath,
		Logger:     logger,
		Version:    "test",
	})
	server.SetRegistry(registry)
	server.SetRequestManager(requestMgr)

	errCh := make(chan error, 1)
	go func() { errCh <- server.Start() }()

	// Wait for server to be ready
	client := proxyd.NewClient(socketPath)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Health(); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cleanup := func() {
		server.Shutdown(t.Context())
	}

	return client, cleanup
}

func TestProxyDaemon_SingleProject(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register
	resp, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services: map[string]proxyd.ServiceTarget{
			"app": {Host: "localhost", Port: 13000},
			"api": {Host: "localhost", Port: 13001},
		},
		HTTPSPort: 16443,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if len(resp.Registered) != 2 {
		t.Fatalf("registered %d hostnames, want 2", len(resp.Registered))
	}

	// Verify status
	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ProjectCount != 1 {
		t.Errorf("ProjectCount = %d, want 1", status.ProjectCount)
	}
	if status.RouteCount != 2 {
		t.Errorf("RouteCount = %d, want 2", status.RouteCount)
	}

	// Deregister
	err = client.Deregister(proxyd.DeregisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	// Verify empty
	status, err = client.Status()
	if err != nil {
		t.Fatalf("Status after deregister: %v", err)
	}
	if status.RouteCount != 0 {
		t.Errorf("RouteCount after deregister = %d, want 0", status.RouteCount)
	}
}

func TestProxyDaemon_TwoProjectsSamePort(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register Project A
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"app": {Host: "localhost", Port: 13000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Register Project B — same port, different domain
	_, err = client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-b",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.beta.dev",
		Services:   map[string]proxyd.ServiceTarget{"frontend": {Host: "localhost", Port: 14000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	// Verify both registered
	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ProjectCount != 2 {
		t.Errorf("ProjectCount = %d, want 2", status.ProjectCount)
	}
	if status.RouteCount != 2 {
		t.Errorf("RouteCount = %d, want 2", status.RouteCount)
	}

	// Verify routes
	routes, err := client.Routes()
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}

	hostnames := make(map[string]bool)
	for _, r := range routes {
		hostnames[r.Hostname] = true
	}
	if !hostnames["app.local.alpha.dev"] {
		t.Error("missing route for app.local.alpha.dev")
	}
	if !hostnames["frontend.local.beta.dev"] {
		t.Error("missing route for frontend.local.beta.dev")
	}
}

func TestProxyDaemon_DifferentPorts(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Project A: HTTPS on 16443
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"api": {Host: "localhost", Port: 13000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Project C: HTTP on 18080
	_, err = client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-c",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.gamma.dev",
		Services:   map[string]proxyd.ServiceTarget{"app": {Host: "localhost", Port: 15000}},
		HTTPPort:   18080,
	})
	if err != nil {
		t.Fatalf("Register C: %v", err)
	}

	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.ListenerPorts) != 2 {
		t.Errorf("ListenerPorts = %v, want 2 ports", status.ListenerPorts)
	}
}

func TestProxyDaemon_DomainConflict(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register Project A
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"app": {Host: "localhost", Port: 13000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Try conflicting registration — same domain+port
	_, err = client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-conflict",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"app": {Host: "localhost", Port: 16000}},
		HTTPSPort:  16443,
	})
	if err == nil {
		t.Fatal("expected domain conflict error, got nil")
	}
	t.Logf("got expected conflict error: %v", err)
}

func TestProxyDaemon_ProtocolMismatch(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register HTTPS on port 16443
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"api": {Host: "localhost", Port: 13000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Try HTTP on same port 16443
	_, err = client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-mismatch",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.mismatch.dev",
		Services:   map[string]proxyd.ServiceTarget{"app": {Host: "localhost", Port: 17000}},
		HTTPPort:   16443,
	})
	if err == nil {
		t.Fatal("expected protocol mismatch error, got nil")
	}
	t.Logf("got expected protocol mismatch error: %v", err)
}

func TestProxyDaemon_LastDeregisterStopsDaemon(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register two projects
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.alpha.dev",
		Services:   map[string]proxyd.ServiceTarget{"api": {Host: "localhost", Port: 13000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	_, err = client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-b",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.beta.dev",
		Services:   map[string]proxyd.ServiceTarget{"web": {Host: "localhost", Port: 14000}},
		HTTPSPort:  16443,
	})
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	// Deregister A — daemon should still be alive
	err = client.Deregister(proxyd.DeregisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Deregister A: %v", err)
	}

	status, err := client.Status()
	if err != nil {
		t.Fatalf("Status after deregister A: %v", err)
	}
	if status.ProjectCount != 1 {
		t.Errorf("ProjectCount = %d, want 1", status.ProjectCount)
	}

	// Deregister B — daemon route count should be 0
	err = client.Deregister(proxyd.DeregisterRequest{
		ProjectDir: "/test/project-b",
		PID:        os.Getpid(),
	})
	if err != nil {
		t.Fatalf("Deregister B: %v", err)
	}

	status, err = client.Status()
	if err != nil {
		t.Fatalf("Status after deregister B: %v", err)
	}
	if status.RouteCount != 0 {
		t.Errorf("RouteCount = %d, want 0", status.RouteCount)
	}
}

func TestProxyDaemon_VersionMismatch(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register with wrong version
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "wrong-version",
		Domain:     "local.dev",
		Services:   map[string]proxyd.ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		HTTPSPort:  443,
	})
	if err == nil {
		t.Fatal("expected version mismatch error, got nil")
	}
	t.Logf("got expected version error: %v", err)
}

func TestProxyDaemon_ProxyStopProtected(t *testing.T) {
	skipShort(t)
	client, cleanup := startDaemonServer(t)
	defer cleanup()

	// Register a project
	_, err := client.Register(proxyd.RegisterRequest{
		ProjectDir: "/test/project-a",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.dev",
		Services:   map[string]proxyd.ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		HTTPSPort:  443,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Shutdown without force should fail
	err = client.Shutdown(false)
	if err == nil {
		t.Fatal("expected error for shutdown with active routes, got nil")
	}

	// Shutdown with force should succeed
	err = client.Shutdown(true)
	if err != nil {
		t.Fatalf("Shutdown(force): %v", err)
	}
}
