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
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/stream"
)

// --- helpers ---

// wireLog builds one wire-format log entry as the REST backfill and the SSE
// stream both deliver it. seq 0 models a pre-C8 daemon's entry.
func wireLog(seq uint64, line string) api.LogEntryResponse {
	return api.LogEntryResponse{
		Timestamp: time.Unix(0, 0).UTC().Format(time.RFC3339Nano),
		Process:   "web",
		Stream:    "stdout",
		Line:      line,
		Seq:       seq,
	}
}

// backfill builds a LogsResponse carrying entries from seq lo through hi
// (inclusive, oldest-first), with the buffer bounds a server would report for a
// ring holding oldestSeq..hi.
func backfill(oldestSeq, lo, hi uint64) *api.LogsResponse {
	resp := &api.LogsResponse{OldestSeq: oldestSeq, LatestSeq: hi}
	for seq := lo; seq <= hi; seq++ {
		resp.Logs = append(resp.Logs, wireLog(seq, fmt.Sprintf("line-%d", seq)))
	}
	resp.FilteredCount = len(resp.Logs)
	resp.TotalCount = len(resp.Logs)
	return resp
}

// logLines lists a batch's lines in order, for ordering/duplication assertions.
func logLines(entries []domain.LogEntry) []string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Line)
	}
	return lines
}

// shortenLogsHandshakeWait shrinks the pre-C8 handshake budget for the duration
// of one test, so an old-server case costs milliseconds instead of seconds.
func shortenLogsHandshakeWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := logsHandshakeWait
	logsHandshakeWait = d
	t.Cleanup(func() { logsHandshakeWait = prev })
}

// newLogsSyncHarness drives consumeLogsWithSync attempts against ONE attach
// session, so a test can run several attempts against the cursor the previous
// one adopted — which is the whole point of the reconnect cases. See
// syncHarness (stream_health_test.go) for the machinery.
func newLogsSyncHarness(sess *logsSyncSession) *syncHarness {
	return newSyncHarness(func(ctx context.Context, client TUIClient, send func(tea.Msg), markSynced func()) error {
		return consumeLogsWithSync(ctx, client, sess, send, markSynced)
	})
}

// quietStream is the common script: connect, announce streamID, then hold the
// stream open until the attempt ends. Live entries are the interesting part of
// the other tests, so the ones that only care about the backfill use this.
func quietStream(streamID string) func(context.Context, func(), func(api.HandshakeResponse), func(api.LogEntryResponse)) error {
	return func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), _ func(api.LogEntryResponse)) error {
		onConnect()
		onHandshake(api.HandshakeResponse{StreamID: streamID})
		<-ctx.Done()
		return ctx.Err()
	}
}

// syncOnce runs one attempt against a quiet stream and returns its batch. The
// attempt is cancelled (and joined) before returning, so the session cursor it
// adopted is stable for the caller.
func syncOnce(t *testing.T, sess *logsSyncSession, client *stubTUIClient) LogsSyncMsg {
	t.Helper()
	h := newLogsSyncHarness(sess)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := h.runInBackground(ctx, client)
	msg := awaitSync[LogsSyncMsg](t, h)
	cancel()
	<-errCh
	return msg
}

// --- attempt-level sync protocol ---

// TestConsumeLogsWithSync_DeliversBatchThenMarksSynced pins the barrier on a
// FIRST sync: the backfill is fetched in full (no cursor to resume from),
// entries seen before it lands are BUFFERED into the one LogsSyncMsg rather
// than delivered loose, an entry the backfill already covered is dropped rather
// than duplicated, markSynced fires only after that message, and later entries
// pass straight through.
func TestConsumeLogsWithSync_DeliversBatchThenMarksSynced(t *testing.T) {
	sess := newLogsSyncSession()
	h := newLogsSyncHarness(sess)
	buffered := make(chan struct{})
	client := &stubTUIClient{}
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		// The fetch may only return once the live entries have been buffered.
		<-buffered
		return backfill(1, 1, 3), nil
	}
	client.consumeLogs = func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
		onConnect()
		onHandshake(api.HandshakeResponse{StreamID: "epoch-1"})
		onEvent(wireLog(3, "line-3")) // overlaps the backfill: must not duplicate
		onEvent(wireLog(4, "line-4")) // buffered: the fetch is held above
		close(buffered)
		// Once the sync barrier is crossed, a further entry must arrive as a
		// plain LogEntryMsg rather than being buffered.
		select {
		case <-h.syncedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		onEvent(wireLog(5, "line-5"))
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := h.runInBackground(ctx, client)

	syncMsg := awaitSync[LogsSyncMsg](t, h)
	assert.Equal(t, []string{"line-1", "line-2", "line-3", "line-4"}, logLines(syncMsg.Entries),
		"backfill oldest-first, then the buffered entries, with the overlap dropped")
	assert.Empty(t, syncMsg.Notice, "a first sync reports no discontinuity")

	// Wait for the passthrough entry before tearing the attempt down.
	h.collector.await(t, func(m tea.Msg) bool { return logMsgCarriesLine(m, "line-5") })
	cancel()
	<-errCh

	assert.Equal(t, int32(1), h.syncedN.Load(), "markSynced fires exactly once, after the batch")
	assert.Equal(t, []domain.LogParams{{Lines: maxLogEntries}}, client.logsCalls(),
		"a first sync is a full fetch of the TUI's own buffer size, with no cursor")

	msgs := h.collector.all()
	require.NotEmpty(t, msgs)
	_, first := msgs[0].(LogsSyncMsg)
	assert.True(t, first, "no loose LogEntryMsg before the sync batch; got %T", msgs[0])

	var passthrough []string
	for _, msg := range msgs[1:] {
		if entry, ok := msg.(LogEntryMsg); ok {
			passthrough = append(passthrough, domain.LogEntry(entry).Line)
		}
	}
	assert.Equal(t, []string{"line-5"}, passthrough, "post-sync entries pass straight through")

	lastSeq, streamID := sess.cursor()
	assert.Equal(t, uint64(5), lastSeq, "the cursor tracks the newest delivered entry")
	assert.Equal(t, "epoch-1", streamID)
}

// TestConsumeLogsWithSync_ReconnectResumesFromCursor pins the reconnect case
// the whole protocol exists for: the same epoch, a gap that still fits in the
// server's ring. The second attempt asks for exactly what it missed and gets
// exactly that — no duplicates of what it already holds, and no notice, because
// nothing was lost.
func TestConsumeLogsWithSync_ReconnectResumesFromCursor(t *testing.T) {
	sess := newLogsSyncSession()
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(n int, params domain.LogParams) (*api.LogsResponse, error) {
		if n == 1 {
			return backfill(1, 1, 3), nil
		}
		return backfill(1, params.SinceSeq+1, 5), nil
	}

	first := syncOnce(t, sess, client)
	require.Equal(t, []string{"line-1", "line-2", "line-3"}, logLines(first.Entries))

	second := syncOnce(t, sess, client)

	assert.Equal(t, []string{"line-4", "line-5"}, logLines(second.Entries),
		"exactly the entries missed while disconnected")
	assert.Empty(t, second.Notice, "a gap the ring still covers is not a loss")
	assert.Equal(t, []domain.LogParams{
		{Lines: maxLogEntries},
		{Lines: maxLogEntries, SinceSeq: 3},
	}, client.logsCalls(), "the second fetch resumes from the cursor")

	lastSeq, _ := sess.cursor()
	assert.Equal(t, uint64(5), lastSeq)
}

// TestConsumeLogsWithSync_RolledBufferReportsLostCount pins the honest-loss
// case: the server's ring rolled past our cursor while we were away, so the
// entries in between are gone for good and the batch carries a notice saying
// how many.
func TestConsumeLogsWithSync_RolledBufferReportsLostCount(t *testing.T) {
	sess := newLogsSyncSession()
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(n int, _ domain.LogParams) (*api.LogsResponse, error) {
		if n == 1 {
			return backfill(1, 1, 3), nil
		}
		// The ring now starts at 8: entries 4..7 were evicted.
		return backfill(8, 8, 9), nil
	}

	syncOnce(t, sess, client)
	second := syncOnce(t, sess, client)

	assert.Equal(t, []string{"line-8", "line-9"}, logLines(second.Entries))
	assert.Equal(t, logsLostNotice(4), second.Notice, "oldest_seq 8 - cursor 3 - 1 = 4 lost entries")
}

// TestConsumeLogsWithSync_EpochChangeRefetchesWithNotice pins the daemon-restart
// case: a different stream_id means every seq we hold belongs to a dead epoch,
// so the cursor is abandoned, the backfill is a full fetch, and the batch is
// prefixed with the restart notice that marks the seam.
func TestConsumeLogsWithSync_EpochChangeRefetchesWithNotice(t *testing.T) {
	sess := newLogsSyncSession()
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(n int, _ domain.LogParams) (*api.LogsResponse, error) {
		if n == 1 {
			return backfill(1, 1, 3), nil
		}
		// The new daemon's sequence starts over, numerically BEHIND the cursor
		// we were holding.
		return backfill(1, 1, 2), nil
	}

	syncOnce(t, sess, client)
	client.consumeLogs = quietStream("epoch-2")
	second := syncOnce(t, sess, client)

	assert.Equal(t, []string{"line-1", "line-2"}, logLines(second.Entries))
	assert.Equal(t, logsRestartNotice, second.Notice)
	assert.Equal(t, []domain.LogParams{
		{Lines: maxLogEntries},
		{Lines: maxLogEntries},
	}, client.logsCalls(), "an epoch change must not resume from the dead epoch's cursor")

	lastSeq, streamID := sess.cursor()
	assert.Equal(t, uint64(2), lastSeq, "the cursor is replaced by the new epoch's, not merged")
	assert.Equal(t, "epoch-2", streamID)
}

// TestConsumeLogsWithSync_OldServerStaysLiveOnly pins the compatibility path: a
// pre-C8 daemon sends no handshake, so there is no epoch to compare and no
// cursor to resume from. The attempt fetches nothing, delivers what it saw, and
// reports OK — exactly the pre-C9 behavior — without inventing a sync state.
func TestConsumeLogsWithSync_OldServerStaysLiveOnly(t *testing.T) {
	shortenLogsHandshakeWait(t, 50*time.Millisecond)

	sess := newLogsSyncSession()
	h := newLogsSyncHarness(sess)
	client := &stubTUIClient{
		// onHandshake deliberately never called.
		consumeLogs: func(ctx context.Context, onConnect func(), _ func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
			onConnect()
			onEvent(wireLog(0, "old-line")) // pre-C8 entries carry no seq
			<-ctx.Done()
			return ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := h.runInBackground(ctx, client)
	h.collector.await(t, func(m tea.Msg) bool { return logMsgCarriesLine(m, "old-line") })
	cancel()
	<-errCh

	assert.Empty(t, client.logsCalls(), "no cursor, no epoch, nothing to backfill against")
	assert.Equal(t, int32(1), h.syncedN.Load(), "the loop still reports OK")

	lastSeq, streamID := sess.cursor()
	assert.Zero(t, lastSeq, "no phantom cursor from an old server")
	assert.Empty(t, streamID, "no phantom epoch from an old server")
}

// TestConsumeLogsWithSync_LiveGapAbortsAndNextAttemptRecovers pins the
// discontinuity rule: an entry whose seq jumps past lastSeq+1 means the daemon
// dropped events into our subscription, so the attempt aborts with the distinct
// gap error (PINNED: abort-and-resync, not an inline splice) and the reconnect
// re-fetches exactly the missing entries from the cursor.
func TestConsumeLogsWithSync_LiveGapAbortsAndNextAttemptRecovers(t *testing.T) {
	sess := newLogsSyncSession()
	h := newLogsSyncHarness(sess)
	client := &stubTUIClient{}
	client.logsResponder = func(n int, params domain.LogParams) (*api.LogsResponse, error) {
		if n == 1 {
			return backfill(1, 1, 3), nil
		}
		return backfill(1, params.SinceSeq+1, 6), nil
	}
	client.consumeLogs = func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
		onConnect()
		onHandshake(api.HandshakeResponse{StreamID: "epoch-1"})
		select {
		case <-h.syncedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		onEvent(wireLog(6, "line-6")) // 4 and 5 never arrived
		<-ctx.Done()
		return ctx.Err()
	}

	err := h.run(context.Background(), client)

	require.ErrorIs(t, err, errLogsSyncGap)
	assert.Equal(t, stream.ClassTransient, classifyStreamError(err),
		"the loop must reconnect and re-sync on a sequence gap")
	for _, msg := range h.collector.all() {
		if entry, ok := msg.(LogEntryMsg); ok {
			assert.NotEqual(t, "line-6", domain.LogEntry(entry).Line,
				"the entry that exposed the gap is not delivered out of order")
		}
	}
	lastSeq, _ := sess.cursor()
	require.Equal(t, uint64(3), lastSeq, "the cursor stays where the batch left it")

	// The reconnect recovers everything after the cursor, gap included.
	client.consumeLogs = quietStream("epoch-1")
	second := syncOnce(t, sess, client)
	assert.Equal(t, []string{"line-4", "line-5", "line-6"}, logLines(second.Entries))
	assert.Empty(t, second.Notice)
}

// TestConsumeLogsWithSync_BufferOverflowAbortsAttempt pins the
// nothing-silently-lost rule (mirroring the requests stream): a live buffer
// that fills before the backfill lands ends the attempt with a distinct,
// retryable error rather than dropping entries, and the next attempt re-syncs
// cleanly from the cursor.
func TestConsumeLogsWithSync_BufferOverflowAbortsAttempt(t *testing.T) {
	sess := newLogsSyncSession()
	h := newLogsSyncHarness(sess)
	overflowed := make(chan struct{})
	client := &stubTUIClient{}
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		// Hold the backfill until the buffer has overflowed.
		<-overflowed
		return backfill(1, 1, 1), nil
	}
	client.consumeLogs = func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
		onConnect()
		onHandshake(api.HandshakeResponse{StreamID: "epoch-1"})
		for i := 1; i <= maxLogEntries+1; i++ {
			onEvent(wireLog(uint64(i), fmt.Sprintf("line-%d", i)))
		}
		close(overflowed)
		<-ctx.Done()
		return ctx.Err()
	}

	err := h.run(context.Background(), client)

	require.ErrorIs(t, err, errLogsSyncOverflow)
	assert.Zero(t, h.syncedN.Load(), "an overflowed attempt never reports OK")
	assert.Equal(t, stream.ClassTransient, classifyStreamError(err),
		"the loop must reconnect and re-sync on overflow")
	for _, msg := range h.collector.all() {
		_, isSync := msg.(LogsSyncMsg)
		assert.False(t, isSync, "an overflowed attempt delivers no batch")
	}
	lastSeq, streamID := sess.cursor()
	assert.Zero(t, lastSeq, "an aborted attempt adopts nothing")
	assert.Empty(t, streamID)

	// A re-attempt against a healthy stream syncs normally.
	client.consumeLogs = quietStream("epoch-1")
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		return backfill(1, 1, 2), nil
	}
	retry := syncOnce(t, newLogsSyncSession(), client)
	assert.Equal(t, []string{"line-1", "line-2"}, logLines(retry.Entries))
}

// TestConsumeLogsWithSync_BackfillFetchRetriesThenRecycles pins the
// fetch-failure policy, mirroring the requests stream: retry in place while the
// stream is live (staying unsynced the whole time), then abort the attempt
// after logsBackfillFetchAttempts so the loop's own backoff takes over.
func TestConsumeLogsWithSync_BackfillFetchRetriesThenRecycles(t *testing.T) {
	h := newLogsSyncHarness(newLogsSyncSession())
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		return nil, errors.New("backfill boom")
	}

	err := h.run(context.Background(), client)

	assert.Len(t, client.logsCalls(), logsBackfillFetchAttempts, "retries in place")
	assert.Zero(t, h.syncedN.Load(), "stays unsynced (Syncing) for the whole retry window")
	require.Error(t, err)
	assert.ErrorContains(t, err, "backfill boom")
	assert.ErrorContains(t, err, "backfill fetch failed")
	assert.Equal(t, stream.ClassTransient, classifyStreamError(err))
}

// TestConsumeLogsWithSync_DeadDialNeverSyncs pins that an attempt that never
// connects never fetches and never reports OK.
func TestConsumeLogsWithSync_DeadDialNeverSyncs(t *testing.T) {
	h := newLogsSyncHarness(newLogsSyncSession())
	client := &stubTUIClient{
		consumeLogs: func(context.Context, func(), func(api.HandshakeResponse), func(api.LogEntryResponse)) error {
			return errors.New("connection refused")
		},
	}

	assert.EqualError(t, h.run(context.Background(), client), "connection refused")
	assert.Empty(t, client.logsCalls(), "no backfill without a connection")
	assert.Zero(t, h.syncedN.Load())
}

// --- model application (Update-driven) ---

// TestLogsSync_BatchAppendsNoticeThenEntries pins the model half: the notice
// renders as a system line ahead of the batch, the batch keeps its oldest-first
// order, entries already on screen from a previous epoch are kept as history,
// and every applied line gets a DisplaySeq so the search cursor can anchor to
// it.
func TestLogsSync_BatchAppendsNoticeThenEntries(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, attachClientOptions())
	m, _ = clientUpdateModel(m, tea.WindowSizeMsg{Width: 120, Height: 20})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "web", Line: "before"}))

	m = clientUpdate(m, LogsSyncMsg{
		Notice: logsRestartNotice,
		Entries: []domain.LogEntry{
			{Process: "web", Line: "new-1", Seq: 1},
			{Process: "web", Line: "new-2", Seq: 2},
		},
	})

	require.Len(t, m.logEntries, 4)
	assert.Equal(t, []string{"before", logsRestartNotice, "new-1", "new-2"}, logLines(m.logEntries),
		"prior entries stay as history; the notice marks the seam ahead of the batch")
	assert.Equal(t, "system", m.logEntries[1].Process, "the notice renders as a system log line")

	for i, e := range m.logEntries {
		assert.Equal(t, int64(i+1), e.DisplaySeq, "every applied line is stamped, batch lines included")
	}
	assert.Contains(t, m.View(), "new-2", "the batch rendered")
}

// TestLogsSync_EmptyBatchIsNoOp pins that a caught-up reconnect — the common
// case on a quiet stream — changes nothing at all.
func TestLogsSync_EmptyBatchIsNoOp(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, attachClientOptions())
	m, _ = clientUpdateModel(m, tea.WindowSizeMsg{Width: 120, Height: 20})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "web", Line: "only"}))
	seqBefore := m.logSeq

	m = clientUpdate(m, LogsSyncMsg{})

	assert.Equal(t, []string{"only"}, logLines(m.logEntries))
	assert.Equal(t, seqBefore, m.logSeq, "an empty batch stamps nothing")
}

// TestLogsSync_BatchDoesNotAutoFilterByProcess pins that batch entries flow
// through exactly the same append path as live ones and remain visible with an
// empty filter.
func TestLogsSync_BatchDoesNotAutoFilterByProcess(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, attachClientOptions())
	m, _ = clientUpdateModel(m, tea.WindowSizeMsg{Width: 120, Height: 20})

	m = clientUpdate(m, LogsSyncMsg{Entries: []domain.LogEntry{{Process: "batch-only", Line: "x"}}})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "live-only", Line: "y"}))

	assert.Equal(t, []string{"x", "y"}, logLines(m.filteredEntries()),
		"both remain visible with no active filter")
}

// clientUpdateModel is clientUpdate keeping the returned command, for the
// window-size seeding above.
func clientUpdateModel(m ClientModel, msg tea.Msg) (ClientModel, tea.Cmd) {
	nm, cmd := m.Update(msg)
	return nm.(ClientModel), cmd
}

// --- loop-level wiring ---

// TestRunClientStreams_LogsOKOnlyAfterSync pins the protocol at the loop level,
// mirroring the requests case: the logs stream reports OK only once the
// backfill has been applied, never between connect and sync completion.
func TestRunClientStreams_LogsOKOnlyAfterSync(t *testing.T) {
	collector := newMsgCollector()
	release := make(chan struct{})
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		<-release
		return backfill(1, 1, 2), nil
	}

	startClientStreams(t, client, collector.send)

	// Before the backfill lands the logs loop must not be OK.
	time.Sleep(50 * time.Millisecond)
	for _, msg := range collector.all() {
		if s, ok := msg.(StreamStatusMsg); ok && s.Stream == StreamLogs {
			assert.NotEqual(t, stream.StateOK, s.Status.State, "OK before the backfill applied")
		}
	}

	close(release)
	collector.await(t, func(m tea.Msg) bool {
		s, ok := m.(StreamStatusMsg)
		return ok && s.Stream == StreamLogs && s.Status.State == stream.StateOK
	})

	// ...and the batch reached the models before that OK.
	var syncIdx, okIdx = -1, -1
	for i, msg := range collector.all() {
		switch v := msg.(type) {
		case LogsSyncMsg:
			if syncIdx < 0 {
				syncIdx = i
			}
		case StreamStatusMsg:
			if v.Stream == StreamLogs && v.Status.State == stream.StateOK && okIdx < 0 {
				okIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, syncIdx, 0, "the sync batch must be delivered")
	require.GreaterOrEqual(t, okIdx, 0)
	assert.Less(t, syncIdx, okIdx, "the batch is delivered before the loop reports OK")
}

// TestConsumeLogsWithSync_BackfillEpochMismatchAborts pins the epoch-consistency
// guard (codex C9 review): a REST backfill answered by a DIFFERENT manager
// lifetime than the SSE handshake announced (daemon replaced between the two)
// must abort the attempt rather than label the new epoch's data — and cursor —
// with the old epoch.
func TestConsumeLogsWithSync_BackfillEpochMismatchAborts(t *testing.T) {
	sess := newLogsSyncSession()
	client := &stubTUIClient{consumeLogs: quietStream("epoch-1")}
	client.logsResponder = func(int, domain.LogParams) (*api.LogsResponse, error) {
		resp := backfill(1, 1, 3)
		resp.StreamID = "epoch-2" // restarted daemon answered the fetch
		return resp, nil
	}

	h := newLogsSyncHarness(sess)
	errCh := h.runInBackground(context.Background(), client)

	err := <-errCh
	require.ErrorIs(t, err, errLogsSyncEpochChanged)

	lastSeq, streamID := sess.cursor()
	assert.Zero(t, lastSeq, "no cursor may be adopted from a mixed-epoch fetch")
	assert.Empty(t, streamID)
}
