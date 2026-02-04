package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
)

// Run starts the TUI application
func Run(sup *supervisor.Supervisor, logMgr *logs.Manager, reqMgr *proxy.RequestManager) error {
	model := NewModel(sup, logMgr)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	// Subscribe to logs before starting the forwarder
	subID, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		// Send error as a system log entry so user sees feedback
		// Don't cancel context here - proxy request forwarding should still work
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

	// Subscribe to proxy requests if available
	var reqSubID string
	if reqMgr != nil {
		sub := reqMgr.Subscribe(proxy.RequestFilter{})
		reqSubID = sub.ID
		go forwardProxyRequests(ctx, p, sub.Ch)
	}

	_, runErr := p.Run()

	// Cleanup: cancel context and unsubscribe
	cancel()
	if subID != "" {
		logMgr.Unsubscribe(subID)
	}
	if reqSubID != "" && reqMgr != nil {
		reqMgr.Unsubscribe(reqSubID)
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

// forwardProxyRequests forwards proxy requests from the subscription channel to the TUI program.
// It exits when the context is cancelled or the channel is closed.
func forwardProxyRequests(ctx context.Context, p *tea.Program, ch <-chan proxy.RequestRecord) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-ch:
			if !ok {
				return
			}
			p.Send(ProxyRequestMsg(req))
		}
	}
}

// TUIClient is the interface for TUI client mode API interactions.
// It consolidates all API operations needed by the TUI client.
type TUIClient interface {
	GetProcesses() (*api.ProcessListResponse, error)
	RestartProcess(name string) error
	StreamLogsChannel(params domain.LogParams) (<-chan api.LogEntryResponse, error)
	StreamProxyRequestsChannel(params domain.ProxyRequestParams) (<-chan api.ProxyRequestResponse, error)
	GetProxyRequest(id string, includeBody bool) (*api.ProxyRequestDetailResponse, error)
}

// RunClient starts the TUI application in client mode (connected via API)
func RunClient(client TUIClient) error {
	model := NewClientModel(client)
	p := tea.NewProgram(model, tea.WithAltScreen())

	ctx, cancel := context.WithCancel(context.Background())

	// Start goroutines to stream logs and proxy requests from the API
	go forwardClientLogs(ctx, p, client)
	go forwardClientProxyRequests(ctx, p, client)

	_, err := p.Run()

	// Cleanup: cancel context to stop the forwarder goroutines
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
				// Log warning so server-side timestamp bugs are visible
				p.Send(LogEntryMsg(domain.LogEntry{
					Timestamp: ts,
					Process:   "system",
					Stream:    domain.StreamStderr,
					Line:      "Warning: failed to parse log timestamp: " + parseErr.Error(),
				}))
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

// forwardClientProxyRequests streams proxy requests from the API and sends them to the TUI program.
// It exits when the context is cancelled or the channel is closed.
func forwardClientProxyRequests(ctx context.Context, p *tea.Program, client TUIClient) {
	ch, err := client.StreamProxyRequestsChannel(domain.ProxyRequestParams{})
	if err != nil {
		// Proxy may not be enabled - this is not an error, just silently return
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-ch:
			if !ok {
				// Channel closed - connection lost
				return
			}
			// Convert API response to RequestRecord
			ts, parseErr := time.Parse(time.RFC3339Nano, req.Timestamp)
			if parseErr != nil {
				ts = time.Now() // Fallback for malformed timestamps
				// Log warning so server-side timestamp bugs are visible
				p.Send(LogEntryMsg(domain.LogEntry{
					Timestamp: ts,
					Process:   "system",
					Stream:    domain.StreamStderr,
					Line:      "Warning: failed to parse proxy request timestamp: " + parseErr.Error(),
				}))
			}
			record := proxy.RequestRecord{
				ID:         req.ID,
				Timestamp:  ts,
				Method:     req.Method,
				URL:        req.URL,
				Subdomain:  req.Subdomain,
				StatusCode: req.StatusCode,
				Duration:   time.Duration(req.DurationMs) * time.Millisecond,
				RemoteAddr: req.RemoteAddr,
			}
			p.Send(ProxyRequestMsg(record))
		}
	}
}
