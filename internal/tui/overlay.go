package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Overlay splice invariants (plan 021 WS3 / Codex #1):
//
//  1. Every base row is padded to frame width BEFORE splicing so anchoring is
//     uniform (the process panel is not frame-width-padded on its own).
//  2. Each overlaid row = Cut(base,0,x) + SGR reset + box + OSC-8 terminator +
//     Cut(base,x+boxW,width) + SGR reset. ansi.Cut replays/carries ANSI by
//     design — the goal is VISUAL EQUIVALENCE of outside-box content, never
//     byte equality.
//  3. A cut straddling a wide grapheme drops/keeps the whole grapheme; pad
//     with spaces so the box lands at exact column x.

const (
	sgrReset       = "\x1b[0m"
	osc8Terminator = "\x1b]8;;\x1b\\"
)

// padFrameRow pads or truncates s to exactly width display columns.
func padFrameRow(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(s)
	switch {
	case w > width:
		return ansi.Cut(s, 0, width)
	case w < width:
		return s + strings.Repeat(" ", width-w)
	default:
		return s
	}
}

// overlayRow splices box into base at column x. base must already be padded to
// frameWidth. boxW is the display width of box. Returns a single frame row.
func overlayRow(base string, x, frameWidth int, box string) string {
	if frameWidth <= 0 {
		return ""
	}
	base = padFrameRow(base, frameWidth)
	boxW := ansi.StringWidth(box)
	if boxW <= 0 {
		return base
	}

	// Clamp x so the box stays inside the frame.
	if x < 0 {
		x = 0
	}
	if x+boxW > frameWidth {
		x = frameWidth - boxW
		if x < 0 {
			x = 0
			box = ansi.Cut(box, 0, frameWidth)
			boxW = ansi.StringWidth(box)
		}
	}

	left := ansi.Cut(base, 0, x)
	// Wide-grapheme pad: Cut may yield a shorter visual prefix than x.
	for ansi.StringWidth(left) < x {
		left += " "
	}

	rightStart := x + boxW
	right := ""
	if rightStart < frameWidth {
		right = ansi.Cut(base, rightStart, frameWidth)
		// If Cut kept a wide glyph that starts before rightStart, the right
		// fragment can be visually wider than the remaining columns — trim.
		remain := frameWidth - rightStart
		for ansi.StringWidth(right) > remain {
			right = ansi.Cut(right, 0, remain)
			if remain <= 0 {
				right = ""
				break
			}
			remain = frameWidth - rightStart
			if ansi.StringWidth(right) <= remain {
				break
			}
			// Defensive: shrink one column at a time if Cut can't land exactly.
			remain--
		}
		for ansi.StringWidth(right) < remain {
			right += " "
		}
	}

	// Isolate the box from surrounding SGR / OSC-8 (Codex #1).
	return left + sgrReset + osc8Terminator + box + osc8Terminator + sgrReset + right + sgrReset
}

// overlayLines splices boxRows into lines starting at (x, y). boxRows are
// already styled to a uniform display width. lines are padded to frameWidth.
// Returns a new slice; the dropdown is clamped vertically to the frame.
func overlayLines(lines []string, x, y, frameWidth int, boxRows []string) []string {
	if len(boxRows) == 0 || frameWidth <= 0 || len(lines) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	copy(out, lines)
	for i := range out {
		out[i] = padFrameRow(out[i], frameWidth)
	}

	// Vertical clamp: keep as many rows as fit from y.
	if y < 0 {
		y = 0
	}
	if y >= len(out) {
		return out
	}
	maxRows := len(out) - y
	if len(boxRows) > maxRows {
		boxRows = boxRows[:maxRows]
	}

	for i, row := range boxRows {
		out[y+i] = overlayRow(out[y+i], x, frameWidth, row)
	}
	return out
}
