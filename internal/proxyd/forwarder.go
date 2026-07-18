package proxyd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
)

// ForwardRequests subscribes to the daemon's SSE request stream and forwards
// this project's records into the local RequestManager. This bridges the
// daemon's proxy request data into the project's TUI and API.
//
// projectDir must be the same value sent as RegisterRequest.ProjectDir so the
// daemon-side filter (which scopes records by owning project) matches.
//
// On every (re)connect the bridge also backfills a snapshot of the daemon's
// current ring for this project (see streamRequests / backfillSnapshot), closing
// any gap opened while the subscription was down. The subscription itself stays
// lossy (bounded, non-blocking channels), so the guarantee is bounded gap
// closure, not losslessness.
//
// It runs until ctx is cancelled. On disconnect, it reconnects with backoff.
func ForwardRequests(ctx context.Context, socketPath string, projectDir string, localRM *proxy.RequestManager) {
	// One snapshot client for the whole forwarder lifetime: its transport pools
	// the idle unix connection across reconnect attempts, rather than leaking a
	// fresh idle conn per attempt (the daemon only reaps idle conns after 60s, so
	// a reconnect storm would otherwise accumulate them).
	snapClient := NewClient(socketPath)

	backoff := 500 * time.Millisecond

	for {
		err := streamRequests(ctx, socketPath, snapClient, projectDir, localRM)
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
// snapClient is the shared daemon client used for the backfill snapshot.
func streamRequests(ctx context.Context, socketPath string, snapClient *Client, projectDir string, localRM *proxy.RequestManager) error {
	// Create HTTP client that dials the Unix socket
	dialer := &net.Dialer{}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	streamURL := fmt.Sprintf("http://proxyd/api/v1/requests/stream?project=%s", url.QueryEscape(projectDir))
	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
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

	// The daemon Subscribes before it writes response headers, so once Do
	// returned 200 the subscription is already live. Backfill a snapshot of the
	// daemon's ring concurrently with the read loop below: launching it in a
	// goroutine (rather than fetching before entering the loop) ensures the
	// forwarder never sits blocked on a slow snapshot while live events pile up
	// — a >100-event burst during the fetch would overflow the daemon-side
	// subscription channel, the exact gap this backfill exists to close.
	//
	// The fetch is scoped to attemptCtx, cancelled when streamRequests returns
	// (defer). This bounds the snapshot to THIS connection attempt: a stalled
	// fetch cannot outlive its stream and replay a stale snapshot after a later
	// attempt has already established fresh state (whose distinct IDs would
	// bypass Upsert's dedupe and cause wrong evictions / duplicate notifies).
	// The goroutine exits when the fetch completes OR attemptCtx is cancelled,
	// so it never leaks; it writes only to localRM, which outlives the stream.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go backfillSnapshot(attemptCtx, snapClient, projectDir, localRM)

	// Read SSE events line by line. bufio.Scanner is NOT used: its token size is
	// capped (and even a raised cap would kill the subscription on the first
	// oversized line). Records can legitimately exceed 64KB — on a capture
	// disk-write failure both bodies fall back to inline storage up to 1MB each,
	// and headers are unbounded — so we use a bufio.Reader with an explicit
	// per-event cap (constants.MaxSSEEventSize). An oversized event is skipped
	// (drained to its newline) with a logged warning and the stream continues,
	// rather than tearing down the bridge.
	reader := bufio.NewReaderSize(resp.Body, constants.ScannerBufferSize)
	dataPrefix := []byte("data: ")
	for {
		line, oversize, readErr := readEventLine(reader, constants.MaxSSEEventSize)

		switch {
		case readErr != nil:
			// A line returned alongside a read error has no terminating
			// newline — the SSE event is incomplete (mid-write disconnect).
			// Discard it rather than recording a possibly-partial event; the
			// reconnect loop will re-sync on fresh events.
		case oversize:
			log.Printf("prox: skipping oversized daemon request event (exceeds %d bytes)", constants.MaxSSEEventSize)
		case len(line) > 0:
			// SSE format: "data: {json}". Trim the trailing "\r\n"/"\n".
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, dataPrefix) {
				jsonData := trimmed[len(dataPrefix):]
				var record proxy.RequestRecord
				if err := json.Unmarshal(jsonData, &record); err == nil {
					// Upsert (not Record): applying live events through the
					// monotonic state machine makes interleaving with the
					// concurrent snapshot replay safe — duplicates and stale
					// in-flight events are no-ops, so nothing is delivered twice.
					localRM.Upsert(record)
				}
				// Malformed events are skipped silently, as before.
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// backfillSnapshot fetches the daemon's current record snapshot for projectDir
// and replays it into localRM, closing any gap opened while the SSE bridge was
// disconnected. It is launched from streamRequests once the subscription is
// live and drains concurrently with the read loop; Upsert's monotonic state
// machine makes any interleaving of {snapshot copy, live copy, completion} safe
// (duplicates and stale in-flight events are no-ops), so records are never
// delivered twice.
//
// The limit is pinned to constants.MaxProxyRequests (the daemon ring size) so a
// full ring backfills completely; an omitted limit would silently cap at the
// daemon's default of 100. Client.Requests decodes all-or-nothing, so a
// truncated body applies zero records.
//
// On any failure — non-200, decode error, timeout, or ctx cancellation — it
// logs one warning and returns, leaving the stream to run in degraded,
// stream-only mode. A failed backfill never tears down the bridge.
//
// ctx is the per-attempt context, so a stream error/return cancels an in-flight
// fetch; client is the forwarder's shared snapshot client (pooled unix conn).
func backfillSnapshot(ctx context.Context, client *Client, projectDir string, localRM *proxy.RequestManager) {
	records, err := client.Requests(ctx, projectDir, constants.MaxProxyRequests)
	if err != nil {
		log.Printf("prox: request snapshot backfill failed: %v", err)
		return
	}

	// The endpoint returns records newest-first; replay oldest-first so ring
	// order tracks arrival order as closely as the live stream would have.
	// Bail out mid-replay if the attempt ended: a snapshot fetched by a dying
	// attempt should not keep replaying after a newer attempt took over.
	for i := len(records) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
		localRM.Upsert(records[i])
	}
}

// readEventLine reads a single '\n'-terminated line from r, enforcing maxSize.
// It returns:
//   - the line (including its trailing newline) when within the cap;
//   - oversize=true with a nil line when the line exceeds maxSize — the excess is
//     drained up to (and including) the newline so the reader is positioned at
//     the start of the next event and the caller can continue the stream;
//   - a non-nil error (e.g. io.EOF) from the underlying reader, alongside any
//     partial line read so far.
//
// It uses ReadSlice against r's fixed internal buffer, so no single call buffers
// more than that buffer's size; the accumulated line is bounded by maxSize.
func readEventLine(r *bufio.Reader, maxSize int) (line []byte, oversize bool, err error) {
	var buf []byte
	for {
		chunk, rerr := r.ReadSlice('\n')

		if !oversize {
			if len(buf)+len(chunk) > maxSize {
				// Over the cap: stop accumulating and release what we have; keep
				// draining until the newline so the next event is aligned.
				oversize = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}

		switch rerr {
		case nil:
			// Reached the newline: the line is complete.
			if oversize {
				return nil, true, nil
			}
			return buf, false, nil
		case bufio.ErrBufferFull:
			// No newline in this fill; keep reading the same line.
			continue
		default:
			// Underlying error (io.EOF or a transport failure).
			if oversize {
				return nil, true, rerr
			}
			return buf, false, rerr
		}
	}
}
