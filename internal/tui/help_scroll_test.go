package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Help is a centred modal over the live frame (plan 022 WS4). Offset math
// derives from the modal inner height — not the full-frame height.

func TestHelp_WindowsToModalInnerHeight(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))

	view := m.View()
	assert.LessOrEqual(t, len(strings.Split(view, "\n")), 20,
		"frame must not exceed terminal height")
	assert.Contains(t, view, "Help — Logs", "border title stays visible")
	assert.Contains(t, view, helpModalFooter)
	// Live chrome behind the modal (merged footer; sticky ? help survives at 90).
	assert.Contains(t, ansi.Strip(view), "? help")
	assert.Contains(t, ansi.Strip(view), "[FOLLOW]")
	box := m.helpModalGeometry()
	assert.LessOrEqual(t, box.H, 16, "modal height clamped to frameH-4")
	assert.Greater(t, m.helpMaxOffset(), 0, "body exceeds modal budget at H=20")
}

func TestHelp_ScrollKeysMoveWindow(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))

	require.Contains(t, m.View(), "Help — Logs")
	require.Greater(t, m.helpMaxOffset(), 0)

	m.handleHelpKey(keyRune('j'))
	assert.Equal(t, 1, m.helpOffset)
	scrolled := m.View()
	assert.Contains(t, scrolled, "lines 2-")

	m.handleHelpKey(keyRune('k'))
	assert.Equal(t, 0, m.helpOffset)

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyPgDown})
	assert.Equal(t, m.helpPageStep(), m.helpOffset)

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEnd})
	assert.Equal(t, m.helpMaxOffset(), m.helpOffset)
	endView := m.View()
	assert.Contains(t, endView, helpModalFooter)
}

// k after G must scroll UP immediately. Clamp lives on the real model
// (CodeRabbit PR #102 / plan 022 WS4).
func TestHelp_KAfterGScrollsUpImmediately(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEnd})
	bottom := m.helpOffset
	require.Greater(t, bottom, 0)

	m.handleHelpKey(keyRune('k'))
	assert.Equal(t, bottom-1, m.helpOffset, "k must move one row up from the clamped bottom")
}

func TestHelp_CloseResetsOffset(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m = clientUpdate(m, keyRune('?'))
	m.helpOffset = 3

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, 0, m.helpOffset)
}

func TestHelp_FitsWithoutWindowing(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 60})
	m = clientUpdate(m, keyRune('?'))

	view := m.View()
	assert.Contains(t, view, "Help — Logs")
	assert.Contains(t, view, helpModalFooter)
	assert.Equal(t, 0, m.helpMaxOffset())
	assert.NotContains(t, view, "lines 1-", "no scroll indicator when it fits")
}

func TestHelp_ResizeShrinkClampsOffsetImmediately(t *testing.T) {
	// Short frame → high maxOffset at bottom. Growing the frame drops
	// maxOffset; without model-side clamp the first k looks dead (PR #102 /
	// plan 022 WS4). "Shrink" in the acceptance text is the scroll-range
	// shrink that follows a taller frame.
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 14})
	m = clientUpdate(m, keyRune('?'))
	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEnd})
	bottom := m.helpOffset
	require.Greater(t, bottom, 0)

	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 40})
	assert.Equal(t, ModeHelp, m.mode)
	assert.Equal(t, m.helpMaxOffset(), m.helpOffset, "resize clamps immediately")
	assert.Less(t, m.helpOffset, bottom)

	before := m.helpOffset
	m.handleHelpKey(keyRune('k'))
	if before == 0 {
		assert.Equal(t, 0, m.helpOffset)
	} else {
		assert.Equal(t, before-1, m.helpOffset, "first k after resize must move")
	}
}

func TestHelp_QuestionWithOpenMenuOpensHelp(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())

	m = clientUpdate(m, keyRune('?'))
	assert.Equal(t, ModeHelp, m.mode)
	assert.False(t, m.menuOpen(), "menu closed permanently")

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, ModeNormal, m.mode)
	assert.False(t, m.menuOpen(), "closing help does not restore the menu")
}

func TestHelp_StreamingBehindModal(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	// No timestamp prefix → process column starts at column 0, left of the
	// centred modal. Process names truncate to 10 cols in the log renderer.
	m.settings.Timestamps = false
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)

	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "STREAMBEH1", // exactly 10 cols — fully visible beside the modal
		Line:      "live-update",
	}))
	view := m.View()
	assert.Contains(t, view, "STREAMBEH1")
	assert.Contains(t, view, helpModalFooter)
	assert.Contains(t, view, "Help — Logs")
	assert.Contains(t, view, "1/1 lines")
}

func TestHelpModalRect_NarrowAndTinyFrames(t *testing.T) {
	x, y, w, h := helpModalRect(50, 24, 20)
	assert.Equal(t, 46, w, "width capped at frameW-4")
	assert.Equal(t, 20, h)
	assert.GreaterOrEqual(t, x, 0)
	assert.GreaterOrEqual(t, y, 0)

	// Tiny frame: no panic, clamps to fit.
	assert.NotPanics(t, func() {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 20, Height: 8})
		m = clientUpdate(m, keyRune('?'))
		_ = m.View()
		box := m.helpModalGeometry()
		assert.LessOrEqual(t, box.W, 20)
		assert.LessOrEqual(t, box.H, 8)
		assert.GreaterOrEqual(t, box.W, 1)
		assert.GreaterOrEqual(t, box.H, 1)
	})
}

func TestHelpModalRect_WidthClamp(t *testing.T) {
	_, _, w80, _ := helpModalRect(80, 24, 10)
	assert.Equal(t, 60, w80, "70% of 80 is 56 → clamped up to min(60,76)")

	_, _, w150, _ := helpModalRect(150, 40, 10)
	assert.Equal(t, 100, w150, "70% of 150 is 105 → clamped down to 100")
}

// TestHelp_DrawnBoxMatchesGeometry (T2) pins help modal render honesty:
// every drawn row's ansi.StringWidth == geometry W, row count == geometry H,
// and all four rounded-border corner glyphs are present. Asserts dimensions
// only — not exact border-row content (C10 splices a title into the top
// border). Plan 023 A2 / B2.
func TestHelp_DrawnBoxMatchesGeometry(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		corners bool // bordered box expected (outer W >= 3)
	}{
		{name: "90x26", w: 90, h: 26, corners: true},
		{name: "90x30", w: 90, h: 30, corners: true},
		{name: "120x60", w: 120, h: 60, corners: true},
		// frameW=8 → helpModalRect W=4 (< 6 chrome): drop side padding, keep
		// border. Height must clear the full bordered box so corners survive
		// the rows[:h] clamp (the regression T2 exists to catch).
		{name: "tiny-degraded", w: 8, h: 40, corners: true},
		// frameW=5/6 → W=1/2 (< 3): borderless rung. Geometry must stay
		// honest (row count == H incl. the 2+2 vertical chrome); no corners.
		{name: "borderless-w1", w: 5, h: 40},
		{name: "borderless-w2", w: 6, h: 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
			m = clientUpdate(m, keyRune('?'))
			require.Equal(t, ModeHelp, m.mode)

			geo := m.helpModalGeometry()
			rows, _, _ := m.helpModalBoxRows()
			require.NotEmpty(t, rows)
			assert.Equal(t, geo.H, len(rows), "row count must equal geometry H")
			assert.LessOrEqual(t, geo.W, tc.w, "box must never exceed frame width")
			if tc.name == "tiny-degraded" {
				assert.Less(t, geo.W, helpModalHorizChrome,
					"tiny-frame case should exercise degraded chrome")
			}

			var joined strings.Builder
			for i, row := range rows {
				assert.Equal(t, geo.W, ansi.StringWidth(row),
					"row %d width must equal geometry W", i)
				joined.WriteString(row)
			}
			if tc.corners {
				box := joined.String()
				for _, corner := range []string{"╭", "╮", "╰", "╯"} {
					assert.Contains(t, box, corner, "corner %s must be present", corner)
				}
			}
		})
	}
}
