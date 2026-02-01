package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

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
