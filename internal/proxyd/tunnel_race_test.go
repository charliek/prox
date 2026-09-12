package proxyd

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are the -race pins for the four corrected races the panel found (plan
// 031 P2, P3, P5, and the frozen-publisher/inflight-SYN case in §8). Each is
// deterministic: the ones about ordering drive the exact callback under test
// inline rather than hoping a goroutine loses a timing race, and the ones about
// concurrency assert an invariant that must hold for EVERY interleaving rather
// than a particular one.

// newStubSessionPair returns a hub-side and publisher-side yamux session over a
// real TCP pair, WITHOUT attaching either. Tests that churn sessions from a
// helper goroutine build them up front here, because require.* must only be
// called from the test's own goroutine.
func newStubSessionPair(t *testing.T) (hubSess, pubSess *yamux.Session) {
	t.Helper()
	pubConn, hubConn := tcpPipe(t)
	var err error
	hubSess, err = yamux.Server(hubConn, tunnelYamuxConfig())
	require.NoError(t, err)
	pubSess, err = yamux.Client(pubConn, tunnelYamuxConfig())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pubSess.Close()
		_ = hubSess.Close()
	})
	return hubSess, pubSess
}

// registerAndFreeze registers a project and attaches a tunnel whose publisher
// never accepts a stream — the "frozen publisher" of AC5. Its TCP connection
// stays ESTABLISHED and its kernel keeps ACKing, so nothing below the
// application layer notices anything is wrong; only the CONNECT reply deadline
// does.
func registerAndFreeze(t *testing.T, h *tunnelHub, origin, dir string, services map[string]ServiceTarget) string {
	t.Helper()
	h.mustRegister(t, origin, dir, services)
	key := HubProjectKey(origin, dir)
	// The publisher session is created (so the connection is live and yamux
	// frames flow) but AcceptStream is never called, so no CONNECT is ever
	// answered.
	attachStubTunnel(t, h, key)
	return key
}

// TestTunnelRace_FrozenPublisherServes503 is AC5's mechanism: a publisher that
// is alive at the TCP level but answers nothing must produce the offline 503
// within HubDialTimeout, because the per-dial reply deadline — not TCP, and not
// yamux's own session-death detection — is what fails the request (P10).
func TestTunnelRace_FrozenPublisherServes503(t *testing.T) {
	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	registerAndFreeze(t, h, "shed", "/home/dev/app", services)

	start := time.Now()
	status, body, headers := h.get(t, h.httpsClient(), "web", "/")
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "5", headers.Get("Retry-After"))
	assert.Contains(t, body, "Publisher offline")
	assert.Less(t, elapsed, constants.HubDialTimeout+2*time.Second,
		"a frozen publisher must fail on the dial deadline, not on session death")
}

// TestTunnelRace_FrozenPublisherConcurrentRequests is §8's accepted-risk pin and
// plan 031 F7's regression test: many simultaneous requests to a frozen
// publisher must ALL get a 503 rather than blocking on yamux's inflight-SYN
// budget. The dial timeout is shortened so the test is fast; the code path is
// identical.
//
// The request count is deliberately ABOVE yamux's AcceptBacklog (256). The
// earlier 64 could not reach the limit and so could not see the bug at all:
// OpenStream blocks inside yamux once 256 SYNs are unACKed, with no context and
// no deadline, so the 257th caller waited StreamOpenTimeout (30s) instead of
// HubDialTimeout. Anything at or under the backlog passes either way.
func TestTunnelRace_FrozenPublisherConcurrentRequests(t *testing.T) {
	h := newTunnelHub(t)
	// Shorten before anything attaches: a session copies the bound at attach.
	h.server.tunnels.setDialTimeout(150 * time.Millisecond)

	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	registerAndFreeze(t, h, "shed", "/home/dev/app", services)

	// yamux's AcceptBacklog is 256; this must exceed it, or the acquisition
	// path the fix is about is never entered (plan 031 F7).
	const requests = 300
	client := h.httpsClient()
	statuses := make([]int, requests)

	var wg sync.WaitGroup
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get(h.url("web", "/"))
			if err != nil {
				statuses[i] = -1
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			statuses[i] = resp.StatusCode
		}(i)
	}

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent requests to a frozen publisher blocked instead of failing")
	}

	for i, status := range statuses {
		assert.Equal(t, http.StatusServiceUnavailable, status, "request %d", i)
	}
}

// TestTunnelRace_ReattachBeforeRemovalSurvives is P2's exact shape: the sweep
// observes an expired lease, the publisher reattaches before the removal runs,
// and the removal must decline.
//
// It is driven step by step rather than raced, because the bug was never about
// which goroutine wins — it was that the LOSING interleaving deleted a live
// registration.
func TestTunnelRace_ReattachBeforeRemovalSurvives(t *testing.T) {
	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	key := HubProjectKey("shed", "/home/dev/app")
	h.mustRegister(t, "shed", "/home/dev/app", services)

	first, _ := attachStubTunnel(t, h, key)
	// Tunnel closes; the registration goes into its disconnect grace.
	require.True(t, h.registry.MarkDisconnected(key, first.gen, h.clock.now()))
	require.True(t, h.server.tunnels.detach(key, first.gen))

	h.clock.advance(constants.HubDisconnectGrace + time.Second)
	candidates := h.registry.ExpiredLeases()
	require.Len(t, candidates, 1)
	require.Equal(t, first.gen, candidates[0].SessionGen)

	// The publisher comes back BETWEEN detection and removal.
	second, _ := attachStubTunnel(t, h, key)
	require.Greater(t, second.gen, first.gen)

	removed, _, _ := h.server.removeDisconnectedRemote(candidates[0].Key, candidates[0].SessionGen)
	assert.False(t, removed, "a reattached publisher must not be removed by a stale sweep decision")
	assert.True(t, h.registry.HasProject(key))
	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.True(t, lease.DisconnectedAt.IsZero(), "the reattached registration is connected again")
	assert.Equal(t, second.gen, lease.SessionGen)
}

// TestTunnelRace_ReplacedSessionLateCallback is the other half of P2: session B
// replaces A, and A's close callback then arrives LATE. It must be ignored,
// because by then a newer generation is installed.
//
// The late callback is invoked inline, which is what makes this deterministic —
// waiting for A's watcher goroutine to happen to run late would be exactly the
// kind of timing bet the plan forbids. The production path spawns the same
// function, so running it by hand exercises the real gate.
func TestTunnelRace_ReplacedSessionLateCallback(t *testing.T) {
	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	key := HubProjectKey("shed", "/home/dev/app")
	h.mustRegister(t, "shed", "/home/dev/app", services)

	first, _ := attachStubTunnel(t, h, key)
	second, _ := attachStubTunnel(t, h, key)
	require.Greater(t, second.gen, first.gen)

	// attachTunnel already closed the replaced session, so watchTunnel's wait on
	// CloseChan returns at once: this IS the late callback.
	h.server.watchTunnel(first)

	assert.Same(t, second, h.server.tunnels.get(key), "the successor must still be installed")
	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.True(t, lease.DisconnectedAt.IsZero(),
		"a replaced session's late close must not disconnect its successor")
	assert.Equal(t, second.gen, lease.SessionGen)

	// And the successor's OWN close still works.
	require.True(t, h.server.tunnels.detach(key, second.gen))
	require.True(t, h.registry.MarkDisconnected(key, second.gen, h.clock.now()))
	lease, _ = h.registry.RemoteLease(key)
	assert.False(t, lease.DisconnectedAt.IsZero())
}

// TestTunnelRace_RegisterInsideAttachGraceRefusesNewcomer is P3: between
// register and attach nothing is connected, so without the attach reservation
// D10's "inactive is replaceable" rule would hand the name to a second
// publisher while the first is still completing its handshake.
//
// Note which newcomer is refused: a PLAIN register. A register carrying
// takeover:true is meant to displace an active holder (D10), and does — the
// reservation is what makes the difference between "taken silently" and
// "refused unless you ask for it".
func TestTunnelRace_RegisterInsideAttachGraceRefusesNewcomer(t *testing.T) {
	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	h.mustRegister(t, "alpha", "/home/dev/app", services)

	// Inside the reservation, with no tunnel attached at all.
	_, err := h.client(t).Register(registerBody("beta", "/other/app", services, false))
	require.Error(t, err, "a publisher mid-handshake must not lose its name")
	apiErr := requireAPIError(t, err)
	assert.Equal(t, "HUB_NAME_HELD", apiErr.Code)

	holders := decodeHolders(t, h, registerBody("beta", "/other/app", services, false))
	require.Len(t, holders, 1)
	assert.Equal(t, "alpha", holders[0].Origin)
	assert.False(t, holders[0].Connected, "reserved is not connected; it still holds the name")

	// An explicit takeover still wins, exactly as D10 says.
	_, err = h.client(t).Register(registerBody("gamma", "/third/app", services, true))
	require.NoError(t, err, "takeover displaces a reserved holder")
	assert.False(t, h.registry.HasProject(HubProjectKey("alpha", "/home/dev/app")))

	// Once gamma's own reservation lapses without a tunnel, the name is free.
	h.clock.advance(constants.HubAttachGrace + time.Second)
	_, err = h.client(t).Register(registerBody("beta", "/other/app", services, false))
	require.NoError(t, err, "an expired reservation no longer holds the name")
	assert.False(t, h.registry.HasProject(HubProjectKey("gamma", "/third/app")))
}

// TestTunnelRace_TakeoverDuringDisconnectAndReattach runs a takeover against a
// key whose tunnel is being closed and reattached concurrently (CodeRabbit M3).
// There is no single "right" interleaving to assert; what must hold for EVERY
// interleaving is that the registry ends consistent — the winner owns the route,
// the loser owns nothing — and that the run trips neither the race detector nor
// a lock-order deadlock.
func TestTunnelRace_TakeoverDuringDisconnectAndReattach(t *testing.T) {
	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	alphaKey := HubProjectKey("alpha", "/home/dev/app")
	betaKey := HubProjectKey("beta", "/other/app")
	h.mustRegister(t, "alpha", "/home/dev/app", services)

	const churn = 20
	// Built up front: require.* must not run on the churn goroutine.
	sessions := make([]*yamux.Session, churn)
	for i := range sessions {
		sessions[i], _ = newStubSessionPair(t)
	}
	client := h.client(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, sess := range sessions {
			h.server.attachTunnel(alphaKey, sess)
			_ = sess.Close()
		}
	}()

	wg.Add(1)
	takeoverErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, err := client.Register(registerBody("beta", "/other/app", services, true))
		takeoverErr <- err
	}()
	wg.Wait()

	require.NoError(t, <-takeoverErr, "takeover must succeed regardless of tunnel churn")

	// Beta owns the name; alpha owns nothing.
	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.Equal(t, betaKey, routes[0].ProjectDir)
	assert.False(t, h.registry.HasProject(alphaKey))

	// Any session the churn goroutine installed after the takeover belongs to a
	// registration that no longer exists; closing the hub must still drop it.
	h.server.closeAllTunnels()
	assert.Nil(t, h.server.tunnels.get(alphaKey))
}

// TestTunnelRace_HubStopRacesSessionCloseAndSweep runs `hub stop` against a
// concurrent session close and a concurrent sweep tick — the three paths that
// touch lifecycleMu, the registry lock, and the session mutex. D17's leaf rule
// is what makes this safe; a violation shows up here as a deadlock or a race
// report rather than as a wrong answer.
func TestTunnelRace_HubStopRacesSessionCloseAndSweep(t *testing.T) {
	h := newTunnelHub(t)

	keys := []string{}
	sessions := []*yamux.Session{}
	for _, origin := range []string{"a", "b", "c", "d"} {
		dir := "/home/dev/" + origin
		// Distinct service names: this test is about lifecycle locking, not
		// about D10, so nobody may collide with anybody.
		h.mustRegister(t, origin, dir, map[string]ServiceTarget{"web-" + origin: {Host: "127.0.0.1", Port: 1}})
		key := HubProjectKey(origin, dir)
		hubSess, _ := newStubSessionPair(t)
		h.server.attachTunnel(key, hubSess)
		keys = append(keys, key)
		sessions = append(sessions, hubSess)
	}
	// Every registration must actually BE disconnected before the clock moves,
	// or the sweep has no candidates and this test proves nothing about racing
	// it — attachTunnel marks a registration CONNECTED, and a connected
	// registration is never a lease candidate however far the clock advances.
	for i, key := range keys {
		gen := h.server.tunnels.get(key).gen
		require.True(t, h.registry.MarkDisconnected(key, gen, h.clock.now()),
			"registration %s must be marked disconnected for the sweep to have work", key)
		_ = i
	}
	h.clock.advance(constants.HubDisconnectGrace + constants.HubAttachGrace + time.Second)
	require.Len(t, h.registry.ExpiredLeases(), len(keys),
		"every registration must be a sweep candidate before the race starts")

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		h.server.StopHub()
	}()
	go func() {
		defer wg.Done()
		for _, sess := range sessions {
			_ = sess.Close()
		}
	}()
	go func() {
		defer wg.Done()
		// The daemon's 30s tick body, run as fast as it can go.
		for i := 0; i < 50; i++ {
			for _, cand := range h.registry.ExpiredLeases() {
				h.server.removeDisconnectedRemote(cand.Key, cand.SessionGen)
			}
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("hub stop, a session close, and a sweep tick deadlocked against each other")
	}

	assert.False(t, h.server.hubEnabled(), "hub mode must be off after stop")
	for _, key := range keys {
		assert.False(t, h.registry.HasProject(key), "remote registration %s must be gone", key)
		assert.Nil(t, h.server.tunnels.get(key), "tunnel %s must be closed", key)
	}
	assert.Empty(t, h.registry.AllRoutes())
}

// TestTunnelRace_AttachDuringReplaceIsNotLost is plan 031 F3, driven with a
// barrier rather than raced.
//
// The bug: a remote re-register with a CHANGED config snapshotted the lease,
// removed the registration, re-added it, and then wrote the snapshot back. A
// tunnel attaching inside that sequence marked the NEW registration connected
// under a fresh generation, and the stale write then clobbered it — leaving a
// registration that claimed a tunnel the session manager had never associated
// with it. Nothing could ever fix it up: MarkDisconnected no-ops on a
// generation mismatch, so the registration stayed "connected" with no session,
// unserveable and unsweepable, until `prox hub stop`.
//
// The barrier sits exactly where that window was — inside the registry lock,
// immediately before the lease carry. The attach must land on one side of the
// replacement or the other, never inside it, and afterwards the registration's
// generation must be the one the session manager actually holds.
func TestTunnelRace_AttachDuringReplaceIsNotLost(t *testing.T) {
	h := newTunnelHub(t)
	key := HubProjectKey("shed", "/home/dev/app")
	h.mustRegister(t, "shed", "/home/dev/app", map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}})

	// The session the "concurrent" attach will install, built up front because
	// require.* must run on the test's own goroutine.
	hubSess, _ := newStubSessionPair(t)

	attaching := make(chan struct{})
	attached := make(chan *tunnelSession, 1)
	h.registry.beforeReplaceCommit = func() {
		// Runs with the registry lock held, immediately before the lease carry.
		// The attach below therefore BLOCKS here, which is the point: under the
		// old code it would have completed and then been overwritten.
		close(attaching)
		time.Sleep(20 * time.Millisecond)
	}

	go func() {
		<-attaching
		attached <- h.server.attachTunnel(key, hubSess)
	}()

	// A re-register with a CHANGED service target: the replace arm.
	_, err := h.client(t).Register(registerBody("shed", "/home/dev/app",
		map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 2}}, false))
	require.NoError(t, err)

	var session *tunnelSession
	select {
	case session = <-attached:
	case <-time.After(10 * time.Second):
		t.Fatal("the concurrent attach never completed")
	}

	// The invariant: whatever the interleaving, the registration's generation is
	// the one the session manager holds, and a live session is not reported as
	// disconnected.
	require.Eventually(t, func() bool {
		lease, ok := h.registry.RemoteLease(key)
		return ok && lease.SessionGen == session.gen
	}, 5*time.Second, 2*time.Millisecond,
		"the registration must end up on the generation the session manager installed")

	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.True(t, lease.DisconnectedAt.IsZero(), "a live tunnel must not read as disconnected")
	assert.Same(t, session, h.server.tunnels.get(key))

	// And the registration still reflects the CHANGED config.
	route, ok := h.registry.Lookup("web."+h.domain, h.httpsPort)
	require.True(t, ok)
	assert.Equal(t, 2, route.Target.Port)

	// The close callback still works, which is the proof that the generation on
	// the registration and the one in the session manager really do match: a
	// mismatch would make this a silent no-op.
	require.True(t, h.server.tunnels.detach(key, session.gen))
	require.True(t, h.registry.MarkDisconnected(key, session.gen, h.clock.now()))
}

// TestTunnelRace_ReplaceCarriesTheLeaseInEitherOrder is the deterministic half
// of F3: with the replacement atomic, an attach either precedes it (its state
// is carried) or follows it (it applies to the successor), and a disconnect
// mid-flight is carried too rather than being reset to "connected".
func TestTunnelRace_ReplaceCarriesTheLeaseInEitherOrder(t *testing.T) {
	changed := func(port int) map[string]ServiceTarget {
		return map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: port}}
	}

	t.Run("attach before the replace is carried forward", func(t *testing.T) {
		h := newTunnelHub(t)
		key := HubProjectKey("shed", "/home/dev/app")
		h.mustRegister(t, "shed", "/home/dev/app", changed(1))
		session, _ := attachStubTunnel(t, h, key)

		_, err := h.client(t).Register(registerBody("shed", "/home/dev/app", changed(2), false))
		require.NoError(t, err)

		lease, ok := h.registry.RemoteLease(key)
		require.True(t, ok)
		assert.Equal(t, session.gen, lease.SessionGen, "the live generation survives a config change")
		assert.True(t, lease.DisconnectedAt.IsZero())
	})

	t.Run("attach after the replace applies to the successor", func(t *testing.T) {
		h := newTunnelHub(t)
		key := HubProjectKey("shed", "/home/dev/app")
		h.mustRegister(t, "shed", "/home/dev/app", changed(1))

		_, err := h.client(t).Register(registerBody("shed", "/home/dev/app", changed(2), false))
		require.NoError(t, err)

		session, _ := attachStubTunnel(t, h, key)
		lease, ok := h.registry.RemoteLease(key)
		require.True(t, ok)
		assert.Equal(t, session.gen, lease.SessionGen)
	})

	t.Run("a disconnect before the replace is carried forward", func(t *testing.T) {
		h := newTunnelHub(t)
		key := HubProjectKey("shed", "/home/dev/app")
		h.mustRegister(t, "shed", "/home/dev/app", changed(1))
		session, _ := attachStubTunnel(t, h, key)
		require.True(t, h.server.tunnels.detach(key, session.gen))
		require.True(t, h.registry.MarkDisconnected(key, session.gen, h.clock.now()))

		_, err := h.client(t).Register(registerBody("shed", "/home/dev/app", changed(2), false))
		require.NoError(t, err)

		lease, ok := h.registry.RemoteLease(key)
		require.True(t, ok)
		assert.Equal(t, session.gen, lease.SessionGen)
		assert.False(t, lease.DisconnectedAt.IsZero(),
			"a re-register must not make a publisher with no tunnel look connected")
	})
}
