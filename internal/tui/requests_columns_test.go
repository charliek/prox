package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/proxy"
)

func TestLoadSettings_MissingRequestsTable_DefaultsAllOn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `theme = "dark"

[view]
wrap = true
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, defaultRequestsColumns(), s.RequestsColumns)
}

func TestLoadSettings_RequestsPartialTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `[requests]
method = false
id = false
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.True(t, s.RequestsColumns.Time)
	assert.True(t, s.RequestsColumns.Host)
	assert.False(t, s.RequestsColumns.Method)
	assert.True(t, s.RequestsColumns.Status)
	assert.True(t, s.RequestsColumns.Duration)
	assert.False(t, s.RequestsColumns.ID)
}

func TestSaveSettingsChanged_RequestsColumns_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, SaveSettings(DefaultSettings()))

	s := DefaultSettings()
	s.RequestsColumns.Method = false
	s.RequestsColumns.ID = false
	require.NoError(t, SaveSettingsChanged(s, settingRequestsColumns))

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.False(t, loaded.RequestsColumns.Method)
	assert.False(t, loaded.RequestsColumns.ID)
	assert.True(t, loaded.RequestsColumns.Time)
	assert.True(t, loaded.RequestsColumns.Host)
}

func TestSaveSettingsChanged_RequestsColumns_StaleViewMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, SaveSettings(DefaultSettings()))

	stale1, warnings := LoadSettings()
	require.Empty(t, warnings)
	stale2 := stale1

	stale1.RequestsColumns.Method = false
	require.NoError(t, SaveSettingsChanged(stale1, settingRequestsColumns))

	// stale2 still believes Method=true; saving only theme must not clobber [requests].
	stale2.Theme = "dark"
	require.NoError(t, SaveSettingsChanged(stale2, settingTheme))

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
	assert.False(t, loaded.RequestsColumns.Method, "method=false must survive theme save")
	assert.True(t, loaded.RequestsColumns.Time)

	// And a requests save must not clobber theme from a stale view that still
	// believes Theme is empty.
	stale3 := DefaultSettings()
	stale3.RequestsColumns.Host = false
	require.NoError(t, SaveSettingsChanged(stale3, settingRequestsColumns))

	loaded, warnings = LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme, "theme must survive requests-columns save")
	assert.False(t, loaded.RequestsColumns.Host)
	// Full [requests] table rewrite from stale3 defaults Method back to true.
	assert.True(t, loaded.RequestsColumns.Method)
}

func TestShortRequestID(t *testing.T) {
	assert.Equal(t, "        ", shortRequestID(""))
	assert.Equal(t, "abc     ", shortRequestID("abc"))
	assert.Equal(t, "abcdefgh", shortRequestID("abcdefgh"))
	assert.Equal(t, "abcdefgh", shortRequestID("abcdefghijkl"))
}

func TestFormatProxyRequest_IDColumn(t *testing.T) {
	m := newTestModel()
	req := proxy.RequestRecord{
		ID: "abcdef012345", Timestamp: time.Date(2026, 1, 1, 12, 34, 56, 0, time.UTC),
		Subdomain: "api", Method: "GET", URL: "/x", StatusCode: 200,
		Duration: 50 * time.Millisecond,
	}
	got := stripANSI(m.formatProxyRequest(req))
	assert.Contains(t, got, "abcdef01")
	assert.Contains(t, got, "/x")
	// ID sits between duration and URL.
	durIdx := strings.Index(got, "50ms")
	idIdx := strings.Index(got, "abcdef01")
	urlIdx := strings.Index(got, "/x")
	require.GreaterOrEqual(t, durIdx, 0)
	require.GreaterOrEqual(t, idIdx, 0)
	require.GreaterOrEqual(t, urlIdx, 0)
	assert.Less(t, durIdx, idIdx)
	assert.Less(t, idIdx, urlIdx)
}

func TestFormatProxyRequest_EmptyAndShortID(t *testing.T) {
	m := newTestModel()
	empty := proxy.RequestRecord{
		ID: "", Timestamp: time.Now(), Subdomain: "api", Method: "GET",
		URL: "/e", StatusCode: 200, Duration: time.Millisecond,
	}
	got := stripANSI(m.formatProxyRequest(empty))
	// Eight-column duration, eight-space ID column, then URL.
	assert.Contains(t, got, "     1ms"+"  "+"        "+"  /e")

	short := empty
	short.ID = "ab"
	short.URL = "/s"
	got = stripANSI(m.formatProxyRequest(short))
	assert.Contains(t, got, "ab      ")
	assert.NotContains(t, got, "ab...")
}

func TestFormatProxyRequest_AllOptionalColumnsOff(t *testing.T) {
	m := newTestModel()
	m.settings.RequestsColumns = RequestsColumns{} // all false
	req := proxy.RequestRecord{
		ID: "abcdef012345", Timestamp: time.Now(), Subdomain: "api",
		Method: "GET", URL: "/only-url", StatusCode: 200, Duration: time.Millisecond,
	}
	got := stripANSI(m.formatProxyRequest(req))
	assert.Equal(t, "/only-url", strings.TrimSpace(got))
	assert.NotContains(t, got, "GET")
	assert.NotContains(t, got, "api")
	assert.NotContains(t, got, "abcdef")

	hdr := stripANSI(m.formatRequestsHeaderRow())
	assert.Equal(t, "URL", strings.TrimSpace(hdr))
	assert.NotContains(t, hdr, "Time")
	assert.NotContains(t, hdr, "Method")
}

func TestFormatProxyRequest_WideHostTruncation(t *testing.T) {
	m := newTestModel()
	req := proxy.RequestRecord{
		ID: "id1234567890", Timestamp: time.Now(),
		Subdomain: "verylonghostname", Method: "GET", URL: "/x",
		StatusCode: 200, Duration: time.Millisecond,
	}
	got := stripANSI(m.formatProxyRequest(req))
	assert.Contains(t, got, "verylongho") // 10-char truncate
	assert.NotContains(t, got, "verylonghostname")
}

func TestFormatProxyRequest_HiddenColumnsNoSeparators(t *testing.T) {
	m := newTestModel()
	m.settings.RequestsColumns = RequestsColumns{Method: true, ID: true}
	req := proxy.RequestRecord{
		ID: "deadbeefcafe", Timestamp: time.Now(), Subdomain: "api",
		Method: "POST", URL: "/orders", StatusCode: 201, Duration: 10 * time.Millisecond,
	}
	got := stripANSI(m.formatProxyRequest(req))
	assert.Contains(t, got, "POST")
	assert.Contains(t, got, "deadbeef")
	assert.Contains(t, got, "/orders")
	assert.NotContains(t, got, "api")
	assert.NotContains(t, got, "201")
}

func TestRequestMatchesSearch_VisibleColumnsOnly(t *testing.T) {
	m := newTestModel()
	req := proxy.RequestRecord{
		ID: "abcdef012345", Timestamp: time.Date(2026, 1, 1, 15, 4, 5, 0, time.UTC),
		Subdomain: "shop", Method: "GET", URL: "/orders", StatusCode: 404,
		Duration: 42 * time.Millisecond,
	}

	assert.True(t, m.requestMatchesSearch(req, "GET"))
	assert.True(t, m.requestMatchesSearch(req, "shop"))
	assert.True(t, m.requestMatchesSearch(req, "orders"))
	assert.True(t, m.requestMatchesSearch(req, "abcdef01"))
	assert.True(t, m.requestMatchesSearch(req, "404"))

	m.settings.RequestsColumns.Method = false
	assert.False(t, m.requestMatchesSearch(req, "GET"), "hidden method must not match")
	assert.True(t, m.requestMatchesSearch(req, "orders"), "URL always matches")

	m.settings.RequestsColumns.Host = false
	assert.False(t, m.requestMatchesSearch(req, "shop"))

	m.settings.RequestsColumns.ID = false
	assert.False(t, m.requestMatchesSearch(req, "abcdef01"))

	m.settings.RequestsColumns = RequestsColumns{} // all optional off
	assert.False(t, m.requestMatchesSearch(req, "GET"))
	assert.False(t, m.requestMatchesSearch(req, "404"))
	assert.True(t, m.requestMatchesSearch(req, "orders"))
}

func TestCopyKeys_UnaffectedByColumnVisibility(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newClientRequestsModel(&stubTUIClient{}, 1, 10)
	m.settings.RequestsColumns = RequestsColumns{} // hide ID column etc.
	m.proxyRequests[0] = proxy.RequestRecord{
		ID: "full-request-id", Timestamp: time.Unix(0, 0), Method: "GET",
		URL: "/z", Hostname: "h.local.dev", StatusCode: 200,
	}
	m.opts.ProxyHTTPSPort = 443
	m.setRequestCursor(m.filteredProxyRequests(), 0)

	m = clientUpdate(m, keyRune('y'))
	assert.Equal(t, "full-request-id", clipboard, "y copies full ID even when ID column hidden")

	m = clientUpdate(m, keyRune('c'))
	assert.Equal(t, "curl -X 'GET' 'https://h.local.dev:443/z'", clipboard)
}

func TestMenu_ColumnsSection_RequestsOnly(t *testing.T) {
	m := newTestModel()
	items := m.menuItems(MenuView)
	for _, it := range items {
		assert.NotEqual(t, "Columns", it.Label)
		assert.NotEqual(t, MenuCmdToggleColTime, it.Cmd)
	}

	m.setViewMode(ViewModeRequests)
	items = m.menuItems(MenuView)
	var labels []string
	for _, it := range items {
		if it.Separator {
			labels = append(labels, "---")
			continue
		}
		labels = append(labels, it.Label)
	}
	assert.Contains(t, labels, "Columns")
	assert.Contains(t, labels, "Time")
	assert.Contains(t, labels, "Host")
	assert.Contains(t, labels, "Method")
	assert.Contains(t, labels, "Status")
	assert.Contains(t, labels, "Duration")
	assert.Contains(t, labels, "ID")

	// Detail view also shows Columns (requests family).
	m.viewMode = ViewModeRequestDetail
	items = m.menuItems(MenuView)
	found := false
	for _, it := range items {
		if it.Label == "Columns" {
			found = true
			break
		}
	}
	assert.True(t, found)
}

func TestMenu_ToggleColumn_PersistsAndRelayouts(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))
	require.NoError(t, SaveSettings(DefaultSettings()))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.setViewMode(ViewModeRequests)
	m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
		ID: "abcdef012345", Timestamp: time.Now(), Method: "GET",
		URL: "/x", Subdomain: "api", StatusCode: 200, Duration: time.Millisecond,
	}))
	require.True(t, m.settings.RequestsColumns.Method)

	cmd := m.activateMenuCommand(MenuCmdToggleColMethod)
	assert.Nil(t, cmd)
	assert.False(t, m.settings.RequestsColumns.Method)

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.False(t, loaded.RequestsColumns.Method)

	got := stripANSI(m.formatProxyRequest(m.proxyRequests[0]))
	assert.NotContains(t, got, "GET")
}

// allRequestsColumnCombos enumerates every visibility mask the Columns menu allows.
func allRequestsColumnCombos() []RequestsColumns {
	combos := make([]RequestsColumns, 0, 64)
	for mask := 0; mask < 64; mask++ {
		combos = append(combos, RequestsColumns{
			Time:     mask&1 != 0,
			Host:     mask&2 != 0,
			Method:   mask&4 != 0,
			Status:   mask&8 != 0,
			Duration: mask&16 != 0,
			ID:       mask&32 != 0,
		})
	}
	return combos
}

// requestsColumnStartOffsets returns display-column start offsets for each
// visible column in requestsColumnSpec order, skipping the 2-col row prefix
// (header indent or data cursor marker).
func requestsColumnStartOffsets(rendered string, cols RequestsColumns) []int {
	const rowPrefixW = 2
	pos := rowPrefixW
	i := skipDisplayWidth(rendered, rowPrefixW)
	var starts []int
	first := true
	for _, def := range requestsColumnSpec {
		if !cols.columnVisible(def.key) {
			continue
		}
		if !first {
			i += skipDisplayWidth(rendered[i:], def.sepBefore)
			pos += def.sepBefore
		}
		starts = append(starts, pos)
		if def.width > 0 {
			i += skipDisplayWidth(rendered[i:], def.width)
			pos += def.width
		}
		first = false
	}
	return starts
}

func TestRequestsColumnHeaderDataAlignment(t *testing.T) {
	m := newTestModel()
	synth := proxy.RequestRecord{
		ID:         "abcd1234",
		Timestamp:  time.Date(2026, 1, 1, 12, 34, 56, 0, time.UTC),
		Subdomain:  "api.example",
		Method:     "GET",
		URL:        "/path",
		StatusCode: 200,
		Duration:   50 * time.Millisecond,
	}
	for i, cols := range allRequestsColumnCombos() {
		t.Run(fmt.Sprintf("mask_%02d", i), func(t *testing.T) {
			m.settings.RequestsColumns = cols
			hdr := m.formatRequestsHeaderRow()
			row := styles.Base.Render("  ") + m.formatProxyRequest(synth)
			assert.Equal(t, requestsColumnStartOffsets(hdr, cols), requestsColumnStartOffsets(row, cols))
			if cols.Status {
				plain := stripANSI(hdr)
				assert.Contains(t, plain, "Status")
				assert.NotContains(t, plain, "Sta ")
			}
		})
	}
}

func TestFrameContract_RequestsColumnsOff(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24},
		{40, 12},
		{20, 8},
		{10, 6},
	}
	cfgs := []Settings{
		{
			MenuBar: true, ProcessPanel: true, Timestamps: true, Wrap: false,
			RequestsColumns: RequestsColumns{}, // all optional off → URL only
		},
		{
			MenuBar: true, ProcessPanel: false, Timestamps: true, Wrap: false,
			RequestsColumns: RequestsColumns{ID: true, Method: true},
		},
		DefaultSettings(),
	}
	for _, sz := range sizes {
		for _, cfg := range cfgs {
			chrome := (&BaseModel{settings: cfg}).chromeHeight()
			if sz.h < chrome+1 {
				continue
			}
			t.Run(frameSweepName(sz.w, sz.h, cfg, "req-cols"), func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeRequests)
				assertFrameContract(t, m)
			})
		}
	}
}
