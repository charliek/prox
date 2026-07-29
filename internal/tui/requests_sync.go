package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
// attempt (the logs stream stays a pure stream consumer; C9 owns log sync).
//
// The protocol, mirroring the forwarder's proven backfillSnapshot shape:
//
//  1. Connect. onConnect fires on the SSE reader goroutine and starts — but
//     does NOT complete — the sync: markSynced is deliberately not called yet,
//     so the loop stays in Syncing and the status bar keeps saying so.
//  2. Live events arriving from here on are BUFFERED, not delivered.
//  3. Concurrently (a goroutine — the reader goroutine is busy reading, and
//     buffering would deadlock if the fetch ran on it), fetch the full ring
//     over REST.
//  4. On success, deliver one RequestsSyncMsg carrying the snapshot and the
//     buffer, mark the loop synchronized (StateOK), and flip live events back
//     to direct delivery. All three happen under one mutex, so the reader
//     goroutine cannot interleave an event between the batch and the flip.
//
// The fetch goroutine is tied to a per-attempt context and waited for before
// returning, so it never outlives its attempt — which is what makes calling
// markSynced from it safe despite stream.Config.Attempt's "call it from the
// attempt itself" rule: the rule exists to stop a STRAGGLING goroutine from
// flipping a later attempt's state, and this one cannot straggle (the loop's
// epoch guard is the second line of defense).
func consumeRequestsWithSync(ctx context.Context, client TUIClient, send func(tea.Msg), markSynced func()) error {
	attemptCtx, cancel := context.WithCancel(ctx)

	st := &requestsSyncState{buffering: true, abort: cancel}
	fetchDone := make(chan struct{})
	var fetchStarted atomic.Bool
	var fetchErr error

	// join ends the fetch goroutine and waits for it, so it can never outlive
	// its attempt. Idempotent (cancel is; a closed channel stays receivable),
	// so it is safe both as the deferred backstop and as the explicit join
	// below that makes fetchErr readable.
	join := func() {
		cancel()
		if fetchStarted.Load() {
			<-fetchDone
		}
	}
	defer join()

	err := client.ConsumeProxyRequests(attemptCtx, domain.ProxyRequestParams{},
		func() {
			// One sync per attempt: a client that called onConnect twice would
			// otherwise start a second fetch goroutine and double-close
			// fetchDone.
			if !fetchStarted.CompareAndSwap(false, true) {
				return
			}
			go func() {
				defer close(fetchDone)
				fetchErr = syncRequestsSnapshot(attemptCtx, client, send, st, markSynced)
				if fetchErr != nil {
					// Give up on this attempt: the loop reconnects and the
					// next attempt re-syncs from scratch.
					cancel()
				}
			}()
		},
		func(req api.ProxyRequestResponse) {
			st.observe(send, req)
		})

	// Join before reading fetchErr: the deferred backstop runs only after the
	// return value has been computed.
	join()

	// Prefer the sync-protocol cause. Aborting a sync cancels the attempt
	// context, which makes ConsumeProxyRequests report a generic cancellation
	// that would otherwise hide why the attempt is recycling. A cancellation
	// of the PARENT context (the TUI quitting) is not ours to reinterpret —
	// the loop ends on it regardless.
	if ctx.Err() == nil {
		if st.overflowed() {
			return errRequestsSyncOverflow
		}
		if fetchErr != nil {
			return fetchErr
		}
	}
	return err
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
func syncRequestsSnapshot(ctx context.Context, client TUIClient, send func(tea.Msg), st *requestsSyncState, markSynced func()) error {
	var lastErr error
	for i := 0; i < requestsSnapshotFetchAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(constants.StreamReconnectBaseBackoff):
			}
		}

		resp, err := client.GetProxyRequests(ctx, domain.ProxyRequestParams{Limit: constants.MaxProxyRequests})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			lastErr = err
			continue
		}

		st.complete(send, snapshotRecords(send, resp.Requests), markSynced)
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

	// abort cancels the attempt context. Called (once, outside mu) when the
	// buffer overflows, since a buffering onEvent has no other way to end the
	// attempt.
	abort func()
}

// observe handles one live event: buffered while the snapshot is outstanding,
// delivered directly once the sync has completed.
func (st *requestsSyncState) observe(send func(tea.Msg), req api.ProxyRequestResponse) {
	st.mu.Lock()

	if !st.buffering {
		st.mu.Unlock()
		sendStreamedProxyRequest(send, req)
		return
	}
	if st.overflow {
		// The attempt is already being torn down; nothing gained by keeping
		// events for a batch that will never be delivered.
		st.mu.Unlock()
		return
	}
	if len(st.buf) >= constants.MaxProxyRequests {
		st.overflow = true
		st.mu.Unlock()
		st.abort()
		return
	}

	st.buf = append(st.buf, streamedProxyRequest(send, req))
	st.mu.Unlock()
}

// complete delivers the sync batch and switches to passthrough. A no-op once
// the attempt has overflowed (the batch is moot) or the sync already completed
// (a client that called onConnect twice).
func (st *requestsSyncState) complete(send func(tea.Msg), snapshot []proxy.RequestRecord, markSynced func()) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.buffering || st.overflow {
		return
	}

	buffered := st.buf
	st.buf = nil
	st.buffering = false

	send(RequestsSyncMsg{Snapshot: snapshot, Buffered: buffered})
	// Only NOW is the stream synchronized: the models hold the full ring plus
	// everything that arrived during the fetch.
	markSynced()
}

func (st *requestsSyncState) overflowed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.overflow
}
