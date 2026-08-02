package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// View-level hit-registry regressions (plan 022 WS0 / C0).
//
// ClientModel.View is a value receiver: pre-fix, render wrote hit-rects into
// plain fields on the copy and discarded them. These tests exercise the
// production path — View() then Update(mouse) — NOT mainView on the test model.
// Clicks reuse clickAt (mouse_test.go); releases are no-ops in the handlers.

// findPlainInView returns (col, row) of the first occurrence of needle in the
// ANSI-stripped View output. row is 0-based frame Y.
func findPlainInView(t *testing.T, view, needle string) (col, row int) {
	t.Helper()
	for y, line := range strings.Split(view, "\n") {
		plain := stripANSI(line)
		if i := strings.Index(plain, needle); i >= 0 {
			return i, y
		}
	}
	t.Fatalf("needle %q not found in View output:\n%s", needle, stripANSI(view))
	return 0, 0
}

func TestHitRegistry_MenuCellClickOpensMenu_ViewLevel(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	require.True(t, m.ready)

	view := m.View() // value-receiver production path — must persist hits
	col, row := findPlainInView(t, view, "View")
	require.Equal(t, 0, row, "menu bar is frame row 0")

	m = clientUpdate(m, clickAt(col, row))
	assert.True(t, m.menuOpen(), "menu cell click via View()-recorded hits must open")
	assert.Equal(t, int(MenuView), m.openMenu)
}

func TestHitRegistry_DropdownRowClickActivates_ViewLevel(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())

	_ = m.View() // value-receiver path must persist dropdown hits
	hits := m.mustHits()
	require.True(t, hits.hasDropdown, "View() must leave dropdown hits on the stored model")
	require.GreaterOrEqual(t, len(hits.dropdown.Rows), 2)
	reqs := hits.dropdown.Rows[1] // Requests
	require.Equal(t, MenuCmdSetRequests, reqs.Cmd)
	col, row := reqs.Rect.X+1, reqs.Rect.Y

	m = clientUpdate(m, clickAt(col, row))
	assert.False(t, m.menuOpen(), "dropdown activate closes menu")
	assert.Equal(t, ViewModeRequests, m.viewMode)
}

func TestHitRegistry_ProcessChipClick_ViewLevel(t *testing.T) {
	m := newTestModel()
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
		{Name: "api", State: domain.ProcessStateRunning},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	view := m.View()
	col, row := findPlainInView(t, view, "1:web")

	m = clientUpdate(m, clickAt(col, row))
	assert.Equal(t, "web", m.soloProcess)
}

func TestHitRegistry_StaleRectsRejected(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())

	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	reqs := hits.dropdown.Rows[1]
	col, row := reqs.Rect.X+1, reqs.Rect.Y

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	require.False(t, m.menuOpen())
	require.False(t, m.mustHits().hasDropdown)

	m = clientUpdate(m, clickAt(col, row))
	assert.Equal(t, ViewModeLogs, m.viewMode, "stale dropdown rect must not activate")
	assert.False(t, m.menuOpen())
}

// TestHitLifecycle_ProcessPanelOff_StaleChipClickParksLogCursor is T1
// (plan 023): after `p` hides the process panel, clicking where a chip was
// parks the log cursor — nothing is soloed.
func TestHitLifecycle_ProcessPanelOff_StaleChipClickParksLogCursor(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	lines := []string{"alpha line", "beta line", "gamma line", "delta line", "epsilon line"}
	m := newLogsModel(10, lines)
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
		{Name: "api", State: domain.ProcessStateRunning},
	}
	m.updateViewport()
	_ = m.View()
	chips := m.mustHits().chips
	require.Len(t, chips, 2)
	chip := chips[0]
	clickX, clickY := chip.Rect.X+1, chip.Rect.Y

	m = clientUpdate(m, keyRune('p'))
	require.False(t, m.settings.ProcessPanel)
	require.Empty(t, m.soloProcess)

	_ = m.View()
	require.Empty(t, m.mustHits().chips, "hidden panel must not re-record chips")

	// After ProcessPanel hide, chromeAbove shrinks: the old chip Y is now the
	// viewport panel's top border. Click the first content row beneath it so
	// the T1 intent (stale coords must not solo; content click parks cursor)
	// still holds under the C7 panel inset.
	contentY := clickY
	if m.canDrawPanel() {
		contentY = clickY + 1
	}
	m = clientUpdate(m, clickAt(clickX, contentY))
	assert.Empty(t, m.soloProcess, "stale chip location must not solo a process")
	assert.False(t, m.followMode, "click parks the log cursor and disengages follow")
	entries := m.filteredEntries()
	require.GreaterOrEqual(t, m.logCursorIdx, 0)
	require.Less(t, m.logCursorIdx, len(entries))
	assert.Equal(t, entries[m.logCursorIdx].DisplaySeq, m.logCursorSeq)
}

// TestHitLifecycle_ResizeInvalidatesStaleMenuRects is T3 (plan 023): resize
// with a menu open clears the registry so a click at the OLD rect cannot
// activate from stale coordinates.
func TestHitLifecycle_ResizeInvalidatesStaleMenuRects(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	require.GreaterOrEqual(t, len(hits.dropdown.Rows), 2)
	stale := hits.dropdown.Rows[1] // Requests
	require.Equal(t, MenuCmdSetRequests, stale.Cmd)
	col, row := stale.Rect.X+1, stale.Rect.Y

	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.False(t, m.mustHits().hasDropdown, "resize must resetFrame before next View")
	require.Empty(t, m.mustHits().dropdown.Rows)
	require.True(t, m.menuOpen(), "menu state stays open; only hit-rects clear")

	m = clientUpdate(m, clickAt(col, row))
	assert.Equal(t, ViewModeLogs, m.viewMode, "stale coords must not activate Requests")
}

func TestMustHits_PanicsOnNil(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r)
		assert.Contains(t, fmt.Sprint(r), "hitRegistry is nil")
	}()
	b := &BaseModel{}
	_ = b.mustHits()
}
