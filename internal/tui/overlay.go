package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
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

// fillPad returns n columns of padding carrying styles.Base (theme BG under
// FullFill; plain spaces under legacy — Base is a no-op style then).
func fillPad(n int) string {
	if n <= 0 {
		return ""
	}
	return styles.Base.Render(strings.Repeat(" ", n))
}

// centeredHint returns h rows of width w with text styled and centered both
// horizontally and vertically (plan 023 B6 empty states). Text truncates with
// "…" when wider than w. Empty/degenerate sizes yield blank Base-padded rows.
func centeredHint(text string, style lipgloss.Style, w, h int) []string {
	if h < 1 {
		h = 1
	}
	lines := make([]string, h)
	if w < 1 {
		for i := range lines {
			lines[i] = ""
		}
		return lines
	}
	trunc := ansi.Truncate(text, w, "…")
	tw := ansi.StringWidth(trunc)
	padLeft := (w - tw) / 2
	if padLeft < 0 {
		padLeft = 0
	}
	padRight := w - padLeft - tw
	if padRight < 0 {
		padRight = 0
	}
	centered := fillPad(padLeft) + style.Render(trunc) + fillPad(padRight)
	blank := fillPad(w)
	mid := h / 2
	for i := range lines {
		if i == mid {
			lines[i] = centered
		} else {
			lines[i] = blank
		}
	}
	return lines
}

// padFrameRow pads or truncates row to exactly width display columns.
func padFrameRow(row string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(row)
	switch {
	case w > width:
		return ansi.Cut(row, 0, width)
	case w < width:
		return row + fillPad(width-w)
	default:
		return row
	}
}

// padSelectionRow is padFrameRow for FullFill cursor-band rows: trailing fill
// uses styles.Selection (SelectionBG) so the band spans the full viewport width.
func padSelectionRow(row string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(row)
	switch {
	case w > width:
		return ansi.Cut(row, 0, width)
	case w < width:
		return row + styles.Selection.Render(strings.Repeat(" ", width-w))
	default:
		return row
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
		left += fillPad(1)
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
			right += fillPad(1)
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

// trimTrailingSpacesANSI removes trailing display cells that are a single
// space. Used before padFrameRow on viewport rows: bubbletea's viewport pads
// with raw spaces to Width, which would leave default-BG holes under FullFill
// (padFrameRow is a no-op when the row is already full width).
func trimTrailingSpacesANSI(row string) string {
	for {
		w := ansi.StringWidth(row)
		if w <= 0 {
			return row
		}
		last := ansi.Cut(row, w-1, w)
		if ansi.Strip(last) != " " {
			return row
		}
		row = ansi.Cut(row, 0, w-1)
	}
}
