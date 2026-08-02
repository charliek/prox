package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTheme_UnsafeThemeName(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)

	cases := []struct {
		name string
		in   string
	}{
		{"path separator", "a/b"},
		{"dot dot", ".."},
		{"ellipsis", "..."},
		{"dot dot prefix", "../x"},
		{"absolute path", "/etc/passwd"},
		{"backslash", "foo\\bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, th, w := ResolveTheme(tc.in)
			assert.Equal(t, "tokyo-night", c)
			assert.Equal(t, tokyoNightTheme().OK, th.OK)
			require.NotEmpty(t, w)
			assert.Contains(t, w[0], "invalid theme name")
			assert.Contains(t, w[0], "tokyo-night")
		})
	}
}

func TestClassifyLevel_LogfmtBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		line  string
		level LogLevel
		found bool
	}{
		{"embedded xlevel", "xlevel=info", LogLevelUnknown, false},
		{"embedded blevel", "blevel=debug", LogLevelUnknown, false},
		{"start of line", "level=info", LogLevelInfo, true},
		{"whitespace before key", "msg level=info", LogLevelInfo, true},
		{"embedded mylevel", "mylevel=info", LogLevelUnknown, false},
		{"mid-line with spaces", "retry level=info weird", LogLevelInfo, true},
		{"uppercase at start", "LEVEL=ERROR", LogLevelError, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			level, found := classifyLevel(tc.line)
			assert.Equal(t, tc.level, level)
			assert.Equal(t, tc.found, found)
		})
	}
}

func TestTruncateUTF8Bytes(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("a", 79)
	withRune := prefix + "é"
	assert.True(t, utf8.ValidString(truncateUTF8Bytes(withRune, 80)))
	assert.Equal(t, 79, len(truncateUTF8Bytes(withRune, 80)))
	assert.Equal(t, 81, len(withRune))
	assert.True(t, utf8.ValidString(truncateUTF8Bytes(withRune, 81)))
}

func TestClassifyLevelHeuristics_RuneBoundaryTruncation(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", 68) + "é level=info"
	level, found := classifyLevelHeuristics(line)
	assert.True(t, found)
	assert.Equal(t, LogLevelInfo, level)

	long := strings.Repeat("x", 75) + "é level=info"
	truncated := truncateUTF8Bytes(ansi.Strip(long), bareLevelScanLimit)
	assert.True(t, utf8.ValidString(truncated))
}

func TestPadDisplay_WidthParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		width int
		wantW int
	}{
		{"ascii short", "api", 10, 10},
		{"ascii exact", "webservice", 10, 10},
		{"wide glyph", "あ", 4, 4},
		{"wide plus ascii", "あa", 5, 5},
		{"ansi colored", "\x1b[32mok\x1b[0m", 6, 6},
		{"already wide enough", "hello world", 5, 11},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := padDisplay(tc.in, tc.width)
			assert.Equal(t, tc.wantW, ansi.StringWidth(got))
		})
	}
}

func TestTruncatePadDisplay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		width int
	}{
		{"truncate wide", "あいうえおかき", 6},
		{"truncate ascii", "abcdefghijklmnop", 10},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncatePadDisplay(tc.in, tc.width)
			assert.Equal(t, tc.width, ansi.StringWidth(got))
		})
	}
}

func TestGetProcessStyle_EmptyPalette(t *testing.T) {
	pinANSIProfile(t)
	prev := styles
	t.Cleanup(func() { styles = prev })

	styles.ProcessColors = nil
	got := getProcessStyle("api", nil)
	assert.Equal(t, styles.DefaultProcess, got)

	styles.ProcessColors = []lipgloss.Style{}
	got = getProcessStyle("api", nil)
	assert.Equal(t, styles.DefaultProcess, got)
}

func TestThemeFromTOML_UnknownLogTrace(t *testing.T) {
	t.Parallel()
	_, warns, ok := themeFromTOML(`
base = "dark"
[colors]
log_trace = "#ff0000"
trace = "#00ff00"
`)
	require.True(t, ok)
	require.Len(t, warns, 2)
	joined := strings.Join(warns, "\n") // map iteration order is random
	assert.Contains(t, joined, `unknown colour slot "log_trace"`)
	assert.Contains(t, joined, `unknown colour slot "trace"`)
}
