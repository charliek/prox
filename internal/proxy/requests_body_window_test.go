package proxy

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Replica inline-body window ---
//
// A ring that REPLICATES records captured elsewhere (the forwarder-fed
// project-local ring in shared mode) bounds how many records may hold retained
// INLINE body data, ordered by the daemon-supplied Timestamp rather than by
// ring position. Position on a replica is not recency: backfill races the live
// stream, so a freshly-timestamped record can sit at the ring's oldest
// position. Spilled (FilePath) bodies are exempt — they cost this ring no
// memory and the daemon's disk truth is authoritative on load.

// checkBodyWindowInvariant asserts the body window's three invariants after a
// mutation:
//
//  1. bodySlots[s] != nil exactly when live slot s's record retains inline body
//     data (the hasRetainedInlineBody predicate), and no non-live slot carries
//     an entry;
//  2. every non-nil bodySlots[s] is exactly ONE heap entry whose slot is s and
//     whose heapIdx is its real position, and len(bodyHeap) equals the number
//     of such slots;
//  3. len(bodyHeap) <= bodyWindow — the memory bound, enforced immediately.
//
// It also re-checks the heap ordering property, since a broken heap would let
// the wrong record be stripped without violating 1-3. It takes the ring read
// lock, so it must not be called while the caller holds m.mu.
func (m *RequestManager) checkBodyWindowInvariant(t *testing.T) {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.bodyWindow <= 0 {
		if len(m.bodyHeap) != 0 || m.bodySlots != nil {
			t.Fatalf("body window disabled but state is allocated: heap=%d slots=%d",
				len(m.bodyHeap), len(m.bodySlots))
		}
		return
	}

	live := make(map[int]bool, m.count)
	want := 0
	for i := 0; i < m.count; i++ {
		idx := (m.head - 1 - i + m.capacity) % m.capacity
		live[idx] = true
		retained := hasRetainedInlineBody(m.buffer[idx])
		entry := m.bodySlots[idx]
		if retained != (entry != nil) {
			t.Fatalf("slot %d (record %q): retains inline body=%v but bodySlots entry present=%v",
				idx, m.buffer[idx].ID, retained, entry != nil)
		}
		if entry != nil {
			if entry.slot != idx {
				t.Fatalf("bodySlots[%d] holds an entry claiming slot %d", idx, entry.slot)
			}
			want++
		}
	}
	for slot, entry := range m.bodySlots {
		if entry != nil && !live[slot] {
			t.Fatalf("bodySlots[%d] is set but that slot is not live", slot)
		}
	}

	if len(m.bodyHeap) != want {
		t.Fatalf("heap holds %d entries but %d live slots retain inline bodies", len(m.bodyHeap), want)
	}
	seen := make(map[*bodyHeapEntry]bool, len(m.bodyHeap))
	for i, entry := range m.bodyHeap {
		if entry.heapIdx != i {
			t.Fatalf("heap entry at %d records heapIdx %d", i, entry.heapIdx)
		}
		if seen[entry] {
			t.Fatalf("heap entry for slot %d appears twice", entry.slot)
		}
		seen[entry] = true
		if m.bodySlots[entry.slot] != entry {
			t.Fatalf("heap entry for slot %d is not the one bodySlots points at", entry.slot)
		}
	}
	if len(m.bodyHeap) > m.bodyWindow {
		t.Fatalf("heap holds %d entries, window is %d", len(m.bodyHeap), m.bodyWindow)
	}
	for i := 1; i < len(m.bodyHeap); i++ {
		if parent := (i - 1) / 2; m.bodyHeap.Less(i, parent) {
			t.Fatalf("heap ordering violated: child %d sorts before parent %d", i, parent)
		}
	}
}

// bodyWindowLen reports how many slots the window currently accounts for.
func (m *RequestManager) bodyWindowLen() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bodyHeap)
}

// inlineBodied builds a final record carrying an inline response body plus
// headers, so a strip is observable as "headers kept, payload gone".
func inlineBodied(id string, ts time.Time) RequestRecord {
	return RequestRecord{
		ID:        id,
		Timestamp: ts,
		Method:    "POST",
		URL:       "/" + id,
		Details: &RequestDetails{
			RequestHeaders: map[string][]string{"X-Req": {id}},
			ResponseBody:   &CapturedBody{Size: 7, CapturedSize: 7, Data: []byte("payload")},
		},
	}
}

// retainsInlineBody reports whether the ring's copy of id still serves body
// bytes from memory.
func retainsInlineBody(t *testing.T, m *RequestManager, id string) bool {
	t.Helper()
	rec, ok := m.GetByID(id)
	require.True(t, ok, "record %s must still be in the ring", id)
	return hasRetainedInlineBody(rec)
}

// strippedIDs returns the IDs of ring records whose inline body was dropped,
// sorted, so callers assert on the SET that lost bodies rather than on the ring
// order it happens to sit in.
func strippedIDs(t *testing.T, m *RequestManager, limit int) []string {
	t.Helper()
	var ids []string
	for _, rec := range m.Recent(RequestFilter{Limit: limit}) {
		if !hasRetainedInlineBody(rec) {
			ids = append(ids, rec.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// inlineBodiedRecords counts records still holding inline body data — the body
// window's own predicate, unlike detailedRecords, which counts a spilled body as
// retained because that is what the POSITION window bounds.
func inlineBodiedRecords(recs []RequestRecord) int {
	n := 0
	for _, rec := range recs {
		if hasRetainedInlineBody(rec) {
			n++
		}
	}
	return n
}

// TestReplicaBodyWindow_BoundsAndStripsOldestTimestampFirst is the window's
// core contract: no more than `window` records retain inline body data, and the
// ones that lose it are the oldest by daemon-supplied Timestamp — not the ones
// deepest in the ring.
func TestReplicaBodyWindow_BoundsAndStripsOldestTimestampFirst(t *testing.T) {
	const capacity, window, total = 20, 3, 8
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	base := time.Now()

	for i := 0; i < total; i++ {
		require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("r%d", i), base.Add(time.Duration(i)*time.Second))))
		m.checkBodyWindowInvariant(t)
		m.checkIndexInvariant(t)
	}

	require.Equal(t, total, m.Count(), "the window drops bodies, never records")
	assert.Equal(t, window, m.bodyWindowLen())

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("r%d", i)
		rec, ok := m.GetByID(id)
		require.True(t, ok)
		require.NotNil(t, rec.Details)
		assert.NotEmpty(t, rec.Details.RequestHeaders, "%s keeps its headers either way", id)
		require.NotNil(t, rec.Details.ResponseBody)

		if i >= total-window {
			assert.True(t, hasRetainedInlineBody(rec), "%s is among the newest %d and must keep its body", id, window)
			assert.False(t, rec.Details.ResponseBody.Evicted)
			continue
		}
		assert.False(t, hasRetainedInlineBody(rec), "%s is older than the window and must be stripped", id)
		assert.True(t, rec.Details.ResponseBody.Evicted, "%s must be marked evicted, not silently emptied", id)
		assert.Nil(t, rec.Details.ResponseBody.Data)
		assert.Equal(t, int64(7), rec.Details.ResponseBody.Size, "size metadata survives the strip")

		decoded, err := LoadDecodedBody(rec.Details.ResponseBody, nil)
		require.NoError(t, err, "an evicted body is a benign condition")
		assert.False(t, decoded.Available)
		assert.Equal(t, "evicted", decoded.UnavailableReason)
	}
}

// TestReplicaBodyWindow_KeepsFreshRecordAtOldestRingPosition is the regression
// that decides the whole design. The forwarder's backfill deliberately races
// the live event stream, so a record delivered LIVE can end up at the ring's
// OLDEST position while a burst of backfilled, older-timestamped records sits
// newer than it. A position-ordered window would strip that fresh record's body
// while the daemon still serves it; the timestamp-ordered window must keep it
// and take the genuinely oldest records instead.
func TestReplicaBodyWindow_KeepsFreshRecordAtOldestRingPosition(t *testing.T) {
	const capacity, window = 20, 3
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	base := time.Now()

	// The live delivery lands first: newest timestamp, oldest ring position.
	fresh := inlineBodied("live-fresh", base.Add(time.Hour))
	require.True(t, m.Upsert(fresh))
	m.checkBodyWindowInvariant(t)

	// Backfill then replays older records oldest-first, all newer BY POSITION.
	const backfilled = 5
	for i := 0; i < backfilled; i++ {
		require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("backfill%d", i), base.Add(time.Duration(i)*time.Second))))
		m.checkBodyWindowInvariant(t)
	}

	// Precondition: the fresh record really is at the ring's oldest position,
	// and fewer than `window` records have newer timestamps than it (in fact
	// none do).
	all := m.Recent(RequestFilter{Limit: capacity})
	require.Len(t, all, backfilled+1)
	require.Equal(t, "live-fresh", all[len(all)-1].ID, "precondition: fresh record sits at the oldest ring position")
	newer := 0
	for _, rec := range all {
		if rec.Timestamp.After(fresh.Timestamp) {
			newer++
		}
	}
	require.Less(t, newer, window, "precondition: fewer than a window's worth of records are newer by timestamp")

	assert.True(t, retainsInlineBody(t, m, "live-fresh"),
		"a live-delivered record at the oldest ring POSITION is still the newest by TIMESTAMP and must keep its body")
	assert.Equal(t, []string{"backfill0", "backfill1", "backfill2"}, strippedIDs(t, m, capacity),
		"exactly the oldest-by-timestamp records lose their bodies")
}

// TestReplicaBodyWindow_InFlightCompletionEntersWindow covers the one path that
// adds body data to a slot the ring already holds: an in-flight record carries
// no Details, so its completion is what puts it in the window — and, when the
// request STARTED before everything else in the ring, what strips it again on
// arrival. Both entry points must publish the record as stored, never as
// submitted.
func TestReplicaBodyWindow_InFlightCompletionEntersWindow(t *testing.T) {
	t.Run("completion joins the window and evicts an older record", func(t *testing.T) {
		m := newReplicaRequestManagerWithBodyWindow(10, 2)
		base := time.Now()

		require.True(t, m.Upsert(RequestRecord{ID: "slow", Timestamp: base.Add(time.Hour), Method: "GET", URL: "/slow", InFlight: true}))
		require.True(t, m.Upsert(inlineBodied("old0", base)))
		require.True(t, m.Upsert(inlineBodied("old1", base.Add(time.Second))))
		m.checkBodyWindowInvariant(t)
		require.Equal(t, 2, m.bodyWindowLen())

		sub := m.Subscribe(RequestFilter{})
		defer m.Unsubscribe(sub.ID)

		completion := inlineBodied("slow", base.Add(time.Hour))
		completion.StatusCode = 200
		require.True(t, m.Upsert(completion))
		m.checkBodyWindowInvariant(t)

		assert.True(t, retainsInlineBody(t, m, "slow"), "the newest completion keeps its body")
		assert.True(t, retainsInlineBody(t, m, "old1"))
		assert.False(t, retainsInlineBody(t, m, "old0"), "the oldest bodied record makes room")

		notified := readRecordEvent(t, sub.Ch)
		require.Equal(t, "slow", notified.ID)
		assert.False(t, notified.InFlight, "the completion still applies")
		assert.Equal(t, 200, notified.StatusCode)
		require.NotNil(t, notified.Details)
		require.NotNil(t, notified.Details.ResponseBody)
		assert.Equal(t, []byte("payload"), notified.Details.ResponseBody.Data,
			"a retained body must be published intact")
	})

	t.Run("completion strips itself and Upsert publishes the stripped copy", func(t *testing.T) {
		m := newReplicaRequestManagerWithBodyWindow(10, 2)
		base := time.Now()

		// The slow request STARTED first, so its timestamp is the oldest.
		require.True(t, m.Upsert(RequestRecord{ID: "slow", Timestamp: base, Method: "GET", URL: "/slow", InFlight: true}))
		require.True(t, m.Upsert(inlineBodied("n0", base.Add(time.Second))))
		require.True(t, m.Upsert(inlineBodied("n1", base.Add(2*time.Second))))

		sub := m.Subscribe(RequestFilter{})
		defer m.Unsubscribe(sub.ID)

		completion := inlineBodied("slow", base)
		completion.StatusCode = 200
		require.True(t, m.Upsert(completion))
		m.checkBodyWindowInvariant(t)

		stored, ok := m.GetByID("slow")
		require.True(t, ok)
		assert.False(t, stored.InFlight, "only the body is dropped; the completion applies")
		assert.Equal(t, 200, stored.StatusCode)
		require.NotNil(t, stored.Details.ResponseBody)
		assert.True(t, stored.Details.ResponseBody.Evicted)
		assert.Nil(t, stored.Details.ResponseBody.Data)
		assert.True(t, retainsInlineBody(t, m, "n0"), "the newer records keep theirs")
		assert.True(t, retainsInlineBody(t, m, "n1"))

		notified := readRecordEvent(t, sub.Ch)
		require.Equal(t, "slow", notified.ID)
		require.NotNil(t, notified.Details.ResponseBody)
		assert.True(t, notified.Details.ResponseBody.Evicted,
			"the notified record must not advertise body data the ring refuses to serve")
		assert.Nil(t, notified.Details.ResponseBody.Data)

		// The submitted record itself is untouched — the ring copies on strip.
		assert.Equal(t, []byte("payload"), completion.Details.ResponseBody.Data)
		assert.False(t, completion.Details.ResponseBody.Evicted)
	})

	t.Run("Record publishes the stripped copy too", func(t *testing.T) {
		m := newReplicaRequestManagerWithBodyWindow(10, 1)
		base := time.Now()

		require.True(t, m.Record(inlineBodied("newer", base.Add(time.Hour))))

		sub := m.Subscribe(RequestFilter{})
		defer m.Unsubscribe(sub.ID)

		// An older-timestamped arrival is its own victim.
		require.True(t, m.Record(inlineBodied("older", base)))
		m.checkBodyWindowInvariant(t)

		assert.False(t, retainsInlineBody(t, m, "older"))
		assert.True(t, retainsInlineBody(t, m, "newer"))

		notified := readRecordEvent(t, sub.Ch)
		require.Equal(t, "older", notified.ID)
		require.NotNil(t, notified.Details.ResponseBody)
		assert.True(t, notified.Details.ResponseBody.Evicted,
			"Record notifies after unlock, but with the STORED record")
	})
}

// TestReplicaBodyWindow_AlreadyEvictedArrivalsNeverCount pins that records
// arriving with their bodies already stripped — backfill of records past the
// DAEMON's window — consume no window budget. Counting them would strip live
// bodies to make room for records holding nothing.
func TestReplicaBodyWindow_AlreadyEvictedArrivalsNeverCount(t *testing.T) {
	m := newReplicaRequestManagerWithBodyWindow(10, 2)
	base := time.Now()

	for i := 0; i < 4; i++ {
		rec := inlineBodied(fmt.Sprintf("gone%d", i), base.Add(time.Duration(i)*time.Second))
		rec.Details.ResponseBody = &CapturedBody{Size: 7, CapturedSize: 7, Evicted: true}
		require.True(t, m.Upsert(rec))
		m.checkBodyWindowInvariant(t)
	}
	assert.Zero(t, m.bodyWindowLen(), "already-evicted bodies hold no memory and take no budget")

	require.True(t, m.Upsert(inlineBodied("live0", base.Add(10*time.Second))))
	require.True(t, m.Upsert(inlineBodied("live1", base.Add(11*time.Second))))
	m.checkBodyWindowInvariant(t)
	assert.Equal(t, 2, m.bodyWindowLen())
	assert.True(t, retainsInlineBody(t, m, "live0"))
	assert.True(t, retainsInlineBody(t, m, "live1"))

	// Bodyless records (a plain GET, or a record with no Details at all) are
	// likewise invisible to the window.
	require.True(t, m.Upsert(RequestRecord{ID: "bare", Timestamp: base.Add(20 * time.Second), Method: "GET", URL: "/bare"}))
	require.True(t, m.Upsert(RequestRecord{
		ID: "emptybody", Timestamp: base.Add(21 * time.Second), Method: "GET", URL: "/empty",
		Details: &RequestDetails{ResponseBody: &CapturedBody{}},
	}))
	m.checkBodyWindowInvariant(t)
	assert.Equal(t, 2, m.bodyWindowLen())
	assert.True(t, retainsInlineBody(t, m, "live0"), "bodyless arrivals must not evict a real body")
}

// TestReplicaBodyWindow_DuplicateDeliveryDoesNotDoubleCount pins that the
// backfill/live race — which delivers the same final record twice — cannot
// inflate the window's accounting. Upsert treats a final record as terminal, so
// the second delivery must be a complete no-op for the window too.
func TestReplicaBodyWindow_DuplicateDeliveryDoesNotDoubleCount(t *testing.T) {
	m := newReplicaRequestManagerWithBodyWindow(10, 2)
	base := time.Now()

	rec := inlineBodied("dup-delivery", base)
	require.True(t, m.Upsert(rec))
	require.True(t, m.Upsert(rec))
	require.True(t, m.Upsert(rec))
	m.checkBodyWindowInvariant(t)

	assert.Equal(t, 1, m.Count(), "a duplicate final delivery is a no-op")
	assert.Equal(t, 1, m.bodyWindowLen())
	assert.True(t, retainsInlineBody(t, m, "dup-delivery"))
}

// TestReplicaBodyWindow_DuplicateExplicitIDsCountPerSlot pins that window
// membership is keyed by ring SLOT, not by record ID. The ring deliberately
// permits duplicate explicit IDs, so an ID names no unique occupant: two copies
// hold two bodies and must be counted (and stripped) independently.
func TestReplicaBodyWindow_DuplicateExplicitIDsCountPerSlot(t *testing.T) {
	m := newReplicaRequestManagerWithBodyWindow(10, 1)
	base := time.Now()

	older := inlineBodied("dup", base)
	older.RemoteAddr = "old"
	newer := inlineBodied("dup", base.Add(time.Minute))
	newer.RemoteAddr = "new"

	require.True(t, m.Record(older))
	m.checkBodyWindowInvariant(t)
	assert.Equal(t, 1, m.bodyWindowLen())

	require.True(t, m.Record(newer))
	m.checkBodyWindowInvariant(t)
	m.checkIndexInvariant(t)

	assert.Equal(t, 2, m.Count(), "both copies stay in the ring")
	assert.Equal(t, 1, m.bodyWindowLen(), "two bodies were counted; the window kept one")

	recs := m.Recent(RequestFilter{Limit: 10})
	require.Len(t, recs, 2)
	assert.Equal(t, "new", recs[0].RemoteAddr)
	assert.True(t, hasRetainedInlineBody(recs[0]), "the newer copy keeps its body")
	assert.Equal(t, "old", recs[1].RemoteAddr)
	assert.False(t, hasRetainedInlineBody(recs[1]), "the older copy's body was stripped on its own merits")
}

// TestReplicaBodyWindow_EqualTimestampsStripInArrivalOrder pins the tie-break:
// records sharing a timestamp are stripped first-arrived-first, so the strip
// order is a property of the data rather than of the heap's internal layout.
func TestReplicaBodyWindow_EqualTimestampsStripInArrivalOrder(t *testing.T) {
	m := newReplicaRequestManagerWithBodyWindow(10, 2)
	ts := time.Now()

	for i := 0; i < 5; i++ {
		require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("tie%d", i), ts)))
		m.checkBodyWindowInvariant(t)
	}

	assert.Equal(t, []string{"tie0", "tie1", "tie2"}, strippedIDs(t, m, 10),
		"equal timestamps break FIFO by arrival")
	assert.True(t, retainsInlineBody(t, m, "tie3"))
	assert.True(t, retainsInlineBody(t, m, "tie4"))
}

// TestReplicaBodyWindow_ZeroAndRegressedTimestampsStripFirst pins the behavior
// for records whose clock reading is unusable: they sort oldest and lose their
// bodies first. That is the memory-safe direction — a record with no usable
// timestamp must not be able to hold memory ahead of records that have one.
func TestReplicaBodyWindow_ZeroAndRegressedTimestampsStripFirst(t *testing.T) {
	t.Run("zero timestamp", func(t *testing.T) {
		m := newReplicaRequestManagerWithBodyWindow(10, 1)
		require.True(t, m.Upsert(inlineBodied("zero", time.Time{})))
		require.True(t, m.Upsert(inlineBodied("dated", time.Now())))
		m.checkBodyWindowInvariant(t)

		assert.False(t, retainsInlineBody(t, m, "zero"))
		assert.True(t, retainsInlineBody(t, m, "dated"))
	})

	t.Run("regressed timestamp strips itself", func(t *testing.T) {
		m := newReplicaRequestManagerWithBodyWindow(10, 1)
		now := time.Now()
		require.True(t, m.Upsert(inlineBodied("dated", now)))
		// A clock step (or a long-running request's start time) can put a later
		// ARRIVAL behind an earlier record in time order.
		require.True(t, m.Upsert(inlineBodied("regressed", now.Add(-time.Hour))))
		m.checkBodyWindowInvariant(t)

		assert.True(t, retainsInlineBody(t, m, "dated"))
		assert.False(t, retainsInlineBody(t, m, "regressed"),
			"the record that is oldest by timestamp is stripped even when it arrived last")
	})
}

// TestReplicaBodyWindow_StripsInlineButNeverSpilledBodies pins the exemption
// that keeps the replica honest about bodies it does not own: a spilled body's
// bytes live in the DAEMON's capture dir, so stripping it locally would free no
// memory and would refuse a body the daemon still serves. A mixed record loses
// its inline half and keeps its spilled half servable and unmarked.
func TestReplicaBodyWindow_StripsInlineButNeverSpilledBodies(t *testing.T) {
	dir := t.TempDir()
	spillPath := filepath.Join(dir, "mixed_res.bin")
	require.NoError(t, os.WriteFile(spillPath, []byte("spilled bytes"), 0o600))

	m := newReplicaRequestManagerWithBodyWindow(10, 1)
	base := time.Now()

	mixed := RequestRecord{
		ID: "mixed", Timestamp: base, Method: "POST", URL: "/mixed",
		Details: &RequestDetails{
			RequestHeaders: map[string][]string{"X-Req": {"mixed"}},
			RequestBody:    &CapturedBody{Size: 7, CapturedSize: 7, Data: []byte("payload")},
			ResponseBody:   &CapturedBody{Size: 13, CapturedSize: 13, FilePath: spillPath},
		},
	}
	require.True(t, m.Upsert(mixed))
	m.checkBodyWindowInvariant(t)
	require.Equal(t, 1, m.bodyWindowLen(), "the inline half puts the record in the window")

	// A newer inline-bodied record pushes the mixed record out.
	require.True(t, m.Upsert(inlineBodied("newer", base.Add(time.Second))))
	m.checkBodyWindowInvariant(t)

	rec, ok := m.GetByID("mixed")
	require.True(t, ok)
	require.NotNil(t, rec.Details)
	assert.NotEmpty(t, rec.Details.RequestHeaders, "headers survive the strip")

	require.NotNil(t, rec.Details.RequestBody)
	assert.True(t, rec.Details.RequestBody.Evicted, "the inline body is stripped and marked")
	assert.Nil(t, rec.Details.RequestBody.Data)

	require.NotNil(t, rec.Details.ResponseBody)
	assert.False(t, rec.Details.ResponseBody.Evicted, "the spilled body is left completely untouched")
	assert.Equal(t, spillPath, rec.Details.ResponseBody.FilePath)
	assert.True(t, fileExists(t, spillPath), "the replica must never unlink a daemon-owned capture file")

	data, err := LoadCapturedBody(rec.Details.ResponseBody, []string{dir})
	require.NoError(t, err)
	assert.Equal(t, []byte("spilled bytes"), data, "the spilled body is still servable")

	// Having lost its only inline body, the record has left the window.
	assert.Equal(t, 1, m.bodyWindowLen())
	assert.False(t, retainsInlineBody(t, m, "mixed"))
	assert.True(t, retainsInlineBody(t, m, "newer"))
}

// TestReplicaBodyWindow_NeverInvokesEvictionCallback pins the ownership
// contract: a replica holds no capture files, and a body-window strip never
// needs one unlinked, so the strip path must produce no cleanup work. The
// scenario stays well inside ring capacity, so ring eviction cannot fire the
// callback either and any invocation is the window's doing.
func TestReplicaBodyWindow_NeverInvokesEvictionCallback(t *testing.T) {
	dir := t.TempDir()
	spillPath := filepath.Join(dir, "keep_res.bin")
	require.NoError(t, os.WriteFile(spillPath, []byte("daemon owns this"), 0o600))

	m := newReplicaRequestManagerWithBodyWindow(50, 2)
	var invoked []string
	m.SetEvictionCallback(func(id string) { invoked = append(invoked, id) })

	base := time.Now()
	spilled := RequestRecord{
		ID: "spilled", Timestamp: base, Method: "POST", URL: "/spilled",
		Details: &RequestDetails{ResponseBody: &CapturedBody{Size: 16, CapturedSize: 16, FilePath: spillPath}},
	}
	require.True(t, m.Upsert(spilled))

	for i := 0; i < 10; i++ {
		require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("r%d", i), base.Add(time.Duration(i+1)*time.Second))))
		m.checkBodyWindowInvariant(t)
	}

	require.Equal(t, 11, m.Count(), "capacity is 50: nothing left the ring")
	assert.Equal(t, 2, m.bodyWindowLen())
	assert.Empty(t, invoked, "the body window must never route anything to the eviction callback")
	assert.True(t, fileExists(t, spillPath), "the daemon's capture file is untouched")
}

// TestReplicaBodyWindow_SurvivesCapacityOverwrite runs the window past the
// ring's own eviction boundary: a slot's outgoing occupant must surrender its
// window membership, or the accounting would keep charging for bodies that no
// longer exist and starve live records.
func TestReplicaBodyWindow_SurvivesCapacityOverwrite(t *testing.T) {
	const capacity, window = 4, 3
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	base := time.Now()

	for i := 0; i < capacity*4; i++ {
		require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("r%02d", i), base.Add(time.Duration(i)*time.Second))))
		m.checkBodyWindowInvariant(t)
		m.checkIndexInvariant(t)
	}

	assert.Equal(t, capacity, m.Count())
	assert.Equal(t, window, m.bodyWindowLen(), "the window stays saturated as the ring wraps")
	assert.Equal(t, []string{"r12"}, strippedIDs(t, m, capacity),
		"the oldest surviving record is the one without a body")
}

// TestReplicaBodyWindow_PurgeRebuildsWindow pins the purge path: compaction
// moves records between slots, invalidating every slot-keyed entry, so the
// window is rebuilt from the surviving records. Survivors must keep exactly the
// bodies they had, and the ring must keep enforcing the bound afterwards.
func TestReplicaBodyWindow_PurgeRebuildsWindow(t *testing.T) {
	const capacity, window = 20, 4
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	base := time.Now()

	for i := 0; i < 10; i++ {
		project := "/p/a"
		if i%2 == 1 {
			project = "/p/b"
		}
		rec := inlineBodied(fmt.Sprintf("r%02d", i), base.Add(time.Duration(i)*time.Second))
		rec.ProjectDir = project
		require.True(t, m.Upsert(rec))
		m.checkBodyWindowInvariant(t)
	}
	require.Equal(t, window, m.bodyWindowLen())

	// Bodies retained before the purge, so the rebuild can be checked for
	// resurrections as well as losses.
	before := map[string]bool{}
	for _, rec := range m.Recent(RequestFilter{Limit: capacity}) {
		before[rec.ID] = hasRetainedInlineBody(rec)
	}

	m.PurgeByProject("/p/b")
	m.checkBodyWindowInvariant(t)
	m.checkIndexInvariant(t)

	remaining := m.Recent(RequestFilter{Limit: capacity})
	require.Len(t, remaining, 5)
	for _, rec := range remaining {
		assert.Equal(t, before[rec.ID], hasRetainedInlineBody(rec),
			"purge must neither strip nor resurrect a survivor's body (%s)", rec.ID)
	}

	// The rebuilt window still binds, and still strips oldest-timestamp-first.
	for i := 10; i < 16; i++ {
		rec := inlineBodied(fmt.Sprintf("r%02d", i), base.Add(time.Duration(i)*time.Second))
		rec.ProjectDir = "/p/a"
		require.True(t, m.Upsert(rec))
		m.checkBodyWindowInvariant(t)
	}
	assert.Equal(t, window, m.bodyWindowLen())
	for i := 12; i < 16; i++ {
		assert.True(t, retainsInlineBody(t, m, fmt.Sprintf("r%02d", i)))
	}
}

// TestReplicaBodyWindow_PurgePreservesEqualTimestampFIFO pins that a purge
// rebuild carries each surviving entry's (ts, seq) over unchanged. seq records
// BODY-INSERTION order — the equal-timestamp tie-break — and an in-place
// completion is exactly where that order diverges from ring order: the record's
// slot is old but its body is the newest. A rebuild that re-minted entries in
// ring order would silently change which same-timestamp record the next strip
// takes.
func TestReplicaBodyWindow_PurgePreservesEqualTimestampFIFO(t *testing.T) {
	const capacity, window = 20, 2
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	ts := time.Now()

	// A starts first (oldest ring slot) but gains its body LAST: body-insertion
	// order is B, C, A even though ring order is A, B, C.
	require.True(t, m.Upsert(RequestRecord{ID: "A", Timestamp: ts, Method: "GET", URL: "/slow", InFlight: true}))
	require.True(t, m.Upsert(inlineBodied("B", ts)))
	require.True(t, m.Upsert(inlineBodied("C", ts)))
	completion := inlineBodied("A", ts)
	completion.StatusCode = 200
	require.True(t, m.Upsert(completion))
	m.checkBodyWindowInvariant(t)

	// A's arrival overflowed the window: FIFO among the equal timestamps takes
	// B, the earliest BODY, leaving C and A — with C ahead of A in FIFO order
	// even though A sits in the older ring slot.
	require.Equal(t, []string{"B"}, strippedIDs(t, m, capacity))

	// A bodyless record in another project, purged, forces a rebuild without
	// touching the window's membership.
	decoy := RequestRecord{ID: "D", Timestamp: ts, Method: "GET", URL: "/decoy", ProjectDir: "/p/b"}
	require.True(t, m.Upsert(decoy))
	m.PurgeByProject("/p/b")
	m.checkBodyWindowInvariant(t)

	// The next equal-timestamp arrival must strip C — the oldest surviving
	// BODY — not A, the record in the oldest ring slot. A rebuild that
	// re-minted seqs in ring order would take A here.
	require.True(t, m.Upsert(inlineBodied("E", ts)))
	m.checkBodyWindowInvariant(t)
	assert.True(t, retainsInlineBody(t, m, "E"))
	assert.True(t, retainsInlineBody(t, m, "A"), "A's body is FIFO-newer than C's despite its older ring slot")
	assert.False(t, retainsInlineBody(t, m, "C"), "C holds the oldest surviving body, so FIFO takes it")
}

// TestReplicaBodyWindow_DisabledAndOversizedWindows pins the edges: a
// non-positive window turns the mechanism off entirely (and allocates nothing),
// and a window at or above ring capacity never binds before capacity does.
func TestReplicaBodyWindow_DisabledAndOversizedWindows(t *testing.T) {
	for _, tc := range []struct {
		name             string
		capacity, window int
		records          int
		wantRetained     int
	}{
		{"window disabled", 10, 0, 6, 6},
		{"window negative", 10, -1, 6, 6},
		{"window equals capacity", 6, 6, 6, 6},
		{"window larger than capacity", 6, 100, 6, 6},
		{"count never reaches the window", 10, 5, 4, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplicaRequestManagerWithBodyWindow(tc.capacity, tc.window)
			base := time.Now()
			for i := 0; i < tc.records; i++ {
				require.True(t, m.Upsert(inlineBodied(fmt.Sprintf("r%d", i), base.Add(time.Duration(i)*time.Second))))
				m.checkBodyWindowInvariant(t)
			}
			assert.Equal(t, tc.wantRetained,
				inlineBodiedRecords(m.Recent(RequestFilter{Limit: tc.capacity})))
		})
	}
}

// TestRequestManager_RejectsBothWindows pins the construction-time guard: the
// position window and the timestamp window are alternative answers to the same
// question, and composing them would leave the strip order dependent on which
// one fired first.
func TestRequestManager_RejectsBothWindows(t *testing.T) {
	assert.Panics(t, func() { newRequestManagerWithWindows(10, 2, 2) })
	assert.NotPanics(t, func() { newRequestManagerWithWindows(10, 2, 0) })
	assert.NotPanics(t, func() { newRequestManagerWithWindows(10, 0, 2) })
}

// TestReplicaBodyWindow_RandomizedModel drives the mutation mix the window must
// survive — bodied and bodyless arrivals, already-evicted arrivals, capacity
// overwrites, in-flight completions, purges, duplicate IDs, tied and regressed
// timestamps — and re-checks the invariants after every single one. The
// invariants are what make a stale entry (an entry outliving the record that
// created it) impossible, and transition completeness is the property most
// likely to be broken by a later edit, so this drives every transition rather
// than asserting a specific outcome. The seed is fixed so a failure replays.
func TestReplicaBodyWindow_RandomizedModel(t *testing.T) {
	const capacity, window, steps = 12, 4, 3000
	m := newReplicaRequestManagerWithBodyWindow(capacity, window)
	rng := rand.New(rand.NewSource(20260730))
	base := time.Now()

	// In-flight records remember their start timestamp: a production completion
	// carries the ORIGINAL Timestamp (set once at request start), and forging a
	// fresh one here would dodge the self-strip path the model exists to stress
	// — a completion whose start already ranks oldest among the bodied records.
	type pending struct {
		id string
		ts time.Time
	}
	var inFlight []pending
	for step := 0; step < steps; step++ {
		id := fmt.Sprintf("r%04d", step)
		// Mostly forward-moving timestamps, with ties and regressions mixed in.
		ts := base.Add(time.Duration(step) * time.Millisecond)
		switch rng.Intn(10) {
		case 0:
			ts = base.Add(time.Duration(step-rng.Intn(100)) * time.Millisecond)
		case 1:
			ts = base
		case 2:
			ts = time.Time{}
		}

		switch rng.Intn(12) {
		case 0, 1, 2, 3:
			m.Upsert(inlineBodied(id, ts))
		case 4:
			m.Upsert(RequestRecord{ID: id, Timestamp: ts, Method: "GET", URL: "/bare"})
		case 5:
			rec := inlineBodied(id, ts)
			rec.Details.ResponseBody = &CapturedBody{Size: 7, CapturedSize: 7, Evicted: true}
			m.Upsert(rec)
		case 6:
			rec := inlineBodied(id, ts)
			rec.Details.ResponseBody = &CapturedBody{Size: 9, CapturedSize: 9, FilePath: "/tmp/nonexistent-" + id}
			m.Upsert(rec)
		case 7:
			m.Upsert(RequestRecord{ID: id, Timestamp: ts, Method: "GET", URL: "/slow", InFlight: true})
			inFlight = append(inFlight, pending{id: id, ts: ts})
		case 8:
			if len(inFlight) > 0 {
				pick := rng.Intn(len(inFlight))
				done := inFlight[pick]
				inFlight = append(inFlight[:pick], inFlight[pick+1:]...)
				completion := inlineBodied(done.id, done.ts)
				completion.StatusCode = 200
				m.Upsert(completion)
			}
		case 9:
			// Duplicate explicit ID: Record permits it, Upsert would no-op.
			dup := inlineBodied(fmt.Sprintf("dup%d", rng.Intn(3)), ts)
			dup.ProjectDir = "/p/b"
			m.Record(dup)
		case 10:
			rec := inlineBodied(id, ts)
			rec.ProjectDir = "/p/b"
			m.Upsert(rec)
		case 11:
			if rng.Intn(20) == 0 {
				m.PurgeByProject("/p/b")
			} else {
				m.Upsert(inlineBodied(id, ts))
			}
		}

		m.checkBodyWindowInvariant(t)
		m.checkIndexInvariant(t)
		if t.Failed() {
			t.Fatalf("invariant broken at step %d", step)
		}
	}
}
