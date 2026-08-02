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

// minHangContentWidth is the minimum content-column budget required to apply
// hanging indent on wrapped continuations (logs gutter / help desc column).
// Below this, indent would starve the wrap and we fall back to whole-line wrap
// with no hang indent (plan 024 F5 / N5).
const minHangContentWidth = 10

func splitWrapLines(wrapped string) []string {
	parts := strings.Split(wrapped, "\n")
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

// hangIndentWrap wraps content at contentWidth and emits prefix+first-part on
// row 0, then fillPad(hangCols)+Base-painted part on each continuation.
// contentWidth and hangCols must come from the same width chain as the first
// row (viewport − prefix). When contentWidth < minHangContentWidth or
// hangCols <= 0, falls back to wrapping prefix+content at fullWidth with no
// hang indent.
//
// ansi.Wrap preserves SGR on the first segment only — continuations arrive
// bare — so each continuation is re-stripped and painted through styles.Base
// (theme BG under FullFill; SelectionBG inside withSelectionStyles). Span math
// and render share this helper so logRowSpans stay in sync (plan 024 F5).
func hangIndentWrap(prefix, content string, contentWidth, fullWidth, hangCols int) []string {
	if contentWidth < minHangContentWidth || hangCols <= 0 {
		limit := fullWidth
		if limit < 1 {
			limit = 1
		}
		return splitWrapLines(ansi.Wrap(prefix+content, limit, ""))
	}
	parts := splitWrapLines(ansi.Wrap(content, contentWidth, ""))
	out := make([]string, len(parts))
	indent := fillPad(hangCols)
	for i, p := range parts {
		if i == 0 {
			out[i] = prefix + p
			continue
		}
		out[i] = indent + styles.Base.Render(ansi.Strip(p))
	}
	return out
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
