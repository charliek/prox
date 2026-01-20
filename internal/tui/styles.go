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

	// Process colors for log lines
	processColors []lipgloss.Style
)

func init() {
	// Initialize process color styles
	for _, color := range processColorList {
		processColors = append(processColors, lipgloss.NewStyle().Foreground(color))
	}
}
