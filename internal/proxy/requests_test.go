package proxy

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/constants"
)

func TestRequestManager_Record(t *testing.T) {
	m := NewRequestManager(10)

	record := RequestRecord{
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/api/users",
		Subdomain:  "api",
		StatusCode: 200,
		Duration:   100 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
	}

	m.Record(record)
	assert.Equal(t, 1, m.Count())
}

func TestRequestManager_Recent(t *testing.T) {
	m := NewRequestManager(10)

	// Add some records
	for i := 0; i < 5; i++ {
		m.Record(RequestRecord{
			Timestamp:  time.Now().Add(time.Duration(i) * time.Second),
			Method:     "GET",
			URL:        "/api/users",
			Subdomain:  "api",
			StatusCode: 200,
			Duration:   100 * time.Millisecond,
		})
	}

	t.Run("returns all records", func(t *testing.T) {
		records := m.Recent(RequestFilter{})
		assert.Len(t, records, 5)
	})

	t.Run("respects limit", func(t *testing.T) {
		records := m.Recent(RequestFilter{Limit: 3})
		assert.Len(t, records, 3)
	})

	t.Run("returns newest first", func(t *testing.T) {
		records := m.Recent(RequestFilter{})
		for i := 1; i < len(records); i++ {
			assert.True(t, records[i-1].Timestamp.After(records[i].Timestamp) ||
				records[i-1].Timestamp.Equal(records[i].Timestamp))
		}
	})
}

func TestRequestManager_Filter(t *testing.T) {
	m := NewRequestManager(100)

	// Add mixed records
	m.Record(RequestRecord{Subdomain: "api", Method: "GET", StatusCode: 200})
	m.Record(RequestRecord{Subdomain: "api", Method: "POST", StatusCode: 201})
	m.Record(RequestRecord{Subdomain: "app", Method: "GET", StatusCode: 200})
	m.Record(RequestRecord{Subdomain: "api", Method: "GET", StatusCode: 500})

	t.Run("filter by subdomain", func(t *testing.T) {
		records := m.Recent(RequestFilter{Subdomain: "api"})
		assert.Len(t, records, 3)
	})

	t.Run("filter by method", func(t *testing.T) {
		records := m.Recent(RequestFilter{Method: "GET"})
		assert.Len(t, records, 3)
	})

	t.Run("filter by status range", func(t *testing.T) {
		records := m.Recent(RequestFilter{MinStatus: 200, MaxStatus: 299})
		assert.Len(t, records, 3)
	})

	t.Run("combined filters", func(t *testing.T) {
		records := m.Recent(RequestFilter{Subdomain: "api", Method: "GET"})
		assert.Len(t, records, 2)
	})
}

func TestRequestManager_FilterByURLContains(t *testing.T) {
	m := NewRequestManager(100)

	m.Record(RequestRecord{Subdomain: "api", Method: "GET", URL: "/api/Users?id=1"})
	m.Record(RequestRecord{Subdomain: "api", Method: "GET", URL: "/healthz"})
	m.Record(RequestRecord{Subdomain: "api", Method: "GET", URL: "/api/orders"})

	t.Run("case-insensitive substring match on path", func(t *testing.T) {
		records := m.Recent(RequestFilter{URLContains: "users"})
		assert.Len(t, records, 1)
		assert.Equal(t, "/api/Users?id=1", records[0].URL)
	})

	t.Run("matches query string", func(t *testing.T) {
		records := m.Recent(RequestFilter{URLContains: "id=1"})
		assert.Len(t, records, 1)
	})

	t.Run("matches multiple records by common substring", func(t *testing.T) {
		records := m.Recent(RequestFilter{URLContains: "/api/"})
		assert.Len(t, records, 2)
	})

	t.Run("no match returns empty", func(t *testing.T) {
		records := m.Recent(RequestFilter{URLContains: "nonexistent"})
		assert.Len(t, records, 0)
	})

	t.Run("empty filter matches all", func(t *testing.T) {
		records := m.Recent(RequestFilter{URLContains: ""})
		assert.Len(t, records, 3)
	})

	t.Run("does not match against other fields", func(t *testing.T) {
		// "api" appears in Subdomain but not URL for /healthz - ensure the
		// filter only inspects URL, not Subdomain or Method.
		records := m.Recent(RequestFilter{URLContains: "healthz"})
		assert.Len(t, records, 1)
		assert.Equal(t, "/healthz", records[0].URL)
	})
}

func TestRequestManager_RingBuffer(t *testing.T) {
	m := NewRequestManager(5)

	// Add more records than capacity
	for i := 0; i < 10; i++ {
		m.Record(RequestRecord{
			StatusCode: i,
		})
	}

	assert.Equal(t, 5, m.Count())

	records := m.Recent(RequestFilter{})
	assert.Len(t, records, 5)

	// Should have the newest records (5-9)
	for i, r := range records {
		expected := 9 - i
		assert.Equal(t, expected, r.StatusCode)
	}
}

func TestRequestManager_Subscribe(t *testing.T) {
	m := NewRequestManager(10)

	sub := m.Subscribe(RequestFilter{Subdomain: "api"})
	require.NotNil(t, sub)

	// Record a matching request
	go func() {
		m.Record(RequestRecord{Subdomain: "api", Method: "GET"})
	}()

	select {
	case record := <-sub.Ch:
		assert.Equal(t, "api", record.Subdomain)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for record")
	}

	// Record a non-matching request
	m.Record(RequestRecord{Subdomain: "app", Method: "GET"})

	select {
	case <-sub.Ch:
		t.Fatal("should not receive non-matching record")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}

	m.Unsubscribe(sub.ID)
}

func TestRequestManager_Unsubscribe(t *testing.T) {
	m := NewRequestManager(10)

	sub := m.Subscribe(RequestFilter{})
	m.Unsubscribe(sub.ID)

	// Channel should be closed
	_, ok := <-sub.Ch
	assert.False(t, ok)
}

func TestGenerateRequestID(t *testing.T) {
	timestamp := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	t.Run("generates 12 character ID", func(t *testing.T) {
		id := GenerateRequestID(timestamp, "GET", "/api/users")
		assert.Len(t, id, 12)
	})

	t.Run("same inputs produce different IDs", func(t *testing.T) {
		// The per-process counter disambiguates simultaneous identical
		// requests so capture files can never overwrite each other.
		id1 := GenerateRequestID(timestamp, "GET", "/api/users")
		id2 := GenerateRequestID(timestamp, "GET", "/api/users")
		assert.NotEqual(t, id1, id2)
	})

	t.Run("different inputs produce different IDs", func(t *testing.T) {
		id1 := GenerateRequestID(timestamp, "GET", "/api/users")
		id2 := GenerateRequestID(timestamp, "POST", "/api/users")
		id3 := GenerateRequestID(timestamp.Add(time.Second), "GET", "/api/users")
		assert.NotEqual(t, id1, id2)
		assert.NotEqual(t, id1, id3)
	})

	t.Run("ID is valid hex", func(t *testing.T) {
		id := GenerateRequestID(timestamp, "GET", "/api/users")
		for _, c := range id {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
				"expected hex character, got %c", c)
		}
	})
}

func TestRequestManager_Record_GeneratesID(t *testing.T) {
	m := NewRequestManager(10)

	record := RequestRecord{
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/api/users",
		Subdomain:  "api",
		StatusCode: 200,
	}

	m.Record(record)

	records := m.Recent(RequestFilter{})
	require.Len(t, records, 1)
	assert.Len(t, records[0].ID, 12, "expected ID to be generated")
}

func TestRequestManager_Record_PreservesExistingID(t *testing.T) {
	m := NewRequestManager(10)

	record := RequestRecord{
		ID:         "custom1",
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/api/users",
		Subdomain:  "api",
		StatusCode: 200,
	}

	m.Record(record)

	records := m.Recent(RequestFilter{})
	require.Len(t, records, 1)
	assert.Equal(t, "custom1", records[0].ID, "expected existing ID to be preserved")
}

// TestRequestManager_SubscribeAfterClose pins the shutdown latch: a subscribe
// that races past Close must get an already-closed channel (so an SSE handler
// returns immediately) and must not be registered — otherwise a late
// /proxy/requests/stream request pins the API server open through shutdown.
func TestRequestManager_SubscribeAfterClose(t *testing.T) {
	m := NewRequestManager(10)
	m.Close()

	sub := m.Subscribe(RequestFilter{})
	require.NotNil(t, sub)

	_, ok := <-sub.Ch
	assert.False(t, ok, "post-Close subscription channel must be closed")

	// The dead subscription must not receive records or be tracked.
	m.Record(RequestRecord{Method: "GET", URL: "/after-close"})

	// Unsubscribing it must be a safe no-op, and Close must stay idempotent.
	m.Unsubscribe(sub.ID)
	m.Close()
}

// TestRequestManager_FilterByProjectDir pins that a subscriber scoped to one
// project receives only that project's records, even when two projects share a
// hostname (differing only by owning port, which the daemon collapses to
// project identity).
func TestRequestManager_FilterByProjectDir(t *testing.T) {
	m := NewRequestManager(10)

	subA := m.Subscribe(RequestFilter{ProjectDir: "/projects/a"})
	require.NotNil(t, subA)

	// Record for project B (same hostname, different project) — must not reach A.
	m.Record(RequestRecord{Method: "GET", URL: "/b", Hostname: "api.local.dev", ProjectDir: "/projects/b"})
	// Record for project A — must reach A.
	m.Record(RequestRecord{Method: "GET", URL: "/a", Hostname: "api.local.dev", ProjectDir: "/projects/a"})

	select {
	case rec := <-subA.Ch:
		assert.Equal(t, "/projects/a", rec.ProjectDir)
		assert.Equal(t, "/a", rec.URL)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for project A record")
	}

	// No further records should be waiting for A (B's was filtered out).
	select {
	case rec := <-subA.Ch:
		t.Fatalf("subscriber A received an unexpected record: %+v", rec)
	default:
	}

	// Recent scoped to A returns only A's record.
	recA := m.Recent(RequestFilter{ProjectDir: "/projects/a"})
	require.Len(t, recA, 1)
	assert.Equal(t, "/a", recA[0].URL)
}

// TestRequestManager_PurgeByProject pins that purge is scoped by project: two
// projects sharing a hostname on different ports don't purge each other's
// records, and the eviction callback fires only for the purged project's
// records that carried captured Details.
func TestRequestManager_PurgeByProject(t *testing.T) {
	m := NewRequestManager(10)

	var evicted []string
	m.SetEvictionCallback(func(id string) {
		evicted = append(evicted, id)
	})

	details := &RequestDetails{RequestHeaders: map[string][]string{"X": {"y"}}}

	// A: one detailed record (evictable) and one metadata-only record.
	m.Record(RequestRecord{ID: "a1", Method: "GET", URL: "/a1", Hostname: "api.local.dev", ProjectDir: "/projects/a", Details: details})
	m.Record(RequestRecord{ID: "a2", Method: "GET", URL: "/a2", Hostname: "api.local.dev", ProjectDir: "/projects/a"})
	// B: one detailed record — must survive a purge of A.
	m.Record(RequestRecord{ID: "b1", Method: "GET", URL: "/b1", Hostname: "api.local.dev", ProjectDir: "/projects/b", Details: details})

	m.PurgeByProject("/projects/a")

	// Only B's record remains.
	remaining := m.Recent(RequestFilter{})
	require.Len(t, remaining, 1)
	assert.Equal(t, "b1", remaining[0].ID)

	// Eviction callback fired only for A's detailed record (a2 had no Details).
	assert.Equal(t, []string{"a1"}, evicted)

	// Purging an empty project dir is a no-op.
	m.PurgeByProject("")
	assert.Len(t, m.Recent(RequestFilter{}), 1)
}

// drainRecords empties a subscription channel without blocking.
func drainRecords(ch chan RequestRecord) []RequestRecord {
	var out []RequestRecord
	for {
		select {
		case r := <-ch:
			out = append(out, r)
		default:
			return out
		}
	}
}

func TestRequestManager_Upsert_StateMachine(t *testing.T) {
	inflight := func(id string) RequestRecord {
		return RequestRecord{ID: id, Method: "GET", URL: "/stream", StatusCode: 200, InFlight: true}
	}
	final := func(id string) RequestRecord {
		return RequestRecord{ID: id, Method: "GET", URL: "/stream", StatusCode: 200, Duration: 42 * time.Millisecond}
	}

	t.Run("absent inflight appends and notifies", func(t *testing.T) {
		m := NewRequestManager(10)
		sub := m.Subscribe(RequestFilter{})
		m.Upsert(inflight("r1"))
		assert.Equal(t, 1, m.Count())
		events := drainRecords(sub.Ch)
		require.Len(t, events, 1)
		assert.True(t, events[0].InFlight)
	})

	t.Run("absent final appends and notifies", func(t *testing.T) {
		m := NewRequestManager(10)
		sub := m.Subscribe(RequestFilter{})
		m.Upsert(final("r1"))
		assert.Equal(t, 1, m.Count())
		assert.Len(t, drainRecords(sub.Ch), 1)
	})

	t.Run("inflight over inflight is a silent no-op", func(t *testing.T) {
		m := NewRequestManager(10)
		first := inflight("r1")
		first.RemoteAddr = "10.0.0.1"
		m.Upsert(first)
		sub := m.Subscribe(RequestFilter{})

		dup := inflight("r1")
		dup.RemoteAddr = "10.0.0.2" // must NOT be written
		m.Upsert(dup)

		assert.Equal(t, 1, m.Count())
		got, ok := m.GetByID("r1")
		require.True(t, ok)
		assert.Equal(t, "10.0.0.1", got.RemoteAddr)
		assert.Empty(t, drainRecords(sub.Ch))
	})

	t.Run("inflight to final replaces in place and notifies", func(t *testing.T) {
		m := NewRequestManager(10)
		m.Upsert(inflight("r1"))
		sub := m.Subscribe(RequestFilter{})

		done := final("r1")
		done.Details = &RequestDetails{RequestHeaders: map[string][]string{"X": {"y"}}}
		m.Upsert(done)

		assert.Equal(t, 1, m.Count())
		got, ok := m.GetByID("r1")
		require.True(t, ok)
		assert.False(t, got.InFlight)
		assert.Equal(t, 42*time.Millisecond, got.Duration)
		require.NotNil(t, got.Details)

		events := drainRecords(sub.Ch)
		require.Len(t, events, 1)
		assert.False(t, events[0].InFlight)
	})

	t.Run("final is terminal against inflight and final", func(t *testing.T) {
		m := NewRequestManager(10)
		m.Upsert(final("r1"))
		sub := m.Subscribe(RequestFilter{})

		m.Upsert(inflight("r1")) // stale buffered start event
		dupFinal := final("r1")
		dupFinal.StatusCode = 599 // must NOT be written
		m.Upsert(dupFinal)

		got, ok := m.GetByID("r1")
		require.True(t, ok)
		assert.False(t, got.InFlight)
		assert.Equal(t, 200, got.StatusCode)
		assert.Equal(t, 1, m.Count())
		assert.Empty(t, drainRecords(sub.Ch))
	})

	t.Run("empty ID generates and appends", func(t *testing.T) {
		m := NewRequestManager(10)
		rec := final("")
		rec.Timestamp = time.Now()
		m.Upsert(rec)
		assert.Equal(t, 1, m.Count())
		assert.NotEmpty(t, m.Recent(RequestFilter{})[0].ID)
	})
}

func TestRequestManager_Upsert_PreservesRingPosition(t *testing.T) {
	m := NewRequestManager(10)

	m.Upsert(RequestRecord{ID: "old", Method: "GET", URL: "/old"})
	m.Upsert(RequestRecord{ID: "stream", Method: "GET", URL: "/stream", InFlight: true})
	m.Upsert(RequestRecord{ID: "new", Method: "GET", URL: "/new"})

	// Completing "stream" must not move it to the newest position.
	m.Upsert(RequestRecord{ID: "stream", Method: "GET", URL: "/stream", Duration: time.Second})

	recent := m.Recent(RequestFilter{})
	require.Len(t, recent, 3)
	assert.Equal(t, "new", recent[0].ID)
	assert.Equal(t, "stream", recent[1].ID)
	assert.Equal(t, "old", recent[2].ID)
	assert.False(t, recent[1].InFlight)
	assert.Equal(t, time.Second, recent[1].Duration)
}

func TestRequestManager_Upsert_AppendEvictsLikeRecord(t *testing.T) {
	m := NewRequestManager(2)
	var evicted []string
	m.SetEvictionCallback(func(id string) { evicted = append(evicted, id) })

	details := &RequestDetails{RequestHeaders: map[string][]string{"X": {"y"}}}
	m.Record(RequestRecord{ID: "a", Details: details})
	m.Record(RequestRecord{ID: "b"})

	// Ring full: appending via Upsert must evict "a" (Details-carrying).
	m.Upsert(RequestRecord{ID: "c"})
	assert.Equal(t, []string{"a"}, evicted)

	// Updating in place must NOT evict anything.
	m.Upsert(RequestRecord{ID: "b", InFlight: true}) // no-op: b is final
	m.Upsert(RequestRecord{ID: "c", InFlight: true}) // no-op: c is final
	assert.Equal(t, []string{"a"}, evicted)
	assert.Equal(t, 2, m.Count())
}

func TestRequestManager_Upsert_StuckInFlightWithoutCompletion(t *testing.T) {
	// Documented outcome (plan 006 §9): if a completion event is lost and no
	// later snapshot carries the final record, the row stays in-flight until
	// evicted. Pinned so the semantics are deliberate, not accidental.
	m := NewRequestManager(10)
	m.Upsert(RequestRecord{ID: "lost", InFlight: true})
	for i := 0; i < 5; i++ {
		m.Upsert(RequestRecord{ID: GenerateRequestID(time.Now(), "GET", "/x"), StatusCode: 200})
	}
	got, ok := m.GetByID("lost")
	require.True(t, ok)
	assert.True(t, got.InFlight)
}

func TestRequestManager_Upsert_PurgeAfterUpdateCompacts(t *testing.T) {
	m := NewRequestManager(10)
	m.Upsert(RequestRecord{ID: "p1", ProjectDir: "/p", InFlight: true})
	m.Upsert(RequestRecord{ID: "q1", ProjectDir: "/q", InFlight: true})
	m.Upsert(RequestRecord{ID: "p1", ProjectDir: "/p", StatusCode: 200})
	m.Upsert(RequestRecord{ID: "q1", ProjectDir: "/q", StatusCode: 200})

	m.PurgeByProject("/p")

	remaining := m.Recent(RequestFilter{})
	require.Len(t, remaining, 1)
	assert.Equal(t, "q1", remaining[0].ID)

	// The ring stays consistent for further writes after compaction.
	m.Upsert(RequestRecord{ID: "q2", ProjectDir: "/q", StatusCode: 200})
	assert.Equal(t, 2, m.Count())
}

func TestRequestManager_Upsert_ConcurrentTransitions(t *testing.T) {
	// Race in-flight and final Upserts for the same IDs (with concurrent
	// readers and purges) and assert: storage always converges to final, and
	// no subscriber ever observes in-flight AFTER final for the same ID —
	// the under-lock notify guarantee.
	m := NewRequestManager(100)
	sub := m.Subscribe(RequestFilter{})

	seenFinal := make(map[string]bool)
	violation := false
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for rec := range sub.Ch {
			if rec.InFlight && seenFinal[rec.ID] {
				violation = true
			}
			if !rec.InFlight {
				seenFinal[rec.ID] = true
			}
		}
	}()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Upsert(RequestRecord{ID: id, InFlight: true, StatusCode: 200})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Upsert(RequestRecord{ID: id, StatusCode: 200, Duration: time.Millisecond})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			m.Recent(RequestFilter{})
			m.PurgeByProject("/nonexistent")
		}
	}()
	wg.Wait()
	m.Close()
	<-consumed

	assert.False(t, violation, "subscriber observed in-flight after final for the same ID")
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-%d", i)
		got, ok := m.GetByID(id)
		require.True(t, ok, id)
		assert.False(t, got.InFlight, id)
	}
}

// TestRequestRecord_StaleAt is the truth table for D8 (#53) staleness: stale
// iff InFlight && now.Sub(Timestamp) > constants.InFlightStaleAfter. Covers a
// fresh in-flight row, the exact boundary (exclusive: equal to the threshold
// is NOT stale), just past the boundary, well past it, and final records
// (never stale regardless of age).
func TestRequestRecord_StaleAt(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		record RequestRecord
		want   bool
	}{
		{
			name:   "in-flight, fresh",
			record: RequestRecord{InFlight: true, Timestamp: now.Add(-1 * time.Minute)},
			want:   false,
		},
		{
			name:   "in-flight, exactly at the threshold is not yet stale",
			record: RequestRecord{InFlight: true, Timestamp: now.Add(-constants.InFlightStaleAfter)},
			want:   false,
		},
		{
			name:   "in-flight, one nanosecond past the threshold is stale",
			record: RequestRecord{InFlight: true, Timestamp: now.Add(-constants.InFlightStaleAfter - time.Nanosecond)},
			want:   true,
		},
		{
			name:   "in-flight, well past the threshold",
			record: RequestRecord{InFlight: true, Timestamp: now.Add(-1 * time.Hour)},
			want:   true,
		},
		{
			name:   "final record with an old timestamp is never stale",
			record: RequestRecord{InFlight: false, Timestamp: now.Add(-1 * time.Hour)},
			want:   false,
		},
		{
			name:   "final record with a fresh timestamp is never stale",
			record: RequestRecord{InFlight: false, Timestamp: now},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.record.StaleAt(now))
		})
	}
}

func TestRequestRecord_InFlightOmittedWhenFinal(t *testing.T) {
	data, err := json.Marshal(RequestRecord{ID: "r1", StatusCode: 200})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "in_flight")

	data, err = json.Marshal(RequestRecord{ID: "r1", StatusCode: 200, InFlight: true})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"in_flight":true`)
}

// --- D12 (#50): RecentPage ring-position cursor pagination ---

func TestRequestManager_RecentPage_AnchorMidRing(t *testing.T) {
	m := NewRequestManager(10)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		m.Record(RequestRecord{ID: id, Method: "GET", URL: "/x"})
	}
	// Ring newest->oldest: r5, r4, r3, r2, r1.

	page, next, found := m.RecentPage(RequestFilter{BeforeID: "r3", Limit: 10})
	require.True(t, found)
	require.Len(t, page, 2)
	assert.Equal(t, "r2", page[0].ID)
	assert.Equal(t, "r1", page[1].ID)
	assert.Empty(t, next, "scan reached the ring's oldest record")
}

func TestRequestManager_RecentPage_AnchorEvicted(t *testing.T) {
	m := NewRequestManager(3)
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		m.Record(RequestRecord{ID: id})
	}
	// Capacity 3: r1 has been evicted; ring holds r2, r3, r4.
	_, ok := m.GetByID("r1")
	require.False(t, ok, "precondition: r1 must be evicted")

	page, next, found := m.RecentPage(RequestFilter{BeforeID: "r1"})
	assert.False(t, found)
	assert.Empty(t, page)
	assert.Empty(t, next)

	// A never-existed ID reports the same anchorFound=false shape.
	page2, next2, found2 := m.RecentPage(RequestFilter{BeforeID: "does-not-exist"})
	assert.False(t, found2)
	assert.Empty(t, page2)
	assert.Empty(t, next2)
}

func TestRequestManager_RecentPage_FilterWithCursor(t *testing.T) {
	m := NewRequestManager(10)
	m.Record(RequestRecord{ID: "a1", Method: "GET"})
	m.Record(RequestRecord{ID: "b1", Method: "POST"})
	m.Record(RequestRecord{ID: "a2", Method: "GET"})
	m.Record(RequestRecord{ID: "b2", Method: "POST"})
	m.Record(RequestRecord{ID: "a3", Method: "GET"})
	// Ring newest->oldest: a3, b2, a2, b1, a1.

	page, next, found := m.RecentPage(RequestFilter{Method: "GET", BeforeID: "a3", Limit: 10})
	require.True(t, found)
	require.Len(t, page, 2)
	assert.Equal(t, "a2", page[0].ID)
	assert.Equal(t, "a1", page[1].ID)
	assert.Empty(t, next, "scan reached the ring's oldest record")
}

func TestRequestManager_RecentPage_FullyFilteredPageAdvancesCursor(t *testing.T) {
	m := NewRequestManager(10)
	for i := 0; i < 5; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("old%d", i), Method: "POST"})
	}
	m.Record(RequestRecord{ID: "anchor", Method: "GET"})
	// Ring newest->oldest: anchor, old4, old3, old2, old1, old0.

	page, next, found := m.RecentPage(RequestFilter{Method: "GET", BeforeID: "anchor", Limit: 10})
	require.True(t, found)
	// None of the POST records match the GET filter: a page that returned
	// nothing must still report that the scan reached the ring's oldest
	// record (next empty) instead of stalling on an unadvanced cursor.
	assert.Empty(t, page)
	assert.Empty(t, next)
}

func TestRequestManager_RecentPage_CrossProjectScope(t *testing.T) {
	m := NewRequestManager(10)
	m.Record(RequestRecord{ID: "p1", ProjectDir: "/projects/a"})
	m.Record(RequestRecord{ID: "p2", ProjectDir: "/projects/b"})

	// p1 exists in the ring but belongs to a different project than the
	// filter's scope. It must report the same anchorFound=false shape as an
	// unknown ID so an out-of-scope anchor can't leak its existence (D12).
	page, next, found := m.RecentPage(RequestFilter{ProjectDir: "/projects/b", BeforeID: "p1"})
	assert.False(t, found)
	assert.Empty(t, page)
	assert.Empty(t, next)
}

// TestRequestManager_RecentPage_ReappendOrdering pins the central D12
// invariant: RecentPage orders and pages by RING POSITION (arrival order),
// not by Timestamp. An in-flight record whose original slot is evicted
// while its response is still in flight re-enters the ring at the newest
// position once its completion arrives (Upsert's absent-ID append path) —
// even though it carries the oldest Timestamp of any surviving record.
func TestRequestManager_RecentPage_ReappendOrdering(t *testing.T) {
	m := NewRequestManager(3)
	base := time.Now()

	// s1 starts in-flight: earliest arrival, earliest timestamp.
	m.Upsert(RequestRecord{ID: "s1", Method: "GET", URL: "/stream", InFlight: true, Timestamp: base})
	m.Upsert(RequestRecord{ID: "b", Method: "GET", URL: "/b", Timestamp: base.Add(1 * time.Second)})
	m.Upsert(RequestRecord{ID: "c", Method: "GET", URL: "/c", Timestamp: base.Add(2 * time.Second)})
	// "d" wraps the capacity-3 ring, evicting s1's original slot.
	m.Upsert(RequestRecord{ID: "d", Method: "GET", URL: "/d", Timestamp: base.Add(3 * time.Second)})

	_, ok := m.GetByID("s1")
	require.False(t, ok, "precondition: s1's original slot must be evicted")

	// s1's completion arrives after eviction. Its ID is no longer in the
	// ring, so Upsert treats it as a brand-new arrival at the newest
	// position (evicting "b"), despite carrying the oldest Timestamp here.
	m.Upsert(RequestRecord{ID: "s1", Method: "GET", URL: "/stream", Timestamp: base, Duration: time.Second})

	page, next, found := m.RecentPage(RequestFilter{Limit: 10})
	require.True(t, found)
	require.Len(t, page, 3)
	got := []string{page[0].ID, page[1].ID, page[2].ID}
	assert.Equal(t, []string{"s1", "d", "c"}, got, "ring (arrival) order, not time order")
	assert.Empty(t, next)

	// s1's Timestamp is the OLDEST of the three surviving records even
	// though it paginates as the newest — the divergence this test pins.
	assert.True(t, page[0].Timestamp.Before(page[1].Timestamp))
	assert.True(t, page[0].Timestamp.Before(page[2].Timestamp))

	// Paginating with before_id=s1 must return strictly-older-by-POSITION
	// records (d, c) — not records reordered by Timestamp.
	older, next2, found2 := m.RecentPage(RequestFilter{BeforeID: "s1", Limit: 10})
	require.True(t, found2)
	require.Len(t, older, 2)
	assert.Equal(t, "d", older[0].ID)
	assert.Equal(t, "c", older[1].ID)
	assert.Empty(t, next2)
}

func TestRequestManager_RecentPage_PaginationCarriesAndOmitsCursor(t *testing.T) {
	m := NewRequestManager(10)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		m.Record(RequestRecord{ID: id})
	}
	// Ring newest->oldest: r5, r4, r3, r2, r1.

	// Plain first page (no before_id): next_before_id must still be
	// populated when older records remain, so a poller can start cursoring
	// from an unqualified first call.
	page1, next1, found1 := m.RecentPage(RequestFilter{Limit: 2})
	require.True(t, found1)
	require.Len(t, page1, 2)
	assert.Equal(t, []string{"r5", "r4"}, []string{page1[0].ID, page1[1].ID})
	require.Equal(t, "r4", next1, "next_before_id is the oldest SCANNED record")

	page2, next2, found2 := m.RecentPage(RequestFilter{Limit: 2, BeforeID: next1})
	require.True(t, found2)
	require.Len(t, page2, 2)
	assert.Equal(t, []string{"r3", "r2"}, []string{page2[0].ID, page2[1].ID})
	require.Equal(t, "r2", next2)

	page3, next3, found3 := m.RecentPage(RequestFilter{Limit: 2, BeforeID: next2})
	require.True(t, found3)
	require.Len(t, page3, 1)
	assert.Equal(t, "r1", page3[0].ID)
	// Last page: the scan reached the ring's oldest record, so the cursor is
	// omitted (no more pages) instead of pointing back at r1.
	assert.Empty(t, next3)
}
