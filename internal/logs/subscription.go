package logs

import (
	"log"
	"sync"
	"sync/atomic"

	"github.com/charliek/prox/internal/domain"
)

var subscriptionIDCounter uint64

// Subscription represents a log subscriber
type Subscription struct {
	id     string
	ch     chan domain.LogEntry
	filter *Filter
	closed atomic.Bool
}

// newSubscription creates a new subscription
func newSubscription(filter domain.LogFilter, bufferSize int) (*Subscription, error) {
	f, err := NewFilter(filter)
	if err != nil {
		return nil, err
	}

	id := atomic.AddUint64(&subscriptionIDCounter, 1)

	return &Subscription{
		id:     formatSubscriptionID(id),
		ch:     make(chan domain.LogEntry, bufferSize),
		filter: f,
	}, nil
}

func formatSubscriptionID(id uint64) string {
	return "sub-" + formatUint64(id)
}

func formatUint64(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ID returns the subscription ID
func (s *Subscription) ID() string {
	return s.id
}

// Channel returns the channel for receiving log entries
func (s *Subscription) Channel() <-chan domain.LogEntry {
	return s.ch
}

// Send attempts to send an entry to the subscriber
// Returns false if the channel is full or closed
func (s *Subscription) Send(entry domain.LogEntry) bool {
	if s.closed.Load() {
		return false
	}

	// Check filter
	if !s.filter.Matches(entry) {
		return true // filtered out, but not a failure
	}

	select {
	case s.ch <- entry:
		return true
	default:
		// Channel full, drop message - log for debugging slow clients
		log.Printf("Subscription %s: dropped message from process %s (channel full)", s.id, entry.Process)
		return false
	}
}

// Close closes the subscription
func (s *Subscription) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.ch)
	}
}

// SubscriptionManager manages multiple subscriptions
type SubscriptionManager struct {
	mu            sync.RWMutex
	subscriptions map[string]*Subscription
	bufferSize    int
	// closed latches after Close: new Subscribe calls get an already-closed
	// channel so post-shutdown streams end immediately instead of blocking on
	// a channel nothing will ever close.
	closed bool
}

// NewSubscriptionManager creates a new subscription manager
func NewSubscriptionManager(bufferSize int) *SubscriptionManager {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &SubscriptionManager{
		subscriptions: make(map[string]*Subscription),
		bufferSize:    bufferSize,
	}
}

// Subscribe creates a new subscription. After Close, the returned channel is
// already closed, so an SSE handler that races the shutdown-time Close
// observes end-of-stream immediately.
func (m *SubscriptionManager) Subscribe(filter domain.LogFilter) (string, <-chan domain.LogEntry, error) {
	sub, err := newSubscription(filter, m.bufferSize)
	if err != nil {
		return "", nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		sub.Close()
		return sub.id, sub.ch, nil
	}
	m.subscriptions[sub.id] = sub
	m.mu.Unlock()

	return sub.id, sub.ch, nil
}

// Unsubscribe removes a subscription
func (m *SubscriptionManager) Unsubscribe(id string) {
	m.mu.Lock()
	sub, ok := m.subscriptions[id]
	if ok {
		delete(m.subscriptions, id)
	}
	m.mu.Unlock()

	if ok {
		sub.Close()
	}
}

// Broadcast sends an entry to all subscribers
func (m *SubscriptionManager) Broadcast(entry domain.LogEntry) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, sub := range m.subscriptions {
		sub.Send(entry)
	}
}

// Count returns the number of active subscriptions
func (m *SubscriptionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.subscriptions)
}

// Close closes all subscriptions. It latches: subsequent Subscribe calls
// receive an already-closed channel, so a stream request racing a
// shutdown-time Close cannot re-subscribe and pin the API server open.
// Idempotent.
func (m *SubscriptionManager) Close() {
	m.mu.Lock()
	m.closed = true
	subs := make([]*Subscription, 0, len(m.subscriptions))
	for _, sub := range m.subscriptions {
		subs = append(subs, sub)
	}
	m.subscriptions = make(map[string]*Subscription)
	m.mu.Unlock()

	for _, sub := range subs {
		sub.Close()
	}
}
