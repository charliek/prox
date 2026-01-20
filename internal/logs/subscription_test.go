package logs

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscription_Send(t *testing.T) {
	sub, err := newSubscription(domain.LogFilter{}, 10)
	require.NoError(t, err)

	entry := makeEntry("hello")
	ok := sub.Send(entry)
	assert.True(t, ok)

	received := <-sub.Channel()
	assert.Equal(t, "hello", received.Line)
}

func TestSubscription_Filter(t *testing.T) {
	sub, err := newSubscription(domain.LogFilter{
		Processes: []string{"web"},
	}, 10)
	require.NoError(t, err)

	// Should pass filter
	sub.Send(makeEntryWithProcess("web", "hello"))

	// Should not pass filter (but Send returns true)
	sub.Send(makeEntryWithProcess("api", "hello"))

	// Only one message should be received
	select {
	case msg := <-sub.Channel():
		assert.Equal(t, "web", msg.Process)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected to receive message")
	}

	select {
	case <-sub.Channel():
		t.Fatal("should not receive filtered message")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestSubscription_Close(t *testing.T) {
	sub, err := newSubscription(domain.LogFilter{}, 10)
	require.NoError(t, err)

	sub.Close()

	// Send should return false after close
	ok := sub.Send(makeEntry("hello"))
	assert.False(t, ok)

	// Double close should be safe
	sub.Close()
}

func TestSubscription_FullChannel(t *testing.T) {
	sub, err := newSubscription(domain.LogFilter{}, 2)
	require.NoError(t, err)

	// Fill the buffer
	sub.Send(makeEntry("1"))
	sub.Send(makeEntry("2"))

	// This should drop (non-blocking)
	ok := sub.Send(makeEntry("3"))
	assert.False(t, ok)
}

func TestSubscriptionManager_Subscribe(t *testing.T) {
	m := NewSubscriptionManager(10)

	id, ch, err := m.Subscribe(domain.LogFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	assert.NotNil(t, ch)
	assert.Equal(t, 1, m.Count())
}

func TestSubscriptionManager_Unsubscribe(t *testing.T) {
	m := NewSubscriptionManager(10)

	id, ch, err := m.Subscribe(domain.LogFilter{})
	require.NoError(t, err)

	m.Unsubscribe(id)
	assert.Equal(t, 0, m.Count())

	// Channel should be closed
	_, ok := <-ch
	assert.False(t, ok)
}

func TestSubscriptionManager_Broadcast(t *testing.T) {
	m := NewSubscriptionManager(10)

	_, ch1, _ := m.Subscribe(domain.LogFilter{})
	_, ch2, _ := m.Subscribe(domain.LogFilter{})

	entry := makeEntry("broadcast")
	m.Broadcast(entry)

	msg1 := <-ch1
	msg2 := <-ch2

	assert.Equal(t, "broadcast", msg1.Line)
	assert.Equal(t, "broadcast", msg2.Line)
}

func TestSubscriptionManager_BroadcastWithFilter(t *testing.T) {
	m := NewSubscriptionManager(10)

	_, webCh, _ := m.Subscribe(domain.LogFilter{Processes: []string{"web"}})
	_, apiCh, _ := m.Subscribe(domain.LogFilter{Processes: []string{"api"}})

	m.Broadcast(makeEntryWithProcess("web", "web message"))

	// webCh should receive
	select {
	case msg := <-webCh:
		assert.Equal(t, "web message", msg.Line)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("webCh should receive message")
	}

	// apiCh should not receive
	select {
	case <-apiCh:
		t.Fatal("apiCh should not receive message")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestSubscriptionManager_Close(t *testing.T) {
	m := NewSubscriptionManager(10)

	_, ch1, _ := m.Subscribe(domain.LogFilter{})
	_, ch2, _ := m.Subscribe(domain.LogFilter{})

	m.Close()

	assert.Equal(t, 0, m.Count())

	// Channels should be closed
	_, ok1 := <-ch1
	_, ok2 := <-ch2
	assert.False(t, ok1)
	assert.False(t, ok2)
}

func TestSubscriptionManager_Concurrent(t *testing.T) {
	m := NewSubscriptionManager(100)

	var wg sync.WaitGroup

	// Concurrent subscribes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				id, _, _ := m.Subscribe(domain.LogFilter{})
				m.Unsubscribe(id)
			}
		}()
	}

	// Concurrent broadcasts
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				m.Broadcast(makeEntry("concurrent"))
			}
		}()
	}

	wg.Wait()
}
