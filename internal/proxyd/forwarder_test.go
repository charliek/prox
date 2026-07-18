package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRawSSEServer starts an HTTP server on a unix socket that serves streamH
// on the SSE stream endpoint, and returns the socket path. It lets a test
// control the exact SSE bytes the forwarder reads (framing, oversized lines,
// etc.). The snapshot endpoint (/api/v1/requests, hit by the forwarder's
// backfill) returns an empty snapshot so it stays a no-op for stream-only tests.
func startRawSSEServer(t *testing.T, streamH http.HandlerFunc) string {
	return startRawSSEServerMux(t, streamH, func(w http.ResponseWriter, _ *http.Request) {
		writeSnapshot(w, nil)
	})
}

// startRawSSEServerMux is startRawSSEServer with an explicit snapshot handler so
// backfill tests can control the /api/v1/requests response independently of the
// SSE stream.
func startRawSSEServerMux(t *testing.T, streamH, requestsH http.HandlerFunc) string {
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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/requests/stream", streamH)
	mux.HandleFunc("/api/v1/requests", requestsH)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socketPath
}

// writeSnapshot writes records as the daemon's {"requests":[...]} snapshot
// wrapper. records must already be newest-first (as the daemon returns them). A
// nil slice is normalized to [] to mirror the real daemon, which always emits a
// non-nil slice ("requests":[] when empty) — never null.
func writeSnapshot(w http.ResponseWriter, records []proxy.RequestRecord) {
	if records == nil {
		records = []proxy.RequestRecord{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"requests": records})
}

// holdOpenStream sends the SSE preamble, then blocks until the forwarder's
// context cancels the request — keeping the subscription live so the forwarder
// does not reconnect (and re-backfill) during a test.
func holdOpenStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
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
	require.NoError(t, streamRequests(context.Background(), socketPath, NewClient(socketPath), "/p", localRM))

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
	require.NoError(t, streamRequests(context.Background(), socketPath, NewClient(socketPath), "/p", localRM))

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

	// The bridge backfills a snapshot on connect and then applies live events;
	// this test exercises the live path, recording both projects on a tick until
	// the local side observes A's record.
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

// smallRecords builds n newest-first records with distinct IDs (rec-000 is the
// oldest, rec-<n-1> the newest) for backfill tests.
func smallRecords(n int) []proxy.RequestRecord {
	records := make([]proxy.RequestRecord, n)
	base := time.Now()
	for i := 0; i < n; i++ {
		idx := n - 1 - i // newest-first
		records[i] = proxy.RequestRecord{
			ID:         fmt.Sprintf("rec-%04d", idx),
			Timestamp:  base.Add(time.Duration(idx) * time.Millisecond),
			Method:     "GET",
			URL:        fmt.Sprintf("/r/%d", idx),
			ProjectDir: "/p",
		}
	}
	return records
}

// TestBackfill_AllRecordsDeliveredPinsLimit pins that a full ring (>100 records)
// existing daemon-side before the first connect is delivered locally in its
// entirety — which requires the forwarder to request the explicit
// constants.MaxProxyRequests limit, not the daemon's default of 100.
func TestBackfill_AllRecordsDeliveredPinsLimit(t *testing.T) {
	const n = 150
	records := smallRecords(n)

	limitCh := make(chan string, 1)
	socketPath := startRawSSEServerMux(t, holdOpenStream,
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case limitCh <- r.URL.Query().Get("limit"):
			default:
			}
			writeSnapshot(w, records)
		})

	localRM := proxy.NewRequestManager(constants.MaxProxyRequests)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ForwardRequests(ctx, socketPath, "/p", localRM)

	require.Eventually(t, func() bool { return localRM.Count() == n }, 3*time.Second, 10*time.Millisecond,
		"all %d snapshot records must be delivered locally", n)

	select {
	case gotLimit := <-limitCh:
		assert.Equal(t, "1000", gotLimit, "backfill must request the explicit MaxProxyRequests limit")
	case <-time.After(time.Second):
		t.Fatal("snapshot endpoint was never hit")
	}
}

// TestBackfill_RecordsAddedDuringDisconnectAppearAfterReconnect pins that
// records the daemon accrues while the bridge is severed are delivered once the
// forwarder reconnects and re-backfills. The gap records are served ONLY on the
// second connection, so the test cannot pass via the first attempt's backfill —
// it must observe a genuine reconnect.
func TestBackfill_RecordsAddedDuringDisconnectAppearAfterReconnect(t *testing.T) {
	var connects int32

	socketPath := startRawSSEServerMux(t,
		func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&connects, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if n == 1 {
				return // force a disconnect on the first connect
			}
			<-r.Context().Done() // hold open after reconnect
		},
		func(w http.ResponseWriter, _ *http.Request) {
			// The stream handler increments connects before Do returns 200, so by
			// the time this attempt's backfill fetches, connects reflects the
			// current attempt. Serve gap records only from the second attempt on.
			if atomic.LoadInt32(&connects) >= 2 {
				writeSnapshot(w, smallRecords(2)) // gap records: rec-0000, rec-0001
			} else {
				writeSnapshot(w, nil)
			}
		})

	localRM := proxy.NewRequestManager(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ForwardRequests(ctx, socketPath, "/p", localRM)

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&connects) >= 2 && localRM.Count() == 2
	}, 4*time.Second, 10*time.Millisecond, "gap records must appear only after a reconnect")

	require.GreaterOrEqual(t, atomic.LoadInt32(&connects), int32(2), "a genuine reconnect must have occurred")
	ids := map[string]bool{}
	for _, rec := range localRM.Recent(proxy.RequestFilter{}) {
		ids[rec.ID] = true
	}
	assert.True(t, ids["rec-0000"] && ids["rec-0001"], "both gap records delivered")
}

// TestBackfill_OverlapDedupe pins that a record present in BOTH the snapshot and
// the live stream is delivered to local subscribers exactly once — for a final
// record and for an in-flight one (the monotonic state machine no-ops the dupe).
func TestBackfill_OverlapDedupe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inFlight bool
	}{
		{"final", false},
		{"in_flight", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxy.RequestRecord{
				ID:         "dup-1",
				Timestamp:  time.Now(),
				Method:     "GET",
				URL:        "/x",
				ProjectDir: "/p",
				StatusCode: 200,
				InFlight:   tc.inFlight,
			}
			data, err := json.Marshal(rec)
			require.NoError(t, err)

			// The snapshot must provably serve the overlapping record before the
			// stream sends its copy, so the assertion cannot be satisfied by the
			// stream event alone — both copies enter the pipeline, exactly one
			// notifies.
			var snapshotHits int32
			snapshotServed := make(chan struct{})
			var once sync.Once

			socketPath := startRawSSEServerMux(t,
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-snapshotServed:
					case <-r.Context().Done():
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", data)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					<-r.Context().Done()
				},
				func(w http.ResponseWriter, _ *http.Request) {
					atomic.AddInt32(&snapshotHits, 1)
					writeSnapshot(w, []proxy.RequestRecord{rec})
					once.Do(func() { close(snapshotServed) })
				})

			localRM := proxy.NewRequestManager(100)
			sub := localRM.Subscribe(proxy.RequestFilter{})
			defer localRM.Unsubscribe(sub.ID)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go ForwardRequests(ctx, socketPath, "/p", localRM)

			// Collect notifications for a fixed window; both the snapshot copy and
			// the stream copy apply, but only the first notifies.
			var got []proxy.RequestRecord
			deadline := time.After(time.Second)
		collect:
			for {
				select {
				case rec := <-sub.Ch:
					got = append(got, rec)
				case <-deadline:
					break collect
				}
			}
			require.GreaterOrEqual(t, atomic.LoadInt32(&snapshotHits), int32(1), "snapshot must have served the overlapping record")
			require.Len(t, got, 1, "overlapping record delivered exactly once")
			assert.Equal(t, "dup-1", got[0].ID)
			assert.Equal(t, 1, localRM.Count())
		})
	}
}

// TestBackfill_ConcurrentDrainDelayedSnapshot pins that live events are applied
// without waiting for a slow snapshot fetch — the read loop drains concurrently
// with the backfill goroutine. The snapshot handler blocks on a channel (no
// timing window): the live event must be delivered while it is still blocked.
func TestBackfill_ConcurrentDrainDelayedSnapshot(t *testing.T) {
	release := make(chan struct{})
	socketPath := startRawSSEServerMux(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rec := proxy.RequestRecord{ID: "live-1", Timestamp: time.Now(), Method: "GET", URL: "/l", ProjectDir: "/p"}
			data, _ := json.Marshal(rec)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		},
		func(w http.ResponseWriter, r *http.Request) {
			// Block the snapshot until released; live events must not wait for it.
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeSnapshot(w, nil)
		})

	localRM := proxy.NewRequestManager(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ForwardRequests(ctx, socketPath, "/p", localRM)

	// The snapshot is still blocked (release not yet closed); the live event must
	// nevertheless be delivered — proof the read loop drains concurrently.
	require.Eventually(t, func() bool { return localRM.Count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"live event must be delivered while the snapshot fetch is blocked")

	// Release the snapshot so the backfill goroutine finishes cleanly.
	close(release)
}

// TestBackfill_SnapshotFailureStreamStillDelivers pins that a failed snapshot
// (non-200 or truncated JSON) applies zero records via all-or-nothing decode,
// while live stream events keep flowing (degraded, stream-only mode).
func TestBackfill_SnapshotFailureStreamStillDelivers(t *testing.T) {
	streamRec := proxy.RequestRecord{ID: "stream-1", Timestamp: time.Now(), Method: "GET", URL: "/s", ProjectDir: "/p"}
	streamData, err := json.Marshal(streamRec)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http_500", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"truncated_json", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"requests":[{"id":"x","method":"GET"`) // truncated mid-object
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketPath := startRawSSEServerMux(t,
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", streamData)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					<-r.Context().Done()
				},
				tc.handler)

			localRM := proxy.NewRequestManager(100)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go ForwardRequests(ctx, socketPath, "/p", localRM)

			require.Eventually(t, func() bool { return localRM.Count() == 1 }, 2*time.Second, 10*time.Millisecond)
			// Let any erroneous snapshot apply race in, then confirm still just the
			// stream record.
			time.Sleep(200 * time.Millisecond)
			require.Equal(t, 1, localRM.Count(), "snapshot failure must apply zero records")
			assert.Equal(t, "stream-1", localRM.Recent(proxy.RequestFilter{})[0].ID)
		})
	}
}

// TestBackfillSnapshot_CtxCancelExitsCleanly pins that the backfill goroutine
// exits promptly when ctx is cancelled during a hung snapshot fetch — no leak,
// no records applied.
func TestBackfillSnapshot_CtxCancelExitsCleanly(t *testing.T) {
	entered := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	socketPath := startRawSSEServerMux(t,
		func(_ http.ResponseWriter, _ *http.Request) {}, // stream unused
		func(_ http.ResponseWriter, r *http.Request) {
			close(entered) // signal the fetch has reached the handler
			// Hang until the test ends or the client's ctx cancels the request.
			select {
			case <-block:
			case <-r.Context().Done():
			}
		})

	localRM := proxy.NewRequestManager(100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		backfillSnapshot(ctx, NewClient(socketPath), "/p", localRM)
		close(done)
	}()

	// Cancel only once the fetch is provably mid-flight inside the handler.
	<-entered
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("backfillSnapshot did not exit after ctx cancel")
	}
	assert.Equal(t, 0, localRM.Count(), "no records applied on cancelled fetch")
}
