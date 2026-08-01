package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func wheelDown() tea.MouseMsg {
	return tea.MouseMsg{
		X: 0, Y: 5,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	}
}

func wheelUp() tea.MouseMsg {
	return tea.MouseMsg{
		X: 0, Y: 5,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
	}
}

func clickAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X: x, Y: y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	}
}

func TestMouseWheelEnabledFalseOnNewModel(t *testing.T) {
	m := newTestModel()
	assert.False(t, m.viewport.MouseWheelEnabled, "TUI owns wheel routing (Codex #5)")
}

func TestMouseWheel_LogsScrollsThreeRowsNoDoubleScroll(t *testing.T) {
	m := newLogsModel(10, mouseTestLines(30))
	m = clientUpdate(m, keyRune('g')) // top, follow off
	require.Equal(t, 0, m.viewport.YOffset)

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, wheelScrollRows, m.viewport.YOffset,
		"exactly 3 rows per notch — not 6 from bubbles viewport forwarding")
	require.False(t, m.viewport.MouseWheelEnabled)

	// Direct viewport wheel is a no-op with MouseWheelEnabled=false.
	vp := m.viewport
	vp.MouseWheelEnabled = false
	vpBefore := vp.YOffset
	vp, _ = vp.Update(wheelDown())
	assert.Equal(t, vpBefore, vp.YOffset, "bubbles viewport must not scroll wheel events")
}

func TestMouseWheel_LogsWheelUpDisengagesFollow(t *testing.T) {
	m := newLogsModel(10, mouseTestLines(20))
	require.True(t, m.followMode)
	before := m.viewport.YOffset
	require.Greater(t, before, 0, "precondition: follow parks at bottom")

	m = clientUpdate(m, wheelUp())
	assert.False(t, m.followMode)
	assert.Equal(t, before-wheelScrollRows, m.viewport.YOffset)
}

func TestMouseWheel_LogsWheelDownAtBottomReengagesFollow(t *testing.T) {
	m := newLogsModel(10, mouseTestLines(30))
	m = clientUpdate(m, wheelUp())
	require.False(t, m.followMode)

	for !m.viewport.AtBottom() {
		m = clientUpdate(m, wheelDown())
	}
	assert.True(t, m.followMode, "wheel down to bottom re-engages follow")
}

func TestMouseWheel_RequestsMovesCursorThree(t *testing.T) {
	m := newRequestsModel(10, 8)
	require.Equal(t, 9, m.cursorIdx)

	m = clientUpdate(m, wheelUp())
	assert.Equal(t, 9-wheelScrollRows, m.cursorIdx)
	assert.False(t, m.followMode)
}

func TestMouseWheel_RequestsFollowOnWheelUpDisengages(t *testing.T) {
	m := newRequestsModel(10, 8)
	require.True(t, m.followMode)
	require.Equal(t, 9, m.cursorIdx)

	m = clientUpdate(m, wheelUp())
	assert.False(t, m.followMode)
	assert.Equal(t, 6, m.cursorIdx)
}

func TestMouseWheel_RequestsWheelDownOntoNewestReengagesFollow(t *testing.T) {
	m := newRequestsModel(10, 8)
	m = clientUpdate(m, keyRune('g'))
	require.False(t, m.followMode)
	require.Equal(t, 0, m.cursorIdx)

	m = clientUpdate(m, wheelDown())
	m = clientUpdate(m, wheelDown())
	m = clientUpdate(m, wheelDown())
	assert.Equal(t, 9, m.cursorIdx)
	assert.True(t, m.followMode, "wheel onto newest row re-engages follow like j")
}

func TestMouseWheel_RequestsViewportNotIndependentlyScrolled(t *testing.T) {
	m := newRequestsModel(20, 5)
	m = clientUpdate(m, keyRune('g'))
	require.Equal(t, 0, m.cursorIdx)
	yoBefore := m.viewport.YOffset

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, wheelScrollRows, m.cursorIdx)
	assert.Equal(t, yoBefore, m.viewport.YOffset,
		"wheel moves cursor only — bubbles viewport wheel is disabled")
}

func TestMouseWheel_RequestsOldestVisibleTriggersPaging(t *testing.T) {
	stub := &stubTUIClient{snapshot: olderPage(2), nextBeforeID: "cur-2"}
	m := primedPagingModel(stub, 10, "cur-1")
	require.Equal(t, pagingReady, m.pagingPhase)

	var pagingCmd tea.Cmd
	for i := 0; i < 20; i++ {
		var cmd tea.Cmd
		m, cmd = clientUpdateModel(m, wheelUp())
		if cmd != nil {
			pagingCmd = cmd
			break
		}
	}
	require.NotNil(t, pagingCmd, "wheel onto oldest visible row should arm scroll-back fetch")
	assert.Equal(t, pagingLoading, m.pagingPhase)
	_ = pageMsgFrom(t, pagingCmd)
}

func TestMouseWheel_DetailScrollsViewport(t *testing.T) {
	m := newRequestsModel(5, 4)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "detail body line"
	}
	m.requestDetail = &RequestDetailData{
		ID: "req-004", Method: "GET", URL: "/long",
		RequestBody: &BodyData{Data: strings.Join(lines, "\n"), ContentType: "text/plain"},
	}
	m.detailLoading = false
	m.updateViewport()
	m.viewport.SetYOffset(0)
	require.Greater(t, m.viewport.TotalLineCount(), m.viewport.Height)

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, wheelScrollRows, m.viewport.YOffset)
}

func TestMouse_ClickProcessPanelSoloToggle(t *testing.T) {
	m := newTestModel()
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
		{Name: "api", State: domain.ProcessStateRunning},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 24})
	_ = m.mainView("")
	require.Len(t, m.ensureHits().chips, 2)

	rowY, ok := m.processPanelRowY()
	require.True(t, ok)
	m = clientUpdate(m, clickAt(2, rowY))
	assert.Equal(t, "web", m.soloProcess)

	m = clientUpdate(m, clickAt(2, rowY))
	assert.Empty(t, m.soloProcess, "second click unsolos like 1-9 toggle")
}

func TestMouse_ProcessChipHitsRefreshPerFrame(t *testing.T) {
	m := newTestModel()
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.mainView("")
	require.Len(t, m.ensureHits().chips, 1)

	m.processes = append(m.processes, domain.ProcessInfo{Name: "api", State: domain.ProcessStateRunning})
	_ = m.mainView("")
	require.Len(t, m.ensureHits().chips, 2, "chip rects are re-recorded each frame")
}

func TestMouse_ClickLogLineParksCursorAndDisengagesFollow(t *testing.T) {
	lines := []string{"alpha", "beta target", "gamma"}
	m := newLogsModel(8, lines)
	require.True(t, m.followMode)

	local := 1
	contentY := m.chromeAbove() + local
	m = clientUpdate(m, clickAt(5, contentY))

	entries := m.filteredEntries()
	idx := m.entryIndexContainingRow(entries, m.viewport.YOffset+local)
	require.Equal(t, entries[idx].DisplaySeq, m.logCursorSeq)
	assert.Equal(t, idx, m.logCursorIdx)
	assert.False(t, m.followMode)
}

func TestMouse_ClickRequestRowMovesCursor(t *testing.T) {
	m := newRequestsModel(6, 6)
	require.Equal(t, 5, m.cursorIdx)

	local := 0
	y := m.chromeAbove() + local
	m = clientUpdate(m, clickAt(10, y))
	assert.Equal(t, m.viewport.YOffset, m.cursorIdx)
}

func TestMouse_DoubleClickOpensDetail(t *testing.T) {
	t0 := time.Unix(1000, 0)
	nowFunc = func() time.Time { return t0 }
	defer func() { nowFunc = time.Now }()

	m := newRequestsModel(4, 6)
	row := 2
	y := m.chromeAbove() + row

	m = clientUpdate(m, clickAt(10, y))
	require.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, row, m.cursorIdx)

	nowFunc = func() time.Time { return t0.Add(200 * time.Millisecond) }
	newModel, cmd := m.Update(clickAt(10, y))
	m = newModel.(ClientModel)
	require.NotNil(t, cmd)
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
}

func TestMouse_DoubleClickSlowDoesNotOpenDetail(t *testing.T) {
	t0 := time.Unix(2000, 0)
	nowFunc = func() time.Time { return t0 }
	defer func() { nowFunc = time.Now }()

	m := newRequestsModel(4, 6)
	row := 1
	y := m.chromeAbove() + row

	m = clientUpdate(m, clickAt(10, y))
	nowFunc = func() time.Time { return t0.Add(600 * time.Millisecond) }
	m = clientUpdate(m, clickAt(10, y))
	assert.Equal(t, ViewModeRequests, m.viewMode)
}

func TestMouse_DoubleClickDifferentRowDoesNotOpenDetail(t *testing.T) {
	t0 := time.Unix(3000, 0)
	nowFunc = func() time.Time { return t0 }
	defer func() { nowFunc = time.Now }()

	m := newRequestsModel(4, 6)
	y0 := m.chromeAbove() + 0
	y2 := m.chromeAbove() + 2

	m = clientUpdate(m, clickAt(10, y0))
	nowFunc = func() time.Time { return t0.Add(100 * time.Millisecond) }
	m = clientUpdate(m, clickAt(10, y2))
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, 2, m.cursorIdx)
}

func TestMouse_ClickAfterMenuCloseIgnoresStaleRects(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.mainView("")
	hits := m.ensureHits()
	require.True(t, hits.hasDropdown)

	stale := hits.dropdown.Rows[0]
	m = clientUpdate(m, tea.MouseMsg{
		X: 0, Y: 10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	require.False(t, m.menuOpen())
	require.False(t, m.ensureHits().hasDropdown)

	m = clientUpdate(m, tea.MouseMsg{
		X: stale.Rect.X, Y: stale.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.Equal(t, ViewModeLogs, m.viewMode)
}

func TestMouse_IgnoredInTextInputModes(t *testing.T) {
	m := newLogsModel(8, mouseTestLines(10))
	m = clientUpdate(m, keyRune('s'))
	require.Equal(t, ModeStringFilter, m.mode)
	before := m.viewport.YOffset

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, before, m.viewport.YOffset)
	assert.Equal(t, ModeStringFilter, m.mode)

	contentY := m.chromeAbove() + 1
	m = clientUpdate(m, clickAt(5, contentY))
	assert.Equal(t, ModeStringFilter, m.mode)
	assert.Equal(t, before, m.viewport.YOffset)
}

func TestHelpView_NoDeadBindings(t *testing.T) {
	m := newTestModel()
	// Large frame so renderHelp shows every section without windowing.
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 80})
	logs := m.logsHelpView()
	assert.Contains(t, logs, "Copy")
	assert.Contains(t, logs, "  y            Copy")
	assert.Contains(t, logs, "  m            Toggle menu bar")
	assert.Contains(t, logs, "  t            Cycle theme")
	assert.NotContains(t, logs, "ModeFilter")
	assert.NotContains(t, logs, "Filter mode")
	assert.NotContains(t, logs, "Select all")

	reqs := m.requestsHelpView()
	assert.Contains(t, reqs, "method:GET")
	assert.Contains(t, reqs, "double-click")
	assert.Contains(t, reqs, "  c            Copy as curl")
	assert.NotContains(t, reqs, "ModeFilter")

	m.viewMode = ViewModeRequestDetail
	detail := m.detailHelpView()
	assert.Contains(t, detail, "Copy wire JSON")
	assert.Contains(t, detail, "scroll wheel")
}

func TestRenderKeyHints_FitsFrame(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 60, Height: 24})
	hint := m.renderKeyHints()
	assert.Contains(t, hint, "m menu")
	assert.Contains(t, hint, "? help")
	assert.LessOrEqual(t, lipgloss.Width(hint), 60)
}

func TestMouse_ProcessPanelNoOpInRequestsView(t *testing.T) {
	m := newTestModel()
	m.viewMode = ViewModeRequests
	m.processes = []domain.ProcessInfo{{Name: "web", State: domain.ProcessStateRunning}}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	rowY, _ := m.processPanelRowY()
	m = clientUpdate(m, clickAt(2, rowY))
	assert.Empty(t, m.soloProcess)
}

func mouseTestLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line"
	}
	return out
}
