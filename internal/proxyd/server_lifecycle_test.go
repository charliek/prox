package proxyd

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

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
	dead := deadPID(t)

	req := newTestRequest("/projects/dead", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = dead
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	s.requestManager.Record(proxy.RequestRecord{ID: "d1", Method: "GET", URL: "/d", Hostname: "api.local.dev", ProjectDir: "/projects/dead"})
	require.Equal(t, 1, s.requestManager.Count())

	// The daemon sweep: detect stale PIDs, then remove via the PID-guarded path.
	stale := s.registry.StalePIDs()
	require.Equal(t, []StaleProject{{Dir: "/projects/dead", PID: dead}}, stale)
	for _, sp := range stale {
		removed, _, _ := s.removeStaleProject(sp.Dir, sp.PID, sp.StartTime)
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

	req := newTestRequest("/projects/x", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = deadPID(t)
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

	removed, _, _ := s.removeStaleProject(stale[0].Dir, stale[0].PID, stale[0].StartTime)
	assert.False(t, removed, "guarded removal must skip the re-registered project")
	_, ok := s.registry.Lookup("api.local.dev", 443)
	assert.True(t, ok, "live registration's route must survive")
	assert.Equal(t, 1, s.requestManager.Count(), "live registration's records must survive")
}

// TestServer_RemoveStaleProject_SkipsReusedPID pins the reused-PID teardown fix
// (#61): the sweep snapshots a dead generation's identity (PID + start token),
// but a restart reuses the crashed PID under a NEW start token before removal.
// The identity guard must key on the token too, so the delayed sweep removal
// leaves the live restart — same PID, different token — alone.
func TestServer_RemoveStaleProject_SkipsReusedPID(t *testing.T) {
	s := newLifecycleServer()

	const (
		t1 int64 = 424242 // dead generation's start token
		t2       = t1 + 1 // restart's start token (same PID, new generation)
	)

	// Dead generation {D, deadP, T1}: a non-zero token so the guard does not
	// degrade to bare-PID. The dead PID makes StalePIDs detect it.
	deadP := deadPID(t)
	req := newTestRequest("/projects/x", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = deadP
	req.StartTime = t1
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	// Token-propagation half: the snapshot carries the dead generation's token.
	stale := s.registry.StalePIDs()
	require.Len(t, stale, 1)
	assert.Equal(t, deadP, stale[0].PID)
	assert.Equal(t, t1, stale[0].StartTime, "snapshot must carry the dead generation's start token")

	// Restart reuses the crashed PID under a new start token before the sweep
	// removal fires.
	s.registry.Deregister("/projects/x")
	reReq := newTestRequest("/projects/x", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	reReq.PID = deadP // same PID reused by the restart
	reReq.StartTime = t2
	_, _, err = s.registry.Register(reReq)
	require.NoError(t, err)
	s.requestManager.Record(proxy.RequestRecord{ID: "x1", Method: "GET", URL: "/x", ProjectDir: "/projects/x"})

	// Guard half: the delayed sweep removal targets the dead generation's
	// identity (deadP, T1), but the current registration carries T2 — the
	// identity guard must skip it regardless of the shared PID.
	removed, _, _ := s.removeStaleProject(stale[0].Dir, stale[0].PID, stale[0].StartTime)
	assert.False(t, removed, "reused-PID restart under a new token must survive the sweep")
	_, ok := s.registry.Lookup("api.local.dev", 443)
	assert.True(t, ok, "live restart's route must survive")
	assert.Equal(t, 1, s.requestManager.Count(), "live restart's records must survive")
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

// TestHandleGetRequests_LimitClamp pins that the daemon socket's ?limit=
// honors the same clamp semantics as the project API's
// parseProxyRequestParams: valid values in (0, MaxProxyRequests] apply,
// anything else (missing, zero, negative, or over the max) falls back to the
// default of 100.
func TestHandleGetRequests_LimitClamp(t *testing.T) {
	server, client, _ := startTestServer(t)
	rm := proxy.NewRequestManager(1500)
	server.SetRequestManager(rm)

	for i := 0; i < 150; i++ {
		rm.Record(proxy.RequestRecord{
			ID:         proxy.GenerateRequestID(time.Now(), "GET", "/x"),
			Method:     "GET",
			URL:        "/x",
			ProjectDir: "/projects/a",
		})
	}

	getCount := func(t *testing.T, query string) int {
		t.Helper()
		resp, err := client.httpClient.Get("http://proxyd/api/v1/requests?project=/projects/a" + query)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)

		var body struct {
			Requests []proxy.RequestRecord `json:"requests"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		return len(body.Requests)
	}

	t.Run("valid limit applies", func(t *testing.T) {
		assert.Equal(t, 10, getCount(t, "&limit=10"))
	})

	t.Run("missing limit defaults to 100", func(t *testing.T) {
		assert.Equal(t, 100, getCount(t, ""))
	})

	t.Run("zero limit defaults to 100", func(t *testing.T) {
		assert.Equal(t, 100, getCount(t, "&limit=0"))
	})

	t.Run("negative limit defaults to 100", func(t *testing.T) {
		assert.Equal(t, 100, getCount(t, "&limit=-5"))
	})

	t.Run("non-numeric limit defaults to 100", func(t *testing.T) {
		assert.Equal(t, 100, getCount(t, "&limit=abc"))
	})

	t.Run("over-max limit defaults to 100", func(t *testing.T) {
		assert.Equal(t, 100, getCount(t, "&limit=1001"))
	})

	t.Run("max limit applies", func(t *testing.T) {
		assert.Equal(t, 150, getCount(t, "&limit=1000"))
	})
}

// TestServer_SelfHeal_DeadPIDReRegister pins the #55 crash-restart fix at the
// server level: a same-dir registration whose holder PID is dead is replaced
// inline (200-equivalent), the dead generation's records AND their on-disk body
// files are purged, and an unrelated project is left completely untouched.
func TestServer_SelfHeal_DeadPIDReRegister(t *testing.T) {
	s := newLifecycleServer()

	// A body file that must be deleted when the dead generation's record is
	// purged — the eviction callback fires only for records carrying Details.
	bodyFile := filepath.Join(t.TempDir(), "body.bin")
	require.NoError(t, os.WriteFile(bodyFile, []byte("stale body"), 0o600))
	s.requestManager.SetEvictionCallback(func(id string) {
		if id == "dead-rec" {
			_ = os.Remove(bodyFile)
		}
	})

	// An unrelated project whose registration and records must survive.
	otherReq := newTestRequest("/projects/other", "other.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 5000}}, 0, 8443)
	otherReq.PID = os.Getpid()
	_, _, err := s.registry.Register(otherReq)
	require.NoError(t, err)
	s.requestManager.Record(proxy.RequestRecord{ID: "other-rec", ProjectDir: "/projects/other", Method: "GET", URL: "/o", Details: &proxy.RequestDetails{}})

	// Dead generation: register under a PID that is guaranteed dead.
	deadReq := newTestRequest("/projects/dead", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	deadReq.PID = deadPID(t)
	_, _, err = s.registry.Register(deadReq)
	require.NoError(t, err)
	s.requestManager.Record(proxy.RequestRecord{ID: "dead-rec", ProjectDir: "/projects/dead", Method: "GET", URL: "/d", Details: &proxy.RequestDetails{}})

	// Restart: same dir, a live PID. Self-heal replaces the dead registration.
	reReq := deadReq
	reReq.PID = os.Getpid()
	status, body := s.register(reReq)
	require.Equal(t, http.StatusOK, status, "self-heal re-register should succeed: %v", body)

	// New generation is registered under the live PID.
	assert.Equal(t, 2, s.registry.ProjectCount())
	route, ok := s.registry.Lookup("api.local.dev", 443)
	require.True(t, ok, "new generation's route must be registered")
	assert.Equal(t, os.Getpid(), route.PID, "route must carry the live generation's PID")

	// Dead generation's record purged, including its on-disk body file.
	_, ok = s.requestManager.GetByID("dead-rec")
	assert.False(t, ok, "dead generation's record must be purged")
	assert.NoFileExists(t, bodyFile, "dead generation's body file must be deleted")

	// Unrelated project untouched.
	_, ok = s.registry.Lookup("api.other.dev", 8443)
	assert.True(t, ok, "unrelated project's route must survive")
	_, ok = s.requestManager.GetByID("other-rec")
	assert.True(t, ok, "unrelated project's records must survive")
}

// TestServer_SelfHeal_LivePIDStillConflicts pins that a second prox up in the
// same dir while the first is ALIVE still fails with a 409 naming the holder,
// never a silent replace.
func TestServer_SelfHeal_LivePIDStillConflicts(t *testing.T) {
	s := newLifecycleServer()

	req := newTestRequest("/projects/live", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = os.Getpid() // self — guaranteed alive
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)

	status, body := s.register(req)
	require.Equal(t, http.StatusConflict, status)
	errResp, ok := body.(ErrorResponse)
	require.True(t, ok, "conflict body should be an ErrorResponse")
	assert.Equal(t, "REGISTRATION_CONFLICT", errResp.Code)
	assert.Contains(t, errResp.Error, "already registered by a running prox up")
	assert.Contains(t, errResp.Error, strconv.Itoa(os.Getpid()), "message must name the holding PID")
}

// TestServer_Register_RouteConflictNotRetried pins that a different-project
// route conflict (another project owning the hostname:port) is a plain 409 and
// is never mistaken for a replaceable stale registration.
func TestServer_Register_RouteConflictNotRetried(t *testing.T) {
	s := newLifecycleServer()

	reqA := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	reqA.PID = os.Getpid()
	_, _, err := s.registry.Register(reqA)
	require.NoError(t, err)

	reqB := newTestRequest("/projects/b", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}}, 0, 443)
	reqB.PID = os.Getpid()
	status, body := s.register(reqB)
	require.Equal(t, http.StatusConflict, status)
	errResp := body.(ErrorResponse)
	assert.Equal(t, "REGISTRATION_CONFLICT", errResp.Code)

	// A's registration is intact; B never displaced it.
	assert.Equal(t, 1, s.registry.ProjectCount())
	route, ok := s.registry.Lookup("api.local.dev", 443)
	require.True(t, ok)
	assert.Equal(t, "/projects/a", route.ProjectDir)
}

// TestHandleRegister_RejectsNonPositivePID pins the PID>0 validation: a PID of
// 0 or negative has signal-broadcast semantics that would read as permanently
// alive, so a crashed generation with such a PID could never be replaced.
func TestHandleRegister_RejectsNonPositivePID(t *testing.T) {
	_, client, _ := startTestServer(t)

	for _, pid := range []int{0, -1} {
		req := newTestRequest("/projects/a", "local.dev",
			map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
		req.Version = "test-version"
		req.PID = pid
		_, err := client.Register(req)
		require.Error(t, err, "pid=%d must be rejected", pid)
		assert.Contains(t, err.Error(), "pid must be a positive process id")
	}
}

// TestScheduleShutdownWhenEmpty_StaysEmpty pins that an emptied registry gets a
// graced shutdown request when it is still empty after the delay.
func TestScheduleShutdownWhenEmpty_StaysEmpty(t *testing.T) {
	s := newLifecycleServer()
	s.shutdownDelay = 20 * time.Millisecond

	s.scheduleShutdownWhenEmpty()

	select {
	case <-s.ShutdownCh():
		// shutdown requested — correct
	case <-time.After(2 * time.Second):
		t.Fatal("expected a shutdown request for a registry that stays empty")
	}
}

// TestScheduleShutdownWhenEmpty_RegistrationDuringDelayCancels pins that a
// registration landing within the grace window cancels the shutdown.
func TestScheduleShutdownWhenEmpty_RegistrationDuringDelayCancels(t *testing.T) {
	s := newLifecycleServer()
	s.shutdownDelay = 60 * time.Millisecond

	s.scheduleShutdownWhenEmpty()

	// A registration lands during the grace window, through the real locked
	// register path (the timer's re-check synchronizes on lifecycleMu, so the
	// direct registry call would not exercise the race this test pins).
	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = os.Getpid()
	status, body := s.register(req)
	require.Equal(t, http.StatusOK, status, "register failed: %v", body)

	select {
	case <-s.ShutdownCh():
		t.Fatal("shutdown must not fire when a registration lands during the grace period")
	case <-time.After(200 * time.Millisecond):
		// no shutdown — correct
	}
}

// TestScheduleShutdownWhenEmpty_StaleEpochStandsDown pins that a grace timer
// from an older empty period stands down after ANY lifecycle mutation, so it
// cannot cut a newer empty period's grace short.
func TestScheduleShutdownWhenEmpty_StaleEpochStandsDown(t *testing.T) {
	s := newLifecycleServer()
	s.shutdownDelay = 30 * time.Millisecond

	s.scheduleShutdownWhenEmpty() // timer A captures the pre-mutation epoch

	// Mutations during A's grace: a register then a removal, leaving the
	// registry empty again but at a newer epoch.
	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = os.Getpid()
	status, body := s.register(req)
	require.Equal(t, http.StatusOK, status, "register failed: %v", body)
	s.removeProject("/projects/a")

	// Timer A fires into an empty registry but a changed epoch — it must
	// stand down rather than shut the daemon.
	select {
	case <-s.ShutdownCh():
		t.Fatal("a stale-epoch timer must not shut the daemon down")
	case <-time.After(120 * time.Millisecond):
	}

	// A fresh timer at the current epoch shuts down normally.
	s.scheduleShutdownWhenEmpty()
	select {
	case <-s.ShutdownCh():
		// correct
	case <-time.After(2 * time.Second):
		t.Fatal("a current-epoch timer over an empty registry must shut down")
	}
}

// TestRegister_WhileShuttingDown_Returns503 pins that a register that queued
// behind a shutdown decision reports SHUTTING_DOWN instead of a false success
// from a daemon that is already exiting.
func TestRegister_WhileShuttingDown_Returns503(t *testing.T) {
	s := newLifecycleServer()
	s.RequestShutdown()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = os.Getpid()
	status, body := s.register(req)
	require.Equal(t, http.StatusServiceUnavailable, status)
	er, ok := body.(ErrorResponse)
	require.True(t, ok, "expected ErrorResponse, got %T", body)
	assert.Equal(t, "SHUTTING_DOWN", er.Code)
	assert.True(t, s.registry.IsEmpty(), "no registration may land after shutdown")
}

// TestQuiesceForTeardown_DrainsInFlightTransaction pins the teardown barrier
// (#60): quiesceForTeardown sets the shutdown flag FIRST, then blocks on
// lifecycleMu until the single transaction that was already in flight when the
// flag was set completes. Modeling the in-flight transaction by holding
// lifecycleMu directly lets the test assert the flag-before-lock half
// deterministically (the flag closes while the lock is held) and the
// barrier-waits half robustly (the return cannot happen until the lock frees).
func TestQuiesceForTeardown_DrainsInFlightTransaction(t *testing.T) {
	s := newLifecycleServer()

	// Simulate a lifecycle transaction already in flight by holding the mutex.
	s.lifecycleMu.Lock()

	done := make(chan struct{})
	go func() {
		s.quiesceForTeardown()
		close(done)
	}()

	// Flag-before-lock: the shutdown flag closes even while the in-flight
	// transaction still holds lifecycleMu.
	select {
	case <-s.ShutdownCh():
		// correct — flag set first
	case <-time.After(2 * time.Second):
		t.Fatal("quiesceForTeardown must set the shutdown flag before taking the barrier lock")
	}

	// The barrier must not return while the in-flight holder still owns
	// lifecycleMu. "A goroutine is blocked on a mutex" is not directly
	// observable in Go, so give the quiesce goroutine ample opportunity to run
	// to completion: if the barrier lock were absent, quiesceForTeardown would be
	// nothing but RequestShutdown and `done` would close within this window. With
	// the barrier present, `done` stays open for as long as we hold the lock — no
	// matter how slow the machine — so this assertion is robust in the correct
	// direction and reliably fails a regression that drops the barrier.
	for i := 0; i < 50; i++ {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case <-done:
			t.Fatal("quiesceForTeardown returned before the in-flight transaction released lifecycleMu")
		default:
		}
	}

	// Release the in-flight transaction; the barrier now completes.
	s.lifecycleMu.Unlock()
	select {
	case <-done:
		// correct — barrier drained the in-flight holder and returned
	case <-time.After(2 * time.Second):
		t.Fatal("quiesceForTeardown must return once the in-flight transaction releases lifecycleMu")
	}
}

// TestRemoveProject_NoOpsWhileShuttingDown pins that once the shutdown flag is
// set, the public removal entries no-op instead of racing the exiting daemon's
// physical teardown (dynamicProxy.Shutdown / captureMgr.Cleanup). The route and
// its records must be left intact for the exiting daemon to reap. The
// register-after-flag → 503 counterpart is covered by
// TestRegister_WhileShuttingDown_Returns503.
func TestRemoveProject_NoOpsWhileShuttingDown(t *testing.T) {
	t.Run("removeProject", func(t *testing.T) {
		s := newLifecycleServer()
		req := newTestRequest("/projects/a", "local.dev",
			map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
		req.PID = os.Getpid()
		_, _, err := s.registry.Register(req)
		require.NoError(t, err)
		s.requestManager.Record(proxy.RequestRecord{ID: "a1", Method: "GET", URL: "/a", ProjectDir: "/projects/a"})

		s.RequestShutdown()

		removed, emptyPorts := s.removeProject("/projects/a")
		assert.Nil(t, removed, "removeProject must no-op while shutting down")
		assert.Nil(t, emptyPorts)

		_, ok := s.registry.Lookup("api.local.dev", 443)
		assert.True(t, ok, "route must survive for the exiting daemon to reap")
		assert.Equal(t, 1, s.requestManager.Count(), "records must not be purged mid-teardown")
	})

	t.Run("removeStaleProject", func(t *testing.T) {
		s := newLifecycleServer()
		req := newTestRequest("/projects/a", "local.dev",
			map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
		req.PID = os.Getpid()
		_, _, err := s.registry.Register(req)
		require.NoError(t, err)
		s.requestManager.Record(proxy.RequestRecord{ID: "a1", Method: "GET", URL: "/a", ProjectDir: "/projects/a"})

		s.RequestShutdown()

		removed, hostnames, emptyPorts := s.removeStaleProject("/projects/a", os.Getpid(), 0)
		assert.False(t, removed, "removeStaleProject must no-op while shutting down")
		assert.Nil(t, hostnames)
		assert.Nil(t, emptyPorts)

		_, ok := s.registry.Lookup("api.local.dev", 443)
		assert.True(t, ok, "route must survive for the exiting daemon to reap")
		assert.Equal(t, 1, s.requestManager.Count(), "records must not be purged mid-teardown")
	})
}

// TestHandleShutdown_SetsFlagBeforeResponse pins that handleShutdown sets the
// shutdown flag synchronously (before the response), so the 200 is the shutdown
// linearization point rather than a later goroutine. A register racing the 200
// then reliably observes isShuttingDown().
func TestHandleShutdown_SetsFlagBeforeResponse(t *testing.T) {
	s := newLifecycleServer() // empty registry — no ACTIVE_ROUTES gate

	req := httptest.NewRequest(http.MethodPost, "/api/v1/shutdown", nil)
	rec := httptest.NewRecorder()
	s.handleShutdown(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, s.isShuttingDown(), "flag must be set synchronously by the time handleShutdown returns")
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
