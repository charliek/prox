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

// Send attempts to send an entry to the subscriber.
// Returns false if the channel is full or closed — Broadcast turns that into
// the subscription's removal (see its doc comment). Send deliberately does NOT
// close the channel itself: it runs under the manager's read lock alongside
// concurrent senders, and closing there could race a send into a closed
// channel.
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
		// Channel full - log for debugging slow clients. One line per
		// subscription: Broadcast ends the subscription on this first
		// overflow, so there is no flood to guard against.
		log.Printf("Subscription %s: overflowed on a message from process %s and was closed (channel full)", s.id, entry.Process)
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

// Broadcast sends an entry to all subscribers and ends the ones that
// overflowed.
//
// Overflow is NOT a silent drop (C6): a subscriber whose channel is full has
// lost a line it can never be handed later, so the subscription is closed and
// removed. The SSE handler sees the closed channel as end-of-stream and
// returns; the client reconnects and re-syncs. The local TUI subscribes to the
// same manager, so a local overflow likewise ends its feed and is surfaced as
// "logs: disconnected" — strictly better than silently showing an incomplete
// log pane.
//
// The close runs after deliver has released the read lock, never inside it:
// Send runs under the read lock alongside concurrent senders, and closing there
// could race a send into a closed channel.
func (m *SubscriptionManager) Broadcast(entry domain.LogEntry) {
	for _, sub := range m.deliver(entry) {
		m.dropSubscription(sub)
	}
}

// deliver attempts the send to every subscription under the read lock and
// returns those that overflowed (or were already closed).
func (m *SubscriptionManager) deliver(entry domain.LogEntry) []*Subscription {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var overflowed []*Subscription
	for _, sub := range m.subscriptions {
		if !sub.Send(entry) {
			overflowed = append(overflowed, sub)
		}
	}
	return overflowed
}

// dropSubscription removes an overflowed subscription and closes it, in that
// order and matching Unsubscribe: removing under the write lock first means no
// later deliver can see the subscription, so the close that follows cannot race
// a send. The map entry is compared by pointer so a straggling drop cannot
// evict a different subscription, and Close's latch makes a doubled drop a
// no-op.
func (m *SubscriptionManager) dropSubscription(sub *Subscription) {
	m.mu.Lock()
	if cur, ok := m.subscriptions[sub.id]; ok && cur == sub {
		delete(m.subscriptions, sub.id)
	}
	m.mu.Unlock()

	sub.Close()
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
