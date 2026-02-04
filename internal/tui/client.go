package tui

import (
	tea "github.com/charmbracelet/bubbletea"

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
		m.handleProxyRequest(proxy.RequestRecord(msg))

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
		m.detailLoading = false
		if msg.ID == m.selectedRequestID {
			m.requestDetail = msg.Details
			m.detailError = nil
			m.updateViewport()
		}

	case RequestDetailErrorMsg:
		m.detailLoading = false
		if msg.ID == m.selectedRequestID {
			m.detailError = msg.Err
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
		_, cmd := m.BaseModel.handleFilterKey(msg)
		return m, cmd
	case ModeSearch:
		_, cmd := m.BaseModel.handleSearchKey(msg)
		return m, cmd
	case ModeStringFilter:
		_, cmd := m.BaseModel.handleStringFilterKey(msg)
		return m, cmd
	case ModeHelp:
		m.BaseModel.handleHelpKey(msg)
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
				m.updateViewport()
				return m, m.fetchRequestDetail(requestID)
			}
		}
		return m, nil
	}

	// Handle common navigation keys
	if m.BaseModel.handleNavigationKey(msg) {
		return m, nil
	}

	return m, nil
}

// fetchRequestDetail returns a command to fetch request details from the API
func (m ClientModel) fetchRequestDetail(id string) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.GetProxyRequest(id, true) // Include body
		if err != nil {
			return RequestDetailErrorMsg{ID: id, Err: err}
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
		}

		if resp.Details != nil {
			detail.RequestHeaders = resp.Details.RequestHeaders
			detail.ResponseHeaders = resp.Details.ResponseHeaders

			if resp.Details.RequestBody != nil {
				detail.RequestBody = &BodyData{
					Size:        resp.Details.RequestBody.Size,
					Truncated:   resp.Details.RequestBody.Truncated,
					ContentType: resp.Details.RequestBody.ContentType,
					IsBinary:    resp.Details.RequestBody.IsBinary,
					Data:        resp.Details.RequestBody.Data,
				}
			}

			if resp.Details.ResponseBody != nil {
				detail.ResponseBody = &BodyData{
					Size:        resp.Details.ResponseBody.Size,
					Truncated:   resp.Details.ResponseBody.Truncated,
					ContentType: resp.Details.ResponseBody.ContentType,
					IsBinary:    resp.Details.ResponseBody.IsBinary,
					Data:        resp.Details.ResponseBody.Data,
				}
			}
		}

		return RequestDetailMsg{ID: id, Details: detail}
	}
}

// View renders the TUI
func (m ClientModel) View() string {
	if !m.ready {
		return "Connecting to prox..."
	}

	switch m.mode {
	case ModeHelp:
		return m.BaseModel.helpView()
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
		return m.BaseModel.mainView(statusInfo)
	}
}
