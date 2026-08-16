package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

func TestStreamLogs_Headers(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	// Create a request with a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run in goroutine so we can cancel it
	done := make(chan struct{})
	go func() {
		handlers.StreamLogs(rec, req)
		close(done)
	}()

	// Wait a bit for headers to be written
	time.Sleep(50 * time.Millisecond)

	// Cancel the request
	cancel()

	// Wait for handler to finish
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after context cancel")
	}

	// Check headers
	result := rec.Result()
	defer result.Body.Close()

	if ct := result.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type 'text/event-stream', got %q", ct)
	}
	if cc := result.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control 'no-cache', got %q", cc)
	}
	if conn := result.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("expected Connection 'keep-alive', got %q", conn)
	}
	if xab := result.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("expected X-Accel-Buffering 'no', got %q", xab)
	}
}

func TestStreamLogs_FilterParsing(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	tests := []struct {
		name        string
		queryParams string
	}{
		{"no params", ""},
		{"process filter", "?process=web"},
		{"multiple processes", "?process=web,api"},
		{"pattern", "?pattern=error"},
		{"regex pattern", "?pattern=error.*&regex=true"},
		{"combined", "?process=web&pattern=error&regex=true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest("GET", "/api/v1/logs/stream"+tt.queryParams, nil).WithContext(ctx)
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				handlers.StreamLogs(rec, req)
				close(done)
			}()

			// Wait a bit for setup
			time.Sleep(50 * time.Millisecond)

			// Cancel request
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}

			// Should have received the connection comment
			body := rec.Body.String()
			if !strings.Contains(body, ": connected") {
				t.Errorf("expected connection comment, got %q", body)
			}
		})
	}
}

func TestStreamLogs_DataFormat(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handlers.StreamLogs(rec, req)
		close(done)
	}()

	// Wait for connection to be established
	time.Sleep(50 * time.Millisecond)

	// Write a log entry
	logMgr.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      "test message",
	})

	// Wait for it to be sent
	time.Sleep(50 * time.Millisecond)

	// Cancel request
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}

	// Parse SSE events. The stream now also carries the handshake event (plan
	// 017 C8) as its own "data: " line immediately preceded by "event:
	// handshake" -- track the previous line so that payload isn't mistaken
	// for a log entry.
	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))

	var (
		foundData      bool
		foundHandshake bool
		prevLine       string
	)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if strings.TrimSpace(prevLine) == "event: handshake" {
				var hs HandshakeResponse
				if err := json.Unmarshal([]byte(data), &hs); err != nil {
					t.Errorf("failed to parse handshake data line: %v", err)
				} else if hs.StreamID != logMgr.StreamID() {
					t.Errorf("expected handshake stream_id %q, got %q", logMgr.StreamID(), hs.StreamID)
				}
				foundHandshake = true
				prevLine = line
				continue
			}

			foundData = true
			var entry LogEntryResponse
			if err := json.Unmarshal([]byte(data), &entry); err != nil {
				t.Errorf("failed to parse data line: %v", err)
			} else {
				if entry.Process != "test" {
					t.Errorf("expected Process 'test', got %q", entry.Process)
				}
				if entry.Stream != "stdout" {
					t.Errorf("expected Stream 'stdout', got %q", entry.Stream)
				}
				if entry.Line != "test message" {
					t.Errorf("expected Line 'test message', got %q", entry.Line)
				}
				if entry.Seq == 0 {
					t.Error("expected the streamed entry to carry a non-zero Seq")
				}
			}
		}
		prevLine = line
	}

	if !foundHandshake {
		t.Error("expected to find the handshake data line in SSE response")
	}
	if !foundData {
		t.Error("expected to find data line in SSE response")
	}
}

// TestStreamLogs_Handshake asserts the "event: handshake" frame is written
// immediately after the ": connected" comment and carries the manager's
// current stream ID (plan 017 C8): a reconnecting client must learn the
// epoch before it can decide how to backfill.
func TestStreamLogs_Handshake(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handlers.StreamLogs(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}

	body := rec.Body.String()

	// Drop blank lines (SSE frame separators) so the adjacency check is
	// robust to the ": connected\n\n" / "event:...\ndata:...\n\n" framing.
	var nonEmpty []string
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}

	require.GreaterOrEqual(t, len(nonEmpty), 3, "expected the connected comment, the handshake event line and its data line")
	require.Contains(t, nonEmpty[0], ": connected")
	require.Equal(t, "event: handshake", nonEmpty[1])
	require.True(t, strings.HasPrefix(nonEmpty[2], "data: "))

	var hs HandshakeResponse
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(nonEmpty[2], "data: ")), &hs))
	require.Equal(t, logMgr.StreamID(), hs.StreamID)
}

func TestStreamLogs_InvalidPattern(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	// Invalid regex pattern
	req := httptest.NewRequest("GET", "/api/v1/logs/stream?pattern=[invalid&regex=true", nil)
	rec := httptest.NewRecorder()

	handlers.StreamLogs(rec, req)

	result := rec.Result()
	defer result.Body.Close()

	if result.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", result.StatusCode)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(result.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Code != domain.ErrCodeInvalidPattern {
		t.Errorf("expected code %q, got %q", domain.ErrCodeInvalidPattern, errResp.Code)
	}
}

// noFlushWriter is a minimal http.ResponseWriter that deliberately does NOT
// implement http.Flusher, so it exercises StreamLogs' no-flusher branch.
// (httptest.ResponseRecorder implements Flusher, so it cannot.)
type noFlushWriter struct {
	headers http.Header
	status  int
	body    []byte
}

func (w *noFlushWriter) Header() http.Header {
	if w.headers == nil {
		w.headers = make(http.Header)
	}
	return w.headers
}

func (w *noFlushWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *noFlushWriter) WriteHeader(status int) { w.status = status }

func TestStreamLogs_NoFlusher_ReturnsJSONError(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)

	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil)
	w := &noFlushWriter{}

	handlers.StreamLogs(w, req)

	if w.status != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.status)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", ct)
	}
	// The error path must run before any SSE headers are written; assert none
	// leaked (Content-Type alone is insufficient, since writeJSON overwrites it).
	for _, header := range []string{"Cache-Control", "Connection", "X-Accel-Buffering"} {
		if got := w.Header().Get(header); got != "" {
			t.Errorf("expected %s to be unset on the error path, got %q", header, got)
		}
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(w.body, &errResp); err != nil {
		t.Fatalf("response body is not JSON: %v (body=%q)", err, w.body)
	}
	if errResp.Code != domain.ErrCodeStreamingNotSupported {
		t.Errorf("expected code %q, got %q", domain.ErrCodeStreamingNotSupported, errResp.Code)
	}
}

// readSSEConnected reads the ": connected" comment every SSE handler writes
// on subscribe and returns a buffered reader positioned right after it, ready
// for the caller to keep reading SSE lines from. Shared by the StreamLogs and
// StreamProxyRequests heartbeat/disconnect tests below.
func readSSEConnected(t *testing.T, resp *http.Response) *bufio.Reader {
	t.Helper()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, ": connected")
	return reader
}

// requireSSEHeartbeats reads SSE lines from reader until it has seen minPings
// ": ping" heartbeats and one "data: " line containing dataMarker, failing if
// that takes longer than window. window is the cadence assertion, not just a
// hang guard: callers pick a small multiple of the injected heartbeat
// interval, so an implementation whose real cadence grossly exceeds the
// configured interval cannot pass on scheduling luck, while the margin stays
// wide enough not to flake under CI load. Shared by the StreamLogs and
// StreamProxyRequests heartbeat tests to also assert heartbeats and data
// events interleave rather than one starving the other.
func requireSSEHeartbeats(t *testing.T, reader *bufio.Reader, dataMarker string, minPings int, window time.Duration) {
	t.Helper()
	start := time.Now()
	pings, sawData := 0, false
	for pings < minPings || !sawData {
		if elapsed := time.Since(start); elapsed > window {
			t.Fatalf("saw %d/%d pings, data=%v after %v (window %v)",
				pings, minPings, sawData, elapsed, window)
		}
		l, err := reader.ReadString('\n')
		require.NoError(t, err)
		switch {
		case strings.TrimSpace(l) == ": ping":
			pings++
		case strings.HasPrefix(l, "data: ") && strings.Contains(l, dataMarker):
			sawData = true
		}
	}
}

// requireSSEHandlerReturns waits for done to close (an SSE handler returning
// after its deferred cleanup), failing the test if it doesn't within 2s.
// Shared by the StreamLogs and StreamProxyRequests client-disconnect tests.
func requireSSEHandlerReturns(t *testing.T, done <-chan struct{}, handlerName string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return after the client closed the connection", handlerName)
	}
}

// TestStreamLogs_Heartbeat drives StreamLogs behind a real httptest.Server (an
// httptest.ResponseRecorder never streams to a reader in real time, so a
// heartbeat cadence can't be observed against one) with a short injected
// heartbeat interval. It asserts an idle stream still emits ": ping" comments
// on that cadence, and that a data event published mid-stream is delivered
// interleaved with the heartbeats rather than starving one or the other.
func TestStreamLogs_Heartbeat(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)
	handlers.sseHeartbeatInterval = 20 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamLogs))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)

	// Publish a data event partway through the read loop so it must interleave
	// with the heartbeat ticks rather than block on them.
	go func() {
		time.Sleep(4 * handlers.sseHeartbeatInterval)
		logMgr.Write(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "hb",
			Stream:    domain.StreamStdout,
			Line:      "interleaved",
		})
	}()

	// 3 pings within 1s at a 20ms interval: generous 16× margin against CI
	// scheduling, yet a cadence regression to even 500ms/ping cannot pass.
	requireSSEHeartbeats(t, reader, "interleaved", 3, time.Second)
}

// TestStreamLogs_ClientDisconnect_ReturnsHandler covers the teardown path with
// a real connection close (as opposed to the context-cancellation simulation
// used by TestStreamLogs_Headers et al.): once the client closes its side of
// the connection, the handler's next write must fail and it must return,
// freeing the log subscription via its deferred Unsubscribe.
func TestStreamLogs_ClientDisconnect_ReturnsHandler(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", "", nil)
	handlers.sseHeartbeatInterval = 10 * time.Millisecond

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.StreamLogs(w, r)
		close(done)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	readSSEConnected(t, resp)

	require.NoError(t, resp.Body.Close())

	requireSSEHandlerReturns(t, done, "StreamLogs")
}
