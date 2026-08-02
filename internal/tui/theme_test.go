package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTestTheme installs name and restores the PREVIOUS theme on cleanup.
// Theme-mutating tests must NOT call t.Parallel — the style set is a
// package-global singleton (Codex #7).
func withTestTheme(t *testing.T, name string) {
	t.Helper()
	prevName := CurrentThemeName()
	prevTheme := CurrentTheme()
	SetThemeByName(name)
	t.Cleanup(func() {
		currentThemeName = prevName
		SetTheme(prevTheme)
	})
}

func withTestThemesDir(t *testing.T, dir string) {
	t.Helper()
	prev := themesDirFunc
	themesDirFunc = func() string { return dir }
	t.Cleanup(func() { themesDirFunc = prev })
}

func TestPresetInvariants(t *testing.T) {
	for _, name := range presetOrder {
		t.Run(name, func(t *testing.T) {
			th := PresetTheme(name)
			require.NotNil(t, th)

			// Waiting ≠ Starting (Warn)
			assert.NotEqual(t, th.Waiting, th.Warn, "Waiting must differ from Warn/Starting")

			// HealthyDot ≠ UnhealthyDot (OK ≠ Err) — colours
			assert.NotEqual(t, th.OK, th.Err, "HealthyDot/OK must differ from UnhealthyDot/Err")

			// Distinct glyphs are pinned in TestHealthDot; colours pairwise:
			assert.NotEqual(t, th.OK, th.Warn)
			assert.NotEqual(t, th.OK, th.Err)
			assert.NotEqual(t, th.Warn, th.Err)

			assert.NotEqual(t, th.SearchHitBG, th.SearchHitFG)

			for _, slot := range allColorSlots(th) {
				assert.NotEmpty(t, string(slot.color), "slot %s must be non-empty", slot.name)
			}
			assert.NotEmpty(t, th.ProcPalette, "ProcPalette must be non-empty")
			for i, c := range th.ProcPalette {
				assert.NotEmpty(t, string(c), "ProcPalette[%d] must be non-empty", i)
			}
		})
	}
}

type namedColor struct {
	name  string
	color lipgloss.Color
}

func allColorSlots(th *Theme) []namedColor {
	return []namedColor{
		{"BG", th.BG}, {"FG", th.FG}, {"Dim", th.Dim},
		{"Border", th.Border}, {"BorderFocused", th.BorderFocused}, {"Title", th.Title},
		{"HeaderBG", th.HeaderBG}, {"HeaderFG", th.HeaderFG},
		{"FooterBG", th.FooterBG}, {"FooterFG", th.FooterFG}, {"FooterKey", th.FooterKey},
		{"SelectionBG", th.SelectionBG}, {"SelectionFG", th.SelectionFG}, {"Cursor", th.Cursor},
		{"OK", th.OK}, {"Warn", th.Warn}, {"Err", th.Err}, {"Waiting", th.Waiting},
		{"HTTPSuccess", th.HTTPSuccess}, {"HTTPRedirect", th.HTTPRedirect},
		{"HTTPWarning", th.HTTPWarning}, {"HTTPError", th.HTTPError},
		{"LogError", th.LogError}, {"LogWarn", th.LogWarn}, {"LogInfo", th.LogInfo},
		{"LogDebug", th.LogDebug},
		{"JSONKey", th.JSONKey}, {"JSONString", th.JSONString},
		{"JSONNumber", th.JSONNumber}, {"JSONBool", th.JSONBool},
		{"SearchHitBG", th.SearchHitBG}, {"SearchHitFG", th.SearchHitFG},
		{"ErrBadgeFG", th.ErrBadgeFG}, {"ErrBadgeBG", th.ErrBadgeBG},
	}
}

func TestAliasFolding(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"mocha", "catppuccin"},
		{"TOKYO", "tokyo-night"},
		{"gruvbox_dark", "gruvbox"},
		{"tokyonight", "tokyo-night"},
		{"tokyo", "tokyo-night"},
		{"catppuccin-mocha", "catppuccin"},
		{"Catppuccin_Mocha", "catppuccin"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, presetCanonical(tc.in))
			c, th, w := ResolveTheme(tc.in)
			assert.Equal(t, tc.want, c)
			assert.NotNil(t, th)
			assert.Empty(t, w)
		})
	}
}

func TestResolveTheme_TOML(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)

	t.Run("base+override", func(t *testing.T) {
		path := filepath.Join(dir, "custom.toml")
		require.NoError(t, os.WriteFile(path, []byte(`
base = "dark"
[colors]
ok = "#112233"
waiting = "aabbcc"
`), 0o644))
		c, th, w := ResolveTheme("custom")
		assert.Equal(t, "custom", c)
		assert.Empty(t, w)
		assert.Equal(t, lipgloss.Color("#112233"), th.OK)
		assert.Equal(t, lipgloss.Color("#aabbcc"), th.Waiting)
		// Un-overridden slot keeps base
		assert.Equal(t, darkTheme().Dim, th.Dim)
	})

	t.Run("bad hex keeps base + warning", func(t *testing.T) {
		path := filepath.Join(dir, "badhex.toml")
		require.NoError(t, os.WriteFile(path, []byte(`
base = "dark"
[colors]
ok = "not-a-color"
`), 0o644))
		baseOK := darkTheme().OK
		c, th, w := ResolveTheme("badhex")
		assert.Equal(t, "badhex", c)
		assert.Equal(t, baseOK, th.OK)
		require.NotEmpty(t, w)
		assert.Contains(t, w[0], "invalid colour")
	})

	t.Run("malformed file resolves to default + warning", func(t *testing.T) {
		path := filepath.Join(dir, "broken.toml")
		require.NoError(t, os.WriteFile(path, []byte(`{{{{not toml`), 0o644))
		c, th, w := ResolveTheme("broken")
		assert.Equal(t, "tokyo-night", c)
		assert.Equal(t, tokyoNightTheme().OK, th.OK)
		require.NotEmpty(t, w)
	})

	t.Run("preset shadows user file", func(t *testing.T) {
		path := filepath.Join(dir, "dark.toml")
		require.NoError(t, os.WriteFile(path, []byte(`
base = "light"
[colors]
ok = "#0000ff"
`), 0o644))
		c, th, w := ResolveTheme("dark")
		assert.Equal(t, "dark", c)
		assert.Empty(t, w)
		assert.Equal(t, darkTheme().OK, th.OK, "preset must win over same-named user file")
		assert.NotEqual(t, lipgloss.Color("#0000ff"), th.OK)
	})

	t.Run("palette override", func(t *testing.T) {
		path := filepath.Join(dir, "pal.toml")
		require.NoError(t, os.WriteFile(path, []byte(`
base = "legacy"
palette = ["#ff0000", "#00ff00"]
`), 0o644))
		_, th, w := ResolveTheme("pal")
		assert.Empty(t, w)
		require.Len(t, th.ProcPalette, 2)
		assert.Equal(t, lipgloss.Color("#ff0000"), th.ProcPalette[0])
		assert.Equal(t, lipgloss.Color("#00ff00"), th.ProcPalette[1])
	})
}

func TestAvailableThemes_Order(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zebra.toml"), []byte(`base = "dark"`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha.toml"), []byte(`base = "dark"`), 0o644))
	// Shadowed by preset — must be omitted
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dark.toml"), []byte(`base = "light"`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mocha.toml"), []byte(`base = "light"`), 0o644))

	got := AvailableThemes()
	want := append(append([]string{}, presetOrder...), "alpha", "zebra")
	assert.Equal(t, want, got)
}

func TestCycleTheme_Wraps(t *testing.T) {
	dir := t.TempDir()
	withTestThemesDir(t, dir)

	names := AvailableThemes()
	require.Equal(t, presetOrder, names)

	// Cycle from last preset wraps to first
	c, th := CycleTheme(names[len(names)-1])
	assert.Equal(t, names[0], c)
	assert.NotNil(t, th)

	// Unknown current restarts at 0
	c, _ = CycleTheme("no-such-theme")
	assert.Equal(t, names[0], c)

	c, _ = CycleTheme(names[0])
	assert.Equal(t, names[1], c)
}

// Theme-mutating: must not call t.Parallel.
func TestSetTheme_RebuildsStyles(t *testing.T) {
	withTestTheme(t, "legacy")
	legacyOK := colorStr(s.Running.GetForeground())
	assert.Equal(t, "10", legacyOK)

	withTestTheme(t, "tokyo-night")
	assert.NotEqual(t, legacyOK, colorStr(s.Running.GetForeground()))
	assert.Equal(t, "tokyo-night", CurrentThemeName())
}

func TestDirectBaseModel_StylesNonNil(t *testing.T) {
	// Package init installed tokyo-night; constructing via newTestBaseModel
	// must not see nil/zero styles (panel S5).
	_ = newTestBaseModel()
	assert.NotEmpty(t, colorStr(s.Running.GetForeground()))
	assert.NotEmpty(t, s.ProcessColors)
}
