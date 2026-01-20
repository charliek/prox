package logs

import (
	"github.com/charliek/prox/internal/domain"
)

// ManagerConfig holds configuration for the log manager
type ManagerConfig struct {
	BufferSize         int // Number of entries to keep in ring buffer
	SubscriptionBuffer int // Buffer size for subscription channels
}

// DefaultManagerConfig returns the default configuration
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		BufferSize:         1000,
		SubscriptionBuffer: 100,
	}
}

// Manager manages log storage and subscriptions
type Manager struct {
	buffer        *RingBuffer
	subscriptions *SubscriptionManager
}

// NewManager creates a new log manager
func NewManager(config ManagerConfig) *Manager {
	if config.BufferSize <= 0 {
		config.BufferSize = DefaultManagerConfig().BufferSize
	}
	if config.SubscriptionBuffer <= 0 {
		config.SubscriptionBuffer = DefaultManagerConfig().SubscriptionBuffer
	}

	return &Manager{
		buffer:        NewRingBuffer(config.BufferSize),
		subscriptions: NewSubscriptionManager(config.SubscriptionBuffer),
	}
}

// Write adds a log entry to the buffer and broadcasts to subscribers
func (m *Manager) Write(entry domain.LogEntry) {
	m.buffer.Write(entry)
	m.subscriptions.Broadcast(entry)
}

// Query retrieves log entries matching the filter
// Returns the entries and the total count before limiting
func (m *Manager) Query(filter domain.LogFilter, limit int) ([]domain.LogEntry, int, error) {
	entries := m.buffer.Read()
	return FilterEntriesLimit(entries, filter, limit)
}

// QueryLast retrieves the last n log entries matching the filter
func (m *Manager) QueryLast(filter domain.LogFilter, n int) ([]domain.LogEntry, int, error) {
	entries := m.buffer.Read()
	filtered, err := FilterEntries(entries, filter)
	if err != nil {
		return nil, 0, err
	}

	total := len(filtered)
	if n > 0 && len(filtered) > n {
		filtered = filtered[len(filtered)-n:]
	}

	return filtered, total, nil
}

// Subscribe creates a subscription for log entries matching the filter
func (m *Manager) Subscribe(filter domain.LogFilter) (string, <-chan domain.LogEntry, error) {
	return m.subscriptions.Subscribe(filter)
}

// Unsubscribe removes a subscription
func (m *Manager) Unsubscribe(id string) {
	m.subscriptions.Unsubscribe(id)
}

// Stats returns statistics about the log manager
func (m *Manager) Stats() domain.LogStats {
	return domain.LogStats{
		TotalEntries: m.buffer.Count(),
		BufferSize:   m.buffer.Capacity(),
		Subscribers:  m.subscriptions.Count(),
	}
}

// Close closes the manager and all subscriptions
func (m *Manager) Close() {
	m.subscriptions.Close()
}
