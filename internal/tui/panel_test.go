package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func TestPanel_TitlesByView(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.canDrawPanel())

	frame := m.View()
	lines := strings.Split(frame, "\n")
	top := ansi.Strip(lines[m.chromeAbove()])
	assert.Contains(t, top, "Logs")
	assert.True(t, strings.HasPrefix(top, "╭"), "rounded top-left")
	assert.True(t, strings.HasSuffix(top, "╮"), "rounded top-right in %q", top)

	m.setViewMode(ViewModeRequests)
	m.updateViewport()
	frame = m.View()
	lines = strings.Split(frame, "\n")
	top = ansi.Strip(lines[m.chromeAbove()])
	assert.Contains(t, top, "Requests")

	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-abc"
	m.updateViewport()
	frame = m.View()
	lines = strings.Split(frame, "\n")
	top = ansi.Strip(lines[m.chromeAbove()])
	assert.Contains(t, top, "Request req-abc")
}

func TestPanel_TitleTruncation(t *testing.T) {
	cases := []struct{ w, h int }{
		{20, 12},
		{12, 10},
		{8, 10},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
			require.True(t, m.canDrawPanel(), "w=%d h=%d should draw panel", tc.w, tc.h)
			assertFrameContract(t, m)
			top := ansi.Strip(strings.Split(m.View(), "\n")[m.chromeAbove()])
			assert.Equal(t, tc.w, ansi.StringWidth(top))
			assert.True(t, strings.HasPrefix(top, "╭"))
			assert.True(t, strings.HasSuffix(top, "╮"))
		})
	}

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 24, Height: 12})
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = strings.Repeat("x", 40)
	m.updateViewport()
	assertFrameContract(t, m)
	top := ansi.Strip(strings.Split(m.View(), "\n")[m.chromeAbove()])
	assert.Equal(t, 24, ansi.StringWidth(top))
	assert.Contains(t, top, "…")
}

func TestPanel_DegenerateBorderless(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 3, Height: 24})
	assert.False(t, m.canDrawPanel())
	assert.Equal(t, 0, m.panelBorder())
	assert.Equal(t, 3, m.viewport.Width)
	assert.NotContains(t, ansi.Strip(m.View()), "╭")
	assertFrameContract(t, m)

	m = newTestModel()
	// chrome=4 → h=6 → contentRect.H=2 < panelMinHeight.
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 6})
	require.Equal(t, 2, m.contentRect().H)
	assert.False(t, m.canDrawPanel())
	assert.Equal(t, 0, m.panelBorder())
	assert.Equal(t, 80, m.viewport.Width)
	assert.Equal(t, 2, m.viewport.Height)
	assert.NotContains(t, ansi.Strip(m.View()), "╭")
	assertFrameContract(t, m)

	m = newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 7}) // contentRect.H=3
	require.Equal(t, 3, m.contentRect().H)
	assert.True(t, m.canDrawPanel())
	assert.Equal(t, 2, m.panelBorder())
	assert.Equal(t, 1, m.viewport.Height)
	assert.Contains(t, ansi.Strip(m.View()), "╭")
	assertFrameContract(t, m)
}

func TestPanel_WidthChain(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.canDrawPanel())
	assert.Equal(t, 2, m.panelBorder())
	assert.Equal(t, 80-2, m.viewport.Width,
		"viewport.Width must subtract panelBorder (WS-C chain)")
	assert.Equal(t, m.contentRect().H-2, m.viewport.Height)

	entry := domain.LogEntry{Process: "api", Line: "hello"}
	m.settings.Timestamps = true
	w := m.logContentWidth(entry)
	assert.Equal(t, 78-8-1-10-1, w)

	m.logSearchQuery = "hello"
	wSearch := m.logContentWidth(entry)
	assert.Equal(t, 78-2-8-1-10-1, wSearch,
		"searchMarker (2) subtracted when log search active (C14)")
	m.logSearchQuery = ""

	m.settings.Wrap = true
	m.logEntries = []domain.LogEntry{{
		Process:    "api",
		Line:       strings.Repeat("word ", 40),
		DisplaySeq: 1,
	}}
	m.logSeq = 1
	m.updateViewport()
	sp := m.logRowSpans[1]
	assert.Greater(t, sp.Last-sp.First, 0, "wrap uses panel-inset viewport.Width")
	for _, row := range strings.Split(m.viewport.View(), "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(ansi.Strip(row)), m.viewport.Width)
	}
}

func TestPanel_MouseIgnoresBorderRows(t *testing.T) {
	lines := []string{"alpha", "beta target", "gamma"}
	m := newLogsModel(8, lines)
	require.True(t, m.canDrawPanel())
	require.True(t, m.followMode)
	r := m.contentRect()

	m2 := clientUpdate(m, clickAt(5, r.Y))
	assert.Equal(t, int64(0), m2.logCursorSeq)
	assert.True(t, m2.followMode, "border click must not disengage follow")

	// Same pattern as TestMouse_ClickLogLineParksCursorAndDisengagesFollow.
	local := 1
	_, oy := m.viewportOrigin()
	m = clientUpdate(m, clickAt(5, oy+local))
	entries := m.filteredEntries()
	idx := m.entryIndexContainingRow(entries, m.viewport.YOffset+local)
	require.Equal(t, entries[idx].DisplaySeq, m.logCursorSeq)
	assert.False(t, m.followMode)
}

func TestRenderBorderTitleMid_Spacing(t *testing.T) {
	br := lipgloss.RoundedBorder()
	mid := renderBorderTitleMid("Logs", 12, styles.Panel, styles.PanelTitle)
	plain := ansi.Strip(mid)
	assert.Equal(t, 12, ansi.StringWidth(plain))
	assert.True(t, strings.HasPrefix(plain, br.Top+" Logs "+br.Top),
		"want `─ Logs ─` prefix, got %q", plain)

	helpMid := renderHelpBorderTitleMid("Help — Logs", 20)
	helpPlain := ansi.Strip(helpMid)
	assert.True(t, strings.HasPrefix(helpPlain, br.Top+" Help — Logs "+br.Top),
		"help border title: %q", helpPlain)

	// Too narrow: no label splice.
	narrow := ansi.Strip(renderBorderTitleMid("Logs", 4, styles.Panel, styles.PanelTitle))
	assert.Equal(t, strings.Repeat(br.Top, 4), narrow)
}

func TestPanel_OverlayStillComposites(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.canDrawPanel())
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	assertFrameContract(t, m)
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	require.NotEmpty(t, hits.dropdown.Rows)
	assert.GreaterOrEqual(t, hits.dropdown.Rows[0].Rect.Y, 1)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)
	assertFrameContract(t, m)
}
