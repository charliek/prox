package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// padDisplay pads s on the right with spaces to width terminal columns. s may
// contain ANSI escapes and wide runes; width is measured with ansi.StringWidth.
func padDisplay(s string, width int) string {
	w := ansi.StringWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// truncatePadDisplay truncates s to width display columns (no tail) then pads
// the result to exactly width columns.
func truncatePadDisplay(s string, width int) string {
	return padDisplay(ansi.Truncate(s, width, ""), width)
}
