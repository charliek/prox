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
// StreamLogs and StreamProxyRequests (handlers.go).
func sseWrite(rc *http.ResponseController, w http.ResponseWriter, parts ...[]byte) error {
	_ = rc.SetWriteDeadline(time.Now().Add(constants.SSEWriteTimeout))
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return rc.Flush()
}
