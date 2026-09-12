package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// Client communicates with a proxy daemon: over its Unix socket (NewClient) or
// over TCP against a hub daemon's network control plane (NewHTTPClient). The
// transport and the base URL are the only difference between the two — every
// method below is written against c.baseURL and works over either.
type Client struct {
	// baseURL is the scheme://host[/path] prefix every request is built
	// against. For the Unix-socket client it is the dummy "http://proxyd"
	// (the socket transport ignores the host); for a hub client it is the
	// hub's URL, validated and trailing-slash-normalized at construction.
	baseURL string

	// token, when non-empty, is sent as "Authorization: Bearer <token>" on
	// every request. It is applied in newRequest — the single request-building
	// helper every method funnels through — so no method can forget it.
	token string

	// origin is this client's own machine name when it is talking to a hub's
	// NETWORK mount, and empty for the Unix socket. It is what lets the
	// capture calls (Requests, and the forwarder's SSE subscription) send the
	// origin/project pair the network mount requires instead of the composed
	// key the socket mount takes: D15 has the hub compose <origin>:<dir>
	// itself and never accept a pre-qualified key from the wire.
	//
	// Holding it on the client rather than threading it through every call
	// site is what keeps `ForwardRequestsWithClient` one function with one
	// projectKey argument on both mounts — see scopedRequestQuery.
	origin string

	// unary and stream are TWO http.Clients deliberately sharing ONE
	// http.Transport (connection pool, dialer, TLS config).
	//
	// DO NOT COLLAPSE THEM INTO ONE CLIENT. http.Client.Timeout is a
	// WHOLE-REQUEST bound that covers reading the response body, not just the
	// handshake and headers:
	//
	//   - unary carries constants.HubUnaryTimeout (30s, historically the
	//     NewClient timeout) and is correct for the small JSON calls —
	//     register, deregister, status, routes, requests, shutdown, health.
	//   - stream carries Timeout: 0 (unbounded) and is the ONLY client that
	//     may carry a long-lived response body: the SSE request subscription
	//     and, later, the hub tunnel upgrade. Routing those through unary
	//     would silently cut every subscription at 30 seconds and look like a
	//     flapping daemon; the forwarder used to build its own timeout-free
	//     client for exactly this reason (plan 031 P1/D16).
	//
	// Callers bound a stream with the request context instead, which is what
	// the forwarder's per-attempt context already does.
	//
	// Both refuse redirects (CheckRedirect returns http.ErrUseLastResponse) so
	// a bearer token can never be replayed to another origin by a 30x from a
	// compromised or misconfigured hub (D16/P9). The daemon never redirects,
	// so this changes nothing on the socket path.
	unary  *http.Client
	stream *http.Client
}

// DaemonAPIError is returned by Client methods when the daemon responds with a
// non-2xx status and a decodable ErrorResponse body. It carries the daemon's
// Code (e.g. "SHUTTING_DOWN", "VERSION_MISMATCH") so callers can drive
// recovery via errors.As instead of matching on message text — following the
// ProjectConflictError precedent (registry.go). Error() reproduces the plain
// message text callers previously saw from readError, so existing
// message-only handling keeps working unchanged.
type DaemonAPIError struct {
	// Code is the daemon's machine-readable error code (ErrorResponse.Code).
	// Empty when the daemon's body didn't include one.
	Code string
	// Message is the daemon's human-readable error text (ErrorResponse.Error).
	Message string
	// Status is the HTTP status code the daemon responded with.
	Status int
	// Holders carries every conflicting holder on a 409 HUB_NAME_HELD (plan 031
	// D10/§4.3) and is nil on every other error. It is decoded here rather than
	// left in the body because by the time `prox up` decides whether to prompt
	// for a takeover the body is long gone — and the prompt has to name the
	// holders, not just say that there were some.
	Holders []HubHolder
	// HubProtocol is the hub's own protocol version on a 409 PROTOCOL_MISMATCH,
	// and 0 otherwise. `prox status` renders it as "protocol mismatch: hub 2,
	// this prox 1"; parsing it back out of the message text would be a second,
	// silently-drifting copy of the wording.
	HubProtocol int
}

func (e *DaemonAPIError) Error() string {
	return e.Message
}

// NewClient creates a new daemon client that connects via Unix socket. The
// socket path lives in the transport's dialer closure; the base URL is the
// dummy "http://proxyd" the Unix transport ignores.
func NewClient(socketPath string) *Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return newClientOverTransport("http://proxyd", "", transport, constants.HubUnaryTimeout)
}

// NewHTTPClient creates a client for a hub daemon's network control plane over
// TCP. token (may be empty, for a hub configured `auth: none`) is sent as a
// bearer credential on every request.
//
// Unlike NewClient it returns an error, because a hub URL comes from user
// configuration and must be validated before a credential is ever attached to
// it: a query, fragment, or userinfo component, a non-http(s) scheme, or a
// missing host is rejected here rather than producing a confusing request
// later. A trailing slash is normalized away so baseURL+path never doubles it.
func NewHTTPClient(baseURL, token string) (*Client, error) {
	return newHubClient(baseURL, token, "", constants.HubUnaryTimeout, constants.HubResponseHeaderTimeout)
}

// NewHubClient is NewHTTPClient plus the caller's own origin, which scopes the
// capture calls (Requests and the forwarder's SSE subscription) to this
// publisher's own registrations (plan 031 D15). A publisher that means to read
// its own captured traffic through a hub must use this constructor; an empty
// origin behaves exactly like NewHTTPClient.
func NewHubClient(baseURL, token, origin string) (*Client, error) {
	return newHubClient(baseURL, token, origin, constants.HubUnaryTimeout, constants.HubResponseHeaderTimeout)
}

// newHubClient is NewHTTPClient with injectable timeouts. Tests use it to
// exercise the bounded/unbounded client split and the black-hole header
// timeout in milliseconds instead of waiting out the real constants;
// production always passes them.
func newHubClient(baseURL, token, origin string, unaryTimeout, responseHeaderTimeout time.Duration) (*Client, error) {
	normalized, err := normalizeHubBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	// An explicit transport rather than a clone of http.DefaultTransport:
	// supplying DialContext also keeps Go from auto-negotiating HTTP/2 over
	// TLS (ForceAttemptHTTP2 stays false), which matters because the hub
	// tunnel is an HTTP/1.1 Connection: Upgrade handshake that h2 cannot
	// carry.
	transport := &http.Transport{
		// Proxy is deliberately NIL — hub control traffic never goes through an
		// ambient forward proxy. DO NOT "fix" this back to
		// http.ProxyFromEnvironment, which is the usual default and is wrong
		// here: a hub URL is plain HTTP on a private/tailnet address (D6 refuses
		// a public listen address outright), so an HTTP_PROXY set for ordinary
		// internet access would receive the ENTIRE request — including the
		// "Authorization: Bearer <hub token>" header — and the token would leave
		// the tailnet to a third party. Refusing redirects does not help,
		// because the proxy sees the request before any response exists. A
		// forward proxy is never the right path to a hub (plan 031 D6/D16).
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// ResponseHeaderTimeout bounds ESTABLISHMENT, not the body, which is
		// what makes it safe to put on the transport the unbounded stream
		// client shares: it is the only thing that fails a subscription to a
		// hub that accepts the TCP connection and then never answers. See
		// constants.HubResponseHeaderTimeout.
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	c := newClientOverTransport(normalized, token, transport, unaryTimeout)
	c.origin = origin
	return c, nil
}

// newClientOverTransport builds the two-client-over-one-transport pair the
// Client struct documents: a bounded client for unary JSON calls and an
// unbounded one for long-lived streams, both refusing redirects.
//
// It takes the http.RoundTripper INTERFACE rather than *http.Transport because
// that is all either client needs, and because it lets a test drive a Client
// over a stub round-tripper — asserting what leaves the process (a proxy that
// must not be consulted, a header that must be present) without a real socket.
func newClientOverTransport(baseURL, token string, transport http.RoundTripper, unaryTimeout time.Duration) *Client {
	// A bearer token must never be replayed to whatever origin a 30x names, so
	// redirects are not followed at all: the caller sees the 30x response
	// itself (D16/P9).
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		unary: &http.Client{
			Transport:     transport,
			Timeout:       unaryTimeout,
			CheckRedirect: noRedirect,
		},
		stream: &http.Client{
			Transport: transport,
			// Timeout: 0 — unbounded ON PURPOSE. See the struct comment.
			CheckRedirect: noRedirect,
		},
	}
}

// normalizeHubBaseURL validates a configured hub URL and returns its canonical
// scheme://host[/path] form.
//
// The rule lives in domain.NormalizeHubURL so that internal/config enforces the
// identical one when it validates a project's hubs: block and when `prox hub
// add` writes ~/.prox/hubs.yaml — the two packages cannot import each other, so
// a copy in each would be two rules that drift (plan 031 D8/D16). This wrapper
// only names the offender, since the domain messages are written to follow a
// caller-supplied subject.
func normalizeHubBaseURL(raw string) (string, error) {
	normalized, err := domain.NormalizeHubURL(raw)
	if err != nil {
		return "", fmt.Errorf("hub url %q %w", raw, err)
	}
	return normalized, nil
}

// Health checks if the daemon is alive and returns its version.
func (c *Client) Health() (string, error) {
	return c.HealthWithContext(context.Background())
}

// HealthWithContext is Health bounded by ctx, so a caller can probe a possibly
// unresponsive daemon with a short timeout instead of waiting out the client's
// 30s default (used by the version-skew drain poll in D1).
func (c *Client) HealthWithContext(ctx context.Context) (string, error) {
	resp, err := c.getWithContext(ctx, "/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon health check returned %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding health response: %w", err)
	}
	return result["version"], nil
}

// Register registers a project's routes with the daemon. On a non-2xx
// response it returns a *DaemonAPIError (errors.As) carrying the daemon's
// error Code — e.g. "SHUTTING_DOWN" during the daemon's graceful-shutdown
// grace (D4 retries this in tryDaemonProxy) or "VERSION_MISMATCH".
func (c *Client) Register(req RegisterRequest) (*RegisterResponse, error) {
	return c.RegisterWithContext(context.Background(), req)
}

// RegisterWithContext is Register bounded by ctx. `prox up`'s FIRST hub
// register uses it with a constants.HubConnectTimeout context so a black-holed
// hub cannot stall startup (plan 031 D16/AC11); every later attempt is owned by
// the tunnel's own retry loop and is bounded by the run's context instead.
func (c *Client) RegisterWithContext(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	resp, err := c.postWithContext(ctx, "/api/v1/register", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var result RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding register response: %w", err)
	}
	return &result, nil
}

// Deregister removes a project's routes from the daemon.
func (c *Client) Deregister(req DeregisterRequest) error {
	return c.DeregisterWithContext(context.Background(), req)
}

// DeregisterWithContext is Deregister bounded by ctx, so a teardown stage can
// cap how long it waits for a hub that may itself be gone (plan 031 C5).
func (c *Client) DeregisterWithContext(ctx context.Context, req DeregisterRequest) error {
	resp, err := c.postWithContext(ctx, "/api/v1/deregister", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.readError(resp)
	}
	return nil
}

// Status returns the daemon's current status.
func (c *Client) Status() (*DaemonStatusResponse, error) {
	return c.StatusWithContext(context.Background())
}

// StatusWithContext is Status bounded by ctx. The version-skew recovery path
// probes a possibly-draining old daemon with a short timeout (D1) rather than
// the client's 30s default.
func (c *Client) StatusWithContext(ctx context.Context) (*DaemonStatusResponse, error) {
	resp, err := c.getWithContext(ctx, "/api/v1/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var result DaemonStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding status response: %w", err)
	}
	return &result, nil
}

// Routes returns all registered routes.
func (c *Client) Routes() ([]RouteInfo, error) {
	resp, err := c.get("/api/v1/routes")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var result struct {
		Routes []RouteInfo `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding routes response: %w", err)
	}
	return result.Routes, nil
}

// Requests fetches a snapshot of the daemon's recent records for a project over
// the Unix socket (GET /api/v1/requests?project=<dir>&limit=<n>), decoding the
// {"requests":[...]} wrapper. The daemon returns records newest-first.
//
// The decode is all-or-nothing: the entire response body is read and
// unmarshalled into a slice before any record is returned, so a truncated or
// malformed body yields (nil, error) rather than a partial set — a caller
// replaying the result into a RequestManager applies zero records on failure.
//
// ctx bounds the fetch: the forwarder passes its own context so a shutdown or
// reconnect cancels an in-flight snapshot instead of waiting out the client's
// 30s timeout. limit must be supplied explicitly (an omitted limit backfills
// only the daemon's default of 100).
func (c *Client) Requests(ctx context.Context, projectKey string, limit int) ([]proxy.RequestRecord, error) {
	q := c.scopedRequestQuery(projectKey)
	q.Set("limit", strconv.Itoa(limit))

	resp, err := c.getWithContext(ctx, "/api/v1/requests?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	// Stream-decode into the wrapper (no io.ReadAll: a full snapshot's raw
	// JSON alongside its decoded records would double peak memory). The
	// all-or-nothing contract holds: Decode either fully unmarshals the
	// wrapper or errors, and on error no records are returned; a truncated
	// response fails the decode rather than yielding a partial slice.
	var result struct {
		Requests []proxy.RequestRecord `json:"requests"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding requests response: %w", err)
	}
	// Reject trailing non-whitespace data — a concatenated or garbled
	// response should not pass as a valid snapshot.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after requests response")
	}
	// The daemon always emits a non-nil slice ("requests":[] when empty), so a
	// nil slice means valid JSON of the wrong shape ({} or a misspelled key) —
	// treat it as malformed rather than a silent empty backfill.
	if result.Requests == nil {
		return nil, fmt.Errorf("snapshot response missing requests key")
	}
	return result.Requests, nil
}

// scopedRequestQuery builds the identity half of the capture query
// (/api/v1/requests and /api/v1/requests/stream) for projectKey, so the same
// key works on both mounts (plan 031 D15).
//
// On the Unix socket the key IS the project dir and goes out as ?project=. On a
// hub's network mount the key is the composed HubProjectKey(origin, dir), and
// the hub refuses to accept a pre-qualified key: it takes ?origin= and
// ?project= and composes the key itself. So the two halves are split back out
// here, and only when the key's origin is this client's OWN origin, which by
// construction it always is (the same two strings built the key and the
// client).
//
// What that composition buys on the server side is accident-proofing — an
// honest caller reaches its own records and no caller can name a LOCAL
// project — not authentication of the origin; see validateHubOrigin.
func (c *Client) scopedRequestQuery(projectKey string) url.Values {
	q := url.Values{}
	if c.origin != "" {
		if origin, dir, ok := splitHubProjectKey(projectKey); ok && origin == c.origin {
			q.Set("origin", origin)
			q.Set("project", dir)
			return q
		}
	}
	q.Set("project", projectKey)
	return q
}

// Shutdown requests the daemon to shut down.
func (c *Client) Shutdown(force bool) error {
	path := "/api/v1/shutdown"
	if force {
		path += "?force=true"
	}
	resp, err := c.post(path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.readError(resp)
	}
	return nil
}

// HubStart asks the daemon to turn hub mode on (or reconfigure it) and returns
// the resulting status, whose Listen is the address actually bound (plan 031
// D13/D14). Socket-only: the network mount carries no hub/* routes at all.
//
// A nil cfg means "start from ~/.prox/hub.yaml as it stands". A non-nil cfg
// PROPOSES a configuration, and the daemon persists it only after it has bound
// it (plan 031 F13) — which is why the CLI hands the config over instead of
// writing the file itself and hoping the bind that follows succeeds.
func (c *Client) HubStart(cfg *HubConfig) (*HubStatus, error) {
	return c.hubCall(http.MethodPost, "/api/v1/hub/start", HubStartRequest{Config: cfg})
}

// HubStop asks the daemon to close the network control plane, drop every remote
// registration, and re-arm the ordinary empty-daemon shutdown check.
func (c *Client) HubStop() (*HubStatus, error) {
	return c.hubCall(http.MethodPost, "/api/v1/hub/stop", nil)
}

// HubStatus returns the hub HOST's view (D20). Enabled is false when hub mode
// is off; that is a normal answer, not an error.
func (c *Client) HubStatus() (*HubStatus, error) {
	return c.hubCall(http.MethodGet, "/api/v1/hub/status", nil)
}

// HubRotateToken makes the daemon write a fresh token and accept ONLY it, with
// no grace (D18), returning the new value and the file it landed in.
func (c *Client) HubRotateToken() (*HubTokenResponse, error) {
	resp, err := c.post("/api/v1/hub/token", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}
	var result HubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding hub token response: %w", err)
	}
	return &result, nil
}

// hubCall is the shared request/decode body of the three hub/* endpoints that
// answer with a HubStatus.
func (c *Client) hubCall(method, path string, body any) (*HubStatus, error) {
	var resp *http.Response
	var err error
	if method == http.MethodGet {
		resp, err = c.get(path)
	} else {
		resp, err = c.post(path, body)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}
	var result HubStatus
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding hub status response: %w", err)
	}
	return &result, nil
}

// Stream issues a GET on the UNBOUNDED client and returns the live response
// without reading it, so the caller owns the body (and MUST close it). This is
// the only way to open a response that outlives constants.HubUnaryTimeout: the
// SSE request subscription and, later, the hub tunnel upgrade.
//
// ctx is the stream's real bound — cancelling it tears the response down —
// because the client itself imposes none. See the Client struct comment for
// why routing these through the unary client would be a silent 30s cap.
func (c *Client) Stream(ctx context.Context, path string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return c.stream.Do(req)
}

// streamUpgrade issues a request with caller-supplied method and headers on the
// UNBOUNDED client and returns the live response WITHOUT reading it, so the
// caller owns the body (and MUST close it). It is the hub tunnel's HTTP/1.1
// upgrade handshake: on a 101 the response body IS the raw connection, which
// net/http hands back as an io.ReadWriteCloser.
//
// It must use the unbounded client for the same reason Stream does: a
// whole-request timeout covers reading the body, so the bounded client would
// sever every tunnel after constants.HubUnaryTimeout (plan 031 P1/D16). ctx is
// the only bound, which is what lets a publisher's shutdown tear the tunnel
// down promptly.
func (c *Client) streamUpgrade(ctx context.Context, method, path string, headers http.Header) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	return c.stream.Do(req)
}

// newRequest builds a request against c.baseURL and attaches the bearer token
// when one is configured. EVERY request this client issues is built here (get,
// getWithContext, post, and Stream all funnel through it), so a method cannot
// forget the Authorization header on the network mount.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// get performs an HTTP GET to the daemon on the bounded (unary) client.
func (c *Client) get(path string) (*http.Response, error) {
	return c.getWithContext(context.Background(), path)
}

// getWithContext performs an HTTP GET to the daemon bound to ctx, so a caller
// can cancel the request (e.g. on shutdown) without waiting out the client's
// timeout. The existing get helper is left ctx-less for its callers.
func (c *Client) getWithContext(ctx context.Context, path string) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return c.unary.Do(req)
}

// post performs an HTTP POST to the daemon with a JSON body, on the bounded
// (unary) client.
func (c *Client) post(path string, body any) (*http.Response, error) {
	return c.postWithContext(context.Background(), path, body)
}

// postWithContext is post bounded by ctx, so a caller can cap an individual
// call below the client's own unary timeout (the hub's first register) or
// abandon one on teardown.
func (c *Client) postWithContext(ctx context.Context, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := c.newRequest(ctx, http.MethodPost, path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.unary.Do(req)
}

// readError reads an error response from the daemon. When the body decodes to
// an ErrorResponse with a non-empty Error field, it returns a *DaemonAPIError
// (matched with errors.As by callers, e.g. the D4 SHUTTING_DOWN retry) rather
// than a plain error, so the daemon's Code survives past this call. Its
// Error() text is identical to what callers previously received.
func (c *Client) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)

	var errResp ErrorResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return &DaemonAPIError{
			Code:        errResp.Code,
			Message:     errResp.Error,
			Status:      resp.StatusCode,
			Holders:     errResp.Holders,
			HubProtocol: errResp.HubProtocol,
		}
	}
	return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(body))
}
