package proxyd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spillRes drives a response-body capture larger than the 64KB inline threshold
// so it spills to disk through the accountant, returning the spill file path.
func spillRes(t *testing.T, cm *proxy.CaptureManager, requestID string, n int) string {
	t.Helper()
	rec := httptest.NewRecorder()
	crw := cm.WrapResponseWriter(rec, proxy.CapturePolicy{})
	_, err := crw.Write([]byte(strings.Repeat("x", n)))
	require.NoError(t, err)
	body, _ := cm.FinalizeResponse(requestID, crw, proxy.CapturePolicy{})
	require.NotEmpty(t, body.FilePath, "body over inline threshold must spill to disk")
	return body.FilePath
}

const (
	gib512 = 512 * 1024 * 1024
	gib2   = 2 * 1024 * 1024 * 1024
)

// captureReq builds a register request with capture enabled and an explicit disk
// budget (0 = unset -> daemon default).
func captureReq(projectDir string, httpsPort int, diskBudget int64) RegisterRequest {
	req := newTestRequest(projectDir, "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, httpsPort)
	req.CaptureEnabled = true
	req.DiskBudget = diskBudget
	return req
}

// TestEffectiveCaptureDiskBudget_Matrix pins the daemon-wide effective-budget
// computation across the pinned matrix (#69): unset defaults, an explicit value
// can only LOWER the bound, and deregistration recomputes.
func TestEffectiveCaptureDiskBudget_Matrix(t *testing.T) {
	t.Run("no capture-enabled projects -> default", func(t *testing.T) {
		reg := NewRegistry()
		assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("unset + unset -> default", func(t *testing.T) {
		reg := NewRegistry()
		_, _, err := reg.Register(captureReq("/a", 443, 0))
		require.NoError(t, err)
		_, _, err = reg.Register(captureReq("/b", 8443, 0))
		require.NoError(t, err)
		assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("unset + 512MB -> 512MB", func(t *testing.T) {
		reg := NewRegistry()
		_, _, err := reg.Register(captureReq("/a", 443, 0))
		require.NoError(t, err)
		_, _, err = reg.Register(captureReq("/b", 8443, gib512))
		require.NoError(t, err)
		assert.Equal(t, int64(gib512), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("2GB alone -> 2GB (raising allowed when the only capture-enabled project opts in)", func(t *testing.T) {
		reg := NewRegistry()
		_, _, err := reg.Register(captureReq("/a", 443, gib2))
		require.NoError(t, err)
		assert.Equal(t, int64(gib2), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("2GB + unset -> default (explicit cannot raise ANOTHER project's default)", func(t *testing.T) {
		reg := NewRegistry()
		_, _, err := reg.Register(captureReq("/a", 443, gib2))
		require.NoError(t, err)
		// B is capture-enabled but leaves the budget unset -> contributes the
		// default to the min, holding the effective bound down to 1GiB.
		_, _, err = reg.Register(captureReq("/b", 8443, 0))
		require.NoError(t, err)
		assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("512MB deregisters -> back to default", func(t *testing.T) {
		reg := NewRegistry()
		_, _, err := reg.Register(captureReq("/a", 443, 0))
		require.NoError(t, err)
		_, _, err = reg.Register(captureReq("/b", 8443, gib512))
		require.NoError(t, err)
		require.Equal(t, int64(gib512), reg.EffectiveCaptureDiskBudget())

		reg.Deregister("/b")
		assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), reg.EffectiveCaptureDiskBudget())
	})

	t.Run("capture-disabled project with tiny budget does not lower bound", func(t *testing.T) {
		reg := NewRegistry()
		req := captureReq("/a", 443, gib512)
		req.CaptureEnabled = false // disabled -> must not influence the bound
		_, _, err := reg.Register(req)
		require.NoError(t, err)
		assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), reg.EffectiveCaptureDiskBudget())
	})
}

// budgetServer builds a lifecycle server wired to a real capture manager so
// syncCaptureBudget pushes the effective budget onto it (#69).
func budgetServer(t *testing.T) *Server {
	t.Helper()
	s := newLifecycleServer()
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	s.SetCaptureManager(cm)
	return s
}

// TestSyncCaptureBudget_RegisterDeregister pins that a full register drives the
// capture manager's budget and a deregister recomputes it back up (#69).
func TestSyncCaptureBudget_RegisterDeregister(t *testing.T) {
	s := budgetServer(t)

	status, _ := s.register(captureReq("/a", 443, gib512))
	require.Equal(t, 200, status)
	assert.Equal(t, int64(gib512), s.captureMgr.DiskBudget(), "register lowers the bound")

	s.removeProject("/a")
	assert.Equal(t, int64(constants.DefaultCaptureDiskBudget), s.captureMgr.DiskBudget(),
		"deregister recomputes back to default")
}

// TestSyncCaptureBudget_NoOpReRegister pins that an idempotent no-op re-register
// (same config + identity) takes the D6a no-op refresh path and does NOT re-run
// syncCaptureBudget: a sentinel budget set out-of-band on the manager survives
// the no-op, proving no enforcement fired (#69).
func TestSyncCaptureBudget_NoOpReRegister(t *testing.T) {
	s := budgetServer(t)

	pid, token := liveIdentity(t)
	req := captureReq("/a", 443, 0) // unset -> default
	req.PID, req.StartTime = pid, token
	status, _ := s.register(req)
	require.Equal(t, 200, status)

	// Poke a sentinel budget the registry would never compute. If the no-op
	// refresh path (wrongly) called syncCaptureBudget, it would overwrite this
	// with the default; a correct no-op leaves it untouched.
	const sentinel = int64(1234567)
	s.captureMgr.SetDiskBudget(sentinel)

	// Re-register with identical config + identity -> no-op refresh (D6a).
	status2, _ := s.register(req)
	require.Equal(t, 200, status2)
	assert.Equal(t, sentinel, s.captureMgr.DiskBudget(),
		"no-op refresh must not re-run enforcement/sync")
}

// TestSyncCaptureBudget_RollbackRestoresPriorBudget pins that a registration
// whose listener bind FAILS is rolled back and syncCaptureBudget recomputes the
// prior effective budget — the failed project must not leave its (lower) budget
// in force (#69).
func TestSyncCaptureBudget_RollbackRestoresPriorBudget(t *testing.T) {
	s := newProxyServer(t)
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	s.SetCaptureManager(cm)

	// Project A registers cleanly with a 512MB budget -> effective 512MB.
	a := captureReq("/a", 0, gib512)
	a.HTTPPort = freePort(t)
	registerOK(t, s, a)
	require.Equal(t, int64(gib512), s.captureMgr.DiskBudget())

	// Occupy a port (wildcard, matching how the proxy binds ":port") so project
	// B's bind fails after the bounded retry window.
	occupiedPort := freePort(t)
	occupied, err := net.Listen("tcp", fmt.Sprintf(":%d", occupiedPort))
	require.NoError(t, err)
	defer occupied.Close()

	// Project B asks for an even lower budget but its bind fails -> rollback.
	b := captureReq("/b", 0, 256*1024*1024)
	b.ProjectDir = "/b"
	b.HTTPPort = occupiedPort
	status, _ := s.register(b)
	require.Equal(t, http.StatusInternalServerError, status)

	// B was rolled back; the effective budget is A's 512MB, NOT B's 256MB.
	assert.Nil(t, s.registry.projects["/b"], "failed registration must be rolled back")
	assert.Equal(t, int64(gib512), s.captureMgr.DiskBudget(),
		"rollback must restore the prior effective budget")
}

// TestSyncCaptureBudget_ChangedBudgetReRegister pins that re-registering the same
// project with a CHANGED disk budget forces a real re-register (registrationMatches
// returns false) and updates the effective bound (#69).
func TestSyncCaptureBudget_ChangedBudgetReRegister(t *testing.T) {
	s := budgetServer(t)

	pid, token := liveIdentity(t)
	req := captureReq("/a", 443, 0) // unset -> default
	req.PID, req.StartTime = pid, token
	status, _ := s.register(req)
	require.Equal(t, 200, status)
	require.Equal(t, int64(constants.DefaultCaptureDiskBudget), s.captureMgr.DiskBudget())

	// Same identity, but a lower explicit budget -> not a no-op refresh.
	req2 := req
	req2.DiskBudget = gib512
	assert.False(t, s.registry.registrationMatches(req2), "changed budget must not match")
	status2, _ := s.register(req2)
	require.Equal(t, 200, status2)
	assert.Equal(t, int64(gib512), s.captureMgr.DiskBudget(), "changed budget lowers the bound")
}

// TestStatusEndpoint_ReportsCaptureDiskUsage pins that /status surfaces
// capture_disk_used / capture_disk_budget and that a spill beyond a tiny budget
// evicts the oldest record group's files off disk (#69).
func TestStatusEndpoint_ReportsCaptureDiskUsage(t *testing.T) {
	server, client, _ := startTestServer(t)

	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	server.SetCaptureManager(cm)

	// Tiny budget: two 100KB bodies fit, a third evicts the oldest group.
	const body = 100 * 1024
	cm.SetDiskBudget(250 * 1024)

	aPath := spillRes(t, cm, "A", body)
	_ = spillRes(t, cm, "B", body)

	// Status reports both bodies on disk under the configured budget.
	status, err := client.Status()
	require.NoError(t, err)
	assert.Equal(t, int64(250*1024), status.CaptureDiskBudget)
	assert.Equal(t, int64(2*body), status.CaptureDiskUsed)

	// A third spill trips the budget and evicts the oldest group (A).
	cPath := spillRes(t, cm, "C", body)

	assert.False(t, fileExistsAt(t, aPath), "oldest group A's file must be evicted off disk")
	assert.True(t, fileExistsAt(t, cPath), "newest group C survives")

	status2, err := client.Status()
	require.NoError(t, err)
	assert.Equal(t, int64(2*body), status2.CaptureDiskUsed, "used reflects two surviving groups")
}

func fileExistsAt(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Clean(path))
	if err == nil {
		return true
	}
	require.True(t, os.IsNotExist(err), "unexpected stat error: %v", err)
	return false
}
