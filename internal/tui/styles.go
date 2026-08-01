package tui

import "github.com/charmbracelet/lipgloss"

// styleSet holds every lipgloss style the TUI render path uses. Built wholesale
// from a Theme; never field-mutated after publish (see SetTheme).
type styleSet struct {
	Running, Stopped, Crashed   lipgloss.Style
	Starting, Stopping, Waiting lipgloss.Style
	Blocked, Completed          lipgloss.Style
	DefaultProcess              lipgloss.Style
	Warn                        lipgloss.Style // generic ⚠ segments (stream health, paging)
	HealthyDot, UnhealthyDot    lipgloss.Style
	Header, Status, Help        lipgloss.Style
	Err, Dim                    lipgloss.Style
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
	// Install the default so no style is ever nil: tests construct &BaseModel{}
	// directly (panel S5) without any theme install.
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

func buildStyleSet(t *Theme) styleSet {
	ss := styleSet{
		Running:  lipgloss.NewStyle().Foreground(t.OK).Bold(true),
		Stopped:  lipgloss.NewStyle().Foreground(t.Dim),
		Crashed:  lipgloss.NewStyle().Foreground(t.Err).Bold(true),
		Starting: lipgloss.NewStyle().Foreground(t.Warn),
		Stopping: lipgloss.NewStyle().Foreground(t.Warn),
		// Gated-launch + task terminal states (plan 013 D5). Waiting is a
		// distinct amber; blocked shares the crashed red (bold); completed
		// rests gray like stopped.
		Waiting:   lipgloss.NewStyle().Foreground(t.Waiting),
		Blocked:   lipgloss.NewStyle().Foreground(t.Err).Bold(true),
		Completed: lipgloss.NewStyle().Foreground(t.Dim),

		DefaultProcess: lipgloss.NewStyle(),
		Warn:           lipgloss.NewStyle().Foreground(t.Warn),

		// Health dots reuse OK/Err (plan 018 D13) so they stay consistent with
		// process-state colouring.
		HealthyDot:   lipgloss.NewStyle().Foreground(t.OK),
		UnhealthyDot: lipgloss.NewStyle().Foreground(t.Err),

		// Header/Status match pre-theme construction (BG + padding only — no
		// FG) so the legacy preset's escape codes stay byte-identical.
		Header: lipgloss.NewStyle().
			Background(t.HeaderBG).
			Padding(0, 1).
			MarginBottom(1),
		Status: lipgloss.NewStyle().
			Background(t.FooterBG).
			Padding(0, 1),
		Help: lipgloss.NewStyle().
			Background(t.BG).
			Padding(1, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(t.Border),

		Err: lipgloss.NewStyle().
			Foreground(t.ErrBadgeFG).
			Background(t.ErrBadgeBG).
			Bold(true),
		Dim: lipgloss.NewStyle().Foreground(t.Dim),

		HTTPSuccess:  lipgloss.NewStyle().Foreground(t.HTTPSuccess),
		HTTPRedirect: lipgloss.NewStyle().Foreground(t.HTTPRedirect),
		HTTPWarning:  lipgloss.NewStyle().Foreground(t.HTTPWarning),
		HTTPError:    lipgloss.NewStyle().Foreground(t.HTTPError),

		// Method colors (C9): GET=info-ish redirect cyan, POST=OK, PUT=waiting,
		// DELETE=err, PATCH=warn. Unknown methods stay default FG.
		HTTPGet:    lipgloss.NewStyle().Foreground(t.HTTPRedirect),
		HTTPPost:   lipgloss.NewStyle().Foreground(t.HTTPSuccess),
		HTTPPut:    lipgloss.NewStyle().Foreground(t.Waiting),
		HTTPDelete: lipgloss.NewStyle().Foreground(t.HTTPError),
		HTTPPatch:  lipgloss.NewStyle().Foreground(t.HTTPWarning),

		Status2xx: lipgloss.NewStyle().Foreground(t.HTTPSuccess),
		Status4xx: lipgloss.NewStyle().Foreground(t.HTTPWarning),
		Status5xx: lipgloss.NewStyle().Foreground(t.HTTPError),

		LogError: lipgloss.NewStyle().Foreground(t.LogError),
		LogWarn:  lipgloss.NewStyle().Foreground(t.LogWarn),
		LogInfo:  lipgloss.NewStyle().Foreground(t.LogInfo),
		LogDebug: lipgloss.NewStyle().Foreground(t.LogDebug),

		JSONKey:    lipgloss.NewStyle().Foreground(t.JSONKey),
		JSONString: lipgloss.NewStyle().Foreground(t.JSONString),
		JSONNumber: lipgloss.NewStyle().Foreground(t.JSONNumber),
		JSONBool:   lipgloss.NewStyle().Foreground(t.JSONBool),
		JSONNull:   lipgloss.NewStyle().Foreground(t.Dim), // Theme has no JSONNull slot

		Bold: lipgloss.NewStyle().Foreground(t.FG).Bold(true),

		// Cursor styles only the "❯ " marker, never the whole row: each row is a
		// concatenation of individually styled segments whose ANSI resets would
		// terminate an outer attribute mid-line (D10).
		Cursor: lipgloss.NewStyle().Foreground(t.Cursor).Bold(true),
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
		ss.ProcessColors = append(ss.ProcessColors, lipgloss.NewStyle().Foreground(c))
	}
	return ss
}
