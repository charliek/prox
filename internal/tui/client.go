package tui

import (
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
)

// LogEntryMsg is sent when a new log entry arrives
type LogEntryMsg domain.LogEntry

// ProxyRequestMsg is sent when a new proxy request is recorded
type ProxyRequestMsg proxy.RequestRecord

// ProcessesMsg is sent when processes should be refreshed
type ProcessesMsg []domain.ProcessInfo

// RestartResultMsg is sent when a restart operation completes
type RestartResultMsg struct {
	Process string
	Err     error
}

// RestartResultClearMsg is sent to clear the restart result after a delay
type RestartResultClearMsg struct{}

// StatusFlashClearMsg clears a short-lived status-bar flash (theme cycle,
// copy, etc.). Seq is the flash generation captured when the timer was
// scheduled: two delays (2s copy, 3s general) share the statusFlash field,
// and without a generation a stale timer clears a NEWER flash early
// (CodeRabbit PR #102). Clears only when Seq matches the current generation.
type StatusFlashClearMsg struct {
	Seq int
}

// StartupWarningsMsg delivers non-fatal settings/theme load warnings to the log pane.
type StartupWarningsMsg struct {
	Warnings []string
}

// RequestDetailMsg is sent when request details are loaded. Seq is the
// fetchRequestDetail call's sequence number (ClientModel.detailFetchSeq at
// dispatch time) — the handler drops any msg whose Seq doesn't match the
// current value, so a stale or superseded fetch can never clobber a newer
// one (D16).
type RequestDetailMsg struct {
	ID      string
	Seq     int
	Details *RequestDetailData
	// Raw is the wire GET /api/v1/proxy/requests/{id}?include=body payload.
	// Retained for Y copy — RequestDetailData drops hostname and other fields
	// (plan 021 WS10 / Codex #10).
	Raw *api.ProxyRequestDetailResponse
}

// RequestDetailErrorMsg is sent when loading request details fails. Seq is
// the fetchRequestDetail call's sequence number (see RequestDetailMsg).
type RequestDetailErrorMsg struct {
	ID  string
	Seq int
	Err error
}

// RequestDetailData holds the detailed information about a request for TUI display
type RequestDetailData struct {
	ID         string
	Timestamp  string
	Method     string
	URL        string
	Subdomain  string
	StatusCode int
	DurationMs int64
	RemoteAddr string
	// InFlight marks a request whose response is still streaming: Duration
	// renders as "(in flight)" and a nil Details gets an explanatory note
	// instead of being treated as "capture not enabled".
	InFlight bool
	// Stale marks an in-flight request that has been in-flight longer than
	// constants.InFlightStaleAfter (D8, #53): the completion event may have
	// been lost and the true outcome is unknown. Always false when InFlight
	// is false.
	Stale           bool
	RequestHeaders  map[string][]string
	ResponseHeaders map[string][]string
	RequestBody     *BodyData
	ResponseBody    *BodyData
}

// BodyData holds captured body information
type BodyData struct {
	Size            int64
	Truncated       bool
	ContentType     string
	ContentEncoding string
	IsBinary        bool
	Data            string
	// DataBase64 marks Data as base64-encoded (the wire format: the API
	// base64-encodes binary bodies). A producer that already holds raw bytes
	// leaves this false, so the hex preview never has to guess — raw bytes
	// that merely LOOK like base64 must not be decoded.
	DataBase64 bool
	// Unavailable is true when the body existed but could no longer be loaded
	// (e.g. its disk file was evicted); UnavailableReason explains why.
	Unavailable       bool
	UnavailableReason string
}

// restartResultClearDelay is how long to show restart result before clearing
const restartResultClearDelay = 3 * time.Second

// statusFlashClearDelay matches restart feedback timing for consistency.
const statusFlashClearDelay = 3 * time.Second

// restartResultClearCmd returns a command that clears the restart result after a delay
func restartResultClearCmd() tea.Cmd {
	return tea.Tick(restartResultClearDelay, func(t time.Time) tea.Msg {
		return RestartResultClearMsg{}
	})
}

// ClientOptions carries everything that differs between the two client-mode
// callers — `prox attach`, talking to another process's daemon over HTTP, and
// `prox up --tui`, talking to its own in-process API. The model, the streams and
// every key binding are shared; only the wording and the shutdown wiring below
// are per-caller, so this stays deliberately small rather than growing into a
// general settings bag.
type ClientOptions struct {
	// Help supplies the help overlay's title suffix and quit wording. The two
	// callers must word quit differently: leaving attach abandons a daemon that
	// keeps running, leaving `up --tui` takes the processes down with it.
	Help HelpConfig

	// ConnectedStatus is the status-line text rendered whenever nothing is
	// wrong. Every degraded wording — connection outage, old daemon, restart
	// result — outranks it; see View.
	ConnectedStatus string

	// ProjectName is shown in the menu bar (WS3). Empty → RunClient resolves
	// the cwd base. CLI wiring of an explicit name lands with a later commit;
	// C3 only adds the field + cwd fallback.
	ProjectName string

	// ProxyHTTPSPort/ProxyHTTPPort are the local proxy listen ports (up --tui
	// only). Attach passes 0 — curl copy omits :port for shared-daemon-on-443
	// (plan 021 WS10 / panel B2). HTTPPort is reserved; curl is HTTPS-first.
	ProxyHTTPSPort int
	ProxyHTTPPort  int

	// ShutdownCh, when non-nil, quits the program on close. This is how an
	// out-of-band shutdown request (POST /shutdown, via the coordinator's
	// trigger channel) reaches a --tui daemon, which otherwise blocks in
	// tea.Program.Run forever. Attach mode passes nil: it supervises nothing,
	// so nothing can ask it to shut down out of band.
	//
	// The wait is PROGRAM-OWNED — a tea.Cmd returned from Init (see
	// waitForExternalShutdown), not a goroutine holding the *tea.Program and
	// calling Quit on it. That keeps quitting on one path (a message through
	// Update) so it cannot race the program's own teardown, at the cost of one
	// command goroutine parked on the channel for the rest of a hand-quit
	// session — harmless, since both callers exit the process once RunClient
	// returns.
	ShutdownCh <-chan struct{}
}

// ExternalShutdownMsg reports that ClientOptions.ShutdownCh closed, i.e. that
// something outside the TUI asked the program to stop. Update answers it with
// tea.Quit, so an out-of-band shutdown and a user pressing q leave through
// exactly the same path.
type ExternalShutdownMsg struct{}

// ClientModel is the bubbletea model for TUI client mode (connected via API)
type ClientModel struct {
	BaseModel

	// Dependencies
	client TUIClient

	// opts is the run's per-caller configuration. Help is copied into
	// BaseModel by newBaseModel; the rest is read from here.
	opts ClientOptions

	// connectionError is the attach session's global connection state, DERIVED
	// (C12) from the processes stream's health rather than reported by any
	// failing call: nothing polls any more, so there is no request left to fail
	// and produce it. noteProcessesStreamHealth owns every write; nil means
	// connected.
	connectionError error

	// detailFetchSeq counts every fetchRequestDetail call (Enter and D16's
	// background live-refresh alike); each call's closure captures its own
	// seq. RequestDetailMsg/RequestDetailErrorMsg carry the seq they were
	// produced for, and the handlers only apply a result whose seq equals
	// the current value — a stale or superseded (overlapping) fetch can
	// never clobber a newer one. Attach-mode-only concept (local mode never
	// fetches), so it lives here rather than on BaseModel, matching
	// connectionError above.
	detailFetchSeq int

	// startupWarnings are surfaced as system log lines on first Update (WS2).
	startupWarnings []string

	// requestDetailRaw is the last applied wire detail response for Y copy
	// (plan 021 WS10 / Codex #10). Cleared when leaving detail view.
	requestDetailRaw *api.ProxyRequestDetailResponse
}

// NewClientModel creates a new TUI model for client mode
func NewClientModel(client TUIClient, opts ClientOptions) ClientModel {
	m := ClientModel{
		BaseModel: newBaseModel(opts.Help),
		client:    client,
		opts:      opts,
	}
	m.projectName = resolveProjectName(opts.ProjectName)
	return m
}

// Init starts the external-shutdown wait, and nothing else. Client mode has no
// periodic work: every feed — logs, requests and, since 017 C12, processes — is
// pushed by a stream loop that RunClient starts alongside the program, and the
// processes stream's snapshot-on-connect delivers the initial process list
// without anyone asking for it.
//
// With no ShutdownCh (attach) there is nothing to wait for and Init returns nil,
// which is also what the no-poll proof in client_model_test.go pins.
func (m ClientModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.opts.ShutdownCh != nil {
		cmds = append(cmds, waitForExternalShutdown(m.opts.ShutdownCh))
	}
	if len(m.startupWarnings) > 0 {
		warnings := m.startupWarnings
		cmds = append(cmds, func() tea.Msg {
			return StartupWarningsMsg{Warnings: warnings}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// waitForExternalShutdown blocks in a command goroutine until ch closes (or
// receives), then reports ExternalShutdownMsg. bubbletea runs each command on
// its own goroutine, so blocking here costs nothing but that goroutine; see
// ClientOptions.ShutdownCh for why the wait lives in a command rather than
// beside the program.
func waitForExternalShutdown(ch <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		return ExternalShutdownMsg{}
	}
}

// errProcessesStreamLost is the connection error shown when the processes
// stream dropped without an error of its own (the daemon closed the stream
// cleanly, e.g. a shutdown mid-attach).
var errProcessesStreamLost = errors.New("connection to the prox daemon was lost")

// errProcessesStreamUnsupported is the version-skew case: the loop parks on a
// 404 (classifyProcessesStreamError), which means the daemon predates the
// processes stream and no reconnect can help until it is replaced.
var errProcessesStreamUnsupported = errors.New("the prox daemon is too old to push process state; restart it on this prox version")

// noteProcessesStreamHealth derives connectionError from the processes stream's
// health. That stream is attach mode's liveness signal — it is the one feed
// whose absence means the whole view is stale, and the only one that exists on
// every daemon regardless of configuration (the requests feed is off whenever
// the proxy is) — so its state, and only its state, drives the status line's
// "Connection error (retrying...)".
//
// Reconnecting and Closed are outages; Unavailable is the old-daemon park.
// Only OK clears the error: the loop emits Syncing before every retry DIALS,
// so clearing on Syncing/Connecting would flip the banner back to "Connected"
// for the whole life of a dial that may hang for its full 30s header timeout
// against a blackholed daemon (codex C12 finding). During startup neither
// state sets nor clears anything — connectionError is nil until the first
// degradation.
func (m *ClientModel) noteProcessesStreamHealth(msg StreamStatusMsg) {
	if msg.Stream != StreamProcesses {
		return
	}
	switch msg.Status.State {
	case stream.StateReconnecting, stream.StateClosed:
		if msg.Status.Err != nil {
			m.connectionError = msg.Status.Err
		} else {
			m.connectionError = errProcessesStreamLost
		}
	case stream.StateUnavailable:
		m.connectionError = errProcessesStreamUnsupported
	case stream.StateOK:
		m.connectionError = nil
	}
}

// Update handles messages
func (m ClientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		if handled, cmd := m.handleMenuMouse(msg); handled {
			return m, cmd
		}
		// Non-menu mouse in text/help modes is ignored (menu blur handled above).
		if m.mode == ModeSearch || m.mode == ModeStringFilter || m.mode == ModeHelp {
			return m, nil
		}
		if handled, navCmd := m.handleContentMouse(msg); handled {
			cmd := m.maybeFetchOlderRequests()
			return m, tea.Batch(navCmd, cmd)
		}

	case ExternalShutdownMsg:
		// Same exit as q/Ctrl-C: RunClient returns and the caller runs its
		// normal shutdown sequence, so both routes are identical.
		return m, tea.Quit

	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)
		m.updateViewport()

	case LogEntryMsg:
		m.handleLogEntry(domain.LogEntry(msg))

	case LogsSyncMsg:
		// One batch, one render (C9).
		m.handleLogsSync(msg)

	case ProxyRequestMsg:
		record := proxy.RequestRecord(msg)
		m.handleProxyRequest(record)
		// Live-refresh an open detail view once its request completes (D16).
		// Streamed attach records never carry Details (§2), so — unlike
		// local mode — a re-fetch is required. detailLoading is deliberately
		// left alone: the existing (in-flight) snapshot stays on screen with
		// no loading flicker while the fetch runs in the background.
		if m.viewMode == ViewModeRequestDetail && m.selectedRequestID == record.ID && !record.InFlight {
			m.detailFetchSeq++
			cmds = append(cmds, m.fetchRequestDetail(record.ID, m.detailFetchSeq))
		}

	case RequestsSyncMsg:
		// One batch, one render (C6).
		m.handleRequestsSync(msg)
		// D16 also applies to a completion that arrives via the sync batch
		// instead of a live event: an open detail still showing an in-flight
		// snapshot must refetch once its row went final. Guarded on the
		// detail's own in-flight state (unlike the live path, which fires at
		// most once per ID by upsert monotonicity) so routine reconnect syncs
		// don't refetch an already-final detail on every re-sync.
		if m.viewMode == ViewModeRequestDetail && m.selectedRequestID != "" &&
			(m.requestDetail == nil || m.requestDetail.InFlight) {
			for _, r := range m.proxyRequests {
				if r.ID == m.selectedRequestID && !r.InFlight {
					m.detailFetchSeq++
					cmds = append(cmds, m.fetchRequestDetail(r.ID, m.detailFetchSeq))
					break
				}
			}
		}

	case RequestsPageMsg:
		// One scroll-back page, one render (D11). Supersession, error
		// classification and the phase transition all live in the helper.
		m.prependOlderRequests(msg)

	case StreamStatusMsg:
		m.handleStreamStatus(msg)
		m.noteProcessesStreamHealth(msg)

	case ProcessesMsg:
		// One full snapshot off the processes stream (C12). connectionError is
		// deliberately NOT touched here: it is derived from stream health
		// alone, and the OK transition that precedes the first snapshot has
		// already cleared it.
		m.processes = []domain.ProcessInfo(msg)

	case RestartResultMsg:
		m.lastRestartProcess = msg.Process
		m.lastRestartError = msg.Err
		cmds = append(cmds, restartResultClearCmd())

	case RestartResultClearMsg:
		m.lastRestartProcess = ""
		m.lastRestartError = nil

	case StatusFlashClearMsg:
		if msg.Seq == m.statusFlashSeq {
			m.statusFlash = ""
		}

	case StartupWarningsMsg:
		for _, w := range msg.Warnings {
			m.appendLogEntry(systemLogEntry(w))
		}

	case RequestDetailMsg:
		// Every mutation — including detailLoading, previously cleared
		// before this guard for ALL results including stale ones — lives
		// inside the ID+seq guard: a stale-ID or superseded-seq result
		// (an overlapping fetch this one was not the last to start) is
		// dropped entirely, so it can never clear the loading state or
		// content owned by the current selection/fetch (D16).
		if msg.ID == m.selectedRequestID && msg.Seq == m.detailFetchSeq {
			// Belt-and-braces content guard: a payload that is itself still
			// in-flight can't supersede an already-displayed final
			// snapshot (e.g. a server-side race in GetProxyRequest) — drop
			// it rather than regress the view.
			supersededByFinal := msg.Details != nil && msg.Details.InFlight &&
				m.requestDetail != nil && !m.requestDetail.InFlight
			if !supersededByFinal {
				m.detailLoading = false
				m.requestDetail = msg.Details
				m.requestDetailRaw = msg.Raw
				m.detailError = nil
				m.detailRefreshFailed = false
				m.updateViewport()
				m.clampViewportToContent()
			}
		}

	case RequestDetailErrorMsg:
		if msg.ID == m.selectedRequestID && msg.Seq == m.detailFetchSeq {
			m.detailLoading = false
			if m.requestDetail != nil {
				// A background live-refresh failed: keep the snapshot on
				// screen and surface the failure instead of replacing a
				// useful view with the error screen (D16).
				m.detailRefreshFailed = true
			} else {
				m.detailError = msg.Err
			}
			m.updateViewport()
		}
	}

	// Handle viewport updates for unhandled messages (wheel disabled — TUI routes scroll).
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	// Handle text input if in filter/search mode
	if m.mode == ModeSearch || m.mode == ModeStringFilter {
		m.textInput, cmd = m.textInput.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// handleKey processes keyboard input.
// Key routing order (Codex #4, pinned): open-menu capture → text/help modes →
// client actions (q/r/Enter) → menu openers (m/v/f) + normal navigation.
func (m ClientModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 1. Open-menu capture: every key consumed, never re-dispatched.
	if m.menuOpen() {
		return m, m.handleMenuKey(msg)
	}

	// 2. Mode-specific keys (textinput / help).
	switch m.mode {
	case ModeSearch:
		_, cmd := m.handleSearchKey(msg)
		return m, cmd
	case ModeStringFilter:
		_, cmd := m.handleStringFilterKey(msg)
		return m, cmd
	case ModeHelp:
		m.handleHelpKey(msg)
		return m, nil
	}

	// 3. Client actions.
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "r":
		// Restart the solo'd process via API
		if m.soloProcess != "" {
			processName := m.soloProcess
			return m, func() tea.Msg {
				err := m.client.RestartProcess(processName)
				return RestartResultMsg{Process: processName, Err: err}
			}
		}
		return m, nil

	case "enter":
		// In requests view, show detail for selected request
		if m.viewMode == ViewModeRequests {
			requestID := m.getSelectedRequest()
			if requestID != "" {
				return m, m.beginRequestDetail(requestID)
			}
		}
		return m, nil
	}

	// 4. Grab-for-agent copy (WS10): y/c/Y in requests + detail; logs y when
	// a /-search cursor is parked.
	if handled, cmd := m.handleCopyKey(msg); handled {
		return m, cmd
	}

	// 5. Menu openers (m/v/f) + common navigation.
	wasDetail := m.viewMode == ViewModeRequestDetail
	handled, navCmd := m.handleNavigationKey(msg)
	if wasDetail && m.viewMode != ViewModeRequestDetail {
		m.requestDetailRaw = nil
	}
	if handled {
		// maybeFetchOlderRequests is evaluated BEFORE m is copied into the
		// return values: it mutates m (pagingPhase=loading), and Go does not
		// specify the order of a plain operand relative to a call in the same
		// return statement — `return m, tea.Batch(navCmd, m.maybeFetchOlderRequests())`
		// could legally return a model copy without the mutation, silently
		// breaking single-flight (CodeRabbit, PR #88).
		cmd := m.maybeFetchOlderRequests()
		return m, tea.Batch(navCmd, cmd)
	}

	return m, nil
}

// beginRequestDetail opens the request detail view and starts a fetch.
func (m *ClientModel) beginRequestDetail(requestID string) tea.Cmd {
	m.selectedRequestID = requestID
	m.viewMode = ViewModeRequestDetail
	m.detailLoading = true
	m.requestDetail = nil
	m.requestDetailRaw = nil
	m.detailError = nil
	m.detailRefreshFailed = false
	m.renderDetailFromTop()
	m.detailFetchSeq++
	return m.fetchRequestDetail(requestID, m.detailFetchSeq)
}

// fetchRequestDetail returns a command to fetch request details from the API,
// tagged with seq (the caller's just-incremented m.detailFetchSeq at the time
// of the call) so the Update handler can tell this result apart from any
// other overlapping fetch for the same or a different request (D16).
func (m ClientModel) fetchRequestDetail(id string, seq int) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.GetProxyRequest(id, true) // Include body
		if err != nil {
			return RequestDetailErrorMsg{ID: id, Seq: seq, Err: err}
		}

		// Convert API response to RequestDetailData
		detail := &RequestDetailData{
			ID:         resp.ID,
			Timestamp:  resp.Timestamp,
			Method:     resp.Method,
			URL:        resp.URL,
			Subdomain:  resp.Subdomain,
			StatusCode: resp.StatusCode,
			DurationMs: resp.DurationMs,
			RemoteAddr: resp.RemoteAddr,
			InFlight:   resp.InFlight,
			Stale:      resp.Stale,
		}

		if resp.Details != nil {
			detail.RequestHeaders = resp.Details.RequestHeaders
			detail.ResponseHeaders = resp.Details.ResponseHeaders

			if resp.Details.RequestBody != nil {
				detail.RequestBody = clientBodyToBodyData(resp.Details.RequestBody)
			}

			if resp.Details.ResponseBody != nil {
				detail.ResponseBody = clientBodyToBodyData(resp.Details.ResponseBody)
			}
		}

		return RequestDetailMsg{ID: id, Seq: seq, Details: detail, Raw: resp}
	}
}

// clientBodyToBodyData maps an API CapturedBodyResponse to TUI BodyData. Data
// is already decoded (text) or base64 (binary) by the server; an
// unavailable_reason marks an evicted body.
func clientBodyToBodyData(body *api.CapturedBodyResponse) *BodyData {
	return &BodyData{
		Size:            body.Size,
		Truncated:       body.Truncated,
		ContentType:     body.ContentType,
		ContentEncoding: body.ContentEncoding,
		IsBinary:        body.IsBinary,
		Data:            body.Data,
		DataBase64:      body.IsBinary, // API base64-encodes binary body data

		Unavailable:       body.UnavailableReason != "",
		UnavailableReason: body.UnavailableReason,
	}
}

// View renders the TUI
func (m ClientModel) View() string {
	if !m.ready {
		return "Connecting to prox..."
	}

	switch m.mode {
	case ModeHelp:
		return m.helpView()
	default:
		statusInfo := m.opts.ConnectedStatus
		if errors.Is(m.connectionError, errProcessesStreamUnsupported) {
			// The old-daemon park is not an outage and never self-heals by
			// waiting, so "retrying..." would be a lie — render the actionable
			// hint instead.
			statusInfo = truncateError(m.connectionError, maxErrorDisplayLen)
		} else if m.connectionError != nil && m.streamHealth[StreamProcesses].State == stream.StateClosed {
			// Terminal: the loop is gone (auth failure classified terminal, or
			// quit teardown) and no retry will ever happen — promising one
			// would be a lie too (codex C12 finding).
			statusInfo = "Connection lost: " + truncateError(m.connectionError, maxErrorDisplayLen)
		} else if m.connectionError != nil {
			// One wording for every transient degraded processes-stream state:
			// the per-stream detail lives in the status bar's health segments.
			statusInfo = "Connection error (retrying...)"
		} else if m.statusFlash != "" {
			statusInfo = m.statusFlash
		} else if m.lastRestartProcess != "" {
			if m.lastRestartError != nil {
				statusInfo = "Restart failed: " + truncateError(m.lastRestartError, maxErrorDisplayLen)
			} else {
				statusInfo = "Restarted: " + m.lastRestartProcess
			}
		}
		return m.mainView(statusInfo)
	}
}
