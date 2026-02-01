package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// Run starts the TUI application
func Run(sup *supervisor.Supervisor, logMgr *logs.Manager) error {
	model := NewModel(sup, logMgr)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	// Subscribe to logs before starting the forwarder
	subID, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		cancel()
		// Send error as a system log entry so user sees feedback
		p.Send(LogEntryMsg(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "system",
			Stream:    domain.StreamStderr,
			Line:      "Error subscribing to logs: " + err.Error(),
		}))
	} else {
		// Start a goroutine to forward log entries to the TUI
		go forwardLogs(ctx, p, ch)
	}

	_, runErr := p.Run()

	// Cleanup: cancel context and unsubscribe
	cancel()
	if subID != "" {
		logMgr.Unsubscribe(subID)
	}

	return runErr
}

// forwardLogs forwards log entries from the subscription channel to the TUI program.
// It exits when the context is cancelled or the channel is closed.
func forwardLogs(ctx context.Context, p *tea.Program, ch <-chan domain.LogEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			p.Send(LogEntryMsg(entry))
		}
	}
}

// TUIClient is the interface for TUI client mode API interactions.
// It consolidates all API operations needed by the TUI client.
type TUIClient interface {
	GetProcesses() (*api.ProcessListResponse, error)
	RestartProcess(name string) error
	StreamLogsChannel(params domain.LogParams) (<-chan api.LogEntryResponse, error)
}

// RunClient starts the TUI application in client mode (connected via API)
func RunClient(client TUIClient) error {
	model := NewClientModel(client)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	// Start a goroutine to stream logs from the API
	go forwardClientLogs(ctx, p, client)

	_, err := p.Run()

	// Cleanup: cancel context to stop the forwarder goroutine
	cancel()

	return err
}

// forwardClientLogs streams log entries from the API and sends them to the TUI program.
// It exits when the context is cancelled or the channel is closed.
func forwardClientLogs(ctx context.Context, p *tea.Program, client TUIClient) {
	ch, err := client.StreamLogsChannel(domain.LogParams{})
	if err != nil {
		// Send error as a system log entry so user sees feedback
		p.Send(LogEntryMsg(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "system",
			Stream:    domain.StreamStderr,
			Line:      "Error connecting to log stream: " + err.Error(),
		}))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				// Channel closed - connection lost
				p.Send(LogEntryMsg(domain.LogEntry{
					Timestamp: time.Now(),
					Process:   "system",
					Stream:    domain.StreamStderr,
					Line:      "Log stream connection closed",
				}))
				return
			}
			// Convert API response to LogEntry
			ts, parseErr := time.Parse(time.RFC3339Nano, entry.Timestamp)
			if parseErr != nil {
				ts = time.Now() // Fallback for malformed timestamps
			}
			logEntry := domain.LogEntry{
				Timestamp: ts,
				Process:   entry.Process,
				Stream:    domain.Stream(entry.Stream),
				Line:      entry.Line,
			}
			p.Send(LogEntryMsg(logEntry))
		}
	}
}
