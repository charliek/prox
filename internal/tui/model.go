package tui

import (
	"time"

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
	BaseModel

	// Dependencies
	supervisor *supervisor.Supervisor
	logManager *logs.Manager

	// Subscription ID for log tracking
	subID string
}

// NewModel creates a new TUI model
func NewModel(sup *supervisor.Supervisor, logMgr *logs.Manager) Model {
	base := newBaseModel()

	// Initialize filter to show all processes
	for _, p := range sup.Processes() {
		base.filterProcesses[p.Name] = true
	}
	base.processes = sup.Processes()

	return Model{
		BaseModel:  base,
		supervisor: sup,
		logManager: logMgr,
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

// RestartResultMsg is sent when a restart operation completes
type RestartResultMsg struct {
	Process string
	Err     error
}

// RestartResultClearMsg is sent to clear the restart result after a delay
type RestartResultClearMsg struct{}

// restartResultClearDelay is how long to show restart result before clearing
const restartResultClearDelay = 3 * time.Second

// restartResultClearCmd returns a command that clears the restart result after a delay
func restartResultClearCmd() tea.Cmd {
	return tea.Tick(restartResultClearDelay, func(t time.Time) tea.Msg {
		return RestartResultClearMsg{}
	})
}

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
