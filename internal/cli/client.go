package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charliek/prox/internal/api"
)

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

// LogParams contains parameters for log queries
type LogParams struct {
	Process string
	Lines   int
	Pattern string
	Regex   bool
}

// GetLogs gets logs with optional filtering
func (c *Client) GetLogs(params LogParams) (*api.LogsResponse, error) {
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

// StreamLogs streams logs and calls the callback for each entry
func (c *Client) StreamLogs(params LogParams, callback func(api.LogEntryResponse)) error {
	query := url.Values{}
	if params.Process != "" {
		query.Set("process", params.Process)
	}
	if params.Pattern != "" {
		query.Set("pattern", params.Pattern)
	}
	if params.Regex {
		query.Set("regex", "true")
	}

	path := "/api/v1/logs/stream"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var entry api.LogEntryResponse
			if err := json.Unmarshal([]byte(data), &entry); err == nil {
				callback(entry)
			}
		}
	}
}

func (c *Client) get(path string, v interface{}) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
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
			return fmt.Errorf("%s: %s", errResp.Code, errResp.Error)
		}
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) post(path string, v interface{}) error {
	req, err := http.NewRequest("POST", c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuthHeader(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp api.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil {
			return fmt.Errorf("%s: %s", errResp.Code, errResp.Error)
		}
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(v)
}

// addAuthHeader adds the Authorization header if a token is available
func (c *Client) addAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
