package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
)

// fakeAPIError implements APIStatusError, standing in for *cli.APIError, which
// internal/tui cannot import (internal/cli imports internal/tui).
type fakeAPIError struct {
	status int
	code   string
}

func (e *fakeAPIError) Error() string     { return fmt.Sprintf("api error %d", e.status) }
func (e *fakeAPIError) StatusCode() int   { return e.status }
func (e *fakeAPIError) ErrorCode() string { return e.code }

// readyClientModel builds a ClientModel wide enough that the status bar renders
// without wrapping the health segments.
func readyClientModel() ClientModel {
	m := NewClientModel(&stubTUIClient{})
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	return nm.(ClientModel)
}

func statusMsg(id StreamID, state stream.State) StreamStatusMsg {
	return StreamStatusMsg{Stream: id, Status: stream.Status{State: state}}
}

// TestStreamStatus_ReconnectingRendersWarning pins that a reconnecting logs
// stream both updates the health map and reaches the rendered status bar.
func TestStreamStatus_ReconnectingRendersWarning(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamLogs, stream.StateReconnecting))

	assert.Equal(t, stream.StateReconnecting, m.streamHealth[StreamLogs].State)
	assert.Contains(t, m.View(), "⚠ logs: reconnecting…")
	assert.NotContains(t, m.View(), "requests:")
}

// TestStreamStatus_RequestsUnavailableIsPassive pins the one non-warning
// degraded rendering: the proxy simply is not enabled.
func TestStreamStatus_RequestsUnavailableIsPassive(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamRequests, stream.StateUnavailable))

	view := m.View()
	assert.Contains(t, view, "requests: n/a")
	assert.NotContains(t, view, "⚠")
}

// TestStreamStatus_OKRendersNothing pins that a healthy stream contributes no
// status-bar segment, including after it recovers from a drop.
func TestStreamStatus_OKRendersNothing(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamLogs, stream.StateOK))
	assert.NotContains(t, m.View(), "logs:")

	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateReconnecting))
	require.Contains(t, m.View(), "⚠ logs: reconnecting…")

	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateOK))
	assert.NotContains(t, m.View(), "logs:")
	assert.False(t, m.streamDropped[StreamLogs], "recovery clears the drop latch")
}

// TestStreamStatus_InitialConnectAndSyncSilent pins that startup is quiet:
// nothing has dropped yet, so neither Connecting nor the first Syncing renders.
func TestStreamStatus_InitialConnectAndSyncSilent(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamLogs, stream.StateConnecting))
	assert.NotContains(t, m.View(), "logs:")

	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateSyncing))
	assert.NotContains(t, m.View(), "logs:")
}

// TestStreamStatus_PostDropSyncingRendersReconnecting pins the latch: the
// Syncing the loop emits on each retry keeps the "reconnecting…" wording rather
// than flickering through a third phrase.
func TestStreamStatus_PostDropSyncingRendersReconnecting(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamLogs, stream.StateConnecting))
	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateReconnecting))
	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateSyncing))

	assert.Contains(t, m.View(), "⚠ logs: reconnecting…")
}

// TestStreamStatus_ClosedRendersDisconnected covers the local-mode terminal
// state: the feed is gone for the session, so it stays on the bar.
func TestStreamStatus_ClosedRendersDisconnected(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamRequests, stream.StateClosed))
	assert.Contains(t, m.View(), "⚠ requests: disconnected")
}

// TestStreamStatus_SegmentOrderIsFixed pins that two degraded streams render in
// allStreams order rather than map order.
func TestStreamStatus_SegmentOrderIsFixed(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamRequests, stream.StateUnavailable))
	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateReconnecting))

	view := m.View()
	assert.Less(t, strings.Index(view, "logs:"), strings.Index(view, "requests:"))
}

// TestLocalModel_StreamsStartHealthy pins the local-mode initialization: every
// stream is OK up front, so the bar is clean until a subscription dies.
func TestLocalModel_StreamsStartHealthy(t *testing.T) {
	m := newTestModel()
	for _, id := range allStreams {
		assert.Equal(t, stream.StateOK, m.streamHealth[id].State, "stream %s", id)
	}
	assert.Empty(t, m.streamHealthSegments())
}

// TestLocalModel_HandlesStreamStatusMsg pins that the local Update loop routes
// the message too — both models share the rendering path.
func TestLocalModel_HandlesStreamStatusMsg(t *testing.T) {
	m := updateModel(newTestModel(), tea.WindowSizeMsg{Width: 200, Height: 20})
	m = updateModel(m, statusMsg(StreamLogs, stream.StateClosed))

	assert.Equal(t, stream.StateClosed, m.streamHealth[StreamLogs].State)
	assert.Contains(t, m.View(), "⚠ logs: disconnected")
}

// --- classifier ---

// TestClassifyStreamError covers the shared policy: auth failures are terminal,
// everything else is worth retrying. The wrapped case proves errors.As traversal.
func TestClassifyStreamError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want stream.Classification
	}{
		{"unauthorized", &fakeAPIError{status: http.StatusUnauthorized}, stream.ClassTerminal},
		{"forbidden", &fakeAPIError{status: http.StatusForbidden}, stream.ClassTerminal},
		{"wrapped unauthorized", fmt.Errorf("stream: %w", &fakeAPIError{status: http.StatusUnauthorized}), stream.ClassTerminal},
		{"server error", &fakeAPIError{status: http.StatusInternalServerError}, stream.ClassTransient},
		{"network error", errors.New("dial tcp: connection refused"), stream.ClassTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyStreamError(tt.err))
		})
	}
}

// TestClassifyRequestsStreamError pins the requests-only addition: a 503
// carrying PROXY_NOT_ENABLED parks the loop; a bare 503 (daemon starting up)
// does not.
func TestClassifyRequestsStreamError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want stream.Classification
	}{
		{
			"proxy not enabled",
			&fakeAPIError{status: http.StatusServiceUnavailable, code: domain.ErrCodeProxyNotEnabled},
			stream.ClassUnavailable,
		},
		{
			"503 without the code",
			&fakeAPIError{status: http.StatusServiceUnavailable},
			stream.ClassTransient,
		},
		{
			"code on another status",
			&fakeAPIError{status: http.StatusInternalServerError, code: domain.ErrCodeProxyNotEnabled},
			stream.ClassTransient,
		},
		{"unauthorized", &fakeAPIError{status: http.StatusUnauthorized}, stream.ClassTerminal},
		{"network error", errors.New("dial tcp: connection refused"), stream.ClassTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyRequestsStreamError(tt.err))
		})
	}
}

// --- local-mode forwarders ---

// msgCollector is a thread-safe stand-in for (*tea.Program).Send.
type msgCollector struct {
	mu   sync.Mutex
	msgs []tea.Msg
	ch   chan tea.Msg
}

func newMsgCollector() *msgCollector {
	return &msgCollector{ch: make(chan tea.Msg, 64)}
}

func (c *msgCollector) send(msg tea.Msg) {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
	select {
	case c.ch <- msg:
	default:
	}
}

func (c *msgCollector) all() []tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tea.Msg(nil), c.msgs...)
}

// await blocks until a message satisfying match arrives or the deadline passes.
func (c *msgCollector) await(t *testing.T, match func(tea.Msg) bool) tea.Msg {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-c.ch:
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timed out waiting for message; got %v", c.all())
			return nil
		}
	}
}

// syncHarness runs one attach stream's connect-and-consume sync attempts: it
// collects the messages an attempt delivers, counts markSynced calls, and
// publishes syncedCh so a scripted stream can wait for the sync barrier before
// streaming more events. attempt is the protocol under test, so the same
// harness drives both consumeRequestsWithSync and consumeLogsWithSync.
type syncHarness struct {
	collector *msgCollector
	syncedCh  chan struct{}
	syncedN   atomic.Int32
	once      sync.Once
	attempt   func(ctx context.Context, client TUIClient, send func(tea.Msg), markSynced func()) error
}

func newSyncHarness(attempt func(context.Context, TUIClient, func(tea.Msg), func()) error) *syncHarness {
	return &syncHarness{collector: newMsgCollector(), syncedCh: make(chan struct{}), attempt: attempt}
}

func (h *syncHarness) markSynced() {
	h.syncedN.Add(1)
	h.once.Do(func() { close(h.syncedCh) })
}

func (h *syncHarness) run(ctx context.Context, client TUIClient) error {
	return h.attempt(ctx, client, h.collector.send, h.markSynced)
}

// runInBackground starts one attempt and returns a channel carrying its error.
func (h *syncHarness) runInBackground(ctx context.Context, client TUIClient) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- h.run(ctx, client) }()
	return errCh
}

// awaitSync blocks until the attempt delivers its sync batch, of whichever
// message type the stream under test carries.
func awaitSync[T tea.Msg](t *testing.T, h *syncHarness) T {
	t.Helper()
	msg := h.collector.await(t, func(m tea.Msg) bool { _, ok := m.(T); return ok })
	return msg.(T)
}

// TestForwardLogs_ChannelCloseReportsClosed pins the W7 hardening: a local
// subscription that dies on its own marks the stream closed and says so once.
func TestForwardLogs_ChannelCloseReportsClosed(t *testing.T) {
	collector := newMsgCollector()
	ch := make(chan domain.LogEntry)
	close(ch)

	forwardLogs(context.Background(), collector.send, ch)

	msgs := collector.all()
	require.Len(t, msgs, 2)
	assert.Equal(t, StreamStatusMsg{Stream: StreamLogs, Status: stream.Status{State: stream.StateClosed}}, msgs[0])
	entry, ok := msgs[1].(LogEntryMsg)
	require.True(t, ok)
	assert.Equal(t, "system", entry.Process)
	assert.Contains(t, entry.Line, "Log stream closed")
}

// TestForwardLogs_CancelledCloseIsSilent pins the other half: a close during
// shutdown is expected and must not spam the log pane or the status bar.
func TestForwardLogs_CancelledCloseIsSilent(t *testing.T) {
	collector := newMsgCollector()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan domain.LogEntry)
	close(ch)

	forwardLogs(ctx, collector.send, ch)

	assert.Empty(t, collector.all())
}

// TestForwardProxyRequests_ChannelCloseReportsClosed mirrors the logs case for
// the requests subscription.
func TestForwardProxyRequests_ChannelCloseReportsClosed(t *testing.T) {
	collector := newMsgCollector()
	ch := make(chan proxy.RequestRecord)
	close(ch)

	forwardProxyRequests(context.Background(), collector.send, ch)

	msgs := collector.all()
	require.Len(t, msgs, 2)
	assert.Equal(t, StreamStatusMsg{Stream: StreamRequests, Status: stream.Status{State: stream.StateClosed}}, msgs[0])
	entry, ok := msgs[1].(LogEntryMsg)
	require.True(t, ok)
	assert.Contains(t, entry.Line, "Proxy request stream closed")
}

// TestForwardProxyRequests_CancelledCloseIsSilent pins the shutdown path.
func TestForwardProxyRequests_CancelledCloseIsSilent(t *testing.T) {
	collector := newMsgCollector()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan proxy.RequestRecord)
	close(ch)

	forwardProxyRequests(ctx, collector.send, ch)

	assert.Empty(t, collector.all())
}

// TestForwardLogs_DeliversEntries keeps the happy path covered after the send
// injection.
func TestForwardLogs_DeliversEntries(t *testing.T) {
	collector := newMsgCollector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan domain.LogEntry, 1)
	ch <- domain.LogEntry{Process: "web", Line: "hello"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardLogs(ctx, collector.send, ch)
	}()

	msg := collector.await(t, func(m tea.Msg) bool { _, ok := m.(LogEntryMsg); return ok })
	assert.Equal(t, "hello", domain.LogEntry(msg.(LogEntryMsg)).Line)
	cancel()
	<-done
}

// --- attach-mode loop wiring ---

// startClientStreams runs runClientStreams in the background, cancelling it and
// waiting for both loops to exit when the test ends.
func startClientStreams(t *testing.T, client TUIClient, send func(tea.Msg)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClientStreams(ctx, client, send)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestRunClientStreams_ForwardsEventsAndStatus pins the wiring end to end: a
// streamed entry reaches the model and the loop's own transitions arrive as
// StreamStatusMsg for the right stream.
//
// Which message carries the entry is timing-dependent since C9: an entry that
// races the backfill fetch is buffered into the LogsSyncMsg, and one that
// arrives after the sync barrier passes through as a LogEntryMsg. Both are
// correct deliveries, so the assertion accepts either.
func TestRunClientStreams_ForwardsEventsAndStatus(t *testing.T) {
	collector := newMsgCollector()
	client := &stubTUIClient{
		consumeLogs: func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
			onConnect()
			onHandshake(api.HandshakeResponse{StreamID: stubLogEpoch})
			onEvent(api.LogEntryResponse{
				Timestamp: time.Now().Format(time.RFC3339Nano),
				Process:   "web",
				Stream:    "stdout",
				Line:      "streamed",
				Seq:       1,
			})
			<-ctx.Done()
			return ctx.Err()
		},
	}

	startClientStreams(t, client, collector.send)

	collector.await(t, func(m tea.Msg) bool { return logMsgCarriesLine(m, "streamed") })

	// markSynced now rides the sync barrier: OK follows the backfill.
	collector.await(t, func(m tea.Msg) bool {
		s, ok := m.(StreamStatusMsg)
		return ok && s.Stream == StreamLogs && s.Status.State == stream.StateOK
	})
}

// logMsgCarriesLine reports whether msg delivers a log line with the given
// text, through either attach-mode path (live entry or sync batch).
func logMsgCarriesLine(msg tea.Msg, line string) bool {
	switch v := msg.(type) {
	case LogEntryMsg:
		return domain.LogEntry(v).Line == line
	case LogsSyncMsg:
		for _, e := range v.Entries {
			if e.Line == line {
				return true
			}
		}
	}
	return false
}

// TestRunClientStreams_AttemptErrorReportsReconnecting pins that an attempt
// failure surfaces as a status message instead of silently killing the feed.
func TestRunClientStreams_AttemptErrorReportsReconnecting(t *testing.T) {
	collector := newMsgCollector()
	client := &stubTUIClient{
		// onConnect deliberately not called: a dead-on-arrival dial.
		consumeLogs: func(context.Context, func(), func(api.HandshakeResponse), func(api.LogEntryResponse)) error {
			return errors.New("connection refused")
		},
	}

	startClientStreams(t, client, collector.send)

	msg := collector.await(t, func(m tea.Msg) bool {
		s, ok := m.(StreamStatusMsg)
		return ok && s.Stream == StreamLogs && s.Status.State == stream.StateReconnecting
	})
	assert.EqualError(t, msg.(StreamStatusMsg).Status.Err, "connection refused")

	// A dial that never establishes a connection must never report OK: OK
	// clears the outage warning, so a spurious one would flash the UI healthy
	// mid-outage on every retry (codex C5 finding).
	for _, m := range collector.all() {
		if s, ok := m.(StreamStatusMsg); ok && s.Stream == StreamLogs {
			assert.NotEqual(t, stream.StateOK, s.Status.State,
				"failed dial must not emit OK")
		}
	}
}

// TestRunClientStreams_ProxyNotEnabledParks pins the classifier reaching the
// requests loop: the proxy being off is reported as unavailable, not as an
// endless reconnect.
func TestRunClientStreams_ProxyNotEnabledParks(t *testing.T) {
	collector := newMsgCollector()
	client := &stubTUIClient{
		consumeRequests: func(context.Context, func(), func(api.ProxyRequestResponse)) error {
			return &fakeAPIError{status: http.StatusServiceUnavailable, code: domain.ErrCodeProxyNotEnabled}
		},
	}

	startClientStreams(t, client, collector.send)

	collector.await(t, func(m tea.Msg) bool {
		s, ok := m.(StreamStatusMsg)
		return ok && s.Stream == StreamRequests && s.Status.State == stream.StateUnavailable
	})
}

// TestSendStreamedLogEntry_BadTimestampWarns pins the conversion factored out of
// the old forwarder: a malformed timestamp still delivers the entry, preceded by
// a system warning.
func TestSendStreamedLogEntry_BadTimestampWarns(t *testing.T) {
	collector := newMsgCollector()
	sendStreamedLogEntry(collector.send, api.LogEntryResponse{
		Timestamp: "not-a-timestamp",
		Process:   "web",
		Stream:    "stdout",
		Line:      "hello",
	})

	msgs := collector.all()
	require.Len(t, msgs, 2)
	assert.Contains(t, domain.LogEntry(msgs[0].(LogEntryMsg)).Line, "failed to parse log timestamp")
	entry := domain.LogEntry(msgs[1].(LogEntryMsg))
	assert.Equal(t, "hello", entry.Line)
	assert.False(t, entry.Timestamp.IsZero(), "malformed timestamps fall back to now")
}

// TestSendStreamedProxyRequest_Converts pins the requests conversion, including
// the millisecond-to-Duration mapping.
func TestSendStreamedProxyRequest_Converts(t *testing.T) {
	collector := newMsgCollector()
	ts := time.Now().UTC().Truncate(time.Millisecond)
	sendStreamedProxyRequest(collector.send, api.ProxyRequestResponse{
		ID:         "req-1",
		Timestamp:  ts.Format(time.RFC3339Nano),
		Method:     "GET",
		URL:        "/x",
		StatusCode: 200,
		DurationMs: 25,
		InFlight:   true,
	})

	msgs := collector.all()
	require.Len(t, msgs, 1)
	record := proxy.RequestRecord(msgs[0].(ProxyRequestMsg))
	assert.Equal(t, "req-1", record.ID)
	assert.True(t, ts.Equal(record.Timestamp))
	assert.Equal(t, 25*time.Millisecond, record.Duration)
	assert.True(t, record.InFlight)
}
