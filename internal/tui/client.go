package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// ClientModel is the bubbletea model for TUI client mode (connected via API)
type ClientModel struct {
	BaseModel

	// Dependencies
	client TUIClient

	// Connection state
	connectionError error // Last API connection error, nil if connected

	// detailFetchSeq counts every fetchRequestDetail call (Enter and D16's
	// background live-refresh alike); each call's closure captures its own
	// seq. RequestDetailMsg/RequestDetailErrorMsg carry the seq they were
	// produced for, and the handlers only apply a result whose seq equals
	// the current value — a stale or superseded (overlapping) fetch can
	// never clobber a newer one. Attach-mode-only concept (local mode never
	// fetches), so it lives here rather than on BaseModel, matching
	// connectionError above.
	detailFetchSeq int
}

// NewClientModel creates a new TUI model for client mode
func NewClientModel(client TUIClient) ClientModel {
	return ClientModel{
		BaseModel: newBaseModel(HelpConfig{
			TitleSuffix: "(Client Mode)",
			QuitMessage: "Quit (daemon continues running)",
		}),
		client: client,
	}
}

// Init initializes the model
func (m ClientModel) Init() tea.Cmd {
	return tea.Batch(
		m.fetchProcesses(),
		tickCmd(),
	)
}

// fetchProcesses returns a command to fetch processes from the API
func (m ClientModel) fetchProcesses() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.GetProcesses()
		if err != nil {
			return ClientErrorMsg{Err: err}
		}

		// Convert API response to domain ProcessInfo
		// Note: ProcessState is cast directly from the status string.
		// Known valid states: starting, running, stopping, stopped, failed.
		// Unknown states will result in default styling in the TUI.
		processes := make([]domain.ProcessInfo, len(resp.Processes))
		for i, p := range resp.Processes {
			processes[i] = domain.ProcessInfo{
				Name:         p.Name,
				State:        domain.ProcessState(p.Status),
				PID:          p.PID,
				RestartCount: p.Restarts,
				Health:       domain.HealthStatus(p.Health),
			}
		}
		return ProcessesMsg(processes)
	}
}

// ClientErrorMsg is sent when an API error occurs
type ClientErrorMsg struct {
	Err error
}

// Update handles messages
func (m ClientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)
		m.updateViewport()

	case LogEntryMsg:
		m.handleLogEntry(domain.LogEntry(msg))

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

	case ProcessesMsg:
		m.processes = []domain.ProcessInfo(msg)
		m.connectionError = nil // Clear error on successful fetch
		// Update filter map with any new processes
		for _, p := range m.processes {
			if _, ok := m.filterProcesses[p.Name]; !ok {
				m.filterProcesses[p.Name] = true
			}
		}

	case ClientErrorMsg:
		// Note: No automatic reconnection is attempted. If daemon stops,
		// user must quit (q) and re-run 'prox attach'. This is intentional
		// to avoid masking daemon failures.
		m.connectionError = msg.Err

	case RestartResultMsg:
		m.lastRestartProcess = msg.Process
		m.lastRestartError = msg.Err
		cmds = append(cmds, restartResultClearCmd())

	case RestartResultClearMsg:
		m.lastRestartProcess = ""
		m.lastRestartError = nil

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

	case TickMsg:
		// Refresh processes periodically
		cmds = append(cmds, m.fetchProcesses())
		cmds = append(cmds, tickCmd())
	}

	// Handle viewport updates
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	// Handle text input if in filter/search mode
	if m.mode == ModeFilter || m.mode == ModeSearch || m.mode == ModeStringFilter {
		m.textInput, cmd = m.textInput.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// handleKey processes keyboard input
func (m ClientModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle mode-specific keys first
	switch m.mode {
	case ModeFilter:
		_, cmd := m.handleFilterKey(msg)
		return m, cmd
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

	// Normal mode keys
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
				m.selectedRequestID = requestID
				m.viewMode = ViewModeRequestDetail
				m.detailLoading = true
				m.requestDetail = nil
				m.detailError = nil
				m.detailRefreshFailed = false
				m.renderDetailFromTop()
				m.detailFetchSeq++
				return m, m.fetchRequestDetail(requestID, m.detailFetchSeq)
			}
		}
		return m, nil
	}

	// Handle common navigation keys
	if m.handleNavigationKey(msg) {
		return m, nil
	}

	return m, nil
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

		return RequestDetailMsg{ID: id, Seq: seq, Details: detail}
	}
}

// clientBodyToBodyData maps an API CapturedBodyResponse to TUI BodyData. Data
// is already decoded (text) or base64 (binary) by the server; an
// unavailable_reason marks an evicted body.
func clientBodyToBodyData(body *api.CapturedBodyResponse) *BodyData {
	return &BodyData{
		Size:              body.Size,
		Truncated:         body.Truncated,
		ContentType:       body.ContentType,
		ContentEncoding:   body.ContentEncoding,
		IsBinary:          body.IsBinary,
		Data:              body.Data,
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
		statusInfo := "Connected via API"
		if m.connectionError != nil {
			statusInfo = "Connection error (retrying...)"
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
