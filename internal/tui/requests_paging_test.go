package tui

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// --- helpers ---

// primedPagingModel builds an attach-mode requests view whose pagination state
// was installed by a REAL sync (the production path): the snapshot is the newest
// n records oldest-first, nextBeforeID is the cursor the server handed back.
// followMode is still on, so the cursor sits pinned to the newest row and no
// trigger has fired yet.
func primedPagingModel(stub *stubTUIClient, n int, nextBeforeID string) ClientModel {
	m := newClientRequestsModel(stub, 0, 10)
	return clientUpdate(m, RequestsSyncMsg{
		Snapshot:     makeTestRequests(n),
		NextBeforeID: nextBeforeID,
	})
}

// primeForPaging installs pagination state directly. Used only by the cap-sized
// fixtures, where driving a real sync would mean merging thousands of records for
// no added coverage; every other test primes through a sync.
func primeForPaging(m ClientModel, cursor string) ClientModel {
	m.pagingCursor = cursor
	m.pagingPhase = pagingReady
	m.pagingGen++
	return m
}

// olderPage builds one page response in WIRE order (newest-first) with ids
// older-{n-1}..older-0, as the REST endpoint returns them.
func olderPage(n int) []api.ProxyRequestResponse {
	page := make([]api.ProxyRequestResponse, 0, n)
	for i := n - 1; i >= 0; i-- {
		page = append(page, wireRequest(fmt.Sprintf("older-%d", i), 200, false))
	}
	return page
}

// pageMsgFrom runs a dispatched page-fetch command and returns its message.
func pageMsgFrom(t *testing.T, cmd tea.Cmd) RequestsPageMsg {
	t.Helper()
	require.NotNil(t, cmd, "expected a page fetch command")
	msg, ok := cmd().(RequestsPageMsg)
	require.True(t, ok, "a page fetch must produce a RequestsPageMsg")
	return msg
}

// pageMsgFor builds a page message tagged for the model's CURRENT generation and
// cursor, i.e. one that passes the supersession guard.
func pageMsgFor(m ClientModel, records []proxy.RequestRecord, nextBeforeID string) RequestsPageMsg {
	return RequestsPageMsg{
		Generation:   m.pagingGen,
		ForCursor:    m.pagingCursor,
		Records:      records,
		NextBeforeID: nextBeforeID,
	}
}

// gotoOldest presses `g` (home): cursor to the oldest visible row, follow off —
// the canonical scroll-back gesture. Returns the model and whatever command the
// trigger dispatched (nil when a condition was not met).
func gotoOldest(m ClientModel) (ClientModel, tea.Cmd) {
	return clientUpdateModel(m, keyRune('g'))
}

// navNoFetch presses key and fails if it dispatched anything. A stray command is
// RUN before failing, so an accidental fetch also shows up in the stub's counter
// for the enclosing assertions.
func navNoFetch(t *testing.T, m ClientModel, key tea.KeyMsg) ClientModel {
	t.Helper()
	m, cmd := clientUpdateModel(m, key)
	if cmd != nil {
		cmd()
		t.Fatalf("key %q dispatched a command; expected none", key.String())
	}
	return m
}

// --- page success / ordering ---

// TestRequestsPaging_PageSuccessPrependsOldestFirst is the happy path end to
// end: the gesture dispatches ONE unfiltered before_id fetch, and the page lands
// in front of the list in oldest-first order with the cursor advanced.
func TestRequestsPaging_PageSuccessPrependsOldestFirst(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(3), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 2, "cur-1")
	require.Equal(t, pagingReady, m.pagingPhase)

	m, cmd := gotoOldest(m)
	assert.Equal(t, pagingLoading, m.pagingPhase, "the trigger arms single-flight before returning")

	msg := pageMsgFrom(t, cmd)
	assert.Equal(t, m.pagingGen, msg.Generation)
	assert.Equal(t, "cur-1", msg.ForCursor)

	params := stub.snapshotCallParams()
	require.Len(t, params, 1, "exactly one page fetch")
	assert.Equal(t, "cur-1", params[0].BeforeID)
	assert.Equal(t, constants.TUIRequestsSyncLimit, params[0].Limit)

	m = clientUpdate(m, msg)

	assert.Equal(t, []string{"older-0", "older-1", "older-2", "req-000", "req-001"}, requestIDs(m))
	assert.Equal(t, "cur-2", m.pagingCursor)
	assert.Equal(t, pagingReady, m.pagingPhase)
	assert.NoError(t, m.pagingErr)
}

// TestRequestsPaging_TriggerUsesFilteredOldestRow pins the trigger's notion of
// "oldest": the oldest VISIBLE row, not the oldest row in the list. A filtered
// view whose top row still has unfiltered rows below it must page — that top row
// is the end of the user's own view. The fetch itself stays UNFILTERED, so the
// cursor keeps walking the same sequence the list holds.
func TestRequestsPaging_TriggerUsesFilteredOldestRow(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(1), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 5, "cur-1")
	m.setRequestsFilterQuery("/path/003") // only req-003 visible; req-000..002 are older
	m.updateViewport()

	m, cmd := gotoOldest(m)
	require.Equal(t, "req-003", m.cursorID)
	require.NotNil(t, cmd, "the oldest visible row triggers even with older rows filtered out")
	pageMsgFrom(t, cmd)

	params := stub.snapshotCallParams()
	require.Len(t, params, 1)
	assert.Empty(t, params[0].URLContains, "a client-side filter must not become a server-side one")
	assert.Empty(t, params[0].Subdomain)
	assert.Empty(t, params[0].Method)
}

// TestRequestsPaging_ReachingOldestExhausts pins end-of-history: a page whose
// NextBeforeID is empty exhausts the state machine, and further navigation
// fetches nothing at all.
func TestRequestsPaging_ReachingOldestExhausts(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(2)} // nextBeforeID "" — the ring's oldest
	m := primedPagingModel(stub, 1, "cur-1")

	m, cmd := gotoOldest(m)
	m = clientUpdate(m, pageMsgFrom(t, cmd))

	assert.Equal(t, pagingExhausted, m.pagingPhase)
	assert.Empty(t, m.pagingCursor)
	assert.Equal(t, []string{"older-0", "older-1", "req-000"}, requestIDs(m))
	assert.Contains(t, m.View(), "start of history")

	// Nothing further is fetchable.
	m = navNoFetch(t, m, keyRune('g'))
	m = navNoFetch(t, m, keyRune('k'))
	assert.Equal(t, 1, stub.snapshotCalls(), "an exhausted list never fetches again")
}

// TestRequestsPaging_EmptyPageExhausts covers the degenerate response: no
// records and no cursor is the end of history just as much as a full last page.
func TestRequestsPaging_EmptyPageExhausts(t *testing.T) {
	m := primedPagingModel(&stubTUIClient{}, 2, "cur-1")

	m = clientUpdate(m, pageMsgFor(m, nil, ""))

	assert.Equal(t, pagingExhausted, m.pagingPhase)
	assert.Equal(t, []string{"req-000", "req-001"}, requestIDs(m), "an empty page changes nothing")
}

// --- overlap resolution (both directions) ---

// TestRequestsPaging_OverlapResolvesMonotonicallyInPlace pins that a page
// re-serving rows the list already holds resolves by the same monotonic rule as
// every other arrival, IN PLACE: an existing final row is terminal, an existing
// in-flight row is upgraded by the page's final copy without moving, and neither
// is duplicated.
func TestRequestsPaging_OverlapResolvesMonotonicallyInPlace(t *testing.T) {
	m := newClientRequestsModel(&stubTUIClient{}, 0, 10)
	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot: []proxy.RequestRecord{
			syncRecord("done", 204, false), // final: terminal
			syncRecord("live", 0, true),    // in-flight: upgradeable
		},
		NextBeforeID: "cur-1",
	})

	// Wire order (newest-first): the two overlaps plus one novel older record.
	m = clientUpdate(m, pageMsgFor(m, pageRecords([]api.ProxyRequestResponse{
		wireRequest("live", 200, false), // a final copy of the in-flight row
		wireRequest("done", 500, false), // a contradictory copy of a final row
		wireRequest("older-0", 200, false),
	}), "cur-2"))

	assert.Equal(t, []string{"older-0", "done", "live"}, requestIDs(m),
		"overlaps stay in place; only the novel record is prepended")
	assert.Equal(t, 204, findRequest(t, m, "done").StatusCode, "final is terminal")
	live := findRequest(t, m, "live")
	assert.False(t, live.InFlight, "an in-flight row is upgraded by the page's final copy")
	assert.Equal(t, 200, live.StatusCode)
}

// TestRequestsPaging_DuplicateWithinPageLandsOnce pins the list invariant the
// cursor and render stack rely on — no duplicate rows — even for a page that
// carries the same ID twice, resolving the pair by the monotonic rule.
func TestRequestsPaging_DuplicateWithinPageLandsOnce(t *testing.T) {
	m := primedPagingModel(&stubTUIClient{}, 1, "cur-1")

	m = clientUpdate(m, pageMsgFor(m, pageRecords([]api.ProxyRequestResponse{
		wireRequest("older-0", 200, false), // newest-first: the completion first
		wireRequest("older-0", 0, true),
	}), "cur-2"))

	assert.Equal(t, []string{"older-0", "req-000"}, requestIDs(m))
	assert.False(t, findRequest(t, m, "older-0").InFlight, "the final copy wins")
}

// --- cursor / trim ---

// TestRequestsPaging_CursorStableAcrossPrepend pins that the ID-anchored cursor
// rides its own row as the page lands in front of it: same row, index shifted by
// the size of the prepended block.
func TestRequestsPaging_CursorStableAcrossPrepend(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(3), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 4, "cur-1")

	m, cmd := gotoOldest(m)
	require.Equal(t, "req-000", m.cursorID)
	require.Equal(t, 0, m.cursorIdx)

	m = clientUpdate(m, pageMsgFrom(t, cmd))

	assert.Equal(t, "req-000", m.cursorID, "the cursor keeps its row")
	assert.Equal(t, 3, m.cursorIdx, "and rides down by the prepended block's size")
	assert.False(t, m.followMode, "a page never re-engages follow")
}

// TestRequestsPaging_TrimAtCapKeepsNewest pins the cap: a page that would push
// the list past maxRequestHistory loses its OWN oldest rows rather than evicting
// newer history the user is watching.
func TestRequestsPaging_TrimAtCapKeepsNewest(t *testing.T) {
	m := newClientRequestsModel(&stubTUIClient{}, maxRequestHistory-2, 10)
	m = primeForPaging(m, "cur-1")
	newest := m.proxyRequests[len(m.proxyRequests)-1].ID

	m = clientUpdate(m, pageMsgFor(m, pageRecords(olderPage(5)), "cur-2"))

	require.Len(t, m.proxyRequests, maxRequestHistory)
	assert.Equal(t, "older-3", m.proxyRequests[0].ID, "the page's own oldest rows are trimmed")
	assert.Equal(t, newest, m.proxyRequests[len(m.proxyRequests)-1].ID, "the newest row survives")
}

// TestRequestsPaging_TriggerDisabledAtCap pins the other half: a full list stops
// paging entirely, rather than fetching a page whose rows the trim would
// immediately discard.
func TestRequestsPaging_TriggerDisabledAtCap(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientRequestsModel(stub, maxRequestHistory, 10)
	m = primeForPaging(m, "cur-1")

	m = navNoFetch(t, m, keyRune('g'))

	assert.Equal(t, pagingReady, m.pagingPhase, "no fetch, so no loading state either")
	assert.Zero(t, stub.snapshotCalls())
}

// --- supersession ---

// TestRequestsPaging_StaleGenerationDropped pins the interaction with D12: a
// sync that completes while a page is in flight bumps the generation, and the
// page — selected against an anchor the sync may have just dropped — is
// discarded WHOLE: no records, no cursor change, no phase change.
func TestRequestsPaging_StaleGenerationDropped(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(2), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 2, "cur-1")

	m, cmd := gotoOldest(m)
	inFlight := pageMsgFrom(t, cmd)
	require.Equal(t, pagingLoading, m.pagingPhase)

	// A reconnect's sync lands first.
	m = clientUpdate(m, RequestsSyncMsg{
		Snapshot:     makeTestRequests(2),
		NextBeforeID: "cur-9",
	})
	genAfterSync := m.pagingGen
	idsAfterSync := requestIDs(m)
	require.Equal(t, pagingReady, m.pagingPhase)

	m = clientUpdate(m, inFlight)

	assert.Equal(t, idsAfterSync, requestIDs(m), "a superseded page contributes no records")
	assert.Equal(t, genAfterSync, m.pagingGen)
	assert.Equal(t, "cur-9", m.pagingCursor, "the sync's cursor is untouched")
	assert.Equal(t, pagingReady, m.pagingPhase)
}

// TestRequestsPaging_StaleForCursorDropped pins the second half of the
// supersession guard, which the generation alone does not cover: a duplicate (or
// late) result for a cursor the state machine has already moved PAST, inside the
// same generation, must not re-apply its records.
func TestRequestsPaging_StaleForCursorDropped(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(2), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 1, "cur-1")

	m, cmd := gotoOldest(m)
	page := pageMsgFrom(t, cmd)
	m = clientUpdate(m, page)
	require.Equal(t, "cur-2", m.pagingCursor)
	idsAfterPage := requestIDs(m)
	genAfterPage := m.pagingGen

	// The very same message again: same generation, but ForCursor now names a
	// cursor the state machine has moved past.
	m = clientUpdate(m, page)

	assert.Equal(t, idsAfterPage, requestIDs(m), "a stale-cursor page is dropped")
	assert.Equal(t, genAfterPage, m.pagingGen)
	assert.Equal(t, "cur-2", m.pagingCursor)
	assert.Equal(t, pagingReady, m.pagingPhase)
}

// --- errors ---

// TestRequestsPaging_CursorGoneExhausts pins the 410 policy (D11): an anchor
// that aged out WITHIN this generation means everything older is gone too, so
// end-of-history is the truth — not an error worth showing.
func TestRequestsPaging_CursorGoneExhausts(t *testing.T) {
	m := primedPagingModel(&stubTUIClient{}, 2, "cur-1")

	m = clientUpdate(m, RequestsPageMsg{
		Generation: m.pagingGen,
		ForCursor:  m.pagingCursor,
		Err:        &fakeAPIError{status: http.StatusGone, code: domain.ErrCodeCursorGone},
	})

	assert.Equal(t, pagingExhausted, m.pagingPhase)
	assert.NoError(t, m.pagingErr, "end of history is not an error state")
	assert.Empty(t, m.pagingCursor, "the dead anchor is retired with the phase")
	view := m.View()
	assert.Contains(t, view, "start of history")
	assert.NotContains(t, view, "⚠ older:")
}

// TestRequestsPaging_TransientErrorStaysReadyAndRetriggers pins that every
// OTHER failure is transient: the phase returns to ready, the status bar carries
// the reason, and the same gesture re-triggers (clearing the note).
func TestRequestsPaging_TransientErrorStaysReadyAndRetriggers(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(1), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 2, "cur-1")

	m, cmd := gotoOldest(m)
	require.NotNil(t, cmd)
	m = clientUpdate(m, RequestsPageMsg{
		Generation: m.pagingGen,
		ForCursor:  m.pagingCursor,
		Err:        errors.New("boom"),
	})

	assert.Equal(t, pagingReady, m.pagingPhase)
	require.Error(t, m.pagingErr)
	assert.Contains(t, m.View(), "⚠ older: boom")

	// The same gesture re-triggers, and the note clears with the new attempt.
	m, retry := clientUpdateModel(m, keyRune('k'))
	require.NotNil(t, retry, "a transient failure stays re-triggerable")
	assert.Equal(t, pagingLoading, m.pagingPhase)
	assert.NoError(t, m.pagingErr)
	assert.Contains(t, m.View(), "loading older…")

	m = clientUpdate(m, pageMsgFrom(t, retry))
	assert.Equal(t, []string{"older-0", "req-000", "req-001"}, requestIDs(m))
	assert.Equal(t, pagingReady, m.pagingPhase)
}

// TestRequestsPaging_NonCursorGoneAPIErrorsAreTransient pins that the 410 rule
// is an EXACT match rather than a blanket status or code check: a 410 without the
// code, and the code on any other status, both stay transient.
func TestRequestsPaging_NonCursorGoneAPIErrorsAreTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"410 without the cursor-gone code", &fakeAPIError{status: http.StatusGone}},
		{"cursor-gone code on another status", &fakeAPIError{status: http.StatusInternalServerError, code: domain.ErrCodeCursorGone}},
		{"a plain transport error", errors.New("connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := primedPagingModel(&stubTUIClient{}, 1, "cur-1")

			m = clientUpdate(m, RequestsPageMsg{
				Generation: m.pagingGen,
				ForCursor:  m.pagingCursor,
				Err:        tc.err,
			})

			assert.Equal(t, pagingReady, m.pagingPhase)
			assert.Error(t, m.pagingErr)
			assert.Equal(t, "cur-1", m.pagingCursor, "a transient failure keeps the cursor")
		})
	}
}

// TestRequestsPaging_FetchErrorReachesTheModel pins the command's own error
// path: a failing client call produces a RequestsPageMsg carrying the error
// (tagged for its generation and cursor) rather than a dropped message.
func TestRequestsPaging_FetchErrorReachesTheModel(t *testing.T) {
	stub := &stubTUIClient{snapshotErr: errors.New("api error 503")}
	m := primedPagingModel(stub, 2, "cur-1")

	m, cmd := gotoOldest(m)
	msg := pageMsgFrom(t, cmd)

	require.Error(t, msg.Err)
	assert.Equal(t, "cur-1", msg.ForCursor)
	assert.Equal(t, m.pagingGen, msg.Generation)
	assert.Empty(t, msg.Records)
}

// --- trigger conditions / single flight ---

// TestRequestsPaging_TriggerConditions pins every condition that must SUPPRESS a
// page fetch. Each case drives a real navigation key so the check runs where it
// ships (ClientModel.handleKey, after handleNavigationKey).
func TestRequestsPaging_TriggerConditions(t *testing.T) {
	cases := []struct {
		name  string
		build func(stub *stubTUIClient) ClientModel
		key   tea.KeyMsg
	}{
		{
			name: "follow mode on",
			// A one-row list: the single row is both oldest and newest, so `j`
			// leaves the cursor on the oldest visible row with follow engaged —
			// the only condition left to suppress the fetch.
			build: func(stub *stubTUIClient) ClientModel { return primedPagingModel(stub, 1, "cur-1") },
			key:   keyRune('j'),
		},
		{
			name: "not the requests view",
			build: func(stub *stubTUIClient) ClientModel {
				m := primedPagingModel(stub, 4, "cur-1")
				m.viewMode = ViewModeLogs
				return m
			},
			key: keyRune('g'),
		},
		{
			name: "a page is already loading",
			build: func(stub *stubTUIClient) ClientModel {
				m := primedPagingModel(stub, 4, "cur-1")
				m.pagingPhase = pagingLoading
				return m
			},
			key: keyRune('g'),
		},
		{
			name: "no sync has primed the cursor",
			build: func(stub *stubTUIClient) ClientModel {
				// Never synced: rows arrived live, so nothing is known about
				// what is older.
				m := newClientRequestsModel(stub, 4, 10)
				require.Equal(t, pagingUnprimed, m.pagingPhase)
				return m
			},
			key: keyRune('g'),
		},
		{
			name: "history is exhausted",
			build: func(stub *stubTUIClient) ClientModel {
				return primedPagingModel(stub, 4, "") // the sync reached the oldest record
			},
			key: keyRune('g'),
		},
		{
			name: "the cursor is not on the oldest visible row",
			build: func(stub *stubTUIClient) ClientModel {
				m := primedPagingModel(stub, 4, "cur-1")
				m.followMode = false
				m.setRequestCursor(m.filteredProxyRequests(), 1)
				m.updateViewport()
				return m
			},
			key: keyRune('j'), // moves to row 2, still off the oldest row
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubTUIClient{snapshot: olderPage(1), nextBeforeID: "cur-2"}
			m := tc.build(stub)
			phaseBefore := m.pagingPhase

			m = navNoFetch(t, m, tc.key)

			assert.Zero(t, stub.snapshotCalls(), "no page fetch may be dispatched")
			assert.Equal(t, phaseBefore, m.pagingPhase, "a suppressed trigger changes no state")
		})
	}
}

// TestRequestsPaging_SingleFlightUnderRapidNavigation pins the loading gate:
// hammering the navigation keys while a page is in flight dispatches exactly one
// fetch.
func TestRequestsPaging_SingleFlightUnderRapidNavigation(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(2), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 3, "cur-1")

	m, cmd := gotoOldest(m)
	require.NotNil(t, cmd)
	first := pageMsgFrom(t, cmd)

	for i := 0; i < 5; i++ {
		m = navNoFetch(t, m, keyRune('k'))
	}
	assert.Equal(t, 1, stub.snapshotCalls(), "loading blocks every re-trigger")

	// Once the page lands, the gesture works again — but only after the cursor
	// has walked back up to the NEW oldest row: the page it just applied now sits
	// in front of the row it was parked on.
	m = clientUpdate(m, first)
	require.Equal(t, 2, m.cursorIdx, "the cursor rode down by the prepended block")
	m = navNoFetch(t, m, keyRune('k')) // row 1: still not the oldest
	_, retry := m.Update(keyRune('k')) // row 0: the oldest visible row again
	assert.NotNil(t, retry, "the gate opens again after the page lands")
}
