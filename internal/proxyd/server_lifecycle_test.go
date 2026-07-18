package proxyd

import (
	"io"
	"log/slog"
	"os/exec"
	"testing"

	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLifecycleServer builds a Server wired to a registry and request manager
// but without starting the socket listener — enough to exercise the
// consolidated removeProject path directly.
func newLifecycleServer() *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	s := NewServer(ServerConfig{SocketPath: "", Logger: logger, Version: "test"})
	s.SetRegistry(NewRegistry())
	s.SetRequestManager(proxy.NewRequestManager(100))
	return s
}

// TestServer_RemoveProject_ScopedByProject pins the consolidated removal path:
// two projects sharing a hostname on different ports don't remove each other's
// routes or purge each other's records.
func TestServer_RemoveProject_ScopedByProject(t *testing.T) {
	s := newLifecycleServer()

	// A and B both own hostname api.local.dev, but on different ports.
	_, _, err := s.registry.Register(newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443))
	require.NoError(t, err)
	_, _, err = s.registry.Register(newTestRequest("/projects/b", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}}, 0, 8443))
	require.NoError(t, err)

	// Records for each project, same hostname.
	s.requestManager.Record(proxy.RequestRecord{ID: "a1", Method: "GET", URL: "/a", Hostname: "api.local.dev", ProjectDir: "/projects/a"})
	s.requestManager.Record(proxy.RequestRecord{ID: "b1", Method: "GET", URL: "/b", Hostname: "api.local.dev", ProjectDir: "/projects/b"})

	removed, _ := s.removeProject("/projects/a")
	assert.Equal(t, []string{"api.local.dev"}, removed)

	// A's route gone, B's route (same hostname, different port) survives.
	_, okA := s.registry.Lookup("api.local.dev", 443)
	assert.False(t, okA, "A's route should be removed")
	_, okB := s.registry.Lookup("api.local.dev", 8443)
	assert.True(t, okB, "B's route should survive")

	// A's records purged, B's survive.
	remaining := s.requestManager.Recent(proxy.RequestFilter{})
	require.Len(t, remaining, 1)
	assert.Equal(t, "b1", remaining[0].ID)
}

// TestServer_StalePIDSweep_PurgesRecords pins the crash path (CodeRabbit H2):
// a project whose PID has died is detected by StalePIDs and removed through the
// same consolidated removeProject path, so its captured records are purged —
// the stale-PID sweep must not bypass record cleanup.
func TestServer_StalePIDSweep_PurgesRecords(t *testing.T) {
	s := newLifecycleServer()

	// Produce a PID that is guaranteed dead: run a process to completion.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	deadPID := cmd.Process.Pid

	req := newTestRequest("/projects/dead", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = deadPID
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	s.requestManager.Record(proxy.RequestRecord{ID: "d1", Method: "GET", URL: "/d", Hostname: "api.local.dev", ProjectDir: "/projects/dead"})
	require.Equal(t, 1, s.requestManager.Count())

	// The daemon sweep: detect stale PIDs, then remove via the PID-guarded path.
	stale := s.registry.StalePIDs()
	require.Equal(t, []StaleProject{{Dir: "/projects/dead", PID: deadPID}}, stale)
	for _, sp := range stale {
		removed, _, _ := s.removeStaleProject(sp.Dir, sp.PID)
		assert.True(t, removed)
	}

	assert.True(t, s.registry.IsEmpty(), "registry should be empty after stale sweep")
	assert.Equal(t, 0, s.requestManager.Count(), "stale project's records must be purged")
}

// TestServer_RemoveStaleProject_SkipsReRegistered pins the detection→removal
// race fix: when a project re-registers (new live PID) between StalePIDs
// detection and removal, the guarded removal must leave the new registration
// and its records alone.
func TestServer_RemoveStaleProject_SkipsReRegistered(t *testing.T) {
	s := newLifecycleServer()

	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	deadPID := cmd.Process.Pid

	req := newTestRequest("/projects/x", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = deadPID
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	stale := s.registry.StalePIDs()
	require.Len(t, stale, 1)

	// Simulate the race: project deregisters and re-registers with a live PID
	// after detection but before removal.
	s.registry.Deregister("/projects/x")
	reReq := newTestRequest("/projects/x", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3001}}, 0, 443)
	reReq.PID = 1 // always-alive PID (launchd/init)
	_, _, err = s.registry.Register(reReq)
	require.NoError(t, err)
	s.requestManager.Record(proxy.RequestRecord{ID: "x1", Method: "GET", URL: "/x", ProjectDir: "/projects/x"})

	removed, _, _ := s.removeStaleProject(stale[0].Dir, stale[0].PID)
	assert.False(t, removed, "guarded removal must skip the re-registered project")
	_, ok := s.registry.Lookup("api.local.dev", 443)
	assert.True(t, ok, "live registration's route must survive")
	assert.Equal(t, 1, s.requestManager.Count(), "live registration's records must survive")
}

// TestHandleRegister_RejectsEmptyProjectDir pins the identity requirement:
// records are filtered and purged by ProjectDir, so a registration without one
// would create records nothing could ever clean up.
func TestHandleRegister_RejectsEmptyProjectDir(t *testing.T) {
	_, client, _ := startTestServer(t)

	req := newTestRequest("", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.Version = "test-version" // pass the exact-match version gate
	_, err := client.Register(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_dir is required")
}

// TestRequestEndpoints_RequireProjectParam pins that both daemon request
// endpoints reject an unscoped query rather than matching every project's
// records.
func TestRequestEndpoints_RequireProjectParam(t *testing.T) {
	server, client, _ := startTestServer(t)
	server.SetRequestManager(proxy.NewRequestManager(10))

	resp, err := client.httpClient.Get("http://proxyd/api/v1/requests")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 400, resp.StatusCode)

	respStream, err := client.httpClient.Get("http://proxyd/api/v1/requests/stream")
	require.NoError(t, err)
	defer respStream.Body.Close()
	assert.Equal(t, 400, respStream.StatusCode)
}

// TestServer_RemoveProject_NilRequestManager guards the daemon-startup window
// where the request manager may not be set yet.
func TestServer_RemoveProject_NilRequestManager(t *testing.T) {
	s := newLifecycleServer()
	s.requestManager = nil // model the startup window before the manager is wired

	_, _, err := s.registry.Register(newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443))
	require.NoError(t, err)

	require.NotPanics(t, func() { s.removeProject("/projects/a") })
	assert.True(t, s.registry.IsEmpty())
}
