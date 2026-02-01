package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/domain"
)

// restartTimeout is the maximum time to wait for a restart operation
const restartTimeout = 30 * time.Second

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

	case ProcessesMsg:
		m.processes = m.supervisor.Processes()

	case TickMsg:
		m.processes = m.supervisor.Processes()
		cmds = append(cmds, tickCmd())

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
	}

	// Handle common navigation keys
	if m.BaseModel.handleNavigationKey(msg) {
		return m, nil
	}

	return m, nil
}

// nearBottomThreshold is the scroll percentage (0.0-1.0) at which we consider
// the viewport to be "near" the bottom for auto-follow purposes.
const nearBottomThreshold = 0.98
