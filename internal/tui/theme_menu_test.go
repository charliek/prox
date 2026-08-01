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
	assert.Equal(t, "theme: dark", m.statusFlash)

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
	assert.Contains(t, m.statusFlash, "theme: tokyo-night")
	assert.Contains(t, m.statusFlash, "theme TOML parse error")

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
	assert.Contains(t, updated.statusFlash, "theme: tokyo-night")
	assert.Contains(t, updated.statusFlash, "theme TOML parse error")

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
	_ = m.mainView("")
	hits := m.ensureHits()
	require.True(t, hits.hasDropdown)

	// Index 2 = light preset.
	row := hits.dropdown.Rows[2]
	require.Equal(t, menuCmdSetTheme("light"), row.Cmd)
	m = clientUpdate(m, tea.MouseMsg{
		X:      row.Rect.X,
		Y:      row.Rect.Y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	assert.False(t, m.menuOpen())
	assert.Equal(t, "light", CurrentThemeName())
	assert.Equal(t, "theme: light", m.statusFlash)
}
