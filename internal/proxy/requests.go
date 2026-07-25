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

	// BeforeID anchors RecentPage (and, transitively, Recent) at the ring
	// position of the record with this ID: only records strictly OLDER than
	// the anchor, BY RING POSITION, are considered (D12, #50). This is
	// arrival order, not time order. Backfill interleavings and
	// completion-after-eviction re-appends (see Upsert: an in-flight record
	// whose original slot was evicted re-enters the ring at the newest
	// position when its completion arrives) mean a record's ring position
	// can diverge from its Timestamp — a page can legitimately contain a
	// record with a newer Timestamp than the anchor. Empty = no anchor
	// (start at the newest record, i.e. today's Recent behavior).
	BeforeID string
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

	// idIndex maps a record ID to its ring slot, giving Upsert/GetByID/anchor
	// resolution O(1) lookups instead of the newest→oldest linear scan (#71).
	// Invariant: it points at the NEWEST copy of each live ID (Record permits
	// duplicate explicit IDs) and holds no entry for a slot that isn't live.
	// Every ring mutation (both append paths, slot overwrite, replace-in-place,
	// PurgeByProject compaction) maintains it in lock-step with buffer/head/count.
	idIndex map[string]int

	subMu  sync.RWMutex
	subs   map[string]*RequestSubscription
	nextID int
	// closed latches after Close: new Subscribe calls get an already-closed
	// channel so post-shutdown streams end immediately.
	closed bool

	// writesClosed latches ring WRITES after Close (D13): a manager destroyed
	// on deregister must reject a straggler Record/Upsert racing the teardown
	// — an accepted write would land in a detached ring after the final purge
	// already ran, leaking that request's capture files. Rejected writers get
	// false back and clean up their own capture state.
	writesClosed atomic.Bool

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
		idIndex:  make(map[string]int, capacity),
	}
}

// SetEvictionCallback sets the callback to be invoked when requests are evicted.
func (m *RequestManager) SetEvictionCallback(fn EvictionCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvict = fn
}

// Record adds a new request record to the buffer and notifies subscribers.
// If the record doesn't have an ID, one is generated. It reports whether the
// record was accepted: false means the manager was already Closed (see
// writesClosed) and the caller owns any capture-file cleanup for the record.
func (m *RequestManager) Record(record RequestRecord) bool {
	if m.writesClosed.Load() {
		return false
	}
	if record.ID == "" {
		record.ID = GenerateRequestID(record.Timestamp, record.Method, record.URL)
	}

	m.mu.Lock()
	// Re-check the latch under mu: Close does not hold the ring lock, so a
	// writer that passed the pre-lock check can otherwise commit after the
	// destroy path's final purge (CodeRabbit PR #68). Under mu the store is
	// either visible here (rejected) or this write commits before the purge's
	// own mu acquisition (covered by the purge).
	if m.writesClosed.Load() {
		m.mu.Unlock()
		return false
	}
	evictedID, onEvict := m.appendRecord(record)
	m.mu.Unlock()

	// Call eviction callback outside of lock
	if evictedID != "" && onEvict != nil {
		onEvict(evictedID)
	}

	// Notify subscribers
	m.notifySubscribers(record)
	return true
}

// evictIndex drops the index entry for a slot about to be overwritten. It
// deletes ONLY when the entry still points at that slot (#71): Record permits
// duplicate explicit IDs, and overwriting an OLDER duplicate must not erase the
// NEWER copy's mapping (which points elsewhere) so GetByID keeps returning the
// newest. Deliberately NOT gated on evicted.Details — the eviction *callback*
// is Details-gated, but index maintenance must run for every overwrite. Caller
// holds m.mu.
func (m *RequestManager) evictIndex(id string, slot int) {
	if id == "" {
		return
	}
	if m.idIndex[id] == slot {
		delete(m.idIndex, id)
	}
}

// appendRecord writes record into the newest ring slot and advances head,
// evicting the oldest record when the ring is full. It maintains idIndex on
// both the overwrite (evictIndex) and the insert (#71), so the map stays in
// lock-step on both append paths. When the evicted record carried Details it
// returns that record's ID and the eviction callback so the caller can invoke
// onEvict after releasing m.mu (disk IO must not run under the lock); both are
// zero otherwise. This is the shared append arm behind Record and Upsert's
// insert case. Caller holds m.mu.
func (m *RequestManager) appendRecord(record RequestRecord) (evictedID string, onEvict EvictionCallback) {
	if m.count == m.capacity {
		evicted := m.buffer[m.head]
		if evicted.ID != "" && evicted.Details != nil {
			evictedID = evicted.ID
			onEvict = m.onEvict
		}
		m.evictIndex(evicted.ID, m.head)
	}
	m.buffer[m.head] = record
	m.idIndex[record.ID] = m.head // newest copy of this ID lives here (#71)
	m.head = (m.head + 1) % m.capacity
	if m.count < m.capacity {
		m.count++
	}
	return evictedID, onEvict
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
// Upsert reports whether the record was accepted; false means the manager was
// already Closed (writesClosed) and the caller owns capture-file cleanup.
func (m *RequestManager) Upsert(record RequestRecord) bool {
	if m.writesClosed.Load() {
		return false
	}
	if record.ID == "" {
		// Defensive: callers always supply IDs. Generate one and fall through
		// to the normal append path (NOT Record, whose after-unlock notify
		// would escape the ordering guarantee above).
		record.ID = GenerateRequestID(record.Timestamp, record.Method, record.URL)
	}

	var evictedID string
	var onEvict EvictionCallback

	m.mu.Lock()
	// Re-check the latch under mu (same reasoning as Record).
	if m.writesClosed.Load() {
		m.mu.Unlock()
		return false
	}
	// O(1) lookup of the newest slot holding this ID (#71). The index points
	// at the newest copy, matching the old newest→oldest scan's result.
	idx, exists := m.idIndex[record.ID]

	switch {
	case exists && (!m.buffer[idx].InFlight || record.InFlight):
		// Terminal existing record, or duplicate in-flight delivery.
		m.mu.Unlock()
		return true
	case exists:
		// In-flight → final: replace in place, ring position (and its index
		// entry, same ID/slot) preserved.
		m.buffer[idx] = record
	default:
		evictedID, onEvict = m.appendRecord(record)
	}
	m.notifySubscribers(record)
	m.mu.Unlock()

	if evictedID != "" && onEvict != nil {
		onEvict(evictedID)
	}
	return true
}

// Recent returns the most recent requests matching the filter. It is
// RecentPage without the cursor metadata, kept as the simple signature for
// the many callers (CLI, daemon-internal endpoints, tests) that don't page.
func (m *RequestManager) Recent(filter RequestFilter) []RequestRecord {
	records, _, _ := m.RecentPage(filter)
	return records
}

// RecentPage returns up to filter.Limit matching records, newest first,
// optionally anchored at filter.BeforeID (ring-position cursor pagination,
// D12/#50). It is the shared scan behind Recent.
//
// nextBeforeID is the ID of the OLDEST SCANNED record in this call's scan
// window — not the oldest RETURNED record. The scan continues past
// non-matching records (up to the ring's oldest record) until it collects
// filter.Limit matches, so a page whose filter excludes everything
// remaining still advances all the way through the ring and reports where
// it stopped, rather than stalling a poller that keeps re-requesting the
// same cursor. nextBeforeID is empty when the scan reached the ring's
// oldest record (no older records remain: this is the last page).
//
// anchorFound is true when filter.BeforeID is empty (no anchor requested)
// or names a record that both exists in the ring and matches the rest of
// filter. An anchor that is unknown, evicted, or excluded by scope (e.g. a
// different filter.ProjectDir than the anchor's) reports anchorFound=false
// with no records — callers must treat both cases identically (410 Gone)
// so an out-of-scope anchor can't be distinguished from a nonexistent one
// and leak the other scope's record existence.
func (m *RequestManager) RecentPage(filter RequestFilter) (records []RequestRecord, nextBeforeID string, anchorFound bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 || limit > m.count {
		limit = m.count
	}

	// startOffset is how many newest-to-oldest steps to skip before scanning
	// begins: 0 with no anchor (start at the newest record), or the
	// anchor's position + 1 (strictly older) when filter.BeforeID is set.
	startOffset := 0
	if filter.BeforeID != "" {
		anchorIdx, ok := m.idIndex[filter.BeforeID]
		if !ok {
			return nil, "", false
		}
		if !m.matchesFilter(m.buffer[anchorIdx], filter) {
			// Anchor exists but is out of the requested scope (e.g. another
			// project). Same signal as "not found" — see doc comment.
			return nil, "", false
		}
		// Convert the anchor's slot back to its newest→oldest offset (#71); the
		// index points at a live slot, so anchorPos is in [0, count-1).
		anchorPos := (m.head - 1 - anchorIdx + m.capacity) % m.capacity
		startOffset = anchorPos + 1
	}

	result := make([]RequestRecord, 0, limit)
	lastScanned := startOffset - 1

	for i := startOffset; i < m.count; i++ {
		idx := (m.head - 1 - i + m.capacity) % m.capacity
		record := m.buffer[idx]
		lastScanned = i

		if m.matchesFilter(record, filter) {
			result = append(result, record)
		}
		if len(result) >= limit {
			break
		}
	}

	if lastScanned >= startOffset && lastScanned != m.count-1 {
		lastIdx := (m.head - 1 - lastScanned + m.capacity) % m.capacity
		nextBeforeID = m.buffer[lastIdx].ID
	}

	return result, nextBeforeID, true
}

// GetByID returns a request record by its ID.
// Returns the record and true if found, or an empty record and false if not found.
func (m *RequestManager) GetByID(id string) (RequestRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// O(1) lookup; the index points at the newest copy of a duplicated ID,
	// matching the old newest→oldest scan (#71).
	if idx, ok := m.idIndex[id]; ok {
		return m.buffer[idx], true
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
	// Latch writes BEFORE subscriptions: a destroy-path Close must guarantee
	// that any Record/Upsert observing the latch is rejected, so every record
	// that DID land in the ring is covered by the destroy's final purge.
	m.writesClosed.Store(true)

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

	// Rebuild the index from the compacted buffer (#71). kept is oldest→newest,
	// so a later assignment for a duplicated ID overwrites the earlier one,
	// leaving the newest copy — the invariant the index promises.
	newIndex := make(map[string]int, len(kept))
	for p, rec := range kept {
		newIndex[rec.ID] = p
	}
	m.idIndex = newIndex
	m.mu.Unlock()

	// Call eviction callbacks outside of lock
	if onEvict != nil {
		for _, id := range evictIDs {
			onEvict(id)
		}
	}
}
