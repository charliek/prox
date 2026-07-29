package tui

import (
	"errors"
	"net/http"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/stream"
)

// StreamID names one of the TUI's data streams. Attach mode runs each of them
// on its own internal/stream.Loop and reports the loop's health per stream;
// local mode has no loops but reports the same identities so the status bar is
// rendered by one shared path.
type StreamID int

const (
	StreamLogs StreamID = iota
	StreamRequests
	// StreamProcesses is attach mode's process-state feed, which replaced the
	// polling tick in C12. Its health is reported like the others' but rendered
	// differently — see streamHealthSegments.
	StreamProcesses
)

// String is the name rendered in the status bar.
func (s StreamID) String() string {
	switch s {
	case StreamLogs:
		return "logs"
	case StreamRequests:
		return "requests"
	case StreamProcesses:
		return "processes"
	default:
		return "unknown"
	}
}

// allStreams fixes the status-bar segment order. Iterating this rather than the
// health map keeps the bar from reshuffling between renders.
var allStreams = []StreamID{StreamLogs, StreamRequests, StreamProcesses}

// StreamStatusMsg carries one stream.Loop state transition into the models.
// Both Update loops route it through BaseModel.handleStreamStatus.
type StreamStatusMsg struct {
	Stream StreamID
	Status stream.Status
}

// APIStatusError is the structural view of a daemon error response that the
// stream reconnect policies discriminate on. *cli.APIError implements it;
// internal/cli imports internal/tui, so the concrete type cannot be named here.
type APIStatusError interface {
	// StatusCode is the HTTP status of the failed response.
	StatusCode() int
	// ErrorCode is the machine-readable code from the JSON error body, empty
	// when the body carried none.
	ErrorCode() string
}

// classifyStreamError is the shared attach-mode reconnect policy: an
// authentication or authorization failure will not fix itself by retrying, so
// it ends the loop; everything else (dial failures, hangups, 5xx) is transient.
func classifyStreamError(err error) stream.Classification {
	var apiErr APIStatusError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode() {
		case http.StatusUnauthorized, http.StatusForbidden:
			return stream.ClassTerminal
		}
	}
	return stream.ClassTransient
}

// classifyRequestsStreamError extends the shared policy with the one condition
// unique to the requests stream: the daemon runs fine with the proxy disabled,
// so a 503 PROXY_NOT_ENABLED is a standing "no such feed" rather than an
// outage. The loop parks instead of retrying on a timer, and the status bar
// renders it passively.
func classifyRequestsStreamError(err error) stream.Classification {
	var apiErr APIStatusError
	if errors.As(err, &apiErr) &&
		apiErr.StatusCode() == http.StatusServiceUnavailable &&
		apiErr.ErrorCode() == domain.ErrCodeProxyNotEnabled {
		return stream.ClassUnavailable
	}
	return classifyStreamError(err)
}

// classifyProcessesStreamError extends the shared policy with the one condition
// unique to the processes stream: version skew. A daemon predating plan 017 has
// no /api/v1/processes/stream at all and answers 404, which no amount of
// retrying fixes — only replacing the daemon does. The loop parks
// (ClassUnavailable) rather than hammering a route that does not exist, and the
// attach model renders the parked state as a standing connection error naming
// the old daemon (see ClientModel.noteProcessesStreamHealth).
func classifyProcessesStreamError(err error) stream.Classification {
	var apiErr APIStatusError
	if errors.As(err, &apiErr) && apiErr.StatusCode() == http.StatusNotFound {
		return stream.ClassUnavailable
	}
	return classifyStreamError(err)
}

// handleStreamStatus records a stream's new health. Both maps are allocated by
// newBaseModel.
//
// streamDropped latches on StateReconnecting so the Syncing that follows a drop
// keeps rendering "reconnecting…": the loop's Reconnecting → Syncing → OK
// sequence would otherwise flicker through a third wording on every retry. It
// clears on any state that is neither Reconnecting nor Syncing, which leaves
// the startup Connecting → Syncing → OK path silent.
func (b *BaseModel) handleStreamStatus(msg StreamStatusMsg) {
	b.streamHealth[msg.Stream] = msg.Status

	switch msg.Status.State {
	case stream.StateReconnecting:
		b.streamDropped[msg.Stream] = true
	case stream.StateSyncing:
		// Keep the latch: this is either the post-drop retry (stay loud) or a
		// first sync that never set it (stay silent).
	default:
		delete(b.streamDropped, msg.Stream)
	}
}

// streamHealthSegments renders the status-bar segments for every stream that is
// not healthy, in allStreams order. A healthy, still-connecting or
// never-reported stream contributes nothing: startup must not be noisy.
//
// StateUnavailable is deliberately passive ("requests: n/a", no warning sign) —
// it means the feed does not exist here, not that something broke.
//
// StreamProcesses contributes NO segment, ever (C12). It is attach mode's
// global-liveness signal rather than one feed among three: losing it means the
// daemon is gone, which the status line already reports as "Connection error
// (retrying...)" via ClientModel.connectionError, derived from exactly this
// stream's health. A segment here would report the same outage twice, in two
// wordings. Local mode never degrades it at all (newBaseModel starts every
// stream OK and nothing moves the processes entry off it).
func (b *BaseModel) streamHealthSegments() []string {
	var segs []string
	for _, id := range allStreams {
		if id == StreamProcesses {
			continue
		}
		st, ok := b.streamHealth[id]
		if !ok {
			continue
		}
		switch {
		// A post-drop Syncing keeps the reconnecting wording (see the latch in
		// handleStreamStatus); a first-connect Syncing stays silent.
		case st.State == stream.StateReconnecting,
			st.State == stream.StateSyncing && b.streamDropped[id]:
			segs = append(segs, "⚠ "+id.String()+": reconnecting…")
		case st.State == stream.StateUnavailable:
			segs = append(segs, id.String()+": n/a")
		case st.State == stream.StateClosed:
			segs = append(segs, "⚠ "+id.String()+": disconnected")
		}
	}
	return segs
}
