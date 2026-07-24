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

	"github.com/charliek/prox/internal/proxy"
)

// Client communicates with the proxy daemon over its Unix socket.
type Client struct {
	socketPath string
	httpClient *http.Client
}

// NewClient creates a new daemon client that connects via Unix socket.
func NewClient(socketPath string) *Client {
	dialer := &net.Dialer{}
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
	}
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

// Register registers a project's routes with the daemon.
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

// get performs an HTTP GET to the daemon.
func (c *Client) get(path string) (*http.Response, error) {
	// "http://proxyd" is a dummy host — the Unix socket transport ignores it.
	return c.httpClient.Get("http://proxyd" + path)
}

// getWithContext performs an HTTP GET to the daemon bound to ctx, so a caller
// can cancel the request (e.g. on shutdown) without waiting out the client's
// timeout. The existing get helper is left ctx-less for its callers.
func (c *Client) getWithContext(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://proxyd"+path, nil)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// post performs an HTTP POST to the daemon with a JSON body.
func (c *Client) post(path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	return c.httpClient.Post("http://proxyd"+path, "application/json", bodyReader)
}

// readError reads an error response from the daemon.
func (c *Client) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)

	var errResp ErrorResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Errorf("%s", errResp.Error)
	}
	return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(body))
}
