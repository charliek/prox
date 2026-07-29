package tui

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
	"github.com/charliek/prox/internal/supervisor"
)

// Run starts the TUI application.
//
// shutdownCh, when it closes, quits the program: this is how an out-of-band
// shutdown request (POST /shutdown, via the coordinator's trigger channel)
// reaches a --tui daemon, which otherwise blocks here forever. On quit -- whether
// triggered this way or by the user pressing q/Ctrl-C -- Run returns and the
// caller runs the normal shutdown sequence, so both routes are identical.
func Run(sup *supervisor.Supervisor, logMgr *logs.Manager, reqMgr *proxy.RequestManager, shutdownCh <-chan struct{}) error {
	model := NewModel(sup, logMgr)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	// Quit the program when an external shutdown is requested. The goroutine also
	// exits when ctx is cancelled (after p.Run returns), so a hand-quit TUI never
	// leaks it.
	go func() {
		select {
		case <-shutdownCh:
			p.Quit()
		case <-ctx.Done():
		}
	}()

	// Subscribe to logs before starting the forwarder
	subID, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		// Don't cancel the context here - proxy request forwarding should still work.
		p.Send(LogEntryMsg(systemLogEntry("Error subscribing to logs: " + err.Error())))
	} else {
		go forwardLogs(ctx, p.Send, ch)
	}

	// Subscribe to proxy requests if available
	var reqSubID string
	if reqMgr != nil {
		sub := reqMgr.Subscribe(proxy.RequestFilter{})
		reqSubID = sub.ID
		go forwardProxyRequests(ctx, p.Send, sub.Ch)
	}

	_, runErr := p.Run()

	// Cleanup: cancel context and unsubscribe
	cancel()
	if subID != "" {
		logMgr.Unsubscribe(subID)
	}
	if reqSubID != "" && reqMgr != nil {
		reqMgr.Unsubscribe(reqSubID)
	}

	return runErr
}

// systemLogEntry builds the synthetic "system" log line the TUI uses to show
// itself a message in the log pane.
func systemLogEntry(line string) domain.LogEntry {
	return domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "system",
		Stream:    domain.StreamStderr,
		Line:      line,
	}
}

// forwardSubscription pumps a local-mode subscription channel into the TUI. It
// exits when the context is cancelled or the channel is closed. toMsg wraps one
// element as the message the models expect; closedLine is the system log line
// recorded if the channel ends on its own.
func forwardSubscription[T any](ctx context.Context, send func(tea.Msg), ch <-chan T, id StreamID, closedLine string, toMsg func(T) tea.Msg) {
	for {
		select {
		case <-ctx.Done():
			return
		case v, ok := <-ch:
			if !ok {
				reportLocalStreamClosed(ctx, send, id, closedLine)
				return
			}
			send(toMsg(v))
		}
	}
}

// forwardLogs forwards log entries from the subscription channel to the TUI.
func forwardLogs(ctx context.Context, send func(tea.Msg), ch <-chan domain.LogEntry) {
	forwardSubscription(ctx, send, ch, StreamLogs, "Log stream closed",
		func(entry domain.LogEntry) tea.Msg { return LogEntryMsg(entry) })
}

// forwardProxyRequests forwards proxy requests from the subscription channel to
// the TUI.
func forwardProxyRequests(ctx context.Context, send func(tea.Msg), ch <-chan proxy.RequestRecord) {
	forwardSubscription(ctx, send, ch, StreamRequests, "Proxy request stream closed",
		func(req proxy.RequestRecord) tea.Msg { return ProxyRequestMsg(req) })
}

// reportLocalStreamClosed surfaces a local-mode subscription channel that ended
// on its own. Local mode has no reconnect loop, so the feed is gone for the rest
// of the session and the user has to be told: the status bar marks the stream
// closed and one system log line records it. A close observed during shutdown
// (ctx already cancelled, which the select can lose the race to) is expected and
// stays silent.
func reportLocalStreamClosed(ctx context.Context, send func(tea.Msg), id StreamID, line string) {
	if ctx.Err() != nil {
		return
	}
	send(StreamStatusMsg{Stream: id, Status: stream.Status{State: stream.StateClosed}})
	send(LogEntryMsg(systemLogEntry(line)))
}

// TUIClient is the interface for TUI client mode API interactions.
// It consolidates all API operations needed by the TUI client.
//
// The stream methods are attempt-shaped rather than channel-shaped: each call is
// one connect-and-consume attempt owned by an internal/stream.Loop, which needs
// the terminal error to classify it. The channel forms remain on *cli.Client for
// the --follow commands.
type TUIClient interface {
	GetProcesses() (*api.ProcessListResponse, error)
	RestartProcess(name string) error
	// ConsumeLogs takes an onHandshake hook alongside onConnect: the logs sync
	// (C9) must learn the server's log epoch before it can decide how to
	// backfill, and that epoch rides a named handshake frame on the stream
	// itself. A daemon that sends none never fires it.
	ConsumeLogs(ctx context.Context, params domain.LogParams, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error
	ConsumeProxyRequests(ctx context.Context, params domain.ProxyRequestParams, onConnect func(), onEvent func(api.ProxyRequestResponse)) error
	GetProxyRequest(id string, includeBody bool) (*api.ProxyRequestDetailResponse, error)

	// GetProxyRequests and GetLogs fetch what the requests and logs streams
	// synchronize against on every connect. They are the ctx-taking non-stream
	// methods: each fetch is owned by a stream attempt and must be abandoned
	// the moment that attempt ends.
	GetProxyRequests(ctx context.Context, params domain.ProxyRequestParams) (*api.ProxyRequestsResponse, error)
	GetLogs(ctx context.Context, params domain.LogParams) (*api.LogsResponse, error)
}

// RunClient starts the TUI application in client mode (connected via API)
func RunClient(client TUIClient) error {
	model := NewClientModel(client)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	go runClientStreams(ctx, client, p.Send)

	_, err := p.Run()

	// Cleanup: cancel context to stop the stream loops
	cancel()

	return err
}

// runClientStreams runs one reconnect loop per attach-mode stream and blocks
// until every loop has ended, which happens when ctx is cancelled (on quit) or a
// loop's error is classified terminal. send is the program's message sink,
// injected so the wiring is testable without a live *tea.Program.
func runClientStreams(ctx context.Context, client TUIClient, send func(tea.Msg)) {
	// One log-sync session for the whole attach run: it carries the cursor and
	// epoch ACROSS reconnects, which is what lets attempt N+1 resume where
	// attempt N stopped instead of re-fetching the world (C9).
	logsSess := newLogsSyncSession()

	loops := []*stream.Loop{
		// Neither data stream is a pure stream consumer: each connect
		// synchronizes against a REST fetch before reporting OK (C6, C9).
		streamLoop(StreamLogs, send, classifyStreamError, func(ctx context.Context, markSynced func()) error {
			return consumeLogsWithSync(ctx, client, logsSess, send, markSynced)
		}),
		streamLoop(StreamRequests, send, classifyRequestsStreamError, func(ctx context.Context, markSynced func()) error {
			return consumeRequestsWithSync(ctx, client, send, markSynced)
		}),
	}

	var wg sync.WaitGroup
	wg.Add(len(loops))
	for _, l := range loops {
		go func() {
			defer wg.Done()
			l.Run(ctx)
		}()
	}
	wg.Wait()
}

// streamLoop builds one attach-mode reconnect loop: consume is a single
// connect-and-consume attempt, classify is the stream's reconnect policy, and
// every transition reaches the models as a StreamStatusMsg for id.
//
// markSynced is handed to the attempt rather than called for it. Both of
// today's attach streams call it from inside their sync protocol, after the
// batch barrier, so an attempt that connects but cannot synchronize stays in
// Syncing and can never flash OK or clear the outage warning (codex C5
// finding).
func streamLoop(id StreamID, send func(tea.Msg), classify func(error) stream.Classification, consume func(ctx context.Context, markSynced func()) error) *stream.Loop {
	return stream.NewLoop(stream.Config{
		Attempt:  consume,
		Classify: classify,
		OnStatus: func(s stream.Status) {
			send(StreamStatusMsg{Stream: id, Status: s})
		},
	})
}

// parseStreamTimestamp parses a server-supplied event timestamp. A malformed one
// falls back to now and emits a warning line naming what, so a server-side
// timestamp bug stays visible instead of being papered over.
func parseStreamTimestamp(send func(tea.Msg), what, raw string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		send(LogEntryMsg(systemLogEntry("Warning: failed to parse " + what + " timestamp: " + err.Error())))
		return time.Now()
	}
	return ts
}

// sendStreamedLogEntry converts one streamed log entry and delivers it.
func sendStreamedLogEntry(send func(tea.Msg), entry api.LogEntryResponse) {
	send(LogEntryMsg(streamedLogEntry(send, entry)))
}

// streamedLogEntry converts one wire log entry without delivering it, so the
// sync protocol can buffer live entries and convert backfill entries (same wire
// type) through exactly the same mapping. send is still needed: a malformed
// timestamp warns through the log pane.
//
// Seq is carried through: it is the server ingest sequence the sync protocol's
// cursor arithmetic runs on, and is unrelated to the TUI-local DisplaySeq that
// BaseModel stamps on arrival (D7).
func streamedLogEntry(send func(tea.Msg), entry api.LogEntryResponse) domain.LogEntry {
	return domain.LogEntry{
		Timestamp: parseStreamTimestamp(send, "log", entry.Timestamp),
		Process:   entry.Process,
		Stream:    domain.Stream(entry.Stream),
		Line:      entry.Line,
		Seq:       entry.Seq,
	}
}

// sendStreamedProxyRequest converts one streamed proxy request and delivers it.
func sendStreamedProxyRequest(send func(tea.Msg), req api.ProxyRequestResponse) {
	send(ProxyRequestMsg(streamedProxyRequest(send, req)))
}

// streamedProxyRequest converts one wire proxy request to a record without
// delivering it, so the sync protocol can buffer live events and convert
// snapshot records (same wire type) through exactly the same mapping. send is
// still needed: a malformed timestamp warns through the log pane.
func streamedProxyRequest(send func(tea.Msg), req api.ProxyRequestResponse) proxy.RequestRecord {
	return proxy.RequestRecord{
		ID:         req.ID,
		Timestamp:  parseStreamTimestamp(send, "proxy request", req.Timestamp),
		Method:     req.Method,
		URL:        req.URL,
		Subdomain:  req.Subdomain,
		StatusCode: req.StatusCode,
		Duration:   time.Duration(req.DurationMs) * time.Millisecond,
		RemoteAddr: req.RemoteAddr,
		InFlight:   req.InFlight,
	}
}
