package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
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
// TestHubRegister_ComposesKeyServerSide pins D15's derivation rule: the key is
// built from the caller's own origin and directory, and a pre-qualified key on
// the wire is not merely rejected — it cannot be expressed.
func TestHubRegister_ComposesKeyServerSide(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "popos", "/home/dev/app",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	assert.NotNil(t, s.registry.ProjectHostnames(HubProjectKey("popos", "/home/dev/app")))
	assert.Nil(t, s.registry.ProjectHostnames("/home/dev/app"), "the bare dir must never become a key")

	// A pre-qualified dir cannot smuggle another publisher's key past the
	// composition: it is simply the second half of this caller's own key.
	registerViaHub(t, base, "tok", "popos", "/hub:mac|/home/other",
		map[string]ServiceTarget{"other": {Host: "localhost", Port: 3100}})
	assert.NotNil(t, s.registry.ProjectHostnames(HubProjectKey("popos", "/hub:mac|/home/other")))
	assert.Nil(t, s.registry.ProjectHostnames(HubProjectKey("mac", "/home/other")),
		"a wire-supplied qualified key must never be honored")

	// The routes report the hub-side view: composed PROJECT, publisher ORIGIN.
	routes := s.registry.AllRoutes()
	require.NotEmpty(t, routes)
	for _, r := range routes {
		assert.Equal(t, "popos", r.Origin)
		assert.True(t, isHubProjectKey(r.ProjectDir))
	}
}

// TestSocketRegister_RefusesAHubKeyPrefix is the other half of the F2
// invariant. Hub keys all begin with "hub:", so a LOCAL registration may not:
// with both halves in place, "no composition can name a local project" holds
// for any origin, any directory, and any platform's idea of a path — rather
// than resting on "a local key is an absolute path", which a Windows-shaped
// `C:\work\app` would have disproved.
func TestSocketRegister_RefusesAHubKeyPrefix(t *testing.T) {
	s := newLifecycleServer()

	req := newTestRequest("hub:popos|/home/dev/app", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16443)
	req.Version = s.version
	body, err := json.Marshal(req)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/register", bytes.NewReader(body)))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er))
	assert.Equal(t, "BAD_REQUEST", er.Code)
	assert.Contains(t, er.Error, "hub:")
	assert.Nil(t, s.registry.ProjectHostnames("hub:popos|/home/dev/app"))
}

// TestHubDeregister_KeyCompositionPreventsNamingAnotherPublishersDir is the
// FIRST of the three composition tests, and its name says exactly what it
// proves: an HONEST publisher that names someone else's directory reaches its
// own key, not theirs.
//
// It does NOT prove that Alice cannot reach Bob. It cannot: the hub's only
// credential is one shared bearer token (D6/D12), so nothing on this mount
// establishes that a caller is the machine it claims to be. What composition
// buys is that cross-tenant access cannot happen BY ACCIDENT — a stale
// project_dir, a copied config, a publisher confused about its own identity.
// The deliberate case is
// TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation, which
// demonstrates the forgery succeeding.
func TestHubDeregister_KeyCompositionPreventsNamingAnotherPublishersDir(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "alice", "/home/a/app", map[string]ServiceTarget{"a": {Host: "localhost", Port: 3000}})
	registerViaHub(t, base, "tok", "bob", "/home/b/app", map[string]ServiceTarget{"b": {Host: "localhost", Port: 3001}})

	// Alice, holding a perfectly valid token and honestly sending her own
	// origin, names Bob's directory.
	resp := hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
		Origin:     "alice",
		ProjectDir: "/home/b/app",
		PID:        os.Getpid(),
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "the call succeeds — it just cannot name Bob")

	assert.NotNil(t, s.registry.ProjectHostnames(HubProjectKey("bob", "/home/b/app")), "Bob's registration must survive")
	assert.NotNil(t, s.registry.ProjectHostnames(HubProjectKey("alice", "/home/a/app")), "Alice's own registration is untouched too")

	// And Alice deregistering her OWN project works, so the endpoint is not
	// simply broken.
	resp = hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
		Origin:     "alice",
		ProjectDir: "/home/a/app",
		PID:        os.Getpid(),
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, s.registry.ProjectHostnames(HubProjectKey("alice", "/home/a/app")))
	assert.NotNil(t, s.registry.ProjectHostnames(HubProjectKey("bob", "/home/b/app")))
}

// TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation
// demonstrates the hole the three composition tests do NOT close, so that no
// future reader mistakes what the guarantee is.
//
// The hub authenticates ONE shared bearer token and takes the origin from the
// caller. A publisher holding that token can therefore send another
// publisher's origin and address their registration: deregister it, read its
// captured traffic, or attach a tunnel in its place. Below, Alice does exactly
// that to Bob, and it WORKS — which is what this test asserts.
//
// This is the accepted posture of plan 031 D12/§8, not an oversight: publishers
// on one hub are declared mutually trusted, and the hub token is the trust
// boundary. The panel proposed an opaque per-registration lease ID returned by
// register and required on tunnel and deregister; it was declined for v1 as a
// scope increase against a threat the trust model does not include. Closing it
// properly needs PER-ORIGIN CREDENTIALS — each publisher holding its own
// secret, so the origin is proven rather than claimed — at which point this
// test should start failing and be replaced by one asserting a 401/403.
//
// Until then, "cross-tenant access is impossible" is false and must not be
// written anywhere: what is true is "impossible by accident, impossible by
// composition". The F8 default (bind only loopback and tailnet addresses)
// shrinks the exposure by keeping the token off wires strangers share; it does
// not remove it for anyone already holding the token.
func TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "bob", "/home/b/app", map[string]ServiceTarget{"b": {Host: "localhost", Port: 3001}})
	bobKey := HubProjectKey("bob", "/home/b/app")
	recordInto(s, proxy.RequestRecord{
		ID: "bob-secret-1", Method: "POST", URL: "/login",
		Hostname: "b.llt.test", ProjectDir: bobKey,
	})
	require.Equal(t, 1, projectCount(s, bobKey))

	// Alice holds the shared token and simply CLAIMS to be Bob.
	t.Run("reads Bob's captured traffic", func(t *testing.T) {
		resp := hubDo(t, http.MethodGet,
			base+"/api/v1/requests?origin=bob&project=%2Fhome%2Fb%2Fapp&limit=100", "tok", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var result struct {
			Requests []proxy.RequestRecord `json:"requests"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		require.Len(t, result.Requests, 1,
			"KNOWN LIMITATION (D12/§8): a forged origin reads another publisher's records")
		assert.Equal(t, "bob-secret-1", result.Requests[0].ID)
	})

	t.Run("deregisters Bob", func(t *testing.T) {
		resp := hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
			Origin:     "bob",
			ProjectDir: "/home/b/app",
			PID:        os.Getpid(),
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Nil(t, s.registry.ProjectHostnames(bobKey),
			"KNOWN LIMITATION (D12/§8): a forged origin deregisters another publisher")
	})
}

// TestHubDeregister_KeyCompositionCannotProduceALocalKey is the STRUCTURAL one
// of the three, and the only one that holds against a hostile caller as well as
// an honest one: a local project's key can never be produced by any
// "<origin>|<dir>" composition, because every composed key carries the "hub:"
// prefix and the socket mount refuses a project_dir that does. Forging an
// origin does not help — there is no origin that composes to a local key.
func TestHubDeregister_KeyCompositionCannotProduceALocalKey(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	// A local registration, exactly as `prox up` on the hub host makes it.
	_, _, err := s.registry.Register(newTestRequest("/home/dev/local-project", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16443))
	require.NoError(t, err)

	// Every shape a publisher could try, including one that "looks like" the
	// local key, one that tries to escape the composition, and one that forges
	// an origin — the attack the other two tests do not stop.
	attempts := []struct{ origin, dir string }{
		{"popos", "/home/dev/local-project"},
		{"popos", "/local-project"},
		{"popos", "/hub:popos|/home/dev/local-project"},
		{"hub", "/home/dev/local-project"},
		{"", "/home/dev/local-project"},
	}
	for _, a := range attempts {
		resp := hubDo(t, http.MethodPost, base+"/api/v1/deregister", "tok", DeregisterRequest{
			Origin:     a.origin,
			ProjectDir: a.dir,
			PID:        os.Getpid(),
		})
		// Either a 400 (a malformed origin) or a 200 that named the caller's own
		// non-existent key. Never a removal.
		assert.NotNil(t, s.registry.ProjectHostnames("/home/dev/local-project"),
			"local project must survive deregister attempt %q/%q (status %d)", a.origin, a.dir, resp.StatusCode)
	}

	_, ok := s.registry.Lookup("web.local.test", 16443)
	assert.True(t, ok, "the local route must still be serving")
}

// TestHubRequests_KeyCompositionScopesToTheCallersOwnOrigin is the third
// composition test. Captured request bodies are the most sensitive thing the
// daemon holds, and an HONEST publisher asking for another publisher's dir gets
// its OWN (empty) ring rather than theirs.
//
// As with the deregister case, this is about accidents, not about a hostile
// caller: see TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation
// for what a publisher that lies about its origin can still read.
func TestHubRequests_KeyCompositionScopesToTheCallersOwnOrigin(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	registerViaHub(t, base, "tok", "alice", "/home/a/app", map[string]ServiceTarget{"a": {Host: "localhost", Port: 3000}})
	registerViaHub(t, base, "tok", "bob", "/home/b/app", map[string]ServiceTarget{"b": {Host: "localhost", Port: 3001}})

	// Bob's traffic, in Bob's ring (keyed by the composed key, as the hub
	// registers it).
	bobKey := HubProjectKey("bob", "/home/b/app")
	recordInto(s, proxy.RequestRecord{
		ID: "bob-secret-1", Method: "POST", URL: "/login",
		Hostname: "b.llt.test", ProjectDir: bobKey,
	})
	require.Equal(t, 1, projectCount(s, bobKey))

	t.Run("snapshot", func(t *testing.T) {
		// Alice asks for Bob's dir under her own origin.
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

		// The composed key has no ring, so the stream ends cleanly right after
		// the preamble instead of delivering Bob's events.
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
	newProxy().triggerDeadRouteProbe(HubProjectKey("popos", "/home/dev/app"), 4242, 1, "popos")
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

	assert.Nil(t, s.registry.ProjectHostnames(HubProjectKey("popos", "/home/dev/app")),
		"remote registrations go with the hub")
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
	assert.Equal(t, HubProjectKey("popos", "/home/dev/app"), status.Publishers[0].Key)
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

// TestHubRegister_BoundsAndValidatesTheBody is plan 031 F9: the network mount
// takes JSON from another machine, so it applies the same naming and target
// rules internal/config applies to a project's own prox.yaml — plus the bounds
// a local config file never needed.
func TestHubRegister_BoundsAndValidatesTheBody(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	badRequests := []struct {
		name string
		req  RegisterRequest
	}{
		{
			name: "service name is not a DNS label",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"not a label": {Host: "localhost", Port: 3000}}),
		},
		{
			name: "service name is a wildcard",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"*": {Host: "localhost", Port: 3000}}),
		},
		{
			name: "service name traverses",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"../evil": {Host: "localhost", Port: 3000}}),
		},
		{
			name: "target host is not a host",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"app": {Host: "not a host!", Port: 3000}}),
		},
		{
			name: "target port is out of range",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"app": {Host: "localhost", Port: 70000}}),
		},
		{
			name: "target port is zero",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"app": {Host: "localhost", Port: 0}}),
		},
		{
			name: "no services at all",
			req:  hubRegisterRequest("popos", "/home/dev/app", map[string]ServiceTarget{}),
		},
		{
			name: "relative project dir",
			req: hubRegisterRequest("popos", "app",
				map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}}),
		},
		{
			name: "windows-shaped project dir",
			req: hubRegisterRequest("popos", `C:\work\app`,
				map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}}),
		},
		{
			// Plan 031, review B6: a host longer than DNS allows. The hostname
			// rules said what characters were legal and nothing about how many,
			// so a host of any length passed.
			name: "target host is longer than a DNS name may be",
			req: hubRegisterRequest("popos", "/home/dev/app",
				map[string]ServiceTarget{"app": {Host: strings.Repeat("a", 64) + ".example.com", Port: 3000}}),
		},
	}
	for _, tt := range badRequests {
		t.Run(tt.name, func(t *testing.T) {
			resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", tt.req)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))
		})
	}

	t.Run("too many services", func(t *testing.T) {
		services := make(map[string]ServiceTarget, hubMaxServices+1)
		for i := 0; i <= hubMaxServices; i++ {
			services[fmt.Sprintf("svc-%d", i)] = ServiceTarget{Host: "localhost", Port: 3000}
		}
		resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok",
			hubRegisterRequest("popos", "/home/dev/app", services))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))
	})

	t.Run("an oversized body is refused rather than buffered", func(t *testing.T) {
		// The body is a PERFECTLY VALID registration, padded past
		// maxControlRequestBytes with a field the decoder would otherwise
		// ignore. That is the point (plan 031, review B8): the earlier version
		// of this test padded the project_dir, which every later validator
		// would have rejected anyway, so it could not distinguish "the size cap
		// refused this" from "some other rule did". This one registers
		// successfully if MaxBytesReader is removed, so only the cap can be
		// what fails it.
		valid, err := json.Marshal(hubRegisterRequest("popos", "/home/dev/big",
			map[string]ServiceTarget{"big": {Host: "localhost", Port: 3000}}))
		require.NoError(t, err)
		padded := append([]byte(nil), valid[:len(valid)-1]...) // drop the closing brace
		padded = append(padded, []byte(`,"ignored_padding":"`)...)
		padded = append(padded, bytes.Repeat([]byte("a"), maxControlRequestBytes+1024)...)
		padded = append(padded, []byte(`"}`)...)

		req, err := http.NewRequest(http.MethodPost, base+"/api/v1/register", bytes.NewReader(padded))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hubClient().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))

		// And nothing was registered: the request was refused, not truncated
		// into a smaller registration.
		assert.False(t, s.registry.HasProject(HubProjectKey("popos", "/home/dev/big")),
			"an over-cap body must register nothing at all")
	})

	t.Run("trailing JSON is refused", func(t *testing.T) {
		// Two objects in one body: the first must not be quietly registered
		// while the second is silently discarded.
		first, err := json.Marshal(hubRegisterRequest("popos", "/home/dev/app",
			map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}}))
		require.NoError(t, err)
		body := append(append([]byte{}, first...), first...)

		req, err := http.NewRequest(http.MethodPost, base+"/api/v1/register", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		resp, err := hubClient().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))
	})

	t.Run("a well-formed registration still succeeds", func(t *testing.T) {
		registerViaHub(t, base, "tok", "popos", "/home/dev/app",
			map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	})
}

// TestHubRegister_CannotLowerTheCaptureDiskBudget is the cross-tenant half of
// F9, and the reason that finding is not merely hygiene.
//
// The capture disk budget is not per-project: EffectiveCaptureDiskBudget folds
// every capture-enabled project's value into ONE daemon-wide minimum for the
// hub host's single capture dir. A remote registration asking for a tiny budget
// would therefore start evicting every OTHER project's captured bodies on a
// machine the publisher does not own — reachable by a publisher that is
// supposed to be able to touch nothing but its own registration.
func TestHubRegister_CannotLowerTheCaptureDiskBudget(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	// A local project on the hub host that opted every capture-enabled project
	// up to 4 GiB.
	local := newTestRequest("/home/dev/local", "local.test",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}}, 0, 16443)
	local.CaptureEnabled = true
	local.DiskBudget = 4 << 30
	_, _, err := s.registry.Register(local)
	require.NoError(t, err)
	require.Equal(t, int64(4<<30), s.registry.EffectiveCaptureDiskBudget())

	// A publisher tries to pull the daemon-wide bound down to 1 MiB.
	hostile := hubRegisterRequest("popos", "/home/dev/app",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
	hostile.CaptureEnabled = true
	hostile.DiskBudget = 1 << 20
	resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", hostile)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the registration itself is fine — only its budget claim is ignored")

	assert.Equal(t, int64(4<<30), s.registry.EffectiveCaptureDiskBudget(),
		"a remote registration must not move the hub host's capture disk budget")

	// Belt and braces: the stored registration carries no budget at all, so even
	// a future accountant that forgot to skip remote registrations reads zero
	// (i.e. "the default") rather than the publisher's number.
	publishers := s.registry.RemotePublishers()
	require.Len(t, publishers, 1)
	snap, ok := s.registry.snapshotProject(publishers[0].Key)
	require.True(t, ok)
	assert.Equal(t, int64(0), snap.proj.DiskBudget)
}

// TestHubRegister_ClampsTheCaptureBodyCap is DiskBudget's sibling (plan 031 F9).
//
// MaxBodySize is how many bytes a capture buffer holds in MEMORY before it
// spills, per request and per response, on every concurrent request through the
// route. Clearing DiskBudget bounds the spill files and nothing else, so an
// unbounded MaxBodySize from a remote publisher is a memory-exhaustion lever on
// a machine it does not own. A value at or below the hub's own default is the
// publisher's business and is kept exactly as sent.
func TestHubRegister_ClampsTheCaptureBodyCap(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	// Each subtest registers its own service name and directory: two projects
	// claiming one hostname is the takeover rule, which is a different test.
	registerWithCap := func(t *testing.T, svc string, capBytes int64) int64 {
		t.Helper()
		dir := "/home/dev/" + svc
		req := hubRegisterRequest("popos", dir,
			map[string]ServiceTarget{svc: {Host: "localhost", Port: 3000}})
		req.CaptureEnabled = true
		req.MaxBodySize = capBytes
		resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", req)
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"the registration itself is fine — only an over-large cap is held down")
		snap, ok := s.registry.snapshotProject(HubProjectKey("popos", dir))
		require.True(t, ok)
		return snap.proj.MaxBodySize
	}

	t.Run("an enormous cap is held to the hub default", func(t *testing.T) {
		assert.Equal(t, int64(constants.DefaultCaptureMaxBodySize),
			registerWithCap(t, "greedy", 4<<30))
	})

	t.Run("a modest cap is the publisher's own business", func(t *testing.T) {
		assert.Equal(t, int64(64<<10), registerWithCap(t, "modest", 64<<10))
	})

	t.Run("zero still means the daemon default", func(t *testing.T) {
		assert.Equal(t, int64(0), registerWithCap(t, "unset", 0))
	})

	t.Run("a negative cap is refused outright", func(t *testing.T) {
		req := hubRegisterRequest("popos", "/home/dev/nonsense",
			map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})
		req.MaxBodySize = -1
		resp := hubDo(t, http.MethodPost, base+"/api/v1/register", "tok", req)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "BAD_REQUEST", errorCode(t, resp))
	})
}

// TestHubCaptureForwarding_EndToEndOverTheHubMount is plan 031 F12's regression
// test: a legitimate hub publisher must be able to read and stream its OWN
// captured requests through the hub's network mount.
//
// The bug it guards against was a functional one, not a hardening nit. The
// network capture endpoints require origin AND project and compose the key
// themselves (D15), but the forwarder sent the already-composed key as
// `project` alone — so every hub publisher got a 400 and its TUI stayed empty
// forever. C5's Client.scopedRequestQuery splits the key back into its two
// halves for a hub client; this exercises BOTH callers of it (the snapshot and
// the SSE subscription) against a real hub mount, since the split lives in one
// place precisely so both can rely on it.
func TestHubCaptureForwarding_EndToEndOverTheHubMount(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok"})

	const (
		origin = "popos"
		dir    = "/home/dev/app"
	)
	key := HubProjectKey(origin, dir)

	client, err := NewHubClient(base, "tok", origin)
	require.NoError(t, err)
	_, err = client.Register(hubRegisterRequest(origin, dir,
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}}))
	require.NoError(t, err)

	// A record captured on the HUB, keyed as the hub registered the project.
	recordInto(s, proxy.RequestRecord{
		ID: "hub-1", Method: "GET", URL: "/one", Hostname: "app.llt.test", ProjectDir: key,
	})

	t.Run("snapshot", func(t *testing.T) {
		// The publisher asks with the COMPOSED key, exactly as the forwarder
		// does; the client splits it into origin+project for this mount.
		records, err := client.Requests(context.Background(), key, 100)
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Equal(t, "hub-1", records[0].ID)
	})

	t.Run("stream and backfill through the forwarder", func(t *testing.T) {
		localRM := proxy.NewRequestManager(100)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			ForwardRequestsWithClient(ctx, client, key, localRM, nil, nil)
		}()

		// The backfill snapshot brings the existing record across...
		require.Eventually(t, func() bool {
			return len(localRM.Recent(proxy.RequestFilter{})) == 1
		}, 10*time.Second, 5*time.Millisecond, "the hub backfill must reach the publisher's local ring")

		// ...and a record captured afterwards arrives over the SSE stream.
		recordInto(s, proxy.RequestRecord{
			ID: "hub-2", Method: "POST", URL: "/two", Hostname: "app.llt.test", ProjectDir: key,
		})
		require.Eventually(t, func() bool {
			for _, r := range localRM.Recent(proxy.RequestFilter{}) {
				if r.ID == "hub-2" {
					return true
				}
			}
			return false
		}, 10*time.Second, 5*time.Millisecond, "the hub request stream must reach the publisher's local ring")

		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the forwarder did not stop")
		}
	})

	t.Run("a socket client's bare key still works on the socket mount", func(t *testing.T) {
		// The other half of the contract: the SAME method with no origin on the
		// client sends ?project=<key> as it always did, so the socket mount is
		// untouched.
		socket := NewClient("")
		q := socket.scopedRequestQuery("/home/dev/local")
		assert.Equal(t, "/home/dev/local", q.Get("project"))
		assert.Empty(t, q.Get("origin"))

		// And a hub client asked for a key that is not its own falls back to the
		// unsplit form rather than silently addressing someone else.
		q = client.scopedRequestQuery(HubProjectKey("someone-else", dir))
		assert.Empty(t, q.Get("origin"))
	})
}

// hubStartWithConfig posts a hub/start carrying a proposed config, the way
// `prox hub start` with flags does.
func hubStartWithConfig(t *testing.T, s *Server, cfg HubConfig) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(HubStartRequest{Config: &cfg})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/start", bytes.NewReader(body)))
	return rec
}

// TestHubStart_CommitsTheConfigOnlyAfterBinding is plan 031 F13: validate,
// bind, and commit are ONE operation, performed by the daemon.
//
// `prox hub start` used to write ~/.prox/hub.yaml and then ask the daemon to
// bind it. A rebind that failed therefore left the old listener serving while
// the file described an address nothing was listening on — and the next daemon
// start would autostart into that broken config. The file must only ever
// describe something the daemon has actually bound.
func TestHubStart_CommitsTheConfigOnlyAfterBinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newLifecycleServer()
	t.Cleanup(s.StopHub)

	good := HubConfig{Domain: "llt.test", Listen: "127.0.0.1:0", HTTPSPort: 16443, Auth: HubAuthToken}
	rec := hubStartWithConfig(t, s, good)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bound := s.HubListenAddr()

	stored, err := LoadHubConfig()
	require.NoError(t, err, "a successful start writes the file")
	assert.Equal(t, "llt.test", stored.Domain)
	assert.Equal(t, 16443, stored.HTTPSPort)
	assert.Empty(t, stored.Token, "the token is never written into hub.yaml")

	t.Run("a refused config is not written", func(t *testing.T) {
		bad := good
		bad.Listen = "0.0.0.0:8443"
		rec := hubStartWithConfig(t, s, bad)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "HUB_CONFIG_INVALID", errorCodeFromRecorder(t, rec))

		after, err := LoadHubConfig()
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:0", after.Listen, "a refused config must not reach hub.yaml")
		assert.Equal(t, bound, s.HubListenAddr(), "and the running hub must be untouched")
	})

	t.Run("a config that cannot bind is not written", func(t *testing.T) {
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer blocker.Close()

		occupied := good
		occupied.Listen = blocker.Addr().String()
		rec := hubStartWithConfig(t, s, occupied)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, "HUB_BIND_FAILED", errorCodeFromRecorder(t, rec))

		after, err := LoadHubConfig()
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:0", after.Listen,
			"a config the daemon could not bind must not be left in hub.yaml for the next autostart to pick up")
		assert.Equal(t, bound, s.HubListenAddr(), "the previous listener keeps serving")
	})
}

// TestHubStart_InPlaceReconfigureCommitsBeforeLiveState is the SAME commit rule
// on startHub's other arm (plan 031 F13).
//
// The rebind arm binds before it commits. The same-address fast path used to
// assign s.hubCfg and s.hubToken and only THEN call commitHubConfig, so a
// `prox hub start --auth none` whose save failed answered 500 while the live
// hub had already switched to accepting unauthenticated calls — with
// ~/.prox/hub.yaml still describing the old mode. An operator who sees an error
// must be able to believe nothing changed.
func TestHubStart_InPlaceReconfigureCommitsBeforeLiveState(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok", Domain: "llt.test", HTTPSPort: 16443})
	current := s.hubConfigSnapshot().cfg
	require.Equal(t, HubAuthToken, current.Auth)

	// Fail the save at the rename: nothing is persisted, so hub.yaml and the
	// live hub must agree on the OLD config afterwards.
	restore := hubFileWriter
	hubFileWriter.RenameFn = func(string, string) error { return errors.New("disk full") }
	t.Cleanup(func() { hubFileWriter = restore })

	next := current
	next.Auth = HubAuthNone
	rec := hubStartWithConfig(t, s, next)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	mode, token := s.hubAuthSnapshot()
	assert.Equal(t, HubAuthToken, mode, "a failed save must not switch the live hub to auth: none")
	assert.Equal(t, "tok", token)

	// And on the wire, which is what the operator is actually exposed to.
	assert.Equal(t, http.StatusUnauthorized,
		hubDo(t, http.MethodGet, base+"/api/v1/routes", "", nil).StatusCode,
		"the hub must still demand the token it was started with")
	assert.Equal(t, http.StatusOK,
		hubDo(t, http.MethodGet, base+"/api/v1/routes", "tok", nil).StatusCode)
}

// TestStartHub_RefusesDomainOrPortChangeWithPublishers is the other half of F13.
//
// Domain and the data-plane ports are baked into every remote route when it
// registers (D4): the hostname is "<service>.<domain>" and the route is keyed by
// "<hostname>:<port>". Swapping them under a live publisher would leave the
// routes on the old values while `prox hub status` reported the new ones —
// status describing a configuration the routes do not use. The chosen arm is
// REJECT, with a message that says what to do instead.
func TestStartHub_RefusesDomainOrPortChangeWithPublishers(t *testing.T) {
	s, base := newHubServer(t, HubConfig{Token: "tok", Domain: "llt.test", HTTPSPort: 16443})
	registerViaHub(t, base, "tok", "popos", "/home/dev/app",
		map[string]ServiceTarget{"app": {Host: "localhost", Port: 3000}})

	current := s.hubConfigSnapshot().cfg

	t.Run("domain change is refused", func(t *testing.T) {
		next := current
		next.Domain = "other.test"
		err := s.StartHub(next)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "publisher(s) are registered")
		assert.Contains(t, err.Error(), "prox hub stop")
		assert.Equal(t, "llt.test", s.hubStatus().Domain, "status must keep reporting what the routes use")
	})

	t.Run("port change is refused", func(t *testing.T) {
		next := current
		next.HTTPSPort = 17443
		err := s.StartHub(next)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "publisher(s) are registered")
	})

	t.Run("a listen-address change is still allowed", func(t *testing.T) {
		// The listen address appears in no route, so moving the control plane
		// does not invalidate anything a publisher registered.
		next := current
		next.Listen = "127.0.0.1:0"
		require.NoError(t, s.StartHub(next))
		assert.True(t, s.registry.HasProject(HubProjectKey("popos", "/home/dev/app")))
	})

	t.Run("and the change is allowed once the publishers are gone", func(t *testing.T) {
		s.removeProject(HubProjectKey("popos", "/home/dev/app"))
		next := s.hubConfigSnapshot().cfg
		next.Domain = "other.test"
		require.NoError(t, s.StartHub(next))
		assert.Equal(t, "other.test", s.hubStatus().Domain)
	})
}

// TestHubLifecycle_StartDoesNotOverwriteAConcurrentRotation is plan 031 F14.
//
// `hub start` reads the token, binds a listener, and writes the token into the
// running hub's state — a read-modify-write with I/O in the middle. Without one
// lifecycle mutex over start/stop/rotate, a rotation landing inside that window
// was silently overwritten with the pre-rotation value, so a credential the
// operator had just revoked kept working. The invariant asserted here is the
// one that matters: whatever the interleaving, the token the hub ACCEPTS is the
// token on disk.
func TestHubLifecycle_StartDoesNotOverwriteAConcurrentRotation(t *testing.T) {
	s, _ := newHubServer(t, HubConfig{Token: ""}) // real token, from the token file

	// Both loops start together (plan 031, review B8). Two goroutines each
	// racing away from `go` is not a race the test controls — the first can
	// easily finish its twenty iterations before the second is scheduled, and
	// then nothing ever interleaved. A start barrier makes the overlap the
	// point of the test rather than a hope about the scheduler.
	base := "http://" + s.HubListenAddr()
	start := make(chan struct{})
	// accepted collects every superseded token the hub was still willing to
	// accept AFTER a newer one had replaced it — the exact symptom of the bug.
	accepted := make(chan string, 64)
	rotations := 0

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 20; i++ {
			cfg := s.hubConfigSnapshot().cfg
			cfg.Token = "" // as `prox hub start` sends it: the daemon resolves the token
			_ = s.StartHub(cfg)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		prev := ""
		for i := 0; i < 20; i++ {
			rec := httptest.NewRecorder()
			s.router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hub/token", nil))
			var body HubTokenResponse
			if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Token == "" {
				continue
			}
			rotations++
			// D18: rotation has NO grace. The token that was just superseded
			// must be refused from the very next call — including when a
			// concurrent `hub start` read it a moment ago and is about to
			// commit it. This is the assertion the test was named for and did
			// not make (plan 031, review B8), and it has to happen DURING the
			// race: a stale token that is re-admitted for a few milliseconds
			// and then corrected is invisible to any check made at the end.
			if prev != "" && prev != body.Token {
				req, err := http.NewRequest(http.MethodGet, base+"/api/v1/routes", nil)
				if err == nil {
					req.Header.Set("Authorization", "Bearer "+prev)
					resp, err := hubClient().Do(req)
					if err == nil {
						if resp.StatusCode == http.StatusOK {
							accepted <- prev
						}
						resp.Body.Close()
					}
				}
			}
			prev = body.Token
		}
	}()
	close(start)
	wg.Wait()
	close(accepted)

	var stale []string
	for token := range accepted {
		stale = append(stale, token)
	}
	assert.Empty(t, stale,
		"a rotated-away token must be refused immediately, even when a concurrent hub start had already read it")
	assert.GreaterOrEqual(t, rotations, 2, "the rotation loop must actually have rotated")

	onDisk, err := ReadHubToken()
	require.NoError(t, err)
	resp := hubDo(t, http.MethodGet, base+"/api/v1/routes", onDisk, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"the hub must accept the token that is on disk, whatever order start and rotate ran in")
}

// TestHubRebind_ClosesTheReplacedServerAndKeepsTunnels is plan 031 F15.
//
// A rebind shuts the replaced server down gracefully, and graceful means
// "waits for in-flight handlers" — which for this mount includes an SSE
// subscription a publisher holds open indefinitely. The Shutdown therefore
// reliably times out, and discarding that error left the replaced server's
// handlers and connections alive for as long as the subscriber cared to hold
// them. Close ends them.
//
// The second half is what must NOT break: net/http neither tracks nor closes
// HIJACKED connections in Shutdown or Close, so the yamux tunnels attached
// through the replaced server survive the rebind — a publisher does not have to
// reconnect because the operator moved the control plane.
func TestHubRebind_ClosesTheReplacedServerAndKeepsTunnels(t *testing.T) {
	h := newTunnelHub(t)
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "still here")
	})
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	key, _ := h.startPublisher(t, "shed", "/home/dev/app", services, services)
	session := h.server.tunnels.get(key)
	require.NotNil(t, session)

	// A long-lived SSE subscription against the CURRENT control plane, which is
	// exactly what makes the replaced server's graceful shutdown time out.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
		h.baseURL+"/api/v1/requests/stream?origin=shed&project=%2Fhome%2Fdev%2Fapp", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.token)
	streamClient := &http.Client{}
	streamResp, err := streamClient.Do(req)
	require.NoError(t, err)
	defer streamResp.Body.Close()
	require.Equal(t, http.StatusOK, streamResp.StatusCode)

	// Rebind the control plane onto a new ephemeral port.
	oldAddr := h.server.HubListenAddr()
	cfg := h.server.hubConfigSnapshot().cfg
	// A concrete port, not ":0": the running hub's CONFIGURED address is
	// "127.0.0.1:0", and StartHub treats an unchanged configured address as a
	// reconfigure-in-place rather than a rebind.
	cfg.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	start := time.Now()
	require.NoError(t, h.server.StartHub(cfg))
	newAddr := h.server.HubListenAddr()
	require.NotEqual(t, oldAddr, newAddr)

	// The replaced server's shutdown is bounded and then forced, so the SSE
	// handler's connection ends rather than living on.
	//
	// The read happens on its own goroutine, and the BOUND is this select
	// (plan 031, review B8). require.Eventually cannot bound a condition
	// function that blocks: Body.Read on a stream nobody is closing returns
	// only when the connection ends, so an Eventually around it would wait
	// forever — a test that could hang but never fail.
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			if _, rerr := streamResp.Body.Read(buf); rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()
	select {
	case err := <-readErr:
		require.Error(t, err)
	case <-time.After(constants.HubShutdownGrace + 10*time.Second):
		t.Fatal("the replaced server never closed its SSE handler: the forced Close after the grace did not happen")
	}
	assert.Less(t, time.Since(start), constants.HubShutdownGrace+15*time.Second)

	// And the tunnel attached through the replaced server is untouched: same
	// session, still serving.
	assert.Same(t, session, h.server.tunnels.get(key), "a rebind must not drop an attached tunnel")
	assert.False(t, session.sess.IsClosed())
	status, body, _ := h.get(t, h.httpsClient(), "web", "/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "still here", body)
}

// errorCodeFromRecorder decodes an ErrorResponse recorded by an httptest
// recorder and returns its Code.
func errorCodeFromRecorder(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &er))
	return er.Code
}
