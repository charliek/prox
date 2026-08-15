package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// deadlineReader wraps an io.Reader and sets a read deadline on each read.
// This prevents indefinite hangs when the server dies without closing the connection.
type deadlineReader struct {
	r       io.Reader
	conn    net.Conn
	timeout time.Duration
}

func (d *deadlineReader) Read(p []byte) (n int, err error) {
	if d.conn != nil {
		if err := d.conn.SetReadDeadline(time.Now().Add(d.timeout)); err != nil {
			return 0, err
		}
	}
	return d.r.Read(p)
}

// Client is an HTTP client for the prox API
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	// lifecycleClient is used for the start/stop/restart calls, which the
	// daemon may legitimately hold open for up to a configured stop budget
	// (capped at constants.MaxStopTimeout). Its timeout sits above that cap so
	// the client never aborts a valid long stop; the server is authoritative
	// and Ctrl-C on the CLI is safe (#35, D2).
	lifecycleClient *http.Client
}

// NewClient creates a new API client, reading the auth token from disk
// (~/.prox/token). This is the form every out-of-process CLI command wants: it
// has no other way to learn the daemon's token.
func NewClient(baseURL string) *Client {
	// Try to load token from file
	token, _ := loadToken() // Ignore error - token may not exist
	return NewClientWithToken(baseURL, token)
}

// NewClientWithToken creates a new API client against an explicitly supplied
// token, skipping the token file entirely. `prox up --tui` needs this: it talks
// to the API server it just started in this very process, and it already holds
// the token it minted (empty when auth is disabled). Re-reading ~/.prox/token
// there would be both pointless and wrong — the file is a single global slot, so
// another project's `prox up` starting concurrently would have overwritten it
// with a token this server does not accept.
func NewClientWithToken(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		lifecycleClient: &http.Client{
			Timeout: constants.LifecycleTimeoutCeiling,
		},
	}
}

// GetStatus gets supervisor status
func (c *Client) GetStatus() (*api.StatusResponse, error) {
	return c.GetStatusWithContext(context.Background())
}

// GetStatusWithContext is GetStatus with a caller-supplied context, following
// the same pattern as GetLogs/GetProxyRequests. The ownership probe on the
// discovery path (plan 020 C3) needs it: that probe runs before EVERY client
// command and must be bounded by constants.OwnershipProbeTimeout, not by the
// 30s httpClient timeout, so a wedged listener on the recorded port cannot hang
// the CLI. Sharing the Client (and therefore its token) with the real request
// that follows is deliberate — a 401 here guarantees a 401 there, which is why
// the probe passes auth failures through instead of refusing.
func (c *Client) GetStatusWithContext(ctx context.Context) (*api.StatusResponse, error) {
	var resp api.StatusResponse
	if err := c.getCtx(ctx, "/api/v1/status", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetProcesses gets all processes
func (c *Client) GetProcesses() (*api.ProcessListResponse, error) {
	var resp api.ProcessListResponse
	if err := c.get("/api/v1/processes", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetProcess gets a single process
func (c *Client) GetProcess(name string) (*api.ProcessDetailResponse, error) {
	var resp api.ProcessDetailResponse
	if err := c.get("/api/v1/processes/"+url.PathEscape(name), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StartProcess starts a process
func (c *Client) StartProcess(name string) error {
	var resp api.SuccessResponse
	return c.postLifecycle("/api/v1/processes/"+url.PathEscape(name)+"/start", &resp)
}

// StopProcess stops a process
func (c *Client) StopProcess(name string) error {
	var resp api.SuccessResponse
	return c.postLifecycle("/api/v1/processes/"+url.PathEscape(name)+"/stop", &resp)
}

// RestartProcess restarts a process
func (c *Client) RestartProcess(name string) error {
	var resp api.SuccessResponse
	return c.postLifecycle("/api/v1/processes/"+url.PathEscape(name)+"/restart", &resp)
}

// ShutdownFailure is one process whose group survived a waited full stop.
type ShutdownFailure struct {
	Process string `json:"process"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

// ShutdownResult decodes a POST /api/v1/shutdown?wait=true response.
//
// Waited is a POINTER on purpose: an old daemon predates the wait param, ignores
// the query, and acks with a bare {"success":true} that has no "waited" field —
// leaving Waited nil. A nil Waited therefore means "old daemon, fire-and-forget",
// which is distinct from a false value (never sent on the wire). Callers MUST
// treat nil as the legacy path rather than as waited==false.
type ShutdownResult struct {
	Success  bool              `json:"success"`
	Waited   *bool             `json:"waited"`
	Failures []ShutdownFailure `json:"failures"`
}

// Shutdown requests a full daemon shutdown.
//
// With wait=true it posts /api/v1/shutdown?wait=true on the lifecycle client
// (11m ceiling), blocks until the daemon returns the process-stop verdict, and
// returns the decoded *ShutdownResult. With wait=false it posts the legacy async
// shutdown on the plain client and returns a nil result (the daemon acks
// immediately and tears down in the background).
func (c *Client) Shutdown(wait bool) (*ShutdownResult, error) {
	if !wait {
		var resp api.SuccessResponse
		return nil, c.post("/api/v1/shutdown", &resp)
	}

	var result ShutdownResult
	if err := c.postLifecycle("/api/v1/shutdown?wait=true", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// buildLogQueryParams builds URL query parameters from LogParams
func buildLogQueryParams(params domain.LogParams) url.Values {
	query := url.Values{}
	if params.Process != "" {
		query.Set("process", params.Process)
	}
	if params.Lines > 0 {
		query.Set("lines", fmt.Sprintf("%d", params.Lines))
	}
	if params.Pattern != "" {
		query.Set("pattern", params.Pattern)
	}
	if params.Regex {
		query.Set("regex", "true")
	}
	// since_seq is sent only when it names a real cursor. A zero SinceSeq means
	// the caller has consumed nothing, and sending `since_seq=0` would flip the
	// server onto its resume path (every entry in the buffer, oldest-first,
	// capped at `lines` from the OLDEST end) when what a cursorless caller wants
	// is the last-`lines` path. The two agree only when the buffer holds fewer
	// than `lines` entries, so the distinction is load-bearing (plan 017 C9).
	//
	// This builder is shared with logsStreamPath: the SSE endpoint ignores
	// since_seq today, and no stream subscription sets it.
	if params.SinceSeq > 0 {
		query.Set("since_seq", fmt.Sprintf("%d", params.SinceSeq))
	}
	return query
}

// buildProxyRequestQueryParams builds URL query parameters from ProxyRequestParams
func buildProxyRequestQueryParams(params domain.ProxyRequestParams) url.Values {
	query := url.Values{}
	if params.Subdomain != "" {
		query.Set("subdomain", params.Subdomain)
	}
	if params.Method != "" {
		query.Set("method", params.Method)
	}
	if params.MinStatus > 0 {
		query.Set("min_status", fmt.Sprintf("%d", params.MinStatus))
	}
	if params.MaxStatus > 0 {
		query.Set("max_status", fmt.Sprintf("%d", params.MaxStatus))
	}
	if !params.Since.IsZero() {
		query.Set("since", params.Since.Format(time.RFC3339Nano))
	}
	if params.URLContains != "" {
		query.Set("url_contains", params.URLContains)
	}
	if params.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	// before_id is sent only when the caller actually holds a pagination
	// cursor (domain.ProxyRequestParams.BeforeID). This builder is shared with
	// proxyRequestsStreamPath: the stream endpoint parses and ignores
	// before_id, and no stream subscription sets it (list-fetch use only).
	if params.BeforeID != "" {
		query.Set("before_id", params.BeforeID)
	}
	return query
}

// pathWithQuery appends query to path as a query string, leaving path
// untouched when there is nothing to encode.
func pathWithQuery(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

// GetLogs gets logs with optional filtering.
//
// It takes a context for the same reason GetProxyRequests does: the TUI's
// logs-stream sync calls it once per connect and must abandon an in-flight
// backfill the moment that stream attempt ends (tui.TUIClient, plan 017 C9).
func (c *Client) GetLogs(ctx context.Context, params domain.LogParams) (*api.LogsResponse, error) {
	path := pathWithQuery("/api/v1/logs", buildLogQueryParams(params))

	var resp api.LogsResponse
	if err := c.getCtx(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetProxyRequests gets recent proxy requests with optional filtering.
//
// It takes a context because the TUI's requests-stream sync calls it once per
// connect and must abandon an in-flight snapshot fetch the moment its stream
// attempt ends (tui.TUIClient).
func (c *Client) GetProxyRequests(ctx context.Context, params domain.ProxyRequestParams) (*api.ProxyRequestsResponse, error) {
	path := pathWithQuery("/api/v1/proxy/requests", buildProxyRequestQueryParams(params))

	var resp api.ProxyRequestsResponse
	if err := c.getCtx(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetProxyRequest gets a specific proxy request by ID
func (c *Client) GetProxyRequest(id string, includeBody bool) (*api.ProxyRequestDetailResponse, error) {
	path := "/api/v1/proxy/requests/" + url.PathEscape(id)
	if includeBody {
		path += "?include=body"
	}

	var resp api.ProxyRequestDetailResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// APIError is a non-2xx API response. Status is the HTTP status; Code is the
// machine-readable error code from the JSON error body (empty if the body
// wasn't parseable). Callers that need to discriminate on a specific failure
// (e.g. a 503 PROXY_NOT_ENABLED) use errors.As rather than string matching.
type APIError struct {
	Status  int
	Code    string
	Message string
}

// Error renders the daemon's own message when it sent one, and otherwise a
// status-specific fallback. The text is user-facing: CLI commands surface it
// verbatim through clientError.
func (e *APIError) Error() string {
	// Code is optional on the wire: a handler that writes {"error":"..."}
	// without a code must not render a bare ": msg" (CodeRabbit PR #87).
	if e.Message != "" && e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	if e.Message != "" {
		return e.Message
	}

	switch e.Status {
	case http.StatusUnauthorized:
		return "authentication failed: invalid or missing token"
	case http.StatusForbidden:
		return "access denied: insufficient permissions"
	case http.StatusNotFound:
		return "not found: the requested resource does not exist"
	case http.StatusInternalServerError:
		return "server error: the prox daemon encountered an internal error"
	case http.StatusServiceUnavailable:
		return "service unavailable: the prox daemon is not ready"
	default:
		return fmt.Sprintf("request failed with status %d", e.Status)
	}
}

// StatusCode and ErrorCode expose the two discriminating fields as methods so
// consumers that cannot name this type can still classify it structurally:
// internal/cli imports internal/tui, so the TUI's reconnect policies match on
// the tui.APIStatusError interface these satisfy rather than on *APIError.
func (e *APIError) StatusCode() int   { return e.Status }
func (e *APIError) ErrorCode() string { return e.Code }

// httpStatusError builds the *APIError for a non-2xx response. errResp is nil
// when the body was absent or unparseable.
func httpStatusError(statusCode int, errResp *api.ErrorResponse) *APIError {
	e := &APIError{Status: statusCode}
	if errResp != nil {
		e.Code = errResp.Code
		e.Message = errResp.Error
	}
	return e
}

func (c *Client) doRequest(method, path string, v interface{}) error {
	return c.doRequestWith(context.Background(), c.httpClient, method, path, v)
}

func (c *Client) doRequestWith(ctx context.Context, client *http.Client, method, path string, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	c.addAuthHeader(req)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp api.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return httpStatusError(resp.StatusCode, &errResp)
		}
		return httpStatusError(resp.StatusCode, nil)
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) get(path string, v interface{}) error {
	return c.doRequest("GET", path, v)
}

// getCtx is get with a caller-supplied context, so a cancellable caller aborts
// its request rather than waiting out the client's 30s timeout.
func (c *Client) getCtx(ctx context.Context, path string, v interface{}) error {
	return c.doRequestWith(ctx, c.httpClient, "GET", path, v)
}

func (c *Client) post(path string, v interface{}) error {
	return c.doRequest("POST", path, v)
}

// postLifecycle issues a POST using the lifecycle client, whose longer timeout
// tolerates a stop/restart that the daemon holds open up to a configured stop
// budget (#35, D2).
func (c *Client) postLifecycle(path string, v interface{}) error {
	return c.doRequestWith(context.Background(), c.lifecycleClient, "POST", path, v)
}

// addAuthHeader adds the Authorization header if a token is available
func (c *Client) addAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// parseSSELogEntry parses a single SSE data line into a log entry.
// Returns the parsed entry and true if successful, or an empty entry and false if parsing failed.
//
// readSSE (below) now tracks "event:" lines and routes an "event: handshake"
// frame's payload to the handshake hook instead of this parser (plan 017 C9),
// so the handshake no longer reaches it in practice. The guard below stays
// regardless, as defense against any other non-entry payload riding the
// "data:" convention: json.Unmarshal ignores unknown fields, so such a payload
// decodes into an api.LogEntryResponse without error, leaving every field at
// its zero value. A real log entry always has either a non-empty Process, a
// non-empty Line, or a manager-assigned Seq >= 1, so Process=="" && Line=="" &&
// Seq==0 can only be a non-log payload — without this it would surface as a
// phantom empty log row in the attach TUI.
func parseSSELogEntry(data string) (api.LogEntryResponse, bool) {
	var entry api.LogEntryResponse
	if err := json.Unmarshal([]byte(data), &entry); err != nil {
		// log.Printf, not a direct stderr write: these parsers run on the TUI's
		// OWN stream goroutines, so a raw fd-2 write lands in the middle of a
		// rendered alt-screen frame. Routed through the stdlib logger they go
		// wherever the session's stdio sink points (stdio_sink.go). log.SetFlags(0)
		// means the emitted line is byte-identical to the old fmt.Fprintf.
		log.Printf("warning: failed to parse SSE log entry: %v", err)
		return entry, false
	}
	if entry.Process == "" && entry.Line == "" && entry.Seq == 0 {
		return api.LogEntryResponse{}, false
	}
	return entry, true
}

// sseHandshakeEvent is the SSE event name carrying api.HandshakeResponse. The
// logs stream sends exactly one, immediately after ": connected" and before any
// entry (internal/api/sse.go).
const sseHandshakeEvent = "handshake"

// parseSSEHandshake parses the payload of an "event: handshake" frame. A
// malformed payload warns and is dropped: the consumer then behaves exactly as
// it does against a server that sends no handshake at all.
func parseSSEHandshake(data string) (api.HandshakeResponse, bool) {
	var hs api.HandshakeResponse
	if err := json.Unmarshal([]byte(data), &hs); err != nil {
		log.Printf("warning: failed to parse SSE handshake: %v", err)
		return hs, false
	}
	return hs, true
}

// parseSSEProxyRequest parses a single SSE data line into a proxy request.
// Returns the parsed request and true if successful, or an empty request and false if parsing failed.
func parseSSEProxyRequest(data string) (api.ProxyRequestResponse, bool) {
	var req api.ProxyRequestResponse
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		log.Printf("warning: failed to parse SSE proxy request: %v", err)
		return req, false
	}
	return req, true
}

// parseSSEProcessList parses a single SSE data line of the processes stream
// into a full process snapshot. Every event on that stream is a complete
// api.ProcessListResponse (internal/api/sse.go) — there are no deltas — so an
// empty Processes slice is a legitimate payload ("nothing is configured or
// running") rather than a frame to filter out, and no zero-value guard like
// parseSSELogEntry's applies here.
func parseSSEProcessList(data string) (api.ProcessListResponse, bool) {
	var resp api.ProcessListResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		log.Printf("warning: failed to parse SSE process snapshot: %v", err)
		return resp, false
	}
	return resp, true
}

// sseErrorBodyLimit bounds how much of a non-200 SSE connect body is read
// before parsing it as an api.ErrorResponse.
const sseErrorBodyLimit = 8 << 10

// sseContentType is the media type an SSE endpoint must answer with. The
// response may append parameters (e.g. "; charset=utf-8"), so it is matched as
// a prefix.
const sseContentType = "text/event-stream"

// sseStream is one dialed SSE attempt: the live response plus the conn that
// carried it. conn is captured by the per-attempt transport so deadlineReader
// can set a read deadline on the exact connection this response is reading
// from -- the transport must therefore dial at most once per attempt.
type sseStream struct {
	resp      *http.Response
	conn      net.Conn
	transport *http.Transport
}

// close releases the body and the attempt's idle connections. Each attempt owns
// its transport, so nothing is shared with a later attempt.
func (s *sseStream) close() {
	s.resp.Body.Close()
	s.transport.CloseIdleConnections()
}

// dialSSE opens an SSE connection to path and validates the response. A non-200
// answer is returned as *APIError, parsed from the (bounded) JSON error body
// when possible.
func dialSSE(ctx context.Context, c *Client, path string) (*sseStream, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", sseContentType)
	c.addAuthHeader(req)

	// Custom transport to capture connection for read deadlines. The capture
	// is only sound while exactly one dial serves the response, so redirects
	// are refused below (a redirect chain can hand the response to a reused
	// idle connection while conn points at the redirect hop's dial — the
	// deadline would then arm the wrong socket). An SSE endpoint never
	// legitimately redirects; a 3xx falls through to the non-200 error path.
	var conn net.Conn
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var err error
			conn, err = dialer.DialContext(ctx, network, addr)
			return conn, err
		},
		// The deadlineReader only guards body reads; without this a server
		// that accepts TCP but never sends response headers would hang the
		// dial forever (http.Client.Timeout is 0 for SSE by design).
		ResponseHeaderTimeout: constants.DefaultRequestTimeout,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // SSE streams are long-lived
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}

	stream := &sseStream{resp: resp, conn: conn, transport: transport}

	if resp.StatusCode != http.StatusOK {
		defer stream.close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, sseErrorBodyLimit))
		var errResp api.ErrorResponse
		if err := json.Unmarshal(body, &errResp); err == nil {
			return nil, httpStatusError(resp.StatusCode, &errResp)
		}
		return nil, httpStatusError(resp.StatusCode, nil)
	}

	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != sseContentType {
		ct := resp.Header.Get("Content-Type")
		stream.close()
		return nil, fmt.Errorf("unexpected content type %q from %s (want %s)", ct, path, sseContentType)
	}

	return stream, nil
}

// readSSE reads events off a dialed stream until it ends, handing each parsed
// event to onEvent. It returns the terminal read error, or nil when ctx was
// cancelled. Reads carry constants.SSEReadTimeout as a deadline so a server
// that dies without closing the connection cannot hang the reader forever.
//
// Frames are read as name+payload: an "event: <name>" line names the frame that
// the next "data:" line carries, and the blank line terminating a frame clears
// the name (comments never do — they are not frames). Only one named event
// exists today: "handshake" (plan 017 C8), whose payload goes to onHandshake
// (nilable; nil means "ignore the frame") and NEVER to the entry parser, which
// would otherwise see a payload that is not an event of type T. An unnamed
// frame is a plain data event, exactly as before this hook existed, so a server
// that sends no handshake simply never fires it.
//
// A server MAY send more than one handshake on a stream; each one is delivered.
// Consumers treat a repeat as harmless (the TUI's logs sync latches the first —
// see logsSyncState.noteHandshake).
func readSSE[T any](ctx context.Context, s *sseStream, parse func(string) (T, bool), onHandshake func(api.HandshakeResponse), onEvent func(T)) error {
	defer s.close()

	bodyReader := &deadlineReader{
		r:       s.resp.Body,
		conn:    s.conn,
		timeout: constants.SSEReadTimeout,
	}
	reader := bufio.NewReader(bodyReader)

	// event names the frame currently being read; "" is an unnamed (plain data)
	// frame.
	event := ""

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			event = "" // end of frame: the name does not carry into the next one
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment (": connected", ": ping") — not a frame
		}

		if name, ok := strings.CutPrefix(line, "event: "); ok {
			event = name
			continue
		}

		if data, ok := strings.CutPrefix(line, "data: "); ok {
			// A data line consumes the pending event name (codex C9 finding):
			// per the SSE model one event field names one dispatch, so a
			// following data line without its own event: prefix is a plain
			// entry, even before any blank-line frame delimiter arrives.
			frameEvent := event
			event = ""
			if frameEvent == sseHandshakeEvent {
				if hs, ok := parseSSEHandshake(data); ok && onHandshake != nil {
					onHandshake(hs)
				}
				continue
			}
			if item, ok := parse(data); ok {
				onEvent(item)
			}
		}
	}
}

// consumeSSE connects and delivers events via onEvent until the stream ends;
// returns the terminal error (nil only on ctx cancellation). onConnect (may
// be nil) fires exactly once, after the dial fully succeeded (headers +
// content type validated) and before the first read — it exists so a
// reconnect loop can mark the attempt healthy only once a connection
// actually stands, never for a dead-on-arrival dial. onHandshake (may be nil)
// receives the stream's handshake frame, if it sends one; see readSSE.
func consumeSSE[T any](ctx context.Context, c *Client, path string, parse func(string) (T, bool), onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(T)) error {
	s, err := dialSSE(ctx, c, path)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if onConnect != nil {
		onConnect()
	}
	return readSSE(ctx, s, parse, onHandshake, onEvent)
}

// streamSSE is the channel form of an SSE attempt: connect-time failures come
// back synchronously, and the channel closes when the stream ends for any reason.
// The terminal error is dropped here on purpose -- consumers of the channel API
// only observe the close.
//
// Contract: a consumer that abandons the channel MUST cancel ctx. Cancellation
// is the only unblock for the reader once the channel buffer fills; abandoning
// without cancelling pins the reader goroutine and its connection until the
// buffer's next send would occur — forever, on a quiet stream.
func streamSSE[T any](ctx context.Context, c *Client, path string, parse func(string) (T, bool)) (<-chan T, error) {
	s, err := dialSSE(ctx, c, path)
	if err != nil {
		return nil, err
	}

	ch := make(chan T, 100)
	go func() {
		defer close(ch)
		// nil handshake hook: the channel form carries one element type and has
		// no way to surface a handshake, so the frame is ignored (as it was
		// before C9 taught the reader to recognize it).
		_ = readSSE(ctx, s, parse, nil, func(item T) {
			// Never block past cancellation: a consumer that stopped reading
			// would otherwise pin this goroutine.
			select {
			case ch <- item:
			case <-ctx.Done():
			}
		})
	}()

	return ch, nil
}

// logsStreamPath builds the log SSE path with params applied as query string.
func logsStreamPath(params domain.LogParams) string {
	return pathWithQuery("/api/v1/logs/stream", buildLogQueryParams(params))
}

// proxyRequestsStreamPath builds the proxy-request SSE path with params applied
// as query string.
func proxyRequestsStreamPath(params domain.ProxyRequestParams) string {
	return pathWithQuery("/api/v1/proxy/requests/stream", buildProxyRequestQueryParams(params))
}

// ConsumeLogs delivers streamed log entries to onEvent until the stream ends,
// returning the error that ended it (nil on ctx cancellation). onConnect
// (nilable) fires once after the connection is established, before the first
// read. onHandshake (nilable) receives the stream's handshake frame, which
// carries the server's current log epoch — the attach TUI's log sync needs it
// to decide how to backfill (plan 017 C9); a pre-C8 daemon sends none and the
// hook simply never fires. It is the attempt form the TUI's reconnect loop
// drives; the channel form below serves the one-shot --follow commands.
func (c *Client) ConsumeLogs(ctx context.Context, params domain.LogParams, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
	return consumeSSE(ctx, c, logsStreamPath(params), parseSSELogEntry, onConnect, onHandshake, onEvent)
}

// ConsumeProxyRequests is the proxy-request counterpart of ConsumeLogs. The
// requests stream sends no handshake (it has no epoch/cursor protocol), so it
// passes a nil hook.
func (c *Client) ConsumeProxyRequests(ctx context.Context, params domain.ProxyRequestParams, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
	return consumeSSE(ctx, c, proxyRequestsStreamPath(params), parseSSEProxyRequest, onConnect, nil, onEvent)
}

// processesStreamPath is the process-state SSE endpoint. Unlike the logs and
// requests streams it takes no query parameters: every event is the full
// process list, unfiltered.
const processesStreamPath = "/api/v1/processes/stream"

// ConsumeProcesses delivers full process-list snapshots to onEvent until the
// stream ends, returning the error that ended it (nil on ctx cancellation).
// onConnect (nilable) fires once after the connection is established, before
// the first read.
//
// The stream sends no handshake (it has no epoch/cursor protocol — each event
// is self-describing current state), so it passes a nil hook. It is the
// attempt form the attach TUI's reconnect loop drives; there is no channel
// form because no --follow command consumes process state.
func (c *Client) ConsumeProcesses(ctx context.Context, onConnect func(), onEvent func(api.ProcessListResponse)) error {
	return consumeSSE(ctx, c, processesStreamPath, parseSSEProcessList, onConnect, nil, onEvent)
}

// StreamProxyRequestsChannel returns a channel that streams proxy requests via SSE.
// The channel is closed when the connection ends, the read times out, or ctx is
// cancelled.
func (c *Client) StreamProxyRequestsChannel(ctx context.Context, params domain.ProxyRequestParams) (<-chan api.ProxyRequestResponse, error) {
	return streamSSE(ctx, c, proxyRequestsStreamPath(params), parseSSEProxyRequest)
}

// StreamLogsChannel returns a channel that streams log entries via SSE.
// The channel is closed when the connection ends, the read times out, or ctx is
// cancelled.
func (c *Client) StreamLogsChannel(ctx context.Context, params domain.LogParams) (<-chan api.LogEntryResponse, error) {
	return streamSSE(ctx, c, logsStreamPath(params), parseSSELogEntry)
}
