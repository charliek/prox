package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// TestMain redirects the DEFAULT settings path to a throwaway file for the
// whole test process: a test that persists without an explicit
// withTestSettingsPath override must never write the developer's real
// ~/.prox/tui/config.toml (TestMenu_FNoOpWhenMenuBarHidden did exactly that —
// menu_bar=false showed up in the real config).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "prox-tui-test-config")
	if err != nil {
		panic(err)
	}
	settingsPathFunc = func() string { return filepath.Join(dir, "config.toml") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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
	// MkdirAll's 0o755 is masked by the process umask (077 → 0700): assert
	// owner access rather than the exact mode (CodeRabbit PR #102).
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm()&0o700)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		assert.False(t, strings.HasSuffix(name, ".tmp"), "stray temp file: %s", name)
	}
}

// Two Settings structs with disjoint typed changed-sets saved sequentially from
// stale in-memory views — both changes must persist (plan 023 D1 / C12).
func TestSaveSettingsChanged_StaleViewMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, SaveSettings(Settings{
		Theme:        "legacy",
		ProcessPanel: true,
		Timestamps:   true,
		Wrap:         true,
		MenuBar:      true,
	}))

	stale1, warnings := LoadSettings()
	require.Empty(t, warnings)
	stale2 := stale1 // identical stale snapshot

	stale1.Wrap = false
	require.NoError(t, SaveSettingsChanged(stale1, settingViewWrap))

	// stale2 still believes Wrap=true; saving only theme must not clobber wrap.
	stale2.Theme = "dark"
	require.NoError(t, SaveSettingsChanged(stale2, settingTheme))

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
	assert.False(t, loaded.Wrap, "wrap=false from first stale save must survive")
	assert.True(t, loaded.ProcessPanel)
	assert.True(t, loaded.Timestamps)
	assert.True(t, loaded.MenuBar)
}

// Lock exclusion: writer1 pauses after re-read under the flock; writer2 reaches
// beforeFlock then blocks on flock until writer1 finishes. Writer2's re-read
// must see writer1's write — the interleaving merge-alone would lose (plan 023 D1).
func TestSaveSettingsChanged_LockExclusion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	require.NoError(t, SaveSettings(Settings{
		Theme:        "legacy",
		ProcessPanel: true,
		Timestamps:   true,
		Wrap:         true,
		MenuBar:      true,
	}))

	w1AtAfterRead := make(chan struct{})
	w1Continue := make(chan struct{})
	w2AtBeforeFlock := make(chan struct{})
	w2MayFlock := make(chan struct{})

	var afterReadCount atomic.Int32
	settingsAfterReadHook = func() {
		if afterReadCount.Add(1) == 1 {
			close(w1AtAfterRead)
			<-w1Continue
		}
	}
	t.Cleanup(func() {
		settingsAfterReadHook = nil
		settingsBeforeFlockHook = nil
	})

	errc := make(chan error, 2)

	go func() {
		s := Settings{Wrap: false, Theme: "legacy", ProcessPanel: true, Timestamps: true, MenuBar: true}
		errc <- SaveSettingsChanged(s, settingViewWrap)
	}()
	<-w1AtAfterRead // w1 holds flock, has re-read original

	settingsBeforeFlockHook = func() {
		close(w2AtBeforeFlock)
		<-w2MayFlock
	}

	go func() {
		// Stale view still has Wrap=true — without lock+re-read this would
		// overwrite w1's wrap=false when merging theme onto a stale root.
		s := Settings{Theme: "dark", Wrap: true, ProcessPanel: true, Timestamps: true, MenuBar: true}
		errc <- SaveSettingsChanged(s, settingTheme)
	}()
	<-w2AtBeforeFlock // w2 about to flock; w1 still holds the lock
	close(w2MayFlock) // w2 proceeds into flock (blocks until w1 unlocks)
	close(w1Continue) // w1 merges+writes+unlocks; w2 then re-reads w1's file

	require.NoError(t, <-errc)
	require.NoError(t, <-errc)

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
	assert.False(t, loaded.Wrap, "w2 must re-read under lock and keep w1's wrap=false")
}

func TestSaveSettings_FsyncFailureWording(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)

	prev := fsyncFileFn
	call := 0
	fsyncFileFn = func(f *os.File) error {
		call++
		// Temp-file sync (pre-rename) succeeds; post-rename path/dir sync fails.
		if call >= 2 {
			return errors.New("injected fsync failure")
		}
		return prev(f)
	}
	t.Cleanup(func() { fsyncFileFn = prev })

	err := SaveSettingsChanged(Settings{Theme: "dark"}, settingTheme)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSettingsMayNotHavePersisted)
	assert.Contains(t, err.Error(), "may not have persisted")
	assert.NotContains(t, formatSettingsSaveError(err), "settings not saved")
	assert.Contains(t, formatSettingsSaveError(err), "may not have persisted")

	// Rename already happened — theme must be on disk despite fsync failure.
	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.Equal(t, "dark", loaded.Theme)
}

func TestFormatSettingsSaveError(t *testing.T) {
	assert.Equal(t, "settings not saved: disk full",
		formatSettingsSaveError(errors.New("disk full")))
	wrapped := fmt.Errorf("%w: %v", errSettingsMayNotHavePersisted, errors.New("sync failed"))
	assert.Equal(t, "settings saved but may not have persisted: sync failed",
		formatSettingsSaveError(wrapped))
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
	assert.Equal(t, "theme: dark", updated.statusFlash.text)

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
		assert.Equal(t, "theme: "+expected, m.statusFlash.text)
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
	cmd := m.setStatusFlash(footerInfo("theme: dark"), flashTransient, statusFlashClearDelay)
	require.NotNil(t, cmd)
	require.Equal(t, 1, m.statusFlashSeq)

	newModel, _ := m.Update(StatusFlashClearMsg{Seq: 1})
	assert.Empty(t, newModel.(ClientModel).statusFlash.text)
}

// A stale clear timer must not erase a NEWER flash: copy flash (2s) racing a
// theme flash (3s) was the reported case (CodeRabbit PR #102).
func TestStatusFlash_StaleTimerKeepsNewerFlash(t *testing.T) {
	m := newTestModel()
	m.setStatusFlash(footerInfo("theme: dark"), flashTransient, statusFlashClearDelay) // seq 1, 3s timer
	m.setStatusFlash(footerInfo("copied curl"), flashTransient, copyFlashClearDelay)   // seq 2, 2s timer

	// The 2s copy timer fires first with seq 2 — clears the copy flash.
	after, _ := m.Update(StatusFlashClearMsg{Seq: 2})
	m = after.(ClientModel)
	m.setStatusFlash(footerInfo("theme: light"), flashTransient, statusFlashClearDelay) // seq 3

	// Now the ORIGINAL 3s timer (seq 1) fires — must NOT clear seq 3's flash.
	after, _ = m.Update(StatusFlashClearMsg{Seq: 1})
	m = after.(ClientModel)
	assert.Equal(t, "theme: light", m.statusFlash.text)

	after, _ = m.Update(StatusFlashClearMsg{Seq: 3})
	assert.Empty(t, after.(ClientModel).statusFlash.text)
}
