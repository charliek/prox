package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRawSSEServer starts an HTTP server on a unix socket that serves handler
// for every request, and returns the socket path. It lets a test control the
// exact SSE bytes the forwarder reads (framing, oversized lines, etc.).
func startRawSSEServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	// Short temp dir to stay under the Unix socket 104-byte path limit on macOS.
	tmpDir, err := os.MkdirTemp("/tmp", "prox-fwd-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "s.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socketPath
}

// TestStreamRequests_LargeInlineBodies pins that a record carrying the
// disk-fallback worst case — two 1MB inline bodies (~2.7MB base64) — parses
// successfully under the new per-event cap, and the inline body bytes round-trip
// through base64 JSON unchanged. This is exactly the event the old 64KB
// bufio.Scanner token limit would have dropped.
func TestStreamRequests_LargeInlineBodies(t *testing.T) {
	big1 := bytes.Repeat([]byte("a"), 1024*1024)
	big2 := bytes.Repeat([]byte("b"), 1024*1024)

	rec := proxy.RequestRecord{
		Timestamp:  time.Now(),
		Method:     "POST",
		URL:        "/big",
		ProjectDir: "/p",
		Details: &proxy.RequestDetails{
			RequestBody:  &proxy.CapturedBody{ContentType: "application/octet-stream", Data: big1},
			ResponseBody: &proxy.CapturedBody{ContentType: "application/octet-stream", Data: big2},
		},
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	// Sanity: the framed event is large but under the cap.
	require.Less(t, len(data)+len("data: \n\n"), constants.MaxSSEEventSize)
	require.Greater(t, len(data), constants.ScannerBufferSize, "event must exceed the reader's buffer to exercise accumulation")

	socketPath := startRawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", data)
	})

	localRM := proxy.NewRequestManager(100)
	// streamRequests returns nil once the server closes the stream (io.EOF).
	require.NoError(t, streamRequests(context.Background(), socketPath, "/p", localRM))

	require.Equal(t, 1, localRM.Count())
	got := localRM.Recent(proxy.RequestFilter{})[0]
	require.NotNil(t, got.Details)
	require.NotNil(t, got.Details.RequestBody)
	require.NotNil(t, got.Details.ResponseBody)
	assert.True(t, bytes.Equal(big1, got.Details.RequestBody.Data), "request body must round-trip")
	assert.True(t, bytes.Equal(big2, got.Details.ResponseBody.Data), "response body must round-trip")
}

// TestStreamRequests_OversizeEventSkippedStreamContinues pins that an event
// exceeding the per-event cap is skipped (not fatal) and the NEXT event is still
// delivered — the reader drains the oversized line to its newline and continues.
func TestStreamRequests_OversizeEventSkippedStreamContinues(t *testing.T) {
	small := proxy.RequestRecord{
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/small",
		ProjectDir: "/p",
	}
	smallData, err := json.Marshal(small)
	require.NoError(t, err)

	// A single data line whose payload exceeds the cap.
	oversize := bytes.Repeat([]byte("x"), constants.MaxSSEEventSize+4096)

	socketPath := startRawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", oversize)
		fmt.Fprintf(w, "data: %s\n\n", smallData)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	localRM := proxy.NewRequestManager(100)
	require.NoError(t, streamRequests(context.Background(), socketPath, "/p", localRM))

	require.Equal(t, 1, localRM.Count(), "oversize event skipped, following event delivered")
	got := localRM.Recent(proxy.RequestFilter{})[0]
	assert.Equal(t, "/small", got.URL)
}

// TestForwardRequests_FiltersByProject pins that the forwarder subscribes by
// project dir (via the ?project= stream param) and the daemon delivers only the
// owning project's records — even when a second project shares the hostname.
func TestForwardRequests_FiltersByProject(t *testing.T) {
	server, _, socketPath := startTestServer(t)

	daemonRM := proxy.NewRequestManager(100)
	server.SetRequestManager(daemonRM)

	localRM := proxy.NewRequestManager(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ForwardRequests(ctx, socketPath, "/projects/a", localRM)

	// The bridge is stream-only (no backfill), so records must be produced after
	// the forwarder subscribes. Record both projects on a tick until the local
	// side observes A's record.
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()

	i := 0
loop:
	for {
		select {
		case <-deadline:
			t.Fatal("forwarder never received project A's record")
		case <-tick.C:
			i++
			ts := time.Now()
			daemonRM.Record(proxy.RequestRecord{Timestamp: ts, Method: "GET", URL: "/a", Hostname: "api.local.dev", ProjectDir: "/projects/a"})
			daemonRM.Record(proxy.RequestRecord{Timestamp: ts.Add(time.Duration(i)), Method: "GET", URL: "/b", Hostname: "api.local.dev", ProjectDir: "/projects/b"})
			if localRM.Count() > 0 {
				break loop
			}
		}
	}

	// Let any (incorrectly) delivered B records land, then assert none did.
	time.Sleep(100 * time.Millisecond)
	for _, rec := range localRM.Recent(proxy.RequestFilter{}) {
		assert.Equal(t, "/projects/a", rec.ProjectDir, "forwarder must only receive project A's records")
	}
}
