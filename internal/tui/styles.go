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
	// DetailTitle is the request-detail "Request: <id>" line and section
	// headings (plan 023 B6) — t.Title+bold on theme BG, not the Header band.
	DetailTitle lipgloss.Style
	// HelpBorder / HelpTitle / HelpSection paint the help modal border chars,
	// the title spliced into the top border, and section headings (plan 023 B5).
	HelpBorder, HelpTitle, HelpSection lipgloss.Style
	HelpKey                            lipgloss.Style // FooterKey color on modal BG
	Err, Dim                           lipgloss.Style
	FooterKey, FooterLabel             lipgloss.Style // two-tone footer hints (plan 023 B2)
	FooterError                        lipgloss.Style // ✗ error flash: Err-bold on FooterBG
	// Panel / PanelTitle paint the viewport panel border chars and the title
	// spliced into the top border (plan 023 E2). Manual composition — lipgloss
	// has no native border-title. FullFill gets t.BG; legacy is FG-only so
	// C5 byte-identity pins stay green (panel is new chrome under legacy).
	Panel, PanelTitle      lipgloss.Style
	HTTPSuccess, HTTPError lipgloss.Style
	// Method / status class styles (plan 021 C9). Mapped from existing Theme
	// HTTP/OK/Warn/Err/FG slots — no new Theme TOML keys.
	HTTPGet, HTTPPost, HTTPPut, HTTPDelete, HTTPPatch lipgloss.Style
	Status2xx, Status4xx, Status5xx                   lipgloss.Style
	// Log level content tints + JSON syntax (C9). Trace stays unstyled
	// (default FG) so debug is the only dim level tint.
	LogError, LogWarn, LogInfo, LogDebug lipgloss.Style
	JSONKey, JSONString, JSONNumber      lipgloss.Style
	JSONBool, JSONNull                   lipgloss.Style
	Bold                                 lipgloss.Style // detail request line
	Cursor, SearchHighlight              lipgloss.Style
	// Selection paints FullFill cursor-row padding (plan 023 E1). Segment BGs
	// on the band come from sel (SelectionBG fill); SearchHighlight keeps
	// SearchHitBG so search hits win over the band.
	Selection     lipgloss.Style
	ProcessColors []lipgloss.Style
}

// Package-global styles. The process hosts one TUI model (Codex #7), so a
// package-level set is sound. SetTheme publishes a fully-built styleSet in one
// assignment — never mutate fields in place. The TUI is single-goroutine;
// theme-mutating tests must not run in parallel (see withTestTheme).
var (
	s                  styleSet
	sel                styleSet // FullFill cursor-row styles (SelectionBG fill)
	selectionRowActive bool     // true while formatters run under sel
	currentTheme       *Theme
	currentThemeName   string
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
	if t.FullFill {
		sel = buildSelectionStyleSet(t)
	} else {
		sel = styleSet{} // legacy: band off; marker-only cursor (C5 byte-identity)
	}
}

// withSelectionStyles runs fn with s swapped to the SelectionBG fill set when
// selected under a FullFill theme. Legacy and non-selected rows keep s as-is
// so escape output stays byte-identical to pre-band rendering.
func withSelectionStyles(selected bool, fn func()) {
	if !selected || currentTheme == nil || !currentTheme.FullFill {
		fn()
		return
	}
	prev, prevFlag := s, selectionRowActive
	s, selectionRowActive = sel, true
	fn()
	s, selectionRowActive = prev, prevFlag
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

// fillBG adds Background(surface) when t.FullFill so FG-only segments still
// paint a theme surface (SGR-reset law). Styles with their own chrome BG skip
// this. surface is t.BG for normal rows and t.SelectionBG for the cursor band.
func fillBG(st lipgloss.Style, t *Theme, surface lipgloss.Color) lipgloss.Style {
	if t.FullFill {
		return st.Background(surface)
	}
	return st
}

func buildStyleSet(t *Theme) styleSet {
	return buildStyleSetFill(t, t.BG, false)
}

// buildSelectionStyleSet rebuilds row styles with SelectionBG fill so cursor
// rows paint a full-width band (plan 023 E1). SearchHighlight keeps SearchHitBG.
func buildSelectionStyleSet(t *Theme) styleSet {
	return buildStyleSetFill(t, t.SelectionBG, true)
}

func buildStyleSetFill(t *Theme, surface lipgloss.Color, selectionBand bool) styleSet {
	base := lipgloss.NewStyle()
	if t.FullFill {
		base = lipgloss.NewStyle().Foreground(t.FG).Background(surface)
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
		BorderForeground(t.BorderFocused)
	if t.FullFill {
		help = help.BorderBackground(t.BG)
	}

	headerSep := lipgloss.NewStyle()
	statusSep := lipgloss.NewStyle()
	if t.FullFill {
		headerSep = headerSep.Background(t.HeaderBG)
		statusSep = statusSep.Background(t.FooterBG)
	}

	errBG := t.ErrBadgeBG
	if selectionBand {
		errBG = t.SelectionBG
	}

	ss := styleSet{
		Base:      base,
		HeaderSep: headerSep,
		StatusSep: statusSep,

		Running:  fillBG(lipgloss.NewStyle().Foreground(t.OK).Bold(true), t, surface),
		Stopped:  fillBG(lipgloss.NewStyle().Foreground(t.Dim), t, surface),
		Crashed:  fillBG(lipgloss.NewStyle().Foreground(t.Err).Bold(true), t, surface),
		Starting: fillBG(lipgloss.NewStyle().Foreground(t.Warn), t, surface),
		Stopping: fillBG(lipgloss.NewStyle().Foreground(t.Warn), t, surface),
		// Gated-launch + task terminal states (plan 013 D5). Waiting is a
		// distinct amber; blocked shares the crashed red (bold); completed
		// rests gray like stopped.
		Waiting:   fillBG(lipgloss.NewStyle().Foreground(t.Waiting), t, surface),
		Blocked:   fillBG(lipgloss.NewStyle().Foreground(t.Err).Bold(true), t, surface),
		Completed: fillBG(lipgloss.NewStyle().Foreground(t.Dim), t, surface),

		DefaultProcess: base,
		Warn:           fillBG(lipgloss.NewStyle().Foreground(t.Warn), t, surface),

		// Health dots reuse OK/Err (plan 018 D13) so they stay consistent with
		// process-state colouring.
		HealthyDot:   fillBG(lipgloss.NewStyle().Foreground(t.OK), t, surface),
		UnhealthyDot: fillBG(lipgloss.NewStyle().Foreground(t.Err), t, surface),

		// Header/Status match pre-theme construction (BG + padding only — no
		// FG) so the legacy preset's escape codes stay byte-identical. Footer
		// text FG is applied per-segment in the footer renderer (plan 023 B2).
		Header: header,
		Status: lipgloss.NewStyle().
			Background(t.FooterBG).
			Padding(0, 1),
		DetailTitle: fillBG(lipgloss.NewStyle().Foreground(t.Title).Bold(true), t, surface),
		Help:        help,
		HelpBorder:  fillBG(lipgloss.NewStyle().Foreground(t.BorderFocused), t, surface),
		HelpTitle:   fillBG(lipgloss.NewStyle().Foreground(t.Title).Bold(true), t, surface),
		HelpSection: fillBG(lipgloss.NewStyle().Foreground(t.Title).Bold(true), t, surface),
		HelpKey:     fillBG(lipgloss.NewStyle().Foreground(t.FooterKey).Bold(true), t, surface),

		// Panel border chars (manual rounded frame around the viewport).
		// Not a lipgloss.Border style — renderers paint ╭─╮│╰╯ directly.
		Panel:      fillBG(lipgloss.NewStyle().Foreground(t.Border), t, surface),
		PanelTitle: fillBG(lipgloss.NewStyle().Foreground(t.Title).Bold(true), t, surface),

		Err: lipgloss.NewStyle().
			Foreground(t.ErrBadgeFG).
			Background(errBG).
			Bold(true),
		Dim: fillBG(lipgloss.NewStyle().Foreground(t.Dim), t, surface),

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

		HTTPSuccess: fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t, surface),
		HTTPError:   fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t, surface),

		// Method colors (C9): GET=info-ish redirect cyan, POST=OK, PUT=waiting,
		// DELETE=err, PATCH=warn. Unknown methods stay default FG.
		HTTPGet:    fillBG(lipgloss.NewStyle().Foreground(t.HTTPRedirect), t, surface),
		HTTPPost:   fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t, surface),
		HTTPPut:    fillBG(lipgloss.NewStyle().Foreground(t.Waiting), t, surface),
		HTTPDelete: fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t, surface),
		HTTPPatch:  fillBG(lipgloss.NewStyle().Foreground(t.HTTPWarning), t, surface),

		Status2xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPSuccess), t, surface),
		Status4xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPWarning), t, surface),
		Status5xx: fillBG(lipgloss.NewStyle().Foreground(t.HTTPError), t, surface),

		LogError: fillBG(lipgloss.NewStyle().Foreground(t.LogError), t, surface),
		LogWarn:  fillBG(lipgloss.NewStyle().Foreground(t.LogWarn), t, surface),
		LogInfo:  fillBG(lipgloss.NewStyle().Foreground(t.LogInfo), t, surface),
		LogDebug: fillBG(lipgloss.NewStyle().Foreground(t.LogDebug), t, surface),

		JSONKey:    fillBG(lipgloss.NewStyle().Foreground(t.JSONKey), t, surface),
		JSONString: fillBG(lipgloss.NewStyle().Foreground(t.JSONString), t, surface),
		JSONNumber: fillBG(lipgloss.NewStyle().Foreground(t.JSONNumber), t, surface),
		JSONBool:   fillBG(lipgloss.NewStyle().Foreground(t.JSONBool), t, surface),
		JSONNull:   fillBG(lipgloss.NewStyle().Foreground(t.Dim), t, surface), // Theme has no JSONNull slot

		Bold: fillBG(lipgloss.NewStyle().Foreground(t.FG).Bold(true), t, surface),

		// Cursor styles the "❯ " marker as its own segment (D10). Under the
		// selection band it also carries SelectionBG so the marker cell is
		// part of the full-width band (plan 023 E1).
		Cursor: fillBG(lipgloss.NewStyle().Foreground(t.Cursor).Bold(true), t, surface),
		// SearchHighlight applies only to the exact matched run of a `/`-search
		// hit, and only when query and line are plain ASCII with no ESC byte —
		// otherwise case-folding could shift byte offsets or the run could land
		// inside an ANSI escape, so formatLogEntry falls back to the row marker
		// alone (isASCIINoESC, D9). Always SearchHitBG — wins over SelectionBG.
		SearchHighlight: lipgloss.NewStyle().
			Foreground(t.SearchHitFG).
			Background(t.SearchHitBG).
			Bold(true),

		Selection: lipgloss.NewStyle().
			Foreground(t.SelectionFG).
			Background(t.SelectionBG),
	}

	ss.ProcessColors = make([]lipgloss.Style, 0, len(t.ProcPalette))
	for _, c := range t.ProcPalette {
		ss.ProcessColors = append(ss.ProcessColors, fillBG(lipgloss.NewStyle().Foreground(c), t, surface))
	}
	return ss
}
