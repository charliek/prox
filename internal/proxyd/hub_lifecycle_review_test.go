package proxyd

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lifecycle defects the second Codex review found (plan 031, reviews
// B1, B2, B3, B7, B9). Each of them is a window between two operations that
// looked atomic and was not, so each test drives the two operations into that
// window rather than asserting the end state of a quiet system.

// TestHubDeregister_DoesNotDeleteANewerGeneration is review B1.
//
// deregisterProject removed BY KEY and then closed the key's tunnel blind. An
// old `prox up` shutting down after its successor had already re-registered
// therefore deleted the successor's registration — and if the successor had
// attached in between, killed its session too. The PID was carried on the wire
// the whole time and only ever logged.
func TestHubDeregister_DoesNotDeleteANewerGeneration(t *testing.T) {
	h := newTunnelHub(t)
	key := HubProjectKey("shed", "/home/dev/app")
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}

	// The first generation registers under PID 4242 and goes away.
	first := registerBody("shed", "/home/dev/app", services, false)
	first.PID = 4242
	_, err := h.client(t).Register(first)
	require.NoError(t, err)

	// The SECOND generation re-registers the same key under a new PID, and its
	// tunnel attaches.
	second := registerBody("shed", "/home/dev/app", services, false)
	second.PID = 5353
	_, err = h.client(t).Register(second)
	require.NoError(t, err)
	session, _ := attachStubTunnel(t, h, key)
	require.NotNil(t, h.server.tunnels.get(key))

	// Now the FIRST generation's shutdown finally lands.
	require.NoError(t, h.client(t).Deregister(DeregisterRequest{
		ProjectDir: "/home/dev/app",
		PID:        4242,
		Origin:     "shed",
	}))

	assert.True(t, h.registry.HasProject(key),
		"a deregister from a process that no longer owns the registration must not delete its successor")
	assert.Same(t, session, h.server.tunnels.get(key),
		"nor close the successor's tunnel")

	// And the CURRENT owner's own deregister still works, closing its session.
	require.NoError(t, h.client(t).Deregister(DeregisterRequest{
		ProjectDir: "/home/dev/app",
		PID:        5353,
		Origin:     "shed",
	}))
	assert.False(t, h.registry.HasProject(key))
	assert.Nil(t, h.server.tunnels.get(key), "the owner's deregister closes the tunnel it owned")
}

// TestHubDeregister_ClosesOnlyTheGenerationItRemoved is B1's other arm: the
// close is by GENERATION, so a session installed between the removal and the
// close — the publisher reconnecting on a new registration — survives.
func TestHubDeregister_ClosesOnlyTheGenerationItRemoved(t *testing.T) {
	h := newTunnelHub(t)
	key := HubProjectKey("shed", "/home/dev/app")
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}

	h.mustRegister(t, "shed", "/home/dev/app", services)
	oldSession, _ := attachStubTunnel(t, h, key)

	// The registration goes; the publisher immediately comes back and attaches
	// a NEW session under the same key.
	res := h.server.removeProjectOwned(key, os.Getpid())
	require.True(t, res.Removed)
	require.Equal(t, oldSession.gen, res.SessionGen)

	h.mustRegister(t, "shed", "/home/dev/app", services)
	newSession, _ := attachStubTunnel(t, h, key)
	require.NotEqual(t, oldSession.gen, newSession.gen)

	// The late close names the generation it removed, so it is a no-op.
	assert.False(t, h.server.closeTunnelGen(key, res.SessionGen),
		"closing a generation that has been replaced must do nothing")
	assert.Same(t, newSession, h.server.tunnels.get(key),
		"the publisher's new session must survive the old registration's teardown")
}

// TestReconcileTunnelLease_ComparesGenerations is review B3.
//
// reconcileTunnelLease accepted ANY installed session as proof that a restored
// registration was connected. A takeover that failed and rolled back could
// therefore leave the registration claiming generation N while the session
// manager held N+1 — connected forever to a session nobody would ever close it
// against, unserveable and unsweepable.
func TestReconcileTunnelLease_ComparesGenerations(t *testing.T) {
	h := newTunnelHub(t)
	key := HubProjectKey("shed", "/home/dev/app")
	services := map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}
	h.mustRegister(t, "shed", "/home/dev/app", services)

	// Generation 1 attaches and is then removed from the manager WITHOUT the
	// registration being told — the shape a rollback leaves behind.
	first, _ := attachStubTunnel(t, h, key)
	require.True(t, h.registry.MarkConnected(key, first.gen))

	// A newer session is installed for the same key.
	second, _ := attachStubTunnel(t, h, key)
	require.NotEqual(t, first.gen, second.gen)

	// Force the registration back onto the OLD generation, as restoring a
	// snapshot taken before the second attach would.
	h.registry.mu.Lock()
	h.registry.projects[key].SessionGen = first.gen
	h.registry.projects[key].DisconnectedAt = time.Time{}
	h.registry.mu.Unlock()

	h.server.reconcileTunnelLease(key)

	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.Equal(t, second.gen, lease.SessionGen,
		"the registration must be re-synced onto the session that is actually installed")
	assert.True(t, lease.DisconnectedAt.IsZero(), "and it is genuinely connected, through that session")

	// Which means its close still reaches it: a generation mismatch would make
	// this a silent no-op and pin the registration forever.
	require.True(t, h.server.tunnels.detach(key, second.gen))
	assert.True(t, h.registry.MarkDisconnected(key, second.gen, h.clock.now()))
}

// TestReconcileTunnelLease_MarksDisconnectedWhenNothingIsInstalled is the other
// half of B3: with no session at all, a lease that claims one is stale and must
// start its disconnect grace, or the registration can never be swept.
func TestReconcileTunnelLease_MarksDisconnectedWhenNothingIsInstalled(t *testing.T) {
	h := newTunnelHub(t)
	key := HubProjectKey("shed", "/home/dev/app")
	h.mustRegister(t, "shed", "/home/dev/app", map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}})

	session, _ := attachStubTunnel(t, h, key)
	require.True(t, h.registry.MarkConnected(key, session.gen))
	// The session leaves the manager without marking anything, exactly as one
	// that closed while its registration was briefly out of the registry did.
	require.NotNil(t, h.server.tunnels.remove(key))

	h.server.reconcileTunnelLease(key)

	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.False(t, lease.DisconnectedAt.IsZero(),
		"a lease whose session is gone must read as disconnected, not as permanently connected")

	h.clock.advance(constants.HubDisconnectGrace + time.Second)
	assert.Len(t, h.registry.ExpiredLeases(), 1, "and it must therefore be sweepable")
}

// TestHubRegister_ReservationStartsWhenTheAnswerIsSent is review B7.
//
// The attach reservation used to be renewed BEFORE certificate generation and
// the listener bind. Those can take seconds on a cold certs directory, so a
// publisher could be handed a 200 for a registration whose 10s grace had
// already run out — eligible for sweeping, or for a newcomer to take, before it
// had any chance to open its tunnel. Here the cert phase consumes more than the
// whole grace.
func TestHubRegister_ReservationStartsWhenTheAnswerIsSent(t *testing.T) {
	h := newTunnelHub(t)
	h.certs.setEnsureHook(func(string) {
		h.clock.advance(constants.HubAttachGrace + time.Second)
	})

	h.mustRegister(t, "shed", "/home/dev/app", map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}})

	key := HubProjectKey("shed", "/home/dev/app")
	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.True(t, h.clock.now().Before(lease.ReservedUntil),
		"the publisher must get its whole attach grace AFTER the 200, not from before a slow cert phase")
	assert.Empty(t, h.registry.ExpiredLeases(),
		"a registration that has just been confirmed must not already be sweepable")
}

// TestStartHub_SeesARegistrationThatIsStillCommitting is review B2.
//
// Config selection, the publisher-count check and the registration commit were
// three different lock scopes. A `prox hub start` could see zero publishers
// while a register that had already read the OLD domain went on to commit routes
// under it — leaving `prox hub status` describing a config the routes do not
// use, which is the exact state the reconfigure guard exists to prevent.
//
// The register is held inside its transaction (in the cert phase, which is
// where a real one spends its time) while the reconfigure runs.
func TestStartHub_SeesARegistrationThatIsStillCommitting(t *testing.T) {
	h := newTunnelHub(t)

	inCertPhase := make(chan struct{})
	releaseCert := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCert) }) }
	// However this test ends — including on a failed assertion, which unwinds
	// the goroutine it runs on — the held registration must be let go, or the
	// register keeps lifecycleMu and everything after it deadlocks.
	t.Cleanup(release)

	var enterOnce sync.Once
	h.certs.setEnsureHook(func(string) {
		enterOnce.Do(func() { close(inCertPhase) })
		<-releaseCert
	})

	registered := make(chan error, 1)
	go func() {
		_, err := h.client(t).Register(registerBody("shed", "/home/dev/app",
			map[string]ServiceTarget{"web": {Host: "127.0.0.1", Port: 1}}, false))
		registered <- err
	}()

	select {
	case <-inCertPhase:
	case <-time.After(10 * time.Second):
		t.Fatal("the register never reached its cert phase")
	}

	// The reconfigure runs while that registration is mid-commit. It must block
	// on the same lock and then SEE the publisher, rather than deciding against
	// a registry the register had not reached yet.
	reconfigured := make(chan error, 1)
	go func() {
		cfg := h.server.hubConfigSnapshot().cfg
		cfg.Domain = "moved.test"
		reconfigured <- h.server.StartHub(cfg)
	}()

	// It must NOT have finished while the register is still holding the lock.
	var early bool
	select {
	case err := <-reconfigured:
		early = true
		t.Errorf("the reconfigure committed while a registration was mid-flight (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	require.NoError(t, <-registered)
	if early {
		return // the rest asserts against a state this run never reached
	}

	err := <-reconfigured
	require.Error(t, err, "a domain change with a publisher registered must be refused")
	assert.Contains(t, err.Error(), "publisher(s) are registered")

	// And the routes and the reported config still agree.
	assert.Equal(t, h.domain, h.server.hubStatus().Domain)
	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.Equal(t, "web."+h.domain, routes[0].Hostname)
}

// TestHubTunnel_ValidatesTheProjectDirHeader is review B9: the tunnel upgrade
// was the one key-composition path that only checked its project-dir header for
// emptiness, while register, deregister and the capture endpoints all applied
// the shared rule.
func TestHubTunnel_ValidatesTheProjectDirHeader(t *testing.T) {
	h := newTunnelHub(t)

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"empty", ""},
		{"relative", "home/dev/app"},
		{"windows-shaped", `C:\work\app`},
		{"control character", "/home/dev/\x01app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/tunnel", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+h.token)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", tunnelUpgradeProtocol)
			req.Header.Set("X-Prox-Origin", "shed")
			if tc.dir != "" {
				// http.Header rejects a control character at write time for a
				// value it cannot frame, so set it through the map directly and
				// let the server decide.
				req.Header["X-Prox-Project-Dir"] = []string{tc.dir}
			}

			resp, err := hubClient().Do(req)
			if err != nil {
				// ONLY the control-character case may fail client-side:
				// http.Header refuses to frame it, and a header the client
				// will not send never reaches a registry key either. Every
				// other value here is one the client CAN send, so letting
				// them take this branch would let the case pass without ever
				// reaching the handler assertion below — the test would stop
				// testing the server (plan 031, CodeRabbit).
				require.Equal(t, "control character", tc.name,
					"%s is a value the client can send, so it must reach the handler", tc.name)
				return
			}
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestHubRegistration_HostLimitImpliesTunnelFraming is B6's second half, stated
// as the relationship it actually is.
//
// validateHubRegistration checks that a target fits the tunnel's 512-byte
// CONNECT framing, and with the DNS limits now enforced on the host that check
// can never fire: the longest host ValidateHost accepts, with the longest port,
// still frames comfortably. That is the POINT — the two bounds must stay
// consistent, and this test is what fails if someone relaxes the host rule and
// leaves the framing rule to discover the problem one dial at a time.
func TestHubRegistration_HostLimitImpliesTunnelFraming(t *testing.T) {
	label := strings.Repeat("a", 63)
	host := strings.Join([]string{label, label, label, strings.Repeat("b", 61)}, ".")
	require.Len(t, host, 253, "the longest host DNS allows")
	require.NoError(t, domain.ValidateHost(host))

	line := formatConnectLine(host, 65535)
	assert.LessOrEqual(t, len(line), tunnelPreambleMaxBytes,
		"every host the registration rules accept must fit the tunnel's CONNECT framing")

	// And the framing check does its job on a target that somehow got past the
	// host rule, which is the case it exists for.
	err := validateHubRegistration(RegisterRequest{
		ProjectDir: "/home/dev/app",
		PID:        1,
		Services:   map[string]ServiceTarget{"app": {Host: strings.Repeat("a", 600), Port: 3000}},
	})
	require.Error(t, err)
}
