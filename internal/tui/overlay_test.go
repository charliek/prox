package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// visualCols strips SGR / OSC-8 and returns the display-column string used for
// visual-equivalence checks (Codex #1 — never byte equality).
func visualCols(s string) string {
	var b strings.Builder
	inESC := false
	osc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inESC {
			if c == 0x1b {
				inESC = true
				osc = false
				continue
			}
			b.WriteByte(c)
			continue
		}
		// ESC [
		if !osc && c == '[' {
			// consume until letter
			for i+1 < len(s) {
				i++
				if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') {
					break
				}
			}
			inESC = false
			continue
		}
		// ESC ] OSC … BEL or ST
		if !osc && c == ']' {
			osc = true
			continue
		}
		if osc {
			if c == 0x07 { // BEL
				inESC = false
				osc = false
				continue
			}
			if c == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST
				i++
				inESC = false
				osc = false
				continue
			}
			continue
		}
		inESC = false
	}
	return b.String()
}

func TestOverlayRow_UnclosedSGRAtCut(t *testing.T) {
	// Base has an unclosed SGR that spans the cut column.
	base := "\x1b[31mABCDEFGHIJ\x1b[0m"
	frameW := ansi.StringWidth(visualCols(base))
	require.Equal(t, 10, frameW)
	box := "XX"
	out := overlayRow(padFrameRow(base, frameW), 3, frameW, box)

	vis := visualCols(out)
	assert.Equal(t, 10, ansi.StringWidth(vis), "frame width preserved")
	assert.Equal(t, "ABC", visualCols(ansi.Cut(padFrameRow(base, frameW), 0, 3)),
		"left outside-box content matches Cut")
	// Box replaces cols 3-4; right is cols 5..
	assert.Contains(t, vis, "XX")
	assert.True(t, strings.HasPrefix(vis, "ABC"), "left prefix visually intact: %q", vis)
	assert.True(t, strings.HasSuffix(strings.TrimRight(vis, " "), "FGHIJ") ||
		strings.Contains(vis, "FGHIJ"), "right fragment present: %q", vis)
}

func TestOverlayRow_OSC8HyperlinkSpanningBox(t *testing.T) {
	// OSC-8 hyperlink wrapping the whole base; box splice must terminate it at
	// the edges so the link does not "leak" into the box (Codex #1).
	link := "\x1b]8;;https://example.com\x1b\\ABCDEFGHIJ\x1b]8;;\x1b\\"
	frameW := 10
	box := "@@@@"
	out := overlayRow(padFrameRow(link, frameW), 2, frameW, box)
	assert.Contains(t, out, osc8Terminator, "OSC-8 terminator isolates the box")
	vis := visualCols(out)
	assert.Equal(t, frameW, ansi.StringWidth(vis))
	assert.Contains(t, vis, "@@@@")
}

func TestOverlayRow_WideGlyphAtBoundaries(t *testing.T) {
	// CJK ideograph is 2 columns; place box so cuts land on both sides of it.
	base := "あいうえお" // 5 glyphs × 2 = 10 cols
	frameW := ansi.StringWidth(base)
	require.Equal(t, 10, frameW)

	box := "BOX!" // 4 cols
	// x=1 straddles the first glyph (cols 0-1).
	out := overlayRow(padFrameRow(base, frameW), 1, frameW, box)
	vis := visualCols(out)
	assert.Equal(t, frameW, ansi.StringWidth(vis), "wide-glyph pad keeps frame width: %q", vis)
	assert.Contains(t, vis, "BOX!")

	// x=0 exact; x=frameW-boxW at right edge.
	outR := overlayRow(padFrameRow(base, frameW), frameW-4, frameW, box)
	visR := visualCols(outR)
	assert.Equal(t, frameW, ansi.StringWidth(visR))
	assert.True(t, strings.HasSuffix(strings.TrimRight(visR, " "), "BOX!") ||
		strings.Contains(visR, "BOX!"), visR)
}

func TestOverlayRow_CombiningClusterAtBoundaries(t *testing.T) {
	// e + combining acute (U+0301) is one grapheme, typically 1 column.
	base := "a\u0301bcdefghij" // ábcdefghij
	frameW := ansi.StringWidth(base)
	box := "##"
	out := overlayRow(padFrameRow(base, frameW), 0, frameW, box)
	assert.Equal(t, frameW, ansi.StringWidth(visualCols(out)))
	out2 := overlayRow(padFrameRow(base, frameW), frameW-2, frameW, box)
	assert.Equal(t, frameW, ansi.StringWidth(visualCols(out2)))
}

func TestOverlayRow_ShortBaseRow(t *testing.T) {
	base := "hi" // shorter than box x
	frameW := 20
	box := "MENU"
	out := overlayRow(padFrameRow(base, frameW), 10, frameW, box)
	vis := visualCols(out)
	assert.Equal(t, frameW, ansi.StringWidth(vis))
	assert.Contains(t, vis, "MENU")
}

func TestOverlayRow_ClampedAtRightEdge(t *testing.T) {
	base := strings.Repeat(".", 20)
	frameW := 20
	box := "WIDEBOX12" // 9 cols; x=15 would overflow
	out := overlayRow(padFrameRow(base, frameW), 15, frameW, box)
	vis := visualCols(out)
	assert.Equal(t, frameW, ansi.StringWidth(vis))
	assert.Contains(t, vis, "WIDEBOX12")
	// Box must end at or before frame edge.
	idx := strings.Index(vis, "WIDEBOX12")
	require.GreaterOrEqual(t, idx, 0)
	assert.LessOrEqual(t, idx+9, frameW)
}

func TestOverlayLines_HeightClamp(t *testing.T) {
	lines := []string{"0", "1", "2", "3", "4"}
	for i := range lines {
		lines[i] = padFrameRow(lines[i], 10)
	}
	box := []string{"A", "B", "C", "D", "E", "F"} // taller than remaining
	out := overlayLines(lines, 0, 3, 10, box)
	require.Len(t, out, 5)
	// Only rows 3 and 4 can take overlay content (2 of 6 box rows).
	assert.Contains(t, visualCols(out[3]), "A")
	assert.Contains(t, visualCols(out[4]), "B")
	assert.NotContains(t, visualCols(out[2]), "A")
}
