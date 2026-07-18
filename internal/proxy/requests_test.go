package proxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("generates 7 character ID", func(t *testing.T) {
		id := GenerateRequestID(timestamp, "GET", "/api/users")
		assert.Len(t, id, 7)
	})

	t.Run("same inputs produce same ID", func(t *testing.T) {
		id1 := GenerateRequestID(timestamp, "GET", "/api/users")
		id2 := GenerateRequestID(timestamp, "GET", "/api/users")
		assert.Equal(t, id1, id2)
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
	assert.Len(t, records[0].ID, 7, "expected ID to be generated")
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
