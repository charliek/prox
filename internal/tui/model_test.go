package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestUpdateSearchMatches(t *testing.T) {
	model := newTestModel()

	model.logEntries = []domain.LogEntry{
		{Line: "error: something failed"},
		{Line: "info: all good"},
		{Line: "error: another failure"},
		{Line: "debug: test message"},
	}

	model.searchPattern = "error"
	model.updateSearchMatches()

	assert.Len(t, model.searchMatches, 2)
	assert.Equal(t, 0, model.searchMatches[0])
	assert.Equal(t, 2, model.searchMatches[1])
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
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.proxyRequests = makeTestRequests(n)
	return updateModel(m, tea.WindowSizeMsg{Width: 120, Height: viewportHeight + 6})
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
