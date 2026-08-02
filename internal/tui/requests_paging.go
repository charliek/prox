package tui

import (
	"context"
	"errors"
	"net/http"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// pagingPhase is the requests scroll-back state machine (D11). It is explicit
// rather than derived from "is the cursor set?" so single-flight, end-of-history
// and the never-synced start are each a state of their own instead of a
// combination of nil checks.
//
//	unprimed  — no sync has completed yet: nothing is known about what is older,
//	            so nothing may be fetched.
//	ready     — pagingCursor names a real older page; a trigger may fetch it.
//	loading   — a page fetch is in flight. This is the single-flight gate: rapid
//	            navigation cannot stack fetches.
//	exhausted — the oldest record the server still holds is loaded (or the cursor
//	            aged out, which means the same thing — see prependOlderRequests).
//
// The invariant across all four: pagingCursor is non-empty exactly in ready and
// loading. unprimed has never had one, and both exhausted paths retire it.
type pagingPhase int

const (
	pagingUnprimed pagingPhase = iota
	pagingReady
	pagingLoading
	pagingExhausted
)

// RequestsPageMsg is one completed scroll-back page fetch.
//
// Generation and ForCursor are the supersession guard: a sync that completes
// while a page is in flight bumps the generation and re-installs the cursor
// (D12 drop-on-resync), which invalidates the page in flight — its records were
// selected relative to an anchor the list no longer holds. The apply drops any
// message that does not match BOTH, so a late result can never splice records
// into a list that has moved on.
//
// Records are oldest-first, like every other record slice in the model layer
// (snapshotRecords does the reversal from the endpoint's newest-first order).
// NextBeforeID is the cursor for the page older than this one; "" means this
// page reached the ring's oldest record.
type RequestsPageMsg struct {
	Generation   int
	ForCursor    string
	Records      []proxy.RequestRecord
	NextBeforeID string
	Err          error
}

// installRequestsPaging resets the pagination state from a completed sync's
// cursor. It runs on EVERY sync, unconditionally, because the generation bump is
// what retires a page in flight (see RequestsPageMsg): skipping it when the
// cursor looks unchanged would leave a stale page appliable.
func (b *BaseModel) installRequestsPaging(nextBeforeID string) {
	b.pagingGen++
	b.setPagingCursor(nextBeforeID)
}

// setPagingCursor installs the cursor for the next older page and puts the phase
// where that cursor implies: an empty cursor means nothing older is reachable.
// This is the single enforcement point for the invariant in pagingPhase's doc —
// pagingCursor is non-empty exactly in ready and loading — so every path that
// retires or advances the cursor comes through here.
func (b *BaseModel) setPagingCursor(cursor string) {
	b.pagingCursor = cursor
	b.pagingErr = nil
	if cursor == "" {
		b.pagingPhase = pagingExhausted
		return
	}
	b.pagingPhase = pagingReady
}

// requestsPagingSegments renders the status-bar segments for scroll-back state,
// in the same list as streamHealthSegments (see statusBar).
//
// "start of history" is gated on the requests view: it is passive context about
// the list the user is looking at, and would be noise anywhere else. The
// loading and error segments are NOT view-gated — a fetch in flight and a failed
// fetch are events, and the user may well have tabbed away while one was
// running. Styling follows streamHealthSegments: a warning sign for the thing
// that went wrong, plain text for the passive states.
func (b *BaseModel) requestsPagingSegments() []string {
	var segs []string
	switch b.pagingPhase {
	case pagingLoading:
		segs = append(segs, styles.FooterLabel.Render("loading older…"))
	case pagingExhausted:
		// Suppressed on an empty list: "start of history" on a list with no
		// history at all (a proxy that has served nothing yet) says nothing.
		if b.viewMode == ViewModeRequests && len(b.proxyRequests) > 0 {
			segs = append(segs, styles.FooterLabel.Render("start of history"))
		}
	}
	if b.pagingErr != nil {
		segs = append(segs, styles.Warn.Render("⚠ older: "+truncateError(b.pagingErr, maxErrorDisplayLen)))
	}
	return segs
}

// maybeFetchOlderRequests is D11's trigger, called after a navigation key was
// handled: if the cursor has landed on the oldest VISIBLE row and there is a
// known older page, fetch it. Returns nil when any condition fails, which is
// the common case — every j/k in the list runs through here.
//
// The conditions, each for its own reason:
//   - requests view: scroll-back is that list's gesture, and the cursor means
//     nothing in the other views.
//   - follow off: follow pins the cursor to the NEWEST row, so a trigger under
//     follow would be a page nobody asked for.
//   - phase ready: each of the other three phases has its own reason not to
//     fetch (see pagingPhase).
//   - below the history cap: at the cap a prepended page would immediately
//     trim newer rows away, which is not what the user asked for.
//   - cursor on the oldest visible row: the gesture itself. "Visible" is the
//     FILTERED list — the user's own view of the list — even though the fetch
//     is unfiltered (see fetchOlderRequests).
func (m *ClientModel) maybeFetchOlderRequests() tea.Cmd {
	if m.viewMode != ViewModeRequests || m.followMode {
		return nil
	}
	if m.pagingPhase != pagingReady || m.pagingCursor == "" {
		return nil
	}
	if len(m.proxyRequests) >= maxRequestHistory {
		return nil
	}
	// cursorIdx is resolved against the filtered list at every render
	// (resolveRequestCursor), so index 0 IS the oldest visible row; -1 is the
	// empty-list sentinel.
	if m.cursorIdx != 0 {
		return nil
	}

	cmd := m.fetchOlderRequests(m.pagingGen, m.pagingCursor)
	m.pagingPhase = pagingLoading
	m.pagingErr = nil // a new attempt supersedes the last failure's note
	return cmd
}

// fetchOlderRequests builds the page-fetch command, tagged with the generation
// and cursor it was dispatched for so the apply can drop a superseded result.
//
// The fetch is deliberately UNFILTERED (only Limit and BeforeID are set),
// matching the sync fetch's semantics: the TUI's filters are client-side and
// applied at render, so a server-side filter here would make the cursor walk a
// different sequence than the list holds.
func (m ClientModel) fetchOlderRequests(generation int, beforeID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		// Bounded like every other one-shot TUI fetch: *cli.Client caps its own
		// requests at 30s (which is what the ctx-less fetchRequestDetail relies
		// on), and this ctx pins the same bound for any other TUIClient. There
		// is no attempt to cancel for: a superseded page is dropped at apply
		// time by the generation/cursor guard.
		ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultRequestTimeout)
		defer cancel()

		resp, err := client.GetProxyRequests(ctx, domain.ProxyRequestParams{
			Limit:    constants.TUIRequestsSyncLimit,
			BeforeID: beforeID,
		})
		if err != nil {
			return RequestsPageMsg{Generation: generation, ForCursor: beforeID, Err: err}
		}
		return RequestsPageMsg{
			Generation:   generation,
			ForCursor:    beforeID,
			Records:      pageRecords(resp.Requests),
			NextBeforeID: resp.NextBeforeID,
		}
	}
}

// pageRecords converts one page's wire records the same way the sync snapshot
// does — same conversion, same newest-first-to-oldest-first reversal — with a
// DISCARDING sink: a command's only output is the message it returns, so it has
// nowhere to send parseStreamTimestamp's malformed-timestamp warning. The
// fallback (now) still applies, and such a daemon would already have warned
// through the requests stream, which converts the same records with a real sink.
func pageRecords(requests []api.ProxyRequestResponse) []proxy.RequestRecord {
	return snapshotRecords(func(tea.Msg) {}, requests)
}

// prependOlderRequests applies one page result (D11). Every exit path leaves the
// phase somewhere terminal-for-now — ready, exhausted, or untouched for a
// dropped message — so the state machine can never strand in loading.
func (b *BaseModel) prependOlderRequests(msg RequestsPageMsg) {
	// Superseded — see RequestsPageMsg. Dropped WHOLE: no records, no phase
	// change, no error note, because whatever superseded it owns the state now.
	if msg.Generation != b.pagingGen || msg.ForCursor != b.pagingCursor {
		return
	}

	if msg.Err != nil {
		b.notePageError(msg.Err)
		return
	}

	b.spliceOlderRequests(msg.Records)
	// An empty NextBeforeID is exhaustion whether this page reached the ring's
	// oldest record or came back empty at the end of history.
	b.setPagingCursor(msg.NextBeforeID)
	b.renderAfterProxyRequests()
}

// notePageError classifies a failed page fetch. A 410 CURSOR_GONE is
// end-of-history, not a failure to report:
//
// the TUI walks strictly older records from its oldest-loaded anchor, and the
// ring evicts oldest-first, so an anchor that aged out WITHIN THIS GENERATION
// implies everything older than it is gone too. (This is why we exhaust rather
// than follow the handler's generic "restart pagination unanchored" guidance —
// restarting would re-fetch records we already hold.) The one case where
// restarting would matter, a replaced daemon whose ring is unrelated to ours, is
// covered by generations: replacement forces a reconnect, whose sync bumps the
// generation, and the stale 410 is dropped by the guard above before reaching
// here.
//
// Anything else is transient: the phase returns to ready so the same gesture
// re-triggers, and the error rides the status bar until then.
func (b *BaseModel) notePageError(err error) {
	var apiErr APIStatusError
	if errors.As(err, &apiErr) &&
		apiErr.StatusCode() == http.StatusGone &&
		apiErr.ErrorCode() == domain.ErrCodeCursorGone {
		// Retiring the cursor is what exhausts the phase, and it also means the
		// dead anchor can never be dispatched or applied against again.
		b.setPagingCursor("")
		return
	}
	b.pagingPhase = pagingReady
	b.pagingErr = err
}

// spliceOlderRequests merges one page into the front of the list. Records and
// list are both oldest-first, so the walk runs forwards.
//
// Overlaps — a page re-serving a record the list already holds — resolve by the
// same monotonic rule as every other arrival (applyMonotonicAt), IN PLACE: an
// existing final row is terminal, an existing in-flight row is upgraded by the
// page's final copy without moving. Only genuinely novel records are prepended,
// as one contiguous block, which keeps the list a single oldest-first sequence.
// The cursor needs no special handling: it is ID-anchored
// (resolveRequestCursor), so it rides its own row down as the block lands in
// front of it.
func (b *BaseModel) spliceOlderRequests(records []proxy.RequestRecord) {
	if len(records) == 0 {
		return
	}

	// One index pass instead of a newest-first scan per record: a page's
	// overlaps sit at the OLDEST end of the list, which is the far end of that
	// scan. Later occurrences win, matching upsertExistingRequest's
	// newest-first resolution.
	byID := make(map[string]int, len(b.proxyRequests))
	for i, req := range b.proxyRequests {
		if req.ID != "" {
			byID[req.ID] = i
		}
	}

	novel := make([]proxy.RequestRecord, 0, len(records))
	novelIdx := make(map[string]int, len(records))
	for _, req := range records {
		if idx, ok := byID[req.ID]; ok {
			b.applyMonotonicAt(idx, req)
			continue
		}
		// A page carrying the same ID twice is not something the ring produces,
		// but "no duplicate rows" is a list invariant the whole cursor/render
		// stack relies on, so resolve it by the same rule rather than growing a
		// second row for it.
		if idx, ok := novelIdx[req.ID]; ok {
			novel[idx] = monotonicWinner(novel[idx], req)
			continue
		}
		if req.ID != "" {
			novelIdx[req.ID] = len(novel)
		}
		novel = append(novel, req)
	}
	if len(novel) == 0 {
		return
	}

	b.proxyRequests = append(novel, b.proxyRequests...)
	// Keeps the NEWEST records, as every other growth path does: a page that
	// would push the list past the cap loses its own oldest rows rather than
	// evicting live history the user is watching.
	b.trimRequestHistory()
}
