package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/domain"
)

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		headerHeight := 4 // Process panel
		footerHeight := 2 // Status bar
		verticalMargins := headerHeight + footerHeight

		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-verticalMargins)
			m.viewport.YPosition = headerHeight
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - verticalMargins
		}

		m.updateViewport()

	case LogEntryMsg:
		m.logEntries = append(m.logEntries, domain.LogEntry(msg))
		// Keep only last 1000 entries
		if len(m.logEntries) > 1000 {
			m.logEntries = m.logEntries[len(m.logEntries)-1000:]
		}
		m.updateViewport()

	case ProcessesMsg:
		m.processes = m.supervisor.Processes()

	case TickMsg:
		m.processes = m.supervisor.Processes()
		cmds = append(cmds, tickCmd())

	case subIDMsg:
		m.subID = string(msg)
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
		return m.handleFilterKey(msg)
	case ModeSearch:
		return m.handleSearchKey(msg)
	case ModeStringFilter:
		return m.handleStringFilterKey(msg)
	case ModeHelp:
		return m.handleHelpKey(msg)
	}

	// Normal mode keys
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "?":
		m.mode = ModeHelp
		return m, nil

	case "f":
		m.mode = ModeFilter
		m.textInput.Focus()
		return m, nil

	case "/":
		m.mode = ModeSearch
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, nil

	case "s":
		m.mode = ModeStringFilter
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, nil

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Solo process
		idx := int(msg.String()[0] - '1')
		if idx < len(m.processes) {
			name := m.processes[idx].Name
			if m.soloProcess == name {
				// Toggle off
				m.soloProcess = ""
			} else {
				m.soloProcess = name
			}
			m.updateViewport()
		}
		return m, nil

	case "r":
		// Restart the solo'd process (selected via 1-9 keys)
		if m.soloProcess != "" {
			go func() {
				ctx := context.Background()
				m.supervisor.RestartProcess(ctx, m.soloProcess)
			}()
		}
		return m, nil

	case "esc":
		// Clear filters
		m.soloProcess = ""
		m.searchPattern = ""
		m.searchMatches = nil
		m.updateViewport()
		return m, nil

	case "up", "k":
		m.viewport.LineUp(1)
		return m, nil

	case "down", "j":
		m.viewport.LineDown(1)
		return m, nil

	case "pgup":
		m.viewport.HalfViewUp()
		return m, nil

	case "pgdown":
		m.viewport.HalfViewDown()
		return m, nil

	case "home", "g":
		m.viewport.GotoTop()
		return m, nil

	case "end", "G":
		m.viewport.GotoBottom()
		return m, nil
	}

	return m, nil
}

// handleFilterKey handles keys in filter mode
func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = ModeNormal
		m.textInput.Blur()
		return m, nil

	case "enter":
		m.mode = ModeNormal
		m.textInput.Blur()
		m.updateViewport()
		return m, nil

	case " ":
		// Space is passed to text input for normal typing
		// Multi-select is done via 'a' (all) and 'n' (none) keys

	case "a":
		// Select all
		for name := range m.filterProcesses {
			m.filterProcesses[name] = true
		}
		return m, nil

	case "n":
		// Select none
		for name := range m.filterProcesses {
			m.filterProcesses[name] = false
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

// handleSearchKey handles keys in search mode
func (m Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = ModeNormal
		m.textInput.Blur()
		return m, nil

	case "enter":
		m.searchPattern = m.textInput.Value()
		m.mode = ModeNormal
		m.textInput.Blur()
		m.updateSearchMatches()
		m.updateViewport()
		return m, nil
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

// handleStringFilterKey handles keys in string filter mode
func (m Model) handleStringFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = ModeNormal
		m.textInput.Blur()
		m.searchPattern = ""
		m.updateViewport()
		return m, nil

	case "enter":
		m.searchPattern = m.textInput.Value()
		m.mode = ModeNormal
		m.textInput.Blur()
		m.updateViewport()
		return m, nil
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	// Live update filter
	m.searchPattern = m.textInput.Value()
	m.updateViewport()
	return m, cmd
}

// handleHelpKey handles keys in help mode
func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "?", "q", "enter":
		m.mode = ModeNormal
		return m, nil
	}
	return m, nil
}

// updateSearchMatches updates the search match indices
func (m *Model) updateSearchMatches() {
	m.searchMatches = nil
	if m.searchPattern == "" {
		return
	}

	// Find matching lines
	for i, entry := range m.logEntries {
		if containsIgnoreCase(entry.Line, m.searchPattern) {
			m.searchMatches = append(m.searchMatches, i)
		}
	}
}

func containsIgnoreCase(s, substr string) bool {
	// Simple case-insensitive contains
	sLower := toLower(s)
	substrLower := toLower(substr)
	return contains(sLower, substrLower)
}

func toLower(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		result[i] = c
	}
	return string(result)
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
