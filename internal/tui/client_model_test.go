package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
)

// stubTUIClient is a test double for the TUIClient interface (app.go). It
// records the IDs passed to GetProxyRequest and returns canned detail/error
// responses, so ClientModel Enter/detail flows can be exercised without a live
// daemon. Shared C2 deliverable — C4 reuses it for the attach-mode
// detail-refresh tests.
// The Consume* stubs block until ctx is cancelled by default — a quiet,
// never-ending stream. Returning immediately would spin a reconnect loop, so a
// test that wants scripted events or a specific failure sets the matching hook
// instead.
type stubTUIClient struct {
	mu           sync.Mutex
	requestedIDs []string // every GetProxyRequest id, in call order
	detailResp   *api.ProxyRequestDetailResponse
	detailErr    error

	// Optional per-stream attempt behavior; nil means "connect successfully,
	// then block until cancelled" (onConnect fires, no events). A scripted
	// hook owns the onConnect call: invoke it to model an established
	// connection, skip it to model a dead-on-arrival dial.
	consumeLogs     func(ctx context.Context, onConnect func(), onEvent func(api.LogEntryResponse)) error
	consumeRequests func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error
}

func (s *stubTUIClient) GetProcesses() (*api.ProcessListResponse, error) {
	return &api.ProcessListResponse{}, nil
}

func (s *stubTUIClient) RestartProcess(string) error { return nil }

func (s *stubTUIClient) ConsumeLogs(ctx context.Context, _ domain.LogParams, onConnect func(), onEvent func(api.LogEntryResponse)) error {
	if s.consumeLogs != nil {
		return s.consumeLogs(ctx, onConnect, onEvent)
	}
	onConnect()
	<-ctx.Done()
	return ctx.Err()
}

func (s *stubTUIClient) ConsumeProxyRequests(ctx context.Context, _ domain.ProxyRequestParams, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
	if s.consumeRequests != nil {
		return s.consumeRequests(ctx, onConnect, onEvent)
	}
	onConnect()
	<-ctx.Done()
	return ctx.Err()
}

func (s *stubTUIClient) GetProxyRequest(id string, _ bool) (*api.ProxyRequestDetailResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestedIDs = append(s.requestedIDs, id)
	if s.detailErr != nil {
		return nil, s.detailErr
	}
	if s.detailResp != nil {
		return s.detailResp, nil
	}
	return &api.ProxyRequestDetailResponse{
		ProxyRequestResponse: api.ProxyRequestResponse{ID: id},
	}, nil
}

// lastRequestedID returns the most recent GetProxyRequest id, or "".
func (s *stubTUIClient) lastRequestedID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requestedIDs) == 0 {
		return ""
	}
	return s.requestedIDs[len(s.requestedIDs)-1]
}

// newClientRequestsModel builds a ClientModel in the requests view holding n
// requests, viewport content sized to viewportHeight (see newRequestsModel).
func newClientRequestsModel(stub *stubTUIClient, n, viewportHeight int) ClientModel {
	m := NewClientModel(stub)
	m.viewMode = ViewModeRequests
	m.proxyRequests = makeTestRequests(n)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: viewportHeight + 6})
	return nm.(ClientModel)
}

func clientUpdate(m ClientModel, msg tea.Msg) ClientModel {
	nm, _ := m.Update(msg)
	return nm.(ClientModel)
}

// TestClientModel_EnterOpensCursorRow verifies the attach-mode Enter path opens
// the cursor row by ID: the returned fetch command targets the cursor's request,
// not the viewport's top row.
func TestClientModel_EnterOpensCursorRow(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientRequestsModel(stub, 10, 5)
	// Move the cursor off the newest row so the test proves it is cursor-driven.
	m = clientUpdate(m, keyRune('k')) // req-008, follow off
	m = clientUpdate(m, keyRune('k')) // req-007
	require.Equal(t, "req-007", m.cursorID)

	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ClientModel)
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
	assert.Equal(t, "req-007", m.selectedRequestID)
	assert.True(t, m.detailLoading)
	require.NotNil(t, cmd, "Enter must return a fetch command")

	// Executing the command performs the fetch; the stub records the id.
	msg := cmd()
	detailMsg, ok := msg.(RequestDetailMsg)
	require.True(t, ok, "fetch command should produce a RequestDetailMsg")
	assert.Equal(t, "req-007", detailMsg.ID)
	assert.Equal(t, "req-007", stub.lastRequestedID(), "the fetch targets the cursor row")
}

// TestClientModel_EnterGotoTop pins that attach-mode Enter starts the detail at
// the top even when opened from deep in a scrolled list.
func TestClientModel_EnterGotoTop(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientRequestsModel(stub, 30, 5) // follow on -> viewport scrolled down
	require.Greater(t, m.viewport.YOffset, 0)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
	assert.Equal(t, 0, m.viewport.YOffset)
}

// newClientDetailModel builds a ready ClientModel with no requests loaded
// (D16's guard tests drive detail state directly rather than through a live
// requests list).
func newClientDetailModel(stub *stubTUIClient) ClientModel {
	m := NewClientModel(stub)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 11})
	return nm.(ClientModel)
}

// finalDetailFor builds a completed (non-in-flight) RequestDetailData for id,
// carrying a header so tests can assert Details actually rendered.
func finalDetailFor(id string) *RequestDetailData {
	return &RequestDetailData{
		ID:             id,
		Method:         "GET",
		URL:            "/x",
		StatusCode:     200,
		DurationMs:     42,
		InFlight:       false,
		RequestHeaders: map[string][]string{"X-Test": {"yes"}},
	}
}

// inFlightDetailFor builds an in-flight RequestDetailData snapshot for id.
func inFlightDetailFor(id string) *RequestDetailData {
	return &RequestDetailData{ID: id, Method: "GET", URL: "/x", InFlight: true}
}

// TestClientModel_FetchRequestDetail_MapsStale verifies fetchRequestDetail
// (attach mode) carries the API response's Stale field through to
// RequestDetailData (D8, #53) — the only place where the server's
// serve-time-computed staleness reaches the TUI in attach mode, since
// RequestDetailData.Timestamp is a display string, not a time.Time the TUI
// could re-derive staleness from itself.
func TestClientModel_FetchRequestDetail_MapsStale(t *testing.T) {
	stub := &stubTUIClient{detailResp: &api.ProxyRequestDetailResponse{
		ProxyRequestResponse: api.ProxyRequestResponse{ID: "req-000", InFlight: true, Stale: true},
	}}
	m := NewClientModel(stub)

	msg := m.fetchRequestDetail("req-000", 1)()
	detailMsg, ok := msg.(RequestDetailMsg)
	require.True(t, ok, "fetch command should produce a RequestDetailMsg")
	require.NotNil(t, detailMsg.Details)
	assert.True(t, detailMsg.Details.InFlight)
	assert.True(t, detailMsg.Details.Stale)
}

// TestClientModel_DetailLiveRefresh_MatchingFinalReturnsFetchCmd pins the
// live half of D16 end to end: opening the detail on an in-flight request via
// Enter, then feeding the matching final ProxyRequestMsg, returns a re-fetch
// command and never flips detailLoading (the existing snapshot stays on
// screen with no loading flicker while the background fetch runs).
func TestClientModel_DetailLiveRefresh_MatchingFinalReturnsFetchCmd(t *testing.T) {
	stub := &stubTUIClient{detailResp: &api.ProxyRequestDetailResponse{
		ProxyRequestResponse: api.ProxyRequestResponse{ID: "req-000", InFlight: true},
	}}
	m := newClientRequestsModel(stub, 1, 5)
	require.Equal(t, "req-000", m.cursorID)

	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ClientModel)
	require.NotNil(t, cmd)
	m = clientUpdate(m, cmd())
	require.NotNil(t, m.requestDetail)
	require.True(t, m.requestDetail.InFlight)
	require.False(t, m.detailLoading)

	newModel, cmd2 := m.Update(ProxyRequestMsg(finalRecordFor("req-000")))
	m = newModel.(ClientModel)
	assert.False(t, m.detailLoading, "background refresh sets no loading flicker")
	require.NotNil(t, cmd2, "completion triggers a re-fetch")

	msg := cmd2()
	detailMsg, ok := msg.(RequestDetailMsg)
	require.True(t, ok)
	assert.Equal(t, "req-000", detailMsg.ID)
	assert.Equal(t, m.detailFetchSeq, detailMsg.Seq)
}

// TestClientModel_DetailLiveRefresh_StaleIDDroppedKeepsLoading pins that a
// result for a request OTHER than the one currently selected is dropped
// entirely — including detailLoading, which today (pre-D16) was cleared
// before the ID check and could stomp the loading state of a newer,
// currently-in-flight fetch for a different row.
func TestClientModel_DetailLiveRefresh_StaleIDDroppedKeepsLoading(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-B"
	m.detailLoading = true
	m.detailFetchSeq = 2

	m = clientUpdate(m, RequestDetailMsg{ID: "req-A", Seq: 2, Details: finalDetailFor("req-A")})

	assert.True(t, m.detailLoading, "a stale-ID result must not clear the current fetch's loading state")
	assert.Nil(t, m.requestDetail, "a stale-ID result must not populate the wrong request's detail")
}

// TestClientModel_DetailLiveRefresh_SupersededSeqSuccessDropped pins that a
// success result from an earlier, superseded fetch for the SAME id is
// dropped once a newer fetch has been started (seq mismatch).
func TestClientModel_DetailLiveRefresh_SupersededSeqSuccessDropped(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 3
	existing := inFlightDetailFor("req-1")
	m.requestDetail = existing

	m = clientUpdate(m, RequestDetailMsg{ID: "req-1", Seq: 2, Details: finalDetailFor("req-1")})

	assert.Same(t, existing, m.requestDetail, "a superseded-seq success must not replace the current snapshot")
}

// TestClientModel_DetailLiveRefresh_SupersededSeqErrorDropped mirrors the
// success case for RequestDetailErrorMsg: an error from an earlier,
// superseded fetch must not touch detailError/detailRefreshFailed.
func TestClientModel_DetailLiveRefresh_SupersededSeqErrorDropped(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 3
	existing := inFlightDetailFor("req-1")
	m.requestDetail = existing
	m.detailLoading = true

	m = clientUpdate(m, RequestDetailErrorMsg{ID: "req-1", Seq: 2, Err: errors.New("boom")})

	assert.True(t, m.detailLoading, "a superseded-seq error must not mutate loading state")
	assert.Same(t, existing, m.requestDetail)
	assert.False(t, m.detailRefreshFailed, "a superseded-seq error must not set the refresh-failed note")
	assert.Nil(t, m.detailError)
}

// TestClientModel_DetailLiveRefresh_InFlightPayloadAfterFinalDropped pins the
// belt-and-braces content guard: a current-seq, current-ID payload that is
// itself still in-flight cannot supersede an already-displayed FINAL
// snapshot (e.g. a server-side race in GetProxyRequest).
func TestClientModel_DetailLiveRefresh_InFlightPayloadAfterFinalDropped(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 1
	finalDetail := finalDetailFor("req-1")
	m.requestDetail = finalDetail

	m = clientUpdate(m, RequestDetailMsg{ID: "req-1", Seq: 1, Details: inFlightDetailFor("req-1")})

	assert.Same(t, finalDetail, m.requestDetail, "an in-flight payload must not regress an already-final view")
}

// TestClientModel_DetailLiveRefresh_FinalAfterInFlightApplies pins the normal
// success path: a current-seq, current-ID final payload applies over an
// in-flight (or failed) snapshot and clears detailRefreshFailed.
func TestClientModel_DetailLiveRefresh_FinalAfterInFlightApplies(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 1
	m.requestDetail = inFlightDetailFor("req-1")
	m.detailRefreshFailed = true // simulate a prior failed attempt

	final := finalDetailFor("req-1")
	m = clientUpdate(m, RequestDetailMsg{ID: "req-1", Seq: 1, Details: final})

	require.NotNil(t, m.requestDetail)
	assert.False(t, m.requestDetail.InFlight)
	assert.Equal(t, final, m.requestDetail)
	assert.False(t, m.detailRefreshFailed, "a successful apply clears the refresh-failed note")
}

// TestClientModel_DetailLiveRefresh_ErrorWithDisplayedDetailKeepsSnapshot
// pins the refresh-failure UX: a current-seq error that arrives while a
// snapshot is already displayed keeps that snapshot on screen (never the
// error view) and sets detailRefreshFailed, which formatRequestDetail
// renders as a distinct note in place of the "(in flight)" one.
func TestClientModel_DetailLiveRefresh_ErrorWithDisplayedDetailKeepsSnapshot(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 1
	snapshot := inFlightDetailFor("req-1")
	m.requestDetail = snapshot

	m = clientUpdate(m, RequestDetailErrorMsg{ID: "req-1", Seq: 1, Err: errors.New("boom")})

	assert.Same(t, snapshot, m.requestDetail, "the snapshot must be kept, not cleared")
	assert.Nil(t, m.detailError, "the error view must not take over while a snapshot is displayed")
	assert.True(t, m.detailRefreshFailed)
	rendered := strings.Join(m.formatRequestDetail(), "\n")
	assert.Contains(t, rendered, "live refresh failed")
	assert.NotContains(t, rendered, "details arrive on completion")
}

// TestClientModel_DetailLiveRefresh_ErrorWithNothingDisplayedShowsErrorView
// pins that today's initial-failure behavior is preserved: a current-seq
// error with NO snapshot displayed renders the error view.
func TestClientModel_DetailLiveRefresh_ErrorWithNothingDisplayedShowsErrorView(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.detailFetchSeq = 1
	m.detailLoading = true

	m = clientUpdate(m, RequestDetailErrorMsg{ID: "req-1", Seq: 1, Err: errors.New("boom")})

	assert.False(t, m.detailLoading)
	require.Error(t, m.detailError)
	assert.False(t, m.detailRefreshFailed)
	rendered := strings.Join(m.formatRequestDetail(), "\n")
	assert.Contains(t, rendered, "Error: boom")
}

// TestClientModel_DetailLiveRefresh_EscClearsRefreshFailed pins that leaving
// the detail view (esc) clears detailRefreshFailed alongside requestDetail,
// so re-entering the detail on a fresh Enter never inherits a stale note.
func TestClientModel_DetailLiveRefresh_EscClearsRefreshFailed(t *testing.T) {
	stub := &stubTUIClient{}
	m := newClientDetailModel(stub)
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"
	m.requestDetail = inFlightDetailFor("req-1")
	m.detailRefreshFailed = true

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})

	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.False(t, m.detailRefreshFailed)
}
