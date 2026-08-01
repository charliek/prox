package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

// Theme holds every semantic colour the TUI renderer uses. Presets and user
// TOML files fill this wholesale; styles.go derives lipgloss styles from it.
// Values are lipgloss.Color so a slot may be an RGB hex ("#1a1b26") or an
// ANSI-256 index ("10") — the legacy preset uses the latter.
type Theme struct {
	// Chrome
	BG, FG, Dim              lipgloss.Color
	Border, BorderFocused    lipgloss.Color
	Title                    lipgloss.Color
	HeaderBG, HeaderFG       lipgloss.Color
	FooterBG, FooterFG       lipgloss.Color
	FooterKey                lipgloss.Color
	SelectionBG, SelectionFG lipgloss.Color
	Cursor                   lipgloss.Color

	// Feedback — process states map here (running→OK, starting/stopping→Warn,
	// crashed/blocked→Err, waiting→Waiting, stopped/completed→Dim). Health
	// dots reuse OK/Err.
	OK, Warn, Err, Waiting lipgloss.Color

	// HTTP — HTTPInFlight reuses Dim (no separate slot).
	HTTPSuccess, HTTPRedirect, HTTPWarning, HTTPError lipgloss.Color

	// Logs / JSON / search / stderr badge
	LogError, LogWarn, LogInfo, LogDebug, LogTrace lipgloss.Color
	JSONKey, JSONString, JSONNumber, JSONBool      lipgloss.Color
	SearchHitBG, SearchHitFG                       lipgloss.Color
	ErrBadgeFG, ErrBadgeBG                         lipgloss.Color

	// ProcPalette cycles process-name colours (9 entries in every preset).
	ProcPalette []lipgloss.Color
}

// Canonical preset names in cycle order. tokyo-night is the default.
var presetOrder = []string{
	"tokyo-night", "dark", "light", "catppuccin", "gruvbox", "legacy",
}

// themesDirFunc returns the user themes directory. Overridable in tests
// (default: ~/.prox/tui/themes — prox's established user dir, not XDG;
// panel Codex #9).
var themesDirFunc = defaultThemesDir

func defaultThemesDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".prox", "tui", "themes")
}

func rgb(r, g, b uint8) lipgloss.Color {
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b))
}

// normalize folds a theme name the way strix does: lowercase, `_` → `-`.
func normalizeThemeName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
}

// presetCanonical returns the canonical preset name for name, or "" if none.
// Shared by PresetTheme and ResolveTheme so aliases fold identically.
func presetCanonical(name string) string {
	switch normalizeThemeName(name) {
	case "tokyo-night", "tokyonight", "tokyo":
		return "tokyo-night"
	case "dark":
		return "dark"
	case "light":
		return "light"
	case "catppuccin", "catppuccin-mocha", "mocha":
		return "catppuccin"
	case "gruvbox", "gruvbox-dark":
		return "gruvbox"
	case "legacy":
		return "legacy"
	default:
		return ""
	}
}

// PresetTheme returns a built-in theme by name (aliases folded), or nil.
func PresetTheme(name string) *Theme {
	canonical := presetCanonical(name)
	if canonical == "" {
		return nil
	}
	return themeForCanonical(canonical)
}

func themeForCanonical(canonical string) *Theme {
	switch canonical {
	case "tokyo-night":
		return tokyoNightTheme()
	case "dark":
		return darkTheme()
	case "light":
		return lightTheme()
	case "catppuccin":
		return catppuccinTheme()
	case "gruvbox":
		return gruvboxTheme()
	case "legacy":
		return legacyTheme()
	default:
		return tokyoNightTheme()
	}
}

// ResolveTheme loads name as a preset (aliases folded) or a user TOML under
// ~/.prox/tui/themes/. Presets shadow same-named user files. Unknown names and
// malformed files resolve to tokyo-night with a warning. Returns the canonical
// name actually loaded (never the requested alias/stem when falling back).
func ResolveTheme(name string) (canonical string, theme *Theme, warnings []string) {
	if c := presetCanonical(name); c != "" {
		return c, themeForCanonical(c), nil
	}

	dir := themesDirFunc()
	if dir == "" {
		return "tokyo-night", tokyoNightTheme(), []string{
			fmt.Sprintf("unknown theme %q; using tokyo-night", name),
		}
	}
	path := filepath.Join(dir, name+".toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "tokyo-night", tokyoNightTheme(), []string{
			fmt.Sprintf("theme %q not found (%v); using tokyo-night", name, err),
		}
	}
	t, warns, ok := themeFromTOML(string(data))
	if !ok {
		return "tokyo-night", tokyoNightTheme(), append(warns,
			fmt.Sprintf("invalid theme file %q; using tokyo-night", path))
	}
	return name, t, warns
}

// AvailableThemes returns presets in canonical order, then user theme stems
// sorted lexically (rescanned every call). Stems that shadow a preset/alias
// are omitted — resolve would load the preset, never the file.
func AvailableThemes() []string {
	names := make([]string, len(presetOrder))
	copy(names, presetOrder)

	dir := themesDirFunc()
	if dir == "" {
		return names
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names
	}
	var user []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		stem := strings.TrimSuffix(name, ".toml")
		if presetCanonical(stem) != "" {
			continue
		}
		user = append(user, stem)
	}
	sort.Strings(user)
	return append(names, user...)
}

// nextThemeName returns the name after current in AvailableThemes, wrapping.
// A current no longer present restarts at index 0.
func nextThemeName(current string) string {
	names := AvailableThemes()
	next := 0
	for i, n := range names {
		if n == current {
			next = (i + 1) % len(names)
			break
		}
	}
	return names[next]
}

// CycleTheme returns the theme after current in AvailableThemes, wrapping.
// A current no longer present restarts at index 0.
func CycleTheme(current string) (string, *Theme) {
	c, t, _ := ResolveTheme(nextThemeName(current))
	return c, t
}

// --- presets ----------------------------------------------------------------

func tokyoNightTheme() *Theme {
	ok := rgb(158, 206, 106)
	// Warn (starting) is a bright yellow; Waiting is the warmer amber — must
	// stay distinct (panel S5). Amber matches strix unstaged / user brief.
	warn := rgb(230, 194, 72)
	waiting := rgb(224, 175, 104)
	errc := rgb(247, 118, 142)
	info := rgb(122, 162, 247)
	dim := rgb(86, 95, 137)
	cyan := rgb(125, 207, 255)
	purple := rgb(187, 154, 247)
	return &Theme{
		BG: rgb(26, 27, 38), FG: rgb(169, 177, 214), Dim: dim,
		Border: rgb(41, 46, 66), BorderFocused: info, Title: info,
		HeaderBG: rgb(22, 22, 30), HeaderFG: rgb(192, 202, 245),
		FooterBG: rgb(22, 22, 30), FooterFG: dim, FooterKey: info,
		SelectionBG: rgb(40, 52, 87), SelectionFG: rgb(192, 202, 245),
		Cursor: purple,
		OK:     ok, Warn: warn, Err: errc, Waiting: waiting,
		HTTPSuccess: ok, HTTPRedirect: cyan, HTTPWarning: warn, HTTPError: errc,
		LogError: errc, LogWarn: waiting, LogInfo: info, LogDebug: dim, LogTrace: rgb(60, 67, 99),
		JSONKey: purple, JSONString: ok, JSONNumber: waiting, JSONBool: cyan,
		SearchHitBG: warn, SearchHitFG: rgb(26, 27, 38),
		ErrBadgeFG: rgb(192, 202, 245), ErrBadgeBG: errc,
		ProcPalette: []lipgloss.Color{
			info, ok, waiting, purple, cyan, errc,
			rgb(115, 218, 202), rgb(255, 158, 100), rgb(192, 202, 245),
		},
	}
}

func darkTheme() *Theme {
	ok := rgb(135, 175, 95)
	warn := rgb(230, 200, 80)
	waiting := rgb(215, 175, 95) // strix unstaged amber
	errc := rgb(215, 95, 95)
	info := rgb(95, 135, 215)
	dim := rgb(128, 128, 128)
	cyan := rgb(95, 175, 215)
	purple := rgb(175, 135, 215)
	return &Theme{
		BG: rgb(28, 28, 28), FG: rgb(208, 208, 208), Dim: dim,
		Border: rgb(58, 58, 58), BorderFocused: info, Title: info,
		HeaderBG: rgb(18, 18, 18), HeaderFG: rgb(228, 228, 228),
		FooterBG: rgb(18, 18, 18), FooterFG: dim, FooterKey: info,
		SelectionBG: rgb(48, 48, 48), SelectionFG: rgb(228, 228, 228),
		Cursor: purple,
		OK:     ok, Warn: warn, Err: errc, Waiting: waiting,
		HTTPSuccess: ok, HTTPRedirect: cyan, HTTPWarning: warn, HTTPError: errc,
		LogError: errc, LogWarn: waiting, LogInfo: info, LogDebug: dim, LogTrace: rgb(88, 88, 88),
		JSONKey: purple, JSONString: ok, JSONNumber: waiting, JSONBool: cyan,
		SearchHitBG: warn, SearchHitFG: rgb(28, 28, 28),
		ErrBadgeFG: rgb(228, 228, 228), ErrBadgeBG: errc,
		ProcPalette: []lipgloss.Color{
			info, ok, waiting, purple, cyan, errc,
			rgb(95, 215, 175), rgb(215, 135, 95), rgb(228, 228, 228),
		},
	}
}

func lightTheme() *Theme {
	ok := rgb(80, 161, 79)
	warn := rgb(200, 150, 20)
	waiting := rgb(193, 132, 1) // strix unstaged
	errc := rgb(228, 86, 73)
	info := rgb(64, 120, 242)
	dim := rgb(160, 161, 167)
	cyan := rgb(1, 132, 188)
	purple := rgb(166, 38, 164)
	return &Theme{
		BG: rgb(250, 250, 250), FG: rgb(56, 58, 66), Dim: dim,
		Border: rgb(212, 212, 212), BorderFocused: info, Title: info,
		HeaderBG: rgb(234, 234, 235), HeaderFG: rgb(56, 58, 66),
		FooterBG: rgb(234, 234, 235), FooterFG: dim, FooterKey: info,
		SelectionBG: rgb(208, 215, 230), SelectionFG: rgb(56, 58, 66),
		Cursor: purple,
		OK:     ok, Warn: warn, Err: errc, Waiting: waiting,
		HTTPSuccess: ok, HTTPRedirect: cyan, HTTPWarning: warn, HTTPError: errc,
		LogError: errc, LogWarn: waiting, LogInfo: info, LogDebug: dim, LogTrace: rgb(192, 192, 192),
		JSONKey: purple, JSONString: ok, JSONNumber: waiting, JSONBool: cyan,
		SearchHitBG: warn, SearchHitFG: rgb(250, 250, 250),
		ErrBadgeFG: rgb(250, 250, 250), ErrBadgeBG: errc,
		ProcPalette: []lipgloss.Color{
			info, ok, waiting, purple, cyan, errc,
			rgb(8, 151, 156), rgb(210, 105, 30), rgb(56, 58, 66),
		},
	}
}

func catppuccinTheme() *Theme {
	ok := rgb(166, 227, 161)
	warn := rgb(249, 226, 175)    // bright yellow (strix unstaged)
	waiting := rgb(245, 194, 131) // peach — distinct from starting yellow
	errc := rgb(243, 139, 168)
	info := rgb(137, 180, 250)
	dim := rgb(108, 112, 134)
	cyan := rgb(137, 220, 235)
	purple := rgb(203, 166, 247)
	return &Theme{
		BG: rgb(30, 30, 46), FG: rgb(205, 214, 244), Dim: dim,
		Border: rgb(49, 50, 68), BorderFocused: info, Title: info,
		HeaderBG: rgb(24, 24, 37), HeaderFG: rgb(205, 214, 244),
		FooterBG: rgb(24, 24, 37), FooterFG: dim, FooterKey: info,
		SelectionBG: rgb(49, 50, 68), SelectionFG: rgb(205, 214, 244),
		Cursor: purple,
		OK:     ok, Warn: warn, Err: errc, Waiting: waiting,
		HTTPSuccess: ok, HTTPRedirect: cyan, HTTPWarning: warn, HTTPError: errc,
		LogError: errc, LogWarn: waiting, LogInfo: info, LogDebug: dim, LogTrace: rgb(69, 71, 90),
		JSONKey: purple, JSONString: ok, JSONNumber: waiting, JSONBool: cyan,
		SearchHitBG: warn, SearchHitFG: rgb(30, 30, 46),
		ErrBadgeFG: rgb(205, 214, 244), ErrBadgeBG: errc,
		ProcPalette: []lipgloss.Color{
			info, ok, waiting, purple, cyan, errc,
			rgb(148, 226, 213), rgb(250, 179, 135), rgb(205, 214, 244),
		},
	}
}

func gruvboxTheme() *Theme {
	ok := rgb(184, 187, 38)
	warn := rgb(250, 189, 47)    // bright gold (starting)
	waiting := rgb(254, 128, 25) // orange — distinct from starting gold
	errc := rgb(251, 73, 52)
	dim := rgb(146, 131, 116)
	aqua := rgb(131, 165, 152)
	purple := rgb(211, 134, 155)
	green := rgb(142, 192, 124)
	return &Theme{
		BG: rgb(40, 40, 40), FG: rgb(235, 219, 178), Dim: dim,
		Border: rgb(60, 56, 54), BorderFocused: warn, Title: warn,
		HeaderBG: rgb(29, 32, 33), HeaderFG: rgb(235, 219, 178),
		FooterBG: rgb(29, 32, 33), FooterFG: dim, FooterKey: warn,
		SelectionBG: rgb(60, 56, 54), SelectionFG: rgb(235, 219, 178),
		Cursor: purple,
		OK:     ok, Warn: warn, Err: errc, Waiting: waiting,
		HTTPSuccess: ok, HTTPRedirect: aqua, HTTPWarning: warn, HTTPError: errc,
		LogError: errc, LogWarn: waiting, LogInfo: aqua, LogDebug: dim, LogTrace: rgb(80, 73, 69),
		JSONKey: purple, JSONString: ok, JSONNumber: waiting, JSONBool: aqua,
		SearchHitBG: warn, SearchHitFG: rgb(40, 40, 40),
		ErrBadgeFG: rgb(235, 219, 178), ErrBadgeBG: errc,
		ProcPalette: []lipgloss.Color{
			aqua, ok, waiting, purple, green, errc,
			rgb(69, 133, 136), rgb(214, 93, 14), rgb(235, 219, 178),
		},
	}
}

// legacyTheme approximates the pre-theme ANSI-256 look from styles.go so
// escape-code output matches the old hardcoded palette.
func legacyTheme() *Theme {
	return &Theme{
		// BG is the help-overlay background (pre-theme helpBg "234"); chrome
		// panels use HeaderBG/FooterBG. Truecolor presets use a unified BG.
		BG: lipgloss.Color("234"), FG: lipgloss.Color("7"), Dim: lipgloss.Color("8"),
		Border: lipgloss.Color("240"), BorderFocused: lipgloss.Color("12"),
		Title:    lipgloss.Color("14"),
		HeaderBG: lipgloss.Color("235"), HeaderFG: lipgloss.Color("7"),
		FooterBG: lipgloss.Color("236"), FooterFG: lipgloss.Color("7"),
		FooterKey:   lipgloss.Color("14"),
		SelectionBG: lipgloss.Color("237"), SelectionFG: lipgloss.Color("15"),
		Cursor: lipgloss.Color("13"),
		OK:     lipgloss.Color("10"), Warn: lipgloss.Color("11"),
		Err: lipgloss.Color("9"), Waiting: lipgloss.Color("214"),
		HTTPSuccess: lipgloss.Color("10"), HTTPRedirect: lipgloss.Color("14"),
		HTTPWarning: lipgloss.Color("11"), HTTPError: lipgloss.Color("9"),
		LogError: lipgloss.Color("9"), LogWarn: lipgloss.Color("11"),
		LogInfo: lipgloss.Color("12"), LogDebug: lipgloss.Color("8"), LogTrace: lipgloss.Color("240"),
		JSONKey: lipgloss.Color("13"), JSONString: lipgloss.Color("10"),
		JSONNumber: lipgloss.Color("11"), JSONBool: lipgloss.Color("12"),
		SearchHitBG: lipgloss.Color("11"), SearchHitFG: lipgloss.Color("0"),
		ErrBadgeFG: lipgloss.Color("15"), ErrBadgeBG: lipgloss.Color("9"),
		ProcPalette: []lipgloss.Color{
			lipgloss.Color("14"),
			lipgloss.Color("13"),
			lipgloss.Color("12"),
			lipgloss.Color("11"),
			lipgloss.Color("10"),
			lipgloss.Color("208"),
			lipgloss.Color("207"),
			lipgloss.Color("159"),
			lipgloss.Color("156"),
		},
	}
}

// --- user TOML --------------------------------------------------------------

type themeFile struct {
	Base    string            `toml:"base"`
	Colors  map[string]string `toml:"colors"`
	Palette []string          `toml:"palette"`
}

// themeFromTOML parses a user theme file. ok=false on TOML parse error.
// Unknown/invalid colour values keep the base and append a warning.
func themeFromTOML(text string) (theme *Theme, warnings []string, ok bool) {
	var file themeFile
	if _, err := toml.Decode(text, &file); err != nil {
		return nil, []string{fmt.Sprintf("theme TOML parse error: %v", err)}, false
	}
	baseName := file.Base
	if baseName == "" {
		baseName = "tokyo-night"
	}
	base := PresetTheme(baseName)
	if base == nil {
		warnings = append(warnings, fmt.Sprintf("unknown base %q; using tokyo-night", baseName))
		base = tokyoNightTheme()
	} else {
		// Copy so overrides don't mutate the shared preset.
		cp := *base
		base = &cp
		if base.ProcPalette != nil {
			base.ProcPalette = append([]lipgloss.Color(nil), base.ProcPalette...)
		}
	}

	for key, val := range file.Colors {
		slot := colorSlot(base, key)
		if slot == nil {
			warnings = append(warnings, fmt.Sprintf("unknown colour slot %q; ignored", key))
			continue
		}
		c, perr := parseHexColor(val)
		if perr != "" {
			warnings = append(warnings, perr)
			continue
		}
		*slot = c
	}

	if file.Palette != nil {
		var palette []lipgloss.Color
		for i, hex := range file.Palette {
			c, perr := parseHexColor(hex)
			if perr != "" {
				warnings = append(warnings, fmt.Sprintf("palette[%d]: %s", i, perr))
				continue
			}
			palette = append(palette, c)
		}
		if len(palette) > 0 {
			base.ProcPalette = palette
		}
	}
	return base, warnings, true
}

func parseHexColor(value string) (lipgloss.Color, string) {
	hex := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(hex) != 6 {
		return "", fmt.Sprintf("invalid colour %q; ignored", value)
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return "", fmt.Sprintf("invalid colour %q; ignored", value)
	}
	return rgb(uint8(n>>16), uint8(n>>8), uint8(n)), ""
}

// colorSlot returns a pointer to the Theme field named by snake_case key.
func colorSlot(t *Theme, key string) *lipgloss.Color {
	switch key {
	case "bg":
		return &t.BG
	case "fg":
		return &t.FG
	case "dim":
		return &t.Dim
	case "border":
		return &t.Border
	case "border_focused":
		return &t.BorderFocused
	case "title":
		return &t.Title
	case "header_bg":
		return &t.HeaderBG
	case "header_fg":
		return &t.HeaderFG
	case "footer_bg":
		return &t.FooterBG
	case "footer_fg":
		return &t.FooterFG
	case "footer_key":
		return &t.FooterKey
	case "selection_bg":
		return &t.SelectionBG
	case "selection_fg":
		return &t.SelectionFG
	case "cursor":
		return &t.Cursor
	case "ok":
		return &t.OK
	case "warn":
		return &t.Warn
	case "err":
		return &t.Err
	case "waiting":
		return &t.Waiting
	case "http_success":
		return &t.HTTPSuccess
	case "http_redirect":
		return &t.HTTPRedirect
	case "http_warning":
		return &t.HTTPWarning
	case "http_error":
		return &t.HTTPError
	case "log_error":
		return &t.LogError
	case "log_warn":
		return &t.LogWarn
	case "log_info":
		return &t.LogInfo
	case "log_debug":
		return &t.LogDebug
	case "log_trace":
		return &t.LogTrace
	case "json_key":
		return &t.JSONKey
	case "json_string":
		return &t.JSONString
	case "json_number":
		return &t.JSONNumber
	case "json_bool":
		return &t.JSONBool
	case "search_hit_bg":
		return &t.SearchHitBG
	case "search_hit_fg":
		return &t.SearchHitFG
	case "err_badge_fg":
		return &t.ErrBadgeFG
	case "err_badge_bg":
		return &t.ErrBadgeBG
	default:
		return nil
	}
}
