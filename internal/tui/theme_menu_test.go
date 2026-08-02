package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func openThemeMenu(m ClientModel) ClientModel {
	m = clientUpdate(m, keyRune('v'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyRight}) // Filter
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyRight}) // Theme
	return m
}

func TestThemeMenu_Rows(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zebra.toml"), []byte(`base = "dark"`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha.toml"), []byte(`base = "dark"`), 0o644))

	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	items := m.menuItems(MenuTheme)

	want := append(append([]string{}, presetOrder...), "alpha", "zebra")
	require.Len(t, items, len(want))
	for i, name := range want {
		assert.Equal(t, name, items[i].Label)
		require.NotNil(t, items[i].Selected)
		assert.Equal(t, name == "tokyo-night", *items[i].Selected)
		assert.Equal(t, menuCmdSetTheme(name), items[i].Cmd)
	}
}

func TestThemeMenu_ActiveRowMarked(t *testing.T) {
	withTestTheme(t, "catppuccin")

	m := newTestModel()
	items := m.menuItems(MenuTheme)
	for _, it := range items {
		require.NotNil(t, it.Selected)
		if it.Label == "catppuccin" {
			assert.True(t, *it.Selected)
		} else {
			assert.False(t, *it.Selected)
		}
	}
}

func TestThemeMenu_ActivatePreset(t *testing.T) {
	withTestTheme(t, "tokyo-night")

	prevProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prevProfile)

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m.ready = true
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.logEntries = []domain.LogEntry{{
		Process: "api",
		Line:    "hello",
	}}
	m.updateViewport()
	styledBefore := m.formatLogEntry(m.logEntries[0])
	require.NotEqual(t, "00:00:00 api        hello", styledBefore)

	m = openThemeMenu(m)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown}) // dark
	require.Equal(t, 1, m.menuHighlight)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, m.menuOpen())
	assert.Equal(t, "dark", CurrentThemeName())
	assert.Equal(t, "theme: dark", m.statusFlash.text)

	styledAfter := m.formatLogEntry(m.logEntries[0])
	assert.NotEqual(t, styledBefore, styledAfter, "viewport should re-render with new theme")

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
}

func TestThemeMenu_ActivateMalformedUserTheme(t *testing.T) {
	themesDir := t.TempDir()
	withTestThemesDir(t, themesDir)
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "broken.toml"), []byte(`{{{{not toml`), 0o644))

	withTestTheme(t, "dark")
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = openThemeMenu(m)

	brokenIdx := len(presetOrder)
	for range brokenIdx {
		m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	require.Equal(t, brokenIdx, m.menuHighlight)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "tokyo-night", CurrentThemeName())
	assert.Contains(t, m.statusFlash.text, "theme: tokyo-night")
	assert.Contains(t, m.statusFlash.text, "theme TOML parse error")

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "tokyo-night", loaded.Theme)
}

func TestThemeCycleKey_FlashesResolveWarning(t *testing.T) {
	themesDir := t.TempDir()
	withTestThemesDir(t, themesDir)
	require.NoError(t, os.WriteFile(filepath.Join(themesDir, "broken.toml"), []byte(`{{{{not toml`), 0o644))

	withTestTheme(t, "legacy")

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	require.NotNil(t, cmd)
	updated := newModel.(ClientModel)

	assert.Equal(t, "tokyo-night", CurrentThemeName())
	assert.Contains(t, updated.statusFlash.text, "theme: tokyo-night")
	assert.Contains(t, updated.statusFlash.text, "theme TOML parse error")

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "tokyo-night", loaded.Theme)
}

func TestThemeMenu_MouseActivate(t *testing.T) {
	withTestTheme(t, "tokyo-night")

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = openThemeMenu(m)
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)

	// Index 2 = light preset.
	row := hits.dropdown.Rows[2]
	require.Equal(t, 2, row.Index)
	m = clientUpdate(m, tea.MouseMsg{
		X:      row.Rect.X,
		Y:      row.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.False(t, m.menuOpen())
	assert.Equal(t, "light", CurrentThemeName())
	assert.Equal(t, "theme: light", m.statusFlash.text)
}

// themeLabels returns MenuTheme item labels (test helper).
func themeLabels(m ClientModel) []string {
	items := m.menuItems(MenuTheme)
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Label
	}
	return out
}

func writeUserTheme(t *testing.T, dir, stem string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, stem+".toml"), []byte(`base = "dark"`), 0o644))
}

// TestThemeMenu_CacheInvalidationOnOpenPaths pins plan 023 C14: the user-theme
// ReadDir is computed once when Theme opens and reused while open; invalidated
// on keyboard open, hover-slide into Theme, and close/reopen.
func TestThemeMenu_CacheInvalidationOnOpenPaths(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)
	writeUserTheme(t, dir, "alpha")
	withTestTheme(t, "tokyo-night")

	assertHas := func(t *testing.T, labels []string, name string, want bool) {
		t.Helper()
		found := false
		for _, l := range labels {
			if l == name {
				found = true
				break
			}
		}
		assert.Equal(t, want, found, "theme %q present=%v", name, found)
	}

	t.Run("keyboard open", func(t *testing.T) {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = openThemeMenu(m)
		require.Equal(t, int(MenuTheme), m.openMenu)
		require.NotNil(t, m.themeMenuNames)
		assertHas(t, themeLabels(m), "alpha", true)
		assertHas(t, themeLabels(m), "beta", false)

		// Directory change while open must NOT appear (cache reused).
		writeUserTheme(t, dir, "beta")
		assertHas(t, themeLabels(m), "beta", false)

		// Close/reopen refreshes.
		m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
		require.False(t, m.menuOpen())
		assert.Nil(t, m.themeMenuNames)
		m = openThemeMenu(m)
		assertHas(t, themeLabels(m), "beta", true)
	})

	t.Run("hover-slide into Theme", func(t *testing.T) {
		// Fresh stem so this subtest is independent of keyboard leftovers.
		writeUserTheme(t, dir, "gamma")
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = clientUpdate(m, keyRune('v'))
		require.Equal(t, int(MenuView), m.openMenu)
		_ = m.View()
		hits := m.mustHits()
		require.GreaterOrEqual(t, len(hits.menuCells), 3)
		themeCell := hits.menuCells[2]
		require.Equal(t, MenuTheme, themeCell.ID)

		m = clientUpdate(m, motionAt(themeCell.Rect.X, themeCell.Rect.Y))
		require.Equal(t, int(MenuTheme), m.openMenu)
		require.NotNil(t, m.themeMenuNames)
		assertHas(t, themeLabels(m), "gamma", true)
		assertHas(t, themeLabels(m), "delta", false)

		writeUserTheme(t, dir, "delta")
		assertHas(t, themeLabels(m), "delta", false)

		// Slide away then back into Theme — open path refreshes.
		filterCell := hits.menuCells[1]
		require.Equal(t, MenuFilter, filterCell.ID)
		m = clientUpdate(m, motionAt(filterCell.Rect.X, filterCell.Rect.Y))
		require.Equal(t, int(MenuFilter), m.openMenu)
		assert.Nil(t, m.themeMenuNames)

		_ = m.View()
		hits = m.mustHits()
		themeCell = hits.menuCells[2]
		m = clientUpdate(m, motionAt(themeCell.Rect.X, themeCell.Rect.Y))
		require.Equal(t, int(MenuTheme), m.openMenu)
		assertHas(t, themeLabels(m), "delta", true)
	})

	t.Run("close reopen", func(t *testing.T) {
		writeUserTheme(t, dir, "epsilon")
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m = openThemeMenu(m)
		assertHas(t, themeLabels(m), "epsilon", true)
		assertHas(t, themeLabels(m), "zeta", false)

		writeUserTheme(t, dir, "zeta")
		assertHas(t, themeLabels(m), "zeta", false)

		m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
		require.False(t, m.menuOpen())
		m = openThemeMenu(m)
		assertHas(t, themeLabels(m), "zeta", true)
	})
}
