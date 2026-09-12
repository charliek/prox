package integration

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxyd"
)

// daemonServerOption configures startDaemonServer. Existing call sites pass
// none and get exactly the daemon they always got.
type daemonServerOption func(*daemonServerConfig)

type daemonServerConfig struct {
	hub     *proxyd.HubConfig
	hubAddr *string
}

// withHubMode starts the daemon with hub mode on (plan 031 C3). The hub binds a
// loopback ephemeral CONTROL-PLANE port and the address actually bound is
// written to boundAddr via Server.HubListenAddr -- the data-plane ports in cfg
// stay real numbers, because there 0 already means "no listener" (P15).
func withHubMode(cfg proxyd.HubConfig, boundAddr *string) daemonServerOption {
	return func(c *daemonServerConfig) {
		c.hub = &cfg
		c.hubAddr = boundAddr
	}
}

// startDaemonServer starts a daemon server on a Unix socket in a temp dir.
func startDaemonServer(t *testing.T, opts ...daemonServerOption) (*proxyd.Client, func()) {
	t.Helper()

	var dcfg daemonServerConfig
	for _, opt := range opts {
		opt(&dcfg)
	}

	// Use a short temp dir path to stay under Unix socket 104-byte limit on macOS
	tmpDir, err := os.MkdirTemp("/tmp", "prox-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "d.sock")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	registry := proxyd.NewRegistry()
	server := proxyd.NewServer(proxyd.ServerConfig{
		SocketPath: socketPath,
		Logger:     logger,
		Version:    "test",
	})
	server.SetRegistry(registry)
	server.SetManagers(proxyd.NewManagers(constants.DefaultProxyRequestBufferSize, nil))

	if dcfg.hub != nil {
		if err := server.StartHub(*dcfg.hub); err != nil {
			t.Fatalf("StartHub: %v", err)
		}
		if dcfg.hubAddr != nil {
			*dcfg.hubAddr = server.HubListenAddr()
		}
	}

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
		server.StopHub()
		server.Shutdown(t.Context())
	}

	return client, cleanup
}

func TestProxyDaemon_SingleProject(t *testing.T) {
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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
	startTest(t, defaultTestBudget)
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

// hubTestConfig is the in-process hub the tests below publish through: a
// loopback control plane on an ephemeral port, a token, and a REAL data-plane
// port number (this daemon starts no dynamic proxy, so nothing binds it).
func hubTestConfig() proxyd.HubConfig {
	return proxyd.HubConfig{
		Domain:    "llt.test",
		Listen:    "127.0.0.1:0",
		HTTPSPort: 16443,
		Auth:      proxyd.HubAuthToken,
		Token:     "integration-hub-token",
	}
}

// TestProxyDaemon_HubRegistration publishes a project through the hub's NETWORK
// control plane with the real proxyd.Client (NewHTTPClient), the way a remote
// `prox up` will, and checks what the hub host then reports: the hub's own
// domain (not the publisher's), the composed "<origin>:<dir>" project key, and
// the publisher's origin on every route (plan 031 D4/D5).
func TestProxyDaemon_HubRegistration(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	var hubAddr string
	socketClient, cleanup := startDaemonServer(t, withHubMode(hubTestConfig(), &hubAddr))
	defer cleanup()

	if hubAddr == "" {
		t.Fatal("hub mode did not report a bound listen address")
	}

	hubClient, err := proxyd.NewHTTPClient("http://"+hubAddr, "integration-hub-token")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	if _, err := hubClient.Health(); err != nil {
		t.Fatalf("hub health: %v", err)
	}

	resp, err := hubClient.Register(proxyd.RegisterRequest{
		ProjectDir:      "/home/dev/remote-app",
		PID:             os.Getpid(),
		Origin:          "popos",
		ProtocolVersion: constants.HubProtocolVersion,
		// The publisher's own domain and port are ignored: the hub owns them.
		Domain:    "local.popos.dev",
		HTTPSPort: 9999,
		Services:  map[string]proxyd.ServiceTarget{"auth": {Host: "localhost", Port: 3000}},
	})
	if err != nil {
		t.Fatalf("hub Register: %v", err)
	}
	if len(resp.Registered) != 1 || resp.Registered[0] != "auth.llt.test" {
		t.Fatalf("Registered = %v, want [auth.llt.test]", resp.Registered)
	}

	routes, err := socketClient.Routes()
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	r := routes[0]
	if r.Hostname != "auth.llt.test" {
		t.Errorf("Hostname = %q, want auth.llt.test (the HUB's domain)", r.Hostname)
	}
	if r.Port != 16443 {
		t.Errorf("Port = %d, want 16443 (the HUB's port)", r.Port)
	}
	if r.Origin != "popos" {
		t.Errorf("Origin = %q, want popos", r.Origin)
	}
	if r.ProjectDir != "popos:/home/dev/remote-app" {
		t.Errorf("ProjectDir = %q, want the composed key popos:/home/dev/remote-app", r.ProjectDir)
	}
	if r.Target != (proxyd.ServiceTarget{Host: "localhost", Port: 3000}) {
		t.Errorf("Target = %+v, want localhost:3000", r.Target)
	}

	// The hub host's own status reports the publisher.
	status, err := socketClient.HubStatus()
	if err != nil {
		t.Fatalf("HubStatus: %v", err)
	}
	if !status.Enabled || status.Listen != hubAddr {
		t.Fatalf("HubStatus = %+v, want enabled on %s", status, hubAddr)
	}
	if len(status.Publishers) != 1 || status.Publishers[0].Origin != "popos" ||
		status.Publishers[0].ProjectDir != "/home/dev/remote-app" {
		t.Fatalf("Publishers = %+v, want one popos:/home/dev/remote-app entry", status.Publishers)
	}

	// The publisher can remove its own registration over the same mount.
	if err := hubClient.Deregister(proxyd.DeregisterRequest{
		Origin:     "popos",
		ProjectDir: "/home/dev/remote-app",
		PID:        os.Getpid(),
	}); err != nil {
		t.Fatalf("hub Deregister: %v", err)
	}
	routes, err = socketClient.Routes()
	if err != nil {
		t.Fatalf("Routes after deregister: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("got %d routes after deregister, want 0", len(routes))
	}
}

// TestProxyDaemon_HubAndLocalProjectsCoexist is D1's premise: one daemon, one
// route table, one port -- a socket-registered local. project and a
// hub-registered llt. project side by side, each visible as what it is.
func TestProxyDaemon_HubAndLocalProjectsCoexist(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	var hubAddr string
	socketClient, cleanup := startDaemonServer(t, withHubMode(hubTestConfig(), &hubAddr))
	defer cleanup()

	// Local project, over the Unix socket, exactly as today.
	if _, err := socketClient.Register(proxyd.RegisterRequest{
		ProjectDir: "/home/dev/local-app",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     "local.dev",
		Services:   map[string]proxyd.ServiceTarget{"web": {Host: "localhost", Port: 4000}},
		HTTPSPort:  16443,
	}); err != nil {
		t.Fatalf("local Register: %v", err)
	}

	// Remote project, over the hub's network mount, onto the SAME port.
	hubClient, err := proxyd.NewHTTPClient("http://"+hubAddr, "integration-hub-token")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if _, err := hubClient.Register(proxyd.RegisterRequest{
		ProjectDir:      "/home/dev/remote-app",
		PID:             os.Getpid(),
		Origin:          "popos",
		ProtocolVersion: constants.HubProtocolVersion,
		Services:        map[string]proxyd.ServiceTarget{"auth": {Host: "localhost", Port: 3000}},
	}); err != nil {
		t.Fatalf("hub Register: %v", err)
	}

	routes, err := socketClient.Routes()
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}

	byHostname := map[string]proxyd.RouteInfo{}
	for _, r := range routes {
		byHostname[r.Hostname] = r
	}
	local, ok := byHostname["web.local.dev"]
	if !ok {
		t.Fatal("missing the local route web.local.dev")
	}
	if local.Origin != "" || !local.Connected {
		t.Errorf("local route = %+v, want no origin and connected", local)
	}
	if local.ProjectDir != "/home/dev/local-app" {
		t.Errorf("local ProjectDir = %q, want the bare dir", local.ProjectDir)
	}
	remote, ok := byHostname["auth.llt.test"]
	if !ok {
		t.Fatal("missing the hub route auth.llt.test")
	}
	if remote.Origin != "popos" {
		t.Errorf("hub route Origin = %q, want popos", remote.Origin)
	}

	status, err := socketClient.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ProjectCount != 2 || len(status.ListenerPorts) != 1 {
		t.Fatalf("ProjectCount=%d ListenerPorts=%v, want 2 projects on one port",
			status.ProjectCount, status.ListenerPorts)
	}
	if status.Hub == nil || !status.Hub.Enabled {
		t.Fatalf("daemon status must carry the hub object while hub mode is on: %+v", status.Hub)
	}

	// Turning hub mode off drops the remote registration and keeps the local
	// one (plan 031 §4.2).
	if _, err := socketClient.HubStop(); err != nil {
		t.Fatalf("HubStop: %v", err)
	}
	routes, err = socketClient.Routes()
	if err != nil {
		t.Fatalf("Routes after hub stop: %v", err)
	}
	if len(routes) != 1 || routes[0].Hostname != "web.local.dev" {
		t.Fatalf("routes after hub stop = %+v, want only the local route", routes)
	}
}

// TestProxyDaemon_HubRejectsUnauthorizedAndBlockedRoutes is the integration
// echo of the two guarantees C3 exists for (plan 031 D6, AC8): the network
// mount needs the token, and it does not carry shutdown at all.
func TestProxyDaemon_HubRejectsUnauthorizedAndBlockedRoutes(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)

	var hubAddr string
	socketClient, cleanup := startDaemonServer(t, withHubMode(hubTestConfig(), &hubAddr))
	defer cleanup()

	wrongToken, err := proxyd.NewHTTPClient("http://"+hubAddr, "not-the-token")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if _, err := wrongToken.Routes(); err == nil {
		t.Fatal("a wrong token must be rejected")
	} else {
		var apiErr *proxyd.DaemonAPIError
		if !errors.As(err, &apiErr) || apiErr.Code != "UNAUTHORIZED" {
			t.Fatalf("err = %v, want a DaemonAPIError with code UNAUTHORIZED", err)
		}
	}

	// Shutdown over the network mount does not exist -- and the daemon is
	// still alive afterwards.
	rightToken, err := proxyd.NewHTTPClient("http://"+hubAddr, "integration-hub-token")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if err := rightToken.Shutdown(false); err == nil {
		t.Fatal("shutdown must not be reachable over the hub's network mount")
	}
	if _, err := socketClient.Status(); err != nil {
		t.Fatalf("the daemon must still be running after a network shutdown attempt: %v", err)
	}
}
