package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
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
	resp, err := c.get("/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

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
	resp, err := c.get("/api/v1/status")
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
	url := "http://proxyd" + path
	return c.httpClient.Get(url)
}

// post performs an HTTP POST to the daemon with a JSON body.
func (c *Client) post(path string, body any) (*http.Response, error) {
	url := "http://proxyd" + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	return c.httpClient.Post(url, "application/json", bodyReader)
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
