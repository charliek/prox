package proxy

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/constants"
)

// --- Captured-body detail window (D9b) ---
//
// Retention (ring capacity) and BODY retention are separate bounds: the ring
// keeps constants.DefaultProxyRequestBufferSize records, but only
// constants.ProxyRequestDetailWindow of them keep their captured bodies. Two
// mechanisms share that constant: a capture-owning ring (standalone, the
// daemon's per-project ring) bounds bodies by ring POSITION — this file's
// concern below. The forwarder-fed replica has no capture of its own to
// strip on the daemon's behalf, so it bounds retained INLINE body data by
// daemon-supplied TIMESTAMP instead; see requests_body_window_test.go for
// that mechanism's own suite, and
// TestReplicaRequestManager_BoundsInlineBodiesAtRealConstants below for its
// shipping-constant coverage.

// bodyRetained reports whether a captured body still holds servable data.
func bodyRetained(b *CapturedBody) bool {
	return b != nil && (len(b.Data) > 0 || b.FilePath != "")
}

// detailedRecords counts ring records that still hold captured body data.
func detailedRecords(recs []RequestRecord) int {
	n := 0
	for _, rec := range recs {
		if rec.Details == nil {
			continue
		}
		if bodyRetained(rec.Details.RequestBody) || bodyRetained(rec.Details.ResponseBody) {
			n++
		}
	}
	return n
}

// TestRequestManager_DetailWindow_BoundsBodiesAtRealConstants exercises the
// window at the SHIPPING constants: a ring sized DefaultProxyRequestBufferSize
// with the default ProxyRequestDetailWindow. More records than the window are
// appended, each carrying an inline body and headers. The window must hold: at
// most ProxyRequestDetailWindow records keep body data, every older record keeps
// its metadata and headers, and a body fetch for an evicted record reports the
// same "evicted" signal a missing spilled file produces.
func TestRequestManager_DetailWindow_BoundsBodiesAtRealConstants(t *testing.T) {
	const extra = 200
	total := constants.ProxyRequestDetailWindow + extra
	require.Less(t, total, constants.DefaultProxyRequestBufferSize,
		"this test must exercise the DETAIL window, not ring eviction")

	m := NewRequestManager(constants.DefaultProxyRequestBufferSize)

	for i := 0; i < total; i++ {
		m.Record(RequestRecord{
			ID:     fmt.Sprintf("r%04d", i),
			Method: "GET",
			URL:    fmt.Sprintf("/x/%d", i),
			Details: &RequestDetails{
				RequestHeaders:  map[string][]string{"X-Req": {fmt.Sprintf("%d", i)}},
				ResponseHeaders: map[string][]string{"X-Res": {fmt.Sprintf("%d", i)}},
				RequestBody:     &CapturedBody{Size: 4, CapturedSize: 4, Data: []byte("body")},
				ResponseBody:    &CapturedBody{Size: 4, CapturedSize: 4, Data: []byte("resp")},
			},
		})
	}

	require.Equal(t, total, m.Count(), "every record is retained; only bodies are evicted")

	all := m.Recent(RequestFilter{Limit: total})
	require.Len(t, all, total)
	assert.Equal(t, constants.ProxyRequestDetailWindow, detailedRecords(all),
		"exactly the newest ProxyRequestDetailWindow records keep body data")

	// The newest window's worth still serves bodies.
	for i := 0; i < constants.ProxyRequestDetailWindow; i++ {
		rec := all[i]
		require.NotNil(t, rec.Details, "record %s must keep Details", rec.ID)
		require.True(t, bodyRetained(rec.Details.ResponseBody), "in-window record %s lost its body", rec.ID)
		assert.False(t, rec.Details.ResponseBody.Evicted)
	}

	// Everything older kept metadata and headers, lost only the payload.
	for i := constants.ProxyRequestDetailWindow; i < total; i++ {
		rec := all[i]
		require.NotNil(t, rec.Details, "evicted record %s must keep Details", rec.ID)
		assert.NotEmpty(t, rec.Details.RequestHeaders, "headers are KEPT past the detail window")
		assert.NotEmpty(t, rec.Details.ResponseHeaders, "headers are KEPT past the detail window")
		assert.NotEmpty(t, rec.URL, "record metadata is KEPT past the detail window")

		for name, body := range map[string]*CapturedBody{
			"request":  rec.Details.RequestBody,
			"response": rec.Details.ResponseBody,
		} {
			require.NotNil(t, body, "%s body metadata is kept, only its data goes", name)
			assert.True(t, body.Evicted, "%s body of %s must be marked evicted", name, rec.ID)
			assert.Nil(t, body.Data, "%s body of %s must hold no inline data", name, rec.ID)
			assert.Empty(t, body.FilePath, "%s body of %s must hold no file path", name, rec.ID)
			assert.Equal(t, int64(4), body.Size, "%s body SIZE metadata survives", name)

			// The serve path reports the same reason a missing spilled file does.
			decoded, err := LoadDecodedBody(body, nil)
			require.NoError(t, err, "an evicted body is a benign condition, not an error")
			assert.False(t, decoded.Available)
			assert.Equal(t, "evicted", decoded.UnavailableReason)
		}
	}
}

// TestRequestManager_DetailWindow_UnlinksSpilledFiles pins that leaving the
// window is not merely a metadata flag: a record's spilled body files are
// actually removed from disk (through the same eviction callback ring eviction
// uses), while in-window records' files are left alone.
func TestRequestManager_DetailWindow_UnlinksSpilledFiles(t *testing.T) {
	cm := newEnabledCaptureManager(t)

	const window = 2
	m := newRequestManagerWithDetailWindow(10, window)
	m.SetEvictionCallback(cm.CleanupRequest)

	const n = 5
	spilled := make([]struct{ id, reqPath, resPath string }, n)
	for i := range spilled {
		s := &spilled[i]
		s.id = fmt.Sprintf("spill%d", i)
		s.reqPath = spillFilePath(cm.captureDir, s.id, "_req")
		s.resPath = spillFilePath(cm.captureDir, s.id, "_res")
		require.NoError(t, os.WriteFile(s.reqPath, []byte("req"), 0o600))
		require.NoError(t, os.WriteFile(s.resPath, []byte("res"), 0o600))

		m.Record(RequestRecord{
			ID:     s.id,
			Method: "POST",
			URL:    "/upload",
			Details: &RequestDetails{
				RequestHeaders: map[string][]string{"X-Req": {s.id}},
				RequestBody:    &CapturedBody{Size: 3, CapturedSize: 3, FilePath: s.reqPath},
				ResponseBody:   &CapturedBody{Size: 3, CapturedSize: 3, FilePath: s.resPath},
			},
		})
	}

	require.Equal(t, n, m.Count(), "no ring eviction here — capacity is 10")
	assert.Equal(t, window, detailedRecords(m.Recent(RequestFilter{Limit: n})))

	for i, s := range spilled {
		inWindow := i >= n-window

		rec, ok := m.GetByID(s.id)
		require.True(t, ok)
		require.NotNil(t, rec.Details)
		assert.NotEmpty(t, rec.Details.RequestHeaders, "%s must keep its headers", s.id)

		for _, path := range []string{s.reqPath, s.resPath} {
			assert.Equal(t, inWindow, fileExists(t, path),
				"record %s (inWindow=%v): wrong on-disk state for %s", s.id, inWindow, path)
		}
	}

	// The accountant's byte total tracks the unlinks too (nothing was tracked
	// here since the files were written directly, but the call must be safe).
	assert.Zero(t, cm.DiskUsed())
}

// TestRequestManager_DetailWindow_InFlightCompletionOutsideWindowKeepsNoBody
// pins the in-flight decision: a request slow enough that its ring slot ages out
// of the detail window before the response finishes does NOT get its bodies
// stored when the completion lands. Replace-in-place would otherwise resurrect
// body data outside the window. Subscribers are notified with the stripped
// record, so the wire never advertises data the ring refuses to serve, and the
// completion's spilled file is unlinked immediately.
func TestRequestManager_DetailWindow_InFlightCompletionOutsideWindowKeepsNoBody(t *testing.T) {
	cm := newEnabledCaptureManager(t)

	m := newRequestManagerWithDetailWindow(10, 2)
	m.SetEvictionCallback(cm.CleanupRequest)
	sub := m.Subscribe(RequestFilter{})
	defer m.Unsubscribe(sub.ID)

	// A slow request publishes an in-flight record (no Details yet).
	require.True(t, m.Upsert(RequestRecord{ID: "slow", Method: "GET", URL: "/slow", InFlight: true}))

	// Two later requests push "slow" to offset 2 — outside the window of 2.
	require.True(t, m.Upsert(RequestRecord{ID: "n1", Method: "GET", URL: "/1"}))
	require.True(t, m.Upsert(RequestRecord{ID: "n2", Method: "GET", URL: "/2"}))

	resPath := spillFilePath(cm.captureDir, "slow", "_res")
	require.NoError(t, os.WriteFile(resPath, []byte("big response"), 0o600))

	require.True(t, m.Upsert(RequestRecord{
		ID:         "slow",
		Method:     "GET",
		URL:        "/slow",
		StatusCode: 200,
		Details: &RequestDetails{
			RequestHeaders:  map[string][]string{"X-Req": {"slow"}},
			ResponseHeaders: map[string][]string{"X-Res": {"slow"}},
			ResponseBody:    &CapturedBody{Size: 12, CapturedSize: 12, FilePath: resPath},
		},
	}))

	rec, ok := m.GetByID("slow")
	require.True(t, ok)
	assert.False(t, rec.InFlight, "the completion still applies — only its body is dropped")
	assert.Equal(t, 200, rec.StatusCode, "completion metadata is kept")
	require.NotNil(t, rec.Details)
	assert.NotEmpty(t, rec.Details.ResponseHeaders, "completion headers are kept")
	require.NotNil(t, rec.Details.ResponseBody)
	assert.True(t, rec.Details.ResponseBody.Evicted)
	assert.Empty(t, rec.Details.ResponseBody.FilePath)
	assert.Nil(t, rec.Details.ResponseBody.Data)

	assert.False(t, fileExists(t, resPath), "the completion's spilled file must be unlinked")

	// The window invariant still holds across the whole ring.
	assert.Equal(t, 0, detailedRecords(m.Recent(RequestFilter{Limit: 10})),
		"no record in this ring ever carried an in-window body")

	// Subscribers saw the stripped completion, never the body it arrived with.
	var completion *RequestRecord
	for i := 0; i < 4; i++ {
		got := readRecordEvent(t, sub.Ch)
		if got.ID == "slow" && !got.InFlight {
			r := got
			completion = &r
		}
	}
	require.NotNil(t, completion, "the completion must still be published")
	require.NotNil(t, completion.Details)
	require.NotNil(t, completion.Details.ResponseBody)
	assert.True(t, completion.Details.ResponseBody.Evicted,
		"the notified record must not advertise body data the ring dropped")
	assert.Empty(t, completion.Details.ResponseBody.FilePath)
}

// TestRequestManager_DetailWindow_InFlightCompletionInsideWindowKeepsBody is the
// negative control for the test above: the eviction must be scoped to
// out-of-window slots, not applied to every completion.
func TestRequestManager_DetailWindow_InFlightCompletionInsideWindowKeepsBody(t *testing.T) {
	m := newRequestManagerWithDetailWindow(10, 3)

	require.True(t, m.Upsert(RequestRecord{ID: "slow", Method: "GET", URL: "/slow", InFlight: true}))
	require.True(t, m.Upsert(RequestRecord{ID: "n1", Method: "GET", URL: "/1"}))

	require.True(t, m.Upsert(RequestRecord{
		ID: "slow", Method: "GET", URL: "/slow", StatusCode: 200,
		Details: &RequestDetails{ResponseBody: &CapturedBody{Size: 4, CapturedSize: 4, Data: []byte("resp")}},
	}))

	rec, ok := m.GetByID("slow")
	require.True(t, ok)
	require.NotNil(t, rec.Details)
	require.NotNil(t, rec.Details.ResponseBody)
	assert.False(t, rec.Details.ResponseBody.Evicted)
	assert.Equal(t, []byte("resp"), rec.Details.ResponseBody.Data,
		"a completion inside the window keeps its body")
}

// TestRequestManager_DetailWindow_DoesNotMutateHandedOutRecords pins the
// concurrency-safety choice behind evictBodies: it allocates fresh Details and
// CapturedBody values instead of mutating the ones the ring already handed out.
// Subscribers, Recent, and GetByID all return record COPIES that SHARE those
// pointers, so in-place mutation would race a reader serializing the record
// outside the ring lock.
func TestRequestManager_DetailWindow_DoesNotMutateHandedOutRecords(t *testing.T) {
	m := newRequestManagerWithDetailWindow(10, 1)

	m.Record(RequestRecord{
		ID: "a", Method: "GET", URL: "/a",
		Details: &RequestDetails{ResponseBody: &CapturedBody{Size: 4, CapturedSize: 4, Data: []byte("keep")}},
	})

	// A reader's snapshot, taken while "a" is still in the window.
	snapshot, ok := m.GetByID("a")
	require.True(t, ok)
	require.NotNil(t, snapshot.Details.ResponseBody)

	// Push "a" out of the window.
	m.Record(RequestRecord{ID: "b", Method: "GET", URL: "/b"})

	assert.Equal(t, []byte("keep"), snapshot.Details.ResponseBody.Data,
		"the already-handed-out snapshot must not be mutated by eviction")
	assert.False(t, snapshot.Details.ResponseBody.Evicted)

	fresh, ok := m.GetByID("a")
	require.True(t, ok)
	assert.True(t, fresh.Details.ResponseBody.Evicted, "the ring's own copy IS evicted")
}

// TestRequestManager_DetailWindow_LeavesBodylessRecordsAlone pins that eviction
// only touches bodies that actually held data: a bodyless GET's empty captured
// body is not relabeled "evicted" (which would tell the user data was lost when
// there never was any), and a record with no Details at all is untouched.
func TestRequestManager_DetailWindow_LeavesBodylessRecordsAlone(t *testing.T) {
	m := newRequestManagerWithDetailWindow(10, 1)

	m.Record(RequestRecord{
		ID: "empty", Method: "GET", URL: "/empty",
		Details: &RequestDetails{
			RequestHeaders: map[string][]string{"X": {"y"}},
			RequestBody:    &CapturedBody{Size: 0, CapturedSize: 0},
		},
	})
	m.Record(RequestRecord{ID: "nodetails", Method: "GET", URL: "/n"})
	m.Record(RequestRecord{ID: "newest", Method: "GET", URL: "/z"})

	empty, ok := m.GetByID("empty")
	require.True(t, ok)
	require.NotNil(t, empty.Details.RequestBody)
	assert.False(t, empty.Details.RequestBody.Evicted,
		"a body that never held data must not be reported as evicted")

	decoded, err := LoadDecodedBody(empty.Details.RequestBody, nil)
	require.NoError(t, err)
	assert.True(t, decoded.Available, "an empty body is available-and-empty, not evicted")

	nodetails, ok := m.GetByID("nodetails")
	require.True(t, ok)
	assert.Nil(t, nodetails.Details)
}

// TestRequestManager_DetailWindow_DisabledKeepsEveryBody pins that a ring whose
// capacity does not exceed the window (every small test ring, and any explicit
// non-positive window) never evicts body data — the window is a bound, not a
// behavior change for small rings.
func TestRequestManager_DetailWindow_DisabledKeepsEveryBody(t *testing.T) {
	for _, tc := range []struct {
		name              string
		capacity, window  int
		records           int
		wantDetailedCount int
	}{
		{"window disabled", 5, 0, 5, 5},
		{"window larger than capacity", 5, 100, 5, 5},
		{"count never reaches the window", 10, 5, 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRequestManagerWithDetailWindow(tc.capacity, tc.window)
			for i := 0; i < tc.records; i++ {
				m.Record(RequestRecord{
					ID: fmt.Sprintf("r%d", i), Method: "GET", URL: "/x",
					Details: &RequestDetails{ResponseBody: &CapturedBody{Data: []byte("d")}},
				})
			}
			assert.Equal(t, tc.wantDetailedCount,
				detailedRecords(m.Recent(RequestFilter{Limit: tc.records})))
		})
	}
}

// TestRequestManager_DetailWindow_HoldsWhileRingWrapsAndPurges runs the window
// past the ring's own eviction boundary and through a PurgeByProject compaction:
// the invariant (at most `window` records hold bodies) must survive both, and
// compaction — which can only shrink a record's distance from the newest — must
// not resurrect bodies for records it pulls back INTO the window.
func TestRequestManager_DetailWindow_HoldsWhileRingWrapsAndPurges(t *testing.T) {
	const capacity, window = 8, 3
	m := newRequestManagerWithDetailWindow(capacity, window)

	for i := 0; i < capacity*3; i++ {
		project := "/p/a"
		if i%2 == 1 {
			project = "/p/b"
		}
		m.Record(RequestRecord{
			ID: fmt.Sprintf("r%02d", i), Method: "GET", URL: "/x", ProjectDir: project,
			Details: &RequestDetails{
				RequestHeaders: map[string][]string{"X": {"y"}},
				ResponseBody:   &CapturedBody{Size: 1, CapturedSize: 1, Data: []byte("d")},
			},
		})
		got := detailedRecords(m.Recent(RequestFilter{Limit: capacity}))
		require.LessOrEqual(t, got, window,
			"after %d records the ring must hold at most %d detailed records, held %d", i+1, window, got)
	}

	m.PurgeByProject("/p/b")

	remaining := m.Recent(RequestFilter{Limit: capacity})
	require.NotEmpty(t, remaining)
	assert.LessOrEqual(t, detailedRecords(remaining), window,
		"compaction must not resurrect evicted bodies")
	for _, rec := range remaining {
		require.NotNil(t, rec.Details)
		assert.NotEmpty(t, rec.Details.RequestHeaders, "%s kept its headers throughout", rec.ID)
	}
}

// TestReplicaRequestManager_BoundsInlineBodiesAtRealConstants exercises the
// replica variant — the forwarder-fed project-local ring in shared mode — at
// the SHIPPING constant. The replica runs no POSITION window; it bounds
// retained INLINE body data at ProxyRequestDetailWindow records ordered by the
// daemon-supplied Timestamp instead (see NewReplicaRequestManager, and
// requests_body_window_test.go for the mechanism's own suite).
//
// A replica needs its own bound because upstream stripping does not reach it:
// the daemon publishes no event when its window drops a body, so a live-
// forwarded record arrives WITH its body and would otherwise keep it forever.
func TestReplicaRequestManager_BoundsInlineBodiesAtRealConstants(t *testing.T) {
	const extra = 200
	total := constants.ProxyRequestDetailWindow + extra

	m := NewReplicaRequestManager(total) // > the default window, ring not full
	base := time.Now()

	for i := 0; i < total; i++ {
		m.Upsert(RequestRecord{
			ID:        fmt.Sprintf("r%04d", i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Method:    "GET",
			URL:       fmt.Sprintf("/x/%d", i),
			Details: &RequestDetails{
				RequestHeaders: map[string][]string{"X-Req": {fmt.Sprintf("%d", i)}},
				RequestBody:    &CapturedBody{Data: []byte("payload"), Size: 7},
			},
		})
	}

	recs := m.Recent(RequestFilter{Limit: total})
	require.Len(t, recs, total, "the replica drops bodies, never records")
	assert.Equal(t, constants.ProxyRequestDetailWindow, inlineBodiedRecords(recs),
		"exactly one window's worth of records keep inline body data")

	for i, rec := range recs {
		require.NotNil(t, rec.Details)
		assert.NotEmpty(t, rec.Details.RequestHeaders, "%s keeps its headers either way", rec.ID)
		require.NotNil(t, rec.Details.RequestBody)
		if i < constants.ProxyRequestDetailWindow {
			assert.False(t, rec.Details.RequestBody.Evicted, "newest-by-timestamp record %s keeps its body", rec.ID)
			continue
		}
		assert.True(t, rec.Details.RequestBody.Evicted, "oldest-by-timestamp record %s is stripped", rec.ID)
		assert.Nil(t, rec.Details.RequestBody.Data)
	}
}
