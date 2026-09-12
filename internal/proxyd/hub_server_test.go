package proxyd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubServer starts a Server with hub mode on a loopback ephemeral port and
// returns it with the network mount's base URL. Every hub test gets its own
// HOME: StartHub can create ~/.prox/hub.token, and no test may touch the
// developer's own daemon files.
func newHubServer(t *testing.T, cfg HubConfig) (*Server, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	s := newLifecycleServer()
	if cfg.Domain == "" {
		cfg.Domain = "llt.test"
	}
	if cfg.Listen == "" {
		// Ephemeral CONTROL-PLANE port. Data-plane ports are never 0 in tests:
		// there, 0 already means "no listener" (plan 031 P15).
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.HTTPSPort == 0 && cfg.HTTPPort == 0 {
		cfg.HTTPSPort = 16443
	}
	if cfg.Auth == "" {
		cfg.Auth = HubAuthToken
	}
	if cfg.Auth == HubAuthToken && cfg.Token == "" {
		cfg.Token = "test-hub-token"
	}
	require.NoError(t, s.StartHub(cfg))
	t.Cleanup(s.StopHub)
	return s, "http://" + s.HubListenAddr()
}

// hubClient is a plain HTTP client for the network mount, so the tests exercise
// the real wire rather than the handler functions.
func hubClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func hubDo(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hubClient().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// errorCode decodes an ErrorResponse body and returns its Code.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	return er.Code
}

// hubRegisterRequest builds a well-formed network register body for origin/dir.
func hubRegisterRequest(origin, dir string, services map[string]ServiceTarget) RegisterRequest {
	return RegisterRequest{
		ProjectDir:      dir,
		PID:             os.Getpid(),
		Origin:          origin,
		ProtocolVersion: constants.HubProtocolVersion,
		// Deliberately "wrong": the hub overwrites both (D4).
		Domain:    "local.publisher.test",
		HTTPSPort: 9999,
		Services:  services,
	}
}

func registerViaHub(t *testing.T, baseURL, token, origin, dir string, services map[string]ServiceTarget) {
	t.Helper()
	resp := hubDo(t, http.MethodPost, baseURL+"/api/v1/register", token, hubRegisterRequest(origin, dir, services))
	require.Equal(t, http.StatusOK, resp.StatusCode, "register via hub should succeed")
}

// TestHubMount_Auth pins the bearer-token middleware (plan 031 D6): every
// network route needs the token, /health never does, and `auth: none` is an
// explicit opt-out rather than a fallback anything can trigger.
func TestHubMount_Auth(t *testing.T) {
	t.Run("token modes", func(t *testing.T) {
		_, base := newHubServer(t, HubConfig{Token: "right-token"})

		tests := []struct {
			name       string
			header     string
			wantStatus int
		}{
			{"correct token", "Bearer right-token", http.StatusOK},
			{"correct token, lowercase scheme", "bearer right-token", http.StatusOK},
			{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
			{"empty token", "Bearer ", http.StatusUnauthorized},
			{"missing header", "", http.StatusUnauthorized},
			{"wrong scheme", "Basic cm9vdDpyb290", http.StatusUnauthorized},
			{"token as a prefix of the real one", "Bearer right", http.StatusUnauthorized},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				req, err := http.NewRequest(http.MethodGet, base+"/api/v1/routes", nil)
				require.NoError(t, err)
				if tt.header != "" {
					req.Header.Set("Authorization", tt.header)
				}
				resp, err := hubClient().Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				assert.Equal(t, tt.wantStatus, resp.StatusCode)
				if tt.wantStatus == http.StatusUnauthorized {
					assert.Equal(t, "UNAUTHORIZED", errorCode(t, resp))
				}
			})
		}
	})

	t.Run("health needs no token", func(t *testing.T) {
		_, base := newHubServer(t, HubConfig{Token: "right-token"})
		resp := hubDo(t, http.MethodGet, base+"/health", "", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("auth none accepts a token-less call", func(t *testing.T) {
		_, base := newHubServer(t, HubConfig{Auth: HubAuthNone})
		resp := hubDo(t, http.MethodGet, base+"/api/v1/routes", "", nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestHubMount_AllowList is the allow-list half of D6: the network mount is a
// SEPARATE router carrying only the routes a publisher may reach. The two
// absences that matter are asserted with a VALID token, so a 404 means "this
// route does not exist here" rather than "auth stopped you".
func TestHubMount_AllowList(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	t.Run("shutdown is not reachable over the network", func(t *testing.T) {
		resp := hubDo(t, http.MethodPost, base+"/api/v1/shutdown", "tok", nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.False(t, s.isShuttingDown(), "a network shutdown attempt must not stop the daemon")
	})

	t.Run("hub control is not reachable over the network", func(t *testing.T) {
		for _, path := range []string{"/api/v1/hub/start", "/api/v1/hub/stop", "/api/v1/hub/token"} {
			resp := hubDo(t, http.MethodPost, base+path, "tok", nil)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "POST %s", path)
		}
		resp := hubDo(t, http.MethodGet, base+"/api/v1/hub/status", "tok", nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.True(t, s.hubEnabled(), "hub mode must survive a publisher's attempt to stop it")
	})

	t.Run("the allowed routes are reachable", func(t *testing.T) {
		for _, path := range []string{"/api/v1/status", "/api/v1/routes"} {
			resp := hubDo(t, http.MethodGet, base+path, "tok", nil)
			assert.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
		}
	})

	t.Run("shutdown still works on the socket mount", func(t *testing.T) {
		// The same Server, reached through its SOCKET router: the allow-list
		// removes nothing from the local control plane.
		socket := newLifecycleServer()
		rec := httptest.NewRecorder()
		socket.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/shutdown", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, socket.isShuttingDown())
	})
}

// TestHubRegister_ProtocolMismatch pins D7: the network path checks the hub
// PROTOCOL version, never the binary version, because the two machines upgrade
// independently.
func TestHubRegister_ProtocolMismatch(t *testing.T) {
	_, base := newHubServer(t, HubConfig{Token: "tok"})

	req := hubRegisterRequest("popos", "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	req.ProtocolVersion = constants.HubProtocolVersion + 1

	resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", req)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "PROTOCOL_MISMATCH", errorCode(t, resp))
}

// TestHubRegister_ProtocolVersionIsMandatory: an omitted protocol_version
// decodes to 0 and must be refused, not treated as "current".
func TestHubRegister_ProtocolVersionIsMandatory(t *testing.T) {
	_, base := newHubServer(t, HubConfig{Token: "tok"})

	req := hubRegisterRequest("popos", "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	req.ProtocolVersion = 0

	resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", req)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "PROTOCOL_MISMATCH", errorCode(t, resp))
}

// TestHubRegister_RejectsBadOrigin: the origin is the caller's identity and the
// first half of every key the hub composes, so it must be present and free of
// the separators that would let a composed key take another key's shape (D15).
func TestHubRegister_RejectsBadOrigin(t *testing.T) {
	_, base := newHubServer(t, HubConfig{Token: "tok"})

	for _, origin := range []string{"", "   ", "mac:/home/dev", "/home/dev"} {
		req := hubRegisterRequest(origin, "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
		resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", req)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "origin %q", origin)
		assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))
	}
}

// TestHubRegister_HubOwnsDomainAndPorts pins D4: a remote registration's own
// domain and ports are OVERWRITTEN from hub.yaml, so hostnames are always
// <service>.<hub domain> on the ports the hub host chose to expose.
func TestHubRegister_HubOwnsDomainAndPorts(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok", Domain: "llt.test", HTTPSPort: 16443})

	registerViaHub(t, base, "tok", "popos", "/home/dev/app",
		map[string]ServiceTarget{"auth": {Host: "localhost", Port: 3000}})

	// The publisher asked for local.publisher.test:9999; the hub published
	// llt.test:16443.
	route, ok := s.registry.Lookup("auth.llt.test", 16443)
	require.True(t, ok, "hub domain and port must win")
	assert.Equal(t, "popos", route.Origin)
	assert.Equal(t, ServiceTarget{Host: "localhost", Port: 3000}, route.Target)

	_, wrong := s.registry.Lookup("auth.local.publisher.test", 9999)
	assert.False(t, wrong, "the publisher's own domain/port must not be registered")
}

// TestHubRegister_ComposesKeyServerSide is D15's core: the registry key is
// DERIVED from the caller's own origin. A publisher that sends an
// already-qualified dir does not get someone else's key — it gets that string
// composed under its OWN origin, which is harmless.
func TestHubRegister_ComposesKeyServerSide(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "popos", "/home/dev/app",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	assert.NotNil(t, s.registry.ProjectHostnames("popos:/home/dev/app"))
	assert.Nil(t, s.registry.ProjectHostnames("/home/dev/app"), "the bare dir must never become a key")

	// A pre-qualified dir cannot smuggle another publisher's key past the
	// composition: it is simply the second half of this caller's own key.
	registerViaHub(t, base, "tok", "popos", "mac:/home/other",
		map[string]ServiceTarget{"other": {Host: "localhost", Port: 3100}})
	assert.NotNil(t, s.registry.ProjectHostnames("popos:mac:/home/other"))
	assert.Nil(t, s.registry.ProjectHostnames("mac:/home/other"), "a wire-supplied qualified key must never be honored")

	// The routes report the hub-side view: composed PROJECT, publisher ORIGIN.
	routes := s.registry.AllRoutes()
	require.NotEmpty(t, routes)
	for _, r := range routes {
		assert.Equal(t, "popos", r.Origin)
		assert.Contains(t, r.ProjectDir, "popos:")
	}
}

// TestHubDeregister_CannotReachAnotherPublisher is D15/P7's first hole: with
// one shared hub token, publisher A must not be able to deregister publisher B
// by naming B's directory.
func TestHubDeregister_CannotReachAnotherPublisher(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "alice", "/home/a/app", map[string]ServiceTarget{"a": {Host: "localhost", Port: 3000}})
	registerViaHub(t, base, "tok", "bob", "/home/b/app", map[string]ServiceTarget{"b": {Host: "localhost", Port: 3001}})

	// Alice, holding a perfectly valid token, names Bob's directory.
	resp := hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
		Origin:     "alice",
		ProjectDir: "/home/b/app",
		PID:        os.Getpid(),
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "the call succeeds — it just cannot name Bob")

	assert.NotNil(t, s.registry.ProjectHostnames("bob:/home/b/app"), "Bob's registration must survive")
	assert.NotNil(t, s.registry.ProjectHostnames("alice:/home/a/app"), "Alice's own registration is untouched too")

	// And Alice deregistering her OWN project works, so the endpoint is not
	// simply broken.
	resp = hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
		Origin:     "alice",
		ProjectDir: "/home/a/app",
		PID:        os.Getpid(),
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, s.registry.ProjectHostnames("alice:/home/a/app"))
	assert.NotNil(t, s.registry.ProjectHostnames("bob:/home/b/app"))
}

// TestHubDeregister_CannotReachALocalProject is D15/P7's second hole, and the
// structural one: a local project's key is a BARE directory, and no
// "<origin>:<dir>" composition can produce that shape — with any origin, and
// with any dir the publisher can send.
func TestHubDeregister_CannotReachALocalProject(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	// A local registration, exactly as `prox up` on the hub host makes it.
	_, _, err := s.registry.Register(newTestRequest("/home/dev/local-project", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16443))
	require.NoError(t, err)

	// Every shape a publisher could try, including one that "looks like" the
	// local key and one that tries to escape the composition.
	attempts := []struct{ origin, dir string }{
		{"popos", "/home/dev/local-project"},
		{"popos", "local-project"},
		{"popos", ":/home/dev/local-project"},
		{"popos", "popos:/home/dev/local-project"},
	}
	for _, a := range attempts {
		resp := hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
			Origin:     a.origin,
			ProjectDir: a.dir,
			PID:        os.Getpid(),
		})
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotNil(t, s.registry.ProjectHostnames("/home/dev/local-project"),
			"local project must survive deregister attempt %q/%q", a.origin, a.dir)
	}

	_, ok := s.registry.Lookup("web.local.test", 16443)
	assert.True(t, ok, "the local route must still be serving")
}

// TestHubRequests_ScopedToCallerOrigin is D15/P7's third hole: captured request
// bodies are the most sensitive thing the daemon holds, and one shared token
// must not turn them into a shared inbox. Asking for another publisher's dir
// yields the caller's OWN (empty) ring.
func TestHubRequests_ScopedToCallerOrigin(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "alice", "/home/a/app", map[string]ServiceTarget{"a": {Host: "localhost", Port: 3000}})
	registerViaHub(t, base, "tok", "bob", "/home/b/app", map[string]ServiceTarget{"b": {Host: "localhost", Port: 3001}})

	// Bob's traffic, in Bob's ring (keyed by the composed key, as the hub
	// registers it).
	recordInto(s, proxy.RequestRecord{
		ID: "bob-secret-1", Method: "POST", URL: "/login",
		Hostname: "b.llt.test", ProjectDir: "bob:/home/b/app",
	})
	require.Equal(t, 1, projectCount(s, "bob:/home/b/app"))

	t.Run("snapshot", func(t *testing.T) {
		// Alice asks for Bob's dir.
		resp := hubDo(t, http.MethodGet,
			base+"/api/v1/requests?origin=alice&project=%2Fhome%2Fb%2Fapp&limit=100", "tok", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "bob-secret-1", "Alice must never see Bob's records")

		var result struct {
			Requests []proxy.RequestRecord `json:"requests"`
		}
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Empty(t, result.Requests, "Alice gets her own empty ring, not Bob's")
	})

	t.Run("stream", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet,
			base+"/api/v1/requests/stream?origin=alice&project=%2Fhome%2Fb%2Fapp", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := hubClient().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// The composed key "alice:/home/b/app" has no ring, so the stream ends
		// cleanly right after the preamble instead of delivering Bob's events.
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "bob-secret-1")
		assert.NotContains(t, string(body), "data:")
	})

	t.Run("bob reads his own", func(t *testing.T) {
		resp := hubDo(t, http.MethodGet,
			base+"/api/v1/requests?origin=bob&project=%2Fhome%2Fb%2Fapp&limit=100", "tok", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var result struct {
			Requests []proxy.RequestRecord `json:"requests"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		require.Len(t, result.Requests, 1)
		assert.Equal(t, "bob-secret-1", result.Requests[0].ID)
	})
}

// TestHubRegister_RemoteConflictNeverProbesPID is P4 on the register path. A
// publisher's PID is a number from another machine: it may be ALIVE here as
// some unrelated local process (here, PID 1), and treating that as "the holder
// is running" would wedge the publisher out of its own registration forever.
func TestHubRegister_RemoteConflictNeverProbesPID(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	first := hubRegisterRequest("popos", "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	first.PID = 1 // init: guaranteed alive ON THE HUB HOST, meaningless as a publisher PID
	first.StartTime = 111
	resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", first)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The publisher restarts: new PID, new start token, changed config. A
	// PID-liveness conflict path would see "holder PID 1 is alive" and refuse.
	second := hubRegisterRequest("popos", "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3100}})
	second.PID = 2
	second.StartTime = 222
	resp = hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", second)
	require.Equal(t, http.StatusOK, resp.StatusCode, "a remote re-register must never be refused on PID liveness")

	route, ok := s.registry.Lookup("app.llt.test", 16443)
	require.True(t, ok)
	assert.Equal(t, 3100, route.Target.Port, "the new generation's target must win")

	// An unchanged re-register is the idempotent no-op refresh, not a replace.
	resp = hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", second)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body RegisterResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, []string{"app.llt.test"}, body.Registered)
}

// TestStalePIDs_SkipsRemoteRegistrations is P4 on the sweep path: a remote
// registration whose PID is dead HERE must not be reaped, because that PID
// never named a process on this machine in the first place.
func TestStalePIDs_SkipsRemoteRegistrations(t *testing.T) {
	reg := NewRegistry()
	dead := deadPID(t)

	remote := newTestRequest(HubProjectKey("popos", "/home/dev/app"), "llt.test",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}}, 0, 16443)
	remote.PID = dead
	remote.Origin = "popos"
	_, _, err := reg.Register(remote)
	require.NoError(t, err)

	assert.Empty(t, reg.StalePIDs(), "a remote registration is never PID-swept (plan 031 P4)")

	// A LOCAL registration with the same dead PID still is — the skip is scoped
	// to remote registrations, not a blanket weakening of the sweep.
	local := newTestRequest("/home/dev/local", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16444)
	local.PID = dead
	_, _, err = reg.Register(local)
	require.NoError(t, err)

	stale := reg.StalePIDs()
	require.Len(t, stale, 1)
	assert.Equal(t, "/home/dev/local", stale[0].Dir)
}

// TestDeadRouteProbe_SkipsRemoteRoutes is P4 on the data plane: a 502 from a
// hub route must not trigger the on-502 dead-owner probe, whose whole premise
// (the owning process runs on this machine) is false for a publisher.
func TestDeadRouteProbe_SkipsRemoteRoutes(t *testing.T) {
	reaped := make(chan string, 4)
	newProxy := func() *DynamicProxy {
		dp := NewDynamicProxy(NewRegistry(), nil, nil, nil,
			slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{})))
		dp.probeMinInterval = 0
		dp.probeIsAlive = func(int, int64) bool { return false } // "owner is dead"
		dp.SetDeadRouteRemover(func(dir string, pid int, startTime int64) { reaped <- dir })
		return dp
	}

	// Remote route: no probe, no reap.
	newProxy().triggerDeadRouteProbe("popos:/home/dev/app", 4242, 1, "popos")
	select {
	case dir := <-reaped:
		t.Fatalf("a remote route must never be PID-probed or reaped, got %q", dir)
	case <-time.After(150 * time.Millisecond):
	}

	// Local route with the identical inputs: still probed and reaped, so the
	// skip is the origin and nothing else.
	newProxy().triggerDeadRouteProbe("/home/dev/app", 4242, 1, "")
	select {
	case dir := <-reaped:
		assert.Equal(t, "/home/dev/app", dir)
	case <-time.After(2 * time.Second):
		t.Fatal("a local route's dead owner must still be reaped")
	}
}

// TestHubMode_KeepsDaemonAliveWhenEmpty pins D13: a hub with no publishers is
// idle, not unwanted. The empty-registry shutdown timer must not fire while hub
// mode is on — and must work normally again once it is off.
func TestHubMode_KeepsDaemonAliveWhenEmpty(t *testing.T) {
	s, _ := newHubServer(t, HubConfig{Token: "tok"})
	s.shutdownDelay = 20 * time.Millisecond

	require.True(t, s.registry.IsEmpty())
	s.scheduleShutdownWhenEmpty()

	select {
	case <-s.ShutdownCh():
		t.Fatal("the daemon must stay alive while hub mode is on")
	case <-time.After(200 * time.Millisecond):
	}

	// Turning hub mode off re-arms the ordinary empty-daemon shutdown.
	s.StopHub()
	select {
	case <-s.ShutdownCh():
	case <-time.After(2 * time.Second):
		t.Fatal("with hub mode off, an empty registry must schedule shutdown again")
	}
}

// TestHubStop_RemovesRemoteRegistrations: the publishers reached this daemon
// only through the listener that just closed, so their routes must go with it —
// while a LOCAL project's routes stay exactly where they were.
func TestHubStop_RemovesRemoteRegistrations(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "popos", "/home/dev/app", map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	_, _, err := s.registry.Register(newTestRequest("/home/dev/local", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16444))
	require.NoError(t, err)

	s.StopHub()

	assert.Nil(t, s.registry.ProjectHostnames("popos:/home/dev/app"), "remote registrations go with the hub")
	assert.NotNil(t, s.registry.ProjectHostnames("/home/dev/local"), "local projects are untouched")
	assert.False(t, s.hubEnabled())
	assert.Empty(t, s.HubListenAddr())
}

// TestHubToken_RotationInvalidatesOldToken pins D18's no-grace rule: the moment
// the token rotates, the old one is rejected and the new one works.
func TestHubToken_RotationInvalidatesOldToken(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "old-token"})

	resp := hubDo(t, http.MethodGet, base+"/api/v1/routes", "old-token", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Rotate through the socket handler, as `prox hub token --rotate` does.
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/token", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var rotated HubTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	require.NotEmpty(t, rotated.Token)
	assert.NotEqual(t, "old-token", rotated.Token)

	resp = hubDo(t, http.MethodGet, base+"/api/v1/routes", "old-token", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the old token must be rejected immediately")
	assert.Equal(t, "UNAUTHORIZED", errorCode(t, resp))

	resp = hubDo(t, http.MethodGet, base+"/api/v1/routes", rotated.Token, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the new token must work")

	// And it landed on disk, so a restart keeps accepting it.
	onDisk, err := ReadHubToken()
	require.NoError(t, err)
	assert.Equal(t, rotated.Token, onDisk)
}

// TestStartHub_RepeatWithSameListenIsNoOp pins the D14 restart rules: the same
// address keeps the listener (a `prox hub start` with no flags must not churn a
// running hub), a different one rebinds, and a FAILED rebind rolls back to the
// listener that was already working.
func TestStartHub_RepeatWithSameListenIsNoOp(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})
	addr := s.HubListenAddr()
	cfg := s.hubConfigSnapshot().cfg

	require.NoError(t, s.StartHub(cfg))
	assert.Equal(t, addr, s.HubListenAddr(), "a repeat start on the same address must not rebind")

	resp := hubDo(t, http.MethodGet, base+"/api/v1/routes", "tok", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the original listener must still be serving")
}

func TestStartHub_RebindsOnNewListenAddress(t *testing.T) {
	s, _ := newHubServer(t, HubConfig{Token: "tok"})
	first := s.HubListenAddr()

	// A second ephemeral port, released immediately so StartHub can take it.
	next := freeLoopbackAddr(t)
	cfg := s.hubConfigSnapshot().cfg
	cfg.Listen = next
	require.NoError(t, s.StartHub(cfg))

	assert.Equal(t, next, s.HubListenAddr())
	assert.NotEqual(t, first, s.HubListenAddr())

	resp := hubDo(t, http.MethodGet, "http://"+s.HubListenAddr()+"/api/v1/routes", "tok", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The old address is no longer served.
	assert.Eventually(t, func() bool {
		_, err := hubClient().Get("http://" + first + "/health")
		return err != nil
	}, 2*time.Second, 20*time.Millisecond, "the replaced listener must be closed")
}

func TestStartHub_RebindFailureRollsBack(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})
	original := s.HubListenAddr()

	// Occupy a port and ask the hub to move onto it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()

	cfg := s.hubConfigSnapshot().cfg
	cfg.Listen = blocker.Addr().String()
	err = s.StartHub(cfg)
	require.Error(t, err, "binding an occupied port must fail")
	assert.Contains(t, err.Error(), "binding hub control plane")

	assert.Equal(t, original, s.HubListenAddr(), "a failed rebind must roll back to the previous listener")
	resp := hubDo(t, http.MethodGet, base+"/api/v1/routes", "tok", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the previous listener must still be serving")
}

// TestAutostartHub covers D13's start-time path and — the part that matters —
// its failure mode: a hub that cannot bind must leave the daemon serving this
// machine's LOCAL projects, because hub mode is additive and may never cost a
// developer their own `prox up`.
func TestAutostartHub(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))

	t.Run("autostart true starts hub mode", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		require.NoError(t, SaveHubConfig(HubConfig{
			Domain: "llt.test", Listen: "127.0.0.1:0", HTTPSPort: 16443,
			Auth: HubAuthNone, Autostart: true,
		}))

		s := newLifecycleServer()
		t.Cleanup(s.StopHub)
		autostartHub(s, logger)

		assert.True(t, s.hubEnabled())
		assert.NotEmpty(t, s.HubListenAddr())
	})

	t.Run("autostart false leaves hub mode off", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		require.NoError(t, SaveHubConfig(HubConfig{
			Domain: "llt.test", Listen: "127.0.0.1:0", HTTPSPort: 16443, Auth: HubAuthNone,
		}))

		s := newLifecycleServer()
		autostartHub(s, logger)
		assert.False(t, s.hubEnabled())
	})

	t.Run("no hub.yaml is not an error", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s := newLifecycleServer()
		autostartHub(s, logger)
		assert.False(t, s.hubEnabled())
	})

	t.Run("a bind failure leaves the daemon serving local projects", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer blocker.Close()

		require.NoError(t, SaveHubConfig(HubConfig{
			Domain: "llt.test", Listen: blocker.Addr().String(), HTTPSPort: 16443,
			Auth: HubAuthNone, Autostart: true,
		}))

		s := newLifecycleServer()
		autostartHub(s, logger)
		require.False(t, s.hubEnabled(), "the hub could not bind")

		// The daemon is still a working local daemon.
		status, body := s.register(newTestRequest("/home/dev/local", "local.test",
			map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16444))
		require.Equal(t, http.StatusOK, status, "local registration must still work: %v", body)
		assert.NotNil(t, s.registry.ProjectHostnames("/home/dev/local"))
	})
}

// TestHubStatus_ReportsPublishersAndIsAbsentWhenOff pins D20's hub-host object,
// including its absence: a daemon with no hub emits no `hub` key at all, which
// is what keeps hub-less status output unchanged (AC1).
func TestHubStatus_ReportsPublishersAndIsAbsentWhenOff(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok", Domain: "llt.test", HTTPSPort: 16443})
	registerViaHub(t, base, "tok", "popos", "/home/dev/app",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hub/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var status HubStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.True(t, status.Enabled)
	assert.Equal(t, "llt.test", status.Domain)
	assert.Equal(t, s.HubListenAddr(), status.Listen)
	require.Len(t, status.Publishers, 1)
	assert.Equal(t, "popos", status.Publishers[0].Origin)
	assert.Equal(t, "/home/dev/app", status.Publishers[0].ProjectDir)
	assert.Equal(t, "popos:/home/dev/app", status.Publishers[0].Key)
	assert.Equal(t, 1, status.Publishers[0].Routes)

	// Daemon status carries the same object while hub mode is on...
	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"hub"`)

	// ...and no `hub` key at all once it is off.
	s.StopHub()
	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"hub"`)
}

// TestHubStart_FromConfigFile covers the socket lifecycle endpoints end to end:
// hub/start reads ~/.prox/hub.yaml (the file is the source of truth, D13/D14)
// and hub/stop turns it off again.
func TestHubStart_FromConfigFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newLifecycleServer()
	t.Cleanup(s.StopHub)

	// No config yet: a clear, distinguishable refusal.
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/start", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er))
	assert.Equal(t, "HUB_NOT_CONFIGURED", er.Code)

	require.NoError(t, SaveHubConfig(HubConfig{
		Domain: "llt.test", Listen: "127.0.0.1:0", HTTPSPort: 16443, Auth: HubAuthToken,
	}))

	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/start", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var status HubStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.True(t, status.Enabled)
	assert.NotEmpty(t, status.Listen)

	// A token was generated for it (auth: token with no token file yet).
	token, err := ReadHubToken()
	require.NoError(t, err)
	resp := hubDo(t, http.MethodGet, "http://"+status.Listen+"/api/v1/routes", token, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	rec = httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/stop", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, s.hubEnabled())
}

// TestHubStart_InvalidConfigIsABadRequest: a hand-edited hub.yaml with a public
// listen address must come back as the user's mistake (400), not a server
// error.
func TestHubStart_InvalidConfigIsABadRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newLifecycleServer()
	t.Cleanup(s.StopHub)

	require.NoError(t, EnsureDaemonDir())
	require.NoError(t, os.WriteFile(HubConfigPath(),
		[]byte("domain: llt.test\nlisten: 93.184.216.34:8443\nhttps_port: 443\n"), 0600))

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/start", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er))
	assert.Equal(t, "HUB_CONFIG_INVALID", er.Code)
	assert.False(t, s.hubEnabled())
}

// freeLoopbackAddr returns a loopback address whose port was bound and released,
// so a caller can bind it next. Racy in principle, reliable in practice, and
// used only where a test needs a SPECIFIC second address.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}
