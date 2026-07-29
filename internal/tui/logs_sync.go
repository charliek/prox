package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// LogsSyncMsg is one completed logs-stream synchronization: the entries fetched
// over REST plus every live entry that arrived while the fetch was running, in
// one oldest-first batch, and an optional one-line notice describing a
// discontinuity the user needs to know about (a restarted daemon, or entries
// the server's ring evicted before we could resume). The models append the
// notice then the entries and re-render ONCE, so a full 1000-line backfill
// costs one render rather than a thousand.
//
// Exactly one of these is delivered per successful stream attempt, immediately
// before the loop is marked synchronized (StateOK). An attempt with nothing to
// report still delivers one (empty), which the model handler no-ops.
type LogsSyncMsg struct {
	Entries []domain.LogEntry
	Notice  string
}

// errLogsSyncOverflow ends an attempt whose live-entry buffer filled before the
// backfill landed. Distinct on purpose: the loop reconnects and re-syncs from
// the cursor, which is the only way to recover the entries the buffer could not
// hold. Nothing is silently lost — the resume asks the server for exactly them.
var errLogsSyncOverflow = errors.New("logs sync: live entry buffer overflowed before the backfill landed")

// errLogsSyncGap ends an attempt that observed a hole in the server's ingest
// sequence — an entry whose Seq jumped past lastSeq+1, which means the daemon
// dropped events into our subscriber channel (logs.Manager drops for a slow
// subscriber rather than blocking ingest).
//
// PINNED SHAPE: abort the attempt and let the loop reconnect and re-sync,
// rather than splicing an inline since_seq fetch into a live stream. The
// reconnect re-runs the same protocol, whose resume fetch asks for everything
// after our cursor and therefore recovers exactly the dropped entries (as long
// as the server's ring still holds them; if it does not, the rolled-buffer
// notice reports the loss honestly). Gap events are rare and a reconnect is
// cheap, so the simpler control flow wins over a second code path that would
// have to interleave a fetch with a live stream all over again.
var errLogsSyncGap = errors.New("logs sync: server-side sequence gap; reconnecting to re-sync")

// errLogsSyncEpochChanged ends an attempt whose REST backfill answered from a
// different manager lifetime than the SSE handshake announced (daemon replaced
// between the two). The batch cannot be labeled with either epoch safely;
// reconnecting yields a fresh handshake and a consistent backfill.
var errLogsSyncEpochChanged = errors.New("logs sync: backfill epoch differs from handshake epoch; reconnecting to re-sync")

// logsBackfillFetchAttempts is how many consecutive backfill fetch failures an
// otherwise-live attempt tolerates before it is aborted and recycled. Same
// reasoning as requestsSnapshotFetchAttempts: retrying in place is right for a
// blip, but an endlessly failing fetch would pin the stream in Syncing forever.
const logsBackfillFetchAttempts = 3

// logsHandshakeWait bounds how long a connected attempt waits for the server's
// handshake frame before concluding it is talking to a pre-C8 daemon that never
// sends one. The real handshake is written immediately after ": connected" and
// before any log entry (internal/api/sse.go), so on a current daemon this wait
// is a round trip; the budget only ever elapses against an old one.
//
// It is a var solely so tests need not spend it. Production never changes it.
var logsHandshakeWait = 2 * time.Second

// logsRestartNotice marks the seam where a NEW daemon epoch's backfill was
// spliced onto entries printed by the previous run, which stay on screen as
// history.
const logsRestartNotice = "log stream restarted; earlier entries are from the previous run"

// logsLostNotice reports entries the server's ring evicted between our cursor
// and the oldest entry it could still hand back.
func logsLostNotice(lost uint64) string {
	return fmt.Sprintf("log stream reconnected; %d earlier entries were lost", lost)
}

// UNFILTERED INVARIANT (pinned): every seq comparison in this file — the
// resume cursor, the rolled-buffer lost count, the live-gap detection — is
// only sound on an UNFILTERED subscription, where "the next entry I should see
// has Seq == lastSeq+1" holds. The attach TUI subscribes with an empty
// domain.LogParams and filters client-side (BaseModel.filteredEntries), so it
// qualifies. A future filtered subscription would see legitimate holes in the
// sequence for every entry the server filtered out, and would report phantom
// gaps and phantom losses on every reconnect: such a stream MUST NOT reuse this
// machinery without first replacing seq arithmetic with something filter-aware.

// logsSyncSession is the CROSS-ATTEMPT half of the protocol: how far the attach
// session has consumed (lastSeq) and which server epoch that cursor belongs to
// (streamID). Both zero means "never synced". One session is created per attach
// run and shared by every attempt of the logs loop, which is what lets attempt
// N+1 resume where attempt N left off instead of re-fetching the world.
//
// Attempts are sequential (one stream.Loop, one attempt at a time), but within
// an attempt the SSE reader goroutine and the fetch goroutine both touch it, so
// it carries its own mutex. Lock order is always logsSyncState.mu →
// logsSyncSession.mu; nothing here calls back into the state.
type logsSyncSession struct {
	mu       sync.Mutex
	lastSeq  uint64
	streamID string
}

func newLogsSyncSession() *logsSyncSession {
	return &logsSyncSession{}
}

// cursor reads the session's resume point.
func (s *logsSyncSession) cursor() (lastSeq uint64, streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq, s.streamID
}

// adopt records the epoch a completed sync synchronized against and the seq it
// consumed up to. It is an assignment, not a merge: after an epoch change the
// new epoch's sequence is unrelated to the old one and may well be lower.
func (s *logsSyncSession) adopt(streamID string, lastSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamID = streamID
	s.lastSeq = lastSeq
}

// advance carries the cursor forward for a live entry delivered post-sync.
func (s *logsSyncSession) advance(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.lastSeq {
		s.lastSeq = seq
	}
}

// consumeLogsWithSync is the logs stream's single connect-and-consume attempt.
// It mirrors consumeRequestsWithSync (C6) deliberately — the two are meant to
// read as one protocol with two payloads — and differs in exactly three ways:
//
//   - The fetch cannot be issued at connect time. It needs the server's epoch
//     first, which arrives as the handshake frame, so the fetch goroutine
//     starts at connect but blocks on the handshake before choosing between a
//     full backfill and a since_seq resume.
//   - Log lines have no ID, so overlap is excluded by cursor before delivery
//     rather than merged by identity in the model.
//   - A hole in the ingest sequence is itself an event (errLogsSyncGap).
//
// The barrier discipline is identical: onConnect starts but does not complete
// the sync, live entries are buffered meanwhile, and the one LogsSyncMsg,
// markSynced, and the flip to passthrough all happen under one mutex so no live
// entry can slip out either side of the batch.
func consumeLogsWithSync(ctx context.Context, client TUIClient, sess *logsSyncSession, send func(tea.Msg), markSynced func()) error {
	attemptCtx, fetch := newSyncFetch(ctx)
	defer fetch.join()

	st := &logsSyncState{
		buffering:   true,
		abort:       fetch.abort,
		send:        send,
		sess:        sess,
		handshakeCh: make(chan string, 1),
	}

	// The subscription is deliberately unfiltered — see the invariant above.
	err := client.ConsumeLogs(attemptCtx, domain.LogParams{},
		func() {
			fetch.start(func() error {
				return syncLogsBackfill(attemptCtx, client, st, markSynced)
			})
		},
		st.noteHandshake,
		st.observe)

	// Join before reading the fetch's error: the deferred backstop runs only
	// after the return value has been computed.
	fetch.join()
	return fetch.attemptError(ctx, st.abortErr(), err)
}

// syncLogsBackfill waits for the epoch, fetches the right backfill for it, and
// completes the sync. It runs on its own goroutine, concurrently with the SSE
// read loop (which is busy reading; buffering would deadlock if the fetch ran
// on it).
//
// The fetch decision, once the epoch is known:
//
//	never synced, or a DIFFERENT epoch → full fetch of the last maxLogEntries
//	                                     lines; an epoch CHANGE (as opposed to
//	                                     a first sync) also carries the restart
//	                                     notice.
//	same epoch, cursor > 0             → resume: everything after the cursor.
//	                                     oldest_seq > cursor+1 means the ring
//	                                     rolled past us: deliver what came back
//	                                     plus the lost-count notice.
//	same epoch, cursor 0               → full fetch, no notice. We hold nothing
//	                                     from this epoch, so everything in the
//	                                     buffer is new and there is nothing to
//	                                     have lost. (Reachable: a first sync
//	                                     against an empty buffer adopts the
//	                                     epoch with cursor 0.)
//
// A failure while the stream is still live is retried in place at the stream
// package's base backoff rather than tearing the connection down — the entries
// we are buffering are exactly what the reconnect would have to re-fetch
// anyway. After logsBackfillFetchAttempts consecutive failures it gives up and
// returns the error, which aborts the attempt so the loop's own backoff and
// status reporting take over. The loop stays in Syncing (never OK) throughout.
//
// A ctx cancellation returns nil: the attempt is already ending for a reason
// the caller knows better than we do, and reporting a bare cancellation here
// would mask the stream's real error.
func syncLogsBackfill(ctx context.Context, client TUIClient, st *logsSyncState, markSynced func()) error {
	streamID, ok := st.awaitHandshake(ctx)
	if !ok {
		if ctx.Err() != nil {
			return nil
		}
		// Pre-C8 daemon: no handshake, and its entries carry no Seq either, so
		// there is no epoch to compare and no cursor to resume from. Complete
		// the sync with an empty batch — which still delivers whatever was
		// buffered during the wait, in order — and let the rest of the attempt
		// run as the pure passthrough consumer it was before C9. The loop
		// therefore reports OK one handshake budget after connect instead of at
		// connect; that delay is bounded by logsHandshakeWait and only ever
		// paid against an old daemon.
		st.complete(logsBatch{}, markSynced)
		return nil
	}

	lastSeq, storedID := st.sess.cursor()
	resume := storedID == streamID && lastSeq > 0

	// Lines is always set: the server defaults an absent `lines` to
	// constants.DefaultLogLimit (100), far below what the TUI holds. On the
	// resume path the cap keeps the OLDEST entries after the cursor (see
	// logs.Manager.QueryFromSeq), so a resume that hits it stops short of the
	// live stream; the resulting seam is caught as a gap and recovered by the
	// next attempt's resume rather than being silently swallowed.
	params := domain.LogParams{Lines: maxLogEntries}
	if resume {
		params.SinceSeq = lastSeq
	}

	var lastErr error
	for i := 0; i < logsBackfillFetchAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(constants.StreamReconnectBaseBackoff):
			}
		}

		resp, err := client.GetLogs(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			lastErr = err
			continue
		}

		// Epoch-consistency guard (codex C9 finding): the REST backfill must
		// belong to the SAME epoch the SSE handshake announced. If the daemon
		// restarted between the handshake and this fetch, the response carries
		// the new manager's stream_id — labeling that data with the handshake
		// epoch would splice two epochs into one batch and adopt a cursor from
		// the wrong sequence space. Abort the attempt instead: the reconnect
		// gets a fresh handshake and a consistent backfill.
		if resp.StreamID != "" && resp.StreamID != streamID {
			return fmt.Errorf("logs sync: backfill epoch %q does not match handshake epoch %q: %w",
				resp.StreamID, streamID, errLogsSyncEpochChanged)
		}

		batch := logsBatch{streamID: streamID}
		for _, e := range resp.Logs {
			batch.entries = append(batch.entries, streamedLogEntry(st.send, e))
		}
		switch {
		case resume:
			batch.cursor = lastSeq
			if resp.OldestSeq > lastSeq+1 {
				batch.notice = logsLostNotice(resp.OldestSeq - lastSeq - 1)
			}
		case storedID != "" && storedID != streamID:
			batch.notice = logsRestartNotice
		}

		st.complete(batch, markSynced)
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("logs sync: backfill fetch failed %d times: %w", logsBackfillFetchAttempts, lastErr)
}

// logsSyncState is the PER-ATTEMPT handoff between the SSE reader goroutine
// (which produces the handshake and live entries) and the fetch goroutine
// (which ends the buffering phase). Everything is guarded by mu: complete's
// "deliver the batch, adopt the cursor, mark synced, flip to passthrough" must
// be atomic with respect to observe, or a live entry could slip out either side
// of the batch or be measured against a cursor that is mid-update.
type logsSyncState struct {
	mu        sync.Mutex
	buffering bool
	buf       []domain.LogEntry
	overflow  bool
	gap       bool

	// handshakeCh carries the epoch from the reader goroutine to the fetch
	// goroutine. Buffered (cap 1) and written at most once, so the handshake
	// may arrive before OR after the fetch goroutine starts waiting: early, it
	// sits in the buffer; late, the waiter is already parked on it.
	handshakeCh   chan string
	handshakeOnce sync.Once

	// abort cancels the attempt context (syncFetch.abort). Called once, outside
	// mu, when the buffer overflows or a sequence gap is seen.
	abort func()

	send func(tea.Msg)
	sess *logsSyncSession
}

// logsBatch is what a completed backfill hands to logsSyncState.complete: the
// fetched entries (oldest-first), the epoch they came from, the seq the fetch
// resumed FROM (0 for a full fetch, which seeds the same overlap/gap arithmetic
// observe applies to live entries), and the discontinuity notice, if any. The
// zero value is the pre-C8 daemon's "nothing to backfill against".
type logsBatch struct {
	entries  []domain.LogEntry
	notice   string
	streamID string
	cursor   uint64
}

// noteHandshake latches the FIRST handshake of the attempt. A server that sends
// more than one is harmless: the epoch cannot change without a new connection,
// and re-triggering the fetch decision mid-attempt would be meaningless work.
func (st *logsSyncState) noteHandshake(hs api.HandshakeResponse) {
	st.handshakeOnce.Do(func() { st.handshakeCh <- hs.StreamID })
}

// awaitHandshake blocks for the epoch. ok is false when the attempt ended or
// the budget elapsed without one (a pre-C8 daemon).
func (st *logsSyncState) awaitHandshake(ctx context.Context) (string, bool) {
	timer := time.NewTimer(logsHandshakeWait)
	defer timer.Stop()

	select {
	case streamID := <-st.handshakeCh:
		return streamID, true
	case <-timer.C:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// observe handles one live entry: buffered while the backfill is outstanding,
// cursor-checked and delivered directly once the sync has completed.
func (st *logsSyncState) observe(entry api.LogEntryResponse) {
	st.mu.Lock()

	if st.buffering {
		if st.overflow || st.gap {
			// The attempt is already being torn down; nothing gained by keeping
			// entries for a batch that will never be delivered.
			st.mu.Unlock()
			return
		}
		// The buffer is sized to the TUI's own log ring: more entries than that
		// arriving during one fetch would evict each other on arrival anyway,
		// and the resume fetch re-reads them from the server.
		if len(st.buf) >= maxLogEntries {
			st.overflow = true
			st.mu.Unlock()
			st.abort()
			return
		}
		st.buf = append(st.buf, streamedLogEntry(st.send, entry))
		st.mu.Unlock()
		return
	}

	// Passthrough. Entries with Seq 0 come from a pre-C8 daemon (or never
	// passed through logs.Manager.Write) and carry no cursor information, so
	// they are always delivered and never move the cursor.
	if entry.Seq > 0 {
		lastSeq, _ := st.sess.cursor()
		switch {
		case entry.Seq <= lastSeq:
			// Already applied by the sync batch: the fetch and the live stream
			// overlap by construction.
			st.mu.Unlock()
			return
		case lastSeq > 0 && entry.Seq > lastSeq+1:
			st.gap = true
			st.mu.Unlock()
			st.abort()
			return
		}
		st.sess.advance(entry.Seq)
	}
	st.mu.Unlock()

	sendStreamedLogEntry(st.send, entry)
}

// complete delivers the sync batch and switches to passthrough. A no-op once
// the attempt has overflowed or seen a gap (the batch is moot) or the sync
// already completed (a client that called onConnect twice).
func (st *logsSyncState) complete(batch logsBatch, markSynced func()) {
	st.mu.Lock()

	if !st.buffering || st.overflow || st.gap {
		st.mu.Unlock()
		return
	}

	entries := batch.entries
	cursor := batch.cursor
	for _, e := range entries {
		if e.Seq > cursor {
			cursor = e.Seq
		}
	}

	// Splice the buffered live entries onto the batch, dropping the overlap the
	// fetch already covered. A hole here is the same server-side drop observe
	// watches for, and is handled the same way: abort, reconnect, and let the
	// next attempt's resume re-fetch the missing entries.
	for _, e := range st.buf {
		if e.Seq == 0 {
			entries = append(entries, e)
			continue
		}
		if cursor > 0 {
			if e.Seq <= cursor {
				continue
			}
			if e.Seq > cursor+1 {
				st.gap = true
				st.mu.Unlock()
				st.abort()
				return
			}
		}
		entries = append(entries, e)
		cursor = e.Seq
	}

	st.buf = nil
	st.buffering = false
	// Adopt BEFORE the flip is observable: the next live entry is measured
	// against this cursor.
	st.sess.adopt(batch.streamID, cursor)

	st.send(LogsSyncMsg{Entries: entries, Notice: batch.notice})
	// Only NOW is the stream synchronized: the models hold the backfill plus
	// everything that arrived during the fetch.
	markSynced()
	st.mu.Unlock()
}

// abortErr reports the sync-protocol reason this attempt must be recycled, or
// nil if it ended for a reason of the stream's own.
func (st *logsSyncState) abortErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.overflow:
		return errLogsSyncOverflow
	case st.gap:
		return errLogsSyncGap
	}
	return nil
}
