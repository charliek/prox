package proxyd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures & helpers for the on-502 dead-owner probe (#74) ---

// newProbeServer builds a Server wired to a real DynamicProxy (HTTP only, no
// certMgr) with the dead-route remover installed exactly as RunDaemon wires it:
// an identity-guarded removeStaleProject plus the sweep epilogue (removal log +
// scheduleShutdownWhenEmpty). It returns both so probe tests can drive the gate
// directly (dp.triggerDeadRouteProbe) or through real 502s (serve).
func newProbeServer(t *testing.T) (*Server, *DynamicProxy) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	s := NewServer(ServerConfig{SocketPath: "", Logger: logger, Version: "test"})
	reg := NewRegistry()
	ms := NewManagers(100, nil)
	dp := NewDynamicProxy(reg, nil, ms, nil, logger)
	s.SetRegistry(reg)
	s.SetProxy(dp)
	s.SetManagers(ms)
	dp.SetDeadRouteRemover(func(dir string, pid int, startTime int64) {
		removed, hostnames, emptyPorts := s.removeStaleProject(dir, pid, startTime)
		if !removed {
			return
		}
		logger.Warn("cleaned stale project registration",
			"project", dir, "pid", pid, "start_time", startTime,
			"removed_hostnames", hostnames, "closed_ports", emptyPorts,
			"trigger", "on-502-probe")
		if reg.IsEmpty() {
			s.scheduleShutdownWhenEmpty()
		}
	})
	t.Cleanup(func() { _ = dp.Shutdown(context.Background()) })
	return s, dp
}

// deadIdentity spawns a real process, captures its start token WHILE ALIVE, then
// kills and reaps it — returning the (pid, startTime) of a dead generation with
// a faithful start token. Unlike a bare dead PID, this exercises the real
// PID-reuse semantics: if the OS later reuses the PID, the captured token no
// longer matches, so daemon.IsProcessAlive still reads the generation as dead.
// On platforms without a start-token implementation the token is 0 (bare-PID
// fallback), which is still correct for a reaped PID.
func deadIdentity(t *testing.T) (pid int, startTime int64) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	pid = cmd.Process.Pid
	token, _ := daemon.ProcessStartTime(pid) // 0 where unsupported; fine for a dead PID
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait() // reap so the PID names no live generation
	require.False(t, daemon.IsProcessAlive(pid, token), "deadIdentity must be dead after reap")
	return pid, token
}

// registerProbeRoute registers a single http route (api.local.dev:80) for dir,
// owned by (pid, startTime), pointing at targetHost:targetPort. It goes straight
// through the registry (no listener bind) — probe tests drive the handler via
// serve, which never binds a real port.
func registerProbeRoute(t *testing.T, s *Server, dir string, pid int, startTime int64, targetHost string, targetPort int) {
	t.Helper()
	req := RegisterRequest{
		ProjectDir: dir, PID: pid, StartTime: startTime, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: targetHost, Port: targetPort}},
		HTTPPort: 80,
	}
	_, _, err := s.registry.Register(req)
	require.NoError(t, err)
}

// probeSnapshot reads a dir's probe-gate state under probeMu (test-only).
func probeSnapshot(dp *DynamicProxy, dir string) (exists, inFlight, pending bool) {
	dp.probeMu.Lock()
	defer dp.probeMu.Unlock()
	st, ok := dp.probes[dir]
	if !ok {
		return false, false, false
	}
	return true, st.inFlight, st.pending
}

// waitFor polls cond until it is true or the deadline elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// startBackendOn binds a real HTTP backend on 127.0.0.1:port, retrying the small
// ephemeral-port race, and tears it down on cleanup.
func startBackendOn(t *testing.T, port int, h http.HandlerFunc) {
	t.Helper()
	var ln net.Listener
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bind backend on %d: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// mockClock is an injectable clock whose Sleep advances virtual time instead of
// blocking, so trailing-probe timing is deterministic with no wall-clock waits.
type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMockClock() *mockClock { return &mockClock{now: time.Unix(0, 0)} }

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mockClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// --- pure gate ---

// TestDecideProbe_Gate table-tests the pure probe gate with an injected clock:
// the atomic check-AND-set that pins single-in-flight, the trailing-edge wait
// inside the cooldown, and the immediate probe once the interval has elapsed.
func TestDecideProbe_Gate(t *testing.T) {
	const interval = time.Second
	base := time.Unix(1000, 0)

	tests := []struct {
		name         string
		st           probeState
		now          time.Time
		wantSpawn    bool
		wantWait     time.Duration
		wantInFlight bool
		wantPending  bool
	}{
		{
			name:         "fresh state probes immediately and claims the chain",
			st:           probeState{},
			now:          base,
			wantSpawn:    true,
			wantWait:     0,
			wantInFlight: true,
			wantPending:  false,
		},
		{
			name:         "chain in flight only records pending",
			st:           probeState{inFlight: true, lastStart: base},
			now:          base.Add(10 * time.Millisecond),
			wantSpawn:    false,
			wantWait:     0,
			wantInFlight: true,
			wantPending:  true,
		},
		{
			name:         "within cooldown defers to the trailing edge",
			st:           probeState{lastStart: base.Add(-400 * time.Millisecond)},
			now:          base,
			wantSpawn:    true,
			wantWait:     600 * time.Millisecond,
			wantInFlight: true,
			wantPending:  false,
		},
		{
			name:         "exactly at lastStart waits the full interval",
			st:           probeState{lastStart: base},
			now:          base,
			wantSpawn:    true,
			wantWait:     interval,
			wantInFlight: true,
			wantPending:  false,
		},
		{
			name:         "past the interval probes immediately",
			st:           probeState{lastStart: base.Add(-2 * interval)},
			now:          base,
			wantSpawn:    true,
			wantWait:     0,
			wantInFlight: true,
			wantPending:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			spawn, wait := decideProbe(&st, tc.now, interval)
			assert.Equal(t, tc.wantSpawn, spawn, "spawn")
			assert.Equal(t, tc.wantWait, wait, "wait")
			assert.Equal(t, tc.wantInFlight, st.inFlight, "inFlight")
			assert.Equal(t, tc.wantPending, st.pending, "pending")
		})
	}
}

// --- behavior ---

// TestDeadRouteProbe_DeadOwnerRemovesRoute pins the headline case: a single
// proxy-generated 502 for a route whose owner is dead converges the route
// promptly (no 30s sweep wait), destroys the project's ring, and makes
// subsequent requests 404.
func TestDeadRouteProbe_DeadOwnerRemovesRoute(t *testing.T) {
	deadPid, deadToken := deadIdentity(t)
	s, dp := newProbeServer(t)

	// Route owned by the dead generation, pointing at a closed port → 502.
	registerProbeRoute(t, s, "/projects/dead", deadPid, deadToken, "127.0.0.1", 1)
	recordInto(s, proxy.RequestRecord{ID: "d1", ProjectDir: "/projects/dead", Method: "GET", URL: "/d"})
	require.NotNil(t, projectRing(s, "/projects/dead"))

	rec := serve(dp, "GET", "http://api.local.dev/down", "", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, "closed backend must 502")

	// Wait on ring destruction (the LAST step of the reap, strictly after route
	// removal) so the assertions observe a fully-settled reap — and so testify
	// never reflects over a *RequestManager the probe is concurrently purging.
	require.True(t, waitFor(t, 2*time.Second, func() bool {
		return projectRing(s, "/projects/dead") == nil
	}), "dead owner's route must be reaped promptly after one 502")

	_, ok := s.registry.Lookup("api.local.dev", 80)
	assert.False(t, ok, "reaped route must be gone from the registry")

	// A subsequent request now has no route → 404.
	rec404 := serve(dp, "GET", "http://api.local.dev/again", "", nil)
	assert.Equal(t, http.StatusNotFound, rec404.Code, "removed route must 404")
}

// TestDeadRouteProbe_LiveOwnerSurvivesFlapping pins the structural flap guard: a
// route owned by a LIVE prox up (self identity) whose backend is down serves
// repeated 502s but is never deregistered, and serves 200 once its backend
// returns — the registry stores the OWNER's identity, so a flapping backend can
// never probe as dead.
func TestDeadRouteProbe_LiveOwnerSurvivesFlapping(t *testing.T) {
	selfToken := mustSelfToken(t)
	s, dp := newProbeServer(t)

	backendPort := freePort(t) // closed for now → 502s
	registerProbeRoute(t, s, "/projects/live", os.Getpid(), selfToken, "127.0.0.1", backendPort)

	for i := 0; i < 5; i++ {
		rec := serve(dp, "GET", "http://api.local.dev/flap", "", nil)
		require.Equal(t, http.StatusBadGateway, rec.Code, "closed backend must 502 (iter %d)", i)
	}

	// Give any probe chains time to run; a live owner must never be removed.
	require.False(t, waitFor(t, 500*time.Millisecond, func() bool {
		_, ok := s.registry.Lookup("api.local.dev", 80)
		return !ok
	}), "a live owner must survive repeated backend 502s")

	// Backend comes up on the same port: the SAME registration now serves 200.
	startBackendOn(t, backendPort, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	var code int
	require.True(t, waitFor(t, 2*time.Second, func() bool {
		code = serve(dp, "GET", "http://api.local.dev/ok", "", nil).Code
		return code == http.StatusOK
	}), "surviving registration must serve 200 once the backend returns (last=%d)", code)
}

// TestDeadRouteProbe_SuppressedTrailingProbeReaps pins panel correction 5: a 502
// that arrives inside the cooldown window (after a first probe found the owner
// alive) still triggers a trailing-edge probe, so a post-death 502 is never
// lost — removal happens with NO further requests after the owner dies.
func TestDeadRouteProbe_SuppressedTrailingProbeReaps(t *testing.T) {
	s, dp := newProbeServer(t)

	// Injected liveness: alive until we flip it; injected clock so the trailing
	// wait resolves instantly (Sleep advances virtual time).
	var alive atomic.Bool
	alive.Store(true)
	dp.probeIsAlive = func(int, int64) bool { return alive.Load() }
	clock := newMockClock()
	dp.probeClock = clock.Now
	dp.probeSleep = clock.Sleep

	registerProbeRoute(t, s, "/projects/x", os.Getpid(), 0, "127.0.0.1", 1)

	// First 502 → immediate probe (wait=0) → owner alive → route survives.
	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/one", "", nil).Code)
	require.True(t, waitFor(t, time.Second, func() bool {
		exists, inFlight, _ := probeSnapshot(dp, "/projects/x")
		return exists && !inFlight
	}), "first probe (owner alive) must settle without removal")
	_, ok := s.registry.Lookup("api.local.dev", 80)
	require.True(t, ok, "live owner's route must survive the first probe")

	// Owner dies; a second 502 lands inside the cooldown (clock unchanged) →
	// trailing-edge probe fires after the injected interval → reap. No third req.
	alive.Store(false)
	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/two", "", nil).Code)

	require.True(t, waitFor(t, 2*time.Second, func() bool {
		_, ok := s.registry.Lookup("api.local.dev", 80)
		return !ok
	}), "the cooldown-suppressed 502 must still converge via the trailing probe")
}

// TestDeadRouteProbe_StormSingleFlight pins the concurrency bound: a 502 storm
// against a dead owner, with the removal callback blocked, produces exactly ONE
// in-flight probe chain, and the data plane keeps returning 502 the whole time
// (the response path never blocks on probing).
func TestDeadRouteProbe_StormSingleFlight(t *testing.T) {
	deadPid, deadToken := deadIdentity(t)
	s, dp := newProbeServer(t)

	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	var calls atomic.Int32
	dp.SetDeadRouteRemover(func(dir string, pid int, startTime int64) {
		calls.Add(1)
		entered <- struct{}{}
		<-release // hold the single probe chain open
		s.removeStaleProject(dir, pid, startTime)
	})

	registerProbeRoute(t, s, "/projects/storm", deadPid, deadToken, "127.0.0.1", 1)

	// Burst of concurrent requests; every one must complete with 502 promptly.
	const burst = 24
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := serve(dp, "GET", "http://api.local.dev/storm", "", nil)
			assert.Equal(t, http.StatusBadGateway, rec.Code, "response path must not block on probing")
		}()
	}
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("data plane blocked: 502 responses did not complete while the probe callback was held")
	}

	// Exactly one probe chain entered the (blocked) remover.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("expected one probe chain to reach the removal callback")
	}
	_, inFlight, _ := probeSnapshot(dp, "/projects/storm")
	assert.True(t, inFlight, "the single probe chain must be in flight while the callback is held")
	assert.Equal(t, int32(1), calls.Load(), "storm must yield exactly one in-flight probe chain")

	close(release)
	require.True(t, waitFor(t, 2*time.Second, func() bool {
		_, ok := s.registry.Lookup("api.local.dev", 80)
		return !ok
	}), "route must be reaped once the callback is released")
	assert.Equal(t, int32(1), calls.Load(), "no second probe chain after the single reap")
}

// TestDeadRouteProbe_UnregisteredDirNoOp pins that a probe for a dir that
// deregistered between the 502 and the probe is a harmless no-op: the guarded
// removal finds nothing and nothing else is disturbed.
func TestDeadRouteProbe_UnregisteredDirNoOp(t *testing.T) {
	deadPid, deadToken := deadIdentity(t)
	s, dp := newProbeServer(t)

	// A live neighbor that must be untouched.
	registerProbeRoute(t, s, "/projects/live", 1, 0, "127.0.0.1", 2)

	// Probe a dir that was never registered (route removed before the probe).
	dp.triggerDeadRouteProbe("/projects/gone", deadPid, deadToken)

	require.True(t, waitFor(t, time.Second, func() bool {
		exists, _, _ := probeSnapshot(dp, "/projects/gone")
		return !exists // dead + guarded no-op removal prunes the transient state
	}), "probe for an unregistered dir must settle and prune its state")

	_, ok := s.registry.Lookup("api.local.dev", 80)
	assert.True(t, ok, "an unrelated live route must be untouched")
	select {
	case <-s.ShutdownCh():
		t.Fatal("a no-op probe must not schedule shutdown")
	default:
	}
}

// TestDeadRouteProbe_StaleGenerationGuarded pins the DeregisterIfIdentity guard:
// a probe carrying a DEAD OLD identity, fired while a NEW LIVE generation for the
// same dir is registered, must not remove the new generation.
func TestDeadRouteProbe_StaleGenerationGuarded(t *testing.T) {
	selfToken := mustSelfToken(t)
	oldPid, oldToken := deadIdentity(t)
	s, dp := newProbeServer(t)

	// New live generation registered for the dir.
	registerProbeRoute(t, s, "/projects/gen", os.Getpid(), selfToken, "127.0.0.1", 1)

	// A 502 from the old generation reaches the probe after re-registration: the
	// frozen identity is the OLD dead one.
	dp.triggerDeadRouteProbe("/projects/gen", oldPid, oldToken)

	require.True(t, waitFor(t, time.Second, func() bool {
		exists, _, _ := probeSnapshot(dp, "/projects/gen")
		return !exists // probe found old dead, guarded removal no-op, state pruned
	}), "stale-generation probe must settle")

	route, ok := s.registry.Lookup("api.local.dev", 80)
	require.True(t, ok, "the new live generation must survive a stale-generation probe")
	assert.Equal(t, os.Getpid(), route.PID, "surviving route must be the new generation")
}

// TestDeadRouteProbe_ReapPathPreservesPending pins the reap-path trailing edge:
// while a probe chain for generation A is inside the (blocked) remover with no
// locks held, generation B re-registers for the same dir and then a 502 for B
// arrives (setting pending on the still-in-flight chain). When the chain returns
// from reaping A, it must honor B's pending and probe B on the trailing edge —
// NOT prune the state and drop B to the 30s sweep. B is dead and receives no
// further traffic, so only the trailing probe can converge it.
func TestDeadRouteProbe_ReapPathPreservesPending(t *testing.T) {
	aPid, aToken := deadIdentity(t)
	bPid, bToken := deadIdentity(t)
	s, dp := newProbeServer(t)

	// Gated remover: block ONLY the first call (generation A) so B can re-register
	// and fire its 502 while the chain is parked in the remover with locks free.
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	dp.SetDeadRouteRemover(func(dir string, pid int, startTime int64) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		s.removeStaleProject(dir, pid, startTime)
	})

	// Generation A registered; its 502 starts the probe chain, which finds A dead
	// and parks in the blocked remover.
	registerProbeRoute(t, s, "/projects/gen", aPid, aToken, "127.0.0.1", 1)
	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/a", "", nil).Code)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("generation A's probe never reached the removal callback")
	}

	// Generation B re-registers for the same dir while A's reap is parked; its 502
	// sets pending on the in-flight chain (no new chain — single in-flight).
	s.registry.Deregister("/projects/gen")
	registerProbeRoute(t, s, "/projects/gen", bPid, bToken, "127.0.0.1", 1)
	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/b", "", nil).Code)
	_, inFlight, pending := probeSnapshot(dp, "/projects/gen")
	require.True(t, inFlight, "A's chain must still own the dir")
	require.True(t, pending, "B's 502 must be pinned as a trailing probe, not spawn a second chain")

	// Release A's reap (a guarded no-op — B holds the dir). The chain must then
	// probe B on the trailing edge and reap it, with NO further traffic.
	close(release)
	require.True(t, waitFor(t, 2*time.Second, func() bool {
		_, ok := s.registry.Lookup("api.local.dev", 80)
		return !ok
	}), "the trailing probe must reap generation B without any further request")
	assert.Equal(t, int32(2), calls.Load(), "exactly two guarded removals: the A no-op then the B reap")
}

// TestDeadRouteProbe_LastProjectSchedulesShutdown pins the panel-HIGH fix: a
// probe that reaps the LAST project schedules the empty-daemon shutdown (the
// sweep epilogue mirrored into the probe callback), so an idle daemon is never
// stranded.
func TestDeadRouteProbe_LastProjectSchedulesShutdown(t *testing.T) {
	deadPid, deadToken := deadIdentity(t)
	s, dp := newProbeServer(t)
	s.shutdownDelay = 20 * time.Millisecond

	registerProbeRoute(t, s, "/projects/only", deadPid, deadToken, "127.0.0.1", 1)

	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/x", "", nil).Code)

	select {
	case <-s.ShutdownCh():
		// correct: reaping the last project scheduled the graced shutdown
	case <-time.After(2 * time.Second):
		t.Fatal("probe-reaping the last project must schedule the empty-daemon shutdown")
	}
}

// TestDeadRouteProbe_ReRegisterCancelsShutdown pins the epoch machinery: a live
// project re-registering after a probe emptied the registry cancels the graced
// shutdown.
func TestDeadRouteProbe_ReRegisterCancelsShutdown(t *testing.T) {
	deadPid, deadToken := deadIdentity(t)
	s, dp := newProbeServer(t)
	s.shutdownDelay = 300 * time.Millisecond

	registerProbeRoute(t, s, "/projects/only", deadPid, deadToken, "127.0.0.1", 1)
	require.Equal(t, http.StatusBadGateway, serve(dp, "GET", "http://api.local.dev/x", "", nil).Code)

	// Wait for the reap to empty the registry, then re-register a live project
	// well within the grace window THROUGH the production register path (so it
	// bumps lifecycleEpoch, exactly what scheduleShutdownWhenEmpty's epoch capture
	// keys on), refilling the registry — the scheduled shutdown must stand down.
	require.True(t, waitFor(t, 2*time.Second, func() bool { return s.registry.IsEmpty() }),
		"probe must empty the registry")
	registerOK(t, s, RegisterRequest{
		ProjectDir: "/projects/new", PID: 1, Version: "test", Domain: "other.dev",
		Services: map[string]ServiceTarget{"api": {Host: "127.0.0.1", Port: 1}},
		HTTPPort: freePort(t),
	})

	select {
	case <-s.ShutdownCh():
		t.Fatal("a racing re-register must cancel the scheduled shutdown")
	case <-time.After(500 * time.Millisecond):
		// correct: no shutdown fired
	}
}

// TestDeadRouteProbe_Concurrency_RegisterVsProbe races a live self-heal
// re-register against the probe's removal on the same dead dir under -race
// (style of TestSelfHeal_Concurrency_RegisterVsSweep). The lifecycle mutex and
// the probe map must make every outcome consistent — a routable live route or a
// clean reap — with no data races and no torn state.
func TestDeadRouteProbe_Concurrency_RegisterVsProbe(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	s := newProxyServer(t)
	dp := s.proxy
	// Force the probe to always find the frozen (dead) identity dead and to reap
	// through the identity-guarded path, exactly like the sweep.
	dp.probeIsAlive = func(int, int64) bool { return false }
	dp.SetDeadRouteRemover(func(dir string, pid int, startTime int64) {
		s.removeStaleProject(dir, pid, startTime)
	})

	port := freePort(t)
	dead := deadPID(t)
	baseReq := RegisterRequest{
		ProjectDir: "/projects/dead", Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}

	const iterations = 30
	for i := 0; i < iterations; i++ {
		s.removeProject("/projects/dead")
		deadReq := baseReq
		deadReq.PID = dead
		registerOK(t, s, deadReq)

		var wg sync.WaitGroup
		var regStatus int
		wg.Add(2)
		go func() {
			defer wg.Done()
			reReq := baseReq
			reReq.PID = os.Getpid()
			regStatus, _ = s.register(reReq)
		}()
		go func() {
			defer wg.Done()
			// Frozen dead identity (bare-PID token 0), matching the sweep guard:
			// the live restart (os.Getpid) reads as a different identity and is
			// left alone.
			dp.triggerDeadRouteProbe("/projects/dead", dead, 0)
		}()
		wg.Wait()

		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, regStatus,
			"iteration %d: only self-heal success or clean conflict (got %d)", i, regStatus)

		if regStatus == http.StatusOK {
			// A late probe for the dead identity must never remove the live route.
			require.True(t, waitFor(t, time.Second, func() bool {
				_, ok := s.registry.Lookup("api.local.dev", port)
				return ok
			}), "iteration %d: live self-heal route must survive the racing probe", i)
		}
	}
}
