package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/charliek/prox/internal/domain"
)

// StreamLogs handles GET /api/v1/logs/stream (SSE)
func (h *Handlers) StreamLogs(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Check if flusher is available
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

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

	// Send initial comment to establish connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

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
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				// Client disconnected or write failed - logged for debugging
				log.Printf("SSE write error (client likely disconnected): %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
