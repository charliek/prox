package tui

import "github.com/charmbracelet/lipgloss"

// styleSet holds every lipgloss style the TUI render path uses. Built wholesale
// from a Theme; never field-mutated after publish (see SetTheme).
type styleSet struct {
	// Base is FG+BG under FullFill themes; empty under legacy. Row builders wrap
	// every plain segment, separator, and padding run in Base (plan 023 B1).
	Base lipgloss.Style

	// HeaderSep / StatusSep paint the raw join separators between header chips
	// and status segments with the chrome BG under FullFill; empty under
	// legacy (no-op Render keeps byte identity).
	HeaderSep, StatusSep lipgloss.Style

	Running, Stopped, Crashed   lipgloss.Style
	Starting, Stopping, Waiting lipgloss.Style
	Blocked, Completed          lipgloss.Style
	DefaultProcess              lipgloss.Style
	Warn                        lipgloss.Style // generic ⚠ segments (stream health, paging)
	HealthyDot, UnhealthyDot    lipgloss.Style
	Header, Status, Help        lipgloss.Style
	Err, Dim                    lipgloss.Style
	FooterKey, FooterLabel      lipgloss.Style // two-tone footer hints (plan 023 B2)
	FooterError                 lipgloss.Style // ✗ error flash: Err-bold on FooterBG
	HTTPSuccess, HTTPRedirect   lipgloss.Style
	HTTPWarning, HTTPError      lipgloss.Style
	// Method / status class styles (plan 021 C9). Mapped from existing Theme
	// HTTP/OK/Warn/Err/FG slots — no new Theme TOML keys.
	HTTPGet, HTTPPost, HTTPPut, HTTPDelete, HTTPPatch lipgloss.Style
	Status2xx, Status4xx, Status5xx                   lipgloss.Style
	// Log level content tints + JSON syntax (C9). LogTrace stays unstyled
	// (default FG) so debug is the only dim level tint.
	LogError, LogWarn, LogInfo, LogDebug lipgloss.Style
	JSONKey, JSONString, JSONNumber      lipgloss.Style
	JSONBool, JSONNull                   lipgloss.Style
	Bold                                 lipgloss.Style // detail request line
	Cursor, SearchHighlight              lipgloss.Style
	ProcessColors                        []lipgloss.Style
}

// Package-global styles. The process hosts one TUI model (Codex #7), so a
// package-level set is sound. SetTheme publishes a fully-built styleSet in one
// assignment — never mutate fields in place. The TUI is single-goroutine;
// theme-mutating tests must not run in parallel (see withTestTheme).
var (
	s                styleSet
	currentTheme     *Theme
	currentThemeName string
)

func init() {
	// Install the default so no style is ever nil: tests construct models via
	// newTestBaseModel / newTestModel (panel S5) without any theme install.
	SetThemeByName("tokyo-night")
}

// SetTheme rebuilds and publishes every style from t in one assignment.
// Mid-session callers must also force a viewport re-render (cached styled
// strings); that wiring lands with the theme-cycle key in a later commit.
func SetTheme(t *Theme) {
	if t == nil {
		t = tokyoNightTheme()
	}
	currentTheme = t
	s = buildStyleSet(t)
}

// SetThemeByName resolves name and installs it. Returns the canonical name and
// any load warnings (for status-flash / log-pane notes in later commits).
func SetThemeByName(name string) (canonical string, warnings []string) {
	c, t, w := ResolveTheme(name)
	currentThemeName = c
	SetTheme(t)
	return c, w
}

// CurrentTheme returns the active Theme (never nil after init).
func CurrentTheme() *Theme { return currentTheme }

// CurrentThemeName returns the canonical name of the active theme.
func CurrentThemeName() string { return currentThemeName }

// fillBG adds Background(t.BG) when t.FullFill so FG-only segments still paint
// the theme surface (SGR-reset law). Styles with their own chrome BG skip this.
func fillBG(st lipgloss.Style, t *Theme) lipgloss.Style {
	if t.FullFill {
		return st.Background(t.BG)
	}
	return st
}

func buildStyleSet(t *Theme) styleSet {
	base := lipgloss.NewStyle()
	if t.FullFill {
		base = lipgloss.NewStyle().Foreground(t.FG).Background(t.BG)
	}

	header := lipgloss.NewStyle().
		Background(t.HeaderBG).
		Padding(0, 1)
	// legacy keeps MarginBottom for byte-identical escape output; FullFill
	// themes replace the margin with an explicitly Base-styled blank row in
	// mainView (margins use MarginBackground, not Background — plan 023 B1).
	if !t.FullFill {
		header = header.MarginBottom(1)
	}

	help := lipgloss.NewStyle().
		Background(t.BG).
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border)
	if t.FullFill {
		help = help.BorderBackground(t.BG)
	}

	headerSep := lipgloss.NewStyle()
	statusSep := lipgloss.NewStyle()
	if t.FullFill {
		headerSep = headerSep.Background(t.HeaderBG)
		statusSep = statusSep.Background(t.FooterBG)
	}

	ss := styleSet{
		Base:      base,
		HeaderSep: headerSep,
		StatusSep: statusSep,

		Running:  fillBG(lipgloss.NewStyle().Foreground(t.OK).Bold(true), t),
		Stopped:  fillBG(lipgloss.NewStyle().Foreground(t.Dim), t),
		Crashed:  fillBG(lipgloss.NewStyle().Foreground(t.Err).Bold(true), t),
		Starting: fillBG(lipgloss.NewStyle().Foreground(t.Warn), t),
		Stopping: fillBG(lipgloss.NewStyle().Foreground(t.Warn), t),
		// Gated-launch + task terminal states (plan 013 D5). Waiting is a
		// distinct amber; blocked shares the crashed red (bold); completed
		// rests gray like stopped.
		Waiting:   fillBG(lipgloss.NewStyle().Foreground(t.Waiting), t),
		Blocked:   fillBG(lipgloss.NewStyle().Foreground(t.Err).Bold(true), t),
		Completed: fillBG(lipgloss.NewStyle().Foreground(t.Dim), t),

		DefaultProcess: base,
		Warn:           fillBG(lipgloss.NewStyle().Foreground(t.Warn), t),

		// Health dots reuse OK/Err (plan 018 D13) so they stay consistent with
		// process-state colouring.
		HealthyDot:   fillBG(lipgloss.NewStyle().Foreground(t.OK), t),
		UnhealthyDot: fillBG(lipgloss.NewStyle().Foreground(t.Err), t),

		// Header/Status match pre-theme construction (BG + padding only — no
		// FG) so the legacy preset's escape codes stay byte-identical. Footer
		// text FG is applied per-segment in the footer renderer (plan 023 B2).
		Header: header,
		Status: lipgloss.NewStyle().
			Background(t.FooterBG).
			Padding(0, 1),
		Help: help,

		Err: lipgloss.NewStyle().
			Foreground(t.ErrBadgeFG).
			Background(t.ErrBadgeBG).
			Bold(true),
		Dim: fillBG(lipgloss.NewStyle().Foreground(t.Dim), t),

		FooterKey: lipgloss.NewStyle().
			Foreground(t.FooterKey).
			Background(t.FooterBG).
			Bold(true),
		FooterLabel: lipgloss.NewStyle().
			Foreground(t.FooterFG).
			Background(t.FooterBG),
		FooterError: lipgloss.NewStyle().
			Foreground(t.Err).
			Background(t.FooterBG).
			Bold(true),

		HTTPSuccess:  fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t),
		HTTPRedirect: fillBG(lipgloss.NewStyle().Foreground(t.HTTPRedirect), t),
		HTTPWarning:  fillBG(lipgloss.NewStyle().Foreground(t.HTTPWarning), t),
		HTTPError:    fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t),

		// Method colors (C9): GET=info-ish redirect cyan, POST=OK, PUT=waiting,
		// DELETE=err, PATCH=warn. Unknown methods stay default FG.
		HTTPGet:    fillBG(lipgloss.NewStyle().Foreground(t.HTTPRedirect), t),
		HTTPPost:   fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t),
		HTTPPut:    fillBG(lipgloss.NewStyle().Foreground(t.Waiting), t),
		HTTPDelete: fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t),
		HTTPPatch:  fillBG(lipgloss.NewStyle().Foreground(t.HTTPWarning), t),

		Status2xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t),
		Status4xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPWarning), t),
		Status5xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t),

		LogError: fillBG(lipgloss.NewStyle().Foreground(t.LogError), t),
		LogWarn:  fillBG(lipgloss.NewStyle().Foreground(t.LogWarn), t),
		LogInfo:  fillBG(lipgloss.NewStyle().Foreground(t.LogInfo), t),
		LogDebug: fillBG(lipgloss.NewStyle().Foreground(t.LogDebug), t),

		JSONKey:    fillBG(lipgloss.NewStyle().Foreground(t.JSONKey), t),
		JSONString: fillBG(lipgloss.NewStyle().Foreground(t.JSONString), t),
		JSONNumber: fillBG(lipgloss.NewStyle().Foreground(t.JSONNumber), t),
		JSONBool:   fillBG(lipgloss.NewStyle().Foreground(t.JSONBool), t),
		JSONNull:   fillBG(lipgloss.NewStyle().Foreground(t.Dim), t), // Theme has no JSONNull slot

		Bold: fillBG(lipgloss.NewStyle().Foreground(t.FG).Bold(true), t),

		// Cursor styles only the "❯ " marker, never the whole row: each row is a
		// concatenation of individually styled segments whose ANSI resets would
		// terminate an outer attribute mid-line (D10).
		Cursor: fillBG(lipgloss.NewStyle().Foreground(t.Cursor).Bold(true), t),
		// SearchHighlight applies only to the exact matched run of a `/`-search
		// hit, and only when query and line are plain ASCII with no ESC byte —
		// otherwise case-folding could shift byte offsets or the run could land
		// inside an ANSI escape, so formatLogEntry falls back to the row marker
		// alone (isASCIINoESC, D9).
		SearchHighlight: lipgloss.NewStyle().
			Foreground(t.SearchHitFG).
			Background(t.SearchHitBG).
			Bold(true),
	}

	ss.ProcessColors = make([]lipgloss.Style, 0, len(t.ProcPalette))
	for _, c := range t.ProcPalette {
		ss.ProcessColors = append(ss.ProcessColors, fillBG(lipgloss.NewStyle().Foreground(c), t))
	}
	return ss
}
