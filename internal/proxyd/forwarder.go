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
// It runs until ctx is cancelled. On disconnect, it reconnects with backoff.
func ForwardRequests(ctx context.Context, socketPath string, projectDir string, localRM *proxy.RequestManager) {
	backoff := 500 * time.Millisecond

	for {
		err := streamRequests(ctx, socketPath, projectDir, localRM)
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
func streamRequests(ctx context.Context, socketPath, projectDir string, localRM *proxy.RequestManager) error {
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
					localRM.Record(record)
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
