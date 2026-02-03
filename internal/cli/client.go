package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
)

// sseReadTimeout is the timeout for SSE reads. If no data is received within
// this duration, the connection is considered dead. SSE servers send heartbeats,
// so this should be longer than the heartbeat interval.
const sseReadTimeout = 60 * time.Second

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
}

// NewClient creates a new API client
func NewClient(baseURL string) *Client {
	// Try to load token from file
	token, _ := loadToken() // Ignore error - token may not exist

	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetStatus gets supervisor status
func (c *Client) GetStatus() (*api.StatusResponse, error) {
	var resp api.StatusResponse
	if err := c.get("/api/v1/status", &resp); err != nil {
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
	return c.post("/api/v1/processes/"+url.PathEscape(name)+"/start", &resp)
}

// StopProcess stops a process
func (c *Client) StopProcess(name string) error {
	var resp api.SuccessResponse
	return c.post("/api/v1/processes/"+url.PathEscape(name)+"/stop", &resp)
}

// RestartProcess restarts a process
func (c *Client) RestartProcess(name string) error {
	var resp api.SuccessResponse
	return c.post("/api/v1/processes/"+url.PathEscape(name)+"/restart", &resp)
}

// Shutdown shuts down the supervisor
func (c *Client) Shutdown() error {
	var resp api.SuccessResponse
	return c.post("/api/v1/shutdown", &resp)
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
	if params.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	return query
}

// GetLogs gets logs with optional filtering
func (c *Client) GetLogs(params domain.LogParams) (*api.LogsResponse, error) {
	query := buildLogQueryParams(params)

	path := "/api/v1/logs"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var resp api.LogsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetProxyRequests gets recent proxy requests with optional filtering
func (c *Client) GetProxyRequests(params domain.ProxyRequestParams) (*api.ProxyRequestsResponse, error) {
	query := buildProxyRequestQueryParams(params)

	path := "/api/v1/proxy/requests"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var resp api.ProxyRequestsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// httpStatusError maps HTTP status codes to user-friendly error messages
func httpStatusError(statusCode int, errResp *api.ErrorResponse) error {
	if errResp != nil && errResp.Error != "" {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Error)
	}

	switch statusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: invalid or missing token")
	case http.StatusForbidden:
		return fmt.Errorf("access denied: insufficient permissions")
	case http.StatusNotFound:
		return fmt.Errorf("not found: the requested resource does not exist")
	case http.StatusInternalServerError:
		return fmt.Errorf("server error: the prox daemon encountered an internal error")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("service unavailable: the prox daemon is not ready")
	default:
		return fmt.Errorf("request failed with status %d", statusCode)
	}
}

func (c *Client) doRequest(method, path string, v interface{}) error {
	req, err := http.NewRequest(method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
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

func (c *Client) post(path string, v interface{}) error {
	return c.doRequest("POST", path, v)
}

// addAuthHeader adds the Authorization header if a token is available
func (c *Client) addAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// parseSSELogEntry parses a single SSE data line into a log entry.
// Returns the parsed entry and true if successful, or an empty entry and false if parsing failed.
func parseSSELogEntry(data string) (api.LogEntryResponse, bool) {
	var entry api.LogEntryResponse
	if err := json.Unmarshal([]byte(data), &entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse SSE log entry: %v\n", err)
		return entry, false
	}
	return entry, true
}

// parseSSEProxyRequest parses a single SSE data line into a proxy request.
// Returns the parsed request and true if successful, or an empty request and false if parsing failed.
func parseSSEProxyRequest(data string) (api.ProxyRequestResponse, bool) {
	var req api.ProxyRequestResponse
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse SSE proxy request: %v\n", err)
		return req, false
	}
	return req, true
}

// streamSSE creates an SSE connection and returns a channel of parsed events.
// The channel is closed when the connection ends or times out.
func streamSSE[T any](req *http.Request, parse func(string) (T, bool)) (<-chan T, error) {
	// Custom transport to capture connection for read deadlines
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
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // SSE streams are long-lived
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, httpStatusError(resp.StatusCode, nil)
	}

	ch := make(chan T, 100)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		bodyReader := &deadlineReader{
			r:       resp.Body,
			conn:    conn,
			timeout: sseReadTimeout,
		}
		reader := bufio.NewReader(bodyReader)

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if item, ok := parse(data); ok {
					ch <- item
				}
			}
		}
	}()

	return ch, nil
}

// StreamProxyRequestsChannel returns a channel that streams proxy requests via SSE.
// The channel is closed when the connection ends or the read times out.
func (c *Client) StreamProxyRequestsChannel(params domain.ProxyRequestParams) (<-chan api.ProxyRequestResponse, error) {
	query := buildProxyRequestQueryParams(params)

	path := "/api/v1/proxy/requests/stream"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.addAuthHeader(req)
	return streamSSE(req, parseSSEProxyRequest)
}

// StreamLogsChannel returns a channel that streams log entries via SSE.
// The channel is closed when the connection ends or the read times out.
func (c *Client) StreamLogsChannel(params domain.LogParams) (<-chan api.LogEntryResponse, error) {
	query := buildLogQueryParams(params)

	path := "/api/v1/logs/stream"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.addAuthHeader(req)
	return streamSSE(req, parseSSELogEntry)
}
