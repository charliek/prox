package logs

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_Write(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10})
	defer m.Close()

	m.Write(makeEntry("hello"))
	m.Write(makeEntry("world"))

	stats := m.Stats()
	assert.Equal(t, 2, stats.TotalEntries)
}

func TestManager_Query(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 100})
	defer m.Close()

	for i := 0; i < 50; i++ {
		m.Write(makeEntryWithProcess("web", "line"))
	}
	for i := 0; i < 30; i++ {
		m.Write(makeEntryWithProcess("api", "line"))
	}

	t.Run("query all", func(t *testing.T) {
		entries, total, err := m.Query(domain.LogFilter{}, 0)
		require.NoError(t, err)
		assert.Len(t, entries, 80)
		assert.Equal(t, 80, total)
	})

	t.Run("query with limit", func(t *testing.T) {
		entries, total, err := m.Query(domain.LogFilter{}, 10)
		require.NoError(t, err)
		assert.Len(t, entries, 10)
		assert.Equal(t, 80, total)
	})

	t.Run("query with filter", func(t *testing.T) {
		entries, total, err := m.Query(domain.LogFilter{Processes: []string{"web"}}, 0)
		require.NoError(t, err)
		assert.Len(t, entries, 50)
		assert.Equal(t, 50, total)
	})
}

func TestManager_QueryLast(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 100})
	defer m.Close()

	for i := 0; i < 20; i++ {
		m.Write(makeEntryWithProcess("web", string(rune('A'+i))))
	}

	entries, total, err := m.QueryLast(domain.LogFilter{}, 5)
	require.NoError(t, err)
	assert.Len(t, entries, 5)
	assert.Equal(t, 20, total)

	// Should be last 5 letters
	assert.Equal(t, "P", entries[0].Line) // 16th letter (0-indexed 15)
	assert.Equal(t, "T", entries[4].Line) // 20th letter (0-indexed 19)
}

func TestManager_Subscribe(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10, SubscriptionBuffer: 10})
	defer m.Close()

	id, ch, err := m.Subscribe(domain.LogFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Write after subscribe
	m.Write(makeEntry("after subscribe"))

	select {
	case msg := <-ch:
		assert.Equal(t, "after subscribe", msg.Line)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected to receive message")
	}
}

func TestManager_SubscribeWithFilter(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10, SubscriptionBuffer: 10})
	defer m.Close()

	_, ch, _ := m.Subscribe(domain.LogFilter{Processes: []string{"web"}})

	m.Write(makeEntryWithProcess("api", "api message"))
	m.Write(makeEntryWithProcess("web", "web message"))

	select {
	case msg := <-ch:
		assert.Equal(t, "web", msg.Process)
		assert.Equal(t, "web message", msg.Line)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected to receive web message")
	}

	// Should not receive api message
	select {
	case <-ch:
		t.Fatal("should not receive api message")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestManager_Unsubscribe(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10, SubscriptionBuffer: 10})
	defer m.Close()

	id, ch, _ := m.Subscribe(domain.LogFilter{})
	m.Unsubscribe(id)

	// Write after unsubscribe
	m.Write(makeEntry("after unsubscribe"))

	// Channel should be closed
	_, ok := <-ch
	assert.False(t, ok)
}

func TestManager_Stats(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 100, SubscriptionBuffer: 10})
	defer m.Close()

	for i := 0; i < 10; i++ {
		m.Write(makeEntry("line"))
	}

	m.Subscribe(domain.LogFilter{})
	m.Subscribe(domain.LogFilter{})

	stats := m.Stats()
	assert.Equal(t, 10, stats.TotalEntries)
	assert.Equal(t, 100, stats.BufferSize)
	assert.Equal(t, 2, stats.Subscribers)
}

func TestManager_Concurrent(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 1000, SubscriptionBuffer: 100})
	defer m.Close()

	var wg sync.WaitGroup

	// Multiple writers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Write(makeEntry("concurrent write"))
			}
		}()
	}

	// Multiple readers
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.Query(domain.LogFilter{}, 10)
				m.QueryLast(domain.LogFilter{}, 10)
			}
		}()
	}

	// Subscribe/unsubscribe
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				id, _, _ := m.Subscribe(domain.LogFilter{})
				m.Unsubscribe(id)
			}
		}()
	}

	wg.Wait()

	stats := m.Stats()
	assert.Equal(t, 500, stats.TotalEntries) // 5 writers * 100 writes
}

func TestManager_WriteAssignsSeq(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10})
	defer m.Close()

	// An entry that never passed through Write carries the unset sentinel.
	assert.Zero(t, makeEntry("unwritten").Seq)

	// Write stamps monotonically from 1, overwriting whatever the caller set.
	m.Write(makeEntry("first"))
	m.Write(makeEntry("second"))
	preset := makeEntry("third")
	preset.Seq = 999
	m.Write(preset)

	entries, _, err := m.Query(domain.LogFilter{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, uint64(1), entries[0].Seq)
	assert.Equal(t, uint64(2), entries[1].Seq)
	assert.Equal(t, uint64(3), entries[2].Seq)

	// The caller's copy is untouched by the stamp.
	assert.Equal(t, uint64(999), preset.Seq)
}

func TestManager_StreamID(t *testing.T) {
	m := NewManager(ManagerConfig{BufferSize: 10})
	defer m.Close()

	id := m.StreamID()
	assert.Len(t, id, 2*streamIDBytes, "hex encoding of the random token")

	// Stable for the manager's lifetime, including across writes.
	m.Write(makeEntry("line"))
	assert.Equal(t, id, m.StreamID())

	// A second manager is a different epoch.
	other := NewManager(ManagerConfig{BufferSize: 10})
	defer other.Close()
	assert.NotEqual(t, id, other.StreamID())
}

// TestManager_ConcurrentWriteSequencing is the reason Write holds ingestMu
// end-to-end: with concurrent writers, seq assignment, the ring append and the
// broadcast must not interleave, so ring order == seq order == every
// subscriber's delivery order, with no gaps.
func TestManager_ConcurrentWriteSequencing(t *testing.T) {
	const (
		writers          = 8
		writesPerWriter  = 100
		total            = writers * writesPerWriter
		subscriberCount  = 3
		subscriptionSize = total + 10 // never overflow: overflow ends the sub (C6)
	)

	m := NewManager(ManagerConfig{BufferSize: total, SubscriptionBuffer: subscriptionSize})

	channels := make([]<-chan domain.LogEntry, 0, subscriberCount)
	for i := 0; i < subscriberCount; i++ {
		_, ch, err := m.Subscribe(domain.LogFilter{})
		require.NoError(t, err)
		channels = append(channels, ch)
	}

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				m.Write(makeEntry("concurrent"))
			}
		}()
	}
	wg.Wait()

	// Close so the subscription channels terminate; buffered entries are still
	// drainable from a closed channel.
	m.Close()

	// Ring order is strictly seq-ascending and gap-free over 1..total.
	entries, _, err := m.Query(domain.LogFilter{}, 0)
	require.NoError(t, err)
	require.Len(t, entries, total)
	for i, e := range entries {
		require.Equal(t, uint64(i+1), e.Seq, "ring entry %d", i)
	}

	// Every subscriber saw the same strictly ascending, gap-free sequence.
	for i, ch := range channels {
		var seqs []uint64
		for e := range ch {
			seqs = append(seqs, e.Seq)
		}
		require.Len(t, seqs, total, "subscriber %d delivery count", i)
		for j, seq := range seqs {
			require.Equal(t, uint64(j+1), seq, "subscriber %d delivery %d", i, j)
		}
	}
}

func TestManager_QueryFromSeq(t *testing.T) {
	t.Run("empty buffer reports zero bounds", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 10})
		defer m.Close()

		entries, oldest, latest, err := m.QueryFromSeq(domain.LogFilter{}, 0, 0)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Equal(t, uint64(0), oldest)
		assert.Equal(t, uint64(0), latest)
	})

	t.Run("resume from mid-buffer", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 100})
		defer m.Close()
		for i := 0; i < 10; i++ {
			m.Write(makeEntry("line"))
		}

		entries, oldest, latest, err := m.QueryFromSeq(domain.LogFilter{}, 6, 0)
		require.NoError(t, err)
		require.Len(t, entries, 4)
		assert.Equal(t, uint64(7), entries[0].Seq)
		assert.Equal(t, uint64(10), entries[3].Seq)
		assert.Equal(t, uint64(1), oldest)
		assert.Equal(t, uint64(10), latest)
	})

	t.Run("sinceSeq zero returns everything buffered", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 100})
		defer m.Close()
		for i := 0; i < 5; i++ {
			m.Write(makeEntry("line"))
		}

		entries, oldest, latest, err := m.QueryFromSeq(domain.LogFilter{}, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 5)
		assert.Equal(t, uint64(1), entries[0].Seq)
		assert.Equal(t, uint64(1), oldest)
		assert.Equal(t, uint64(5), latest)
	})

	t.Run("sinceSeq at or beyond latest returns nothing", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 100})
		defer m.Close()
		for i := 0; i < 5; i++ {
			m.Write(makeEntry("line"))
		}

		entries, _, latest, err := m.QueryFromSeq(domain.LogFilter{}, 5, 0)
		require.NoError(t, err)
		assert.Empty(t, entries, "caught up")
		assert.Equal(t, uint64(5), latest, "and latest == sinceSeq proves it")

		entries, _, latest, err = m.QueryFromSeq(domain.LogFilter{}, 99, 0)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Equal(t, uint64(5), latest)
	})

	t.Run("rolled buffer is detectable via oldestSeq", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 10})
		defer m.Close()
		for i := 0; i < 50; i++ {
			m.Write(makeEntry("line"))
		}

		// The client last saw seq 5, but only 41..50 survive: seqs 6..40 are gone.
		entries, oldest, latest, err := m.QueryFromSeq(domain.LogFilter{}, 5, 0)
		require.NoError(t, err)
		require.Len(t, entries, 10)
		assert.Equal(t, uint64(41), entries[0].Seq)
		assert.Equal(t, uint64(41), oldest)
		assert.Equal(t, uint64(50), latest)
		assert.Greater(t, oldest, uint64(5)+1, "gap is detectable: oldestSeq > sinceSeq+1")
	})

	t.Run("limit keeps the oldest matching entries", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 100})
		defer m.Close()
		for i := 0; i < 20; i++ {
			m.Write(makeEntry("line"))
		}

		entries, _, latest, err := m.QueryFromSeq(domain.LogFilter{}, 4, 3)
		require.NoError(t, err)
		require.Len(t, entries, 3)
		assert.Equal(t, uint64(5), entries[0].Seq, "contiguous with sinceSeq, not the newest slice")
		assert.Equal(t, uint64(7), entries[2].Seq)
		assert.Equal(t, uint64(20), latest)
	})

	t.Run("filter and sinceSeq combine", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 100})
		defer m.Close()
		for i := 0; i < 10; i++ {
			m.Write(makeEntryWithProcess("web", "web line"))
			m.Write(makeEntryWithProcess("api", "api line"))
		}
		// Interleaved: web has odd seqs 1,3,...,19; api has even seqs.

		entries, oldest, latest, err := m.QueryFromSeq(domain.LogFilter{Processes: []string{"web"}}, 10, 0)
		require.NoError(t, err)
		require.Len(t, entries, 5)
		for _, e := range entries {
			assert.Equal(t, "web", e.Process)
			assert.Greater(t, e.Seq, uint64(10))
		}
		assert.Equal(t, uint64(11), entries[0].Seq)
		assert.Equal(t, uint64(19), entries[4].Seq)
		// Bounds describe the whole buffer, not the filtered view.
		assert.Equal(t, uint64(1), oldest)
		assert.Equal(t, uint64(20), latest)
	})

	t.Run("invalid pattern surfaces the filter error", func(t *testing.T) {
		m := NewManager(ManagerConfig{BufferSize: 10})
		defer m.Close()
		m.Write(makeEntry("line"))

		_, _, _, err := m.QueryFromSeq(domain.LogFilter{Pattern: "[", IsRegex: true}, 0, 0)
		require.Error(t, err)
	})
}

func TestManager_DefaultConfig(t *testing.T) {
	m := NewManager(ManagerConfig{})
	defer m.Close()

	stats := m.Stats()
	assert.Equal(t, 1000, stats.BufferSize)
}
