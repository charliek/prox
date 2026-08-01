package tui

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
)

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

// TUIClient is the interface for TUI client mode API interactions.
// It consolidates all API operations needed by the TUI client.
//
// The stream methods are attempt-shaped rather than channel-shaped: each call is
// one connect-and-consume attempt owned by an internal/stream.Loop, which needs
// the terminal error to classify it. The channel forms remain on *cli.Client for
// the --follow commands.
// GetProcesses is deliberately absent: attach mode learns process state from
// the processes stream alone (C12), so nothing here polls REST /processes. The
// method stays on *cli.Client for the one-shot CLI commands.
type TUIClient interface {
	RestartProcess(name string) error
	// ConsumeLogs takes an onHandshake hook alongside onConnect: the logs sync
	// (C9) must learn the server's log epoch before it can decide how to
	// backfill, and that epoch rides a named handshake frame on the stream
	// itself. A daemon that sends none never fires it.
	ConsumeLogs(ctx context.Context, params domain.LogParams, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error
	ConsumeProxyRequests(ctx context.Context, params domain.ProxyRequestParams, onConnect func(), onEvent func(api.ProxyRequestResponse)) error
	// ConsumeProcesses carries a full process-list snapshot per event, so it
	// needs neither params nor a handshake: there is nothing to filter and
	// nothing to resume from.
	ConsumeProcesses(ctx context.Context, onConnect func(), onEvent func(api.ProcessListResponse)) error
	GetProxyRequest(id string, includeBody bool) (*api.ProxyRequestDetailResponse, error)

	// GetProxyRequests and GetLogs fetch what the requests and logs streams
	// synchronize against on every connect. They are the ctx-taking non-stream
	// methods: a sync fetch is owned by a stream attempt and must be abandoned
	// the moment that attempt ends. GetProxyRequests has one other caller,
	// requests scroll-back (fetchOlderRequests), which belongs to no stream
	// attempt and so supplies its own timeout bound instead.
	GetProxyRequests(ctx context.Context, params domain.ProxyRequestParams) (*api.ProxyRequestsResponse, error)
	GetLogs(ctx context.Context, params domain.LogParams) (*api.LogsResponse, error)
}

// RunClient starts the TUI application in client mode (connected via API).
//
// opts carries the caller's wording and, for a caller that supervises processes,
// its shutdown channel. RunClient starts no goroutine of its own to quit the
// program: the wait on opts.ShutdownCh is a command the model returns from Init
// (see ClientOptions.ShutdownCh), so every quit — user keypress or out-of-band
// request — arrives as a message through Update.
func RunClient(client TUIClient, opts ClientOptions) error {
	settings, warnings := LoadSettings()
	if settings.Theme != "" {
		_, themeWarnings := SetThemeByName(settings.Theme)
		warnings = append(warnings, themeWarnings...)
	}

	model := NewClientModel(client, opts)
	model.settings = settings
	model.startupWarnings = warnings
	if model.projectName == "" {
		model.projectName = resolveProjectName(opts.ProjectName)
	}

	// WithMouseCellMotion enables menu and content mouse routing (WS11).
	// viewport.MouseWheelEnabled is false on the model so bubbles does not
	// double-scroll wheel events (Codex #5).
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

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

	// Re-probing is SYMMETRIC (codex C12 finding): each loop's observer wakes
	// every OTHER loop on a reconnect. The loops slice is filled before any
	// Run starts, and OnStatus only fires from a Run goroutine, so the
	// closures' reads are ordered after the write by goroutine creation.
	var loops []*stream.Loop
	probeOthers := func(self int) func(stream.Status) {
		return reprobeOnReconnect(func() {
			for i, l := range loops {
				if i != self {
					l.Probe()
				}
			}
		})
	}

	// Neither data stream is a pure stream consumer: each connect
	// synchronizes against a REST fetch before reporting OK (C6, C9).
	logsLoop := streamLoop(StreamLogs, send, classifyStreamError, probeOthers(0), func(ctx context.Context, markSynced func()) error {
		return consumeLogsWithSync(ctx, client, logsSess, send, markSynced)
	})
	requestsLoop := streamLoop(StreamRequests, send, classifyRequestsStreamError, probeOthers(1), func(ctx context.Context, markSynced func()) error {
		return consumeRequestsWithSync(ctx, client, send, markSynced)
	})
	// The processes stream IS a pure consumer (snapshot-per-event); it also
	// doubles as the run's daemon-liveness signal (see ClientModel).
	processesLoop := streamLoop(StreamProcesses, send, classifyProcessesStreamError,
		probeOthers(2),
		func(ctx context.Context, markSynced func()) error {
			return consumeProcesses(ctx, client, send, markSynced)
		})

	loops = []*stream.Loop{logsLoop, requestsLoop, processesLoop}

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
// markSynced is handed to the attempt rather than called for it. The two
// synchronizing attach streams call it from inside their sync protocol, after
// the batch barrier, so an attempt that connects but cannot synchronize stays
// in Syncing and can never flash OK or clear the outage warning (codex C5
// finding). The processes stream has no such barrier — see consumeProcesses.
//
// observe (nilable) is an extra hook run on every transition, before the
// message is sent. It exists for the one cross-loop behavior in attach mode:
// the processes stream's reconnect re-probing the parked loops.
func streamLoop(id StreamID, send func(tea.Msg), classify func(error) stream.Classification, observe func(stream.Status), consume func(ctx context.Context, markSynced func()) error) *stream.Loop {
	return stream.NewLoop(stream.Config{
		Attempt:  consume,
		Classify: classify,
		OnStatus: func(s stream.Status) {
			if observe != nil {
				observe(s)
			}
			send(StreamStatusMsg{Stream: id, Status: s})
		},
	})
}

// reprobeOnReconnect builds a stream's status observer: every OK transition
// AFTER the first one is a RECONNECT, which is the best evidence attach mode
// has that the daemon it lost was replaced — so it fires probe, which wakes
// the OTHER loops. Any of them may be parked in StateUnavailable with no timer
// of its own: a proxy-disabled requests stream, or the processes stream parked
// on an old daemon's 404 — which is exactly why probing must be symmetric
// rather than processes-only (codex C12 finding): the parked processes loop
// can only be rescued by a sibling's reconnect.
//
// The other loops are probed unconditionally rather than only when parked:
// Loop.Probe coalesces and never blocks, and a probe delivered to a loop that
// is not parked at worst short-circuits one backoff wait — which, mid-daemon-
// restart, is the desired behavior anyway.
//
// The bool needs no synchronization: Config.OnStatus is serialized by the
// loop's own mutex and is never called concurrently.
func reprobeOnReconnect(probe func()) func(stream.Status) {
	sawOK := false
	return func(s stream.Status) {
		if s.State != stream.StateOK {
			return
		}
		if !sawOK {
			sawOK = true // the initial connect is not a reconnect
			return
		}
		probe()
	}
}

// consumeProcesses is the processes stream's single connect-and-consume
// attempt. It is the one attach stream with no sync protocol: the endpoint
// writes a full snapshot immediately after ": connected" and a full snapshot on
// every later change (internal/api/sse.go), so there is no REST fetch to
// reconcile against and no cursor to resume from — connect IS sync, and
// markSynced rides onConnect.
//
// The consequence, accepted deliberately: OK can lead the first snapshot's
// arrival by the microseconds it takes the server's initial write to land, a
// window in which the status bar says healthy while the process list is still
// the previous one (empty at startup). The alternative — deferring markSynced
// to the first event — would leave the stream stuck in Syncing forever against
// a daemon supervising nothing.
func consumeProcesses(ctx context.Context, client TUIClient, send func(tea.Msg), markSynced func()) error {
	return client.ConsumeProcesses(ctx, markSynced, func(resp api.ProcessListResponse) {
		send(ProcessesMsg(streamedProcesses(resp)))
	})
}

// streamedProcesses converts one full process-list snapshot to the domain
// slice the models hold. It is the pure form of what attach mode's polling
// fetchProcesses command used to do inline (C12 deleted the poll).
//
// ProcessState and the rest are cast directly from their status strings; an
// unknown value from a newer daemon renders with default styling rather than
// being rejected.
func streamedProcesses(resp api.ProcessListResponse) []domain.ProcessInfo {
	processes := make([]domain.ProcessInfo, len(resp.Processes))
	for i, p := range resp.Processes {
		processes[i] = domain.ProcessInfo{
			Name:         p.Name,
			State:        domain.ProcessState(p.Status),
			PID:          p.PID,
			RestartCount: p.Restarts,
			Health:       domain.HealthStatus(p.Health),
			Kind:         domain.ProcessKind(p.Kind),
			WaitingOn:    p.WaitingOn,
			BlockedOn:    p.BlockedOn,
		}
	}
	return processes
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
		Hostname:   req.Hostname, // plan 021 C10 / Codex #10 — was dropped before
		StatusCode: req.StatusCode,
		Duration:   time.Duration(req.DurationMs) * time.Millisecond,
		RemoteAddr: req.RemoteAddr,
		InFlight:   req.InFlight,
	}
}
