package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

func TestCenteredHint_Geometry(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	lines := centeredHint("hi", styles.Dim, 10, 5)
	require.Len(t, lines, 5)
	for i, line := range lines {
		assert.Equal(t, 10, ansi.StringWidth(line), "row %d", i)
	}
	plain := ansi.Strip(lines[2]) // mid = h/2 = 2
	assert.Contains(t, plain, "hi")
	assert.True(t, strings.HasPrefix(plain, "    "), "horizontally centered: got %q", plain)
	assert.Equal(t, "", strings.TrimSpace(ansi.Strip(lines[0])))
	assert.Equal(t, "", strings.TrimSpace(ansi.Strip(lines[4])))

	// Truncation with ellipsis when text exceeds width.
	trunc := centeredHint("abcdefghijklmnop", styles.Dim, 5, 1)
	require.Len(t, trunc, 1)
	assert.Equal(t, 5, ansi.StringWidth(trunc[0]))
	assert.Contains(t, ansi.Strip(trunc[0]), "…")

	// h > content: still h rows, single text row.
	tall := centeredHint("x", styles.Dim, 3, 7)
	require.Len(t, tall, 7)
	nonBlank := 0
	for _, line := range tall {
		if strings.TrimSpace(ansi.Strip(line)) != "" {
			nonBlank++
		}
	}
	assert.Equal(t, 1, nonBlank)
}

func TestEmptyStates_FourCases(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	t.Run("logs empty", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m.updateViewport()
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No log output yet")
	})

	t.Run("logs filtered empty LastGood", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "web", Line: "hello"}))
		m.setLogsFilterQuery("level:error")
		m.updateViewport()
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No lines match level:error")
		assert.NotContains(t, plain, "No log output yet")
	})

	t.Run("logs invalid raw keeps LastGood in hint", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "web", Line: "hello"}))
		m.setLogsFilterQuery("zzzzznomatch")
		require.False(t, m.logsFilter.LastGood.IsEmpty())
		require.Empty(t, m.filteredEntries())
		// Mid-typing invalid field: RawQuery junk, LastGood retained.
		m.applyActiveFilterQuery("zzzzznomatch level:chatty")
		require.Error(t, m.logsFilter.ParseErr)
		m.updateViewport()
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No lines match zzzzznomatch")
		assert.NotContains(t, plain, "level:chatty")
	})

	t.Run("requests empty", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m.setViewMode(ViewModeRequests)
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No requests yet — traffic through the proxy appears here")
	})

	t.Run("requests filtered empty LastGood", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
			ID: "r1", Method: "GET", URL: "/ok", StatusCode: 200,
		}))
		m.setViewMode(ViewModeRequests)
		m.setRequestsFilterQuery("status:500")
		m.updateViewport()
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No lines match status:500")
		assert.NotContains(t, plain, "No requests yet")
	})
}

// TestRequestsEmptyHint_ExplainsWhy pins the four answers the empty requests
// pane can give (#92). The pre-C5 pane gave the last one unconditionally, so a
// project with no proxy: block was told to wait for traffic that could never
// arrive.
func TestRequestsEmptyHint_ExplainsWhy(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	// emptyRequestsHint renders the requests pane of a model built with the
	// given proxy facts and returns its plain text.
	emptyRequestsHint := func(t *testing.T, proxyConfigured, captureEnabled bool) string {
		t.Helper()
		opts := attachClientOptions()
		opts.ProxyConfigured = proxyConfigured
		opts.CaptureEnabled = captureEnabled
		m := NewClientModel(&stubTUIClient{}, opts)
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 24})
		m.setViewMode(ViewModeRequests)
		return ansi.Strip(m.viewport.View())
	}

	t.Run("no proxy configured", func(t *testing.T) {
		plain := emptyRequestsHint(t, false, false)
		assert.Contains(t, plain, "No proxy running")
		assert.Contains(t, plain, "prox.yaml")
		assert.NotContains(t, plain, "No requests yet")
		assert.NotContains(t, plain, "capture.enabled")
	})

	// The --no-proxy case: the file's proxy: block is enabled and capture with
	// it, but the RUNTIME proxy is off, so the caller passes false/false and the
	// pane must blame the proxy, not the capture setting.
	t.Run("proxy disabled at runtime outranks capture", func(t *testing.T) {
		plain := emptyRequestsHint(t, false, true)
		assert.Contains(t, plain, "No proxy running")
		assert.NotContains(t, plain, "capture is off")
	})

	// Capture being off does NOT stop rows arriving — both proxy paths still
	// Upsert a metadata-only record (internal/proxy/proxy.go's brw branch). So
	// this hint must still promise traffic, and only qualify what will land in
	// it; claiming capture is why the list is empty would be false.
	t.Run("capture disabled qualifies the promise instead of withdrawing it", func(t *testing.T) {
		plain := emptyRequestsHint(t, true, false)
		assert.Contains(t, plain, "No requests yet — capture is off, so rows will show metadata only")
		assert.NotContains(t, plain, "traffic through the proxy appears here",
			"the unqualified promise is for the capture-on case only")
		assert.NotContains(t, plain, "No proxy running")
	})

	t.Run("proxy and capture on", func(t *testing.T) {
		plain := emptyRequestsHint(t, true, true)
		assert.Contains(t, plain, "No requests yet — traffic through the proxy appears here")
	})

	// An active filter is the user's own doing and outranks every hint above:
	// the list is empty because they narrowed it, not because of the project's
	// proxy configuration.
	t.Run("active filter outranks both hints", func(t *testing.T) {
		opts := attachClientOptions()
		opts.ProxyConfigured = false
		opts.CaptureEnabled = false
		m := NewClientModel(&stubTUIClient{}, opts)
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 24})
		m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
			ID: "r1", Method: "GET", URL: "/ok", StatusCode: 200,
		}))
		m.setViewMode(ViewModeRequests)
		m.setRequestsFilterQuery("status:500")
		m.updateViewport()
		plain := ansi.Strip(m.viewport.View())
		assert.Contains(t, plain, "No lines match status:500")
		assert.NotContains(t, plain, "No proxy running")
		assert.NotContains(t, plain, "capture is off")
	})
}

// TestRequestDetail_CaptureDisabledNote pins the detail view's explanation for a
// completed request with no headers and no bodies. formatRequestDetail renders
// nothing at all for absent sections, so without the note the view is silently
// bare — and the in-flight note must keep its own, unrelated explanation.
func TestRequestDetail_CaptureDisabledNote(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	const note = "Capture is disabled (proxy.capture.enabled: false) — no headers or bodies were recorded"

	detail := func(captureEnabled, inFlight bool) string {
		b := newTestBaseModel()
		b.captureEnabled = captureEnabled
		b.proxyConfigured = true
		b.requestDetail = &RequestDetailData{
			ID: "req-1", Timestamp: "t", Method: "GET", URL: "/x",
			StatusCode: 200, InFlight: inFlight,
		}
		return ansi.Strip(strings.Join(b.formatRequestDetail(), "\n"))
	}

	t.Run("capture disabled", func(t *testing.T) {
		out := detail(false, false)
		assert.Contains(t, out, note)
		assert.NotContains(t, out, "request in flight")
	})

	t.Run("capture enabled", func(t *testing.T) {
		out := detail(true, false)
		assert.NotContains(t, out, note)
	})

	// Both notes explain "no details here", so exactly one must fire. In flight
	// wins: capture may well be on and the details simply not recorded yet.
	t.Run("in flight wins over capture note", func(t *testing.T) {
		out := detail(false, true)
		assert.Contains(t, out, "(request in flight — details arrive on completion)")
		assert.NotContains(t, out, note)
	})
}

func TestDetailTitle_Style(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID: "req-1", Timestamp: "t", Method: "GET", URL: "/x", StatusCode: 200,
	}
	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, styles.DetailTitle.Render("Request: req-1"))
	assert.NotContains(t, out, styles.Header.Render("Request: req-1"))
}

func TestRequestsHeader_FixedUnderScroll(t *testing.T) {
	m := newRequestsModel(20, 4)
	require.Equal(t, 1, m.requestsHeaderRows())
	hy, ok := m.requestsHeaderFrameY()
	require.True(t, ok)

	frame := m.View()
	lines := strings.Split(frame, "\n")
	headerPlain := ansi.Strip(lines[hy])
	assert.Contains(t, headerPlain, "Time")
	assert.Contains(t, headerPlain, "Host")
	assert.Contains(t, headerPlain, "Method")
	assert.Contains(t, headerPlain, "URL")

	// Park at top then move cursor deep enough to scroll — header stays fixed.
	m = clientUpdate(m, keyRune('g'))
	for i := 0; i < 10; i++ {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Greater(t, m.viewport.YOffset, 0)
	frame2 := m.View()
	lines2 := strings.Split(frame2, "\n")
	assert.Equal(t, lines[hy], lines2[hy], "header row must stay fixed under scroll")
}

func TestRequestsHeader_ClickConsumedNoOp(t *testing.T) {
	m := newRequestsModel(6, 6)
	require.Equal(t, 5, m.cursorIdx)
	hy, ok := m.requestsHeaderFrameY()
	require.True(t, ok)

	before := m.cursorIdx
	m = clientUpdate(m, clickAt(10, hy))
	assert.Equal(t, before, m.cursorIdx, "header click must not select request row 0")
	assert.Equal(t, ViewModeRequests, m.viewMode)

	// Contrast: click first viewport row does move cursor.
	_, oy := m.viewportOrigin()
	m = clientUpdate(m, clickAt(10, oy))
	assert.Equal(t, m.viewport.YOffset, m.cursorIdx)
}

func TestRequestsHeader_ViewSwitchRelayout(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	logsH := m.viewport.Height
	require.Equal(t, 0, m.requestsHeaderRows())

	m.setViewMode(ViewModeRequests)
	assert.Equal(t, 1, m.requestsHeaderRows())
	assert.Equal(t, logsH-1, m.viewport.Height, "requests header consumes one viewport row")

	m.setViewMode(ViewModeLogs)
	assert.Equal(t, 0, m.requestsHeaderRows())
	assert.Equal(t, logsH, m.viewport.Height)

	assertFrameContract(t, m)
	m.setViewMode(ViewModeRequests)
	assertFrameContract(t, m)
}

func TestRequestsHeader_RectHonesty(t *testing.T) {
	// Re-run rect-honesty corpus with requests header present (plan 023 C11).
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.setViewMode(ViewModeRequests)
	require.Equal(t, 1, m.requestsHeaderRows())
	assertFrameContract(t, m)

	r := m.contentRect()
	ox, oy := m.viewportOrigin()
	assert.Equal(t, r.X+1, ox)
	assert.Equal(t, r.Y+1+1, oy, "panel inset + requests header")
	assert.Equal(t, r.H-2-1, m.viewport.Height)

	hy, ok := m.requestsHeaderFrameY()
	require.True(t, ok)
	assert.Equal(t, r.Y+1, hy)
	assert.Equal(t, oy-1, hy)

	// Menu rect-honesty still holds after view switch.
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	_ = m.View() // record dropdown hit rects
	assertDropdownRectGeometry(t, m)
	assertFrameContract(t, m)
}

// TestFrameContract_EnterDetailViaBeginRequestDetail pins the C11 fold: the
// real detail-entry path (Enter / double-click → beginRequestDetail) relayouts
// after dropping the requests header row, so the frame stays exact.
func TestFrameContract_EnterDetailViaBeginRequestDetail(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-001", "/x")))
	m.setViewMode(ViewModeRequests)
	require.Equal(t, 1, m.requestsHeaderRows())

	m.beginRequestDetail("req-001")
	require.Equal(t, ViewModeRequestDetail, m.viewMode)
	require.Equal(t, 0, m.requestsHeaderRows())
	assertFrameContract(t, m)

	// And back: Esc returns to the requests list (header row restored).
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, ViewModeRequests, m.viewMode)
	require.Equal(t, 1, m.requestsHeaderRows())
	assertFrameContract(t, m)
}
