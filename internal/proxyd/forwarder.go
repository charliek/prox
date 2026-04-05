package proxyd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/charliek/prox/internal/proxy"
)

// ForwardRequests subscribes to the daemon's SSE request stream and forwards
// matching records into the local RequestManager. This bridges the daemon's
// proxy request data into the project's TUI and API.
//
// It runs until ctx is cancelled. On disconnect, it reconnects with backoff.
func ForwardRequests(ctx context.Context, socketPath string, domains []string, localRM *proxy.RequestManager) {
	domainsParam := strings.Join(domains, ",")
	backoff := 500 * time.Millisecond

	for {
		err := streamRequests(ctx, socketPath, domainsParam, localRM)
		if ctx.Err() != nil {
			return // context cancelled, clean shutdown
		}
		_ = err // reconnect silently

		// Backoff before reconnecting
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// streamRequests opens an SSE connection to the daemon and processes events.
func streamRequests(ctx context.Context, socketPath, domainsParam string, localRM *proxy.RequestManager) error {
	// Create HTTP client that dials the Unix socket
	dialer := &net.Dialer{}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	url := fmt.Sprintf("http://proxyd/api/v1/requests/stream?domains=%s", domainsParam)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("creating SSE request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to daemon SSE: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon SSE returned %d", resp.StatusCode)
	}

	// Read SSE events line by line
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "data: {json}"
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonData := strings.TrimPrefix(line, "data: ")
		var record proxy.RequestRecord
		if err := json.Unmarshal([]byte(jsonData), &record); err != nil {
			continue // skip malformed events
		}

		localRM.Record(record)
	}

	return scanner.Err()
}
