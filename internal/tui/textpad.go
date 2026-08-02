package tui

import (
	"strings"
	"unicode/utf8"

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

// padLeftDisplay pads s on the left with spaces to width terminal columns.
func padLeftDisplay(s string, width int) string {
	w := ansi.StringWidth(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// skipDisplayWidth returns the byte offset into s after the first w display columns.
func skipDisplayWidth(s string, w int) int {
	if w <= 0 {
		return 0
	}
	consumed := 0
	i := 0
	var state byte
	p := ansi.NewParser()
	for i < len(s) && consumed < w {
		if s[i] == 0x1b || s[i] == 0x9b {
			_, dw, n, newState := ansi.DecodeSequence(s[i:], state, p)
			state = newState
			if dw > 0 {
				if consumed+dw > w {
					break
				}
				consumed += dw
			}
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := ansi.StringWidth(string(r))
		if consumed+rw > w {
			break
		}
		consumed += rw
		i += size
	}
	return i
}
