package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUserThemes writes n user theme files so AvailableThemes has presets+n.
func seedUserThemes(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	withTestThemesDir(t, dir)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user-%02d.toml", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(`base = "dark"`), 0o644))
	}
	return dir
}

func openThemeAtHeight(t *testing.T, height int, userThemes int) ClientModel {
	t.Helper()
	seedUserThemes(t, userThemes)
	withTestTheme(t, "tokyo-night")
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: height})
	m = openThemeMenu(m)
	require.True(t, m.menuOpen())
	return m
}

func TestMenuWindow_ClampWithIndicators(t *testing.T) {
	// Height=12 → avail = 12 - 1 - 1 - 2(border) = 8. 6 presets + 6 user = 12 > 8 → clamp.
	m := openThemeAtHeight(t, 12, 6)
	items := m.menuItems(MenuTheme)
	require.GreaterOrEqual(t, len(items), 12)

	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	avail := m.menuAvail()
	require.Equal(t, 8, avail)
	assert.Equal(t, avail, len(hits.dropdown.Rows), "content rows == avail when clamped with bottom indicator")
	assert.Equal(t, avail+menuBorderSize, hits.dropdown.Bounds.H, "Bounds include top+bottom border")
	assert.Equal(t, 0, m.menuWindow)

	// At top: bottom indicator only, no top indicator.
	botInd := hits.dropdown.Rows[len(hits.dropdown.Rows)-1]
	assert.Equal(t, -1, botInd.Index)
	assert.NotEqual(t, -1, hits.dropdown.Rows[0].Index, "first row is an item, not top indicator")

	// Scroll highlight to last → final window, top indicator, no bottom.
	for m.menuHighlight < len(items)-1 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, len(items)-1, m.menuHighlight)
	assert.Equal(t, menuWindowMaxOffset(len(items), avail), m.menuWindow)

	_ = m.View()
	hits = m.mustHits()
	topInd := hits.dropdown.Rows[0]
	assert.Equal(t, -1, topInd.Index)
	lastRow := hits.dropdown.Rows[len(hits.dropdown.Rows)-1]
	assert.NotEqual(t, -1, lastRow.Index, "bottom row is an item, not indicator")
	assert.Equal(t, len(items)-1, lastRow.Index)

	// Middle window (offset 1..max-1): both indicators.
	// From the end, k until window is strictly between 0 and maxOffset.
	maxOff := menuWindowMaxOffset(len(items), avail)
	for m.menuWindow >= maxOff || m.menuWindow == 0 {
		m = clientUpdate(m, keyRune('k'))
		require.True(t, m.menuOpen())
	}
	require.Greater(t, m.menuWindow, 0)
	require.Less(t, m.menuWindow, maxOff)

	_ = m.View()
	hits = m.mustHits()
	require.GreaterOrEqual(t, len(hits.dropdown.Rows), 3)
	assert.Equal(t, -1, hits.dropdown.Rows[0].Index, "top indicator")
	assert.Equal(t, -1, hits.dropdown.Rows[len(hits.dropdown.Rows)-1].Index, "bottom indicator")
}

func TestMenuWindow_NoIndicatorsWhenAvailSmall(t *testing.T) {
	// Height=7 → avail = 7 - 1 - 1 - 2 = 3. Indicators only when avail >= 4.
	m := openThemeAtHeight(t, 7, 6)
	require.Equal(t, 3, m.menuAvail())
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	assert.Equal(t, 3, len(hits.dropdown.Rows))
	assert.Equal(t, 3+menuBorderSize, hits.dropdown.Bounds.H)
	for _, r := range hits.dropdown.Rows {
		assert.NotEqual(t, -1, r.Index, "no indicator rows when avail < 4")
	}
	assert.NotContains(t, stripANSI(m.View()), "more")
}

func TestMenuWindow_FollowsHighlightKeyboard(t *testing.T) {
	m := openThemeAtHeight(t, 12, 6)
	items := m.menuItems(MenuTheme)
	avail := m.menuAvail()
	require.Equal(t, 0, m.menuWindow)

	// j past the initial window edge scrolls minimally.
	_, visEnd, _, _ := menuWindowLayout(len(items), avail, m.menuWindow)
	for m.menuHighlight < visEnd-1 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, visEnd-1, m.menuHighlight)
	assert.Equal(t, 0, m.menuWindow, "still at top while highlight inside window")

	m = clientUpdate(m, keyRune('j')) // one past edge
	assert.Equal(t, visEnd, m.menuHighlight)
	assert.Greater(t, m.menuWindow, 0, "window scrolls when highlight leaves bottom")
	start, end, _, _ := menuWindowLayout(len(items), avail, m.menuWindow)
	assert.GreaterOrEqual(t, m.menuHighlight, start)
	assert.Less(t, m.menuHighlight, end)

	// Wrap last→first resets offset to 0.
	for m.menuHighlight < len(items)-1 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, len(items)-1, m.menuHighlight)
	m = clientUpdate(m, keyRune('j'))
	assert.Equal(t, 0, m.menuHighlight)
	assert.Equal(t, 0, m.menuWindow)

	// Wrap first→last jumps to final window.
	m = clientUpdate(m, keyRune('k'))
	assert.Equal(t, len(items)-1, m.menuHighlight)
	assert.Equal(t, menuWindowMaxOffset(len(items), avail), m.menuWindow)
}

func TestMenuWindow_SiblingSlideResetsOffset(t *testing.T) {
	m := openThemeAtHeight(t, 12, 6)
	items := m.menuItems(MenuTheme)
	for m.menuHighlight < len(items)-1 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Greater(t, m.menuWindow, 0)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyLeft}) // Theme → Filter
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, 0, m.menuWindow, "sibling slide resets window")
}

func TestMenuWheel_MovesHighlightClampedNoWrap(t *testing.T) {
	m := openThemeAtHeight(t, 12, 6)
	items := m.menuItems(MenuTheme)
	require.Equal(t, 0, m.menuHighlight)

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, 1, m.menuHighlight)
	assert.True(t, m.menuOpen())

	// Drive to last; further wheel-down stays put (no wrap).
	for m.menuHighlight < len(items)-1 {
		m = clientUpdate(m, wheelDown())
	}
	require.Equal(t, len(items)-1, m.menuHighlight)
	m = clientUpdate(m, wheelDown())
	assert.Equal(t, len(items)-1, m.menuHighlight, "wheel does not wrap")

	m = clientUpdate(m, wheelUp())
	assert.Equal(t, len(items)-2, m.menuHighlight)

	// At first: wheel-up clamps.
	for m.menuHighlight > 0 {
		m = clientUpdate(m, wheelUp())
	}
	m = clientUpdate(m, wheelUp())
	assert.Equal(t, 0, m.menuHighlight)
}

func TestMenuWheel_SkipsSeparators(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	require.Equal(t, 0, m.menuHighlight) // Logs

	m = clientUpdate(m, wheelDown()) // Requests
	assert.Equal(t, 1, m.menuHighlight)
	m = clientUpdate(m, wheelDown()) // skip sep → Process panel
	assert.Equal(t, 3, m.menuHighlight, "wheel skips separator")
}

func TestMenuWheel_ConsumedOverLogsArea(t *testing.T) {
	seedUserThemes(t, 6)
	withTestTheme(t, "tokyo-night")
	m := newLogsModel(10, mouseTestLines(30))
	m.projectName = "demo"
	m.settings.MenuBar = true
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('g')) // top, follow off
	require.Equal(t, 0, m.viewport.YOffset)

	m = openThemeMenu(m)
	require.True(t, m.menuOpen())
	yoBefore := m.viewport.YOffset
	hlBefore := m.menuHighlight

	// Wheel over the logs area (Y well below menu) must NOT scroll viewport.
	m = clientUpdate(m, tea.MouseMsg{
		X: 0, Y: 15,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	})
	assert.True(t, m.menuOpen())
	assert.Equal(t, yoBefore, m.viewport.YOffset, "menu-open wheel must not scroll viewport")
	assert.Equal(t, hlBefore+1, m.menuHighlight)
}

func TestMenuWheel_ClosedStillScrollsViewport(t *testing.T) {
	m := newLogsModel(10, mouseTestLines(30))
	m = clientUpdate(m, keyRune('g'))
	require.False(t, m.menuOpen())
	require.Equal(t, 0, m.viewport.YOffset)

	m = clientUpdate(m, wheelDown())
	assert.Equal(t, wheelScrollRows, m.viewport.YOffset)
}

// assertDropdownRectGeometry pins the shared geometry honesty checks used by
// every menu (plan 023 C1 rect-honesty + C9 border inset).
func assertDropdownRectGeometry(t *testing.T, m ClientModel) {
	t.Helper()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	frameH := m.height
	avail := m.menuAvail()
	bounds := hits.dropdown.Bounds
	assert.Equal(t, len(hits.dropdown.Rows)+menuBorderSize, bounds.H,
		"Bounds include top+bottom border around content rows")
	assert.LessOrEqual(t, len(hits.dropdown.Rows), avail)
	for _, r := range hits.dropdown.Rows {
		assert.GreaterOrEqual(t, r.Rect.Y, bounds.Y+1, "content inset +1 row for top border")
		assert.Less(t, r.Rect.Y, bounds.Y+bounds.H-1, "content above bottom border")
		assert.Less(t, r.Rect.Y, frameH-menuReservedBottom,
			"row Y=%d must stay above footer", r.Rect.Y)
		assert.Less(t, r.Rect.Y+r.Rect.H, frameH)
		assert.Equal(t, bounds.X+1, r.Rect.X, "content inset +1 col for left border")
		assert.Equal(t, bounds.W-2, r.Rect.W, "content width excludes L/R border")
	}
	// Never cover the footer: outer bottom edge is strictly above footer row.
	assert.LessOrEqual(t, bounds.Y+bounds.H, frameH-menuReservedBottom)
}

// TestMenuWindow_RectHonesty generalizes rect-honesty over View/Filter/Theme
// and standard sizes (plan 023 C1). Theme keeps the scroll-indicator +
// click-to-activate depth of the original test; View/Filter assert geometry
// and that each visible activatable rect activates (menu closes).
func TestMenuWindow_RectHonesty(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24},
		{120, 40},
		{60, 16},
		{80, 12}, // Theme overflows here (12 items > avail 8): exercises indicators
	}
	menus := []struct {
		name MenuID
		open func(ClientModel) ClientModel
	}{
		{MenuView, func(m ClientModel) ClientModel { return clientUpdate(m, keyRune('v')) }},
		{MenuFilter, func(m ClientModel) ClientModel { return clientUpdate(m, keyRune('f')) }},
		{MenuTheme, openThemeMenu},
	}

	for _, sz := range sizes {
		for _, menu := range menus {
			t.Run(fmt.Sprintf("%s_%dx%d", menuLabel(menu.name), sz.w, sz.h), func(t *testing.T) {
				dir := t.TempDir()
				withTestSettingsPath(t, filepath.Join(dir, "config.toml"))
				if menu.name == MenuTheme {
					seedUserThemes(t, 6)
					withTestTheme(t, "tokyo-night")
				}
				m := newTestModel()
				m.projectName = "demo"
				m = clientUpdate(m, tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
				m = menu.open(m)
				require.True(t, m.menuOpen())
				require.Equal(t, int(menu.name), m.openMenu)

				if menu.name == MenuTheme {
					assertThemeRectHonesty(t, m)
					return
				}
				assertMenuRectHonestyActivate(t, m, menu.name)
			})
		}
	}
}

// assertThemeRectHonesty preserves the original Theme honesty depth: scroll
// to a middle window with indicators, geometry checks, indicator click is
// consumed-only, and every visible activatable row click selects that theme.
func assertThemeRectHonesty(t *testing.T, m ClientModel) {
	t.Helper()
	items := m.menuItems(MenuTheme)
	avail := m.menuAvail()
	require.Greater(t, avail, 0)

	target := len(items) / 2
	for m.menuHighlight < target {
		m = clientUpdate(m, keyRune('j'))
	}

	_ = m.View()
	assertDropdownRectGeometry(t, m)
	hits := m.mustHits()

	type targetRow struct {
		index int
		label string
	}
	var activatable []targetRow
	hasInd := false
	for _, r := range hits.dropdown.Rows {
		if r.Index < 0 {
			hasInd = true
			continue
		}
		activatable = append(activatable, targetRow{index: r.Index, label: items[r.Index].Label})
	}
	require.NotEmpty(t, activatable)
	if len(items) > avail {
		require.True(t, hasInd, "overflow Theme menu must show an indicator")
	}

	// Indicator click: consumed only.
	mInd := m
	_ = mInd.View()
	var indRect HitRect
	foundInd := false
	for _, r := range mInd.mustHits().dropdown.Rows {
		if r.Index < 0 {
			indRect = r.Rect
			foundInd = true
			break
		}
	}
	if foundInd {
		mInd = clientUpdate(mInd, clickAt(indRect.X+1, indRect.Y))
		assert.True(t, mInd.menuOpen(), "indicator click stays open")
		assert.Equal(t, "tokyo-night", CurrentThemeName())
	}

	for _, want := range activatable {
		SetThemeByName("tokyo-night")
		m2 := m
		m2.openMenuFirst(MenuTheme)
		for m2.menuHighlight < target {
			m2 = clientUpdate(m2, keyRune('j'))
		}
		_ = m2.View()
		var row menuRowHit
		found := false
		for _, hr := range m2.mustHits().dropdown.Rows {
			if hr.Index == want.index {
				row = hr
				found = true
				break
			}
		}
		require.True(t, found, "index %d visible", want.index)
		m2 = clientUpdate(m2, clickAt(row.Rect.X+1, row.Rect.Y))
		assert.False(t, m2.menuOpen())
		assert.Equal(t, want.label, CurrentThemeName())
	}
	SetThemeByName("tokyo-night")
}

// assertMenuRectHonestyActivate checks geometry then clicks every visible
// activatable row (re-opening between clicks) and asserts the menu closes.
func assertMenuRectHonestyActivate(t *testing.T, m ClientModel, id MenuID) {
	t.Helper()
	_ = m.View()
	assertDropdownRectGeometry(t, m)
	hits := m.mustHits()

	var indexes []int
	for _, r := range hits.dropdown.Rows {
		if r.Index < 0 {
			continue
		}
		indexes = append(indexes, r.Index)
	}
	require.NotEmpty(t, indexes)

	for _, idx := range indexes {
		m2 := m
		m2.openMenuFirst(id)
		_ = m2.View()
		var row menuRowHit
		found := false
		for _, hr := range m2.mustHits().dropdown.Rows {
			if hr.Index == idx {
				row = hr
				found = true
				break
			}
		}
		require.True(t, found, "index %d visible in %s", idx, menuLabel(id))
		m2 = clientUpdate(m2, clickAt(row.Rect.X+1, row.Rect.Y))
		assert.False(t, m2.menuOpen(), "activatable rect for index %d must activate", idx)
	}
}

func TestMenuWindow_DegenerateAvailZero(t *testing.T) {
	// avail = height - boxTop - reserved - border < 1 → height < 5 with menu bar.
	m := openThemeAtHeight(t, 4, 6)
	require.Less(t, m.menuAvail(), 1)
	require.True(t, m.menuOpen())

	_ = m.View() // must not panic
	hits := m.mustHits()
	assert.False(t, hits.hasDropdown)
	assert.Empty(t, hits.dropdown.Rows)

	// Enter still activates via menuHighlight (not rects).
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown}) // dark
	require.Equal(t, 1, m.menuHighlight)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, m.menuOpen())
	assert.Equal(t, "dark", CurrentThemeName())
}

func TestMenuWindowLayout_Math(t *testing.T) {
	// Unit: layout helpers.
	start, end, top, bot := menuWindowLayout(12, 9, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 8, end) // avail-1 content + bottom ind
	assert.False(t, top)
	assert.True(t, bot)

	start, end, top, bot = menuWindowLayout(12, 9, menuWindowMaxOffset(12, 9))
	assert.Equal(t, 4, start) // 12 - 8
	assert.Equal(t, 12, end)
	assert.True(t, top)
	assert.False(t, bot)

	start, end, top, bot = menuWindowLayout(12, 2, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 2, end)
	assert.False(t, top)
	assert.False(t, bot)

	start, end, top, bot = menuWindowLayout(5, 10, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 5, end)
	assert.False(t, top)
	assert.False(t, bot)

	// avail == 3: last height without indicators (indicators need avail >= 4).
	start, end, top, bot = menuWindowLayout(12, 3, 1)
	assert.Equal(t, 1, start)
	assert.Equal(t, 4, end)
	assert.False(t, top)
	assert.False(t, bot)

	// n == avail: no clamp, no indicators.
	start, end, top, bot = menuWindowLayout(9, 9, 0)
	assert.Equal(t, 0, start)
	assert.Equal(t, 9, end)
	assert.False(t, top)
	assert.False(t, bot)

	assert.Equal(t, 0, menuWindowMaxOffset(5, 10))
	assert.Equal(t, 0, menuWindowMaxOffset(9, 9))
	assert.Equal(t, 0, menuWindowMaxOffset(12, 0))
}

func TestMenuWindow_ViewMenuIndexOnHits(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)

	var foundSep, foundReqs bool
	for _, r := range hits.dropdown.Rows {
		if r.Index == 1 {
			foundReqs = true
		}
		if r.Index < 0 {
			foundSep = true
		}
	}
	assert.True(t, foundReqs)
	assert.True(t, foundSep, "separator row recorded as non-activatable")
}
