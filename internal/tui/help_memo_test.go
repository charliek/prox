package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelpMemo_NoRewrapOnViewAndMouseMotion(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)

	callsAfterOpen := m.helpMemo.wrapCalls
	require.Greater(t, callsAfterOpen, 0, "enterHelp must populate memo")

	for i := 0; i < 5; i++ {
		_ = m.View()
	}
	box := m.helpModalGeometry()
	m = clientUpdate(m, tea.MouseMsg{
		X: box.X + 1, Y: box.Y + 1,
		Action: tea.MouseActionMotion,
	})
	assert.Equal(t, callsAfterOpen, m.helpMemo.wrapCalls,
		"repeated View/mouse-motion must not re-wrap")
}

func TestHelpMemo_InvalidatesOnResizeThemeAndViewChange(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))
	before := m.helpMemo.wrapCalls

	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 22})
	assert.Greater(t, m.helpMemo.wrapCalls, before, "resize must re-wrap")
	before = m.helpMemo.wrapCalls

	_ = m.setThemeByName(nextThemeName(CurrentThemeName()))
	assert.Greater(t, m.helpMemo.wrapCalls, before, "theme change must re-wrap")
	before = m.helpMemo.wrapCalls

	m.setViewMode(ViewModeRequests)
	assert.Greater(t, m.helpMemo.wrapCalls, before, "view change must re-wrap")
}

func TestHelpWrap_PreservesANSIAtNarrowWidth(t *testing.T) {
	sections := []helpSection{{
		title: "View & chrome",
		rows: []helpKeyRow{
			{key: "t", desc: "Cycle theme"},
			{key: "scroll wheel", desc: "Scroll (3 lines per notch; up pauses follow)"},
		},
	}}
	wrapped := wrapHelpBody(sections, 12)
	require.NotEmpty(t, wrapped)
	joined := strings.Join(wrapped, "\n")
	assert.Contains(t, joined, "Cycle theme")
	for _, line := range wrapped {
		assert.LessOrEqual(t, ansi.StringWidth(line), 12, "wrapped line: %q", line)
	}
}

// Description continuations hang-indent to the desc column (plan 024 F5).
func TestHelpWrap_DescHangIndent(t *testing.T) {
	sections := []helpSection{{
		title: "Navigation",
		rows: []helpKeyRow{
			{key: "scroll wheel", desc: "Scroll (3 lines per notch; up pauses follow)"},
		},
	}}
	keyW := helpKeyColumnWidth(sections)
	descStart := keyW + len(helpKeyDescGap)
	// Width large enough for hang indent but small enough to force a wrap.
	width := descStart + 20
	require.GreaterOrEqual(t, width-descStart, minHangContentWidth)

	wrapped := wrapHelpBody(sections, width)
	require.GreaterOrEqual(t, len(wrapped), 3, "title + ≥2 desc rows: %v", wrapped)

	// Skip section title; find the key row and its continuation.
	var keyRow, contRow string
	for i, line := range wrapped {
		plain := ansi.Strip(line)
		if strings.Contains(plain, "scroll wheel") {
			keyRow = plain
			require.Less(t, i+1, len(wrapped), "expected continuation after key row")
			contRow = ansi.Strip(wrapped[i+1])
			break
		}
	}
	require.NotEmpty(t, keyRow, "key row not found in %v", wrapped)
	require.NotEmpty(t, contRow)

	assert.True(t, strings.HasPrefix(keyRow, "scroll wheel") || strings.Contains(keyRow, "scroll wheel"))
	gutter := ansi.Cut(contRow, 0, descStart)
	assert.Equal(t, strings.Repeat(" ", descStart), gutter,
		"desc continuation must start at desc column (got %q)", contRow)
	assert.NotContains(t, gutter, "scroll")
	rest := strings.TrimSpace(ansi.Cut(contRow, descStart, ansi.StringWidth(contRow)))
	assert.NotEmpty(t, rest, "desc text after hang indent")
}

func TestDetailHelp_ChromeKeysWork(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "req-1"

	panelBefore := m.settings.ProcessPanel
	m.handleNavigationKey(keyRune('p'))
	assert.Equal(t, !panelBefore, m.settings.ProcessPanel)

	menuBefore := m.settings.MenuBar
	m.handleNavigationKey(keyRune('m'))
	assert.Equal(t, !menuBefore, m.settings.MenuBar)

	tsBefore := m.settings.Timestamps
	m.handleNavigationKey(keyRune('T'))
	assert.Equal(t, !tsBefore, m.settings.Timestamps)

	wrapBefore := m.settings.Wrap
	m.handleNavigationKey(keyRune('w'))
	assert.Equal(t, !wrapBefore, m.settings.Wrap)

	themeBefore := CurrentThemeName()
	m.handleNavigationKey(keyRune('t'))
	assert.NotEqual(t, themeBefore, CurrentThemeName())
}

func TestHelpBorderTitle_SplicedWhenBordered(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 26})
	m = clientUpdate(m, keyRune('?'))
	rows, _, _ := m.helpModalBoxRows()
	require.NotEmpty(t, rows)
	assert.Contains(t, ansi.Strip(rows[0]), "Help — Logs")
}

func TestHelpBorderTitle_FallbackWhenBorderless(t *testing.T) {
	m := newTestModel()
	assert.Equal(t, 2, helpModalFixedRows(2),
		"borderless rung reserves title + footer rows")
	assert.Contains(t, m.helpTitleLine(), "[Logs View]")
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 5, Height: 40})
	m = clientUpdate(m, keyRune('?'))
	rows, _, _ := m.helpModalBoxRows()
	require.NotEmpty(t, rows)
	joined := ansi.Strip(strings.Join(rows, "\n"))
	assert.NotContains(t, joined, "Help — Logs")
}
