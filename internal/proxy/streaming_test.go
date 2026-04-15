package proxy

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSEThroughProxy verifies that SSE connections stream events through the
// proxy without being terminated by server timeouts.
func TestSSEThroughProxy(t *testing.T) {
	// Backend sends an SSE event every 500ms
	eventInterval := 500 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(eventInterval):
				fmt.Fprintf(w, "data: event-%d\n\n", i)
				flusher.Flush()
			}
		}
	}))
	defer backend.Close()

	proxyPort := findFreePort(t)
	svc := startProxyService(t, proxyPort, backend)
	defer svc.Shutdown(context.Background())

	// Connect to the SSE endpoint through the proxy
	client := &http.Client{Timeout: 0}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/events", proxyPort), nil)
	require.NoError(t, err)
	req.Host = "app.local.test.dev"

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Read events for 3 seconds — well beyond any short timeout
	scanner := bufio.NewScanner(resp.Body)
	var events []string
	deadline := time.After(3 * time.Second)

	for {
		select {
		case <-deadline:
			goto done
		default:
			// Set a per-read deadline so we don't block forever
			if scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					events = append(events, line)
				}
			} else {
				t.Fatalf("SSE stream ended unexpectedly after %d events: %v", len(events), scanner.Err())
			}
		}
	}
done:
	// With 500ms interval over 3s, we expect at least 4 events
	assert.GreaterOrEqual(t, len(events), 4,
		"expected at least 4 SSE events over 3 seconds, got %d", len(events))
	t.Logf("received %d SSE events through proxy", len(events))
}

// TestWebSocketUpgradeThroughProxy verifies that WebSocket upgrade requests
// are properly proxied with bidirectional data flow.
func TestWebSocketUpgradeThroughProxy(t *testing.T) {
	// Backend that accepts WebSocket upgrades and echoes messages
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
			return
		}

		// Compute the accept key per RFC 6455
		key := r.Header.Get("Sec-WebSocket-Key")
		acceptKey := computeWebSocketAccept(key)

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}

		conn, bufrw, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		// Send 101 Switching Protocols response
		bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		bufrw.WriteString("Upgrade: websocket\r\n")
		bufrw.WriteString("Connection: Upgrade\r\n")
		bufrw.WriteString("Sec-WebSocket-Accept: " + acceptKey + "\r\n")
		bufrw.WriteString("\r\n")
		bufrw.Flush()

		// Simple echo: read raw bytes and write them back
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	proxyPort := findFreePort(t)
	svc := startProxyService(t, proxyPort, backend)
	defer svc.Shutdown(context.Background())

	// Connect via raw TCP and send a WebSocket upgrade request
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	wsKey := base64.StdEncoding.EncodeToString([]byte("test-key-1234567"))
	upgradeReq := fmt.Sprintf(
		"GET /ws HTTP/1.1\r\n"+
			"Host: app.local.test.dev\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\n"+
			"Sec-WebSocket-Version: 13\r\n"+
			"\r\n", wsKey)

	_, err = conn.Write([]byte(upgradeReq))
	require.NoError(t, err)

	// Read the 101 response
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, statusLine, "101", "expected 101 Switching Protocols, got: %s", statusLine)

	// Read remaining headers
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Verify bidirectional data flow
	testMsg := []byte("hello from client")
	_, err = conn.Write(testMsg)
	require.NoError(t, err)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	echoBuf := make([]byte, len(testMsg))
	_, err = io.ReadFull(reader, echoBuf)
	require.NoError(t, err)
	assert.Equal(t, testMsg, echoBuf, "expected echo of sent message")
}

// startProxyService creates and starts a proxy service with the backend as "app".
func startProxyService(t *testing.T, proxyPort int, backend *httptest.Server) *Service {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	cfg := &config.ProxyConfig{
		Enabled:  true,
		HTTPPort: proxyPort,
		Domain:   "local.test.dev",
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: backendPort, Host: "127.0.0.1"},
	}

	svc, err := NewService(cfg, services, nil, logger, workDir)
	require.NoError(t, err)

	err = svc.Start(context.Background())
	require.NoError(t, err)

	// Wait for proxy to be ready
	require.Eventually(t, func() bool {
		return isPortListening(proxyPort)
	}, 2*time.Second, 10*time.Millisecond, "proxy did not start in time")

	return svc
}

// computeWebSocketAccept computes the Sec-WebSocket-Accept value per RFC 6455.
func computeWebSocketAccept(key string) string {
	const websocketGUID = "258EAFA5-E914-47DA-95CA-5AB5DC587D35"
	h := sha1.New()
	h.Write([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
