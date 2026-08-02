package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func motionAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X: x, Y: y,
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonNone,
	}
}

func dragAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X: x, Y: y,
		Action: tea.MouseActionMotion,
		Button: tea.MouseButtonLeft,
	}
}

func TestHover_SiblingSlideWhileOpen(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	require.Equal(t, int(MenuView), m.openMenu)
	_ = m.View()
	hits := m.mustHits()
	require.GreaterOrEqual(t, len(hits.menuCells), 2)
	filterCell := hits.menuCells[1]
	require.Equal(t, MenuFilter, filterCell.ID)

	m = clientUpdate(m, motionAt(filterCell.Rect.X, filterCell.Rect.Y))
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, m.menuFirstSelectable(MenuFilter), m.menuHighlight)
	assert.Equal(t, 0, m.menuWindow)
}

func TestHover_CellDoesNotOpenMenu(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	hits := m.mustHits()
	require.NotEmpty(t, hits.menuCells)
	cell := hits.menuCells[0]

	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	assert.False(t, m.menuOpen())
}

func TestHover_DropdownRowMovesHighlight(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)

	// Requests row (index 1).
	var reqs menuRowHit
	for _, row := range hits.dropdown.Rows {
		if row.Index == 1 {
			reqs = row
			break
		}
	}
	require.NotEqual(t, MenuCommand(""), reqs.Cmd)

	m = clientUpdate(m, motionAt(reqs.Rect.X, reqs.Rect.Y))
	assert.Equal(t, 1, m.menuHighlight)
}

func TestHover_SeparatorRowNoOp(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	before := m.menuHighlight
	_ = m.View()
	hits := m.mustHits()

	var sep menuRowHit
	for _, row := range hits.dropdown.Rows {
		if row.Index < 0 {
			sep = row
			break
		}
	}
	require.Equal(t, -1, sep.Index)

	m = clientUpdate(m, motionAt(sep.Rect.X, sep.Rect.Y))
	assert.Equal(t, before, m.menuHighlight)
}

func TestHover_WindowFollowsPastEdge(t *testing.T) {
	m := openThemeAtHeight(t, 12, 6)
	items := m.menuItems(MenuTheme)
	avail := m.menuAvail()
	for m.menuWindow == 0 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Greater(t, m.menuWindow, 0, "keyboard precond: scrolled window")
	winBefore := m.menuWindow
	hlBefore := m.menuHighlight

	_ = m.View()
	var target menuRowHit
	minIdx := -1
	for _, row := range m.mustHits().dropdown.Rows {
		if row.Index >= 0 && row.Index < hlBefore {
			if minIdx < 0 || row.Index < minIdx {
				minIdx = row.Index
				target = row
			}
		}
	}
	require.GreaterOrEqual(t, minIdx, 0, "need a rendered row above current highlight")

	m = clientUpdate(m, motionAt(target.Rect.X, target.Rect.Y))
	assert.Equal(t, minIdx, m.menuHighlight)
	wantWin := deriveMenuWindowStart(len(items), avail, winBefore, minIdx)
	assert.Equal(t, wantWin, m.menuWindow, "hover calls followMenuWindow so highlight stays visible")
}

func TestHover_GateIdenticalMotionNoChange(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.View()
	hits := m.mustHits()
	require.Equal(t, m.menuHighlight, hits.dropdown.Rows[0].Index,
		"precond: first row is the current highlight → repeat motion is a no-op")
	row := hits.dropdown.Rows[0]

	// Repeat motion over the highlighted row: consumed but zero state change.
	for range 2 {
		consumed := m.handleMenuMotion(motionAt(row.Rect.X, row.Rect.Y))
		assert.True(t, consumed, "hover over open dropdown is consumed")
	}
	assert.Equal(t, int(MenuView), m.openMenu)
	assert.Equal(t, row.Index, m.menuHighlight)
	assert.Equal(t, 0, m.menuWindow)

	// Motion outside menu chrome is not consumed (falls through harmlessly).
	assert.False(t, m.handleMenuMotion(motionAt(10, 15)))
}

func TestHover_StaleDropdownRectsRejected(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.View() // View dropdown rects recorded
	stale := m.mustHits().dropdown
	require.Equal(t, MenuView, stale.Menu)
	row := stale.Rows[1] // Requests row rect (stale after slide)

	// Hover-slide to Filter; hits.dropdown is now STALE until next render.
	var filterCell menuCellHit
	for _, c := range m.mustHits().menuCells {
		if c.ID == MenuFilter {
			filterCell = c
		}
	}
	m = clientUpdate(m, motionAt(filterCell.Rect.X, filterCell.Rect.Y))
	require.Equal(t, int(MenuFilter), m.openMenu)
	hlAfterSlide := m.menuHighlight

	// Motion inside the STALE View dropdown rect: consumed, no row applied.
	consumed := m.handleMenuMotion(motionAt(row.Rect.X, row.Rect.Y))
	assert.True(t, consumed, "stale dropdown bounds still consume hover")
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, hlAfterSlide, m.menuHighlight, "stale row must not set highlight")
}

func TestHover_MenuBarClosedSetsHover(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	cell := m.mustHits().menuCells[0]

	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	assert.False(t, m.menuOpen())
	assert.Equal(t, int(MenuView), m.hoveredMenuCell)
	assert.Equal(t, -1, m.openMenu)
	assert.Equal(t, 0, m.menuHighlight)
	assert.Equal(t, 0, m.menuWindow)
}

func TestHover_DragIgnored(t *testing.T) {
	t0 := time.Unix(5000, 0)
	nowFunc = func() time.Time { return t0 }
	defer func() { nowFunc = time.Now }()

	m := newRequestsModel(4, 6)
	row := 2
	_, oy := m.viewportOrigin()
	y := oy + row
	m = clientUpdate(m, clickAt(10, y))
	require.Equal(t, row, m.lastRequestClickIdx, "arm tracker via request click")

	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	_ = m.View()
	filterCell := m.mustHits().menuCells[1]

	m = clientUpdate(m, dragAt(filterCell.Rect.X, filterCell.Rect.Y))
	assert.Equal(t, int(MenuView), m.openMenu, "drag does not slide sibling menu")
	assert.Equal(t, 0, m.menuHighlight)
	assert.Equal(t, row, m.lastRequestClickIdx, "drag does not clear tracker")
}

func TestHover_TrackerHygieneWheel(t *testing.T) {
	m := newRequestsModel(6, 6)
	_, oy := m.viewportOrigin()
	y := oy
	m = clientUpdate(m, clickAt(10, y))
	require.NotEqual(t, -1, m.lastRequestClickIdx)

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, -1, m.lastRequestClickIdx)
}

func TestHover_TrackerHygieneMenuPress(t *testing.T) {
	m := newRequestsModel(6, 6)
	_, oy := m.viewportOrigin()
	y := oy
	m = clientUpdate(m, clickAt(10, y))
	require.NotEqual(t, -1, m.lastRequestClickIdx)

	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	cell := m.mustHits().menuCells[0]

	m = clientUpdate(m, clickAt(cell.Rect.X, cell.Rect.Y))
	assert.Equal(t, -1, m.lastRequestClickIdx)
}

func TestHover_TrackerHygieneHelpEnterExit(t *testing.T) {
	m := newRequestsModel(6, 6)
	_, oy := m.viewportOrigin()
	y := oy
	m = clientUpdate(m, clickAt(10, y))
	require.NotEqual(t, -1, m.lastRequestClickIdx)

	m = clientUpdate(m, keyRune('?'))
	assert.Equal(t, ModeHelp, m.mode)
	assert.Equal(t, -1, m.lastRequestClickIdx)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, -1, m.lastRequestClickIdx)
}

func TestHover_FreeMotionPreservesDoubleClick(t *testing.T) {
	t0 := time.Unix(6000, 0)
	nowFunc = func() time.Time { return t0 }
	defer func() { nowFunc = time.Now }()

	m := newRequestsModel(4, 6)
	row := 2
	_, oy := m.viewportOrigin()
	y := oy + row

	m = clientUpdate(m, clickAt(10, y))
	require.Equal(t, row, m.lastRequestClickIdx)

	_ = m.View()
	cell := m.mustHits().menuCells[0]
	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))

	nowFunc = func() time.Time { return t0.Add(200 * time.Millisecond) }
	newModel, cmd := m.Update(clickAt(10, y))
	m = newModel.(ClientModel)
	require.NotNil(t, cmd)
	assert.Equal(t, ViewModeRequestDetail, m.viewMode)
}

func TestHover_TextInputModeMotionDoesNotBlur(t *testing.T) {
	m := newLogsModel(8, mouseTestLines(10))
	m = clientUpdate(m, keyRune('s'))
	require.Equal(t, ModeStringFilter, m.mode)
	_ = m.View()
	cell := m.mustHits().menuCells[0]

	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	assert.Equal(t, ModeStringFilter, m.mode)
	assert.False(t, m.menuOpen())
}
