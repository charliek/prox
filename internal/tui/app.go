package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// Run starts the TUI application
func Run(sup *supervisor.Supervisor, logMgr *logs.Manager) error {
	model := NewModel(sup, logMgr)
	p := tea.NewProgram(model, tea.WithAltScreen())

	// Start a goroutine to forward log entries to the TUI
	go forwardLogs(p, logMgr)

	_, err := p.Run()
	return err
}

// forwardLogs subscribes to log entries and sends them to the TUI program
func forwardLogs(p *tea.Program, logMgr *logs.Manager) {
	// Subscribe with no filter to get all entries
	_, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		return
	}

	for entry := range ch {
		p.Send(LogEntryMsg(entry))
	}
}
