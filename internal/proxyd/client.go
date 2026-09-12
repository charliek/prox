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
	"strings"
	"time"

	"github.com/charliek/prox/internal/constants"
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
	return newHubClient(baseURL, token, constants.HubUnaryTimeout)
}

// newHubClient is NewHTTPClient with an injectable unary timeout. Tests use it
// to exercise the bounded/unbounded client split in milliseconds instead of
// waiting out the real 30s HubUnaryTimeout; production always passes the
// constant.
func newHubClient(baseURL, token string, unaryTimeout time.Duration) (*Client, error) {
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
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return newClientOverTransport(normalized, token, transport, unaryTimeout), nil
}

// newClientOverTransport builds the two-client-over-one-transport pair the
// Client struct documents: a bounded client for unary JSON calls and an
// unbounded one for long-lived streams, both refusing redirects.
func newClientOverTransport(baseURL, token string, transport *http.Transport, unaryTimeout time.Duration) *Client {
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
// scheme://host[/path] form with any trailing slash removed.
//
// The rejections are deliberate rather than tolerant: a query or fragment
// would be silently dropped once a method appends its own path and query, and
// userinfo would put a second credential on a request that already carries a
// bearer token. Failing at construction turns each into a config error the
// user can see instead of a request that quietly goes somewhere else.
func normalizeHubBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("hub url is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing hub url %q: %w", raw, err)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("hub url %q must be an absolute http:// or https:// url", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("hub url %q must use http:// or https:// (got %q)", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("hub url %q has no host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("hub url %q must not contain userinfo (use a token instead)", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("hub url %q must not contain a query string", raw)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("hub url %q must not contain a fragment", raw)
	}

	// Normalize the trailing slash (and any run of them) so callers can append
	// "/api/v1/..." without producing a doubled separator.
	path := strings.TrimRight(u.EscapedPath(), "/")
	return u.Scheme + "://" + u.Host + path, nil
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
	resp, err := c.post("/api/v1/register", req)
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
	resp, err := c.post("/api/v1/deregister", req)
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
func (c *Client) Requests(ctx context.Context, projectDir string, limit int) ([]proxy.RequestRecord, error) {
	q := url.Values{}
	q.Set("project", projectDir)
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
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := c.newRequest(context.Background(), http.MethodPost, path, bodyReader)
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
		return &DaemonAPIError{Code: errResp.Code, Message: errResp.Error, Status: resp.StatusCode}
	}
	return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(body))
}
