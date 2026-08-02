package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// --- Process panel / Timestamps / Wrap toggles (plan 021 C4 / WS4) ---

func TestToggleProcessPanel_KeyPersistsAndRelayouts(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.settings.ProcessPanel)
	vpBefore := m.viewport.Height

	m = clientUpdate(m, keyRune('p'))
	assert.False(t, m.settings.ProcessPanel)
	assert.Equal(t, vpBefore+2, m.viewport.Height, "hiding process panel frees 2 viewport rows")

	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.False(t, loaded.ProcessPanel)

	m = clientUpdate(m, keyRune('p'))
	assert.True(t, m.settings.ProcessPanel)
	assert.Equal(t, vpBefore, m.viewport.Height)
}

func TestToggleTimestamps_KeyPersists(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.settings.Timestamps)

	m = clientUpdate(m, keyRune('T'))
	assert.False(t, m.settings.Timestamps)
	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.False(t, loaded.Timestamps)

	m = clientUpdate(m, keyRune('T'))
	assert.True(t, m.settings.Timestamps)
}

func TestToggleWrap_KeyPersists(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.False(t, m.settings.Wrap)

	m = clientUpdate(m, keyRune('w'))
	assert.True(t, m.settings.Wrap)
	loaded, warnings := LoadSettings()
	require.Empty(t, warnings)
	assert.True(t, loaded.Wrap)

	m = clientUpdate(m, keyRune('w'))
	assert.False(t, m.settings.Wrap)
}

func TestViewToggles_FlashOnSaveError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	withTestSettingsPath(t, path)
	require.NoError(t, os.WriteFile(path, []byte("theme = [[broken"), 0o644))

	cases := []struct {
		name  string
		key   rune
		check func(m ClientModel) bool
	}{
		{"process panel", 'p', func(m ClientModel) bool { return !m.settings.ProcessPanel }},
		{"timestamps", 'T', func(m ClientModel) bool { return !m.settings.Timestamps }},
		{"wrap", 'w', func(m ClientModel) bool { return m.settings.Wrap }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
			m = clientUpdate(m, keyRune(tc.key))
			assert.True(t, tc.check(m), "toggle still applied in-session")
			assert.Contains(t, m.statusFlash.text, "settings not saved")
		})
	}
}

func TestViewToggles_MenuMatchesKey(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	// Process panel: menu row idx 3
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	vpBefore := m.viewport.Height
	m = clientUpdate(m, keyRune('v'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown}) // Requests
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyDown}) // Process panel (skip sep)
	require.Equal(t, 3, m.menuHighlight)
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, m.settings.ProcessPanel)
	assert.Equal(t, vpBefore+2, m.viewport.Height)

	// Timestamps via key then confirm menu would flip the same way
	m2 := newTestModel()
	m2 = clientUpdate(m2, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m2.settings.Timestamps)
	m2 = clientUpdate(m2, keyRune('v'))
	for range 3 {
		m2 = clientUpdate(m2, keyRune('j'))
	}
	require.Equal(t, 4, m2.menuHighlight) // Timestamps
	m2 = clientUpdate(m2, keyRune(' '))
	assert.False(t, m2.settings.Timestamps)

	m3 := newTestModel()
	m3 = clientUpdate(m3, tea.WindowSizeMsg{Width: 80, Height: 24})
	m3 = clientUpdate(m3, keyRune('v'))
	for range 4 {
		m3 = clientUpdate(m3, keyRune('j'))
	}
	require.Equal(t, 5, m3.menuHighlight) // Wrap lines
	m3 = clientUpdate(m3, keyRune(' '))
	assert.True(t, m3.settings.Wrap)
}

func TestViewToggles_DoNotFireInTextModes(t *testing.T) {
	for _, mode := range []Mode{ModeSearch, ModeStringFilter} {
		for _, key := range []rune{'p', 'T', 'w'} {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
			beforePanel, beforeTS, beforeWrap := m.settings.ProcessPanel, m.settings.Timestamps, m.settings.Wrap
			m.mode = mode
			m.textInput.Focus()
			m.textInput.SetValue("")

			m = clientUpdate(m, keyRune(key))
			assert.Equal(t, mode, m.mode)
			assert.Equal(t, beforePanel, m.settings.ProcessPanel)
			assert.Equal(t, beforeTS, m.settings.Timestamps)
			assert.Equal(t, beforeWrap, m.settings.Wrap)
			assert.Equal(t, string(key), m.textInput.Value())
		}
	}
}

func TestFormatLogEntry_TimestampsToggle(t *testing.T) {
	m := newTestModel()
	m.settings.Timestamps = true
	entry := domain.LogEntry{
		Timestamp: time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC),
		Process:   "api",
		Line:      "hello",
	}
	withTS := m.formatLogEntry(entry)
	assert.Contains(t, withTS, "15:04:05")
	assert.Contains(t, withTS, "api")

	m.settings.Timestamps = false
	without := m.formatLogEntry(entry)
	assert.NotContains(t, without, "15:04:05")
	assert.Contains(t, without, "api")
	assert.True(t, strings.HasPrefix(stripANSI(without), "api"),
		"without timestamps the process name leads the line")
}

func TestWrap_LongLineProducesMultipleDisplayRows(t *testing.T) {
	m := newLogsModelNarrow(70, 10, []string{
		strings.Repeat("word ", 40), // well over 70 cols once prefixed
	})
	require.False(t, m.settings.Wrap)
	m.updateViewport()
	assert.Equal(t, 1, m.viewport.TotalLineCount(), "no-wrap: one entry one row")

	m.settings.Wrap = true
	m.updateViewport()
	require.Len(t, m.logEntries, 1)
	sp := m.logRowSpans[m.logEntries[0].DisplaySeq]
	assert.Greater(t, sp.Last-sp.First, 0, "wrapped entry spans multiple rows")
	assert.Equal(t, sp.Last-sp.First+1, m.viewport.TotalLineCount())
}

// An UNBROKEN token longer than the width must still split into multiple
// rows: Wordwrap leaves it over-wide, the terminal hard-wraps it visually,
// and logRowSpans drifts a row behind reality (CodeRabbit PR #102).
func TestWrap_UnbrokenTokenSplitsAcrossRows(t *testing.T) {
	m := newLogsModelNarrow(70, 10, []string{
		strings.Repeat("x", 300),
	})
	m.settings.Wrap = true
	m.updateViewport()

	require.Len(t, m.logEntries, 1)
	sp := m.logRowSpans[m.logEntries[0].DisplaySeq]
	assert.Greater(t, sp.Last-sp.First, 0, "unbroken token spans multiple rows")
	assert.Equal(t, sp.Last-sp.First+1, m.viewport.TotalLineCount())
}

func TestWrap_NoWrapByteIdenticalToPreC4(t *testing.T) {
	lines := []string{"short", "another line", strings.Repeat("x", 200)}
	m := newLogsModelNarrow(70, 20, lines)
	require.False(t, m.settings.Wrap)
	m.updateViewport()
	noWrap := m.viewport.View()

	// Toggle wrap on then off — content must match the initial no-wrap render.
	m.settings.Wrap = true
	m.updateViewport()
	m.settings.Wrap = false
	m.updateViewport()
	assert.Equal(t, noWrap, m.viewport.View())
}

func TestWrap_SearchScrollLandsOnRightDisplayRow(t *testing.T) {
	// Build a corpus where early entries are short (1 row) and a later match
	// wraps to 3+ rows at ~70 cols. After `n`, the cursor span must be visible.
	longNeedle := "NEEDLE " + strings.Repeat("wrapme ", 30)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("pad %02d", i)
	}
	lines[5] = "first NEEDLE short"
	lines[15] = longNeedle

	m := newLogsModelNarrow(70, 5, lines)
	m = clientUpdate(m, keyRune('g')) // follow off, top
	m.settings.Wrap = true
	m.updateViewport()

	m = commitSearch(m, "NEEDLE")
	require.Equal(t, 5, m.logCursorIdx)
	m = clientUpdate(m, keyRune('n'))
	require.Equal(t, 15, m.logCursorIdx)

	sp := m.logRowSpans[m.logCursorSeq]
	yo := m.viewport.YOffset
	h := m.viewport.Height
	visible := sp.Last >= yo && sp.First < yo+h
	assert.True(t, visible, "cursor span [%d,%d] must intersect viewport [%d,%d)",
		sp.First, sp.Last, yo, yo+h)
}

func TestWrap_TogglePreservesTopAnchorWhenNotFollowing(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		if i == 10 {
			lines[i] = strings.Repeat("longword ", 25)
		} else {
			lines[i] = fmt.Sprintf("line %02d", i)
		}
	}
	m := newLogsModelNarrow(70, 8, lines)
	m = clientUpdate(m, keyRune('g'))
	require.False(t, m.followMode)

	// Park so entry 10 is at the top (no-wrap identity: YOffset == entry index).
	m.viewport.SetYOffset(10)
	anchor := m.logEntries[10].DisplaySeq
	require.Equal(t, anchor, m.displaySeqAtYOffset(10))

	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))
	m = clientUpdate(m, keyRune('w'))
	require.True(t, m.settings.Wrap)

	sp := m.logRowSpans[anchor]
	assert.Equal(t, sp.First, m.viewport.YOffset,
		"wrap-on keeps the same DisplaySeq at the top")

	m = clientUpdate(m, keyRune('w'))
	require.False(t, m.settings.Wrap)
	sp = m.logRowSpans[anchor]
	assert.Equal(t, sp.First, m.viewport.YOffset,
		"wrap-off restores the same DisplaySeq at the top")
}

func TestWrap_ToggleWhileFollowingEndsAtBottom(t *testing.T) {
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = strings.Repeat("wrap ", 20)
	}
	m := newLogsModelNarrow(70, 6, lines)
	require.True(t, m.followMode)
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m = clientUpdate(m, keyRune('w'))
	require.True(t, m.settings.Wrap)
	assert.True(t, m.viewport.AtBottom(), "follow + wrap toggle → bottom")

	m = clientUpdate(m, keyRune('w'))
	require.False(t, m.settings.Wrap)
	assert.True(t, m.viewport.AtBottom(), "follow + unwrap toggle → bottom")
}

func TestWrap_FollowStaysAtBottomAsWrappedLinesArrive(t *testing.T) {
	m := newLogsModelNarrow(70, 5, []string{"seed"})
	m.settings.Wrap = true
	m.updateViewport()
	require.True(t, m.followMode)

	for i := 0; i < 10; i++ {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "p",
			Line:      strings.Repeat("incoming ", 20) + fmt.Sprintf("%d", i),
		}))
	}
	assert.True(t, m.followMode)
	assert.True(t, m.viewport.AtBottom())
}

func TestWrap_EnsureLogCursorVisibleMixedSpans(t *testing.T) {
	// Mix of 1-row and 3-row entries; park cursor on a tall entry below the
	// viewport and ensure ensureLogCursorVisible scrolls it into view.
	lines := []string{
		"short a",
		strings.Repeat("tallone ", 30), // wraps
		"short b",
		strings.Repeat("talltwo ", 30),
		"short c",
		"short d",
		"short e",
		"TARGET " + strings.Repeat("endspan ", 30),
	}
	m := newLogsModelNarrow(70, 4, lines)
	m = clientUpdate(m, keyRune('g'))
	m.settings.Wrap = true
	m.updateViewport()

	m = commitSearch(m, "TARGET")
	require.Equal(t, 7, m.logCursorIdx)
	sp := m.logRowSpans[m.logCursorSeq]
	yo := m.viewport.YOffset
	assert.True(t, sp.Last >= yo && sp.First < yo+m.viewport.Height,
		"mixed-span cursor visible: span=[%d,%d] yo=%d h=%d", sp.First, sp.Last, yo, m.viewport.Height)
}

func TestWrap_EvictionDoesNotCorruptSpans(t *testing.T) {
	m := newLogsModelNarrow(70, 8, nil)
	m.settings.Wrap = true
	m.followMode = false

	// Overwhelm the ring; spans are rebuilt per render — search must still work.
	for i := 0; i < maxLogEntries+50; i++ {
		line := fmt.Sprintf("entry %d ", i)
		if i%17 == 0 {
			line = "NEEDLE " + strings.Repeat("wrap ", 15) + line
		} else {
			line = strings.Repeat("x", 5) + " " + line
		}
		m.handleLogEntry(domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "p",
			Line:      line,
		})
	}
	require.Len(t, m.logEntries, maxLogEntries)
	m.updateViewport()
	assert.Equal(t, len(m.filteredEntries()), len(m.logRowSpans),
		"every filtered entry has a span after eviction")

	m = commitSearch(m, "NEEDLE")
	require.GreaterOrEqual(t, m.logCursorIdx, 0)
	sp, ok := m.logRowSpans[m.logCursorSeq]
	require.True(t, ok)
	assert.LessOrEqual(t, sp.First, sp.Last)
	m.ensureLogCursorVisible()
	yo := m.viewport.YOffset
	assert.True(t, sp.Last >= yo && sp.First < yo+m.viewport.Height)
}

func TestWrap_RequestsViewUnaffected(t *testing.T) {
	m := newRequestsModel(5, 10)
	before := m.viewport.View()
	countBefore := m.viewport.TotalLineCount()

	m.settings.Wrap = true
	m.updateViewport()
	assert.Equal(t, countBefore, m.viewport.TotalLineCount(), "requests never wrap")
	assert.Equal(t, before, m.viewport.View())
	assert.Nil(t, m.logRowSpans, "log spans cleared outside logs view")
}

func TestMenu_ViewRowOrder(t *testing.T) {
	m := newTestModel()
	items := m.menuItems(MenuView)
	require.Len(t, items, 7)
	assert.Equal(t, "Logs", items[0].Label)
	assert.Equal(t, "Requests", items[1].Label)
	assert.True(t, items[2].Separator)
	assert.Equal(t, "Process panel", items[3].Label)
	assert.Equal(t, "Timestamps", items[4].Label)
	assert.Equal(t, "Wrap lines", items[5].Label)
	assert.Equal(t, "Follow", items[6].Label)
}

// newLogsModelNarrow is newLogsModel with an explicit terminal width (for wrap
// tests at ~70 cols).
func newLogsModelNarrow(width, viewportHeight int, lines []string) ClientModel {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: width, Height: viewportHeight + defaultChromeHeight() + defaultPanelBorder()})
	base := time.Unix(0, 0)
	for i, line := range lines {
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Process:   "p",
			Line:      line,
		}))
	}
	return m
}

// stripANSI removes CSI sequences for prefix assertions on styled strings.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			inEsc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
