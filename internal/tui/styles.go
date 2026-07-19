package tui

import "github.com/charmbracelet/lipgloss"

// Colors
var (
	// Process state colors
	runningColor  = lipgloss.Color("10") // Green
	stoppedColor  = lipgloss.Color("8")  // Gray
	crashedColor  = lipgloss.Color("9")  // Red
	startingColor = lipgloss.Color("11") // Yellow
	stoppingColor = lipgloss.Color("11") // Yellow

	// UI colors
	headerBg   = lipgloss.Color("235")
	statusBg   = lipgloss.Color("236")
	helpBg     = lipgloss.Color("234")
	errorColor = lipgloss.Color("9")
	dimColor   = lipgloss.Color("8")

	// HTTP status colors
	successColor  = lipgloss.Color("10") // Green for 2xx
	redirectColor = lipgloss.Color("14") // Cyan for 3xx
	warningColor  = lipgloss.Color("11") // Yellow for 4xx
	// errorColor already defined above for 5xx

	// Process name colors (for log lines)
	processColorList = []lipgloss.Color{
		lipgloss.Color("14"),  // Cyan
		lipgloss.Color("13"),  // Magenta
		lipgloss.Color("12"),  // Blue
		lipgloss.Color("11"),  // Yellow
		lipgloss.Color("10"),  // Green
		lipgloss.Color("208"), // Orange
		lipgloss.Color("207"), // Pink
		lipgloss.Color("159"), // Light blue
		lipgloss.Color("156"), // Light green
	}
)

// Styles
var (
	// Process state styles
	runningStyle = lipgloss.NewStyle().
			Foreground(runningColor).
			Bold(true)

	stoppedStyle = lipgloss.NewStyle().
			Foreground(stoppedColor)

	crashedStyle = lipgloss.NewStyle().
			Foreground(crashedColor).
			Bold(true)

	startingStyle = lipgloss.NewStyle().
			Foreground(startingColor)

	stoppingStyle = lipgloss.NewStyle().
			Foreground(stoppingColor)

	defaultProcessStyle = lipgloss.NewStyle()

	// Header style
	headerStyle = lipgloss.NewStyle().
			Background(headerBg).
			Padding(0, 1).
			MarginBottom(1)

	// Status bar style
	statusStyle = lipgloss.NewStyle().
			Background(statusBg).
			Padding(0, 1)

	// Help overlay style
	helpStyle = lipgloss.NewStyle().
			Background(helpBg).
			Padding(1, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	// Error indicator style
	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).
			Background(errorColor).
			Bold(true)

	// Dim style for timestamps
	dimStyle = lipgloss.NewStyle().
			Foreground(dimColor)

	// HTTP status styles
	httpSuccessStyle = lipgloss.NewStyle().
				Foreground(successColor)

	httpRedirectStyle = lipgloss.NewStyle().
				Foreground(redirectColor)

	httpWarningStyle = lipgloss.NewStyle().
				Foreground(warningColor)

	httpErrorStyle = lipgloss.NewStyle().
			Foreground(errorColor)

	// Cursor marker style for the selected row in the requests view. Bold +
	// magenta accent (from the process palette, and distinct from the HTTP
	// status colors) so the "❯ " marker reads clearly against the row's own
	// dim/colored segments. Only the marker is styled, never the whole row:
	// each row is a concatenation of individually styled segments whose ANSI
	// resets would terminate an outer attribute mid-line (see D10).
	cursorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("13")).
			Bold(true)

	// Search highlight style for the matched substring of a logs-view line
	// during `/`-search (D9). An accent background (yellow) with black text so
	// the hit stands out against the line's own coloring. Applied only to the
	// exact matched run, and only when the query and the whole line are plain
	// ASCII with no ESC byte — otherwise case-folding could shift byte offsets
	// or the run could land inside an ANSI escape, so formatLogEntry falls back
	// to the row marker alone (see isASCIINoESC).
	searchHighlightStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(lipgloss.Color("11")).
				Bold(true)

	// Process colors for log lines
	processColors []lipgloss.Style
)

func init() {
	// Initialize process color styles
	for _, color := range processColorList {
		processColors = append(processColors, lipgloss.NewStyle().Foreground(color))
	}
}
