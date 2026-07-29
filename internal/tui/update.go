package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// restartTimeout is the maximum time to wait for a foreground restart from the
// TUI. It sits above the configured stop-budget cap (constants.MaxStopTimeout)
// so a legitimately long stop half of a restart is never cut off here; this
// ceiling is hang protection only, matching the CLI lifecycle client (#35, D2).
const restartTimeout = constants.LifecycleTimeoutCeiling

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		// Live-refresh an open detail view once its request completes (D15).
		// Local records carry full Details already (no fetch needed, unlike
		// attach mode); Upsert's monotonic in-flight->final state machine
		// guarantees at most one matching final message per ID, so this
		// fires once. updateViewport never resets YOffset on its own, so the
		// reader's scroll position is preserved; the clamp covers the one
		// case where the final content is SHORTER than the in-flight view
		// (capture disabled: the note lines vanish, nothing replaces them).
		if m.viewMode == ViewModeRequestDetail && m.selectedRequestID == record.ID && !record.InFlight {
			m.requestDetail = convertRequestRecordToDetail(record)
			m.updateViewport()
			m.clampViewportToContent()
		}

	case ProcessesMsg:
		m.processes = m.supervisor.Processes()

	case TickMsg:
		m.processes = m.supervisor.Processes()
		cmds = append(cmds, tickCmd(constants.TUILocalTickInterval))

	case subIDMsg:
		m.subID = string(msg)

	case RestartResultMsg:
		m.lastRestartProcess = msg.Process
		m.lastRestartError = msg.Err
		cmds = append(cmds, restartResultClearCmd())

	case RestartResultClearMsg:
		m.lastRestartProcess = ""
		m.lastRestartError = nil
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
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		// Restart the solo'd process (selected via 1-9 keys)
		if m.soloProcess != "" {
			processName := m.soloProcess
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
				defer cancel()
				err := m.supervisor.RestartProcess(ctx, processName)
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
				// Find the request in our local list
				for _, req := range m.proxyRequests {
					if req.ID == requestID {
						m.requestDetail = convertRequestRecordToDetail(req)
						break
					}
				}
				m.renderDetailFromTop()
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

// nearBottomThreshold is the scroll percentage (0.0-1.0) at which we consider
// the viewport to be "near" the bottom for auto-follow purposes.
const nearBottomThreshold = 0.98
