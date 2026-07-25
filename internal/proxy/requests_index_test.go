package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkIndexInvariant asserts the #71 idIndex invariant after a mutation:
// every live ring slot's ID maps to the NEWEST slot holding that ID, and the
// map holds exactly the distinct live IDs with no entry pointing outside the
// live slots. It takes the ring read lock, so it must not be called while the
// caller holds m.mu.
func (m *RequestManager) checkIndexInvariant(t *testing.T) {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()

	live := make(map[int]bool, m.count)
	// expected[id] = newest live slot for that ID (first hit scanning newest→oldest).
	expected := make(map[string]int, m.count)
	for i := 0; i < m.count; i++ {
		idx := (m.head - 1 - i + m.capacity) % m.capacity
		live[idx] = true
		id := m.buffer[idx].ID
		if id == "" {
			t.Fatalf("live slot %d has empty ID", idx)
		}
		if _, seen := expected[id]; !seen {
			expected[id] = idx
		}
	}

	if len(m.idIndex) != len(expected) {
		t.Fatalf("idIndex size %d != distinct live IDs %d", len(m.idIndex), len(expected))
	}
	for id, idx := range m.idIndex {
		if !live[idx] {
			t.Fatalf("idIndex[%q]=%d points outside the live slots", id, idx)
		}
		if got := m.buffer[idx].ID; got != id {
			t.Fatalf("idIndex[%q]=%d but that slot holds ID %q", id, idx, got)
		}
		if want := expected[id]; want != idx {
			t.Fatalf("idIndex[%q]=%d but the newest copy is at slot %d", id, idx, want)
		}
	}
}

func TestRequestManager_Index_RecordAppends(t *testing.T) {
	m := NewRequestManager(10)
	m.checkIndexInvariant(t) // empty ring
	for i := 0; i < 5; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("r%d", i)})
		m.checkIndexInvariant(t)
	}
	got, ok := m.GetByID("r3")
	require.True(t, ok)
	assert.Equal(t, "r3", got.ID)
}

func TestRequestManager_Index_UpsertReplaceInPlace(t *testing.T) {
	m := NewRequestManager(10)
	m.Upsert(RequestRecord{ID: "a", InFlight: true})
	m.Upsert(RequestRecord{ID: "b", InFlight: true})
	m.checkIndexInvariant(t)

	// Completing "a" replaces in place: same slot, same index entry.
	m.Upsert(RequestRecord{ID: "a", Duration: time.Second})
	m.checkIndexInvariant(t)

	got, ok := m.GetByID("a")
	require.True(t, ok)
	assert.False(t, got.InFlight)
	assert.Equal(t, time.Second, got.Duration)
}

func TestRequestManager_Index_OverwriteWraparound(t *testing.T) {
	m := NewRequestManager(3)
	for i := 0; i < 7; i++ { // three full wraparounds' worth of evictions
		m.Record(RequestRecord{ID: fmt.Sprintf("r%d", i)})
		m.checkIndexInvariant(t)
	}
	assert.Equal(t, 3, m.Count())
	// Only the three newest survive; evicted IDs are gone from the index.
	for _, gone := range []string{"r0", "r1", "r2", "r3"} {
		_, ok := m.GetByID(gone)
		assert.False(t, ok, gone)
	}
	for _, live := range []string{"r4", "r5", "r6"} {
		_, ok := m.GetByID(live)
		assert.True(t, ok, live)
	}
}

func TestRequestManager_Index_DetailsLessOverwrite(t *testing.T) {
	// Overwrite Details-less records at capacity: index maintenance must run
	// even though the eviction callback (Details-gated) never fires (#71).
	m := NewRequestManager(2)
	var evicted []string
	m.SetEvictionCallback(func(id string) { evicted = append(evicted, id) })

	m.Record(RequestRecord{ID: "a"}) // no Details
	m.Record(RequestRecord{ID: "b"}) // no Details
	m.Record(RequestRecord{ID: "c"}) // evicts a; no callback (a had no Details)
	m.checkIndexInvariant(t)

	assert.Empty(t, evicted, "no Details means no eviction callback")
	_, ok := m.GetByID("a")
	assert.False(t, ok, "a's index entry must still be dropped on overwrite")
}

func TestRequestManager_Index_DuplicateIDOverwriteAtWraparound(t *testing.T) {
	// Record permits duplicate explicit IDs. Overwriting an OLDER copy while a
	// NEWER copy is still live must NOT erase the newer copy's mapping (#71):
	// GetByID must keep returning the newest.
	m := NewRequestManager(3)

	m.Record(RequestRecord{ID: "dup", RemoteAddr: "old"}) // slot 0
	m.Record(RequestRecord{ID: "b"})                      // slot 1
	m.Record(RequestRecord{ID: "dup", RemoteAddr: "new"}) // slot 2 (newer copy)
	m.checkIndexInvariant(t)

	// Ring full. "c" overwrites slot 0 — the OLDER "dup". The newer copy at
	// slot 2 must survive with its mapping intact.
	m.Record(RequestRecord{ID: "c"})
	m.checkIndexInvariant(t)

	got, ok := m.GetByID("dup")
	require.True(t, ok, "newer duplicate must survive the older copy's overwrite")
	assert.Equal(t, "new", got.RemoteAddr)

	// Now evict "b", then evict the newer "dup" itself: mapping must clear.
	m.Record(RequestRecord{ID: "e"}) // evicts b
	m.checkIndexInvariant(t)
	m.Record(RequestRecord{ID: "f"}) // evicts the newer dup
	m.checkIndexInvariant(t)
	_, ok = m.GetByID("dup")
	assert.False(t, ok, "both duplicates evicted: index entry must be gone")
}

func TestRequestManager_Index_UpsertReappendAtWraparound(t *testing.T) {
	// Mirror of the D12 reappend scenario, checking the index (#71): an
	// in-flight record whose slot is evicted re-enters as a fresh append.
	m := NewRequestManager(3)
	base := time.Now()
	m.Upsert(RequestRecord{ID: "s1", InFlight: true, Timestamp: base})
	m.Upsert(RequestRecord{ID: "b", Timestamp: base.Add(time.Second)})
	m.Upsert(RequestRecord{ID: "c", Timestamp: base.Add(2 * time.Second)})
	m.Upsert(RequestRecord{ID: "d", Timestamp: base.Add(3 * time.Second)}) // evicts s1's slot
	m.checkIndexInvariant(t)
	_, ok := m.GetByID("s1")
	require.False(t, ok, "precondition: s1's original slot evicted")

	// s1's completion arrives: absent from the ring, so it appends anew.
	m.Upsert(RequestRecord{ID: "s1", Timestamp: base, Duration: time.Second})
	m.checkIndexInvariant(t)
	got, ok := m.GetByID("s1")
	require.True(t, ok)
	assert.False(t, got.InFlight)
}

func TestRequestManager_Index_PurgeCompaction(t *testing.T) {
	m := NewRequestManager(10)
	details := &RequestDetails{RequestHeaders: map[string][]string{"X": {"y"}}}
	m.Record(RequestRecord{ID: "a1", ProjectDir: "/a", Details: details})
	m.Record(RequestRecord{ID: "b1", ProjectDir: "/b"})
	m.Record(RequestRecord{ID: "a2", ProjectDir: "/a"})
	m.Record(RequestRecord{ID: "b2", ProjectDir: "/b", Details: details})
	m.checkIndexInvariant(t)

	m.PurgeByProject("/a")
	m.checkIndexInvariant(t)

	for _, gone := range []string{"a1", "a2"} {
		_, ok := m.GetByID(gone)
		assert.False(t, ok, gone)
	}
	for _, live := range []string{"b1", "b2"} {
		_, ok := m.GetByID(live)
		assert.True(t, ok, live)
	}

	// The ring (and its index) stay consistent for further writes.
	m.Record(RequestRecord{ID: "b3", ProjectDir: "/b"})
	m.checkIndexInvariant(t)
	assert.Equal(t, 3, m.Count())
}

func TestRequestManager_Index_AfterClose(t *testing.T) {
	m := NewRequestManager(5)
	for i := 0; i < 3; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("r%d", i)})
	}
	m.checkIndexInvariant(t)
	m.Close()
	// Close leaves the ring (and index) intact; only writes/subscriptions latch.
	m.checkIndexInvariant(t)
	got, ok := m.GetByID("r1")
	require.True(t, ok)
	assert.Equal(t, "r1", got.ID)
}

// TestRequestManager_Index_ConcurrentStorm races Record/Upsert/GetByID/RecentPage
// so -race exercises concurrent access to the index, then asserts the invariant
// holds once the storm quiesces (#71).
func TestRequestManager_Index_ConcurrentStorm(t *testing.T) {
	m := NewRequestManager(64)
	const workers = 16
	const iters = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("w%d-%d", w, i%40)
				switch i % 4 {
				case 0:
					m.Record(RequestRecord{ID: id, ProjectDir: "/p"})
				case 1:
					m.Upsert(RequestRecord{ID: id, InFlight: true, ProjectDir: "/p"})
				case 2:
					m.Upsert(RequestRecord{ID: id, ProjectDir: "/p", StatusCode: 200})
				case 3:
					m.GetByID(id)
					m.RecentPage(RequestFilter{Limit: 10, BeforeID: id})
				}
			}
		}(w)
	}
	wg.Wait()

	m.checkIndexInvariant(t)
}
