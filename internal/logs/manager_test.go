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

func TestManager_DefaultConfig(t *testing.T) {
	m := NewManager(ManagerConfig{})
	defer m.Close()

	stats := m.Stats()
	assert.Equal(t, 1000, stats.BufferSize)
}
