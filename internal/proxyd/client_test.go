package proxyd

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- transport shape (plan 031 C1, P1/D16) ---

// TestClient_TwoClientsShareOneTransport pins the structural invariant the
// Client struct comment spells out: the bounded (unary) and unbounded (stream)
// http.Clients are DISTINCT clients sharing ONE transport. Collapsing them —
// the mistake this commit exists to prevent — would either cap every SSE
// subscription at HubUnaryTimeout or leave every unary call unbounded.
func TestClient_TwoClientsShareOneTransport(t *testing.T) {
	socketClient := NewClient("/tmp/does-not-need-to-exist.sock")
	hubClient, err := NewHTTPClient("http://127.0.0.1:8443", "")
	require.NoError(t, err)

	for name, c := range map[string]*Client{"unix": socketClient, "http": hubClient} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, c.unary)
			require.NotNil(t, c.stream)
			assert.NotSame(t, c.unary, c.stream, "unary and stream must be separate http.Clients")
			assert.Same(t, c.unary.Transport, c.stream.Transport,
				"both clients must share ONE transport (one connection pool, one dialer)")

			assert.Equal(t, constants.HubUnaryTimeout, c.unary.Timeout,
				"unary calls keep the historical 30s whole-request bound")
			assert.Zero(t, c.stream.Timeout,
				"the stream client MUST be unbounded: http.Client.Timeout covers reading the body")

			// Neither client may follow a redirect; see the token-replay test below.
			require.NotNil(t, c.unary.CheckRedirect)
			require.NotNil(t, c.stream.CheckRedirect)
			assert.Equal(t, http.ErrUseLastResponse, c.unary.CheckRedirect(nil, nil))
			assert.Equal(t, http.ErrUseLastResponse, c.stream.CheckRedirect(nil, nil))
		})
	}
}

// TestNewClient_BaseURLIsTheSocketDummyHost pins that the Unix-socket client
// still builds its requests against "http://proxyd" — the dummy host the socket
// transport ignores — now that the value lives in a field rather than in each
// helper's string concatenation.
func TestNewClient_BaseURLIsTheSocketDummyHost(t *testing.T) {
	c := NewClient("/tmp/does-not-need-to-exist.sock")
	assert.Equal(t, "http://proxyd", c.baseURL)
	assert.Empty(t, c.token, "the socket client never carries a bearer token")
}

// --- NewHTTPClient: bearer token and request paths ---

// recordedRequest is one request a test hub server observed.
type recordedRequest struct {
	method string
	path   string
	query  string
	auth   string
}

// startRecordingHub serves the handful of endpoints the client methods below
// call, recording each request's method, path, query, and Authorization header.
func startRecordingHub(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()

	var mu sync.Mutex
	var seen []recordedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			auth:   r.Header.Get("Authorization"),
		})
		mu.Unlock()

		switch r.URL.Path {
		case "/api/v1/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"test","pid":1}`)
		case "/api/v1/register":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"registered":["auth.llt.example"]}`)
		case "/api/v1/requests":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"requests":[]}`)
		case "/api/v1/requests/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": connected\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// TestNewHTTPClient_AuthorizationHeaderAndPaths pins that every request an HTTP
// (hub) client issues carries "Authorization: Bearer <token>" when a token is
// configured and NO Authorization header at all when it is empty (a hub running
// `auth: none`), and that the paths are appended to the configured base URL —
// on the unary helpers (GET and POST alike) and on Stream.
func TestNewHTTPClient_AuthorizationHeaderAndPaths(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		wantAuth string
	}{
		{name: "token set sends a bearer credential", token: "s3cr3t-token", wantAuth: "Bearer s3cr3t-token"},
		{name: "empty token sends no header", token: "", wantAuth: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, recorded := startRecordingHub(t)

			c, err := NewHTTPClient(srv.URL, tc.token)
			require.NoError(t, err)

			// A unary GET.
			_, err = c.Status()
			require.NoError(t, err)

			// A unary POST with a JSON body.
			_, err = c.Register(RegisterRequest{ProjectDir: "/p", PID: 1})
			require.NoError(t, err)

			// A unary GET carrying a query string.
			_, err = c.Requests(context.Background(), "/p", 10)
			require.NoError(t, err)

			// The unbounded stream client.
			resp, err := c.Stream(context.Background(), "/api/v1/requests/stream?project=%2Fp")
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())

			got := recorded()
			require.Len(t, got, 4)

			assert.Equal(t, http.MethodGet, got[0].method)
			assert.Equal(t, "/api/v1/status", got[0].path)

			assert.Equal(t, http.MethodPost, got[1].method)
			assert.Equal(t, "/api/v1/register", got[1].path)

			assert.Equal(t, http.MethodGet, got[2].method)
			assert.Equal(t, "/api/v1/requests", got[2].path)
			assert.Equal(t, "limit=10&project=%2Fp", got[2].query)

			assert.Equal(t, http.MethodGet, got[3].method)
			assert.Equal(t, "/api/v1/requests/stream", got[3].path)
			assert.Equal(t, "project=%2Fp", got[3].query)

			for i, r := range got {
				assert.Equal(t, tc.wantAuth, r.auth,
					"request %d (%s %s) carried the wrong Authorization header", i, r.method, r.path)
			}
		})
	}
}

// TestNewHTTPClient_DoesNotFollowRedirects pins D16/P9: a bearer token must
// never be replayed to whatever origin a 30x names. The client does not follow
// the redirect AT ALL — it hands the 302 back to the caller — so the second
// origin sees no request and therefore no credential. Both the bounded and the
// unbounded client are covered, since the tunnel upgrade will use the latter.
func TestNewHTTPClient_DoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var secondHits int
	var secondAuths []string

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		secondHits++
		secondAuths = append(secondAuths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"other-origin"}`)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+r.URL.Path, http.StatusFound)
	}))
	defer first.Close()

	c, err := NewHTTPClient(first.URL, "do-not-leak-me")
	require.NoError(t, err)

	t.Run("unary", func(t *testing.T) {
		resp, err := c.getWithContext(context.Background(), "/api/v1/status")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusFound, resp.StatusCode,
			"the 302 must come back to the caller rather than being followed")
		assert.Equal(t, second.URL+"/api/v1/status", resp.Header.Get("Location"))
	})

	t.Run("stream", func(t *testing.T) {
		resp, err := c.Stream(context.Background(), "/api/v1/requests/stream?project=%2Fp")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusFound, resp.StatusCode)
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, secondHits, "the redirect target must never be contacted")
	assert.Empty(t, secondAuths, "the bearer token must never reach a second origin")
}

// --- base URL validation ---

// TestNormalizeHubBaseURL tables the URLs NewHTTPClient accepts (with their
// canonical form) and the ones it refuses at construction. The refusals exist
// so a misconfigured hub URL is a config error the user sees rather than a
// request that quietly drops its query, or a second credential riding along in
// userinfo next to the bearer token.
func TestNormalizeHubBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "plain host and port", in: "http://100.120.127.126:8443", want: "http://100.120.127.126:8443"},
		{name: "https", in: "https://hub.example", want: "https://hub.example"},
		{name: "trailing slash normalized", in: "http://hub.example:8443/", want: "http://hub.example:8443"},
		{name: "repeated trailing slashes normalized", in: "http://hub.example:8443///", want: "http://hub.example:8443"},
		{name: "base path kept, trailing slash dropped", in: "http://hub.example/prox/", want: "http://hub.example/prox"},
		{name: "surrounding whitespace trimmed", in: "  http://hub.example:8443 ", want: "http://hub.example:8443"},
		{name: "bracketed IPv6 literal", in: "http://[fd00::1]:8443", want: "http://[fd00::1]:8443"},

		{name: "empty", in: "", wantErr: "hub url is empty"},
		{name: "whitespace only", in: "   ", wantErr: "hub url is empty"},
		// "hub.example:8443" parses as an OPAQUE url (scheme "hub.example"),
		// which is the shape a user produces by pasting a host:port with no
		// scheme — caught by the opaque guard rather than the scheme guard.
		{name: "no scheme", in: "hub.example:8443", wantErr: "must be an absolute http:// or https:// url"},
		{name: "wrong scheme", in: "ftp://hub.example", wantErr: "must use http:// or https://"},
		{name: "unix scheme", in: "unix:///tmp/proxy.sock", wantErr: "must use http:// or https://"},
		{name: "no host", in: "http:///api", wantErr: "has no host"},
		{name: "userinfo", in: "http://user:pw@hub.example", wantErr: "must not contain userinfo"},
		{name: "query", in: "http://hub.example?token=leak", wantErr: "must not contain a query string"},
		{name: "empty forced query", in: "http://hub.example/?", wantErr: "must not contain a query string"},
		{name: "fragment", in: "http://hub.example#frag", wantErr: "must not contain a fragment"},
		{name: "unparseable", in: "http://hub.example:not-a-port", wantErr: "parsing hub url"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeHubBaseURL(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				// The constructor must refuse the same inputs, so no client
				// (and therefore no credential) is ever built against them.
				c, cerr := NewHTTPClient(tc.in, "s3cr3t")
				require.Error(t, cerr)
				assert.Nil(t, c)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			c, cerr := NewHTTPClient(tc.in, "s3cr3t")
			require.NoError(t, cerr)
			require.NotNil(t, c)
			assert.Equal(t, tc.want, c.baseURL)
		})
	}
}

// --- THE load-bearing test (plan 031 P1) ---

// TestClientStream_OutlivesUnaryTimeoutOnTheSameClient is the test this commit
// exists for. http.Client.Timeout is a WHOLE-REQUEST bound that includes
// reading the response body, so a single shared client would silently cut every
// SSE subscription (and, later, every tunnel) at HubUnaryTimeout — which looks
// exactly like a flapping daemon rather than like a bug.
//
// It proves both halves at once, on ONE Client over ONE transport:
//   - a Stream response stays readable long after the unary timeout elapses;
//   - a unary call issued WHILE that stream is open still fails at its bound.
//
// The unary timeout is injected (newHubClient) so this runs in ~100ms rather
// than waiting out the real 30s constant.
func TestClientStream_OutlivesUnaryTimeoutOnTheSameClient(t *testing.T) {
	const unaryTimeout = 100 * time.Millisecond

	// release unblocks both handlers. The fallback deadline only exists so a
	// regression cannot wedge httptest.Server.Close() for the whole suite.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	waitForRelease := func() {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream":
			// A long-lived response: one line now, one line only after the
			// unary call below has already timed out.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "one\n")
			w.(http.Flusher).Flush()
			waitForRelease()
			_, _ = io.WriteString(w, "two\n")
			w.(http.Flusher).Flush()
		case "/slow":
			// A unary endpoint that never answers in time.
			waitForRelease()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"too-late"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := newHubClient(srv.URL, "", unaryTimeout)
	require.NoError(t, err)
	require.Equal(t, unaryTimeout, c.unary.Timeout)
	require.Zero(t, c.stream.Timeout)
	require.Same(t, c.unary.Transport, c.stream.Transport, "one transport, two clients")

	streamStart := time.Now()
	resp, err := c.Stream(context.Background(), "/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "one\n", line)

	// Half one: a unary call on the SAME client, while the stream is held open,
	// still hits the bounded client's timeout.
	unaryStart := time.Now()
	_, err = c.get("/slow")
	unaryElapsed := time.Since(unaryStart)
	require.Error(t, err, "a unary call must be bounded by the unary client's timeout")

	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
	assert.True(t, urlErr.Timeout(), "the unary failure must be a timeout, got %v", err)
	assert.GreaterOrEqual(t, unaryElapsed, unaryTimeout,
		"the unary call must not fail before its own timeout")
	assert.Less(t, unaryElapsed, 5*time.Second,
		"the unary call must fail at its timeout, not sit out the handler")

	// Half two: the stream is still alive after the unary bound elapsed — it is
	// only now, past that bound, that the server sends its second line.
	releaseAll()
	line, err = reader.ReadString('\n')
	require.NoError(t, err, "the stream must survive past the unary timeout")
	assert.Equal(t, "two\n", line)
	assert.Greater(t, time.Since(streamStart), unaryTimeout,
		"the stream outlived the unary timeout (sanity check on the timing)")
}
