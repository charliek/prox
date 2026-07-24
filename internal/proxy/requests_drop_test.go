package proxy

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded log sink: the first-drop log line is emitted
// on a goroutine (so logger I/O never runs under the ring mutex on the SSE
// hot path), which means the test's capture buffer is written concurrently
// with the test's own reads and must be synchronized.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRequestManager_DroppedEventsUnderBurst pins D9: a burst larger than a
// subscriber's channel buffer against a subscriber that never reads yields a
// non-zero DroppedEvents total, and the per-subscription first drop logs exactly
// once (not per-drop).
func TestRequestManager_DroppedEventsUnderBurst(t *testing.T) {
	logBuf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	m := NewRequestManager(1000)
	// Subscribe with the default 100-slot channel and never read it, so a burst
	// past 100 forces the non-blocking send in notifySubscribers to drop.
	sub := m.Subscribe(RequestFilter{})
	if sub == nil {
		t.Fatal("Subscribe returned nil")
	}

	const burst = 150
	for i := 0; i < burst; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("r%03d", i), Method: "GET", URL: "/x"})
	}

	if got := m.DroppedEvents(); got <= 0 {
		t.Fatalf("DroppedEvents() = %d, want > 0 after a %d-event burst against an unread subscriber", got, burst)
	}
	// Expect roughly burst-100 drops (the first 100 fill the buffer).
	if got := m.DroppedEvents(); got < int64(burst-100) {
		t.Errorf("DroppedEvents() = %d, want >= %d", got, burst-100)
	}

	// The first-drop log is asynchronous (goroutine); wait for it to land,
	// then confirm no further lines arrive (exactly-once).
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logBuf.String(), "is dropping events") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logBuf.String(), "is dropping events"); n != 1 {
		t.Errorf("first-drop log lines = %d, want exactly 1 (first drop per subscription logs once); log:\n%s", n, logBuf.String())
	}
}
