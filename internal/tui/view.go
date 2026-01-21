package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/prox/internal/domain"
)

// View renders the TUI
func (m Model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	switch m.mode {
	case ModeHelp:
		return m.helpView()
	default:
		return m.mainView()
	}
}

// mainView renders the main TUI layout
func (m Model) mainView() string {
	var b strings.Builder

	// Process panel at top
	b.WriteString(m.processPanel())
	b.WriteString("\n")

	// Main log viewport
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Status bar at bottom
	b.WriteString(m.statusBar())

	return b.String()
}

// processPanel renders the process status header
func (m Model) processPanel() string {
	var items []string

	for i, proc := range m.processes {
		style := processStyle(proc.State)

		// Highlight if solo'd
		name := proc.Name
		if m.soloProcess == proc.Name {
			name = fmt.Sprintf("[%s]", proc.Name)
		}

		// Show number key
		key := fmt.Sprintf("%d:", i+1)
		items = append(items, style.Render(key+name))
	}

	header := lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(items, "  "))
	return headerStyle.Render(header)
}

// statusBar renders the bottom status bar
func (m Model) statusBar() string {
	var left, right string

	// Left side: mode/filter info
	switch m.mode {
	case ModeFilter:
		left = "Filter: " + m.textInput.View()
	case ModeSearch:
		left = "Search: " + m.textInput.View()
	case ModeStringFilter:
		left = "String filter: " + m.textInput.View()
	default:
		if m.soloProcess != "" {
			left = fmt.Sprintf("Showing: %s (ESC to clear)", m.soloProcess)
		} else if m.searchPattern != "" {
			left = fmt.Sprintf("Filter: %s (ESC to clear)", m.searchPattern)
		} else {
			left = "Press ? for help"
		}
	}

	// Right side: follow mode and log count
	visible := len(m.filteredEntries())
	total := len(m.logEntries)
	followIndicator := "[FOLLOW]"
	if !m.followMode {
		followIndicator = "[PAUSED]"
	}
	right = fmt.Sprintf("%s %d/%d lines", followIndicator, visible, total)

	// Calculate widths
	leftWidth := m.width - len(right) - 4
	if leftWidth < 0 {
		leftWidth = 0
	}

	leftPart := statusStyle.Width(leftWidth).Render(left)
	rightPart := statusStyle.Render(right)

	return lipgloss.JoinHorizontal(lipgloss.Top, leftPart, "  ", rightPart)
}

// helpView renders the help overlay
func (m Model) helpView() string {
	help := `
Prox - Process Manager

Navigation:
  j/↓        Scroll down
  k/↑        Scroll up (pauses auto-follow)
  g/Home     Go to top (pauses auto-follow)
  G/End      Go to bottom (resumes auto-follow)
  PgUp/PgDn  Page up/down
  F          Toggle auto-follow mode

Filtering:
  1-9        Solo process (toggle)
  f          Filter mode (process selection)
  /          Search (regex)
  s          String filter (live)
  ESC        Clear filters

Other:
  r          Restart selected process (1-9 to select)
  ?          Toggle help
  q/Ctrl+C   Quit

Press any key to close help...
`
	return helpStyle.Render(help)
}

// updateViewport updates the viewport content
func (m *Model) updateViewport() {
	entries := m.filteredEntries()
	var lines []string

	for _, entry := range entries {
		line := m.formatLogEntry(entry)
		lines = append(lines, line)
	}

	content := strings.Join(lines, "\n")
	m.viewport.SetContent(content)
}

// filteredEntries returns log entries after applying filters
func (m Model) filteredEntries() []domain.LogEntry {
	var result []domain.LogEntry

	for _, entry := range m.logEntries {
		// Process filter
		if m.soloProcess != "" && entry.Process != m.soloProcess {
			continue
		}

		// Check filterProcesses map
		if show, ok := m.filterProcesses[entry.Process]; ok && !show {
			continue
		}

		// String filter
		if m.searchPattern != "" {
			if !containsIgnoreCase(entry.Line, m.searchPattern) {
				continue
			}
		}

		result = append(result, entry)
	}

	return result
}

// formatLogEntry formats a single log entry for display
func (m Model) formatLogEntry(entry domain.LogEntry) string {
	// Get process color
	procStyle := getProcessStyle(entry.Process, m.processes)

	// Format timestamp
	ts := entry.Timestamp.Format("15:04:05")

	// Format process name with padding
	procName := fmt.Sprintf("%-10s", entry.Process)

	// Build line
	prefix := procStyle.Render(procName)
	timestamp := dimStyle.Render(ts)

	// Stream indicator
	streamIndicator := ""
	if entry.Stream == domain.StreamStderr {
		streamIndicator = errorStyle.Render(" ERR ")
	}

	return fmt.Sprintf("%s %s%s %s", timestamp, prefix, streamIndicator, entry.Line)
}

// getProcessStyle returns the style for a process name
func getProcessStyle(name string, processes []domain.ProcessInfo) lipgloss.Style {
	// Find process index for color
	for i, p := range processes {
		if p.Name == name {
			return processColors[i%len(processColors)]
		}
	}
	return defaultProcessStyle
}

// processStyle returns style based on process state
func processStyle(state domain.ProcessState) lipgloss.Style {
	switch state {
	case domain.ProcessStateRunning:
		return runningStyle
	case domain.ProcessStateStopped:
		return stoppedStyle
	case domain.ProcessStateCrashed:
		return crashedStyle
	case domain.ProcessStateStarting:
		return startingStyle
	case domain.ProcessStateStopping:
		return stoppingStyle
	default:
		return defaultProcessStyle
	}
}
