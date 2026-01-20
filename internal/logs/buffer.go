package logs

import (
	"sync"

	"github.com/charliek/prox/internal/domain"
)

// RingBuffer is a fixed-size circular buffer for log entries
type RingBuffer struct {
	mu       sync.RWMutex
	entries  []domain.LogEntry
	head     int // next write position
	count    int // current number of entries
	capacity int // max entries
}

// NewRingBuffer creates a new ring buffer with the given capacity
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &RingBuffer{
		entries:  make([]domain.LogEntry, capacity),
		capacity: capacity,
	}
}

// Write adds a new entry to the buffer
func (b *RingBuffer) Write(entry domain.LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.entries[b.head] = entry
	b.head = (b.head + 1) % b.capacity

	if b.count < b.capacity {
		b.count++
	}
}

// Read returns all entries in chronological order
func (b *RingBuffer) Read() []domain.LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 {
		return nil
	}

	result := make([]domain.LogEntry, b.count)

	// Calculate start position
	start := 0
	if b.count == b.capacity {
		start = b.head // oldest entry is at head when full
	}

	for i := 0; i < b.count; i++ {
		idx := (start + i) % b.capacity
		result[i] = b.entries[idx]
	}

	return result
}

// ReadLast returns the last n entries in chronological order
func (b *RingBuffer) ReadLast(n int) []domain.LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 || n <= 0 {
		return nil
	}

	if n > b.count {
		n = b.count
	}

	result := make([]domain.LogEntry, n)

	// Calculate start position for last n entries
	// Start position = (head - n + capacity) % capacity when count == capacity
	// Or (count - n) when count < capacity
	var start int
	if b.count == b.capacity {
		start = (b.head - n + b.capacity) % b.capacity
	} else {
		start = b.count - n
	}

	for i := 0; i < n; i++ {
		idx := (start + i) % b.capacity
		result[i] = b.entries[idx]
	}

	return result
}

// Count returns the current number of entries in the buffer
func (b *RingBuffer) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// Capacity returns the maximum capacity of the buffer
func (b *RingBuffer) Capacity() int {
	return b.capacity
}

// Clear removes all entries from the buffer
func (b *RingBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.head = 0
	b.count = 0
}
