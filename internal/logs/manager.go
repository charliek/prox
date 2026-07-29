package logs

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/charliek/prox/internal/domain"
)

// streamIDBytes is the entropy behind a manager's stream ID (rendered as twice
// as many hex characters). It only has to make an accidental collision between
// two manager lifetimes implausible, not resist an attacker.
const streamIDBytes = 8

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

	// streamID identifies THIS manager lifetime. It is generated once at
	// construction and never changes, so a client that sees a stream ID
	// different from the one it last resumed against knows every sequence
	// number it holds belongs to a dead epoch and must re-sync from scratch
	// rather than ask for "everything after seq N".
	streamID string

	// ingestMu serializes Write end-to-end. Seq assignment, the ring append and
	// the subscriber broadcast all happen under it, which is what makes the
	// LogEntry.Seq invariant hold: ring order == seq order == per-subscriber
	// delivery order. Without it, concurrent per-process scanner goroutines
	// could assign 1,2 and then append/broadcast 2,1.
	//
	// The buffer and the subscription manager keep their own inner locks (they
	// are used on read paths that do NOT take ingestMu); ingestMu is strictly
	// outside them and nothing under it calls back into Manager, so there is no
	// lock-ordering cycle. Log volume does not warrant anything cleverer.
	ingestMu sync.Mutex
	seq      uint64
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
		streamID:      newStreamID(),
	}
}

// newStreamID returns a random hex token for one manager lifetime. crypto/rand
// Read never fails on the platforms we support (it panics internally instead),
// so there is no error path to surface here.
func newStreamID() string {
	var b [streamIDBytes]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// StreamID returns the token identifying this manager lifetime. It is stable
// for the life of the manager. See Manager.streamID.
func (m *Manager) StreamID() string {
	return m.streamID
}

// Write stamps the entry with the next ingest sequence number, appends it to
// the ring buffer and broadcasts it to subscribers — all under ingestMu, so
// every observer sees the same order. The caller's copy is not modified; the
// stamped copy is the one that is stored and delivered.
func (m *Manager) Write(entry domain.LogEntry) {
	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()

	m.seq++
	entry.Seq = m.seq
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

// QueryFromSeq returns the buffered entries NEWER than sinceSeq (Seq >
// sinceSeq) that match the filter, oldest-first and capped at limit (limit <= 0
// means uncapped). It is the resume primitive behind "give me everything after
// sequence N".
//
// When the cap bites it keeps the OLDEST matching entries, deliberately unlike
// Query/QueryLast which keep the newest: a resuming caller must advance its
// cursor contiguously from sinceSeq and come back for the rest, and handing it
// the newest slice would manufacture the very gap the sequence exists to rule
// out.
//
// oldestSeq/latestSeq describe the CURRENT BUFFER as a whole — every entry in
// the ring, ignoring both the filter and sinceSeq — and are 0, 0 when the buffer
// is empty. They are what lets a caller tell "caught up" apart from "the buffer
// rolled past me": an empty result with latestSeq == sinceSeq means caught up,
// while oldestSeq > sinceSeq+1 means the entries in between were evicted and the
// caller has a real gap. Bounds and entries come from ONE buffer snapshot, so
// they can never describe different moments.
func (m *Manager) QueryFromSeq(filter domain.LogFilter, sinceSeq uint64, limit int) ([]domain.LogEntry, uint64, uint64, error) {
	snapshot := m.buffer.Read()

	var oldestSeq, latestSeq uint64
	if n := len(snapshot); n > 0 {
		oldestSeq = snapshot[0].Seq
		latestSeq = snapshot[n-1].Seq
	}

	filtered, err := FilterEntries(snapshot, filter)
	if err != nil {
		return nil, 0, 0, err
	}

	result := make([]domain.LogEntry, 0, len(filtered))
	for _, entry := range filtered {
		if entry.Seq <= sinceSeq {
			continue
		}
		result = append(result, entry)
		if limit > 0 && len(result) >= limit {
			break
		}
	}

	return result, oldestSeq, latestSeq, nil
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
