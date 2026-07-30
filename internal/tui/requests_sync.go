package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// RequestsSyncMsg is one completed requests-stream synchronization: the ring
// snapshot fetched over REST (oldest-first) plus every live event that arrived
// while the fetch was running (in arrival order). The models apply Snapshot
// then Buffered through the monotonic merge and re-render ONCE, so a
// full-ring replay costs one render rather than a thousand.
//
// Exactly one of these is delivered per successful stream attempt, immediately
// before the loop is marked synchronized (StateOK).
type RequestsSyncMsg struct {
	Snapshot []proxy.RequestRecord
	Buffered []proxy.RequestRecord
}

// errRequestsSyncOverflow ends an attempt whose live-event buffer filled before
// the snapshot landed. It is deliberately a distinct error: the loop reconnects
// and re-syncs from a fresh snapshot, which is the only way to recover the
// events the buffer could not hold. Nothing is silently lost.
var errRequestsSyncOverflow = errors.New("requests sync: live event buffer overflowed before the snapshot landed")

// requestsSnapshotFetchAttempts is how many consecutive snapshot fetch failures
// an otherwise-live stream attempt tolerates before the attempt is aborted and
// recycled. Retrying in place (at the stream package's base backoff) is right
// for a blip — the SSE connection is fine and tearing it down would lose the
// live events we are already buffering — but an endlessly failing fetch would
// otherwise pin the stream in Syncing forever, so the attempt eventually
// recycles and the loop's own backoff takes over.
const requestsSnapshotFetchAttempts = 3

// consumeRequestsWithSync is the requests stream's single connect-and-consume
// attempt. The logs stream runs the same protocol over its own payload
// (consumeLogsWithSync); the two are meant to read as one.
//
// The protocol, mirroring the forwarder's proven backfillSnapshot shape:
//
//  1. Connect. onConnect fires on the SSE reader goroutine and starts — but
//     does NOT complete — the sync: markSynced is deliberately not called yet,
//     so the loop stays in Syncing and the status bar keeps saying so.
//  2. Live events arriving from here on are BUFFERED, not delivered.
//  3. Concurrently, on the fetch goroutine (see syncFetch), fetch the full ring
//     over REST.
//  4. On success, deliver one RequestsSyncMsg carrying the snapshot and the
//     buffer, mark the loop synchronized (StateOK), and flip live events back
//     to direct delivery. All three happen under one mutex, so the reader
//     goroutine cannot interleave an event between the batch and the flip.
func consumeRequestsWithSync(ctx context.Context, client TUIClient, send func(tea.Msg), markSynced func()) error {
	attemptCtx, fetch := newSyncFetch(ctx)
	defer fetch.join()

	st := &requestsSyncState{buffering: true, abort: fetch.abort, send: send}

	err := client.ConsumeProxyRequests(attemptCtx, domain.ProxyRequestParams{},
		func() {
			fetch.start(func() error {
				return syncRequestsSnapshot(attemptCtx, client, st, markSynced)
			})
		},
		st.observe)

	// Join before reading the fetch's error: the deferred backstop runs only
	// after the return value has been computed.
	fetch.join()
	return fetch.attemptError(ctx, st.abortErr(), err)
}

// syncRequestsSnapshot fetches the ring snapshot and completes the sync. It
// runs on its own goroutine, concurrently with the SSE read loop.
//
// A failure while the stream is still live is retried in place at the stream
// package's base backoff rather than tearing the connection down — the events
// we are buffering are exactly what the reconnect would have to re-fetch
// anyway. After requestsSnapshotFetchAttempts consecutive failures it gives up
// and returns the error, which aborts the attempt so the loop's own backoff
// and status reporting take over. The loop stays in Syncing (never OK) for the
// whole retry window.
//
// A ctx cancellation returns nil: the attempt is already ending for a reason
// the caller knows better than we do, and reporting a bare cancellation here
// would mask the stream's real error.
func syncRequestsSnapshot(ctx context.Context, client TUIClient, st *requestsSyncState, markSynced func()) error {
	var lastErr error
	for i := 0; i < requestsSnapshotFetchAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(constants.StreamReconnectBaseBackoff):
			}
		}

		// Scroll-back pages the older records on demand through the BeforeID
		// cursor, so this fetch is a screenful's worth, not the whole ring.
		resp, err := client.GetProxyRequests(ctx, domain.ProxyRequestParams{Limit: constants.TUIRequestsSyncLimit})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			lastErr = err
			continue
		}

		st.complete(snapshotRecords(st.send, resp.Requests), markSynced)
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("requests sync: snapshot fetch failed %d times: %w", requestsSnapshotFetchAttempts, lastErr)
}

// snapshotRecords converts a REST snapshot to records in APPLY order. The
// endpoint returns newest-first; the merge wants oldest-first so the list ends
// up in the same order the live stream would have produced.
func snapshotRecords(send func(tea.Msg), requests []api.ProxyRequestResponse) []proxy.RequestRecord {
	records := make([]proxy.RequestRecord, 0, len(requests))
	for i := len(requests) - 1; i >= 0; i-- {
		records = append(records, streamedProxyRequest(send, requests[i]))
	}
	return records
}

// requestsSyncState is the handoff between the SSE reader goroutine (which
// produces live events) and the snapshot fetch goroutine (which ends the
// buffering phase). Everything is guarded by mu: complete's "deliver the batch,
// mark synced, flip to passthrough" must be atomic with respect to observe, or
// a live event could slip out either side of the batch.
type requestsSyncState struct {
	mu        sync.Mutex
	buffering bool
	buf       []proxy.RequestRecord
	overflow  bool

	// abort cancels the attempt context (syncFetch.abort). Called once, outside
	// mu, when the buffer overflows.
	abort func()

	send func(tea.Msg)
}

// observe handles one live event: buffered while the snapshot is outstanding,
// delivered directly once the sync has completed.
func (st *requestsSyncState) observe(req api.ProxyRequestResponse) {
	st.mu.Lock()

	if !st.buffering {
		st.mu.Unlock()
		sendStreamedProxyRequest(st.send, req)
		return
	}
	if st.overflow {
		// The attempt is already being torn down; nothing gained by keeping
		// events for a batch that will never be delivered.
		st.mu.Unlock()
		return
	}
	// The live-buffer cap stays at the RING size (MaxProxyRequests), not the
	// smaller sync fetch limit: this bounds how many events may pile up while
	// the snapshot is outstanding, and once that many have arrived the snapshot
	// is worthless anyway (the whole ring turned over).
	if len(st.buf) >= constants.MaxProxyRequests {
		st.overflow = true
		st.mu.Unlock()
		st.abort()
		return
	}

	st.buf = append(st.buf, streamedProxyRequest(st.send, req))
	st.mu.Unlock()
}

// complete delivers the sync batch and switches to passthrough. A no-op once
// the attempt has overflowed (the batch is moot) or the sync already completed
// (a client that called onConnect twice).
func (st *requestsSyncState) complete(snapshot []proxy.RequestRecord, markSynced func()) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.buffering || st.overflow {
		return
	}

	buffered := st.buf
	st.buf = nil
	st.buffering = false

	st.send(RequestsSyncMsg{Snapshot: snapshot, Buffered: buffered})
	// Only NOW is the stream synchronized: the models hold the full ring plus
	// everything that arrived during the fetch.
	markSynced()
}

// abortErr reports the sync-protocol reason this attempt must be recycled, or
// nil if it ended for a reason of the stream's own.
func (st *requestsSyncState) abortErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.overflow {
		return errRequestsSyncOverflow
	}
	return nil
}
