package tui

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayout_FrameHeightCombos(t *testing.T) {
	sizes := []struct{ w, h int }{{80, 24}, {120, 40}, {40, 12}}
	for _, menu := range []bool{true, false} {
		for _, panel := range []bool{true, false} {
			for _, sz := range sizes {
				m := newTestModel()
				m.settings.MenuBar = menu
				m.settings.ProcessPanel = panel
				m = clientUpdate(m, tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
				frame := m.mainView(footerMsg{})
				assert.Equal(t, sz.h, lipgloss.Height(frame),
					"menu=%v panel=%v size=%dx%d chrome=%d vp=%d",
					menu, panel, sz.w, sz.h, m.chromeHeight(), m.viewport.Height)
			}
		}
	}
}

func TestRelayout_ToggleCallsRelayout(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.settings.MenuBar)
	vpBefore := m.viewport.Height

	m = clientUpdate(m, keyRune('m'))
	assert.False(t, m.settings.MenuBar)
	assert.Equal(t, vpBefore+1, m.viewport.Height, "hiding menu bar frees one viewport row")
	assert.Equal(t, 24, lipgloss.Height(m.mainView(footerMsg{})))
}

func TestMenuBar_TogglePersists(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.settings.MenuBar)

	m = clientUpdate(m, keyRune('m'))
	assert.False(t, m.settings.MenuBar)

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.False(t, loaded.MenuBar)

	m = clientUpdate(m, keyRune('m'))
	assert.True(t, m.settings.MenuBar)
	loaded, _ = LoadSettings()
	assert.True(t, loaded.MenuBar)
}

func TestMenu_OpenCloseNavigation(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.False(t, m.menuOpen())

	// v opens View.
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	assert.Equal(t, int(MenuView), m.openMenu)
	assert.Equal(t, 0, m.menuHighlight, "first selectable row")

	// Down: Requests (1), skip sep (2), Process panel (3), …
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 1, m.menuHighlight)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 3, m.menuHighlight, "skips separator at idx 2")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 4, m.menuHighlight, "Timestamps")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 5, m.menuHighlight, "Wrap lines")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 6, m.menuHighlight, "Follow")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 0, m.menuHighlight, "wraps to first selectable")

	// Up wraps the other way.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, 6, m.menuHighlight)

	// Sibling switch: Right → Filter, then Theme, then View.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, int(MenuFilter), m.openMenu)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, int(MenuTheme), m.openMenu)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, int(MenuView), m.openMenu)

	// Left / BackTab go the other way.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, int(MenuTheme), m.openMenu)

	// Esc closes, consumed.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, m.menuOpen())

	// Re-open and type an unrelated key: closes, does NOT dispatch (e.g. no quit).
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	newModel, cmd := m.Update(keyRune('q'))
	m = newModel.(ClientModel)
	assert.False(t, m.menuOpen())
	assert.Nil(t, cmd, "q while menu open must be consumed, not quit")
}

func TestMenu_ViewRadioAndFollow(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Equal(t, ViewModeLogs, m.viewMode)
	require.True(t, m.followMode)

	m = clientUpdate(m, keyRune('v'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown}) // Requests
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, m.menuOpen())
	assert.Equal(t, ViewModeRequests, m.viewMode)

	m = clientUpdate(m, keyRune('v'))
	// highlight starts on Logs (idx 0); activate
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, ViewModeLogs, m.viewMode)

	m = clientUpdate(m, keyRune('v'))
	// Logs(0) → Requests(1) → Process panel(3) → Timestamps(4) → Wrap(5) → Follow(6)
	for range 5 {
		m = clientUpdate(m, keyRune('j'))
	}
	require.Equal(t, 6, m.menuHighlight)
	m = clientUpdate(m, keyRune(' '))
	assert.False(t, m.followMode, "Follow check toggles off")
}

func TestSetViewMode_DetailTeardown(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "abc"
	m.requestDetail = &RequestDetailData{ID: "abc"}
	m.detailError = assert.AnError
	m.detailRefreshFailed = true
	m.detailLoading = true

	m.setViewMode(ViewModeRequests)
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Empty(t, m.selectedRequestID)
	assert.Nil(t, m.requestDetail)
	assert.Nil(t, m.detailError)
	assert.False(t, m.detailRefreshFailed)
	assert.False(t, m.detailLoading)

	// Esc path also tears down.
	m.viewMode = ViewModeRequestDetail
	m.selectedRequestID = "xyz"
	m.requestDetail = &RequestDetailData{ID: "xyz"}
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, ViewModeRequests, m.viewMode)
	assert.Empty(t, m.selectedRequestID)
	assert.Nil(t, m.requestDetail)
}

func TestMenu_VOpenerDoesNotFireInTextModes(t *testing.T) {
	for _, mode := range []Mode{ModeSearch, ModeStringFilter} {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m.mode = mode
		m.textInput.Focus()
		m.textInput.SetValue("")

		m = clientUpdate(m, keyRune('v'))
		assert.False(t, m.menuOpen(), "mode=%v", mode)
		assert.Equal(t, mode, m.mode)
		assert.Equal(t, "v", m.textInput.Value())
	}
}

func TestMenu_FOpensFilterMenu(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('f'))
	assert.True(t, m.menuOpen())
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, ModeNormal, m.mode)
}

func TestMenu_MouseOpenBlursTextInput(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	// Render once so hit-rects exist.
	_ = m.mainView(footerMsg{})
	require.NotEmpty(t, m.mustHits().menuCells)

	m.mode = ModeStringFilter
	m.textInput.Focus()
	m.textInput.SetValue("partial")

	viewHit := m.mustHits().menuCells[0]
	m = clientUpdate(m, tea.MouseMsg{
		X:      viewHit.Rect.X,
		Y:      viewHit.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.Equal(t, ModeNormal, m.mode, "mouse-open blurs text mode first")
	assert.True(t, m.menuOpen())
	assert.Equal(t, int(MenuView), m.openMenu)
}

func TestMenu_MouseClickDropdownActivates(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.mainView(footerMsg{}) // record dropdown hits
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)
	require.GreaterOrEqual(t, len(hits.dropdown.Rows), 2)

	reqs := hits.dropdown.Rows[1] // Requests
	require.Equal(t, 1, reqs.Index)
	m = clientUpdate(m, tea.MouseMsg{
		X:      reqs.Rect.X,
		Y:      reqs.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.False(t, m.menuOpen())
	assert.Equal(t, ViewModeRequests, m.viewMode)
}

func TestMenu_MouseClickAwayCloses(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	_ = m.mainView(footerMsg{})

	m = clientUpdate(m, tea.MouseMsg{
		X: 0, Y: 10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.False(t, m.menuOpen())
}
