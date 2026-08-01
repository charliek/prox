package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func withTestSettingsPath(t *testing.T, path string) {
	t.Helper()
	prev := settingsPathFunc
	settingsPathFunc = func() string { return path }
	t.Cleanup(func() { settingsPathFunc = prev })
}

func TestLoadSettings_MissingFile(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, DefaultSettings(), s)
}

func TestLoadSettings_FullFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `theme = "gruvbox"

[view]
process_panel = false
timestamps = true
wrap = true
menu_bar = false
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, "gruvbox", s.Theme)
	assert.False(t, s.ProcessPanel)
	assert.True(t, s.Timestamps)
	assert.True(t, s.Wrap)
	assert.False(t, s.MenuBar)
}

func TestLoadSettings_PartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `theme = "dark"

[view]
wrap = true
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, "dark", s.Theme)
	assert.True(t, s.ProcessPanel) // default
	assert.True(t, s.Timestamps)   // default (timestamps render today; C4 wires the toggle)
	assert.True(t, s.Wrap)
	assert.True(t, s.MenuBar) // default
}

func TestLoadSettings_CorruptTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, os.WriteFile(path, []byte("theme = [[broken"), 0o644))

	s, warnings := LoadSettings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "unparseable")
	assert.Equal(t, DefaultSettings(), s)
}

func TestLoadSettings_UnknownKeysIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `theme = "legacy"
future_flag = 42

[view]
process_panel = true

[other]
nested = "value"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	s, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, "legacy", s.Theme)
	assert.True(t, s.ProcessPanel)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	original := Settings{
		Theme:        "catppuccin",
		ProcessPanel: false,
		Timestamps:   true,
		Wrap:         true,
		MenuBar:      false,
	}
	require.NoError(t, SaveSettings(original))

	loaded, warnings := LoadSettings()
	assert.Empty(t, warnings)
	assert.Equal(t, original, loaded)
}

func TestSaveSettings_PreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	content := `theme = "legacy"
foreign = "keep-me"

[view]
process_panel = true
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	require.NoError(t, SaveSettings(Settings{
		Theme:        "dark",
		ProcessPanel: true,
		Timestamps:   false,
		Wrap:         false,
		MenuBar:      true,
	}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	assert.Contains(t, text, `foreign = "keep-me"`)
	assert.Contains(t, text, `theme = "dark"`)
}

func TestSaveSettings_NeverOverwritesCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	garbage := []byte("not valid {{{ toml")
	require.NoError(t, os.WriteFile(path, garbage, 0o644))

	err := SaveSettings(DefaultSettings())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConfigUnparseable)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, garbage, after)
}

func TestSaveSettings_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, SaveSettings(Settings{Theme: "dark"}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), dirInfo.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"), "stray temp file: %s", e.Name())
	}
}

func TestThemeCycleKey(t *testing.T) {
	withTestTheme(t, "tokyo-night")

	prevProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prevProfile)

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m.ready = true
	m.handleWindowSize(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.logEntries = []domain.LogEntry{{
		Process: "api",
		Line:    "hello",
	}}
	m.updateViewport()
	styledBefore := m.formatLogEntry(m.logEntries[0])
	require.NotEqual(t, "00:00:00 api        hello", styledBefore, "need ANSI profile for style observation")

	assert.Equal(t, "tokyo-night", CurrentThemeName())

	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	require.NotNil(t, cmd)
	updated := newModel.(ClientModel)

	// Cycles to the next preset in canonical order.
	assert.Equal(t, "dark", CurrentThemeName())
	assert.Equal(t, "theme: dark", updated.statusFlash)

	styledAfter := updated.formatLogEntry(updated.logEntries[0])
	assert.NotEqual(t, styledBefore, styledAfter, "formatLogEntry should use new theme styles after updateViewport")

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
}

func TestThemeCycleKey_NoOpInStringFilterMode(t *testing.T) {
	withTestTheme(t, "tokyo-night")

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m.mode = ModeStringFilter
	m.textInput.Focus()
	m.textInput.SetValue("")

	newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	updated := newModel.(ClientModel)
	assert.Equal(t, "tokyo-night", CurrentThemeName())
	assert.Equal(t, "t", updated.textInput.Value())

	_, err := os.Stat(filepath.Join(dir, "config.toml"))
	assert.True(t, os.IsNotExist(err), "theme must not persist from filter-mode typing")
}

func TestThemeCycleKey_AdvancesThroughPresetOrder(t *testing.T) {
	withTestTheme(t, presetOrder[0])

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	for i := 1; i < len(presetOrder); i++ {
		expected := presetOrder[i]
		newModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
		m = newModel.(ClientModel)
		assert.Equal(t, expected, CurrentThemeName())
		assert.Equal(t, "theme: "+expected, m.statusFlash)
	}
}

func TestStartupWarningsMsg(t *testing.T) {
	m := newTestModel()
	m.startupWarnings = []string{"settings file unparseable: bad"}

	newModel, _ := m.Update(StartupWarningsMsg{Warnings: m.startupWarnings})
	updated := newModel.(ClientModel)
	require.Len(t, updated.logEntries, 1)
	assert.Contains(t, updated.logEntries[0].Line, "unparseable")
}

func TestStatusFlashClearMsg(t *testing.T) {
	m := newTestModel()
	m.statusFlash = "theme: dark"

	newModel, _ := m.Update(StatusFlashClearMsg{})
	assert.Empty(t, newModel.(ClientModel).statusFlash)
}
