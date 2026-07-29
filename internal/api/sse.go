package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// StreamLogs handles GET /api/v1/logs/stream (SSE)
func (h *Handlers) StreamLogs(w http.ResponseWriter, r *http.Request) {
	// Check if flusher is available before writing any SSE headers, so the
	// error path can return a clean JSON error (matching StreamProxyRequests).
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "streaming not supported",
			Code:  domain.ErrCodeStreamingNotSupported,
		})
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Parse filter parameters
	filter := domain.LogFilter{}
	if processes := r.URL.Query().Get("process"); processes != "" {
		filter.Processes = strings.Split(processes, ",")
	}
	filter.Pattern = r.URL.Query().Get("pattern")
	if r.URL.Query().Get("regex") == "true" {
		filter.IsRegex = true
	}

	// Subscribe to logs
	subID, ch, err := h.logManager.Subscribe(filter)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: err.Error(),
			Code:  domain.ErrCodeInvalidPattern,
		})
		return
	}
	defer h.logManager.Unsubscribe(subID)

	// rc lets each SSE write set a per-write deadline (see sseWrite below).
	rc := http.NewResponseController(w)

	// Send initial comment to establish connection
	if err := sseWrite(rc, w, []byte(": connected\n\n")); err != nil {
		log.Printf("SSE write error (client likely disconnected): %v", err)
		return
	}

	// Send the handshake event immediately after: a reconnecting client must
	// learn the current stream epoch BEFORE deciding how to backfill (see
	// HandshakeResponse). It rides a named "event: handshake" frame, not a
	// bare "data:" line, precisely so it cannot be mistaken for a log entry by
	// an old client -- see HandshakeResponse's doc comment for the full
	// reasoning and the parseSSELogEntry guard that backs it up.
	handshake, err := json.Marshal(HandshakeResponse{StreamID: h.logManager.StreamID()})
	if err != nil {
		// StreamID is a plain string; Marshal cannot fail for this shape in
		// practice. Guard anyway rather than risk writing a broken frame.
		log.Printf("failed to marshal handshake: %v", err)
		return
	}
	if err := sseWrite(rc, w, []byte("event: handshake\n"), []byte("data: "), handshake, []byte("\n\n")); err != nil {
		log.Printf("SSE write error (client likely disconnected): %v", err)
		return
	}

	ticker := time.NewTicker(h.heartbeatInterval())
	defer ticker.Stop()

	// Stream logs
	// Protection against slow clients:
	// 1. Log subscription uses a buffered channel - if client can't keep up, messages are dropped
	// 2. Write errors cause the handler to return, cleaning up the subscription
	// 3. Context cancellation (client disconnect) is handled via select
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sseWrite(rc, w, []byte(": ping\n\n")); err != nil {
				log.Printf("SSE write error (client likely disconnected): %v", err)
				return
			}
		case entry, ok := <-ch:
			if !ok {
				return
			}

			// Convert to JSON
			resp := ToLogEntryResponse(entry)
			data, err := json.Marshal(resp)
			if err != nil {
				continue
			}

			// Send SSE event - handle write errors to detect slow/disconnected clients
			if err := sseWrite(rc, w, []byte("data: "), data, []byte("\n\n")); err != nil {
				// Client disconnected or write failed - logged for debugging
				log.Printf("SSE write error (client likely disconnected): %v", err)
				return
			}
		}
	}
}

// StreamProcesses handles GET /api/v1/processes/stream (SSE), built on the
// supervisor's process-change bus (plan 017 C10/C11).
//
// Every event on this stream -- the initial send and every subsequent one -- is
// a FULL ProcessListResponse snapshot (the same conversion GET /processes
// uses, via processListResponse in handlers.go). There are no deltas: a client
// that misses an event just waits for the next one, which is self-describing
// current state rather than a diff that needs a base to apply against
// (pinned; poll semantics with push timing).
//
// Subscribe-before-snapshot: the change-bus subscription is registered BEFORE
// the initial snapshot is read below, so a transition landing in that exact
// window is never silently missed -- it sets this subscriber's dirty latch,
// and the main loop's very first wake re-snapshots and resends (a possibly-
// redundant extra event, never a lost one).
func (h *Handlers) StreamProcesses(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "streaming not supported",
			Code:  domain.ErrCodeStreamingNotSupported,
		})
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe FIRST (see doc comment above): a change landing between this
	// call and the initial snapshot below still lands on this subscriber's
	// latch and is picked up by the loop's first wake.
	subID, wake := h.supervisor.Subscribe()
	defer h.supervisor.Unsubscribe(subID)

	// rc lets each SSE write set a per-write deadline (see sseWrite).
	rc := http.NewResponseController(w)

	// Send initial comment to establish connection
	if err := sseWrite(rc, w, []byte(": connected\n\n")); err != nil {
		return
	}

	// Initial snapshot, as a normal data event -- same shape as every later one.
	if !h.writeProcessSnapshot(rc, w) {
		return
	}

	ticker := time.NewTicker(h.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := sseWrite(rc, w, []byte(": ping\n\n")); err != nil {
				return
			}
		case _, ok := <-wake:
			if !ok {
				// Supervisor stopped (CloseEvents): there is no more state to
				// report. End the stream; a reconnecting client gets connection
				// refused until a new daemon is up.
				return
			}

			// Trailing-edge debounce: absorb further wakes for the debounce
			// window before snapshotting, so a burst of transitions (several
			// processes changing together) collapses into one send instead of
			// one per change. wake is a capacity-1 dirty latch (level, not
			// edge): a change that lands after this absorb window closes but
			// before the snapshot below is taken re-arms the latch and is
			// picked up on the NEXT trip through this case -- so the LAST
			// change of any burst always yields a snapshot, just possibly on a
			// later tick, never lost.
			if d := h.processStreamDebounce(); d > 0 {
				if !h.absorbProcessWakes(r, wake, d) {
					return
				}
			}

			if !h.writeProcessSnapshot(rc, w) {
				return
			}
		}
	}
}

// processStreamMaxDebounceStretch bounds how long a continuous wake stream can
// keep restarting the trailing-edge debounce: after stretch×d of absorbing, a
// snapshot is forced even though the bus never went quiet. Without this cap, a
// process flapping faster than the debounce window (a sub-100ms crash/restart
// loop, a hot health check) would starve the stream of snapshots AND
// heartbeats indefinitely (codex C11 finding).
const processStreamMaxDebounceStretch = 5

// absorbProcessWakes implements StreamProcesses' trailing-edge debounce: it
// waits d after the wake that triggered it, restarting the wait on every
// further wake absorbed during that window, so the caller only snapshots once
// the bus has been quiet for a full window — or once the non-resetting
// max-latency bound (processStreamMaxDebounceStretch×d) fires, whichever comes
// first. Returns false if the request ended or the change bus closed while
// absorbing (the caller must then return without sending a final snapshot).
func (h *Handlers) absorbProcessWakes(r *http.Request, wake <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	deadline := time.NewTimer(processStreamMaxDebounceStretch * d)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return false
		case _, ok := <-wake:
			if !ok {
				return false
			}
			// Another change landed inside the window: restart the debounce
			// so the eventual snapshot reflects the LATEST member of the burst.
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(d)
		case <-timer.C:
			return true
		case <-deadline.C:
			// The bus never went quiet: snapshot anyway. The level latch keeps
			// any change landing after this point armed for the next trip.
			return true
		}
	}
}

// writeProcessSnapshot sends the current process list as one SSE data event.
// It reports false on a write failure (the caller must then tear the stream
// down); a JSON marshal failure -- unreachable in practice, since
// ProcessListResponse is all plain/marshalable fields -- is treated as a
// skip-this-tick no-op (mirrors StreamProxyRequests' marshal-error handling)
// rather than tearing down the whole stream over it.
func (h *Handlers) writeProcessSnapshot(rc *http.ResponseController, w http.ResponseWriter) bool {
	data, err := json.Marshal(processListResponse(h.supervisor))
	if err != nil {
		return true
	}
	return sseWrite(rc, w, []byte("data: "), data, []byte("\n\n")) == nil
}

// sseWrite writes an SSE line (a comment or a "data: ..." event, given as one
// or more byte-slice parts so callers never need to build an intermediate
// string) under a per-write deadline, then flushes and surfaces both the
// write and the flush error. A plain http.Flusher.Flush() cannot report a
// failed flush, which is why every SSE write goes through a
// *http.ResponseController instead of calling w.Write/flusher.Flush
// directly. The deadline error is deliberately ignored: a ResponseWriter
// that doesn't support write deadlines (e.g. httptest.ResponseRecorder)
// returns http.ErrNotSupported here, which is not itself a failed write —
// the subsequent Write/Flush still surfaces a real I/O problem. The single
// deadline deliberately bounds the whole frame, not each part: an SSE frame
// is a few hundred bytes, so a client that cannot absorb one within
// SSEWriteTimeout is dead and teardown is the desired outcome. Shared by
// StreamLogs, StreamProxyRequests (handlers.go) and StreamProcesses.
func sseWrite(rc *http.ResponseController, w http.ResponseWriter, parts ...[]byte) error {
	_ = rc.SetWriteDeadline(time.Now().Add(constants.SSEWriteTimeout))
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return rc.Flush()
}
