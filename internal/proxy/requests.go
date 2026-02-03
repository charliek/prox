package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// RequestRecord represents a single proxied request.
type RequestRecord struct {
	// ID is a 7-character hash generated from timestamp, method, and URL.
	ID         string        `json:"id"`
	Timestamp  time.Time     `json:"timestamp"`
	Method     string        `json:"method"`
	URL        string        `json:"url"`
	Subdomain  string        `json:"subdomain"`
	StatusCode int           `json:"status_code"`
	Duration   time.Duration `json:"duration"`
	RemoteAddr string        `json:"remote_addr"`
}

// generateRequestID creates a short hash ID (7 chars, git-style) from request data.
func generateRequestID(timestamp time.Time, method, url string) string {
	data := fmt.Sprintf("%d:%s:%s", timestamp.UnixNano(), method, url)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:7]
}

// RequestFilter specifies criteria for filtering requests.
type RequestFilter struct {
	Subdomain string
	Method    string
	MinStatus int
	MaxStatus int
	Since     time.Time
	Limit     int
}

// RequestSubscription represents a subscription to request updates.
type RequestSubscription struct {
	ID     string
	Filter RequestFilter
	Ch     chan RequestRecord
}

// RequestManager tracks proxied requests in a ring buffer and supports subscriptions.
type RequestManager struct {
	mu       sync.RWMutex
	buffer   []RequestRecord
	head     int
	count    int
	capacity int

	subMu  sync.RWMutex
	subs   map[string]*RequestSubscription
	nextID int
}

// NewRequestManager creates a new request manager with the specified buffer capacity.
func NewRequestManager(capacity int) *RequestManager {
	if capacity <= 0 {
		capacity = 1
	}
	return &RequestManager{
		buffer:   make([]RequestRecord, capacity),
		capacity: capacity,
		subs:     make(map[string]*RequestSubscription),
	}
}

// Record adds a new request record to the buffer and notifies subscribers.
// If the record doesn't have an ID, one is generated.
func (m *RequestManager) Record(record RequestRecord) {
	if record.ID == "" {
		record.ID = generateRequestID(record.Timestamp, record.Method, record.URL)
	}

	m.mu.Lock()
	m.buffer[m.head] = record
	m.head = (m.head + 1) % m.capacity
	if m.count < m.capacity {
		m.count++
	}
	m.mu.Unlock()

	// Notify subscribers
	m.notifySubscribers(record)
}

// Recent returns the most recent requests matching the filter.
func (m *RequestManager) Recent(filter RequestFilter) []RequestRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 || limit > m.count {
		limit = m.count
	}

	result := make([]RequestRecord, 0, limit)

	// Iterate from newest to oldest
	for i := 0; i < m.count && len(result) < limit; i++ {
		idx := (m.head - 1 - i + m.capacity) % m.capacity
		record := m.buffer[idx]

		if m.matchesFilter(record, filter) {
			result = append(result, record)
		}
	}

	return result
}

// Subscribe creates a subscription for real-time request updates.
func (m *RequestManager) Subscribe(filter RequestFilter) *RequestSubscription {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	m.nextID++
	sub := &RequestSubscription{
		ID:     fmt.Sprintf("sub-%d", m.nextID),
		Filter: filter,
		Ch:     make(chan RequestRecord, 100),
	}
	m.subs[sub.ID] = sub

	return sub
}

// Unsubscribe removes a subscription.
func (m *RequestManager) Unsubscribe(id string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	if sub, ok := m.subs[id]; ok {
		close(sub.Ch)
		delete(m.subs, id)
	}
}

// Count returns the number of requests currently in the buffer.
func (m *RequestManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.count
}

// Close closes all subscription channels and cleans up resources.
func (m *RequestManager) Close() {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	for id, sub := range m.subs {
		close(sub.Ch)
		delete(m.subs, id)
	}
}

func (m *RequestManager) notifySubscribers(record RequestRecord) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()

	for _, sub := range m.subs {
		if m.matchesFilter(record, sub.Filter) {
			select {
			case sub.Ch <- record:
			default:
				// Channel full, drop the message
			}
		}
	}
}

func (m *RequestManager) matchesFilter(record RequestRecord, filter RequestFilter) bool {
	if filter.Subdomain != "" && record.Subdomain != filter.Subdomain {
		return false
	}
	if filter.Method != "" && record.Method != filter.Method {
		return false
	}
	if filter.MinStatus > 0 && record.StatusCode < filter.MinStatus {
		return false
	}
	if filter.MaxStatus > 0 && record.StatusCode > filter.MaxStatus {
		return false
	}
	if !filter.Since.IsZero() && record.Timestamp.Before(filter.Since) {
		return false
	}
	return true
}
