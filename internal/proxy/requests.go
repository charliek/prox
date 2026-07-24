package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/constants"
)

// RequestRecord represents a single proxied request.
type RequestRecord struct {
	// ID is a 12-character hash generated from timestamp, method, and URL.
	ID         string        `json:"id"`
	Timestamp  time.Time     `json:"timestamp"`
	Method     string        `json:"method"`
	URL        string        `json:"url"`
	Subdomain  string        `json:"subdomain"`
	Hostname   string        `json:"hostname,omitempty"` // full hostname (e.g., api.local.dev)
	StatusCode int           `json:"status_code"`
	Duration   time.Duration `json:"duration"`
	RemoteAddr string        `json:"remote_addr"`

	// ProjectDir identifies the project that owns the route this request was
	// proxied for. Set daemon-side so records/purges are scoped to a project
	// even when two projects own the same hostname on different ports.
	ProjectDir string `json:"project_dir,omitempty"`

	// InFlight marks a record published at response-header time, before the
	// response body finished. Such records carry the header-time status but
	// zero Duration and nil Details; a completion update (same ID, InFlight
	// false) replaces them via Upsert. Completed records are terminal:
	// omitempty keeps their JSON identical to the pre-in-flight wire format.
	InFlight bool `json:"in_flight,omitempty"`

	// Details contains captured headers and bodies (nil when capture is disabled)
	Details *RequestDetails `json:"details,omitempty"`
}

// StaleAt reports whether this record is stale as of now (D8, #53): still
// marked in-flight, but running longer than constants.InFlightStaleAfter.
// "Stale" means completion-unknown, not broken — see the constant's doc
// comment. Final (non-in-flight) records are never stale: their completion,
// whatever it was, is already known. This is the single staleness check;
// every consumer (API response conversion, CLI rendering, TUI rendering)
// calls it rather than re-deriving the condition.
func (r RequestRecord) StaleAt(now time.Time) bool {
	return r.InFlight && now.Sub(r.Timestamp) > constants.InFlightStaleAfter
}

// RequestDetails contains captured request/response headers and bodies.
type RequestDetails struct {
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	RequestBody     *CapturedBody       `json:"request_body,omitempty"`
	ResponseBody    *CapturedBody       `json:"response_body,omitempty"`
}

// CapturedBody represents a captured request or response body.
type CapturedBody struct {
	// Size is the total bytes observed by the capture wrapper, including
	// bytes discarded past the truncation cap (not Content-Length, not
	// decoded size).
	Size            int64  `json:"size"`
	CapturedSize    int64  `json:"captured_size"`              // Bytes actually retained after truncation
	Truncated       bool   `json:"truncated"`                  // True if body was truncated due to size limit
	ContentType     string `json:"content_type"`               // Content-Type header value
	ContentEncoding string `json:"content_encoding,omitempty"` // Content-Encoding header value (raw wire bytes; not decoded here)
	IsBinary        bool   `json:"is_binary"`                  // True if body appears to be binary data
	Data            []byte `json:"data"`                       // Inline data for small bodies
	FilePath        string `json:"file_path"`                  // Disk path for large bodies (Data is nil when set)
}

// requestIDCounter disambiguates requests that share a timestamp/method/URL.
// Without it, two simultaneous identical requests would hash to the same ID
// and their capture files would overwrite (and cross-delete on eviction).
var requestIDCounter atomic.Uint64

// GenerateRequestID creates a short hash ID (12 chars, git-style) from request
// data plus a per-process counter, so IDs are unique within a process even for
// simultaneous identical requests. (Truncating the hash to 48 bits keeps the
// birthday-collision residual across the 1000-record ring negligible, unlike
// the 28 bits of a 7-char ID whose ~0.2%-per-full-ring collision odds could
// overwrite capture files.) Exported so the shared daemon can generate a
// request ID before proxying (needed for capture file naming).
func GenerateRequestID(timestamp time.Time, method, url string) string {
	data := fmt.Sprintf("%d:%d:%s:%s", timestamp.UnixNano(), requestIDCounter.Add(1), method, url)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:12]
}

// RequestFilter specifies criteria for filtering requests.
type RequestFilter struct {
	Subdomain string
	Hostnames []string // match if record.Hostname is in this list (empty = match all)

	// URLContains matches if record.URL contains this substring
	// (case-insensitive). URL is path+query only (no scheme/host, matching
	// the TUI 's' filter's reference behavior), since RequestRecord.URL is
	// populated from r.URL.String() on the server side. Empty = match all.
	URLContains string
	ProjectDir  string // match if record.ProjectDir equals this exactly (empty = match all)
	Method      string
	MinStatus   int
	MaxStatus   int
	Since       time.Time
	Limit       int
}

// RequestSubscription represents a subscription to request updates.
//
// dropped counts events this subscription lost because its channel was full
// when notifySubscribers tried to deliver (D9). It is an atomic so
// notifySubscribers can bump it under the manager's read lock; the first drop
// (0→1 transition) logs once so a slow subscriber is visible without spamming.
type RequestSubscription struct {
	ID      string
	Filter  RequestFilter
	Ch      chan RequestRecord
	dropped atomic.Int64
}

// EvictionCallback is called when a request is evicted from the ring buffer.
// It receives the request ID for cleanup purposes.
type EvictionCallback func(id string)

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
	// closed latches after Close: new Subscribe calls get an already-closed
	// channel so post-shutdown streams end immediately.
	closed bool

	// droppedTotal is the manager-wide count of notifications dropped because a
	// subscriber's channel was full (D9). Exposed via DroppedEvents(); surfaced
	// daemon-side in DaemonStatusResponse and project-side (via the forwarder's
	// local manager) in the `prox status` proxy block.
	droppedTotal atomic.Int64

	// onEvict is called when a request is evicted from the buffer
	onEvict EvictionCallback
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

// SetEvictionCallback sets the callback to be invoked when requests are evicted.
func (m *RequestManager) SetEvictionCallback(fn EvictionCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvict = fn
}

// Record adds a new request record to the buffer and notifies subscribers.
// If the record doesn't have an ID, one is generated.
func (m *RequestManager) Record(record RequestRecord) {
	if record.ID == "" {
		record.ID = GenerateRequestID(record.Timestamp, record.Method, record.URL)
	}

	var evictedID string
	var onEvict EvictionCallback

	m.mu.Lock()
	// Check if we're about to overwrite an existing record
	if m.count == m.capacity {
		evicted := m.buffer[m.head]
		if evicted.ID != "" && evicted.Details != nil {
			evictedID = evicted.ID
			onEvict = m.onEvict
		}
	}

	m.buffer[m.head] = record
	m.head = (m.head + 1) % m.capacity
	if m.count < m.capacity {
		m.count++
	}
	m.mu.Unlock()

	// Call eviction callback outside of lock
	if evictedID != "" && onEvict != nil {
		onEvict(evictedID)
	}

	// Notify subscribers
	m.notifySubscribers(record)
}

// Upsert applies a record as a monotonic two-state transition keyed by ID:
//
//	existing absent               → append (Record's eviction logic) + notify
//	existing in-flight, incoming in-flight → no-op (duplicate delivery)
//	existing in-flight, incoming final     → replace in place + notify
//	existing final                → no-op (final is terminal)
//
// The no-op rows make concurrent interleavings safe: replaying a snapshot
// while live stream events apply converges to the final record in any order,
// with no duplicate or regressed notifications. Replace-in-place keeps the
// ring slot (no head/count change, no eviction) — safe because in-flight
// records carry no Details, so the transition only ever adds capture state.
//
// Unlike Record, subscribers are notified INSIDE the ring critical section:
// same-ID notifications can never be observed out of transition order.
// notifySubscribers only performs non-blocking channel sends under subMu,
// and no path acquires mu while holding subMu, so this cannot block or
// deadlock. The eviction callback (disk IO) still runs after unlock.
func (m *RequestManager) Upsert(record RequestRecord) {
	if record.ID == "" {
		// Defensive: callers always supply IDs. Generate one and fall through
		// to the normal append path (NOT Record, whose after-unlock notify
		// would escape the ordering guarantee above).
		record.ID = GenerateRequestID(record.Timestamp, record.Method, record.URL)
	}

	var evictedID string
	var onEvict EvictionCallback

	m.mu.Lock()
	// Scan newest→oldest: in-flight records live in the newest slots.
	idx := -1
	for i := 0; i < m.count; i++ {
		j := (m.head - 1 - i + m.capacity) % m.capacity
		if m.buffer[j].ID == record.ID {
			idx = j
			break
		}
	}

	switch {
	case idx >= 0 && (!m.buffer[idx].InFlight || record.InFlight):
		// Terminal existing record, or duplicate in-flight delivery.
		m.mu.Unlock()
		return
	case idx >= 0:
		// In-flight → final: replace in place, ring position preserved.
		m.buffer[idx] = record
	default:
		if m.count == m.capacity {
			evicted := m.buffer[m.head]
			if evicted.ID != "" && evicted.Details != nil {
				evictedID = evicted.ID
				onEvict = m.onEvict
			}
		}
		m.buffer[m.head] = record
		m.head = (m.head + 1) % m.capacity
		if m.count < m.capacity {
			m.count++
		}
	}
	m.notifySubscribers(record)
	m.mu.Unlock()

	if evictedID != "" && onEvict != nil {
		onEvict(evictedID)
	}
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

// GetByID returns a request record by its ID.
// Returns the record and true if found, or an empty record and false if not found.
func (m *RequestManager) GetByID(id string) (RequestRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Search from newest to oldest for better typical case
	for i := 0; i < m.count; i++ {
		idx := (m.head - 1 - i + m.capacity) % m.capacity
		record := m.buffer[idx]
		if record.ID == id {
			return record, true
		}
	}

	return RequestRecord{}, false
}

// Subscribe creates a subscription for real-time request updates.
// After Close, the returned subscription's channel is already closed, so an
// SSE handler that races the shutdown-time Close observes end-of-stream
// immediately instead of blocking on a channel nothing will ever close.
func (m *RequestManager) Subscribe(filter RequestFilter) *RequestSubscription {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	m.nextID++
	sub := &RequestSubscription{
		ID:     fmt.Sprintf("sub-%d", m.nextID),
		Filter: filter,
		Ch:     make(chan RequestRecord, 100),
	}
	if m.closed {
		close(sub.Ch)
		return sub
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

// Close closes all subscription channels and cleans up resources. It latches:
// subsequent Subscribe calls receive an already-closed channel, so a stream
// request racing a shutdown-time Close cannot re-subscribe and pin the API
// server open. Idempotent.
func (m *RequestManager) Close() {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	m.closed = true
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
				// Channel full: drop the message and count it (D9). The
				// manager-wide total feeds DroppedEvents(); the per-subscription
				// count logs once on the first drop so a persistently slow
				// subscriber is visible without a per-drop log flood. The log
				// itself runs on a goroutine: notifySubscribers is called with
				// the ring mutex held on the Upsert path (ordering guarantee),
				// and logger I/O must not stall the SSE hot path under that
				// lock.
				m.droppedTotal.Add(1)
				if sub.dropped.Add(1) == 1 {
					go log.Printf("prox: request subscription %s is dropping events (subscriber not keeping up)", sub.ID)
				}
			}
		}
	}
}

// DroppedEvents returns the manager-wide number of subscriber notifications
// dropped because a subscription's channel was full (D9). It is monotonic for
// the life of the manager.
func (m *RequestManager) DroppedEvents() int64 {
	return m.droppedTotal.Load()
}

func (m *RequestManager) matchesFilter(record RequestRecord, filter RequestFilter) bool {
	if filter.Subdomain != "" && record.Subdomain != filter.Subdomain {
		return false
	}
	if len(filter.Hostnames) > 0 {
		matched := false
		for _, h := range filter.Hostnames {
			if record.Hostname == h {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if filter.ProjectDir != "" && record.ProjectDir != filter.ProjectDir {
		return false
	}
	if filter.URLContains != "" && !strings.Contains(strings.ToLower(record.URL), strings.ToLower(filter.URLContains)) {
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

// PurgeByProject removes all records owned by the given project from the ring
// buffer and calls the eviction callback for each purged record that carried
// captured Details (so its on-disk body files get cleaned up). It compacts the
// buffer to preserve the contiguous ring invariant. Scoping by project (not
// hostname) ensures two projects sharing a hostname on different ports don't
// purge each other's records.
func (m *RequestManager) PurgeByProject(projectDir string) {
	if projectDir == "" {
		return
	}

	var evictIDs []string
	var onEvict EvictionCallback

	m.mu.Lock()
	onEvict = m.onEvict

	// Rebuild the buffer keeping only non-matching records in order.
	kept := make([]RequestRecord, 0, m.count)
	for i := 0; i < m.count; i++ {
		idx := (m.head - m.count + i + m.capacity) % m.capacity
		rec := m.buffer[idx]
		if rec.ID == "" {
			continue
		}
		if rec.ProjectDir == projectDir {
			if rec.Details != nil {
				evictIDs = append(evictIDs, rec.ID)
			}
			continue
		}
		kept = append(kept, rec)
	}

	compacted := make([]RequestRecord, m.capacity)
	copy(compacted, kept)
	m.buffer = compacted
	m.count = len(kept)
	m.head = m.count % m.capacity
	m.mu.Unlock()

	// Call eviction callbacks outside of lock
	if onEvict != nil {
		for _, id := range evictIDs {
			onEvict(id)
		}
	}
}
