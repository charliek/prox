package proxyd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// Wire the per-project ring set so the request endpoints resolve rings;
	// tests that need records ensure their project's ring via server.managers.
	server.SetManagers(NewManagers(constants.DefaultProxyRequestBufferSize, nil))

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

// TestClientRequests pins the Client.Requests contract against the real daemon
// endpoint: it URL-escapes awkward project dirs (spaces/#) so the server matches
// them, honors the explicit limit, and decodes the {"requests":[...]} wrapper.
func TestClientRequests(t *testing.T) {
	server, client, _ := startTestServer(t)

	// A project dir with a space and a '#': if the client did not URL-escape it,
	// the '#' would be treated as a fragment and the server's project filter
	// would never match, returning zero records.
	const dir = "/weird dir #1"
	dirRing := server.managers.ensure(dir)
	for i := 0; i < 5; i++ {
		dirRing.Record(proxy.RequestRecord{
			ID: fmt.Sprintf("r%d", i), ProjectDir: dir, Method: "GET", URL: "/x", Timestamp: time.Now(),
		})
	}
	// A record for a different project (its own ring) must never leak into the
	// filtered result.
	server.managers.ensure("/other").Record(proxy.RequestRecord{ID: "other", ProjectDir: "/other", Method: "GET", URL: "/y", Timestamp: time.Now()})

	t.Run("escapes dir, honors limit, decodes wrapper", func(t *testing.T) {
		recs, err := client.Requests(t.Context(), dir, 3)
		require.NoError(t, err)
		require.Len(t, recs, 3, "explicit limit honored")
		for _, r := range recs {
			assert.Equal(t, dir, r.ProjectDir, "escaped project matched server-side")
		}
	})

	t.Run("full snapshot with MaxProxyRequests limit", func(t *testing.T) {
		recs, err := client.Requests(t.Context(), dir, constants.MaxProxyRequests)
		require.NoError(t, err)
		assert.Len(t, recs, 5, "all of the project's records, none of /other's")
	})
}

// TestClientRequests_TruncatedBodyAllOrNothing pins that a truncated 200
// response yields (nil, error) — no partial records leak through the wrapper.
func TestClientRequests_TruncatedBodyAllOrNothing(t *testing.T) {
	socketPath := startRawSSEServerMux(t,
		func(_ http.ResponseWriter, _ *http.Request) {},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"requests":[{"id":"abc","method":"GET"`) // truncated mid-object
		})

	client := NewClient(socketPath)
	recs, err := client.Requests(context.Background(), "/p", constants.MaxProxyRequests)
	require.Error(t, err)
	assert.Nil(t, recs, "all-or-nothing: no partial records on a truncated body")
}

// TestClientRequests_WrongShapeWrapper pins that a 200 with valid JSON of the
// wrong shape ({} — missing the "requests" key) is treated as malformed rather
// than a silent empty backfill: the daemon always emits a non-nil slice.
func TestClientRequests_WrongShapeWrapper(t *testing.T) {
	socketPath := startRawSSEServerMux(t,
		func(_ http.ResponseWriter, _ *http.Request) {},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		})

	client := NewClient(socketPath)
	recs, err := client.Requests(context.Background(), "/p", constants.MaxProxyRequests)
	require.Error(t, err)
	assert.Nil(t, recs, "missing requests key must error, not read as empty")
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
