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

// TestRequestManager_OverflowClosesSubscription pins D9 as revised by C6: a
// burst larger than a subscriber's channel buffer against a subscriber that
// never reads CLOSES that subscription rather than silently dropping records
// forever. The overflow that forced the close is still counted
// (DroppedEvents), the close logs exactly once, and the manager keeps
// accepting writes afterwards.
func TestRequestManager_OverflowClosesSubscription(t *testing.T) {
	logBuf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	m := NewRequestManager(1000)
	// Subscribe with the default 100-slot channel and never read it, so a burst
	// past 100 forces the non-blocking send in notifySubscribers to overflow.
	sub := m.Subscribe(RequestFilter{})
	if sub == nil {
		t.Fatal("Subscribe returned nil")
	}

	const burst = 150
	for i := 0; i < burst; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("r%03d", i), Method: "GET", URL: "/x"})
	}

	// The overflow event itself is still counted; everything after it is not,
	// because the subscription no longer exists.
	if got := m.DroppedEvents(); got != 1 {
		t.Errorf("DroppedEvents() = %d, want exactly 1 (the overflow that forced the close)", got)
	}

	// The channel is closed: the buffered records are still readable, then the
	// receive reports end-of-stream. That close is what the SSE handlers see,
	// and what makes the client reconnect and re-sync.
	drained := 0
	for range sub.Ch {
		drained++
		if drained > burst {
			t.Fatalf("channel never closed after %d receives", drained)
		}
	}
	if drained != 100 {
		t.Errorf("drained %d buffered records before close, want the full 100-slot buffer", drained)
	}

	// The manager must keep working: a send on the closed channel would panic.
	if !m.Record(RequestRecord{ID: "after", Method: "GET", URL: "/y"}) {
		t.Error("Record after an overflow close was rejected")
	}
	if !m.Upsert(RequestRecord{ID: "after2", Method: "GET", URL: "/z"}) {
		t.Error("Upsert after an overflow close was rejected")
	}
	// Unsubscribe/Close must not double-close the latched channel.
	m.Unsubscribe(sub.ID)
	m.Close()

	// The close log is asynchronous (goroutine); wait for it to land, then
	// confirm no further lines arrive (exactly-once).
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logBuf.String(), "overflowed and was closed") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := strings.Count(logBuf.String(), "overflowed and was closed"); n != 1 {
		t.Errorf("overflow log lines = %d, want exactly 1; log:\n%s", n, logBuf.String())
	}
}

// TestRequestManager_OverflowLeavesOtherSubscribersAlone pins that the close is
// per-subscription: a healthy reader keeps its stream when a slow one is
// dropped.
func TestRequestManager_OverflowLeavesOtherSubscribersAlone(t *testing.T) {
	m := NewRequestManager(1000)
	slow := m.Subscribe(RequestFilter{})
	// The healthy subscriber filters the burst out entirely, so the test is
	// deterministic: only the unread catch-all subscription can overflow.
	fast := m.Subscribe(RequestFilter{Subdomain: "quiet"})

	const burst = 150
	for i := 0; i < burst; i++ {
		m.Record(RequestRecord{ID: fmt.Sprintf("r%03d", i), Method: "GET", URL: "/x"})
	}

	// slow is closed and gone; fast is still live and still delivered to.
	for range slow.Ch {
	}
	m.Record(RequestRecord{ID: "later", Method: "GET", URL: "/later", Subdomain: "quiet"})
	select {
	case rec, ok := <-fast.Ch:
		if !ok {
			t.Fatal("the healthy subscriber's channel was closed")
		}
		if rec.ID != "later" {
			t.Errorf("healthy subscriber got %q, want \"later\"", rec.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy subscriber received nothing after the slow one was dropped")
	}
}
