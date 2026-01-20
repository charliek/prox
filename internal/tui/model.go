package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// Mode represents the current TUI mode
type Mode int

const (
	ModeNormal Mode = iota
	ModeFilter
	ModeSearch
	ModeStringFilter
	ModeHelp
)

// Model is the bubbletea model for the TUI
type Model struct {
	// Dependencies
	supervisor *supervisor.Supervisor
	logManager *logs.Manager

	// State
	processes  []domain.ProcessInfo
	logEntries []domain.LogEntry
	subID      string

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

	// Dimensions
	width  int
	height int
	ready  bool
}

// NewModel creates a new TUI model
func NewModel(sup *supervisor.Supervisor, logMgr *logs.Manager) Model {
	ti := textinput.New()
	ti.Placeholder = "Type to filter..."
	ti.CharLimit = 100
	ti.Width = 40

	// Initialize filter to show all processes
	filterProcesses := make(map[string]bool)
	for _, p := range sup.Processes() {
		filterProcesses[p.Name] = true
	}

	return Model{
		supervisor:      sup,
		logManager:      logMgr,
		processes:       sup.Processes(),
		logEntries:      make([]domain.LogEntry, 0),
		textInput:       ti,
		mode:            ModeNormal,
		filterProcesses: filterProcesses,
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		subscribeToLogs(m.logManager),
		refreshProcesses(),
		tickCmd(),
	)
}

// LogEntryMsg is sent when a new log entry arrives
type LogEntryMsg domain.LogEntry

// ProcessesMsg is sent when processes should be refreshed
type ProcessesMsg []domain.ProcessInfo

// TickMsg is sent periodically
type TickMsg time.Time

// subscribeToLogs starts log subscription (returns subscription ID for tracking)
// Note: Actual log forwarding is handled by forwardLogs in app.go
func subscribeToLogs(logMgr *logs.Manager) tea.Cmd {
	return func() tea.Msg {
		id, _, err := logMgr.Subscribe(domain.LogFilter{})
		if err != nil {
			return nil
		}
		return subIDMsg(id)
	}
}

type subIDMsg string

// refreshProcesses returns a command to refresh process list
func refreshProcesses() tea.Cmd {
	return func() tea.Msg {
		return ProcessesMsg{}
	}
}

// tickCmd returns a command that ticks periodically
func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}
