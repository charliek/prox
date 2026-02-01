package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/prox/internal/domain"
)

// maxLogEntries is the maximum number of log entries to keep in memory
const maxLogEntries = 1000

// maxErrorDisplayLen is the maximum length of error messages in the status bar
const maxErrorDisplayLen = 60

// BaseModel contains shared fields for both Model and ClientModel
type BaseModel struct {
	// State
	processes  []domain.ProcessInfo
	logEntries []domain.LogEntry

	// UI components
	viewport  viewport.Model
	textInput textinput.Model

	// Mode
	mode Mode

	// Filtering
	filterProcesses map[string]bool // Which processes to show
	soloProcess     string          // Single process to show (1-9 keys)
	searchPattern   string          // Current search/filter pattern
	searchMatches   []int           // Line indices matching search

	// Auto-scroll
	followMode bool // Auto-scroll to bottom on new logs

	// Last restart result for feedback
	lastRestartProcess string
	lastRestartError   error

	// Dimensions
	width  int
	height int
	ready  bool
}

// newBaseModel creates a new BaseModel with default values
func newBaseModel() BaseModel {
	ti := textinput.New()
	ti.Placeholder = "Type to filter..."
	ti.CharLimit = 100
	ti.Width = 40

	return BaseModel{
		processes:       make([]domain.ProcessInfo, 0),
		logEntries:      make([]domain.LogEntry, 0),
		textInput:       ti,
		mode:            ModeNormal,
		filterProcesses: make(map[string]bool),
		followMode:      true,
	}
}

// handleWindowSize handles window resize messages
func (b *BaseModel) handleWindowSize(msg tea.WindowSizeMsg) {
	b.width = msg.Width
	b.height = msg.Height

	headerHeight := 4 // Process panel
	footerHeight := 2 // Status bar
	verticalMargins := headerHeight + footerHeight

	viewportHeight := msg.Height - verticalMargins
	if viewportHeight < 1 {
		viewportHeight = 1
	}

	if !b.ready {
		b.viewport = viewport.New(msg.Width, viewportHeight)
		b.viewport.YPosition = headerHeight
		b.ready = true
	} else {
		b.viewport.Width = msg.Width
		b.viewport.Height = viewportHeight
	}
}

// handleLogEntry handles a new log entry message
func (b *BaseModel) handleLogEntry(entry domain.LogEntry) {
	// Check if we're at/near bottom BEFORE adding new content
	wasNearBottom := b.isNearBottom()

	b.logEntries = append(b.logEntries, entry)
	// Keep only last entries - create new slice to release memory from old entries
	if len(b.logEntries) > maxLogEntries {
		newEntries := make([]domain.LogEntry, maxLogEntries)
		copy(newEntries, b.logEntries[len(b.logEntries)-maxLogEntries:])
		b.logEntries = newEntries
	}
	b.updateViewport()

	// If user was at bottom, re-enable follow mode and stay at bottom
	if wasNearBottom {
		b.followMode = true
		b.viewport.GotoBottom()
	} else if b.followMode {
		b.viewport.GotoBottom()
	}
}

// handleFilterKey handles keys in filter mode
func (b *BaseModel) handleFilterKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		return true, nil

	case "enter":
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateViewport()
		return true, nil

	case "a":
		// Select all
		for name := range b.filterProcesses {
			b.filterProcesses[name] = true
		}
		return true, nil

	case "n":
		// Select none
		for name := range b.filterProcesses {
			b.filterProcesses[name] = false
		}
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	return true, cmd
}

// handleSearchKey handles keys in search mode
func (b *BaseModel) handleSearchKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		return true, nil

	case "enter":
		b.searchPattern = b.textInput.Value()
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateSearchMatches()
		b.updateViewport()
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	return true, cmd
}

// handleStringFilterKey handles keys in string filter mode
func (b *BaseModel) handleStringFilterKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		b.searchPattern = ""
		b.updateViewport()
		return true, nil

	case "enter":
		b.searchPattern = b.textInput.Value()
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateViewport()
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	// Live update filter
	b.searchPattern = b.textInput.Value()
	b.updateViewport()
	return true, cmd
}

// handleHelpKey handles keys in help mode
func (b *BaseModel) handleHelpKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "?", "q", "enter":
		b.mode = ModeNormal
		return true
	}
	return true
}

// handleNavigationKey handles common navigation keys
// Returns true if the key was handled
func (b *BaseModel) handleNavigationKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "?":
		b.mode = ModeHelp
		return true

	case "f":
		b.mode = ModeFilter
		b.textInput.Focus()
		return true

	case "/":
		b.mode = ModeSearch
		b.textInput.SetValue("")
		b.textInput.Focus()
		return true

	case "s":
		b.mode = ModeStringFilter
		b.textInput.SetValue("")
		b.textInput.Focus()
		return true

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Solo process
		idx := int(msg.String()[0] - '1')
		if idx < len(b.processes) {
			name := b.processes[idx].Name
			if b.soloProcess == name {
				// Toggle off
				b.soloProcess = ""
			} else {
				b.soloProcess = name
			}
			b.updateViewport()
		}
		return true

	case "esc":
		// Clear filters
		b.soloProcess = ""
		b.searchPattern = ""
		b.searchMatches = nil
		b.updateViewport()
		return true

	case "up", "k":
		b.viewport.LineUp(1)
		b.followMode = false
		return true

	case "down", "j":
		b.viewport.LineDown(1)
		return true

	case "pgup":
		b.viewport.HalfViewUp()
		b.followMode = false
		return true

	case "pgdown":
		b.viewport.HalfViewDown()
		return true

	case "home", "g":
		b.viewport.GotoTop()
		b.followMode = false
		return true

	case "end", "G":
		b.viewport.GotoBottom()
		b.followMode = true
		return true

	case "F":
		b.followMode = !b.followMode
		if b.followMode {
			b.viewport.GotoBottom()
		}
		return true
	}

	return false
}

// updateSearchMatches updates the search match indices
func (b *BaseModel) updateSearchMatches() {
	b.searchMatches = nil
	if b.searchPattern == "" {
		return
	}

	// Find matching lines
	for i, entry := range b.logEntries {
		if containsIgnoreCase(entry.Line, b.searchPattern) {
			b.searchMatches = append(b.searchMatches, i)
		}
	}
}

// isNearBottom checks if the viewport is at or near the bottom
func (b *BaseModel) isNearBottom() bool {
	if b.viewport.AtBottom() {
		return true
	}
	return b.viewport.ScrollPercent() >= nearBottomThreshold
}

// updateViewport updates the viewport content
func (b *BaseModel) updateViewport() {
	entries := b.filteredEntries()
	var lines []string

	for _, entry := range entries {
		line := b.formatLogEntry(entry)
		lines = append(lines, line)
	}

	content := strings.Join(lines, "\n")
	b.viewport.SetContent(content)
}

// filteredEntries returns log entries after applying filters
func (b *BaseModel) filteredEntries() []domain.LogEntry {
	var result []domain.LogEntry

	for _, entry := range b.logEntries {
		// Process filter
		if b.soloProcess != "" && entry.Process != b.soloProcess {
			continue
		}

		// Check filterProcesses map
		if show, ok := b.filterProcesses[entry.Process]; ok && !show {
			continue
		}

		// String filter
		if b.searchPattern != "" {
			if !containsIgnoreCase(entry.Line, b.searchPattern) {
				continue
			}
		}

		result = append(result, entry)
	}

	return result
}

// formatLogEntry formats a single log entry for display
func (b *BaseModel) formatLogEntry(entry domain.LogEntry) string {
	// Get process color
	procStyle := getProcessStyle(entry.Process, b.processes)

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

// processPanel renders the process status header
func (b *BaseModel) processPanel() string {
	var items []string

	for i, proc := range b.processes {
		style := processStyle(proc.State)

		// Highlight if solo'd
		name := proc.Name
		if b.soloProcess == proc.Name {
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
func (b *BaseModel) statusBar(extraInfo string) string {
	var left, right string

	// Left side: mode/filter info
	switch b.mode {
	case ModeFilter:
		left = "Filter: " + b.textInput.View()
	case ModeSearch:
		left = "Search: " + b.textInput.View()
	case ModeStringFilter:
		left = "String filter: " + b.textInput.View()
	default:
		if b.soloProcess != "" {
			left = fmt.Sprintf("Showing: %s (ESC to clear)", b.soloProcess)
		} else if b.searchPattern != "" {
			left = fmt.Sprintf("Filter: %s (ESC to clear)", b.searchPattern)
		} else {
			left = "Press ? for help"
			if extraInfo != "" {
				left += " | " + extraInfo
			}
		}
	}

	// Right side: follow mode and log count
	visible := len(b.filteredEntries())
	total := len(b.logEntries)
	followIndicator := "[FOLLOW]"
	if !b.followMode {
		followIndicator = "[PAUSED]"
	}
	right = fmt.Sprintf("%s %d/%d lines", followIndicator, visible, total)

	// Calculate widths
	leftWidth := b.width - len(right) - 4
	if leftWidth < 0 {
		leftWidth = 0
	}

	leftPart := statusStyle.Width(leftWidth).Render(left)
	rightPart := statusStyle.Render(right)

	return lipgloss.JoinHorizontal(lipgloss.Top, leftPart, "  ", rightPart)
}

// mainView renders the main TUI layout
func (b *BaseModel) mainView(extraStatusInfo string) string {
	var sb strings.Builder

	// Process panel at top
	sb.WriteString(b.processPanel())
	sb.WriteString("\n")

	// Main log viewport
	sb.WriteString(b.viewport.View())
	sb.WriteString("\n")

	// Status bar at bottom
	sb.WriteString(b.statusBar(extraStatusInfo))

	return sb.String()
}

// containsIgnoreCase performs a case-insensitive substring search
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// truncateError truncates an error message to maxLen characters
func truncateError(err error, maxLen int) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxLen {
		return msg[:maxLen-3] + "..."
	}
	return msg
}
