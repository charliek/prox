package proxy

import (
	"container/heap"
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

	// Evicted marks a body whose DATA is gone but whose metadata (and the
	// record's headers) is retained: the record fell outside the ring's
	// captured-body detail window (constants.ProxyRequestDetailWindow, D9b), so
	// Data was dropped and any spilled file unlinked. LoadCapturedBody turns
	// this into an os.ErrNotExist-wrapped error so it flows through the SAME
	// path as a disk-budget-evicted file and reports unavailable_reason
	// "evicted" — callers need no new case. It crosses the daemon→forwarder
	// wire so a backfilled record arrives already marked.
	Evicted bool `json:"evicted,omitempty"`
}

// requestIDCounter disambiguates requests that share a timestamp/method/URL.
// Without it, two simultaneous identical requests would hash to the same ID
// and their capture files would overwrite (and cross-delete on eviction).
var requestIDCounter atomic.Uint64

// GenerateRequestID creates a short hash ID (12 chars, git-style) from request
// data plus a per-process counter, so IDs are unique within a process even for
// simultaneous identical requests. (Truncating the hash to 48 bits keeps the
// birthday-collision residual across a DefaultProxyRequestBufferSize-record ring
// negligible, unlike
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
//
// closed latches the channel close so the several paths that can end a
// subscription — Unsubscribe, manager Close, and the overflow drop (C6) —
// can never double-close it.
type RequestSubscription struct {
	ID      string
	Filter  RequestFilter
	Ch      chan RequestRecord
	dropped atomic.Int64
	closed  atomic.Bool
}

// closeLatched closes the subscription's channel exactly once. Every close path
// goes through it. Callers must hold the manager's subMu WRITE lock (or own the
// subscription outright, as Subscribe does before publishing it), so a close can
// never race a non-blocking send in notifySubscribers — those run under the read
// lock.
func (s *RequestSubscription) closeLatched() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.Ch)
	}
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

	// detailWindow bounds how many of the NEWEST records keep their captured
	// BODY data (D9b). Retention (capacity) and body retention are deliberately
	// separate: metadata + headers are cheap enough to keep for the whole ring,
	// bodies are not. Once a record falls past this many positions from the
	// newest, evictDetailWindowLocked replaces that slot's record with a
	// body-stripped copy (headers KEPT) and its spilled files are unlinked
	// through onEvict. A value <= 0, or one >= capacity, disables the window —
	// every record keeps its bodies.
	detailWindow int

	// bodyWindow bounds how many records in this ring may hold RETAINED INLINE
	// captured body data (CapturedBody.Data). It is the counterpart of
	// detailWindow for a ring that REPLICATES records captured elsewhere, and
	// the two are mutually exclusive: the shared constructor panics if both are
	// set, because composing a position window and a timestamp window would
	// make the strip order depend on which one fired first.
	//
	// Where a capture-owning ring bounds bodies by ring POSITION, a replica
	// must bound them by the daemon-supplied Timestamp, because arrival
	// position on a replica is not recency. The forwarder's backfill
	// deliberately races the live event stream, so a record delivered live
	// before its backfill copy arrives sinks toward the ring's OLDEST position
	// even though it is one of the newest requests; a position window would
	// strip a body the daemon still serves. Timestamps ride the wire verbatim
	// from the daemon's single clock, so that same record still ranks as new.
	//
	// Only inline Data is counted and stripped. A spilled FilePath body costs
	// this ring no memory — the bytes live in the daemon's capture dir, which
	// the replica does not own — and it self-corrects: once the daemon unlinks
	// the file, the local load path already reports "evicted". Stripping it
	// here would refuse a body the daemon still serves for zero memory gain.
	// A strip therefore never needs to unlink anything, so this path produces
	// no cleanup IDs and never reaches the eviction callback.
	//
	// A value <= 0 disables the mechanism entirely (no allocation, no
	// bookkeeping); a value >= capacity is legal and simply never binds before
	// ring capacity does.
	bodyWindow int

	// bodyHeap orders the slots that hold retained inline body data by
	// (Timestamp, insertion ordinal), oldest first, so enforcing the bound is
	// one pop. bodySlots is the inverse: bodySlots[s] is the heap entry
	// belonging to the CURRENT occupant of ring slot s, or nil when that
	// occupant holds no retained inline body.
	//
	// Identity here is the SLOT, not the record ID: the ring deliberately
	// permits duplicate explicit IDs, so an ID does not name a unique
	// occupant. Every mutation that changes a slot's occupant goes through
	// replaceSlotLocked, which updates both structures eagerly, making a stale
	// entry unrepresentable rather than merely unlikely.
	// Both are nil when bodyWindow <= 0. Guarded by mu, like the ring itself.
	bodyHeap  bodyWindowHeap
	bodySlots []*bodyHeapEntry

	// bodySeq is the monotonic insertion ordinal that breaks Timestamp ties in
	// bodyHeap, so records sharing a timestamp are stripped in arrival order
	// rather than in whatever order the heap happens to lay them out.
	bodySeq uint64
}

// bodyHeapEntry records one ring slot's membership in the replica body window.
// heapIdx is the entry's live position in bodyHeap, maintained by Swap, so an
// entry can be removed in O(log n) when its slot is vacated instead of being
// searched for.
type bodyHeapEntry struct {
	ts      int64
	seq     uint64
	slot    int
	heapIdx int
}

// bodyWindowHeap is a container/heap min-heap of bodyHeapEntry ordered by
// (ts, seq). Its minimum is the oldest record still holding inline body data —
// the one a strip must take.
type bodyWindowHeap []*bodyHeapEntry

func (h bodyWindowHeap) Len() int { return len(h) }

func (h bodyWindowHeap) Less(i, j int) bool {
	if h[i].ts != h[j].ts {
		return h[i].ts < h[j].ts
	}
	return h[i].seq < h[j].seq
}

func (h bodyWindowHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *bodyWindowHeap) Push(x any) {
	entry := x.(*bodyHeapEntry)
	entry.heapIdx = len(*h)
	*h = append(*h, entry)
}

func (h *bodyWindowHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil // drop the reference so the entry can be collected
	entry.heapIdx = -1
	*h = old[:n-1]
	return entry
}

// NewRequestManager creates a new request manager with the specified buffer
// capacity and the default captured-body detail window
// (constants.ProxyRequestDetailWindow).
func NewRequestManager(capacity int) *RequestManager {
	return newRequestManagerWithDetailWindow(capacity, constants.ProxyRequestDetailWindow)
}

// NewReplicaRequestManager creates a request manager for a ring that REPLICATES
// records captured elsewhere (the forwarder-fed project-local ring in shared
// mode) rather than owning capture itself. It runs the timestamp-ordered body
// window (bodyWindow) instead of the position-ordered detail window, bounding
// retained INLINE body data at constants.ProxyRequestDetailWindow records —
// parity with a capture-owning ring's inline worst case.
//
// A replica needs its own bound because upstream stripping does not reach it:
// the daemon's detail window publishes no event when it drops a body, so a
// record forwarded LIVE arrives with its body and keeps it locally forever,
// long after the daemon has evicted it. Only BACKFILL-delivered records past
// the daemon's window arrive already marked CapturedBody.Evicted.
//
// The window is ordered by the daemon-supplied Timestamp, not by ring position,
// because position on a replica is not recency: the forwarder's backfill
// deliberately races the live event stream and Upsert treats a final record as
// terminal, so a record delivered live BEFORE its backfill copy sinks toward
// the ring's oldest position while still being one of the newest requests. A
// position window would strip its body while the daemon still serves it;
// timestamp order ranks it correctly, because Timestamp is set once daemon-side
// and rides the wire verbatim. That is a bounded approximation of daemon truth,
// not a mirror of it — a clock step can change WHICH record is stripped, never
// HOW MANY retain data.
//
// Spilled (FilePath) bodies are exempt: they cost this ring no memory and the
// daemon's disk truth is authoritative on load. See the bodyWindow field.
func NewReplicaRequestManager(capacity int) *RequestManager {
	return newReplicaRequestManagerWithBodyWindow(capacity, constants.ProxyRequestDetailWindow)
}

// newRequestManagerWithDetailWindow is NewRequestManager with an explicit
// captured-body detail window, so tests can exercise the window with a handful
// of records instead of thousands. Deliberately unexported: the window is not a
// configurable knob (see docs/reference/configuration.md), so production code
// must not be able to pick an arbitrary window — the only sanctioned variants
// are the default and the replica's window above. A non-positive detailWindow
// keeps bodies for every record in the ring.
func newRequestManagerWithDetailWindow(capacity, detailWindow int) *RequestManager {
	return newRequestManagerWithWindows(capacity, detailWindow, 0)
}

// newReplicaRequestManagerWithBodyWindow is NewReplicaRequestManager with an
// explicit inline-body window, so tests can exercise the window with a handful
// of records instead of thousands. Unexported for the same reason as its detail
// window sibling: the window is a constant, not a knob. A non-positive window
// keeps every inline body.
func newReplicaRequestManagerWithBodyWindow(capacity, bodyWindow int) *RequestManager {
	return newRequestManagerWithWindows(capacity, 0, bodyWindow)
}

// newRequestManagerWithWindows is the single constructor behind every variant.
//
// It PANICS when both windows are requested. They are alternative answers to
// the same question — how much captured body data may this ring hold — chosen
// by whether the ring owns capture or replicates it, and running both would
// leave the strip order dependent on which fired first. No caller has any
// reason to ask for both, so a construction-time panic is cheaper than a
// semantics nobody would be able to reason about.
//
// The panic is load-bearing, not merely tidy: evictDetailWindowLocked strips a
// slot's inline body by writing m.buffer directly, without going through
// bodyWindowVacateLocked, so a co-running pair would leave the body heap
// charging for bytes that are gone and break the bodySlots invariant. Anyone
// tempted to delete this guard must make that path window-aware first.
func newRequestManagerWithWindows(capacity, detailWindow, bodyWindow int) *RequestManager {
	if capacity <= 0 {
		capacity = 1
	}
	if detailWindow != 0 && bodyWindow != 0 {
		panic(fmt.Sprintf("proxy: request manager cannot run both windows (detailWindow=%d, bodyWindow=%d)", detailWindow, bodyWindow))
	}
	m := &RequestManager{
		buffer:       make([]RequestRecord, capacity),
		capacity:     capacity,
		subs:         make(map[string]*RequestSubscription),
		idIndex:      make(map[string]int, capacity),
		detailWindow: detailWindow,
		bodyWindow:   bodyWindow,
	}
	if bodyWindow > 0 {
		m.bodySlots = make([]*bodyHeapEntry, capacity)
		// The heap holds at most one entry per slot, and at most one over the
		// window (push-then-drain), so it can be sized once instead of doubling
		// its way there as the ring warms up.
		m.bodyHeap = make(bodyWindowHeap, 0, min(bodyWindow+1, capacity))
	}
	return m
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
	stored, evictIDs, onEvict := m.appendRecord(record)
	m.mu.Unlock()

	// Call eviction callbacks outside of lock
	runEvictions(onEvict, evictIDs)

	// Notify subscribers with the record as STORED, not as submitted: the
	// replica body window can strip the arriving record itself (its timestamp
	// may already be the oldest bodied one), and no entry point may advertise
	// body data the ring will refuse to serve.
	m.notifySubscribers(stored)
	return true
}

// runEvictions invokes onEvict for each ID. Every ring path that produces
// cleanup work funnels through it AFTER releasing m.mu — the callback does disk
// IO.
func runEvictions(onEvict EvictionCallback, ids []string) {
	if onEvict == nil {
		return
	}
	for _, id := range ids {
		onEvict(id)
	}
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
// lock-step on both append paths.
//
// It returns the record as actually STORED, which is not always the record
// passed in: on a ring running the replica body window the new record can be
// the one the window strips (its timestamp may already be the oldest bodied
// one). Callers notify subscribers with this copy, never with their own.
//
// It also returns the IDs whose on-disk capture files the caller must clean up
// after releasing m.mu (disk IO must not run under the lock), together with the
// eviction callback. Two independent things can land in that list per append:
// the record pushed out of the ring entirely, and — because the append advanced
// head by one — the record that just fell outside the captured-body detail
// window (D9b). Both are amortized O(1): the
// window boundary moves exactly one slot per append, so at most ONE record
// crosses it and no scan is needed. The body window contributes nothing here:
// it only ever drops inline bytes, which need no disk work.
//
// This is the shared append arm behind Record and Upsert's insert case. Caller
// holds m.mu.
func (m *RequestManager) appendRecord(record RequestRecord) (stored RequestRecord, evictIDs []string, onEvict EvictionCallback) {
	slot := m.head
	if m.count == m.capacity {
		evicted := m.buffer[slot]
		if evicted.ID != "" && evicted.Details != nil {
			evictIDs = append(evictIDs, evicted.ID)
		}
		m.evictIndex(evicted.ID, slot)
	}
	// The slot's previous occupant is leaving whether or not the ring was full,
	// so its body-window membership goes with it — replaceSlotLocked hands that
	// membership to the arriving record, and may strip the arrival itself.
	stored = m.replaceSlotLocked(slot, record)
	m.idIndex[record.ID] = slot // newest copy of this ID lives here (#71)
	m.head = (m.head + 1) % m.capacity
	if m.count < m.capacity {
		m.count++
	}
	if id := m.evictDetailWindowLocked(); id != "" {
		evictIDs = append(evictIDs, id)
	}
	return stored, evictIDs, m.onEvict
}

// evictDetailWindowLocked strips the captured body data from the single record
// that just fell outside the newest-detailWindow window, and returns its ID when
// that record had SPILLED bodies whose files now need unlinking ("" otherwise —
// an inline-only body needs no disk work). Called once per append, after head
// has advanced, so the boundary only ever moves by one slot: O(1), never a scan
// of the ring.
//
// Deliberately publishes NO event, unlike Upsert's out-of-window arm: crossing
// the boundary is not a change to the request, and one event per append would
// double this ring's notification traffic forever. Clients learn a body is gone
// when they next fetch it (unavailable_reason "evicted"); a client still holding
// the pre-eviction copy may show a body the ring no longer serves, which is the
// accepted cost. Caller holds m.mu.
func (m *RequestManager) evictDetailWindowLocked() string {
	if m.detailWindow <= 0 || m.count <= m.detailWindow {
		return ""
	}
	// The record at newest→oldest offset detailWindow is the one that just
	// crossed the boundary. detailWindow < count <= capacity here, so the
	// single +capacity is enough to keep the dividend non-negative.
	slot := (m.head - 1 - m.detailWindow + m.capacity) % m.capacity
	rec := m.buffer[slot]
	if rec.ID == "" || rec.Details == nil {
		return ""
	}
	stripped, spilled := evictBodies(rec)
	if stripped.Details == rec.Details {
		// Nothing here held body data — a bodyless GET, or a record that arrived
		// already Evicted over the daemon→forwarder wire. evictBodies allocated
		// nothing, so leave the slot untouched rather than rewriting it.
		return ""
	}
	m.buffer[slot] = stripped
	if !spilled {
		return ""
	}
	return rec.ID
}

// outsideDetailWindowLocked reports whether the record in slot has already
// fallen past the captured-body detail window, i.e. whether storing body data
// there would violate the window invariant. Caller holds m.mu.
func (m *RequestManager) outsideDetailWindowLocked(slot int) bool {
	if m.detailWindow <= 0 {
		return false
	}
	pos := (m.head - 1 - slot + m.capacity) % m.capacity
	return pos >= m.detailWindow
}

// evictBodies returns a copy of record whose captured body DATA is gone: inline
// bytes dropped, any spilled FilePath cleared, and each affected body marked
// Evicted so a later load reports unavailable_reason "evicted" instead of
// masquerading as an empty body. Captured HEADERS are kept — that is the whole
// point of the detail window: the record stays inspectable, only its payload
// goes. A body that never held data (a bodyless GET) is left untouched rather
// than mislabeled as evicted.
//
// Fresh *RequestDetails and *CapturedBody values are allocated rather than
// mutated in place: subscribers, Recent, and GetByID all hand out record COPIES
// that share those pointers, so mutating them would race a reader serializing
// the record outside m.mu. Swapping the pointer under m.mu is safe — every
// reader takes the copy under the lock.
//
// spilled reports whether any evicted body had spilled to disk, i.e. whether the
// caller must invoke the eviction callback to unlink files.
//
// When there was nothing to evict, stripped.Details is the ORIGINAL pointer —
// callers can use that identity to skip rewriting a ring slot.
func evictBodies(record RequestRecord) (stripped RequestRecord, spilled bool) {
	if record.Details == nil {
		return record, false
	}
	reqBody, reqSpilled := evictedBody(record.Details.RequestBody)
	resBody, resSpilled := evictedBody(record.Details.ResponseBody)
	if reqBody == record.Details.RequestBody && resBody == record.Details.ResponseBody {
		return record, false
	}
	// Copy-then-override, not a field-by-field rebuild: a field added to
	// RequestDetails later must survive eviction by construction.
	details := *record.Details
	details.RequestBody = reqBody
	details.ResponseBody = resBody
	record.Details = &details
	return record, reqSpilled || resSpilled
}

// evictedBody returns body with its data removed and Evicted set, allocating a
// copy so the original (shared with readers) is never mutated. A nil body, an
// already-evicted one, and one that holds no data at all are returned as-is.
// spilled reports whether the body had a disk file that now needs unlinking.
func evictedBody(body *CapturedBody) (evicted *CapturedBody, spilled bool) {
	if body == nil || body.Evicted {
		return body, false
	}
	if len(body.Data) == 0 && body.FilePath == "" {
		return body, false
	}
	copied := *body
	copied.Data = nil
	copied.FilePath = ""
	copied.Evicted = true
	return &copied, body.FilePath != ""
}

// hasRetainedInlineBody reports whether record still holds captured body bytes
// IN THIS PROCESS'S MEMORY — the single predicate behind the replica body
// window's accounting and its strip. A record qualifies when it has captured
// Details and at least one body with un-evicted inline Data.
//
// Spilled bodies deliberately do NOT qualify: a FilePath costs the ring only
// the string, and on a replica the bytes belong to the daemon (see the
// bodyWindow field). Counting them would make the window strip records for
// memory it never held.
func hasRetainedInlineBody(record RequestRecord) bool {
	if record.Details == nil {
		return false
	}
	return hasRetainedInlineData(record.Details.RequestBody) || hasRetainedInlineData(record.Details.ResponseBody)
}

// hasRetainedInlineData is hasRetainedInlineBody for a single body. Named for
// the question it answers, not the value it returns: its file neighbours
// evictedBody and strippedInlineBody are noun-phrase functions that hand back a
// *CapturedBody, and this one is a predicate.
func hasRetainedInlineData(body *CapturedBody) bool {
	return body != nil && !body.Evicted && len(body.Data) > 0
}

// stripInlineBodies returns a copy of record whose retained INLINE body bytes
// are gone and marked Evicted, so a later load reports unavailable_reason
// "evicted" rather than masquerading as an empty body. It is evictBodies'
// sibling for the replica body window, and differs from it in exactly one way:
// a spilled body is left completely untouched — FilePath intact, Evicted
// unset — because dropping it would free no memory here and would refuse a
// body the daemon still serves. Consequently a strip never produces work for
// the eviction callback: there is nothing to unlink.
//
// Headers and every other captured field survive, and the copy-on-write
// discipline is evictBodies': fresh *RequestDetails and *CapturedBody values
// rather than mutation, because subscribers, Recent, and GetByID all hand out
// record copies that SHARE those pointers. Mutating them would race a reader
// serializing the record outside m.mu; swapping the pointer under m.mu is safe
// because every reader takes its copy under the lock.
//
// The two early returns are defence only: the window hands this function
// nothing but records its heap has already certified as holding retained inline
// data. Unlike evictBodies — whose caller really does test Details identity to
// skip a slot rewrite — no caller here consults the returned pointer.
//
// Data and FilePath are mutually exclusive on a captured body (a body is
// spilled or inline, never both), so a stripped body never ends up marked
// evicted while still naming a readable file.
func stripInlineBodies(record RequestRecord) RequestRecord {
	if record.Details == nil {
		return record
	}
	reqBody := strippedInlineBody(record.Details.RequestBody)
	resBody := strippedInlineBody(record.Details.ResponseBody)
	if reqBody == record.Details.RequestBody && resBody == record.Details.ResponseBody {
		return record
	}
	// Copy-then-override, not a field-by-field rebuild: a field added to
	// RequestDetails later must survive stripping by construction.
	details := *record.Details
	details.RequestBody = reqBody
	details.ResponseBody = resBody
	record.Details = &details
	return record
}

// strippedInlineBody returns body without its inline data, allocating a copy so
// the original (shared with readers) is never mutated. A body with no retained
// inline data — nil, already evicted, spilled, or never bodied at all — is
// returned as-is.
func strippedInlineBody(body *CapturedBody) *CapturedBody {
	if !hasRetainedInlineData(body) {
		return body
	}
	copied := *body
	copied.Data = nil
	copied.Evicted = true
	return &copied
}

// bodyWindowVacateLocked drops slot's body-window membership because its
// occupant is being replaced or removed. Every path that changes what lives in
// a slot calls it, which is what keeps a heap entry from outliving the record
// that created it. Caller holds m.mu.
func (m *RequestManager) bodyWindowVacateLocked(slot int) {
	if m.bodyWindow <= 0 {
		return
	}
	if entry := m.bodySlots[slot]; entry != nil {
		heap.Remove(&m.bodyHeap, entry.heapIdx)
		m.bodySlots[slot] = nil
	}
}

// newBodyEntryLocked mints the heap entry for slot's current occupant. It is the
// single place the heap's sort key is derived, so the ordering rule below has
// one definition rather than one per insertion path. Caller holds m.mu.
func (m *RequestManager) newBodyEntryLocked(slot int) *bodyHeapEntry {
	m.bodySeq++
	return &bodyHeapEntry{
		// UnixNano on a zero or regressed Timestamp sorts oldest, so such a
		// record is stripped first. That is the memory-safe direction, and the
		// alternative (trusting an unusable timestamp) would let a record with
		// no clock reading hold memory ahead of records that have one.
		ts:   m.buffer[slot].Timestamp.UnixNano(),
		seq:  m.bodySeq,
		slot: slot,
	}
}

// bodyWindowStoreLocked registers slot's NEW occupant with the body window and
// re-enforces the bound. Callers must have written the record into
// m.buffer[slot] and vacated the previous occupant first — replaceSlotLocked is
// the one sanctioned way to do both.
//
// It reads and writes m.buffer[slot] rather than taking and returning a record,
// because the strip loop can take the record just stored — a completion whose
// start-timestamp is already the oldest, or a backfilled record arriving after
// fresher live ones. Self-stripping is the correct outcome, and keeping the ring
// slot as the only copy is what stops a caller from notifying subscribers with a
// record the ring no longer serves.
//
// Enforcement is immediate rather than eventual: the loop runs until the heap
// is back within the window, so the memory bound holds after every mutation.
// Caller holds m.mu.
func (m *RequestManager) bodyWindowStoreLocked(slot int) {
	if m.bodyWindow <= 0 || !hasRetainedInlineBody(m.buffer[slot]) {
		return
	}

	entry := m.newBodyEntryLocked(slot)
	heap.Push(&m.bodyHeap, entry)
	m.bodySlots[slot] = entry

	for len(m.bodyHeap) > m.bodyWindow {
		victim := heap.Pop(&m.bodyHeap).(*bodyHeapEntry)
		m.bodySlots[victim.slot] = nil
		m.buffer[victim.slot] = stripInlineBodies(m.buffer[victim.slot])
	}
}

// replaceSlotLocked makes record the occupant of slot, transferring body-window
// membership from the outgoing occupant to it, and returns the record AS STORED.
//
// It is the only sanctioned way to change a slot's occupant: the body window's
// "no entry outlives the record that created it" invariant depends on vacate and
// store bracketing every such write, and a helper makes that structural instead
// of a rule each call site has to remember.
//
// The returned record is not always the one passed in — the window can strip the
// arrival itself (see bodyWindowStoreLocked) — and callers must notify
// subscribers with the returned copy, never with their own. Caller holds m.mu.
func (m *RequestManager) replaceSlotLocked(slot int, record RequestRecord) RequestRecord {
	m.bodyWindowVacateLocked(slot)
	m.buffer[slot] = record
	m.bodyWindowStoreLocked(slot)
	return m.buffer[slot]
}

// rebuildBodyWindowLocked re-links the body window after PurgeByProject's
// compaction moved records between slots: entries[i] is the surviving heap
// entry for the record now living in slot i (nil when that record holds no
// retained inline body). O(n) with heap.Init, on a path that runs only when a
// project's records are dropped.
//
// The surviving entries are REUSED, not re-minted, because seq is more than a
// unique ordinal: it is the equal-timestamp tie-break, and it records
// BODY-INSERTION order, which ring order cannot reconstruct — an in-place
// completion gains its body long after its slot was appended. Minting fresh
// entries in ring order would silently reorder which same-timestamp record the
// next strip takes.
//
// The bound cannot be violated by a rebuild — a purge only removes records — so
// no strip loop is needed. Caller holds m.mu with the compacted buffer already
// installed.
func (m *RequestManager) rebuildBodyWindowLocked(entries []*bodyHeapEntry) {
	if m.bodyWindow <= 0 {
		return
	}
	clear(m.bodySlots)
	clear(m.bodyHeap) // drop the references so the purged entries can be collected
	m.bodyHeap = m.bodyHeap[:0]
	for slot, entry := range entries {
		if entry == nil {
			continue
		}
		entry.slot = slot
		entry.heapIdx = len(m.bodyHeap)
		m.bodyHeap = append(m.bodyHeap, entry)
		m.bodySlots[slot] = entry
	}
	heap.Init(&m.bodyHeap)
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
// ring slot (no head/count change, no ring eviction) — safe because in-flight
// records carry no Details, so the transition only ever adds capture state. The
// one exception is the captured-body windows: a completion landing in a slot
// that has already aged out of the POSITION window (D9b) has its bodies
// stripped before it is stored, so a slow request cannot reintroduce body data
// outside the window, and on a replica the completion joins the timestamp
// window (bodyWindow) and may be stripped by it — including by its own
// arrival. Either way the record NOTIFIED is the record stored (see the case
// below).
//
// Unlike Record, subscribers are notified INSIDE the ring critical section:
// same-ID notifications can never be observed out of transition order.
// notifySubscribers only performs non-blocking channel sends (plus, on
// overflow, a subscription removal) under subMu, and no path acquires mu
// while holding subMu, so this cannot block or deadlock. The eviction
// callback (disk IO) still runs after unlock.
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

	var evictIDs []string
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
		//
		// A long-running in-flight record can outlive its own slot's stay in the
		// captured-body detail window: by the time the completion arrives, that
		// slot may already sit outside the newest-detailWindow window (D9b). The
		// completion carries the bodies, so storing it verbatim would resurrect
		// body data in an out-of-window slot and break the invariant. Strip the
		// bodies BEFORE storing, and notify subscribers with the stripped record
		// so the wire never advertises data this ring refuses to serve.
		if m.outsideDetailWindowLocked(idx) {
			var spilled bool
			record, spilled = evictBodies(record)
			if spilled {
				evictIDs, onEvict = []string{record.ID}, m.onEvict
			}
		}
		// The completion is what puts body data in this slot, so it is also
		// what can push the replica body window over its bound — possibly
		// stripping itself, when the request STARTED before every other bodied
		// record in the ring. replaceSlotLocked also vacates the outgoing
		// occupant: an in-flight record carries no Details today, but the
		// invariant must not depend on that.
		record = m.replaceSlotLocked(idx, record)
	default:
		record, evictIDs, onEvict = m.appendRecord(record)
	}
	// Always the stored copy — see appendRecord.
	m.notifySubscribers(record)
	m.mu.Unlock()

	runEvictions(onEvict, evictIDs)
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
		sub.closeLatched()
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
		sub.closeLatched()
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
		sub.closeLatched()
		delete(m.subs, id)
	}
}

// notifySubscribers delivers record to every matching subscription and ends the
// ones that overflowed.
//
// Overflow is NOT a silent drop (C6): a subscriber whose channel is full has
// lost a record, and there is no way to hand it that record later, so the
// subscription is closed and removed instead. The SSE handlers see the closed
// channel as end-of-stream and return; the client reconnects and re-syncs from
// a fresh snapshot, which is the only repair that actually restores the missing
// record. The local TUI subscribes to the same manager, so a local overflow
// likewise closes its feed — surfaced by the local close-reporting path as
// "requests: disconnected", which is strictly better than silently showing an
// incomplete list.
//
// The close runs OUTSIDE the read lock, under the write lock: sends happen
// under the read lock, so taking the write lock is what guarantees no send can
// be in flight while a channel is being closed. No path acquires the ring mutex
// while holding subMu, so this still cannot deadlock with the Upsert caller
// that holds it.
func (m *RequestManager) notifySubscribers(record RequestRecord) {
	for _, sub := range m.deliver(record) {
		m.dropSubscription(sub)
	}
}

// deliver performs the non-blocking sends under the read lock and returns the
// subscriptions whose channels were full. Caller must not hold subMu.
//
// Sending to a subscription still present in m.subs is safe without a
// closed check: every close path removes the subscription from the map and
// closes it in the same write-lock critical section, so a subscription visible
// here cannot be closed. Two concurrent deliveries may both report the same
// overflowed subscription; dropSubscription is idempotent.
func (m *RequestManager) deliver(record RequestRecord) []*RequestSubscription {
	m.subMu.RLock()
	defer m.subMu.RUnlock()

	var overflowed []*RequestSubscription
	for _, sub := range m.subs {
		if !m.matchesFilter(record, sub.Filter) {
			continue
		}
		select {
		case sub.Ch <- record:
		default:
			// Channel full: count the overflow (D9) and mark the subscription
			// for closure. The manager-wide total feeds DroppedEvents(); the
			// per-subscription count logs once on the first drop so a slow
			// subscriber is visible without a per-drop log flood. The log
			// itself runs on a goroutine: notifySubscribers is called with the
			// ring mutex held on the Upsert path (ordering guarantee), and
			// logger I/O must not stall the SSE hot path under that lock.
			m.droppedTotal.Add(1)
			if sub.dropped.Add(1) == 1 {
				go log.Printf("prox: request subscription %s overflowed and was closed (subscriber not keeping up)", sub.ID)
			}
			overflowed = append(overflowed, sub)
		}
	}
	return overflowed
}

// dropSubscription removes an overflowed subscription and closes its channel.
// The map entry is compared by pointer so a re-Subscribe that reused the ID
// (impossible today, nextID is monotonic — but cheap insurance) is not evicted
// by a straggling drop, and the latched close makes a doubled drop a no-op.
func (m *RequestManager) dropSubscription(sub *RequestSubscription) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	if cur, ok := m.subs[sub.ID]; ok && cur == sub {
		delete(m.subs, sub.ID)
	}
	sub.closeLatched()
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
//
// The compaction needs no detail-window work (D9b): it preserves arrival order
// and only removes records, so a surviving record's newest→oldest offset can
// only shrink. Records can move INTO the window (they stay stripped — evicted
// bodies are gone for good) but never out of it, so no new violation is created.
// The replica's timestamp-ordered body window is keyed by SLOT rather than by
// offset, so compaction DOES invalidate it; it is rebuilt below.
func (m *RequestManager) PurgeByProject(projectDir string) {
	if projectDir == "" {
		return
	}

	var evictIDs []string
	var onEvict EvictionCallback

	m.mu.Lock()
	onEvict = m.onEvict

	// Rebuild the buffer keeping only non-matching records in order. The
	// surviving body-window entries travel WITH their records (keptEntries[i]
	// belongs to kept[i]) so the rebuild can preserve each one's (ts, seq) —
	// see rebuildBodyWindowLocked for why they must not be re-minted.
	kept := make([]RequestRecord, 0, m.count)
	var keptEntries []*bodyHeapEntry
	if m.bodyWindow > 0 {
		keptEntries = make([]*bodyHeapEntry, 0, m.count)
	}
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
		if m.bodyWindow > 0 {
			keptEntries = append(keptEntries, m.bodySlots[idx])
		}
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

	// The body window's entries are slot-keyed, and compaction moved the
	// records between slots, so it is rebuilt rather than patched.
	m.rebuildBodyWindowLocked(keptEntries)
	m.mu.Unlock()

	// Call eviction callbacks outside of lock
	runEvictions(onEvict, evictIDs)
}
