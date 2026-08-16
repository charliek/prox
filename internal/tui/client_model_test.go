package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/stream"
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
	// announce the default log epoch, then block until cancelled" (onConnect
	// and — for logs — onHandshake fire, no events). A scripted hook owns both
	// calls: invoke onConnect to model an established connection, skip it to
	// model a dead-on-arrival dial; invoke onHandshake to model a C8+ daemon,
	// skip it to model an old one that sends no handshake.
	consumeLogs      func(ctx context.Context, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error
	consumeRequests  func(ctx context.Context, onConnect func(), onEvent func(api.ProxyRequestResponse)) error
	consumeProcesses func(ctx context.Context, onConnect func(), onEvent func(api.ProcessListResponse)) error

	// processesN counts ConsumeProcesses attempts, so a test can watch the
	// processes loop reconnect. getProcessesN counts REST GetProcesses calls,
	// which C12 expects to stay at zero for the whole attach session: the
	// method is no longer on TUIClient at all, and this counter is the
	// runtime half of that proof.
	processesN    int
	requestsN     int
	getProcessesN int

	// snapshot is the requests-sync REST payload (newest-first, as the real
	// endpoint returns). nil means an empty snapshot. snapshotErr, when set,
	// fails every fetch; snapshotCalls counts them. nextBeforeID is the
	// response's pagination cursor (plan 018 D12/C6) — "" (the default) means
	// "reached the ring's oldest record", matching the server's own omitempty
	// wire behavior.
	//
	// snapshotCall is an optional per-call hook — n is the 1-based call count,
	// params is what the caller passed to GetProxyRequests (so a test can see
	// the BeforeID it sent). It runs BEFORE snapshot/snapshotErr/nextBeforeID
	// are read, so a hook may mutate them (e.g. keyed on params.BeforeID) to
	// script a different page per call — C7 uses this to serve
	// before_id-anchored scroll-back pages. snapshotParams records every
	// call's params, in order, mirroring logsParams below.
	snapshot       []api.ProxyRequestResponse
	snapshotErr    error
	nextBeforeID   string
	snapshotCall   func(n int, params domain.ProxyRequestParams)
	snapshotN      int // number of GetProxyRequests calls made
	snapshotParams []domain.ProxyRequestParams

	// logsResponder backs the C9 logs-sync backfill (GetLogs): it owns the
	// response for each call, so a test can serve a different payload per
	// attempt (n is the 1-based call count). nil means an empty response.
	// logsParams records every call's params, in order.
	logsResponder func(n int, params domain.LogParams) (*api.LogsResponse, error)
	logsParams    []domain.LogParams
}

// stubLogEpoch is the stream_id the default ConsumeLogs stub announces. Tests
// that script their own handshake pick their own.
const stubLogEpoch = "epoch-stub"

// GetProcesses is deliberately kept on the stub even though C12 removed it from
// the TUIClient interface: it is the tripwire for a poll creeping back in. If
// some future code path reaches for REST process state, this counter catches it
// (see TestClientModel_NeverPollsProcesses).
func (s *stubTUIClient) GetProcesses() (*api.ProcessListResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getProcessesN++
	return &api.ProcessListResponse{}, nil
}

// getProcessesCalls returns how many times the REST process poll was called.
func (s *stubTUIClient) getProcessesCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getProcessesN
}

// ConsumeProcesses stands in for the processes stream. The default is a
// connected, silent stream: onConnect fires (which is the whole sync barrier
// for this stream) and nothing is ever delivered.
func (s *stubTUIClient) ConsumeProcesses(ctx context.Context, onConnect func(), onEvent func(api.ProcessListResponse)) error {
	s.mu.Lock()
	s.processesN++
	hook := s.consumeProcesses
	s.mu.Unlock()

	if hook != nil {
		return hook(ctx, onConnect, onEvent)
	}
	onConnect()
	<-ctx.Done()
	return ctx.Err()
}

// processesCalls returns how many ConsumeProcesses attempts have been made.
func (s *stubTUIClient) processesCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processesN
}

func (s *stubTUIClient) RestartProcess(string) error { return nil }

func (s *stubTUIClient) ConsumeLogs(ctx context.Context, _ domain.LogParams, onConnect func(), onHandshake func(api.HandshakeResponse), onEvent func(api.LogEntryResponse)) error {
	if s.consumeLogs != nil {
		return s.consumeLogs(ctx, onConnect, onHandshake, onEvent)
	}
	onConnect()
	onHandshake(api.HandshakeResponse{StreamID: stubLogEpoch})
	<-ctx.Done()
	return ctx.Err()
}

// GetLogs serves the logs-sync backfill. Params are recorded so tests can pin
// the full-fetch vs since_seq-resume decision.
func (s *stubTUIClient) GetLogs(ctx context.Context, params domain.LogParams) (*api.LogsResponse, error) {
	s.mu.Lock()
	s.logsParams = append(s.logsParams, params)
	n := len(s.logsParams)
	responder := s.logsResponder
	s.mu.Unlock()

	if responder != nil {
		return responder(n, params)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &api.LogsResponse{}, nil
}

// logsCalls returns the params of every GetLogs call, in order.
func (s *stubTUIClient) logsCalls() []domain.LogParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.LogParams(nil), s.logsParams...)
}

func (s *stubTUIClient) ConsumeProxyRequests(ctx context.Context, _ domain.ProxyRequestParams, onConnect func(), onEvent func(api.ProxyRequestResponse)) error {
	s.mu.Lock()
	s.requestsN++
	hook := s.consumeRequests
	s.mu.Unlock()

	if hook != nil {
		return hook(ctx, onConnect, onEvent)
	}
	onConnect()
	<-ctx.Done()
	return ctx.Err()
}

// requestsCalls returns how many ConsumeProxyRequests attempts have been made —
// the observable that proves a parked requests loop was re-probed.
func (s *stubTUIClient) requestsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestsN
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

func (s *stubTUIClient) GetProxyRequests(ctx context.Context, params domain.ProxyRequestParams) (*api.ProxyRequestsResponse, error) {
	s.mu.Lock()
	s.snapshotN++
	n := s.snapshotN
	s.snapshotParams = append(s.snapshotParams, params)
	hook := s.snapshotCall
	s.mu.Unlock()

	if hook != nil {
		// Runs outside the lock (a hook may block, e.g. waiting on a channel)
		// but BEFORE the response fields are read below, so a scripted hook
		// can mutate snapshot/snapshotErr/nextBeforeID — keyed on n or
		// params.BeforeID — to shape this call's own response.
		hook(n, params)
	}

	s.mu.Lock()
	err, records, next := s.snapshotErr, s.snapshot, s.nextBeforeID
	s.mu.Unlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return &api.ProxyRequestsResponse{Requests: records, NextBeforeID: next}, nil
}

// snapshotCalls returns how many times GetProxyRequests has been called.
func (s *stubTUIClient) snapshotCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotN
}

// snapshotCallParams returns the params of every GetProxyRequests call, in
// order — the observable that proves a scroll-back fetch (C7) sent the
// BeforeID it meant to.
func (s *stubTUIClient) snapshotCallParams() []domain.ProxyRequestParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.ProxyRequestParams(nil), s.snapshotParams...)
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

// attachClientOptions mirrors what runAttach passes RunClient, so model tests
// render the same help and status text the shipped attach TUI does. ShutdownCh
// stays nil, as in attach mode.
func attachClientOptions() ClientOptions {
	return ClientOptions{
		Help: HelpConfig{
			TitleSuffix: "(Client Mode)",
			QuitMessage: "Quit (daemon continues running)",
		},
		ConnectedStatus: "Connected via API",
		// What attach passes when it knows nothing: attachProxyFacts answers
		// true/true for a nil status block or a daemon predating capture_enabled,
		// i.e. "keep the wording that predates these fields".
		ProxyConfigured: true,
		CaptureEnabled:  true,
	}
}

// newClientRequestsModel builds a ClientModel in the requests view holding n
// requests, viewport content sized to viewportHeight (see newRequestsModel).
func newClientRequestsModel(stub *stubTUIClient, n, viewportHeight int) ClientModel {
	m := NewClientModel(stub, attachClientOptions())
	m.viewMode = ViewModeRequests
	m.proxyRequests = makeTestRequests(n)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: viewportHeight + defaultChromeHeight() + defaultPanelBorder() + defaultRequestsHeaderRows()})
	return nm.(ClientModel)
}

func clientUpdate(m ClientModel, msg tea.Msg) ClientModel {
	nm, _ := m.Update(msg)
	return nm.(ClientModel)
}

// --- C12: the poll is gone; process state arrives on a stream ---

// unhandledTickMsg stands in for the periodic tick the deleted local-mode
// poll used to ride (TickMsg no longer exists — 018 C4 deleted local mode
// entirely). Any message type Update doesn't switch on must be just as inert,
// so this unexported, locally-defined type keeps that invariant covered.
type unhandledTickMsg struct{}

// TestClientModel_NeverPollsProcesses is the panel-mandated no-poll proof.
// Init must return no command at all, and no message the model can receive —
// including an unhandled periodic-tick-shaped message — may produce a REST
// process fetch. The compile-level half of the proof is that
// ClientModel.fetchProcesses and TUIClient.GetProcesses no longer exist;
// this is the runtime half.
func TestClientModel_NeverPollsProcesses(t *testing.T) {
	stub := &stubTUIClient{}
	m := NewClientModel(stub, attachClientOptions())

	require.Nil(t, m.Init(), "attach mode has no periodic work left")

	// A synthetic, unhandled message must be inert (the tick case was
	// deleted, not repurposed).
	_, cmd := m.Update(unhandledTickMsg{})
	if cmd != nil {
		// tea.Batch of nothing can still be non-nil; running it must at least
		// never produce a fetch.
		cmd()
	}

	assert.Zero(t, stub.getProcessesCalls(), "nothing in attach mode may poll REST /processes")
}

// TestClientModel_ProcessesMsgUpdatesList pins that a snapshot delivered by the
// stream lands exactly as the poll's result did: the process list is replaced
// wholesale.
func TestClientModel_ProcessesMsgUpdatesList(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, attachClientOptions())

	m = clientUpdate(m, ProcessesMsg([]domain.ProcessInfo{
		{Name: "web", State: domain.ProcessState("running"), PID: 10},
		{Name: "api", State: domain.ProcessState("starting")},
	}))

	require.Len(t, m.processes, 2)
	assert.Equal(t, "web", m.processes[0].Name)
	assert.Equal(t, 10, m.processes[0].PID)

	// A later snapshot replaces the list wholesale (processes can disappear).
	m = clientUpdate(m, ProcessesMsg([]domain.ProcessInfo{{Name: "api"}}))
	require.Len(t, m.processes, 1)
	assert.Equal(t, "api", m.processes[0].Name)
}

// TestClientModel_ConnectionErrorDerivedFromProcessesStream pins C12's derived
// connectionError: the processes stream's health, and nothing else, drives the
// status line's connection notice.
func TestClientModel_ConnectionErrorDerivedFromProcessesStream(t *testing.T) {
	m := readyClientModel()

	// A drop reports the outage, carrying the loop's own error.
	m = clientUpdate(m, StreamStatusMsg{
		Stream: StreamProcesses,
		Status: stream.Status{State: stream.StateReconnecting, Err: errors.New("connection refused")},
	})
	require.Error(t, m.connectionError)
	assert.EqualError(t, m.connectionError, "connection refused")
	assert.Contains(t, m.View(), "Connection error (retrying...)")
	assert.NotContains(t, m.View(), "processes:", "the processes stream never renders its own segment")

	// Recovery clears it.
	m = clientUpdate(m, statusMsg(StreamProcesses, stream.StateOK))
	assert.NoError(t, m.connectionError)
	assert.NotContains(t, m.View(), "Connection error")
}

// TestClientModel_ConnectionErrorCleanDropHasGenericError covers the drop with
// no error of its own (the daemon closed the stream cleanly): the notice still
// has to say something.
func TestClientModel_ConnectionErrorCleanDropHasGenericError(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamProcesses, stream.StateReconnecting))

	require.Error(t, m.connectionError)
	assert.Equal(t, errProcessesStreamLost, m.connectionError)
	assert.Contains(t, m.View(), "Connection error (retrying...)")
}

// TestClientModel_ConnectionErrorClosedStream pins the terminal case: a loop
// that ended (auth failure, cancellation) is still an outage, not a clean state.
func TestClientModel_ConnectionErrorClosedStream(t *testing.T) {
	m := clientUpdate(readyClientModel(), StreamStatusMsg{
		Stream: StreamProcesses,
		Status: stream.Status{State: stream.StateClosed, Err: errors.New("api error 401")},
	})

	assert.EqualError(t, m.connectionError, "api error 401")
	// Terminal means no retry will ever happen — the rendering must not
	// promise one (codex C12 finding).
	assert.Contains(t, m.View(), "Connection lost: api error 401")
	assert.NotContains(t, m.View(), "retrying")
}

// TestClientModel_ConnectionErrorLatchedThroughRetrySyncing pins the outage
// latch (codex C12 finding): the loop emits Syncing before every retry DIALS,
// and a dial against a blackholed daemon can hang for its full header timeout
// — the banner must keep reporting the outage until an attempt actually
// reaches OK.
func TestClientModel_ConnectionErrorLatchedThroughRetrySyncing(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamProcesses, stream.StateReconnecting))
	require.Error(t, m.connectionError)

	m = clientUpdate(m, statusMsg(StreamProcesses, stream.StateSyncing))
	assert.Error(t, m.connectionError, "a retry's pre-dial Syncing must not clear the outage")
	assert.Contains(t, m.View(), "Connection error (retrying...)")

	m = clientUpdate(m, statusMsg(StreamProcesses, stream.StateOK))
	assert.NoError(t, m.connectionError, "only OK clears the outage")
	assert.Contains(t, m.View(), "Connected via API")
}

// TestClientModel_ConnectionErrorOldDaemon pins the version-skew rendering: a
// parked (404) processes stream reports the old daemon by name rather than
// leaving the UI silently frozen on an empty process list.
func TestClientModel_ConnectionErrorOldDaemon(t *testing.T) {
	m := clientUpdate(readyClientModel(), statusMsg(StreamProcesses, stream.StateUnavailable))

	require.Error(t, m.connectionError)
	assert.Equal(t, errProcessesStreamUnsupported, m.connectionError)
	assert.Contains(t, m.connectionError.Error(), "too old")
	// The park never self-heals by waiting, so the actionable hint renders
	// instead of the transient "retrying..." wording.
	assert.Contains(t, m.View(), "too old")
	assert.NotContains(t, m.View(), "Connection error (retrying...)")
}

// TestClientModel_ConnectionErrorIgnoresOtherStreams pins that the derivation
// is processes-only: a dead logs or requests stream degrades its own status-bar
// segment and must not claim the whole connection is down.
func TestClientModel_ConnectionErrorIgnoresOtherStreams(t *testing.T) {
	m := readyClientModel()
	m = clientUpdate(m, statusMsg(StreamLogs, stream.StateReconnecting))
	m = clientUpdate(m, statusMsg(StreamRequests, stream.StateUnavailable))

	assert.NoError(t, m.connectionError)
	view := m.View()
	assert.NotContains(t, view, "Connection error")
	assert.Contains(t, view, "⚠ logs: reconnecting…")
}

// TestClientModel_StartupIsQuiet pins that a fresh attach session shows no
// connection error before any stream has reported anything.
func TestClientModel_StartupIsQuiet(t *testing.T) {
	m := readyClientModel()
	assert.NoError(t, m.connectionError)
	assert.NotContains(t, m.View(), "Connection error")

	m = clientUpdate(m, statusMsg(StreamProcesses, stream.StateConnecting))
	assert.NoError(t, m.connectionError)
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
	m := NewClientModel(stub, attachClientOptions())
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
	m := NewClientModel(stub, attachClientOptions())

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

// --- 018 C1: ClientOptions (help text, status wording, external shutdown) ---

// runCmdWithin runs cmd and returns its message, failing the test if none
// arrives promptly. Every ShutdownCh assertion below goes through it: a
// regression that blocks forever (a waiter watching the wrong channel, or one
// that is never released) must fail the suite fast instead of hanging it.
func runCmdWithin(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	require.NotNil(t, cmd, "expected a command to run")

	msgs := make(chan tea.Msg, 1)
	go func() { msgs <- cmd() }()

	select {
	case msg := <-msgs:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("command produced no message")
		return nil
	}
}

// TestClientModel_InitWithoutShutdownChReturnsNil pins the attach shape: no
// external shutdown channel means Init has nothing at all to do, exactly as
// before ClientOptions existed (and see TestClientModel_NeverPollsProcesses for
// the no-poll half of the same guarantee).
func TestClientModel_InitWithoutShutdownChReturnsNil(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, attachClientOptions())
	assert.Nil(t, m.Init(), "a nil ShutdownCh leaves Init with no work")
}

// TestClientModel_ExternalShutdownQuits pins the program-owned shutdown path
// end to end: Init returns the waiter, closing the channel releases it as
// ExternalShutdownMsg, and Update answers that message with tea.Quit. No
// goroutine outside the program ever touches the program to quit it.
func TestClientModel_ExternalShutdownQuits(t *testing.T) {
	shutdownCh := make(chan struct{})
	opts := attachClientOptions()
	opts.ShutdownCh = shutdownCh
	m := NewClientModel(&stubTUIClient{}, opts)

	cmd := m.Init()
	require.NotNil(t, cmd, "a supervising caller's ShutdownCh must be waited on")

	close(shutdownCh)
	assert.Equal(t, ExternalShutdownMsg{}, runCmdWithin(t, cmd))

	_, quitCmd := m.Update(ExternalShutdownMsg{})
	assert.Equal(t, tea.QuitMsg{}, runCmdWithin(t, quitCmd),
		"an external shutdown leaves through the same quit as q/Ctrl-C")
}

// TestClientModel_ExternalShutdownAlreadyClosed covers the race the waiter has
// to survive: the shutdown request can land before the program starts, so a
// channel already closed at Init time must release the command immediately
// rather than park it.
func TestClientModel_ExternalShutdownAlreadyClosed(t *testing.T) {
	shutdownCh := make(chan struct{})
	close(shutdownCh)

	opts := attachClientOptions()
	opts.ShutdownCh = shutdownCh
	m := NewClientModel(&stubTUIClient{}, opts)

	assert.Equal(t, ExternalShutdownMsg{}, runCmdWithin(t, m.Init()))
}

// TestClientModel_ConnectedStatusFromOptions pins that the healthy status-line
// wording comes from the options rather than being baked into View: `up --tui`
// is not "Connected via API" to a user, even though it runs the same model.
func TestClientModel_ConnectedStatusFromOptions(t *testing.T) {
	opts := attachClientOptions()
	opts.ConnectedStatus = "Managing 2 processes"
	m := NewClientModel(&stubTUIClient{}, opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	m = nm.(ClientModel)

	view := m.View()
	assert.Contains(t, view, "Managing 2 processes")
	assert.NotContains(t, view, "Connected via API", "no wording is hardcoded any more")
}

// TestClientModel_EmptyConnectedStatusRendersNothing pins the zero value: a
// caller that wants no healthy-state wording gets none, not a default.
func TestClientModel_EmptyConnectedStatusRendersNothing(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, ClientOptions{})
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	m = nm.(ClientModel)

	assert.NotContains(t, m.View(), "Connected via API")
}

// TestClientModel_HelpConfigFromOptions pins the help modal's two per-caller
// strings threading through ClientOptions into BaseModel. View() shows BOTH
// live chrome and the help box (plan 022 WS4).
func TestClientModel_HelpConfigFromOptions(t *testing.T) {
	opts := ClientOptions{Help: HelpConfig{
		TitleSuffix: "(Local Mode)",
		QuitMessage: "Quit (stops all processes)",
	}}
	m := NewClientModel(&stubTUIClient{}, opts)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 80})
	m = nm.(ClientModel)
	nm, _ = m.Update(keyRune('?')) // open via the real entry path
	m = nm.(ClientModel)
	require.True(t, m.mode == ModeHelp)

	help := m.View()
	assert.Contains(t, help, "Quit (stops all processes)")
	assert.NotContains(t, help, "(Client Mode)")
	assert.NotContains(t, help, "daemon continues running")
	// TitleSuffix no longer appears in the bordered title (plan 023 B5); border
	// shows the view label only.
	assert.Contains(t, help, "Help — Logs")
	// Live chrome behind the modal (merged footer).
	assert.Contains(t, ansi.Strip(help), "? help")
	assert.Contains(t, ansi.Strip(help), "[FOLLOW]")
	assert.Contains(t, help, helpModalFooter)
}
