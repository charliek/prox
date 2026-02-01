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
