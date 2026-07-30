package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
)

// --- helpers ---

// wireRequest builds one wire-format record as the REST snapshot and the SSE
// stream both deliver it.
func wireRequest(id string, status int, inFlight bool) api.ProxyRequestResponse {
	return api.ProxyRequestResponse{
		ID:         id,
		Timestamp:  time.Unix(0, 0).Format(time.RFC3339Nano),
		Method:     "GET",
		URL:        "/" + id,
		StatusCode: status,
		InFlight:   inFlight,
	}
}

// syncRecord builds one already-converted record, for seeding a model's list
// and for building RequestsSyncMsg payloads directly.
func syncRecord(id string, status int, inFlight bool) proxy.RequestRecord {
	return proxy.RequestRecord{
		ID:         id,
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/" + id,
		StatusCode: status,
		InFlight:   inFlight,
	}
}

// requestIDs lists the model's rows in order, for duplicate/ordering assertions.
func requestIDs(m ClientModel) []string {
	ids := make([]string, 0, len(m.proxyRequests))
	for _, req := range m.proxyRequests {
		ids = append(ids, req.ID)
	}
	return ids
}

// findRequest returns the row with the given ID.
func findRequest(t *testing.T, m ClientModel, id string) proxy.RequestRecord {
	t.Helper()
	for _, req := range m.proxyRequests {
		if req.ID == id {
			return req
		}
	}
	t.Fatalf("no row with id %q; have %v", id, requestIDs(m))
	return proxy.RequestRecord{}
}

// syncModel builds an attach-mode model in the requests view with no rows.
func syncModel() ClientModel {
	return newClientRequestsModel(&stubTUIClient{}, 0, 10)
}

// --- merge / interleaving permutations (Update-driven) ---

// TestRequestsSync_SnapshotInFlightCannotRegressLiveFinal covers the ordering
// that snapshot replay makes unavoidable: the completion arrived live before
// the sync, and the snapshot (taken earlier, server-side) still shows the
// request in flight. Final is terminal, so the row must not regress.
func TestRequestsSync_SnapshotInFlightCannotRegressLiveFinal(t *testing.T) {
	m := syncModel()
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("req-1", 200, false)))

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("req-1", 0, true)},
	})

	assert.Equal(t, []string{"req-1"}, requestIDs(m), "no duplicate row")
	row := findRequest(t, m, "req-1")
	assert.False(t, row.InFlight, "a snapshot's stale in-flight copy must not regress a final row")
	assert.Equal(t, 200, row.StatusCode)
}

// TestRequestsSync_BufferedFinalWinsOverSnapshotInFlight covers the same race
// the other way round: the completion arrived DURING the fetch, so it rides in
// Buffered and is applied after the snapshot's in-flight copy.
func TestRequestsSync_BufferedFinalWinsOverSnapshotInFlight(t *testing.T) {
	m := syncModel()

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("req-1", 0, true)},
		Buffered: []proxy.RequestRecord{syncRecord("req-1", 204, false)},
	})

	assert.Equal(t, []string{"req-1"}, requestIDs(m))
	row := findRequest(t, m, "req-1")
	assert.False(t, row.InFlight)
	assert.Equal(t, 204, row.StatusCode)
}

// TestRequestsSync_SnapshotFinalSurvivesBufferedInFlight is the inverse: a
// duplicate in-flight event buffered behind a snapshot that already has the
// completion cannot un-finish it.
func TestRequestsSync_SnapshotFinalSurvivesBufferedInFlight(t *testing.T) {
	m := syncModel()

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("req-1", 200, false)},
		Buffered: []proxy.RequestRecord{syncRecord("req-1", 0, true)},
	})

	row := findRequest(t, m, "req-1")
	assert.False(t, row.InFlight, "final is terminal")
	assert.Equal(t, 200, row.StatusCode)
}

// TestRequestsSync_LiveArrivalDuringSyncAppearsOnce pins that a request that
// started during the fetch — present only in the buffer — lands exactly once.
func TestRequestsSync_LiveArrivalDuringSyncAppearsOnce(t *testing.T) {
	m := syncModel()

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("old-1", 200, false)},
		Buffered: []proxy.RequestRecord{syncRecord("new-1", 0, true), syncRecord("new-1", 201, false)},
	})

	assert.Equal(t, []string{"old-1", "new-1"}, requestIDs(m))
	assert.Equal(t, 201, findRequest(t, m, "new-1").StatusCode)
}

// TestRequestsSync_DuplicateIDsAcrossSnapshotAndBufferDoNotDuplicate pins the
// no-duplicates property over a batch where every buffered event also appears
// in the snapshot (the common case: the fetch raced the same events).
func TestRequestsSync_DuplicateIDsAcrossSnapshotAndBufferDoNotDuplicate(t *testing.T) {
	m := syncModel()

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{
			syncRecord("req-1", 200, false),
			syncRecord("req-2", 0, true),
			syncRecord("req-3", 0, true),
		},
		Buffered: []proxy.RequestRecord{
			syncRecord("req-2", 0, true),
			syncRecord("req-3", 500, false),
			syncRecord("req-1", 200, false),
		},
	})

	assert.Equal(t, []string{"req-1", "req-2", "req-3"}, requestIDs(m))
	assert.True(t, findRequest(t, m, "req-2").InFlight, "still in flight")
	assert.Equal(t, 500, findRequest(t, m, "req-3").StatusCode, "completion applied")
}

// TestRequestsSync_SnapshotAppliesOldestFirst pins the replay order: the list
// must end up in the same order the live stream would have produced.
func TestRequestsSync_SnapshotAppliesOldestFirst(t *testing.T) {
	send := func(tea.Msg) {}
	// The REST endpoint returns newest-first.
	records := snapshotRecords(send, []api.ProxyRequestResponse{
		wireRequest("req-3", 200, false),
		wireRequest("req-2", 200, false),
		wireRequest("req-1", 200, false),
	})

	m := clientUpdate(syncModel(), RequestsSyncMsg{Snapshot: records})
	assert.Equal(t, []string{"req-1", "req-2", "req-3"}, requestIDs(m))
}

// TestRequestsSync_BatchRendersOnce is deliberately absent as an instrumented
// assertion: BaseModel exposes no render counter and adding one purely for the
// test would put test-only state on the production model. The property is
// structural instead — handleRequestsSync has exactly ONE call to
// renderAfterProxyRequests, after the whole batch is merged — and the tests
// above pin that the batch's end state matches record-by-record application.

// --- cursor / follow semantics across a batch ---

// TestRequestsSync_CursorSurvivesBatch pins that a user parked on a row keeps
// it across a sync batch: the cursor is ID-anchored, so rows arriving before
// and after it cannot drag it.
func TestRequestsSync_CursorSurvivesBatch(t *testing.T) {
	m := newClientRequestsModel(&stubTUIClient{}, 5, 10)
	m = clientUpdate(m, keyRune('g')) // cursor row 0, follow off
	m = clientUpdate(m, keyRune('j')) // cursor row 1 (req-001)
	require.Equal(t, "req-001", m.cursorID)
	require.False(t, m.followMode)

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("snap-1", 200, false)},
		Buffered: []proxy.RequestRecord{syncRecord("live-1", 200, false)},
	})

	assert.Equal(t, "req-001", m.cursorID, "the ID-anchored cursor survives the batch")
	assert.Equal(t, 1, m.cursorIdx, "and keeps its index: the batch only appended")
	assert.False(t, m.followMode, "a sync batch never re-engages follow")
}

// TestRequestsSync_FollowPinsToNewestAfterBatch pins the other half: with
// follow engaged, the cursor lands on the newest row of the merged list.
func TestRequestsSync_FollowPinsToNewestAfterBatch(t *testing.T) {
	m := newClientRequestsModel(&stubTUIClient{}, 3, 10)
	require.True(t, m.followMode)

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("snap-1", 200, false)},
		Buffered: []proxy.RequestRecord{syncRecord("live-1", 200, false)},
	})

	assert.Equal(t, "live-1", m.cursorID, "follow pins to the newest row")
	assert.Equal(t, len(m.proxyRequests)-1, m.cursorIdx)
}

// --- prior-epoch sweep ---

// TestRequestsSync_PriorEpochInFlightSweptStale pins that an in-flight row the
// synchronized server has never heard of is marked stale immediately: its
// daemon is gone, so no completion can ever arrive and the row would otherwise
// spin "...ms" forever.
func TestRequestsSync_PriorEpochInFlightSweptStale(t *testing.T) {
	m := syncModel()
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("orphan", 0, true)))
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("done", 200, false)))
	require.False(t, m.requestIsStale(findRequest(t, m, "orphan")), "not stale before the sync")

	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("known", 0, true)},
		Buffered: []proxy.RequestRecord{syncRecord("fresh", 0, true)},
	})

	assert.True(t, m.requestIsStale(findRequest(t, m, "orphan")), "swept: absent from the snapshot")
	assert.False(t, m.requestIsStale(findRequest(t, m, "known")), "in the snapshot")
	assert.False(t, m.requestIsStale(findRequest(t, m, "fresh")), "started during the fetch")
	assert.False(t, m.requestIsStale(findRequest(t, m, "done")), "final rows are never stale")
	assert.Contains(t, m.View(), "stale?", "the sweep is visible in the requests list")
}

// TestRequestsSync_SweptRowUnmarkedByLateCompletion pins the mark is not
// permanent: a completion that does arrive clears it.
func TestRequestsSync_SweptRowUnmarkedByLateCompletion(t *testing.T) {
	m := syncModel()
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("orphan", 0, true)))
	m = clientUpdate(m, RequestsSyncMsg{})
	require.True(t, m.requestIsStale(findRequest(t, m, "orphan")))

	m = clientUpdate(m, ProxyRequestMsg(syncRecord("orphan", 200, false)))
	assert.False(t, m.requestIsStale(findRequest(t, m, "orphan")))
}

// --- attempt-level sync protocol ---

// newRequestsSyncHarness drives consumeRequestsWithSync attempts. See
// syncHarness (stream_health_test.go) for the machinery.
func newRequestsSyncHarness() *syncHarness {
	return newSyncHarness(consumeRequestsWithSync)
}

// TestConsumeRequestsWithSync_DeliversBatchThenMarksSynced pins the barrier:
// events seen before the snapshot land are BUFFERED into the one
// RequestsSyncMsg (never delivered loose), markSynced fires only after that
// message, and later events pass straight through.
func TestConsumeRequestsWithSync_DeliversBatchThenMarksSynced(t *testing.T) {
	h := newRequestsSyncHarness()
	buffered := make(chan struct{})
	client := &stubTUIClient{
		snapshot: []api.ProxyRequestResponse{wireRequest("snap-1", 200, false)},
	}
	client.consumeRequests = func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
		onConnect()
		onEvent(wireRequest("live-1", 0, true)) // buffered: the fetch is held below
		close(buffered)
		// Once the sync barrier is crossed, a further event must arrive as a
		// plain ProxyRequestMsg rather than being buffered.
		select {
		case <-h.syncedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		onEvent(wireRequest("live-2", 200, false))
		<-ctx.Done()
		return ctx.Err()
	}
	// The fetch may only return once the first live event has been buffered.
	client.snapshotCall = func(int, domain.ProxyRequestParams) { <-buffered }

	ctx, cancel := context.WithCancel(context.Background())
	errCh := h.runInBackground(ctx, client)

	syncMsg := awaitSync[RequestsSyncMsg](t, h)
	assert.Equal(t, []string{"snap-1"}, wireIDs(syncMsg.Snapshot))
	assert.Equal(t, []string{"live-1"}, wireIDs(syncMsg.Buffered))

	// Wait for the passthrough event before tearing the attempt down.
	h.collector.await(t, func(m tea.Msg) bool {
		req, ok := m.(ProxyRequestMsg)
		return ok && req.ID == "live-2"
	})
	cancel()
	<-errCh

	assert.Equal(t, int32(1), h.syncedN.Load(), "markSynced fires exactly once, after the batch")

	msgs := h.collector.all()
	require.NotEmpty(t, msgs)
	_, first := msgs[0].(RequestsSyncMsg)
	assert.True(t, first, "no loose ProxyRequestMsg before the sync batch; got %T", msgs[0])

	var passthrough []string
	for _, msg := range msgs[1:] {
		if req, ok := msg.(ProxyRequestMsg); ok {
			passthrough = append(passthrough, req.ID)
		}
	}
	assert.Equal(t, []string{"live-2"}, passthrough, "post-sync events pass straight through")
}

// TestConsumeRequestsWithSync_BufferOverflowAbortsAttempt pins the
// nothing-silently-lost rule: a buffer that fills before the snapshot lands
// ends the attempt with a distinct, retryable error rather than dropping
// events, and the next attempt re-syncs cleanly.
func TestConsumeRequestsWithSync_BufferOverflowAbortsAttempt(t *testing.T) {
	h := newRequestsSyncHarness()
	overflowed := make(chan struct{})
	client := &stubTUIClient{}
	client.consumeRequests = func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
		onConnect()
		for i := 0; i <= constants.MaxProxyRequests; i++ {
			onEvent(wireRequest(fmt.Sprintf("req-%04d", i), 200, false))
		}
		close(overflowed)
		<-ctx.Done()
		return ctx.Err()
	}
	// Hold the snapshot until the buffer has overflowed.
	client.snapshotCall = func(int, domain.ProxyRequestParams) { <-overflowed }

	err := h.run(context.Background(), client)

	require.ErrorIs(t, err, errRequestsSyncOverflow)
	assert.Zero(t, h.syncedN.Load(), "an overflowed attempt never reports OK")
	assert.Equal(t, stream.ClassTransient, classifyRequestsStreamError(err),
		"the loop must reconnect and re-sync on overflow")
	for _, msg := range h.collector.all() {
		_, isSync := msg.(RequestsSyncMsg)
		assert.False(t, isSync, "an overflowed attempt delivers no batch")
	}

	// A re-attempt against a healthy stream syncs normally.
	retryH := newRequestsSyncHarness()
	retry := &stubTUIClient{snapshot: []api.ProxyRequestResponse{wireRequest("snap-1", 200, false)}}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := retryH.runInBackground(ctx, retry)
	assert.Equal(t, []string{"snap-1"}, wireIDs(awaitSync[RequestsSyncMsg](t, retryH).Snapshot))
	cancel()
	<-errCh
	assert.Equal(t, int32(1), retryH.syncedN.Load())
}

// TestConsumeRequestsWithSync_SnapshotFetchRetriesThenRecycles pins the
// fetch-failure policy: retry in place while the stream is live (staying
// unsynced the whole time), then abort the attempt after
// requestsSnapshotFetchAttempts so the loop's own backoff takes over.
func TestConsumeRequestsWithSync_SnapshotFetchRetriesThenRecycles(t *testing.T) {
	client := &stubTUIClient{
		snapshotErr: errors.New("snapshot boom"),
		consumeRequests: func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
			onConnect()
			<-ctx.Done()
			return ctx.Err()
		},
	}

	h := newRequestsSyncHarness()
	err := h.run(context.Background(), client)

	assert.Equal(t, requestsSnapshotFetchAttempts, client.snapshotCalls(), "retries in place")
	assert.Zero(t, h.syncedN.Load(), "stays unsynced (Syncing) for the whole retry window")
	require.Error(t, err)
	assert.ErrorContains(t, err, "snapshot boom")
	assert.ErrorContains(t, err, "snapshot fetch failed")
	assert.Equal(t, stream.ClassTransient, classifyRequestsStreamError(err))
}

// TestConsumeRequestsWithSync_StreamErrorSurvivesTheSync pins that a stream
// that simply dies reports ITS error, not a cancellation manufactured by the
// sync teardown.
func TestConsumeRequestsWithSync_StreamErrorSurvivesTheSync(t *testing.T) {
	client := &stubTUIClient{
		consumeRequests: func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
			onConnect()
			return errors.New("connection reset")
		},
	}

	h := newRequestsSyncHarness()
	assert.EqualError(t, h.run(context.Background(), client), "connection reset")
}

// TestConsumeRequestsWithSync_DeadDialNeverSyncs pins that an attempt that
// never connects never fetches and never reports OK.
func TestConsumeRequestsWithSync_DeadDialNeverSyncs(t *testing.T) {
	client := &stubTUIClient{
		consumeRequests: func(context.Context, func(), func(api.ProxyRequestResponse)) error {
			return errors.New("connection refused")
		},
	}

	h := newRequestsSyncHarness()
	assert.EqualError(t, h.run(context.Background(), client), "connection refused")
	assert.Equal(t, 0, client.snapshotCalls(), "no snapshot fetch without a connection")
	assert.Zero(t, h.syncedN.Load())
}

// --- loop-level wiring ---

// TestRunClientStreams_RequestsOKOnlyAfterSync pins the whole point of the
// protocol at the loop level: the requests stream reports OK only once the
// snapshot has been applied, never between connect and sync completion.
func TestRunClientStreams_RequestsOKOnlyAfterSync(t *testing.T) {
	collector := newMsgCollector()
	release := make(chan struct{})
	client := &stubTUIClient{
		snapshot: []api.ProxyRequestResponse{wireRequest("snap-1", 200, false)},
		consumeRequests: func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
			onConnect()
			<-ctx.Done()
			return ctx.Err()
		},
	}
	client.snapshotCall = func(int, domain.ProxyRequestParams) { <-release }

	startClientStreams(t, client, collector.send)

	// Before the snapshot lands the requests loop must not be OK.
	time.Sleep(50 * time.Millisecond)
	for _, msg := range collector.all() {
		if s, ok := msg.(StreamStatusMsg); ok && s.Stream == StreamRequests {
			assert.NotEqual(t, stream.StateOK, s.Status.State, "OK before the snapshot applied")
		}
	}

	close(release)
	collector.await(t, func(m tea.Msg) bool {
		s, ok := m.(StreamStatusMsg)
		return ok && s.Stream == StreamRequests && s.Status.State == stream.StateOK
	})

	// ...and the batch reached the models before that OK.
	var syncIdx, okIdx = -1, -1
	for i, msg := range collector.all() {
		switch v := msg.(type) {
		case RequestsSyncMsg:
			if syncIdx < 0 {
				syncIdx = i
			}
		case StreamStatusMsg:
			if v.Stream == StreamRequests && v.Status.State == stream.StateOK && okIdx < 0 {
				okIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, syncIdx, 0, "the sync batch must be delivered")
	require.GreaterOrEqual(t, okIdx, 0)
	assert.Less(t, syncIdx, okIdx, "the batch is delivered before the loop reports OK")
}

// --- small helpers used above ---

func wireIDs(records []proxy.RequestRecord) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestRequestsSync_CompletionViaSyncRefreshesOpenDetail pins D16 for the sync
// path (cursor C6 review): a detail view open on an in-flight request whose
// completion arrives inside a sync batch — not as a live event — must refetch,
// exactly as the live-completion path does.
func TestRequestsSync_CompletionViaSyncRefreshesOpenDetail(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientRequestsModel(stub, 0, 10)
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("req-1", 0, true)))
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.requestDetail = inFlightDetailFor("req-1")
	seqBefore := m.detailFetchSeq

	nm, cmd := m.Update(RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("req-1", 200, false)},
	})
	m = nm.(ClientModel)

	require.NotNil(t, cmd, "sync-borne completion must trigger a detail refetch")
	assert.Equal(t, seqBefore+1, m.detailFetchSeq)
	// Executing the command performs the fetch against the selected row.
	cmd()
	assert.Equal(t, "req-1", stub.lastRequestedID())
}

// TestRequestsSync_FinalDetailNotRefetchedOnResync pins the guard the sync path
// adds over the live path: a detail already showing a final response is not
// refetched by every routine reconnect re-sync that includes its row.
func TestRequestsSync_FinalDetailNotRefetchedOnResync(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientRequestsModel(stub, 0, 10)
	m = clientUpdate(m, ProxyRequestMsg(syncRecord("req-1", 200, false)))
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.requestDetail = finalDetailFor("req-1")
	seqBefore := m.detailFetchSeq

	nm, cmd := m.Update(RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{syncRecord("req-1", 200, false)},
	})
	m = nm.(ClientModel)

	assert.Nil(t, cmd, "an already-final detail must not refetch on re-sync")
	assert.Equal(t, seqBefore, m.detailFetchSeq)
}
