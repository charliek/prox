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
	"github.com/charliek/prox/internal/proxy"
)

// newTestModel creates a ClientModel with default test dependencies (a fresh
// stubTUIClient and attach-mode options). This reduces boilerplate in tests
// that need a basic model. Ported from local-mode Model onto ClientModel in
// plan 018 C3 — local mode (Model) is deleted in C4, and Init's no-poll
// behavior this used to guard is now pinned directly by
// TestClientModel_NeverPollsProcesses in client_model_test.go.
func newTestModel() ClientModel {
	return NewClientModel(&stubTUIClient{}, attachClientOptions())
}

func TestNewModel(t *testing.T) {
	model := newTestModel()

	assert.Equal(t, ModeNormal, model.mode)
	assert.False(t, model.ready)
	assert.Empty(t, model.logEntries)
}

func TestTUI_HandleKey_Quit(t *testing.T) {
	model := newTestModel()

	// Test quit with 'q'
	newModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assert.NotNil(t, cmd)
	_ = newModel
}

// TestCtrlC_QuitsFromAllCaptureStates (T6) pins ctrl+c as a global quit that
// fires before every capture layer: mode dispatch (help/search/filter) and
// open-menu key capture (plan 023 A3 / B3). q keeps per-mode behavior.
func TestCtrlC_QuitsFromAllCaptureStates(t *testing.T) {
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}

	assertQuits := func(t *testing.T, m ClientModel) {
		t.Helper()
		_, cmd := clientUpdateModel(m, ctrlC)
		assert.Equal(t, tea.QuitMsg{}, runCmdWithin(t, cmd))
	}

	t.Run("Normal", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		require.Equal(t, ModeNormal, m.mode)
		assertQuits(t, m)
	})

	t.Run("Help", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('?'))
		require.Equal(t, ModeHelp, m.mode)
		assertQuits(t, m)

		// q closes help without quitting (per-mode behavior preserved).
		m = newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('?'))
		m, cmd := clientUpdateModel(m, keyRune('q'))
		assert.Equal(t, ModeNormal, m.mode)
		assert.Nil(t, cmd, "q in help must not quit")
	})

	t.Run("Search", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('/'))
		require.Equal(t, ModeSearch, m.mode)
		assertQuits(t, m)
	})

	t.Run("StringFilter", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('s'))
		require.Equal(t, ModeStringFilter, m.mode)
		assertQuits(t, m)
	})

	t.Run("OpenDropdown", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('v'))
		require.True(t, m.menuOpen())
		assertQuits(t, m)
	})
}

func TestTUI_HandleKey_ModeSwitch(t *testing.T) {
	model := newTestModel()

	// Test switching to help mode
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m := newModel.(ClientModel)
	assert.Equal(t, ModeHelp, m.mode)

	// Test switching to filter menu (f opens Filter dropdown when menu bar visible)
	model = newTestModel()
	model = clientUpdate(model, tea.WindowSizeMsg{Width: 80, Height: 24})
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = newModel.(ClientModel)
	assert.True(t, m.menuOpen())
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, ModeNormal, m.mode)

	// Test switching to search mode
	model = newTestModel()
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = newModel.(ClientModel)
	assert.Equal(t, ModeSearch, m.mode)

	// Test switching to string filter mode
	model = newTestModel()
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = newModel.(ClientModel)
	assert.Equal(t, ModeStringFilter, m.mode)
}

func TestTUI_HandleKey_EscClearsFilters(t *testing.T) {
	model := newTestModel()
	model.soloProcess = "test"
	model.setLogsFilterQuery("pattern")
	model.setRequestsFilterQuery("status:500")

	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m := newModel.(ClientModel)

	assert.Empty(t, m.soloProcess)
	assert.Empty(t, m.logsFilter.RawQuery)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
	assert.Empty(t, m.requestsFilter.RawQuery)
	assert.True(t, m.requestsFilter.LastGood.IsEmpty())
}

func TestTUI_LogEntryMsg(t *testing.T) {
	model := newTestModel()
	model.ready = true // Set ready to avoid viewport issues

	entry := domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      "test log line",
	}

	newModel, _ := model.Update(LogEntryMsg(entry))
	m := newModel.(ClientModel)

	assert.Len(t, m.logEntries, 1)
	assert.Equal(t, "test", m.logEntries[0].Process)
	assert.Equal(t, "test log line", m.logEntries[0].Line)
}

func TestTUI_LogEntryLimit(t *testing.T) {
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
		model = newModel.(ClientModel)
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

	// String filter (bare substring — same as today's s-bar)
	model.soloProcess = ""
	model.setLogsFilterQuery("log 1")
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
			m := newModel.(ClientModel)

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
			m := newModel.(ClientModel)

			assert.True(t, m.followMode, "followMode should be true after %s", tt.name)
		})
	}
}

func TestFollowModeToggle(t *testing.T) {
	model := newTestModel()
	assert.True(t, model.followMode) // starts true

	// First toggle - should disable
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m := newModel.(ClientModel)
	assert.False(t, m.followMode)

	// Second toggle - should enable
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m = newModel.(ClientModel)
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
	model.setRequestsFilterQuery("users")
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)

	// String filter on method
	model.setRequestsFilterQuery("GET")
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
	for _, r := range requests {
		assert.Equal(t, "GET", r.Method)
	}

	// String filter on subdomain
	model.setRequestsFilterQuery("api")
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
	for _, r := range requests {
		assert.Equal(t, "api", r.Subdomain)
	}

	// Case-insensitive filter
	model.setRequestsFilterQuery("API")
	requests = model.filteredProxyRequests()
	assert.Len(t, requests, 2)
}

func TestProxyRequestBufferLimit(t *testing.T) {
	model := newTestModel()
	model.ready = true

	// Overfill the history cap. The bound is the constant, not a literal, so the
	// test scales with a retuned cap (D12b raised it from the sync limit to the
	// server's retention).
	for i := 0; i < maxRequestHistory+5; i++ {
		req := proxy.RequestRecord{
			Timestamp: time.Now(),
			Subdomain: "api",
			Method:    "GET",
			URL:       "/test",
		}
		newModel, _ := model.Update(ProxyRequestMsg(req))
		model = newModel.(ClientModel)
	}

	assert.Len(t, model.proxyRequests, maxRequestHistory)
}

func TestTUI_ProxyRequestMsg(t *testing.T) {
	model := newTestModel()
	model.ready = true

	// Send a proxy request through Update() — in-flight, as the live stream
	// publishes it at response-header time.
	req := proxy.RequestRecord{
		ID:         "req-1",
		Timestamp:  time.Now(),
		Subdomain:  "web",
		Method:     "POST",
		URL:        "/api/users",
		StatusCode: 201,
		RemoteAddr: "192.168.1.1:54321",
		InFlight:   true,
	}

	newModel, _ := model.Update(ProxyRequestMsg(req))
	m := newModel.(ClientModel)

	// Verify request was added
	assert.Len(t, m.proxyRequests, 1)
	assert.Equal(t, "web", m.proxyRequests[0].Subdomain)
	assert.Equal(t, "POST", m.proxyRequests[0].Method)
	assert.Equal(t, "/api/users", m.proxyRequests[0].URL)
	assert.Equal(t, 201, m.proxyRequests[0].StatusCode)

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
	m = newModel.(ClientModel)

	assert.Len(t, m.proxyRequests, 2)
	filtered = m.filteredProxyRequests()
	assert.Len(t, filtered, 2)

	// Test filtering
	m.setRequestsFilterQuery("users")
	filtered = m.filteredProxyRequests()
	assert.Len(t, filtered, 1)
	assert.Equal(t, "/api/users", filtered[0].URL)

	// A same-ID re-record (the in-flight row's completion event) updates the
	// row in place rather than appending a duplicate.
	req1Updated := req
	req1Updated.StatusCode = 204
	req1Updated.Duration = 99 * time.Millisecond
	req1Updated.InFlight = false

	newModel, _ = m.Update(ProxyRequestMsg(req1Updated))
	m = newModel.(ClientModel)

	assert.Len(t, m.proxyRequests, 2, "same-ID update must not duplicate the row")
	assert.Equal(t, 204, m.proxyRequests[0].StatusCode)
	assert.Equal(t, 99*time.Millisecond, m.proxyRequests[0].Duration)

	// Final is terminal (C6): neither a duplicate final nor a late in-flight
	// copy of the same ID may regress the completed row.
	regress := req1Updated
	regress.StatusCode = 500
	regress.InFlight = true
	newModel, _ = m.Update(ProxyRequestMsg(regress))
	m = newModel.(ClientModel)

	assert.Len(t, m.proxyRequests, 2)
	assert.Equal(t, 204, m.proxyRequests[0].StatusCode, "a stale in-flight copy must not regress a final row")
	assert.False(t, m.proxyRequests[0].InFlight)
}

// TestModel_ProxyRequestMsg_SelectionStable pins the cursor invariant (D11):
// the ID-anchored cursor survives an in-place upsert of ANOTHER row and of its
// OWN row without moving, so an arriving completion never changes what Enter
// opens.
func TestTUI_ProxyRequestMsg_SelectionStable(t *testing.T) {
	m := newRequestsModel(3, 20)
	// The rows start in-flight: an in-place update is only ever a completion
	// arriving for a live row (final rows are terminal — C6).
	for i := range m.proxyRequests {
		m.proxyRequests[i].InFlight = true
	}
	m = clientUpdate(m, keyRune('g')) // cursor row 0, follow off
	m = clientUpdate(m, keyRune('j')) // cursor row 1 (req-001), not the last row
	assert.Equal(t, "req-001", m.cursorID)
	assert.Equal(t, 1, m.cursorIdx)
	assert.False(t, m.followMode)

	// In-place upsert of ANOTHER row (row 0 completing) must not move the cursor.
	other := m.proxyRequests[0]
	other.StatusCode = 204
	m = clientUpdate(m, ProxyRequestMsg(other))
	assert.Len(t, m.proxyRequests, 3, "in-place update must not change the row count")
	assert.Equal(t, "req-001", m.cursorID, "cursor stays on its row by ID")
	assert.Equal(t, 1, m.cursorIdx)
	assert.Equal(t, 204, m.proxyRequests[0].StatusCode, "the target row must reflect the update")
	assert.Equal(t, "req-002", m.proxyRequests[2].ID, "other rows keep their index")

	// In-place upsert of the cursor's OWN row must not move it either.
	own := m.proxyRequests[1]
	own.StatusCode = 500
	m = clientUpdate(m, ProxyRequestMsg(own))
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
	m := newModel.(ClientModel)
	assert.Equal(t, ViewModeRequests, m.viewMode)

	// Tab again switches back to Logs view
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = newModel.(ClientModel)
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
	assert.Contains(t, formatted, "     0") // Status code 0 with 6-char right-aligned padding

	// Verify exact padding for subdomain (10 chars left-aligned)
	// "api" should be followed by 7 spaces to make 10 chars total
	assert.Contains(t, formatted, "api       ") // 10 chars total

	// Verify exact padding for method (7 chars left-aligned)
	// "GET" should be followed by 4 spaces to make 7 chars total
	assert.Contains(t, formatted, "GET    ") // 7 chars total

	// Verify duration is 8 chars right-aligned (100ms).
	assert.Contains(t, formatted, "   100ms")
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

	assert.Contains(t, formatted, "   ...ms", "duration column should render dots, 8-char padded, with the ms suffix")
	assert.NotContains(t, formatted, "0ms", "in-flight rows must not render a fake zero duration")
	assert.Contains(t, formatted, "200", "status should show the real header-time code")
}

// TestFormatProxyRequest_Stale verifies a stale in-flight row (D8, #53: the
// completion event may have been lost, true outcome unknown) renders
// "stale?" in the duration column instead of the ordinary in-flight dots.
func TestFormatProxyRequest_Stale(t *testing.T) {
	model := newTestModel()

	req := proxy.RequestRecord{
		Timestamp:  time.Now().Add(-10 * time.Minute),
		Subdomain:  "api",
		Method:     "GET",
		URL:        "/stream",
		StatusCode: 200,
		InFlight:   true,
	}

	formatted := model.formatProxyRequest(req)

	assert.Contains(t, formatted, "stale?", "duration column should render 'stale?' for a stale in-flight row")
	assert.NotContains(t, formatted, "...", "a stale row should not also render the fresh in-flight dots")
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
		wantStatus string // Expected status with padding (6 chars)
		wantDur    string // Expected duration with padding (8 chars)
	}{
		{
			name:       "short fields",
			subdomain:  "a",
			method:     "GET",
			statusCode: 200,
			durationMs: 1,
			wantSub:    "a         ", // 1 + 9 spaces
			wantMethod: "GET    ",    // 3 + 4 spaces
			wantStatus: "   200",     // right-aligned in 6 cols
			wantDur:    "     1ms",   // right-aligned in 8 cols
		},
		{
			name:       "max length subdomain",
			subdomain:  "webservice",
			method:     "DELETE",
			statusCode: 404,
			durationMs: 9999,
			wantSub:    "webservice", // exactly 10 chars
			wantMethod: "DELETE ",    // 6 + 1 space
			wantStatus: "   404",
			wantDur:    "  9999ms",
		},
		{
			name:       "single digit status",
			subdomain:  "api",
			method:     "OPTIONS",
			statusCode: 0,
			durationMs: 50,
			wantSub:    "api       ",
			wantMethod: "OPTIONS",  // exactly 7 chars
			wantStatus: "     0",   // right-aligned in 6 cols
			wantDur:    "    50ms", // right-aligned in 8 cols
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

// newRequestsModel builds a ClientModel in the requests view holding n requests
// (IDs req-000..req-{n-1}, URLs /path/000..), with the viewport sized so its
// content Height is viewportHeight (handleWindowSize/relayout subtracts
// defaultChromeHeight + defaultPanelBorder + defaultRequestsHeaderRows). followMode starts true, so the cursor begins pinned
// to the newest row.
func newRequestsModel(n, viewportHeight int) ClientModel {
	return newSearchModel(viewportHeight, makeTestRequests(n))
}

// makeTestRequests builds n proxy request records with the shared fixture
// convention (IDs req-000.., URLs /path/000.., monotonic timestamps). Shared by
// both the ClientModel and ClientModel requests-view test constructors.
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
func cursorVisible(m ClientModel) bool {
	yo := m.viewport.YOffset
	return m.cursorIdx >= yo && m.cursorIdx < yo+m.viewport.Height
}

func newArrival(id, url string) proxy.RequestRecord {
	return proxy.RequestRecord{ID: id, Timestamp: time.Now(), Method: "GET", URL: url, StatusCode: 200}
}

func TestRequestsCursor_Movement(t *testing.T) {
	m := newRequestsModel(10, 5)
	assert.Equal(t, 9, m.cursorIdx, "follow pins the cursor to the newest row")

	m = clientUpdate(m, keyRune('k'))
	assert.Equal(t, 8, m.cursorIdx)
	assert.False(t, m.followMode, "k disengages follow")

	m = clientUpdate(m, keyRune('k'))
	assert.Equal(t, 7, m.cursorIdx)

	m = clientUpdate(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.False(t, m.followMode)

	// Clamp at the top.
	m = clientUpdate(m, keyRune('k'))
	assert.Equal(t, 0, m.cursorIdx)

	// Half-page paging: step = Height/2 = 5/2 = 2.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyPgDown})
	assert.Equal(t, 2, m.cursorIdx)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyPgUp})
	assert.Equal(t, 0, m.cursorIdx)
	assert.False(t, m.followMode)

	m = clientUpdate(m, keyRune('G'))
	assert.Equal(t, 9, m.cursorIdx)
	assert.True(t, m.followMode, "G re-engages follow")

	// Clamp at the bottom.
	m = clientUpdate(m, keyRune('j'))
	assert.Equal(t, 9, m.cursorIdx)
}

func TestRequestsCursor_VisibilityKeyboardJumps(t *testing.T) {
	m := newRequestsModel(30, 5)

	m = clientUpdate(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset)
	assert.True(t, cursorVisible(m))

	// Walk the cursor down; it must stay on-screen with minimal scrolling.
	for i := 1; i <= 20; i++ {
		m = clientUpdate(m, keyRune('j'))
		assert.Equal(t, i, m.cursorIdx)
		assert.True(t, cursorVisible(m),
			"cursor %d must be visible (YOffset %d, height %d)", i, m.viewport.YOffset, m.viewport.Height)
	}

	m = clientUpdate(m, keyRune('G'))
	assert.Equal(t, 29, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	m = clientUpdate(m, keyRune('g'))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset)
}

// TestRequestsCursor_VisibilityTabIn covers the invariant across a tab-in from a
// scrolled logs view: the shared viewport retains the logs YOffset, and the
// requests branch of updateViewport must scroll it so the cursor is visible.
func TestRequestsCursor_VisibilityTabIn(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 5 + defaultChromeHeight() + defaultPanelBorder()}) // viewport height 5

	// Fill logs and requests while in the logs view.
	for i := 0; i < 30; i++ {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Timestamp: time.Now(), Process: "p", Line: fmt.Sprintf("log %d", i)}))
	}
	for i := 0; i < 30; i++ {
		m = clientUpdate(m, ProxyRequestMsg(newArrival(fmt.Sprintf("req-%03d", i), fmt.Sprintf("/path/%03d", i))))
	}

	// Scroll the logs viewport up (disengages follow) so YOffset is large.
	m = clientUpdate(m, keyRune('k'))
	require.Greater(t, m.viewport.YOffset, 5, "logs viewport should be scrolled well down")
	require.False(t, m.followMode)

	// Tab into the requests view: cursor resolves to row 0 (follow off, no prior
	// cursor) and the viewport must scroll up to show it.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset, "tab-in must reveal the cursor row")
}

func TestRequestsCursor_VisibilityResize(t *testing.T) {
	m := newRequestsModel(40, 10)
	m = clientUpdate(m, keyRune('g')) // follow off, cursor 0
	for i := 0; i < 20; i++ {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, 20, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// Shrink the window: cursor must stay visible.
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 5 + defaultChromeHeight() + defaultPanelBorder() + defaultRequestsHeaderRows()})
	assert.Equal(t, 20, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// Grow it back: still visible, and the viewport must not be left scrolled
	// past the true bottom (a grown window shrinks the valid max YOffset —
	// blank overscroll would report the cursor "visible" while showing gaps).
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 30 + defaultChromeHeight() + defaultPanelBorder() + defaultRequestsHeaderRows()})
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
	m.setRequestsFilterQuery("/path/03")
	m.followMode = false
	m.updateViewport()

	m = clientUpdate(m, keyRune('g')) // filtered row 0 = req-030
	m = clientUpdate(m, keyRune('j')) // req-031
	m = clientUpdate(m, keyRune('j')) // req-032
	require.Equal(t, "req-032", m.cursorID)
	require.False(t, m.followMode)

	// esc clears the filter; the cursor's row (req-032, full-list index 32) must
	// remain and be scrolled on-screen.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.requestsFilter.RawQuery)
	assert.True(t, m.requestsFilter.LastGood.IsEmpty())
	assert.Equal(t, "req-032", m.cursorID)
	assert.Equal(t, 32, m.cursorIdx)
	assert.True(t, cursorVisible(m))
}

func TestRequestsCursor_VisibilityDetailReturn(t *testing.T) {
	m := newRequestsModel(30, 5)
	m = clientUpdate(m, keyRune('g'))
	for i := 0; i < 15; i++ {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, "req-015", m.cursorID)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)

	// esc back to the list: the cursor must be restored and visible.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
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
	m = clientUpdate(m, keyRune('k'))
	require.Equal(t, 1, m.cursorIdx)
	require.False(t, m.followMode)
	require.True(t, m.viewport.AtBottom(), "precondition: a short list is always AtBottom")

	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, 4)
	assert.Equal(t, "req-001", m.cursorID, "arrival must not move the cursor")
	assert.Equal(t, 1, m.cursorIdx)
	assert.False(t, m.followMode, "arrival must not re-engage follow")
}

func TestRequestsCursor_JOntoLastRowReengagesFollow(t *testing.T) {
	m := newRequestsModel(5, 20)
	m = clientUpdate(m, keyRune('g'))
	require.False(t, m.followMode)

	for i := 1; i <= 3; i++ {
		m = clientUpdate(m, keyRune('j'))
		assert.False(t, m.followMode, "follow stays off until the cursor lands on the last row (idx %d)", m.cursorIdx)
	}
	require.Equal(t, 3, m.cursorIdx)

	m = clientUpdate(m, keyRune('j')) // onto the last row
	assert.Equal(t, 4, m.cursorIdx)
	assert.True(t, m.followMode, "j onto the newest row re-engages follow")
}

func TestRequestsCursor_GAndFReengageFollow(t *testing.T) {
	m := newRequestsModel(10, 5)
	m = clientUpdate(m, keyRune('k'))
	require.False(t, m.followMode)

	m = clientUpdate(m, keyRune('G'))
	assert.True(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)

	m = clientUpdate(m, keyRune('g')) // follow off, cursor 0
	require.False(t, m.followMode)
	m = clientUpdate(m, keyRune('F')) // toggle on -> pins to newest
	assert.True(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)
	m = clientUpdate(m, keyRune('F')) // toggle off
	assert.False(t, m.followMode)
	assert.Equal(t, 9, m.cursorIdx)
}

func TestRequestsCursor_FollowArrivalPinsNewest(t *testing.T) {
	m := newRequestsModel(5, 20)
	require.True(t, m.followMode)
	require.Equal(t, 4, m.cursorIdx)

	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, 6)
	assert.Equal(t, 5, m.cursorIdx, "follow pins the cursor to the newest row")
	assert.Equal(t, "req-new", m.cursorID)
	assert.True(t, m.viewport.AtBottom(), "follow keeps the viewport at the bottom")
}

func TestRequestsCursor_AppendTrimMidList(t *testing.T) {
	m := newRequestsModel(maxRequestHistory, 5) // exactly full
	m.followMode = false
	m.setRequestCursor(m.filteredProxyRequests(), 500)
	m.updateViewport()
	require.Equal(t, "req-500", m.cursorID)

	// A new arrival appends and trims the oldest row; the ID-anchored cursor
	// rides down one index onto its still-present row.
	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, maxRequestHistory)
	assert.Equal(t, "req-500", m.cursorID)
	assert.Equal(t, 499, m.cursorIdx)
}

func TestRequestsCursor_TrimmedRowClamps(t *testing.T) {
	m := newRequestsModel(maxRequestHistory, 5)
	m.followMode = false
	m.setRequestCursor(m.filteredProxyRequests(), 0) // cursor on the oldest row
	m.updateViewport()
	require.Equal(t, "req-000", m.cursorID)

	// The arrival trims req-000 away; the cursor clamps to the row now at idx 0.
	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Len(t, m.proxyRequests, maxRequestHistory)
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, "req-001", m.cursorID, "cursor re-anchors to the row now at its index")
}

func TestRequestsCursor_EmptyToNonEmptyFollowOn(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 11})
	require.True(t, m.followMode)
	require.Equal(t, -1, m.cursorIdx, "empty list is the no-cursor sentinel")

	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-x", "/x")))
	assert.Equal(t, 0, m.cursorIdx)
	assert.Equal(t, "req-x", m.cursorID, "follow pins the first arrival")
}

func TestRequestsCursor_EmptyToNonEmptyFollowOff(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.followMode = false
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 11})
	require.Equal(t, -1, m.cursorIdx)

	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-x", "/x")))
	assert.Equal(t, 0, m.cursorIdx, "follow off lands the cursor on row 0")
	assert.Equal(t, "req-x", m.cursorID)
}

func TestRequestsCursor_EmptyListEnterNoop(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 11})

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeRequests, m.viewMode, "Enter on an empty list is a no-op")
	assert.Empty(t, m.selectedRequestID)
}

// TestRequestsCursor_DetailArrivalNoScroll pins the m4 scroll-yank guard: while
// a detail view is open, an arriving proxy request updates only the list data
// and must not scroll the detail viewport out from under the reader.
func TestRequestsCursor_DetailArrivalNoScroll(t *testing.T) {
	m := newRequestsModel(10, 2) // tiny viewport so the detail scrolls
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ClientModel)
	require.Equal(t, ViewModeRequestDetail, m.viewMode)
	require.NotNil(t, cmd, "Enter must return a fetch command")
	// Run the fetch so the detail view has real content to scroll — unlike
	// local mode's synchronous fill, attach mode starts on "Loading..." (too
	// short to scroll) until this command's result is applied.
	m = clientUpdate(m, cmd())
	require.NotNil(t, m.requestDetail)

	// Reader scrolls down inside the detail.
	m.viewport.SetYOffset(3)
	require.Equal(t, 3, m.viewport.YOffset, "precondition: detail content taller than the viewport")

	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-new", "/new")))
	assert.Equal(t, 3, m.viewport.YOffset, "arrival must not scroll the open detail view")
	assert.Len(t, m.proxyRequests, 11, "list data is still updated")
}

func TestRequestsCursor_MarkerOnCursorRow(t *testing.T) {
	m := newRequestsModel(5, 20) // all rows visible
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j')) // cursor on row 1 (req-001, /path/001)
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

// newSearchModel builds a ClientModel in the requests view holding the given request
// records, viewport sized to viewportHeight, follow-mode default (true) so the
// cursor starts pinned to the newest row. Height accounts for the C11 requests
// header row (defaultRequestsHeaderRows) in addition to chrome + panel border.
func newSearchModel(viewportHeight int, reqs []proxy.RequestRecord) ClientModel {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.proxyRequests = reqs
	return clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: viewportHeight + defaultChromeHeight() + defaultPanelBorder() + defaultRequestsHeaderRows()})
}

// commitSearch drives the `/`-search flow: enter search mode, set the query,
// and press Enter. handleSearchKey's enter case routes the commit itself
// (jump in the requests view, filter in the logs view) based on m.viewMode,
// so one driver helper serves both views.
func commitSearch(m ClientModel, query string) ClientModel {
	m = clientUpdate(m, keyRune('/'))
	m.textInput.SetValue(query)
	return clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
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
	m = clientUpdate(m, keyRune('g')) // cursor row 0 (/alpha), follow off
	require.Equal(t, "r0", m.cursorID)

	// Cursor already sits on a match: at-or-after leaves it in place.
	m = commitSearch(m, "alpha")
	assert.Equal(t, "alpha", m.requestSearchQuery)
	assert.Equal(t, "r0", m.cursorID, "at-or-after stays on a cursor that already matches")

	// From row 1 (/beta, no match), the next match at-or-after is row 2.
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j')) // row 1
	require.Equal(t, "r1", m.cursorID)
	m = commitSearch(m, "alpha")
	assert.Equal(t, "r2", m.cursorID, "jumps forward to the next match")

	// From row 3 (/gamma, no match), at-or-after wraps to row 0.
	m = clientUpdate(m, keyRune('j')) // row 3 (last)
	require.Equal(t, "r3", m.cursorID)
	m = commitSearch(m, "alpha")
	assert.Equal(t, "r0", m.cursorID, "at-or-after wraps around to the first match")
}

func TestRequestsSearch_NextPrevWrap(t *testing.T) {
	m := newSearchModel(20, searchFixture())
	m = clientUpdate(m, keyRune('g')) // row 0
	m = commitSearch(m, "alpha")
	require.Equal(t, "r0", m.cursorID)

	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "r2", m.cursorID, "n advances to the next match")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "r0", m.cursorID, "n wraps to the first match")

	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, "r2", m.cursorID, "N wraps backward to the last match")
	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, "r0", m.cursorID, "N retreats to the previous match")
}

func TestRequestsSearch_OffScreenScrollsMinimally(t *testing.T) {
	reqs := makeTestRequests(30)
	reqs[2].URL = "/target/a"
	reqs[25].URL = "/target/b"
	m := newSearchModel(5, reqs) // viewport height 5
	m = clientUpdate(m, keyRune('g'))
	require.Equal(t, 0, m.viewport.YOffset)

	// Jump to the first match (row 2), already on-screen: no scroll.
	m = commitSearch(m, "target")
	assert.Equal(t, 2, m.cursorIdx)
	assert.True(t, cursorVisible(m))

	// n jumps down to row 25 off-screen: scroll the minimum (YOffset = 25-5+1).
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 25, m.cursorIdx)
	assert.True(t, cursorVisible(m))
	assert.Equal(t, 21, m.viewport.YOffset, "downward jump scrolls minimally")

	// N jumps back up to row 2 off-screen: scroll up to place it at the top.
	m = clientUpdate(m, keyRune('N'))
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
	m.setRequestsFilterQuery("api") // active `s` filter: only the 10 api rows remain
	m.followMode = false
	m.updateViewport()
	m = clientUpdate(m, keyRune('g'))

	// Matches are computed over the FILTERED list, so /match/b (web) is skipped.
	m = commitSearch(m, "match")
	assert.Equal(t, "req-004", m.cursorID, "search over the filtered list finds the first api match")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "req-012", m.cursorID)
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "req-004", m.cursorID, "wraps over api matches only, skipping the filtered-out web row")
	assert.Equal(t, "api", m.requestsFilter.RawQuery, "the `s` filter is untouched")
}

func TestRequestsSearch_DoesNotFilter(t *testing.T) {
	reqs := makeTestRequests(10)
	reqs[3].URL = "/needle"
	m := newSearchModel(20, reqs)
	before := len(m.filteredProxyRequests())

	m = commitSearch(m, "needle")
	assert.Empty(t, m.requestsFilter.RawQuery, "/ in the requests view must not touch the `s` filter")
	assert.Equal(t, "needle", m.requestSearchQuery)
	assert.Len(t, m.filteredProxyRequests(), before, "/ navigates; it must not hide rows")
	assert.Len(t, m.proxyRequests, 10)
}

func TestRequestsSearch_NoMatch(t *testing.T) {
	reqs := makeTestRequests(10)
	m := newSearchModel(20, reqs)
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j')) // row 1
	require.Equal(t, 1, m.cursorIdx)

	m = commitSearch(m, "zzz-nomatch")
	assert.Equal(t, 1, m.cursorIdx, "a query with no match leaves the cursor unmoved")
	assert.Equal(t, "req-001", m.cursorID)

	// n/N are also no-ops when nothing matches.
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 1, m.cursorIdx)

	bar := m.statusBar(footerMsg{})
	assert.Contains(t, bar, "/zzz-nomatch (0 matches)", "status shows the 0-match form")
}

func TestRequestsSearch_EscClearsQuery(t *testing.T) {
	reqs := makeTestRequests(10)
	reqs[3].URL = "/needle"
	m := newSearchModel(20, reqs)
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.requestSearchQuery)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.requestSearchQuery, "esc clears the requests-view search query")
}

// --- Logs-view search navigation tests (C4 / D6-D10) ---

// newLogsModel builds a ClientModel in the (default) logs view, viewport sized so its
// content height is viewportHeight (handleWindowSize/relayout subtracts
// defaultChromeHeight + defaultPanelBorder), then streams the given lines through handleLogEntry so each entry is
// stamped with a unique non-zero Seq — the logs search cursor anchors by Seq, so
// a zero Seq would break the anchor. followMode starts true (default), pinning
// the cursor to the newest line until a jump or scroll disengages it.
func newLogsModel(viewportHeight int, lines []string) ClientModel {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: viewportHeight + defaultChromeHeight() + defaultPanelBorder()})
	base := time.Unix(0, 0)
	for i, line := range lines {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
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
func logCursorLine(t *testing.T, m ClientModel) string {
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
	assert.Empty(t, m.logsFilter.RawQuery, "logs-view / must not touch the `s` filter")
	assert.Empty(t, m.requestSearchQuery, "logs-view / must not set the requests query")
	assert.Len(t, m.filteredEntries(), before, "logs-view / navigates; it must not hide lines")
	assert.Equal(t, "drop it", logCursorLine(t, m), "the cursor lands on the matching line")
}

func TestLogsSearch_JumpAtOrAfterAndWrap(t *testing.T) {
	// "needle" matches rows 0 and 2; rows 1 and 3 do not.
	m := newLogsModel(20, []string{"needle a", "x", "needle b", "z"})
	m = clientUpdate(m, keyRune('g')) // top of viewport, follow off; origin -> row 0
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
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, 0, m.logCursorIdx)

	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 2, m.logCursorIdx, "n advances to the next match")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 0, m.logCursorIdx, "n wraps to the first match")

	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, 2, m.logCursorIdx, "N wraps backward to the last match")
	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, 0, m.logCursorIdx, "N retreats to the previous match")
}

func TestLogsSearch_ComposesWithFilter(t *testing.T) {
	// The `s` filter keeps only "keep" lines; search then navigates WITHIN that
	// filtered set, never selecting a hidden "drop" row.
	m := newLogsModel(20, []string{
		"keep needle a", "drop needle b", "keep plain", "keep needle c", "drop needle d",
	})
	m.setLogsFilterQuery("keep") // active `s` filter -> rows 0,2,3 survive
	m.followMode = false
	m.updateViewport()
	m = clientUpdate(m, keyRune('g')) // origin -> top of the filtered list

	m = commitSearch(m, "needle")
	assert.Equal(t, "keep needle a", logCursorLine(t, m), "search starts within the `s`-filtered set")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "keep needle c", logCursorLine(t, m), "n skips the filtered-out drop rows")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, "keep needle a", logCursorLine(t, m), "wraps over the filtered matches only")
	assert.Equal(t, "keep", m.logsFilter.RawQuery, "the `s` filter is untouched")
}

func TestLogsSearch_NoMatch(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "bbb", "ccc"})
	m = clientUpdate(m, keyRune('g'))

	m = commitSearch(m, "zzz")
	assert.Equal(t, "zzz", m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx, "a query with no match leaves no cursor")

	// n/N are also no-ops when nothing matches.
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, -1, m.logCursorIdx)

	bar := m.statusBar(footerMsg{})
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
	assert.Contains(t, m.statusBar(footerMsg{}), "/zzz (0 matches)")
}

func TestLogsSearch_EscClears(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "needle", "bbb"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)
	require.NotEqual(t, -1, m.logCursorIdx)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logSearchQuery, "esc clears the logs-view search query")
	assert.Equal(t, int64(0), m.logCursorSeq, "esc resets the logs cursor anchor")
	assert.Equal(t, -1, m.logCursorIdx, "esc resets the logs cursor index")
}

func TestLogsSearch_StatusIndicator(t *testing.T) {
	m := newLogsModel(20, []string{"needle a", "x", "needle b"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, 0, m.logCursorIdx)

	assert.Contains(t, m.statusBar(footerMsg{}), "/needle (1/2)", "shows the cursor's match position of the total")
	m = clientUpdate(m, keyRune('n'))
	assert.Contains(t, m.statusBar(footerMsg{}), "/needle (2/2)", "advancing updates the position")
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
	require.Contains(t, m.statusBar(footerMsg{}), "/hit (1/1)")

	// A new log line arrives while the search is parked off-newest.
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Unix(10, 0),
		Process:   "p",
		Line:      "e newest",
	}))

	assert.False(t, m.followMode, "a streaming arrival must not re-engage follow while a search is parked")
	assert.Equal(t, "hit mid", logCursorLine(t, m), "the cursor stays on the match, not yanked to the new newest row")
	assert.Contains(t, m.statusBar(footerMsg{}), "/hit (1/1)", "match position is preserved across the arrival")
}

func TestLogsSearch_EvictionAnchorSurvives(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 5 + defaultChromeHeight() + defaultPanelBorder()}) // viewport height 5
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
	m = clientUpdate(m, keyRune('g')) // GotoTop, follow off
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
		require.NotEqual(t, needleSeq, e.DisplaySeq, "NEEDLE has been evicted")
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

	m = clientUpdate(m, keyRune('k')) // disengage follow
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

	// ASCII line + ASCII query: the matched run is wrapped in styles.SearchHighlight.
	// Leading/trailing runs go through styles.Base (plan 024 F1).
	m.logSearchQuery = "match"
	ascii := m.styleLogContent("hello match world", styles.Base)
	assert.NotEqual(t, "hello match world", ascii, "an ASCII match is inline-highlighted")
	assert.Contains(t, ascii, styles.SearchHighlight.Render("match"), "the matched run is styled")
	assert.Contains(t, ascii, styles.Base.Render("hello "))
	assert.Contains(t, ascii, styles.Base.Render(" world"))

	// Unicode line: case-folding would shift byte offsets, so the guard trips and
	// the line falls back to no inline highlight (row marker alone) — Base-wrapped.
	uni := "日本語 match テスト"
	assert.Equal(t, styles.Base.Render(uni), m.styleLogContent(uni, styles.Base),
		"a unicode line falls back to no highlight (Base-wrapped)")

	// ESC-bearing line: a digit query can match inside the ANSI escape, so the
	// guard trips and the escape is left intact rather than split.
	m.logSearchQuery = "31"
	ansiLine := "x \x1b[31mred\x1b[0m match"
	got := m.styleLogContent(ansiLine, styles.Base)
	assert.Equal(t, styles.Base.Render(ansiLine), got, "an ESC-bearing line falls back, escape intact")
	assert.Contains(t, got, "\x1b[31mred\x1b[0m", "source escape survives inside Base wrap")
}

func TestLogsSearch_UnicodeAndAnsiRenderSafely(t *testing.T) {
	// End-to-end: a unicode line and a line already carrying ANSI escapes. The
	// search must not panic or split the escape, and the row marker still lands
	// on the matched row.
	m := newLogsModel(20, []string{"before", "cafe \x1b[31mMATCH\x1b[0m after", "café résumé MATCH"})
	m = clientUpdate(m, keyRune('g'))
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
	m.setRequestsFilterQuery("path")
	m.followMode = false
	m.updateViewport()
	m = clientUpdate(m, keyRune('g'))

	m = commitSearch(m, "needle")
	bar := m.statusBar(footerMsg{})
	assert.Contains(t, bar, "/needle (1/1)", "search indicator wins, with the cursor's match position")
	assert.Contains(t, bar, "filter: path", "the active `s` filter is appended")
	assert.NotContains(t, bar, "Showing:", "solo is never shown in the requests view")

	// Prompt precedence: while typing a search, the input prompt wins over the
	// committed indicator.
	m2 := clientUpdate(m, keyRune('/'))
	assert.Contains(t, m2.statusBar(footerMsg{}), "Search:", "the mode prompt takes precedence")
}

func TestRequestsSearch_UnicodeStatusWidth(t *testing.T) {
	m := newSearchModel(40, makeTestRequests(5))
	m.width = 120

	m.requestSearchQuery = "日本語テスト"
	unicodeBar := m.statusBar(footerMsg{})

	// The layout is measured in display columns, not bytes: an ASCII query of
	// the same DISPLAY width must produce a bar of the same rendered width.
	// Scope note: this guards layout stability for wide-rune queries (the
	// left side, laid out by lipgloss). It cannot catch a regression of the
	// right side's lipgloss.Width back to len — the right side is
	// structurally ASCII, so no assertion can distinguish them there.
	m.requestSearchQuery = strings.Repeat("x", lipgloss.Width("日本語テスト"))
	asciiBar := m.statusBar(footerMsg{})
	assert.Equal(t, lipgloss.Width(asciiBar), lipgloss.Width(unicodeBar),
		"a Unicode query must not change the status-bar layout width")
}

func TestRequestsSearch_WrapToNewestNoFollow(t *testing.T) {
	reqs := makeTestRequests(6)
	reqs[0].URL = "/hit/a"
	reqs[5].URL = "/hit/b" // the newest (last) row also matches
	m := newSearchModel(20, reqs)
	m = clientUpdate(m, keyRune('g')) // cursor row 0, follow off
	require.False(t, m.followMode)

	m = commitSearch(m, "hit")
	require.Equal(t, 0, m.cursorIdx)
	require.False(t, m.followMode)

	// n wraps forward onto the newest row. Unlike `j`, a search jump is
	// positioning, not scrolling intent: it must NOT re-engage follow.
	m = clientUpdate(m, keyRune('n'))
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
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "hit")
	require.Equal(t, 2, m.cursorIdx)

	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 2, m.cursorIdx, "n with a sole match is a no-op")
	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, 2, m.cursorIdx, "N with a sole match is a no-op")
	assert.Equal(t, "req-002", m.cursorID)
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

// --- Live (incremental) `/` search tests (plan 030 C1 / WS1-WS2) ---

// typeSearch drives REAL per-rune keystrokes into an open `/` bar. The live path
// runs off textinput value changes, so it must be exercised keystroke by
// keystroke — commitSearch's SetValue deliberately bypasses it and only the
// Enter re-apply saves it (plan 030 WS2).
func typeSearch(m ClientModel, term string) ClientModel {
	for _, r := range term {
		m = clientUpdate(m, keyRune(r))
	}
	return m
}

// backspaceSearch is typeSearch's inverse: n real backspace keystrokes, the
// keys that shrink a term back toward the origin.
func backspaceSearch(m ClientModel, n int) ClientModel {
	for i := 0; i < n; i++ {
		m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	return m
}

// liveSearchRequests builds requests whose URLs make prefix typing meaningful:
// "a" and "ab" first match q1, "abc" only q3. IDs, method, status and duration
// carry none of those letters, so a one-rune term can only match a URL.
func liveSearchRequests() []proxy.RequestRecord {
	base := time.Unix(0, 0)
	urls := []string{"/zero", "/ab-hit", "/filler", "/abc-hit", "/end"}
	reqs := make([]proxy.RequestRecord, len(urls))
	for i, u := range urls {
		reqs[i] = proxy.RequestRecord{
			ID: fmt.Sprintf("q%d", i), Timestamp: base.Add(time.Duration(i) * time.Second),
			Method: "GET", URL: u, StatusCode: 200, Duration: 5 * time.Millisecond,
		}
	}
	return reqs
}

func TestLiveSearch_LogsSeeksFromOriginPerKeystroke(t *testing.T) {
	m := newLogsModel(20, []string{"zero", "ab hit", "filler", "abc hit", "tail"})
	m = clientUpdate(m, keyRune('g')) // top, follow off -> origin is row 0
	m = clientUpdate(m, keyRune('/'))

	// Each keystroke applies the term and jumps — no Enter involved.
	m = typeSearch(m, "a")
	assert.Equal(t, "a", m.logSearchQuery, "the keystroke writes the committed query field")
	assert.Equal(t, "ab hit", logCursorLine(t, m))
	m = typeSearch(m, "b")
	assert.Equal(t, "ab hit", logCursorLine(t, m))
	m = typeSearch(m, "c")
	assert.Equal(t, "abc hit", logCursorLine(t, m), "a narrower term seeks on from the ORIGIN, not from row 1")

	// Backspacing to a shorter term must find the match the previous landing had
	// already passed — the whole point of restore-then-seek. Seeking from the
	// cursor would strand it on "abc hit", which also contains "ab".
	m = backspaceSearch(m, 1)
	assert.Equal(t, "ab", m.logSearchQuery)
	assert.Equal(t, "ab hit", logCursorLine(t, m), "backspace re-seeks from the origin")
}

func TestLiveSearch_RequestsSeeksFromOriginPerKeystroke(t *testing.T) {
	m := newSearchModel(20, liveSearchRequests())
	m = clientUpdate(m, keyRune('g')) // cursor q0, follow off -> origin is q0
	require.Equal(t, "q0", m.cursorID)
	m = clientUpdate(m, keyRune('/'))

	m = typeSearch(m, "a")
	assert.Equal(t, "a", m.requestSearchQuery)
	assert.Equal(t, "q1", m.cursorID)
	m = typeSearch(m, "b")
	assert.Equal(t, "q1", m.cursorID)
	m = typeSearch(m, "c")
	assert.Equal(t, "q3", m.cursorID, "a narrower term seeks on from the ORIGIN row")

	m = backspaceSearch(m, 1)
	assert.Equal(t, "q1", m.cursorID, "backspace re-seeks from the origin")

	// Emptying the bar previews a cleared search: no query, cursor back on the
	// origin row, `s` filter never touched.
	m = backspaceSearch(m, 2)
	assert.Empty(t, m.requestSearchQuery)
	assert.Equal(t, "q0", m.cursorID, "an empty bar leaves the cursor at the origin")
	assert.Empty(t, m.requestsFilter.RawQuery)
}

func TestLiveSearch_EmptyInputRestoresOriginView(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	lines[20] = "abc target"
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k')) // disengage follow
	require.False(t, m.followMode)
	m.viewport.SetYOffset(2) // origin: top visible row 2, no parked cursor

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "abc")
	require.Equal(t, 20, m.logCursorIdx)
	require.NotEqual(t, 2, m.viewport.YOffset, "the live jump scrolled away from the origin")

	// Backspace the term away: identical to no search at the origin.
	m = backspaceSearch(m, 3)
	assert.Empty(t, m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx, "an empty bar leaves no cursor")
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.Equal(t, 2, m.viewport.YOffset, "the view is back at the origin scroll position")
	assert.NotContains(t, m.viewport.View(), "❯", "no marker renders without a query")
}

func TestLiveSearch_NoMatchWhileTypingClearsCursorAtOrigin(t *testing.T) {
	m := newLogsModel(20, []string{"alpha", "hit here", "gamma"})
	m = clientUpdate(m, keyRune('g')) // origin: row 0, follow off
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "hit")
	require.Equal(t, "hit here", logCursorLine(t, m))

	m = typeSearch(m, "z") // "hitz" matches nothing
	assert.Equal(t, "hitz", m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx, "a no-match keystroke clears the cursor")
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.Equal(t, 0, m.viewport.YOffset, "the view stays at the origin")

	// A no-match keystroke must NOT disengage follow: only a jump that lands off
	// the newest row does that, and there is no jump here (setLogCursor never
	// touches followMode).
	m2 := newLogsModel(20, []string{"a", "b", "c"})
	require.True(t, m2.followMode)
	m2 = clientUpdate(m2, keyRune('/'))
	m2 = typeSearch(m2, "zzz")
	assert.True(t, m2.followMode, "a no-match keystroke leaves follow engaged")
	assert.Equal(t, -1, m2.logCursorIdx)
}

func TestLiveSearch_LogsArrivalsWhileBarOpen(t *testing.T) {
	// The origin under follow is the row that was newest AT THE PRESS, not
	// whatever has streamed in since: the at-or-after seek must find the ARRIVED
	// match rather than wrapping around to the older one.
	m := newLogsModel(20, []string{"a needle", "b", "c"})
	require.True(t, m.followMode)
	m = clientUpdate(m, keyRune('/'))

	for i, line := range []string{"d needle", "e"} {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Timestamp: time.Unix(int64(10+i), 0),
			Process:   "p",
			Line:      line,
		}))
	}

	m = typeSearch(m, "needle")
	assert.Equal(t, "d needle", logCursorLine(t, m),
		"the seek starts at the press-time newest row, so the arrival is the first match at-or-after it")
}

func TestLiveSearch_LogsEvictionWhileBarOpen(t *testing.T) {
	// Sized like any logs model, but fed DIRECTLY (not through clientUpdate):
	// this test needs ~1200 entries to push past the eviction ring, and a full
	// re-render per line would dominate its runtime.
	m := newLogsModel(5, nil)
	feed := func(line string) {
		m.handleLogEntry(domain.LogEntry{Timestamp: time.Unix(0, 0), Process: "p", Line: line})
	}
	for i := 0; i < 300; i++ {
		feed(fmt.Sprintf("log %04d", i))
	}
	feed("NEEDLE row")
	for i := 0; i < 300; i++ {
		feed(fmt.Sprintf("log %04d", 300+i))
	}

	m = clientUpdate(m, keyRune('g')) // origin: oldest retained line, follow off
	m = clientUpdate(m, keyRune('/'))
	originSeq := m.searchSession.logAnchorSeq
	require.NotZero(t, originSeq)

	// Stream past the ring while the bar is open: the origin anchor is evicted
	// outright, so the restore falls back to the oldest retained line.
	for i := 0; i < 600; i++ {
		feed(fmt.Sprintf("flood %04d", i))
	}
	require.Len(t, m.logEntries, maxLogEntries)
	for _, e := range m.logEntries {
		require.NotEqual(t, originSeq, e.DisplaySeq, "the origin anchor has been evicted")
	}

	m = typeSearch(m, "NEEDLE")
	require.GreaterOrEqual(t, m.logCursorIdx, 0, "the search still lands with an evicted origin")
	assert.Contains(t, logCursorLine(t, m), "NEEDLE")
	sp, ok := m.logRowSpans[m.logCursorSeq]
	require.True(t, ok)
	assert.True(t, sp.Last >= m.viewport.YOffset && sp.First < m.viewport.YOffset+m.viewport.Height,
		"the live jump scrolled the match into view")
}

func TestLiveSearch_LogsEvictedOriginSeeksFromOldestRetained(t *testing.T) {
	// The ring evicts from the FRONT, so an evicted origin sat before every line
	// still retained: the at-or-after scan has to start at the oldest retained
	// line. Resuming from the origin's remembered INDEX would point mid-list and
	// skip the true first match — which is why the session anchors carry no
	// index at all (see sessionLogOriginIdx).
	m := newLogsModel(5, nil)
	feed := func(line string) {
		m.handleLogEntry(domain.LogEntry{Timestamp: time.Unix(0, 0), Process: "p", Line: line})
	}
	for i := 0; i < maxLogEntries; i++ {
		switch i {
		case 400:
			feed("NEEDLE early")
		case 900:
			feed("NEEDLE late")
		default:
			feed(fmt.Sprintf("log %04d", i))
		}
	}

	m = clientUpdate(m, keyRune('k')) // disengage follow
	m.viewport.SetYOffset(200)        // origin: entry 200, well into the list
	m = clientUpdate(m, keyRune('/'))
	originSeq := m.searchSession.logAnchorSeq
	require.NotZero(t, originSeq)

	// Flood past the origin while the bar is open: it is evicted, BOTH matches
	// survive, and the origin's old index now points between them.
	for i := 0; i < 300; i++ {
		feed(fmt.Sprintf("flood %04d", i))
	}
	entries := m.filteredEntries()
	require.Equal(t, -1, entryIndexOfSeq(entries, originSeq), "the origin anchor was evicted")
	require.Contains(t, entries[100].Line, "NEEDLE early")
	require.Contains(t, entries[600].Line, "NEEDLE late")

	m = typeSearch(m, "NEEDLE")
	assert.Equal(t, "NEEDLE early", logCursorLine(t, m),
		"an evicted origin seeks from the oldest retained line, not from a stale index")
}

func TestLiveSearch_EmptyListAtPressSeeksFromFirstArrival(t *testing.T) {
	// Nothing on screen when `/` was pressed, so every line retained now arrived
	// AFTER the press: the at-or-after origin is the oldest of them. Degrading to
	// the ordinary follow-mode origin (the newest row) would wrap and land on the
	// LAST match instead of the first.
	m := newLogsModel(20, nil)
	require.True(t, m.followMode)
	require.Empty(t, m.filteredEntries())

	m = clientUpdate(m, keyRune('/'))
	require.Zero(t, m.searchSession.logAnchorSeq, "an empty list has no anchor to capture")

	for i, line := range []string{"first needle", "second needle"} {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Timestamp: time.Unix(int64(i), 0),
			Process:   "p",
			Line:      line,
		}))
	}

	m = typeSearch(m, "needle")
	assert.Equal(t, "first needle", logCursorLine(t, m),
		"the origin is the oldest line that arrived after the press")
}

func TestLiveSearch_RequestsArrivalsWhileBarOpen(t *testing.T) {
	// The requests origin is the cursor row's ID, so a rebuild that drops older
	// rows (a reconnect sync) shifts every index without moving the origin.
	reqs := []proxy.RequestRecord{
		newArrival("r0", "/needle-old"),
		newArrival("r1", "/x1"),
		newArrival("r2", "/x2"),
		newArrival("r3", "/needle-mid"),
		newArrival("r4", "/x4"),
	}
	m := newSearchModel(20, reqs)
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j'))
	m = clientUpdate(m, keyRune('j'))
	require.Equal(t, "r2", m.cursorID, "origin: the r2 row")
	m = clientUpdate(m, keyRune('/'))

	// A live arrival, then a sync rebuild that drops the two oldest rows: r2 is
	// now index 0, and the old index 2 holds r4.
	m = clientUpdate(m, ProxyRequestMsg(newArrival("r5", "/needle-new")))
	m = clientUpdate(m, RequestsSyncMsg{Snapshot: []proxy.RequestRecord{
		reqs[2], reqs[3], reqs[4], newArrival("r5", "/needle-new"),
	}})
	require.Equal(t, "r2", m.cursorID, "the ID-anchored cursor rode the rebuild")

	m = typeSearch(m, "needle")
	assert.Equal(t, "r3", m.cursorID,
		"the seek starts at the origin RECORD (now index 0), not at its old index")
}

func TestLiveSearch_ComposesWithStringFilter(t *testing.T) {
	m := newLogsModel(20, []string{
		"keep needle-a", "drop needle-b", "keep plain", "keep needle-c", "drop needle-d",
	})
	m.setLogsFilterQuery("keep") // active `s` filter -> rows 0,2,3 survive
	m.followMode = false
	m.updateViewport()
	m = clientUpdate(m, keyRune('g')) // origin: top of the FILTERED list

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "needle")
	assert.Equal(t, "keep needle-a", logCursorLine(t, m), "live search runs within the filtered set")
	m = typeSearch(m, "-c")
	assert.Equal(t, "keep needle-c", logCursorLine(t, m), "narrowing skips the filtered-out drop rows")
	assert.Equal(t, "keep", m.logsFilter.RawQuery, "the `s` filter is untouched")
	assert.Len(t, m.filteredEntries(), 3, "search never hides lines the filter kept")
}

func TestLiveSearch_EnterOnEmptyBarClearsCommittedSearch(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "needle", "bbb"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)
	require.Equal(t, 1, m.logCursorIdx)

	// Reopen (the bar comes up seeded — plan 030 WS4), clear it, and commit the
	// empty bar: a committed CLEARED search.
	m = clientUpdate(m, keyRune('/'))
	require.Equal(t, "needle", m.textInput.Value(), "the reopened bar carries the previous term")
	m = backspaceSearch(m, len("needle"))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Empty(t, m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx)
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.False(t, m.searchSession.active, "the session is cleared on exit")
	assert.NotContains(t, m.viewport.View(), "❯")
}

func TestLiveSearch_EnterKeepsLandingAndNextPrevCycleFromIt(t *testing.T) {
	m := newLogsModel(20, []string{"needle a", "x", "needle b", "z"})
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "needle")
	require.Equal(t, 0, m.logCursorIdx)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, "needle", m.logSearchQuery, "Enter keeps the live-applied query")
	assert.Equal(t, 0, m.logCursorIdx, "and the live landing")
	assert.False(t, m.searchSession.active)

	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 2, m.logCursorIdx, "n cycles on from the live landing")
	m = clientUpdate(m, keyRune('n'))
	assert.Equal(t, 0, m.logCursorIdx, "n wraps")
	m = clientUpdate(m, keyRune('N'))
	assert.Equal(t, 2, m.logCursorIdx)
}

func TestLiveSearch_CursorMovementKeysDoNotReseek(t *testing.T) {
	// Cursor-movement keys leave the term alone, so they must re-run neither the
	// O(n) scan nor the restore — the live apply is gated on the value actually
	// changing. Parking the search cursor somewhere a re-seek would move it away
	// from is what makes the no-op observable.
	m := newLogsModel(20, []string{"zero", "ab hit", "filler", "abc hit", "tail"})
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "abc")
	require.Equal(t, 3, m.logCursorIdx)
	m.setLogCursor(m.filteredEntries(), 0)

	for _, kt := range []tea.KeyType{tea.KeyLeft, tea.KeyRight, tea.KeyHome, tea.KeyEnd} {
		m = clientUpdate(m, tea.KeyMsg{Type: kt})
	}
	assert.Equal(t, "abc", m.logSearchQuery, "movement keys are not text")
	assert.Equal(t, 0, m.logCursorIdx, "movement keys must not re-seek")
	assert.Equal(t, ModeSearch, m.mode)
}

func TestLiveSearch_MouseMenuOpenCommitsLiveQuery(t *testing.T) {
	// Mouse-open blurs the bar; for `/` that is Enter-equivalent (plan 030 WS2):
	// the live-applied query stands and the session is cleared.
	m := newLogsModel(20, []string{"alpha", "needle here", "gamma"})
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "needle")
	require.Equal(t, 1, m.logCursorIdx)

	_ = m.View() // record hit-rects for the menu bar
	require.NotEmpty(t, m.mustHits().menuCells)
	hit := m.mustHits().menuCells[0]
	m = clientUpdate(m, tea.MouseMsg{
		X:      hit.Rect.X,
		Y:      hit.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	assert.Equal(t, ModeNormal, m.mode)
	assert.True(t, m.menuOpen())
	assert.Equal(t, "needle", m.logSearchQuery, "the live query stands after the blur")
	assert.Equal(t, 1, m.logCursorIdx, "and so does its landing")
	assert.False(t, m.searchSession.active, "the session is cleared")
}

func TestLiveSearch_RequestsFollowArrivalsSeekFromPressTimeNewest(t *testing.T) {
	// Under follow the restore pins the CURSOR to the current newest row — right
	// for the view, which is what follow means — but the seek must still start on
	// the row `/` was pressed on. Seeking from the restored cursor would wrap
	// past the arrival onto the older match, and flip the landing on every
	// arrival mid-typing.
	m := newSearchModel(20, []proxy.RequestRecord{
		newArrival("r0", "/match-a"),
		newArrival("r1", "/x1"),
		newArrival("r2", "/x2"),
	})
	require.True(t, m.followMode)
	require.Equal(t, "r2", m.cursorID, "follow pins the cursor to the press-time newest row")

	m = clientUpdate(m, keyRune('/'))
	m = clientUpdate(m, ProxyRequestMsg(newArrival("r3", "/match-b")))
	m = clientUpdate(m, ProxyRequestMsg(newArrival("r4", "/x4")))
	require.Equal(t, "r4", m.cursorID, "follow moved the live cursor to the newest arrival")

	m = typeSearch(m, "match")
	assert.Equal(t, "r3", m.cursorID,
		"the seek starts at the press-time newest row, so the arrival is the first match at-or-after it")
	assert.False(t, m.followMode, "the jump landed off the newest row")
}

func TestLiveSearch_EmptyInputRestoresOriginViewUnderWrap(t *testing.T) {
	// With wrap on, the 2-column marker prefix a non-empty query adds MOVES wrap
	// points — these lines are sized to flip from one display row to two — so the
	// row spans the restore translates its anchor through are stale the moment
	// the query changes. Emptying the bar must land on the anchor's row in the
	// render the user actually sees, not in the marker-era one.
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = strings.Repeat("x", 38)
	}
	lines[30] = "abc" + strings.Repeat("x", 35)
	m := newLogsModelNarrow(60, 6, lines)
	m.settings.Wrap = true
	m.followMode = false
	m.updateViewport()
	m.viewport.SetYOffset(20)

	m = clientUpdate(m, keyRune('/'))
	anchorSeq := m.searchSession.logAnchorSeq
	require.NotZero(t, anchorSeq)
	require.Equal(t, 20, m.logRowSpans[anchorSeq].First, "one row per entry while no query is set")

	m = typeSearch(m, "abc")
	require.Equal(t, 30, m.logCursorIdx, "the live jump moved the view off the origin")
	require.Equal(t, 40, m.logRowSpans[anchorSeq].First, "the marker prefix wrapped every entry onto two rows")

	m = backspaceSearch(m, 3)
	require.Empty(t, m.logSearchQuery)
	assert.Equal(t, m.logRowSpans[anchorSeq].First, m.viewport.YOffset,
		"the restored offset is the anchor's row in the CURRENT (marker-free) render")
	assert.Equal(t, 20, m.viewport.YOffset)
}

// --- `/` bar Esc-cancel tests (plan 030 C2 / WS3) ---

func TestLiveSearch_EscRestoresLogsOrigin(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	lines[20] = "abc target"
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k')) // disengage follow
	m.viewport.SetYOffset(2)          // origin: top visible row 2, no parked cursor

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "abc")
	require.Equal(t, 20, m.logCursorIdx)
	require.NotEqual(t, 2, m.viewport.YOffset, "the live jump scrolled away from the origin")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Empty(t, m.logSearchQuery, "esc cancels the search, not just the edit")
	assert.Equal(t, -1, m.logCursorIdx)
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.Equal(t, 2, m.viewport.YOffset, "the view is back where `/` was pressed")
	assert.False(t, m.followMode, "follow as of the press")
	assert.False(t, m.searchSession.active, "the session is cleared")
	assert.NotContains(t, m.viewport.View(), "❯", "a cancelled search renders as if it never happened")
}

func TestLiveSearch_EscRestoresRequestsOrigin(t *testing.T) {
	m := newSearchModel(20, liveSearchRequests())
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j'))
	require.Equal(t, "q1", m.cursorID, "origin: the q1 row, follow off")

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "abc")
	require.Equal(t, "q3", m.cursorID, "the live jump moved the cursor off the origin")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Empty(t, m.requestSearchQuery)
	assert.Equal(t, "q1", m.cursorID, "the cursor is back on the origin row")
	assert.False(t, m.followMode)
	assert.False(t, m.searchSession.active)
}

func TestLiveSearch_EscScopedToActiveView(t *testing.T) {
	// Esc cancels the ACTIVE view's search only — the `s` bar's Esc scoping.
	// Clearing both views is normal-mode Esc's job, and it is untouched.
	m := newLogsModel(20, []string{"alpha", "needle here", "gamma"})
	m.proxyRequests = makeTestRequests(6)
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, ViewModeRequests, m.viewMode)
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "path/003")
	require.Equal(t, "req-003", m.cursorID)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.requestSearchQuery, "the active view's search is cancelled")
	assert.Equal(t, "needle", m.logSearchQuery, "the other view's committed search survives")
}

func TestLiveSearch_EscEvictedLogsOriginRestoresOldestRetained(t *testing.T) {
	// Best-effort restore: the ring evicted the row the view was anchored on
	// while the bar was open, so the closest truth left is the oldest line still
	// retained.
	m := newLogsModel(5, nil)
	feed := func(line string) {
		m.handleLogEntry(domain.LogEntry{Timestamp: time.Unix(0, 0), Process: "p", Line: line})
	}
	for i := 0; i < maxLogEntries; i++ {
		if i == 400 {
			feed("NEEDLE row")
			continue
		}
		feed(fmt.Sprintf("log %04d", i))
	}

	m = clientUpdate(m, keyRune('k')) // disengage follow
	m.viewport.SetYOffset(200)        // origin: entry 200, well into the list
	m = clientUpdate(m, keyRune('/'))
	originSeq := m.searchSession.logAnchorSeq
	require.NotZero(t, originSeq)
	m = typeSearch(m, "NEEDLE")
	require.GreaterOrEqual(t, m.logCursorIdx, 0)

	// The origin scrolls out of the ring while the bar is still open.
	for i := 0; i < 300; i++ {
		feed(fmt.Sprintf("flood %04d", i))
	}
	require.Equal(t, -1, entryIndexOfSeq(m.filteredEntries(), originSeq), "the origin anchor was evicted")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx)
	assert.Equal(t, 0, m.viewport.YOffset, "an evicted origin restores to the oldest retained line")
	assert.False(t, m.followMode, "and not by silently re-engaging follow")
}

func TestLiveSearch_EscDroppedRequestsOriginFallsBackToNearestIndex(t *testing.T) {
	reqs := []proxy.RequestRecord{
		newArrival("r0", "/x0"),
		newArrival("r1", "/x1"),
		newArrival("r2", "/x2"),
		newArrival("r3", "/x3"),
		newArrival("r4", "/x4"),
		newArrival("r5", "/hit-far"),
	}
	m := newSearchModel(20, reqs)
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('j'))
	m = clientUpdate(m, keyRune('j'))
	require.Equal(t, "r2", m.cursorID, "origin: the r2 row, follow off")

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "hit")
	require.Equal(t, "r5", m.cursorID, "the live jump moved the cursor off the origin")

	// A reconnect sync rebuilds the list without the origin record.
	m = clientUpdate(m, RequestsSyncMsg{Snapshot: []proxy.RequestRecord{
		reqs[0], reqs[1], reqs[3], reqs[4], reqs[5],
	}})
	require.Equal(t, -1, requestIndexOfID(m.filteredProxyRequests(), "r2"), "the origin record is gone")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.requestSearchQuery)
	assert.Equal(t, "r3", m.cursorID, "a dropped origin record falls back to its index, now r3")
	assert.False(t, m.followMode)
}

func TestLiveSearch_EscRestoresFollow(t *testing.T) {
	// A live jump off the newest row disengages follow (it has to, or the pin to
	// newest would undo the jump). Cancelling gives follow back, both views, and
	// the view re-lands on newest — the restored follow WINS over the positional
	// anchor.
	m := newLogsModel(20, []string{"a", "hit mid", "c", "d"})
	require.True(t, m.followMode)
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "hit")
	require.Equal(t, 1, m.logCursorIdx)
	require.False(t, m.followMode, "the live jump landed off the newest row")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, m.followMode, "esc restores follow as of the press")
	assert.Equal(t, -1, m.logCursorIdx)
	assert.True(t, m.viewport.AtBottom(), "restored follow re-lands on newest")

	// And the cancelled search leaves no parked guard behind: arrivals follow
	// again, exactly as they did before `/` was pressed.
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Unix(9, 0), Process: "p", Line: "e newest",
	}))
	assert.True(t, m.followMode, "an arrival after a cancelled search still follows")

	reqs := makeTestRequests(6)
	reqs[2].URL = "/hit/mid"
	m2 := newSearchModel(20, reqs)
	require.True(t, m2.followMode)
	m2 = clientUpdate(m2, keyRune('/'))
	m2 = typeSearch(m2, "hit")
	require.Equal(t, 2, m2.cursorIdx)
	require.False(t, m2.followMode)

	m2 = clientUpdate(m2, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, m2.followMode)
	assert.Equal(t, "req-005", m2.cursorID, "restored follow re-pins the cursor to newest")
}

func TestLiveSearch_EscClearsPreviouslyCommittedSearch(t *testing.T) {
	// Accepted, user-pinned behavior (WS3): Esc cancels the SEARCH, not merely
	// the edit, so a query committed by an earlier Enter goes with it. Once `/`
	// seeds the bar with that query (C3), keeping it is what Enter is for.
	m := newLogsModel(20, []string{"aaa", "needle", "bbb"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)
	require.Equal(t, 1, m.logCursorIdx)

	m = clientUpdate(m, keyRune('/'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logSearchQuery, "the previously committed query is cancelled too")
	assert.Equal(t, -1, m.logCursorIdx)
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.NotContains(t, m.viewport.View(), "❯")
}

func TestLiveSearch_EscRestoresOriginViewUnderWrap(t *testing.T) {
	// Sibling of TestLiveSearch_EmptyInputRestoresOriginViewUnderWrap: cancelling
	// clears the query BEFORE the final render, so the scroll restore reads spans
	// with the marker columns already gone.
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = strings.Repeat("x", 38)
	}
	lines[30] = "abc" + strings.Repeat("x", 35)
	m := newLogsModelNarrow(60, 6, lines)
	m.settings.Wrap = true
	m.followMode = false
	m.updateViewport()
	m.viewport.SetYOffset(20)

	m = clientUpdate(m, keyRune('/'))
	anchorSeq := m.searchSession.logAnchorSeq
	require.NotZero(t, anchorSeq)
	m = typeSearch(m, "abc")
	require.Equal(t, 30, m.logCursorIdx)
	require.Equal(t, 40, m.logRowSpans[anchorSeq].First, "the marker prefix wrapped every entry onto two rows")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	require.Empty(t, m.logSearchQuery)
	assert.Equal(t, m.logRowSpans[anchorSeq].First, m.viewport.YOffset,
		"the restored offset is the anchor's row in the CURRENT (marker-free) render")
	assert.Equal(t, 20, m.viewport.YOffset)
}

func TestLiveSearch_EscOnDrainedRequestsListLandsOnSentinel(t *testing.T) {
	// Everything the origin could name is gone: a reconnect sync drained the list
	// while the bar was open. The restore has no row to land on, so it must reach
	// the no-cursor sentinel rather than clamping onto an index that no longer
	// exists.
	m := newSearchModel(5, makeTestRequests(30))
	m = clientUpdate(m, keyRune('G')) // newest, follow on
	m = clientUpdate(m, keyRune('k')) // origin: req-028, follow off
	require.Equal(t, "req-028", m.cursorID)
	require.Positive(t, m.viewport.YOffset, "the origin is scrolled well down the list")

	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "path/02")
	require.NotEmpty(t, m.requestSearchQuery)

	m = clientUpdate(m, RequestsSyncMsg{}) // empty snapshot: the list is drained
	require.Empty(t, m.filteredProxyRequests())
	require.Equal(t, 0, m.viewport.YOffset, "the drain itself already reset the view")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Empty(t, m.requestSearchQuery)
	assert.Equal(t, -1, m.cursorIdx, "no-cursor sentinel")
	assert.Empty(t, m.cursorID)
	assert.Equal(t, 0, m.viewport.YOffset, "esc must not resurrect the pre-drain scroll position")
	assert.False(t, m.searchSession.active)
}

func TestLiveSearch_LiveApplyRoutesByTheSessionView(t *testing.T) {
	// Guards an INVARIANT, not a reachable state: nothing switches views while
	// the bar is open today. The clear and the origin restore must name the same
	// view, so liveApplySearch routes by the session's view rather than the live
	// one — force them apart and the logs session still owns both halves.
	m := newLogsModel(20, []string{"alpha", "needle here", "gamma"})
	m = clientUpdate(m, keyRune('g'))
	m = clientUpdate(m, keyRune('/'))
	m = typeSearch(m, "needle")
	require.Equal(t, 1, m.logCursorIdx)
	m.proxyRequests = makeTestRequests(4)
	m.requestSearchQuery = "committed"

	m.viewMode = ViewModeRequests // only a test can do this mid-bar
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})

	assert.Empty(t, m.logSearchQuery, "the SESSION's view is the one cancelled")
	assert.Equal(t, -1, m.logCursorIdx, "and the one whose cursor is restored")
	assert.Equal(t, "committed", m.requestSearchQuery, "the other view is left alone")
}

func TestLiveSearch_EscMatchesBackspaceToEmpty(t *testing.T) {
	// Cancel IS the empty-bar preview made permanent (cancelLiveSearch runs
	// through liveApplySearch("")), so backspacing a term away and then pressing
	// Esc must land in exactly the state Esc alone would have reached — one
	// behavior, not two that drift.
	build := func() ClientModel {
		lines := make([]string, 30)
		for i := range lines {
			lines[i] = fmt.Sprintf("row %02d", i)
		}
		lines[20] = "abc target"
		m := newLogsModel(5, lines)
		m = clientUpdate(m, keyRune('k')) // disengage follow
		m.viewport.SetYOffset(2)
		m = clientUpdate(m, keyRune('/'))
		return typeSearch(m, "abc")
	}

	viaBackspace := clientUpdate(backspaceSearch(build(), 3), tea.KeyMsg{Type: tea.KeyEscape})
	direct := clientUpdate(build(), tea.KeyMsg{Type: tea.KeyEscape})

	assert.Equal(t, direct.logSearchQuery, viaBackspace.logSearchQuery)
	assert.Equal(t, direct.logCursorIdx, viaBackspace.logCursorIdx)
	assert.Equal(t, direct.logCursorSeq, viaBackspace.logCursorSeq)
	assert.Equal(t, direct.followMode, viaBackspace.followMode)
	assert.Equal(t, direct.viewport.YOffset, viaBackspace.viewport.YOffset)
	assert.Equal(t, direct.mode, viaBackspace.mode)
	assert.Equal(t, direct.searchSession, viaBackspace.searchSession)
	// ...and that shared state is the origin, not merely equal to itself.
	assert.Empty(t, direct.logSearchQuery)
	assert.Equal(t, -1, direct.logCursorIdx)
	assert.Equal(t, 2, direct.viewport.YOffset)
}

// --- `/` seeds the previous term (plan 030 C3 / WS4) ---

func TestLiveSearch_SlashSeedsPreviousTerm(t *testing.T) {
	m := newLogsModel(20, []string{"aaa", "needle here", "bbb"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "needle")
	require.Equal(t, "needle", m.logSearchQuery)

	// Leave the input cursor mid-term before reopening: SetValue only re-homes it
	// when it sat at 0 or past the end, so this is what makes CursorEnd load-bearing.
	m = clientUpdate(m, keyRune('/'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyLeft})
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyLeft})
	require.Equal(t, len("needle")-2, m.textInput.Position())
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	m = clientUpdate(m, keyRune('/'))
	assert.Equal(t, ModeSearch, m.mode)
	assert.Equal(t, "needle", m.textInput.Value(), "the bar reopens seeded with the committed term")
	assert.Equal(t, len("needle"), m.textInput.Position(), "cursor at the end, ready to extend")

	// So typing EXTENDS the previous term rather than starting a fresh one.
	m = typeSearch(m, "!")
	assert.Equal(t, "needle!", m.logSearchQuery)

	// With nothing committed there is nothing to seed: a first search opens empty.
	fresh := newLogsModel(20, []string{"x"})
	fresh = clientUpdate(fresh, keyRune('/'))
	assert.Empty(t, fresh.textInput.Value())
	assert.Equal(t, -1, fresh.logCursorIdx, "an empty seed applies nothing at all")
}

func TestLiveSearch_ReopenOverParkedMatchDoesNotMoveView(t *testing.T) {
	// The seed applies itself, and its at-or-after seek starts from an origin
	// that INCLUDES the cursor the term already parked — so it lands back on that
	// same row and reopening the bar is visually inert.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	lines[20] = "abc target"
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k')) // disengage follow
	m.viewport.SetYOffset(2)
	m = commitSearch(m, "abc")
	require.Equal(t, 20, m.logCursorIdx)
	seq, yOffset, follow := m.logCursorSeq, m.viewport.YOffset, m.followMode

	m = clientUpdate(m, keyRune('/'))
	assert.Equal(t, "abc", m.textInput.Value())
	assert.Equal(t, 20, m.logCursorIdx, "the parked cursor stays put")
	assert.Equal(t, seq, m.logCursorSeq)
	assert.Equal(t, yOffset, m.viewport.YOffset, "and the scroll position with it")
	assert.Equal(t, follow, m.followMode)

	reqs := makeTestRequests(20)
	reqs[7].URL = "/hit/here"
	m2 := newSearchModel(5, reqs)
	m2 = clientUpdate(m2, keyRune('g'))
	m2 = commitSearch(m2, "hit")
	require.Equal(t, "req-007", m2.cursorID)
	yOffset2 := m2.viewport.YOffset

	m2 = clientUpdate(m2, keyRune('/'))
	assert.Equal(t, "hit", m2.textInput.Value())
	assert.Equal(t, "req-007", m2.cursorID, "the requests cursor stays on its match")
	assert.Equal(t, yOffset2, m2.viewport.YOffset)
}

func TestLiveSearch_ReopenAfterNoMatchMayJump(t *testing.T) {
	// Accepted qualifier (WS4): a search that ended with NO match cleared its
	// cursor, so the seed's apply seeks from the viewport origin instead — and
	// entries that streamed in since may now match. Reopening therefore CAN move
	// the view, which is arguably what the user wants to see.
	m := newLogsModel(20, []string{"aaa", "bbb", "ccc"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "zzz")
	require.Equal(t, -1, m.logCursorIdx, "no match: no cursor")

	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Unix(9, 0), Process: "p", Line: "zzz now here",
	}))
	require.Equal(t, -1, m.logCursorIdx, "the committed no-match search still has no cursor")

	m = clientUpdate(m, keyRune('/'))
	assert.Equal(t, "zzz", m.textInput.Value())
	assert.Equal(t, "zzz now here", logCursorLine(t, m), "the seed's apply lands on the arrival")
}

func TestLiveSearch_SeedApplyDoesNotDestroyTheOrigin(t *testing.T) {
	// The pinned order (capture → seed → apply). The seed's own seek MOVES the
	// view here, so an origin captured after it would be the seed's landing and
	// Esc would "restore" to the very position the reopen introduced. The origin
	// must be the pre-seed position — where `/` was actually pressed.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k')) // disengage follow
	m.viewport.SetYOffset(2)
	m = commitSearch(m, "zzz")
	require.Equal(t, -1, m.logCursorIdx, "no match: no cursor to seek from later")
	require.Equal(t, 2, m.viewport.YOffset)

	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Unix(99, 0), Process: "p", Line: "zzz arrived",
	}))
	require.Equal(t, 2, m.viewport.YOffset, "the arrival did not move a parked search")

	m = clientUpdate(m, keyRune('/'))
	require.Equal(t, "zzz arrived", logCursorLine(t, m), "the seed's apply jumped to the arrival")
	require.NotEqual(t, 2, m.viewport.YOffset, "...and scrolled the view to it")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, 2, m.viewport.YOffset, "esc restores the PRE-seed position")
	assert.Empty(t, m.logSearchQuery)
	assert.Equal(t, -1, m.logCursorIdx)
}

func TestLiveSearch_SeedThenEscClearsEverything(t *testing.T) {
	// Composes with C2: Esc cancels the search the bar was SEEDED with, exactly
	// as it cancels one typed from scratch, and restores the position as of this
	// `/` press — not the position from before the original search.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	lines[20] = "abc target"
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k'))
	m.viewport.SetYOffset(2)
	m = commitSearch(m, "abc")
	require.Equal(t, 20, m.logCursorIdx)
	pressOffset := m.viewport.YOffset
	require.NotEqual(t, 2, pressOffset)

	m = clientUpdate(m, keyRune('/'))
	require.Equal(t, "abc", m.textInput.Value())
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})

	assert.Empty(t, m.logSearchQuery, "the seeded query does not survive the cancel")
	assert.Equal(t, -1, m.logCursorIdx)
	assert.Equal(t, int64(0), m.logCursorSeq)
	assert.Equal(t, pressOffset, m.viewport.YOffset, "restored to where THIS `/` was pressed")
	assert.NotContains(t, m.viewport.View(), "❯")
}

func TestLiveSearch_SeedThenEnterRoundTrips(t *testing.T) {
	// Reopen-and-commit with no edit is a no-op: same query, same landing, same
	// scroll. Enter's re-apply runs over the seeded value, and restore-then-seek
	// from an origin that already sits on the match returns it unchanged.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %02d", i)
	}
	lines[20] = "abc target"
	m := newLogsModel(5, lines)
	m = clientUpdate(m, keyRune('k'))
	m.viewport.SetYOffset(2)
	m = commitSearch(m, "abc")
	query, seq, yOffset, follow := m.logSearchQuery, m.logCursorSeq, m.viewport.YOffset, m.followMode

	m = clientUpdate(m, keyRune('/'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, query, m.logSearchQuery)
	assert.Equal(t, seq, m.logCursorSeq)
	assert.Equal(t, 20, m.logCursorIdx)
	assert.Equal(t, yOffset, m.viewport.YOffset)
	assert.Equal(t, follow, m.followMode)
	assert.False(t, m.searchSession.active)
}

func TestLiveSearch_SeedsPerView(t *testing.T) {
	// Each view seeds from its OWN committed term, like the `s` bar seeds from
	// the active view's RawQuery.
	m := newLogsModel(20, []string{"alpha one", "beta two", "gamma"})
	m.proxyRequests = makeTestRequests(6)
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "alpha")
	require.Equal(t, "alpha", m.logSearchQuery)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, ViewModeRequests, m.viewMode)
	m = commitSearch(m, "path/004")
	require.Equal(t, "path/004", m.requestSearchQuery)

	m = clientUpdate(m, keyRune('/'))
	assert.Equal(t, "path/004", m.textInput.Value(), "the requests bar seeds the requests term")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, ViewModeLogs, m.viewMode)
	m = clientUpdate(m, keyRune('/'))
	assert.Equal(t, "alpha", m.textInput.Value(), "the logs bar seeds the logs term")
}
