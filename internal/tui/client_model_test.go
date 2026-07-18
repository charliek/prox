package tui

import (
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
type stubTUIClient struct {
	mu           sync.Mutex
	requestedIDs []string // every GetProxyRequest id, in call order
	detailResp   *api.ProxyRequestDetailResponse
	detailErr    error
}

func (s *stubTUIClient) GetProcesses() (*api.ProcessListResponse, error) {
	return &api.ProcessListResponse{}, nil
}

func (s *stubTUIClient) RestartProcess(string) error { return nil }

func (s *stubTUIClient) StreamLogsChannel(domain.LogParams) (<-chan api.LogEntryResponse, error) {
	ch := make(chan api.LogEntryResponse)
	close(ch)
	return ch, nil
}

func (s *stubTUIClient) StreamProxyRequestsChannel(domain.ProxyRequestParams) (<-chan api.ProxyRequestResponse, error) {
	ch := make(chan api.ProxyRequestResponse)
	close(ch)
	return ch, nil
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
