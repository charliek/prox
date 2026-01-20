package logs

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
)

func makeEntry(line string) domain.LogEntry {
	return domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      line,
	}
}

func TestRingBuffer_Write_Read(t *testing.T) {
	b := NewRingBuffer(5)

	b.Write(makeEntry("1"))
	b.Write(makeEntry("2"))
	b.Write(makeEntry("3"))

	entries := b.Read()
	assert.Len(t, entries, 3)
	assert.Equal(t, "1", entries[0].Line)
	assert.Equal(t, "2", entries[1].Line)
	assert.Equal(t, "3", entries[2].Line)
}

func TestRingBuffer_Overflow(t *testing.T) {
	b := NewRingBuffer(3)

	b.Write(makeEntry("1"))
	b.Write(makeEntry("2"))
	b.Write(makeEntry("3"))
	b.Write(makeEntry("4")) // Overwrites "1"

	entries := b.Read()
	assert.Len(t, entries, 3)
	assert.Equal(t, "2", entries[0].Line)
	assert.Equal(t, "3", entries[1].Line)
	assert.Equal(t, "4", entries[2].Line)
}

func TestRingBuffer_OverflowMultiple(t *testing.T) {
	b := NewRingBuffer(3)

	// Write more than capacity
	for i := 1; i <= 10; i++ {
		b.Write(makeEntry(string(rune('0' + i))))
	}

	entries := b.Read()
	assert.Len(t, entries, 3)
	// Should have last 3 entries (8, 9, 10 -> "8", "9", ":")
	assert.Equal(t, 3, b.Count())
}

func TestRingBuffer_ReadLast(t *testing.T) {
	b := NewRingBuffer(10)

	for i := 1; i <= 5; i++ {
		b.Write(makeEntry(string(rune('0' + i))))
	}

	// Read last 3
	entries := b.ReadLast(3)
	assert.Len(t, entries, 3)
	assert.Equal(t, "3", entries[0].Line)
	assert.Equal(t, "4", entries[1].Line)
	assert.Equal(t, "5", entries[2].Line)
}

func TestRingBuffer_ReadLast_MoreThanExists(t *testing.T) {
	b := NewRingBuffer(10)

	b.Write(makeEntry("1"))
	b.Write(makeEntry("2"))

	entries := b.ReadLast(10)
	assert.Len(t, entries, 2)
}

func TestRingBuffer_ReadLast_AfterOverflow(t *testing.T) {
	b := NewRingBuffer(3)

	b.Write(makeEntry("1"))
	b.Write(makeEntry("2"))
	b.Write(makeEntry("3"))
	b.Write(makeEntry("4"))
	b.Write(makeEntry("5"))

	entries := b.ReadLast(2)
	assert.Len(t, entries, 2)
	assert.Equal(t, "4", entries[0].Line)
	assert.Equal(t, "5", entries[1].Line)
}

func TestRingBuffer_Empty(t *testing.T) {
	b := NewRingBuffer(5)

	entries := b.Read()
	assert.Nil(t, entries)
	assert.Equal(t, 0, b.Count())
}

func TestRingBuffer_Clear(t *testing.T) {
	b := NewRingBuffer(5)

	b.Write(makeEntry("1"))
	b.Write(makeEntry("2"))
	b.Clear()

	entries := b.Read()
	assert.Nil(t, entries)
	assert.Equal(t, 0, b.Count())
}

func TestRingBuffer_Concurrent(t *testing.T) {
	b := NewRingBuffer(100)

	var wg sync.WaitGroup
	numWriters := 10
	writesPerWriter := 100

	// Concurrent writers
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				b.Write(makeEntry("msg"))
			}
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = b.Read()
				_ = b.ReadLast(10)
			}
		}()
	}

	wg.Wait()

	// Should have 100 entries (buffer size)
	assert.Equal(t, 100, b.Count())
}

func TestRingBuffer_DefaultCapacity(t *testing.T) {
	b := NewRingBuffer(0)
	assert.Equal(t, 1000, b.Capacity())

	b2 := NewRingBuffer(-5)
	assert.Equal(t, 1000, b2.Capacity())
}
