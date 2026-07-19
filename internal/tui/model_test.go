package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
)

// newTestModel creates a Model with default test dependencies.
// This reduces boilerplate in tests that need a basic model.
func newTestModel() Model {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())
	return NewModel(sup, logMgr)
}

func TestNewModel(t *testing.T) {
	model := newTestModel()

	assert.Equal(t, ModeNormal, model.mode)
	assert.False(t, model.ready)
	assert.Empty(t, model.logEntries)
}

func TestModel_HandleKey_Quit(t *testing.T) {
	model := newTestModel()

	// Test quit with 'q'
	newModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assert.NotNil(t, cmd)
	_ = newModel
}

func TestModel_HandleKey_ModeSwitch(t *testing.T) {
	model := newTestModel()

	// Test switching to help mode
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m := newModel.(Model)
	assert.Equal(t, ModeHelp, m.mode)

	// Test switching to filter mode
	model = newTestModel()
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = newModel.(Model)
	assert.Equal(t, ModeFilter, m.mode)

	// Test switching to search mode
	model = newTestModel()
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = newModel.(Model)
	assert.Equal(t, ModeSearch, m.mode)

	// Test switching to string filter mode
	model = newTestModel()
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = newModel.(Model)
	assert.Equal(t, ModeStringFilter, m.mode)
}

func TestModel_HandleKey_EscClearsFilters(t *testing.T) {
	model := newTestModel()
	model.soloProcess = "test"
	model.searchPattern = "pattern"

	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m := newModel.(Model)

	assert.Empty(t, m.soloProcess)
	assert.Empty(t, m.searchPattern)
}

func TestModel_LogEntryMsg(t *testing.T) {
	model := newTestModel()
	model.ready = true // Set ready to avoid viewport issues

	entry := domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      "test log line",
	}

	newModel, _ := model.Update(LogEntryMsg(entry))
	m := newModel.(Model)

	assert.Len(t, m.logEntries, 1)
	assert.Equal(t, "test", m.logEntries[0].Process)
	assert.Equal(t, "test log line", m.logEntries[0].Line)
}

func TestModel_LogEntryLimit(t *testing.T) {
	model := newTestModel()
	model.ready = true

	// Add more than 1000 entries
	for i := 0; i < 1005; i++ {
		entry := domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "test",
			Stream:    domain.StreamStdout,
			Line:      "test log line",
		}
		newModel, _ := model.Update(LogEntryMsg(entry))
		model = newModel.(Model)
	}

	// Should be capped at 1000
	assert.Len(t, model.logEntries, 1000)
}

func TestFilteredEntries(t *testing.T) {
	model := newTestModel()

	// Add some log entries
	model.logEntries = []domain.LogEntry{
		{Process: "web", Line: "web log 1"},
		{Process: "api", Line: "api log 1"},
		{Process: "web", Line: "web log 2"},
		{Process: "api", Line: "api log 2"},
	}

	// No filter - should return all
	entries := model.filteredEntries()
	assert.Len(t, entries, 4)

	// Solo process filter
	model.soloProcess = "web"
	entries = model.filteredEntries()
	assert.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, "web", e.Process)
	}

	// String filter
	model.soloProcess = ""
	model.searchPattern = "log 1"
	entries = model.filteredEntries()
	assert.Len(t, entries, 2)
	for _, e := range entries {
		assert.Contains(t, e.Line, "log 1")
	}
}

func TestContainsIgnoreCase(t *testing.T) {
	tests := []struct {
		s      string
		substr string
		want   bool
	}{
		{"Hello World", "world", true},
		{"Hello World", "WORLD", true},
		{"Hello World", "hello", true},
		{"Hello World", "xyz", false},
		{"", "", true},
		{"test", "", true},
		{"", "test", false},
	}

	for _, tt := range tests {
		got := containsIgnoreCase(tt.s, tt.substr)
		assert.Equal(t, tt.want, got, "containsIgnoreCase(%q, %q)", tt.s, tt.substr)
	}
}

func TestFollowModeDefaults(t *testing.T) {
	model := newTestModel()

	// followMode should default to true
	assert.True(t, model.followMode)
}

func TestFollowModeDisabledOnScrollUp(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"k key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}},
		{"up arrow", tea.KeyMsg{Type: tea.KeyUp}},
		{"g key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}},
		{"home key", tea.KeyMsg{Type: tea.KeyHome}},
		{"pgup key", tea.KeyMsg{Type: tea.KeyPgUp}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel()
			assert.True(t, model.followMode) // starts true

			newModel, _ := model.Update(tt.key)
			m := newModel.(Model)

			assert.False(t, m.followMode, "followMode should be false after %s", tt.name)
		})
	}
}

func TestFollowModeEnabledOnGoToBottom(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"G key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}},
		{"end key", tea.KeyMsg{Type: tea.KeyEnd}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel()
			model.followMode = false // start with followMode disabled

			newModel, _ := model.Update(tt.key)
			m := newModel.(Model)

			assert.True(t, m.followMode, "followMode should be true after %s", tt.name)
		})
	}
}

func TestFollowModeToggle(t *testing.T) {
	model := newTestModel()
	assert.True(t, model.followMode) // starts true

	// First toggle - should disable
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m := newModel.(Model)
	assert.False(t, m.followMode)

	// Second toggle - should enable
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m = newModel.(Model)
	assert.True(t, m.followMode)
}

func TestFilteredProxyRequests(t *testing.T) {
	model := newTestModel()

	// Add some proxy requests
	model.proxyRequests = []proxy.RequestRecord{
		{Subdomain: "api", Method: "GET", URL: "/users"},
		{Subdomain: "web", Method: "POST", URL: "/login"},
		{Subdomain: "api", Method: "GET", URL: "/posts"},
		{Subdomain: "admin", Method: "DELETE", URL: "/users/1"},
	}

	// No filter - should return all
	requests := model.filteredProxyRequests()
	assert.Len(t, requests, 4)

	// String filter on URL
	model.searchPattern = "users"
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)

	// String filter on method
	model.searchPattern = "GET"
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
	for _, r := range requests {
		assert.Equal(t, "GET", r.Method)
	}

	// String filter on subdomain
	model.searchPattern = "api"
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
	for _, r := range requests {
		assert.Equal(t, "api", r.Subdomain)
	}

	// Case-insensitive filter
	model.searchPattern = "API"
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
}

func TestProxyRequestBufferLimit(t *testing.T) {
	model := newTestModel()
	model.ready = true

	// Add more than maxProxyRequests (1000) entries
	for i := 0; i < 1005; i++ {
		req := proxy.RequestRecord{
			Timestamp: time.Now(),
			Subdomain: "api",
			Method:    "GET",
			URL:       "/test",
		}
		newModel, _ := model.Update(ProxyRequestMsg(req))
		model = newModel.(Model)
	}

	// Should be capped at 1000
	assert.Len(t, model.proxyRequests, 1000)
}

func TestModel_ProxyRequestMsg(t *testing.T) {
	model := newTestModel()
	model.ready = true

	// Send a proxy request through Update()
	req := proxy.RequestRecord{
		ID:         "req-1",
		Timestamp:  time.Now(),
		Subdomain:  "web",
		Method:     "POST",
		URL:        "/api/users",
		StatusCode: 201,
		Duration:   50 * time.Millisecond,
		RemoteAddr: "192.168.1.1:54321",
	}

	newModel, _ := model.Update(ProxyRequestMsg(req))
	m := newModel.(Model)

	// Verify request was added
	assert.Len(t, m.proxyRequests, 1)
	assert.Equal(t, "web", m.proxyRequests[0].Subdomain)
	assert.Equal(t, "POST", m.proxyRequests[0].Method)
	assert.Equal(t, "/api/users", m.proxyRequests[0].URL)
	assert.Equal(t, 201, m.proxyRequests[0].StatusCode)
	assert.Equal(t, 50*time.Millisecond, m.proxyRequests[0].Duration)

	// Verify request is accessible via filteredProxyRequests
	filtered := m.filteredProxyRequests()
	assert.Len(t, filtered, 1)
	assert.Equal(t, "/api/users", filtered[0].URL)

	// Add another request and verify both are present
	req2 := proxy.RequestRecord{
		ID:         "req-2",
		Timestamp:  time.Now(),
		Subdomain:  "api",
		Method:     "GET",
		URL:        "/health",
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
	}

	newModel, _ = m.Update(ProxyRequestMsg(req2))
	m = newModel.(Model)

	assert.Len(t, m.proxyRequests, 2)
	filtered = m.filteredProxyRequests()
	assert.Len(t, filtered, 2)

	// Test filtering
	m.searchPattern = "users"
	filtered = m.filteredProxyRequests()
	assert.Len(t, filtered, 1)
	assert.Equal(t, "/api/users", filtered[0].URL)

	// A same-ID re-record (e.g. an in-flight row's completion event) updates
	// the row in place rather than appending a duplicate.
	req1Updated := req
	req1Updated.StatusCode = 204
	req1Updated.Duration = 99 * time.Millisecond

	newModel, _ = m.Update(ProxyRequestMsg(req1Updated))
	m = newModel.(Model)

	assert.Len(t, m.proxyRequests, 2, "same-ID update must not duplicate the row")
	assert.Equal(t, 204, m.proxyRequests[0].StatusCode)
	assert.Equal(t, 99*time.Millisecond, m.proxyRequests[0].Duration)
}

// TestModel_ProxyRequestMsg_SelectionStable pins the cursor invariant (D11):
// the ID-anchored cursor survives an in-place upsert of ANOTHER row and of its
// OWN row without moving, so an arriving completion never changes what Enter
// opens.
func TestModel_ProxyRequestMsg_SelectionStable(t *testing.T) {
	m := newRequestsModel(3, 20)
	m = updateModel(m, keyRune('g')) // cursor row 0, follow off
	m = updateModel(m, keyRune('j')) // cursor row 1 (req-001), not the last row
	assert.Equal(t, "req-001", m.cursorID)
	assert.Equal(t, 1, m.cursorIdx)
	assert.False(t, m.followMode)

	// In-place upsert of ANOTHER row (row 0 completing) must not move the cursor.
	other := m.proxyRequests[0]
	other.StatusCode = 204
	m = updateModel(m, ProxyRequestMsg(other))
	assert.Len(t, m.proxyRequests, 3, "in-place update must not change the row count")
	assert.Equal(t, "req-001", m.cursorID, "cursor stays on its row by ID")
	assert.Equal(t, 1, m.cursorIdx)
	assert.Equal(t, 204, m.proxyRequests[0].StatusCode, "the target row must reflect the update")
	assert.Equal(t, "req-002", m.proxyRequests[2].ID, "other rows keep their index")

	// In-place upsert of the cursor's OWN row must not move it either.
	own := m.proxyRequests[1]
	own.StatusCode = 500
	m = updateModel(m, ProxyRequestMsg(own))
	assert.Len(t, m.proxyRequests, 3)
	assert.Equal(t, "req-001", m.cursorID)
	assert.Equal(t, 1, m.cursorIdx)
	assert.Equal(t, 500, m.proxyRequests[1].StatusCode)
}

func TestViewModeSwitch(t *testing.T) {
	model := newTestModel()

	// Default view mode is Logs
	assert.Equal(t, ViewModeLogs, model.viewMode)

	// Tab key switches to Requests view
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	m := newModel.(Model)
	assert.Equal(t, ViewModeRequests, m.viewMode)

	// Tab again switches back to Logs view
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = newModel.(Model)
	assert.Equal(t, ViewModeLogs, m.viewMode)
}

func TestFormatProxyRequest_StatusCode0(t *testing.T) {
	model := newTestModel()

	// Status code 0 indicates connection error or timeout
	req := proxy.RequestRecord{
		Timestamp:  time.Now(),
		Subdomain:  "api",
		Method:     "GET",
		URL:        "/test",
		StatusCode: 0,
		Duration:   100 * time.Millisecond,
	}

	formatted := model.formatProxyRequest(req)

	// Status 0 should be formatted (verify it doesn't panic and contains expected fields)
	assert.Contains(t, formatted, "api")
	assert.Contains(t, formatted, "GET")
	assert.Contains(t, formatted, "/test")
	assert.Contains(t, formatted, "  0") // Status code 0 with 3-char right-aligned padding

	// Verify exact padding for subdomain (10 chars left-aligned)
	// "api" should be followed by 7 spaces to make 10 chars total
	assert.Contains(t, formatted, "api       ") // 10 chars total

	// Verify exact padding for method (7 chars left-aligned)
	// "GET" should be followed by 4 spaces to make 7 chars total
	assert.Contains(t, formatted, "GET    ") // 7 chars total

	// Verify duration is 5 chars right-aligned (100ms = "  100")
	assert.Contains(t, formatted, "  100")
}

func TestFormatProxyRequest_DurationOverflow(t *testing.T) {
	model := newTestModel()

	// Duration exceeding 9999ms should show "9999+"
	req := proxy.RequestRecord{
		Timestamp:  time.Now(),
		Subdomain:  "api",
		Method:     "POST",
		URL:        "/slow-endpoint",
		StatusCode: 200,
		Duration:   15 * time.Second, // 15000ms > 9999ms
	}

	formatted := model.formatProxyRequest(req)

	// Should contain "9999+" for overflow duration (5 chars total)
	assert.Contains(t, formatted, "9999+")
	assert.Contains(t, formatted, "api")
	assert.Contains(t, formatted, "POST")

	// Verify exact padding for subdomain (10 chars left-aligned)
	assert.Contains(t, formatted, "api       ") // 10 chars total

	// Verify exact padding for method (7 chars left-aligned)
	// "POST" should be followed by 3 spaces to make 7 chars total
	assert.Contains(t, formatted, "POST   ") // 7 chars total

	// Verify status code is 3 chars right-aligned
	assert.Contains(t, formatted, "200")
}

func TestFormatProxyRequest_InFlight(t *testing.T) {
	model := newTestModel()

	// An in-flight record carries the real header-time status but no
	// duration yet; the duration column renders dots instead of digits,
	// padded to the same 5-char width as the completed-request case.
	req := proxy.RequestRecord{
		Timestamp:  time.Now(),
		Subdomain:  "api",
		Method:     "GET",
		URL:        "/stream",
		StatusCode: 200,
		InFlight:   true,
	}

	formatted := model.formatProxyRequest(req)

	assert.Contains(t, formatted, "  ...ms", "duration column should render dots, 5-char padded, with the ms suffix")
	assert.NotContains(t, formatted, "0ms", "in-flight rows must not render a fake zero duration")
	assert.Contains(t, formatted, "200", "status should show the real header-time code")
}

func TestFormatProxyRequest_Padding(t *testing.T) {
	model := newTestModel()

	tests := []struct {
		name       string
		subdomain  string
		method     string
		statusCode int
		durationMs int64
		wantSub    string // Expected subdomain with padding (10 chars)
		wantMethod string // Expected method with padding (7 chars)
		wantStatus string // Expected status with padding (3 chars)
		wantDur    string // Expected duration with padding (5 chars)
	}{
		{
			name:       "short fields",
			subdomain:  "a",
			method:     "GET",
			statusCode: 200,
			durationMs: 1,
			wantSub:    "a         ", // 1 + 9 spaces
			wantMethod: "GET    ",    // 3 + 4 spaces
			wantStatus: "200",        // already 3 chars
			wantDur:    "    1",      // 4 spaces + 1
		},
		{
			name:       "max length subdomain",
			subdomain:  "webservice",
			method:     "DELETE",
			statusCode: 404,
			durationMs: 9999,
			wantSub:    "webservice", // exactly 10 chars
			wantMethod: "DELETE ",    // 6 + 1 space
			wantStatus: "404",
			wantDur:    " 9999", // 1 space + 4 digits
		},
		{
			name:       "single digit status",
			subdomain:  "api",
			method:     "OPTIONS",
			statusCode: 0,
			durationMs: 50,
			wantSub:    "api       ",
			wantMethod: "OPTIONS", // exactly 7 chars
			wantStatus: "  0",     // 2 spaces + 0
			wantDur:    "   50",   // 3 spaces + 50
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := proxy.RequestRecord{
				Timestamp:  time.Now(),
				Subdomain:  tt.subdomain,
				Method:     tt.method,
				StatusCode: tt.statusCode,
				Duration:   time.Duration(tt.durationMs) * time.Millisecond,
			}

			formatted := model.formatProxyRequest(req)

			assert.Contains(t, formatted, tt.wantSub, "subdomain padding")
			assert.Contains(t, formatted, tt.wantMethod, "method padding")
			assert.Contains(t, formatted, tt.wantStatus, "status padding")
			assert.Contains(t, formatted, tt.wantDur, "duration padding")
		})
	}
}

// --- Requests-view cursor tests (C2 / D6-D11) ---

// keyRune builds a rune KeyMsg for a single-character key.
func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// updateModel applies a message to a Model and returns the concrete result.
func updateModel(m Model, msg tea.Msg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

// newRequestsModel builds a Model in the requests view holding n requests
// (IDs req-000..req-{n-1}, URLs /path/000..), with the viewport sized so its
// content Height is viewportHeight (handleWindowSize subtracts the 6-row
// header+footer margin). followMode starts true, so the cursor begins pinned
// to the newest row.
func newRequestsModel(n, viewportHeight int) Model {
	return newSearchModel(viewportHeight, makeTestRequests(n))
}

// makeTestRequests builds n proxy request records with the shared fixture
// convention (IDs req-000.., URLs /path/000.., monotonic timestamps). Shared by
// both the Model and ClientModel requests-view test constructors.
func makeTestRequests(n int) []proxy.RequestRecord {
	reqs := make([]proxy.RequestRecord, n)
	base := time.Unix(0, 0)
	for i := 0; i < n; i++ {
		reqs[i] = proxy.RequestRecord{
			ID:         fmt.Sprintf("req-%03d", i),
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			Method:     "GET",
			URL:        fmt.Sprintf("/path/%03d", i),
			StatusCode: 200,
			Duration:   5 * time.Millisecond,
		}
	}
	return reqs
}

// cursorVisible reports whether the cursor row is within the viewport window.
func cursorVisible(m Model) bool {
	yo := m.viewport.YOffset
	return m.cursorIdx >= yo && m.cursorIdx < yo+m.viewport.Height
}

func newArrival(id, url string) proxy.RequestRecord {
	return proxy.RequestRecord{ID: id, Timestamp: time.Now(), Method: "GET", URL: url, StatusCode: 200}
}

func TestRequestsCursor_Movement(t *testing.T) {
	m := newRequestsModel(10, 5)
	assert.Equal(t, 9, m.cursorIdx, "follow pins the cursor to the newest row")

	m = updateModel(m, keyRune('k'))
	assert.Equal(t, 8, m.cursorIdx)
	assert.False(t, m.followMode, "k disengages follow")

	m = updateModel(m, keyRune('k'))
	assert.Equal(t, 7, m.cursorIdx)

	m = updateModel(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.False(t, m.followMode)

	// Clamp at the top.
	m = updateModel(m, keyRune('k'))
	assert.Equal(t, 0, m.cursorIdx)

	// Half-page paging: step = Height/2 = 5/2 = 2.
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyPgDown})
	assert.Equal(t, 2, m.cursorIdx)
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyPgUp})
	assert.Equal(t, 0, m.cursorIdx)
	assert.False(t, m.followMode)

	m = updateModel(m, keyRune('G'))
	assert.Equal(t, 9, m.cursorIdx)
	assert.True(t, m.followMode, "G re-engages follow")

	// Clamp at the bottom.
	m = updateModel(m, keyRune('j'))
	assert.Equal(t, 9, m.cursorIdx)
}

func TestRequestsCursor_VisibilityKeyboardJumps(t *testing.T) {
	m := newRequestsModel(30, 5)

	m = updateModel(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset)
	assert.True(t, cursorVisible(m))

	// Walk the cursor down; it must stay on-screen with minimal scrolling.
	for i := 1; i <= 20; i++ {
		m = updateModel(m, keyRune('j'))
		assert.Equal(t, i, m.cursorIdx)
		assert.True(t, cursorVisible(m),
			"cursor %d must be visible (YOffset %d, height %d)", i, m.viewport.YOffset, m.viewport.Height)
	}

	m = updateModel(m, keyRune('G'))
	assert.Equal(t, 29, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	m = updateModel(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset)
}

// TestRequestsCursor_VisibilityTabIn covers the invariant across a tab-in from a
// scrolled logs view: the shared viewport retains the logs YOffset, and the
// requests branch of updateViewport must scroll it so the cursor is visible.
func TestRequestsCursor_VisibilityTabIn(t *testing.T) {
	m := newTestModel()
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 11}) // viewport height 5

	// Fill logs and requests while in the logs view.
	for i := 0; i < 30; i++ {
		m = updateModel(m, LogEntryMsg(domain.LogEntry{Timestamp: time.Now(), Process: "p", Line: fmt.Sprintf("log %d", i)}))
	}
	for i := 0; i < 30; i++ {
		m = updateModel(m, ProxyRequestMsg(newArrival(fmt.Sprintf("req-%03d", i), fmt.Sprintf("/path/%03d", i))))
	}

	// Scroll the logs viewport up (disengages follow) so YOffset is large.
	m = updateModel(m, keyRune('k'))
	require.Greater(t, m.viewport.YOffset, 5, "logs viewport should be scrolled well down")
	require.False(t, m.followMode)

	// Tab into the requests view: cursor resolves to row 0 (follow off, no prior
	// cursor) and the viewport must scroll up to show it.
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset, "tab-in must reveal the cursor row")
}

func TestRequestsCursor_VisibilityResize(t *testing.T) {
	m := newRequestsModel(40, 10)
	m = updateModel(m, keyRune('g')) // follow off, cursor 0
	for i := 0; i < 20; i++ {
		m = updateModel(m, keyRune('j'))
	}
	require.Equal(t, 20, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// Shrink the window: cursor must stay visible.
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 5 + 6})
	assert.Equal(t, 20, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// Grow it back: still visible, and the viewport must not be left scrolled
	// past the true bottom (a grown window shrinks the valid max YOffset —
	// blank overscroll would report the cursor "visible" while showing gaps).
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 30 + 6})
	assert.Equal(t, 20, m.cursorIdx)
	assert.True(t, cursorVisible(m))
	maxOffset := m.viewport.TotalLineCount() - m.viewport.Height
	if maxOffset < 0 {
		maxOffset = 0
	}
	assert.LessOrEqual(t, m.viewport.YOffset, maxOffset,
		"viewport must not overscroll past the last valid offset after a grow-resize")
}

func TestRequestsCursor_VisibilityFilterClear(t *testing.T) {
	m := newRequestsModel(40, 5)
	// Narrow to rows /path/030../path/039 (10 rows), follow off.
	m.searchPattern = "/path/03"
	m.followMode = false
	m.updateViewport()

	m = updateModel(m, keyRune('g')) // filtered row 0 = req-030
	m = updateModel(m, keyRune('j')) // req-031
	m = updateModel(m, keyRune('j')) // req-032
	require.Equal(t, "req-032", m.cursorID)
	require.False(t, m.followMode)

	// esc clears the filter; the cursor's row (req-032, full-list index 32) must
	// remain and be scrolled on-screen.
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.searchPattern)
	assert.Equal(t, "req-032", m.cursorID)
	assert.Equal(t, 32, m.cursorIdx)
	assert.True(t, cursorVisible(m))
}

func TestRequestsCursor_VisibilityDetailReturn(t *testing.T) {
	m := newRequestsModel(30, 5)
	m = updateModel(m, keyRune('g'))
	for i := 0; i < 15; i++ {
		m = updateModel(m, keyRune('j'))
	}
	require.Equal(t, "req-015", m.cursorID)

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)

	// esc back to the list: the cursor must be restored and visible.
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, "req-015", m.cursorID)
	assert.True(t, cursorVisible(m))
}

// TestRequestsCursor_ArrivalDoesNotMoveShortList pins the CodeRabbit M1 trap:
// with a short list that fits the viewport, AtBottom() is always true, so an
// isNearBottom-based follow check would re-engage follow and yank the cursor on
// every arrival. Arrivals must leave a user-positioned cursor untouched.
func TestRequestsCursor_ArrivalDoesNotMoveShortList(t *testing.T) {
	m := newRequestsModel(3, 20) // fits the viewport
	m = updateModel(m, keyRune('k'))
	require.Equal(t, 1, m.cursorIdx)
	require.False(t, m.followMode)
	require.True(t, m.viewport.AtBottom(), "precondition: a short list is always AtBottom")

	m = updateModel(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, 4)
	assert.Equal(t, "req-001", m.cursorID, "arrival must not move the cursor")
	assert.Equal(t, 1, m.cursorIdx)
	assert.False(t, m.followMode, "arrival must not re-engage follow")
}

func TestRequestsCursor_JOntoLastRowReengagesFollow(t *testing.T) {
	m := newRequestsModel(5, 20)
	m = updateModel(m, keyRune('g'))
	require.False(t, m.followMode)

	for i := 1; i <= 3; i++ {
		m = updateModel(m, keyRune('j'))
		assert.False(t, m.followMode, "follow stays off until the cursor lands on the last row (idx %d)", m.cursorIdx)
	}
	require.Equal(t, 3, m.cursorIdx)

	m = updateModel(m, keyRune('j')) // onto the last row
	assert.Equal(t, 4, m.cursorIdx)
	assert.True(t, m.followMode, "j onto the newest row re-engages follow")
}

func TestRequestsCursor_GAndFReengageFollow(t *testing.T) {
	m := newRequestsModel(10, 5)
	m = updateModel(m, keyRune('k'))
	require.False(t, m.followMode)

	m = updateModel(m, keyRune('G'))
	assert.True(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)

	m = updateModel(m, keyRune('g')) // follow off, cursor 0
	require.False(t, m.followMode)
	m = updateModel(m, keyRune('F')) // toggle on -> pins to newest
	assert.True(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)
	m = updateModel(m, keyRune('F')) // toggle off
	assert.False(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)
}

func TestRequestsCursor_FollowArrivalPinsNewest(t *testing.T) {
	m := newRequestsModel(5, 20)
	require.True(t, m.followMode)
	require.Equal(t, 4, m.cursorIdx)

	m = updateModel(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, 6)
	assert.Equal(t, 5, m.cursorIdx, "follow pins the cursor to the newest row")
	assert.Equal(t, "req-new", m.cursorID)
	assert.True(t, m.viewport.AtBottom(), "follow keeps the viewport at the bottom")
}

func TestRequestsCursor_AppendTrimMidList(t *testing.T) {
	m := newRequestsModel(maxProxyRequests, 5) // exactly full
	m.followMode = false
	m.setRequestCursor(m.filteredProxyRequests(), 500)
	m.updateViewport()
	require.Equal(t, "req-500", m.cursorID)

	// A new arrival appends and trims the oldest row; the ID-anchored cursor
	// rides down one index onto its still-present row.
	m = updateModel(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, maxProxyRequests)
	assert.Equal(t, "req-500", m.cursorID)
	assert.Equal(t, 499, m.cursorIdx)
}

func TestRequestsCursor_TrimmedRowClamps(t *testing.T) {
	m := newRequestsModel(maxProxyRequests, 5)
	m.followMode = false
	m.setRequestCursor(m.filteredProxyRequests(), 0) // cursor on the oldest row
	m.updateViewport()
	require.Equal(t, "req-000", m.cursorID)

	// The arrival trims req-000 away; the cursor clamps to the row now at idx 0.
	m = updateModel(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, maxProxyRequests)
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, "req-001", m.cursorID, "cursor re-anchors to the row now at its index")
}

func TestRequestsCursor_EmptyToNonEmptyFollowOn(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 11})
	require.True(t, m.followMode)
	require.Equal(t, -1, m.cursorIdx, "empty list is the no-cursor sentinel")

	m = updateModel(m, ProxyRequestMsg(newArrival("req-x", "/x")))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, "req-x", m.cursorID, "follow pins the first arrival")
}

func TestRequestsCursor_EmptyToNonEmptyFollowOff(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.followMode = false
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 11})
	require.Equal(t, -1, m.cursorIdx)

	m = updateModel(m, ProxyRequestMsg(newArrival("req-x", "/x")))
	assert.Equal(t, 0, m.cursorIdx, "follow off lands the cursor on row 0")
	assert.Equal(t, "req-x", m.cursorID)
}

func TestRequestsCursor_EnterOpensCursorRow(t *testing.T) {
	m := newRequestsModel(10, 5)
	m = updateModel(m, keyRune('g'))
	for i := 0; i < 4; i++ {
		m = updateModel(m, keyRune('j'))
	}
	require.Equal(t, "req-004", m.cursorID)

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
	assert.Equal(t, "req-004", m.selectedRequestID)
	if assert.NotNil(t, m.requestDetail) {
		assert.Equal(t, "req-004", m.requestDetail.ID)
	}
}

func TestRequestsCursor_EnterGotoTop(t *testing.T) {
	m := newRequestsModel(30, 5) // follow on -> cursor deep, viewport scrolled down
	require.Greater(t, m.viewport.YOffset, 0)

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
	assert.Equal(t, 0, m.viewport.YOffset, "entering the detail view starts at the top")
}

func TestRequestsCursor_EmptyListEnterNoop(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 11})

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeRequests, m.viewMode, "Enter on an empty list is a no-op")
	assert.Empty(t, m.selectedRequestID)
}

// TestRequestsCursor_DetailArrivalNoScroll pins the m4 scroll-yank guard: while
// a detail view is open, an arriving proxy request updates only the list data
// and must not scroll the detail viewport out from under the reader.
func TestRequestsCursor_DetailArrivalNoScroll(t *testing.T) {
	m := newRequestsModel(10, 2) // tiny viewport so the detail scrolls
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)

	// Reader scrolls down inside the detail.
	m.viewport.SetYOffset(3)
	require.Equal(t, 3, m.viewport.YOffset, "precondition: detail content taller than the viewport")

	m = updateModel(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Equal(t, 3, m.viewport.YOffset, "arrival must not scroll the open detail view")
	assert.Len(t, m.proxyRequests, 11, "list data is still updated")
}

func TestRequestsCursor_MarkerOnCursorRow(t *testing.T) {
	m := newRequestsModel(5, 20) // all rows visible
	m = updateModel(m, keyRune('g'))
	m = updateModel(m, keyRune('j')) // cursor on row 1 (req-001, /path/001)
	require.Equal(t, 1, m.cursorIdx)

	view := m.viewport.View()
	markerCount := 0
	var markerLine string
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "❯") {
			markerCount++
			markerLine = line
		}
	}
	assert.Equal(t, 1, markerCount, "exactly one cursor marker is rendered")
	assert.Contains(t, markerLine, "/path/001", "the marker sits on the cursor row")
}

// --- Requests-view search navigation tests (C3 / D12-D13) ---

// newSearchModel builds a Model in the requests view holding the given request
// records, viewport sized to viewportHeight, follow-mode default (true) so the
// cursor starts pinned to the newest row.
func newSearchModel(viewportHeight int, reqs []proxy.RequestRecord) Model {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.proxyRequests = reqs
	return updateModel(m, tea.WindowSizeMsg{Width: 120, Height: viewportHeight + 6})
}

// commitSearch drives the `/`-search flow: enter search mode, set the query,
// and press Enter. handleSearchKey's enter case routes the commit itself
// (jump in the requests view, filter in the logs view) based on m.viewMode,
// so one driver helper serves both views.
func commitSearch(m Model, query string) Model {
	m = updateModel(m, keyRune('/'))
	m.textInput.SetValue(query)
	return updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
}

// searchFixture builds requests where /alpha matches rows 0 and 2 (with a POST
// at row 3 and /beta rows between), for at-or-after and n/N tests.
func searchFixture() []proxy.RequestRecord {
	base := time.Unix(0, 0)
	specs := []struct {
		id, method, url string
	}{
		{"r0", "GET", "/alpha"},
		{"r1", "GET", "/beta"},
		{"r2", "GET", "/alpha/2"},
		{"r3", "POST", "/gamma"},
	}
	reqs := make([]proxy.RequestRecord, len(specs))
	for i, s := range specs {
		reqs[i] = proxy.RequestRecord{
			ID: s.id, Timestamp: base.Add(time.Duration(i) * time.Second),
			Method: s.method, URL: s.url, StatusCode: 200, Duration: 5 * time.Millisecond,
		}
	}
	return reqs
}

func TestRequestsSearch_JumpAtOrAfterAndWrap(t *testing.T) {
	m := newSearchModel(20, searchFixture())
	m = updateModel(m, keyRune('g')) // cursor row 0 (/alpha), follow off
	require.Equal(t, "r0", m.cursorID)

	// Cursor already sits on a match: at-or-after leaves it in place.
	m = commitSearch(m, "alpha")
	assert.Equal(t, "alpha", m.requestSearchQuery)
	assert.Equal(t, "r0", m.cursorID, "at-or-after stays on a cursor that already matches")

	// From row 1 (/beta, no match), the next match at-or-after is row 2.
	m = updateModel(m, keyRune('g'))
	m = updateModel(m, keyRune('j')) // row 1
	require.Equal(t, "r1", m.cursorID)
	m = commitSearch(m, "alpha")
	assert.Equal(t, "r2", m.cursorID, "jumps forward to the next match")

	// From row 3 (/gamma, no match), at-or-after wraps to row 0.
	m = updateModel(m, keyRune('j')) // row 3 (last)
	require.Equal(t, "r3", m.cursorID)
	m = commitSearch(m, "alpha")
	assert.Equal(t, "r0", m.cursorID, "at-or-after wraps around to the first match")
}

func TestRequestsSearch_NextPrevWrap(t *testing.T) {
	m := newSearchModel(20, searchFixture())
	m = updateModel(m, keyRune('g')) // row 0
	m = commitSearch(m, "alpha")
	require.Equal(t, "r0", m.cursorID)

	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "r2", m.cursorID, "n advances to the next match")
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "r0", m.cursorID, "n wraps to the first match")

	m = updateModel(m, keyRune('N'))
	assert.Equal(t, "r2", m.cursorID, "N wraps backward to the last match")
	m = updateModel(m, keyRune('N'))
	assert.Equal(t, "r0", m.cursorID, "N retreats to the previous match")
}

func TestRequestsSearch_OffScreenScrollsMinimally(t *testing.T) {
	reqs := makeTestRequests(30)
	reqs[2].URL = "/target/a"
	reqs[25].URL = "/target/b"
	m := newSearchModel(5, reqs) // viewport height 5
	m = updateModel(m, keyRune('g'))
	require.Equal(t, 0, m.viewport.YOffset)

	// Jump to the first match (row 2), already on-screen: no scroll.
	m = commitSearch(m, "target")
	assert.Equal(t, 2, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// n jumps down to row 25 off-screen: scroll the minimum (YOffset = 25-5+1).
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 25, m.cursorIdx)
	assert.True(t, cursorVisible(m))
	assert.Equal(t, 21, m.viewport.YOffset, "downward jump scrolls minimally")

	// N jumps back up to row 2 off-screen: scroll up to place it at the top.
	m = updateModel(m, keyRune('N'))
	assert.Equal(t, 2, m.cursorIdx)
	assert.True(t, cursorVisible(m))
	assert.Equal(t, 2, m.viewport.YOffset, "upward jump scrolls minimally")
}

func TestRequestsSearch_ComposesWithFilter(t *testing.T) {
	reqs := makeTestRequests(20)
	for i := range reqs {
		if i%2 == 0 {
			reqs[i].Subdomain = "api"
		} else {
			reqs[i].Subdomain = "web"
		}
	}
	reqs[4].URL = "/match/a"  // api  -> in the filtered list
	reqs[5].URL = "/match/b"  // web  -> filtered OUT, must never be selected
	reqs[12].URL = "/match/c" // api  -> in the filtered list

	m := newSearchModel(40, reqs)
	m.searchPattern = "api" // active `s` filter: only the 10 api rows remain
	m.followMode = false
	m.updateViewport()
	m = updateModel(m, keyRune('g'))

	// Matches are computed over the FILTERED list, so /match/b (web) is skipped.
	m = commitSearch(m, "match")
	assert.Equal(t, "req-004", m.cursorID, "search over the filtered list finds the first api match")
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "req-012", m.cursorID)
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "req-004", m.cursorID, "wraps over api matches only, skipping the filtered-out web row")
	assert.Equal(t, "api", m.searchPattern, "the `s` filter is untouched")
}

func TestRequestsSearch_DoesNotFilter(t *testing.T) {
	reqs := makeTestRequests(10)
	reqs[3].URL = "/needle"
	m := newSearchModel(20, reqs)
	before := len(m.filteredProxyRequests())

	m = commitSearch(m, "needle")
	assert.Empty(t, m.searchPattern, "/ in the requests view must not touch the `s` filter")
	assert.Equal(t, "needle", m.requestSearchQuery)
	assert.Len(t, m.filteredProxyRequests(), before, "/ navigates; it must not hide rows")
	assert.Len(t, m.proxyRequests, 10)
}

func TestRequestsSearch_NoMatch(t *testing.T) {
	reqs := makeTestRequests(10)
	m := newSearchModel(20, reqs)
	m = updateModel(m, keyRune('g'))
	m = updateModel(m, keyRune('j')) // row 1
	require.Equal(t, 1, m.cursorIdx)

	m = commitSearch(m, "zzz-nomatch")
	assert.Equal(t, 1, m.cursorIdx, "a query with no match leaves the cursor unmoved")
	assert.Equal(t, "req-001", m.cursorID)

	// n/N are also no-ops when nothing matches.
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 1, m.cursorIdx)

	bar := m.statusBar("")
	assert.Contains(t, bar, "/zzz-nomatch (0 matches)", "status shows the 0-match form")
}

func TestRequestsSearch_EscClearsQuery(t *testing.T) {
	reqs := makeTestRequests(10)
	reqs[3].URL = "/needle"
	m := newSearchModel(20, reqs)
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.requestSearchQuery)

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.requestSearchQuery, "esc clears the requests-view search query")
}

// --- Logs-view search navigation tests (C4 / D6-D10) ---

// newLogsModel builds a Model in the (default) logs view, viewport sized so its
// content height is viewportHeight (handleWindowSize subtracts the 6-row
// margin), then streams the given lines through handleLogEntry so each entry is
// stamped with a unique non-zero Seq — the logs search cursor anchors by Seq, so
// a zero Seq would break the anchor. followMode starts true (default), pinning
// the cursor to the newest line until a jump or scroll disengages it.
func newLogsModel(viewportHeight int, lines []string) Model {
	m := newTestModel()
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: viewportHeight + 6})
	base := time.Unix(0, 0)
	for i, line := range lines {
		m = updateModel(m, LogEntryMsg(domain.LogEntry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Process:   "p",
			Line:      line,
		}))
	}
	return m
}

// logCursorLine returns the line the logs search cursor currently sits on
// (resolved against the filtered list), for assertions that care about which
// line was landed rather than its shifting index.
func logCursorLine(t *testing.T, m Model) string {
	t.Helper()
	fe := m.filteredEntries()
	require.GreaterOrEqual(t, m.logCursorIdx, 0)
	require.Less(t, m.logCursorIdx, len(fe))
	return fe[m.logCursorIdx].Line
}

// TestLogsSearch_SlashNavigates replaces the old TestLogsSearch_SlashStillFilters:
// logs `/` no longer filters — it navigates (jumps the cursor) and leaves every
// line visible, mirroring the requests view (D6).
func TestLogsSearch_SlashNavigates(t *testing.T) {
	m := newLogsModel(20, []string{"keep me", "drop it", "keep this too"})
	require.Equal(t, ViewModeLogs, m.viewMode)
	before := len(m.filteredEntries())

	m = commitSearch(m, "drop")
	assert.Equal(t, "drop", m.logSearchQuery, "logs-view / commits the navigation query")
	assert.Empty(t, m.searchPattern, "logs-view / must not touch the `s` filter")
	assert.Empty(t, m.requestSearchQuery, "logs-view / must not set the requests query")
	assert.Len(t, m.filteredEntries(), before, "logs-view / navigates; it must not hide lines")
	assert.Equal(t, "drop it", logCursorLine(t, m), "the cursor lands on the matching line")
}

func TestLogsSearch_JumpAtOrAfterAndWrap(t *testing.T) {
	// "needle" matches rows 0 and 2; rows 1 and 3 do not.
	m := newLogsModel(20, []string{"needle a", "x", "needle b", "z"})
	m = updateModel(m, keyRune('g')) // top of viewport, follow off; origin -> row 0
	require.False(t, m.followMode)

	// At-or-after from the top: row 0 already matches, so the cursor stays put.
	m = commitSearch(m, "needle")
	assert.Equal(t, "needle", m.logSearchQuery)
	assert.Equal(t, 0, m.logCursorIdx, "at-or-after stays on a matching origin")

	// Park the cursor on the last row (row 3, no "needle") via a throwaway query,
	// then a fresh "needle" at-or-after finds nothing to the end and WRAPS to row 0.
	m = commitSearch(m, "z")
	require.Equal(t, 3, m.logCursorIdx)
	m = commitSearch(m, "needle")
	assert.Equal(t, 0, m.logCursorIdx, "at-or-after wraps around to the first match")

	// Park on row 1 (no match), then at-or-after jumps FORWARD to the next match (row 2).
	m = commitSearch(m, "x")
	require.Equal(t, 1, m.logCursorIdx)
	m = commitSearch(m, "needle")
	assert.Equal(t, 2, m.logCursorIdx, "at-or-after jumps forward to the next match")
}

func TestLogsSearch_NextPrevWrap(t *testing.T) {
	m := newLogsModel(20, []string{"needle a", "x", "needle b", "z"})
	m = updateModel(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, 0, m.logCursorIdx)

	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 2, m.logCursorIdx, "n advances to the next match")
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 0, m.logCursorIdx, "n wraps to the first match")

	m = updateModel(m, keyRune('N'))
	assert.Equal(t, 2, m.logCursorIdx, "N wraps backward to the last match")
	m = updateModel(m, keyRune('N'))
	assert.Equal(t, 0, m.logCursorIdx, "N retreats to the previous match")
}

func TestLogsSearch_ComposesWithFilter(t *testing.T) {
	// The `s` filter keeps only "keep" lines; search then navigates WITHIN that
	// filtered set, never selecting a hidden "drop" row.
	m := newLogsModel(20, []string{
		"keep needle a", "drop needle b", "keep plain", "keep needle c", "drop needle d",
	})
	m.searchPattern = "keep" // active `s` filter -> rows 0,2,3 survive
	m.followMode = false
	m.updateViewport()
	m = updateModel(m, keyRune('g')) // origin -> top of the filtered list

	m = commitSearch(m, "needle")
	assert.Equal(t, "keep needle a", logCursorLine(t, m), "search starts within the `s`-filtered set")
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "keep needle c", logCursorLine(t, m), "n skips the filtered-out drop rows")
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, "keep needle a", logCursorLine(t, m), "wraps over the filtered matches only")
	assert.Equal(t, "keep", m.searchPattern, "the `s` filter is untouched")
}

func TestLogsSearch_NoMatch(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "bbb", "ccc"})
	m = updateModel(m, keyRune('g'))

	m = commitSearch(m, "zzz")
	assert.Equal(t, "zzz", m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx, "a query with no match leaves no cursor")

	// n/N are also no-ops when nothing matches.
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, -1, m.logCursorIdx)

	bar := m.statusBar("")
	assert.Contains(t, bar, "/zzz (0 matches)", "status shows the 0-match form")
}

func TestLogsSearch_NoMatchAfterPriorMatchClearsCursor(t *testing.T) {
	// Regression (CodeRabbit): a `/`-search with NO match must clear a cursor left
	// by a PRIOR match, so no stale ❯ marker lingers on a non-matching row while
	// the status shows "(0 matches)".
	m := newLogsModel(20, []string{"alpha", "hit here", "gamma"})
	m = commitSearch(m, "hit")
	require.Equal(t, "hit here", logCursorLine(t, m))

	m = commitSearch(m, "zzz") // no match — must clear the prior cursor
	assert.Equal(t, "zzz", m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx, "a no-match search clears the prior cursor index")
	assert.Equal(t, int64(0), m.logCursorSeq, "and its Seq anchor")
	assert.Contains(t, m.statusBar(""), "/zzz (0 matches)")
}

func TestLogsSearch_EscClears(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "needle", "bbb"})
	m = updateModel(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)
	require.NotEqual(t, -1, m.logCursorIdx)

	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logSearchQuery, "esc clears the logs-view search query")
	assert.Equal(t, int64(0), m.logCursorSeq, "esc resets the logs cursor anchor")
	assert.Equal(t, -1, m.logCursorIdx, "esc resets the logs cursor index")
}

func TestLogsSearch_StatusIndicator(t *testing.T) {
	m := newLogsModel(20, []string{"needle a", "x", "needle b"})
	m = updateModel(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, 0, m.logCursorIdx)

	assert.Contains(t, m.statusBar(""), "/needle (1/2)", "shows the cursor's match position of the total")
	m = updateModel(m, keyRune('n'))
	assert.Contains(t, m.statusBar(""), "/needle (2/2)", "advancing updates the position")
}

func TestLogsSearch_FollowPreservedOnNewestRowMatch(t *testing.T) {
	// Follow on, cursor pinned to the newest line which matches: the jump lands
	// where the cursor already is, so follow must stay engaged.
	m := newLogsModel(20, []string{"a", "b", "c", "hit newest"})
	require.True(t, m.followMode)
	m = commitSearch(m, "hit")
	assert.Equal(t, 3, m.logCursorIdx, "the newest line matches")
	assert.True(t, m.followMode, "a jump landing on the newest line preserves follow")

	// A jump that MOVES the cursor off the newest line must disengage follow, or
	// resolveLogCursor's pin-to-newest would immediately undo the jump.
	m2 := newLogsModel(20, []string{"a", "hit mid", "c", "d"})
	require.True(t, m2.followMode)
	m2 = commitSearch(m2, "hit")
	assert.Equal(t, 1, m2.logCursorIdx, "the jump sticks off the newest line")
	assert.False(t, m2.followMode, "jumping off the newest line disengages follow")
}

func TestLogsSearch_OffNewestJumpSurvivesLogArrival(t *testing.T) {
	// Regression (codex review): after a jump parks the cursor off the newest row
	// (follow disengaged), a streaming log arrival must NOT silently re-engage
	// follow and drag the ❯ marker off the match — even with a short/full viewport
	// where the match is still "near bottom".
	m := newLogsModel(20, []string{"a", "hit mid", "c", "d"})
	m = commitSearch(m, "hit")
	require.Equal(t, "hit mid", logCursorLine(t, m))
	require.False(t, m.followMode)
	require.Contains(t, m.statusBar(""), "/hit (1/1)")

	// A new log line arrives while the search is parked off-newest.
	m = updateModel(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Unix(10, 0),
		Process:   "p",
		Line:      "e newest",
	}))

	assert.False(t, m.followMode, "a streaming arrival must not re-engage follow while a search is parked")
	assert.Equal(t, "hit mid", logCursorLine(t, m), "the cursor stays on the match, not yanked to the new newest row")
	assert.Contains(t, m.statusBar(""), "/hit (1/1)", "match position is preserved across the arrival")
}

func TestLogsSearch_EvictionAnchorSurvives(t *testing.T) {
	m := newTestModel()
	m = updateModel(m, tea.WindowSizeMsg{Width: 120, Height: 11}) // viewport height 5
	feed := func(line string) {
		m.handleLogEntry(domain.LogEntry{Timestamp: time.Unix(0, 0), Process: "p", Line: line})
	}

	// Fill the ring with a lone match parked in the MIDDLE, so scrolling to it on
	// commit does not leave the viewport near the bottom (which would re-engage
	// follow on later arrivals and pin the cursor to the newest row instead).
	for i := 0; i < 500; i++ {
		feed(fmt.Sprintf("log %04d", i))
	}
	feed("NEEDLE row")
	for i := 0; i < 500; i++ {
		feed(fmt.Sprintf("log %04d", 500+i))
	}
	require.Len(t, m.logEntries, maxLogEntries, "the front-eviction ring is full")

	// Search with follow off so the cursor stays Seq-anchored to NEEDLE as the
	// ring shifts, rather than being pinned to the newest line.
	m = updateModel(m, keyRune('g')) // GotoTop, follow off
	m = commitSearch(m, "NEEDLE")
	require.False(t, m.followMode)
	require.Contains(t, m.logEntries[m.logCursorIdx].Line, "NEEDLE")
	needleSeq := m.logCursorSeq
	require.NotZero(t, needleSeq)
	idxBefore := m.logCursorIdx

	// Push more: front eviction shifts NEEDLE's index down, but its Seq survives,
	// so resolveLogCursor re-resolves the cursor onto it (index changes, line same).
	for i := 0; i < 10; i++ {
		feed(fmt.Sprintf("extra %d", i))
	}
	assert.Equal(t, needleSeq, m.logCursorSeq, "the Seq anchor is unchanged by eviction")
	assert.Contains(t, m.logEntries[m.logCursorIdx].Line, "NEEDLE", "cursor rides the Seq across the ring")
	assert.Less(t, m.logCursorIdx, idxBefore, "the anchored index shifted down as the ring evicted")

	// Now evict NEEDLE outright; the cursor must CLAMP into range, never go stale
	// or panic.
	for i := 0; i < 1100; i++ {
		feed(fmt.Sprintf("flood %d", i))
	}
	for _, e := range m.logEntries {
		require.NotEqual(t, needleSeq, e.Seq, "NEEDLE has been evicted")
	}
	assert.GreaterOrEqual(t, m.logCursorIdx, 0, "the evicted cursor clamps in range")
	assert.Less(t, m.logCursorIdx, len(m.logEntries), "the evicted cursor clamps in range")
}

func TestLogsSearch_FirstSearchAfterManualScroll(t *testing.T) {
	// A match sits both ABOVE the visible region (row 2) and inside/after it
	// (row 20). A first search must seed from what the user is looking at (the
	// top visible row), landing on row 20 rather than the earlier row 2.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i)
	}
	lines[2] = "target above"
	lines[20] = "target below"
	m := newLogsModel(5, lines) // viewport height 5, follow on, parked at the bottom

	m = updateModel(m, keyRune('k')) // disengage follow
	require.False(t, m.followMode)
	m.viewport.SetYOffset(15) // top visible row is now 15
	require.Equal(t, 15, m.viewport.YOffset)

	m = commitSearch(m, "target")
	assert.Equal(t, 20, m.logCursorIdx, "first search starts from the visible region, not row 0")
}

func TestLogsSearch_HighlightGuard(t *testing.T) {
	// The default test renderer is Ascii (no color), which would make a
	// highlighted and an unhighlighted line byte-identical. Force a color profile
	// so the inline highlight actually emits detectable ANSI, and restore it.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prev)

	m := newLogsModel(20, []string{"plain"})

	// ASCII line + ASCII query: the matched run is wrapped in searchHighlightStyle.
	m.logSearchQuery = "match"
	ascii := m.highlightLogLine("hello match world")
	assert.NotEqual(t, "hello match world", ascii, "an ASCII match is inline-highlighted")
	assert.Contains(t, ascii, searchHighlightStyle.Render("match"), "the matched run is styled")
	assert.Contains(t, ascii, "hello ")
	assert.Contains(t, ascii, " world")

	// Unicode line: case-folding would shift byte offsets, so the guard trips and
	// the line falls back to no inline highlight (row marker alone) — unchanged.
	uni := "日本語 match テスト"
	assert.Equal(t, uni, m.highlightLogLine(uni), "a unicode line falls back to no highlight")

	// ESC-bearing line: a digit query can match inside the ANSI escape, so the
	// guard trips and the escape is left intact rather than split.
	m.logSearchQuery = "31"
	ansiLine := "x \x1b[31mred\x1b[0m match"
	assert.Equal(t, ansiLine, m.highlightLogLine(ansiLine), "an ESC-bearing line falls back, escape intact")
}

func TestLogsSearch_UnicodeAndAnsiRenderSafely(t *testing.T) {
	// End-to-end: a unicode line and a line already carrying ANSI escapes. The
	// search must not panic or split the escape, and the row marker still lands
	// on the matched row.
	m := newLogsModel(20, []string{"before", "cafe \x1b[31mMATCH\x1b[0m after", "café résumé MATCH"})
	m = updateModel(m, keyRune('g'))
	m = commitSearch(m, "MATCH")
	require.Equal(t, 1, m.logCursorIdx, "the first match at/after the top is the ANSI row")

	view := m.viewport.View()
	assert.Contains(t, view, "❯", "the cursor marker renders on the matched row")
	assert.Contains(t, view, "\x1b[31mMATCH\x1b[0m", "the source line's escape survives intact (not split by a highlight)")
}

func TestLogsSearch_IsASCIINoESC(t *testing.T) {
	assert.True(t, isASCIINoESC("plain ascii 123"))
	assert.True(t, isASCIINoESC(""))
	assert.False(t, isASCIINoESC("café"), "non-ASCII bytes fail")
	assert.False(t, isASCIINoESC("has\x1b[0mesc"), "an ESC byte fails even though it is ASCII")
}

func TestRequestsSearch_StatusPrecedence(t *testing.T) {
	reqs := makeTestRequests(10)
	reqs[3].URL = "/path/needle" // matches both the `path` filter and the search
	m := newSearchModel(40, reqs)
	m.soloProcess = "web" // a logs concept: must NOT appear in the requests view
	m.searchPattern = "path"
	m.followMode = false
	m.updateViewport()
	m = updateModel(m, keyRune('g'))

	m = commitSearch(m, "needle")
	bar := m.statusBar("")
	assert.Contains(t, bar, "/needle (1/1)", "search indicator wins, with the cursor's match position")
	assert.Contains(t, bar, "filter: path", "the active `s` filter is appended")
	assert.NotContains(t, bar, "Showing:", "solo is never shown in the requests view")

	// Prompt precedence: while typing a search, the input prompt wins over the
	// committed indicator.
	m2 := updateModel(m, keyRune('/'))
	assert.Contains(t, m2.statusBar(""), "Search:", "the mode prompt takes precedence")
}

func TestRequestsSearch_UnicodeStatusWidth(t *testing.T) {
	m := newSearchModel(40, makeTestRequests(5))
	m.width = 120

	m.requestSearchQuery = "日本語テスト"
	unicodeBar := m.statusBar("")

	// The layout is measured in display columns, not bytes: an ASCII query of
	// the same DISPLAY width must produce a bar of the same rendered width.
	// Scope note: this guards layout stability for wide-rune queries (the
	// left side, laid out by lipgloss). It cannot catch a regression of the
	// right side's lipgloss.Width back to len — the right side is
	// structurally ASCII, so no assertion can distinguish them there.
	m.requestSearchQuery = strings.Repeat("x", lipgloss.Width("日本語テスト"))
	asciiBar := m.statusBar("")
	assert.Equal(t, lipgloss.Width(asciiBar), lipgloss.Width(unicodeBar),
		"a Unicode query must not change the status-bar layout width")
}

func TestRequestsSearch_WrapToNewestNoFollow(t *testing.T) {
	reqs := makeTestRequests(6)
	reqs[0].URL = "/hit/a"
	reqs[5].URL = "/hit/b" // the newest (last) row also matches
	m := newSearchModel(20, reqs)
	m = updateModel(m, keyRune('g')) // cursor row 0, follow off
	require.False(t, m.followMode)

	m = commitSearch(m, "hit")
	require.Equal(t, 0, m.cursorIdx)
	require.False(t, m.followMode)

	// n wraps forward onto the newest row. Unlike `j`, a search jump is
	// positioning, not scrolling intent: it must NOT re-engage follow.
	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 5, m.cursorIdx)
	assert.Equal(t, "req-005", m.cursorID)
	assert.False(t, m.followMode, "a search jump onto the newest row must not re-engage follow")
}

func TestRequestsSearch_FollowPreservedOnNewestRowMatch(t *testing.T) {
	// Follow on, cursor pinned to the newest row, which matches the query: the
	// jump lands where the cursor already is, so follow must stay engaged —
	// disengaging would silently stop auto-follow after a no-op jump.
	reqs := makeTestRequests(6)
	reqs[5].URL = "/hit/newest"
	m := newSearchModel(20, reqs)
	require.True(t, m.followMode)
	require.Equal(t, 5, m.cursorIdx, "follow pins the cursor to the newest row")

	m = commitSearch(m, "hit")
	assert.Equal(t, 5, m.cursorIdx)
	assert.True(t, m.followMode, "a jump landing on the newest row must preserve follow")

	// A jump that MOVES the cursor off the newest row must disengage follow,
	// or resolveRequestCursor's pin-to-newest would immediately undo it.
	reqs2 := makeTestRequests(6)
	reqs2[2].URL = "/hit/mid"
	m2 := newSearchModel(20, reqs2)
	require.True(t, m2.followMode)
	m2 = commitSearch(m2, "hit")
	assert.Equal(t, 2, m2.cursorIdx, "the jump must stick")
	assert.Equal(t, "req-002", m2.cursorID)
	assert.False(t, m2.followMode, "jumping off the newest row must disengage follow")
}

func TestRequestsSearch_SingleMatchNextPrevNoop(t *testing.T) {
	// n/N scan strictly past the cursor and never revisit its own row, so with
	// the sole match under the cursor they are documented no-ops.
	reqs := makeTestRequests(5)
	reqs[2].URL = "/only/hit"
	m := newSearchModel(20, reqs)
	m = updateModel(m, keyRune('g'))
	m = commitSearch(m, "hit")
	require.Equal(t, 2, m.cursorIdx)

	m = updateModel(m, keyRune('n'))
	assert.Equal(t, 2, m.cursorIdx, "n with a sole match is a no-op")
	m = updateModel(m, keyRune('N'))
	assert.Equal(t, 2, m.cursorIdx, "N with a sole match is a no-op")
	assert.Equal(t, "req-002", m.cursorID)
}

// newInFlightDetailModel opens the detail view (local mode) for a single
// in-flight request, viewport sized so its content Height is viewportHeight,
// and returns the model with the detail already rendered from the top.
func newInFlightDetailModel(t *testing.T, id string, viewportHeight int) Model {
	t.Helper()
	inFlight := proxy.RequestRecord{
		ID: id, Timestamp: time.Now(), Method: "GET", URL: "/x", InFlight: true,
	}
	m := newSearchModel(viewportHeight, []proxy.RequestRecord{inFlight})
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)
	require.NotNil(t, m.requestDetail)
	require.True(t, m.requestDetail.InFlight)
	return m
}

// finalRecordFor builds the completed (non-in-flight) record that would
// arrive as the matching final ProxyRequestMsg for an in-flight row with the
// given id, carrying Details so the refresh has something visible to render.
func finalRecordFor(id string) proxy.RequestRecord {
	return proxy.RequestRecord{
		ID:         id,
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/x",
		StatusCode: 200,
		Duration:   42 * time.Millisecond,
		InFlight:   false,
		Details: &proxy.RequestDetails{
			RequestHeaders: map[string][]string{"X-Test": {"yes"}},
		},
	}
}

// TestModel_DetailLiveRefresh_LocalMatchingFinalRefreshesInPlace pins D15:
// completing an in-flight request whose detail is open refreshes the view in
// place — Details render, the "(in flight)" note disappears, and the
// reader's scroll position is preserved (detail content only grows on
// completion, and SetContent never resets YOffset on growth).
func TestModel_DetailLiveRefresh_LocalMatchingFinalRefreshesInPlace(t *testing.T) {
	m := newInFlightDetailModel(t, "req-1", 5)
	assert.Contains(t, strings.Join(m.formatRequestDetail(), "\n"), "in flight")

	m.viewport.SetYOffset(2)
	require.Equal(t, 2, m.viewport.YOffset, "precondition: reader scrolled mid-read")

	m = updateModel(m, ProxyRequestMsg(finalRecordFor("req-1")))

	require.NotNil(t, m.requestDetail)
	assert.False(t, m.requestDetail.InFlight)
	assert.Equal(t, int64(42), m.requestDetail.DurationMs)
	assert.Equal(t, 2, m.viewport.YOffset, "reader's scroll position is preserved")
	rendered := strings.Join(m.formatRequestDetail(), "\n")
	assert.NotContains(t, rendered, "in flight")
	assert.Contains(t, rendered, "X-Test", "Details render after the refresh")
}

// TestModel_DetailLiveRefresh_NonMatchingIDNoRefresh pins that a final record
// for a DIFFERENT request than the one open in the detail view leaves the
// displayed snapshot untouched (the list still updates independently).
func TestModel_DetailLiveRefresh_NonMatchingIDNoRefresh(t *testing.T) {
	m := newInFlightDetailModel(t, "req-1", 5)

	m = updateModel(m, ProxyRequestMsg(finalRecordFor("req-other")))

	assert.True(t, m.requestDetail.InFlight, "the open detail is unaffected by another row's completion")
	assert.Len(t, m.proxyRequests, 2, "the list still gains the new row")
}

// TestModel_DetailLiveRefresh_StillInFlightNoRefresh pins that a duplicate
// in-flight message for the OPEN request (record.InFlight still true) does
// not trigger a refresh — only a final (!InFlight) record does.
func TestModel_DetailLiveRefresh_StillInFlightNoRefresh(t *testing.T) {
	m := newInFlightDetailModel(t, "req-1", 5)

	stillInFlight := proxy.RequestRecord{
		ID: "req-1", Timestamp: time.Now(), Method: "GET", URL: "/x", InFlight: true,
	}
	m = updateModel(m, ProxyRequestMsg(stillInFlight))

	assert.True(t, m.requestDetail.InFlight, "a still-in-flight duplicate must not be treated as the refresh")
}

// TestModel_DetailLiveRefresh_NotInDetailViewNoRefresh pins the list-only
// path: a final ProxyRequestMsg arriving while the requests view (not the
// detail view) is showing must not populate requestDetail — refresh only
// applies to an OPEN detail view.
func TestModel_DetailLiveRefresh_NotInDetailViewNoRefresh(t *testing.T) {
	inFlight := proxy.RequestRecord{
		ID: "req-1", Timestamp: time.Now(), Method: "GET", URL: "/x", InFlight: true,
	}
	m := newSearchModel(5, []proxy.RequestRecord{inFlight})
	require.Equal(t, ViewModeRequests, m.viewMode)
	require.Nil(t, m.requestDetail)

	m = updateModel(m, ProxyRequestMsg(finalRecordFor("req-1")))

	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Nil(t, m.requestDetail, "no detail view is open, so nothing is populated")
}

func TestModel_DetailLiveRefresh_ShrinkingContentClamps(t *testing.T) {
	// A capture-disabled final detail is SHORTER than the in-flight view it
	// replaces (the two in-flight note lines vanish and no header/body
	// sections take their place). A bottom-scrolled reader must be pulled
	// back to the last valid offset, not left on blank overscroll.
	m := newInFlightDetailModel(t, "req-1", 3)
	m.viewport.GotoBottom()
	require.Positive(t, m.viewport.YOffset, "test needs a scrolled-down detail view")

	final := finalRecordFor("req-1")
	final.Details = nil // capture disabled: no headers/bodies arrive
	m = updateModel(m, ProxyRequestMsg(final))

	require.NotNil(t, m.requestDetail)
	assert.False(t, m.requestDetail.InFlight)
	maxOffset := m.viewport.TotalLineCount() - m.viewport.Height
	if maxOffset < 0 {
		maxOffset = 0
	}
	assert.LessOrEqual(t, m.viewport.YOffset, maxOffset,
		"refresh onto shorter content must clamp the viewport offset")
}
