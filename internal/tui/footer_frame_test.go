package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/stream"
)

// TestFooter_BandFillAndFlushRight pins plan 024 F3: every footer-band cell in
// FullFill themes carries a chrome BG (no default-bg holes), and when a right
// group is present its last glyph sits at the final column.
func TestFooter_BandFillAndFlushRight(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")
	th := CurrentTheme()
	require.True(t, th.FullFill)

	type state struct {
		name  string
		width int
		setup func(*ClientModel)
	}
	states := []state{
		{
			name:  "idle",
			width: 100,
			setup: func(m *ClientModel) {
				m.statusFlash = footerInfo("Tab: switch view | ? for help")
			},
		},
		{
			name:  "filter-edit",
			width: 100,
			setup: func(m *ClientModel) {
				m.mode = ModeStringFilter
				m.textInput.SetValue("level:chatty")
				m.textInput.Focus()
				m.setLogsFilterQuery("level:chatty") // invalid → [invalid filter]
			},
		},
		{
			name:  "error-flash",
			width: 100,
			setup: func(m *ClientModel) {
				m.statusFlash = footerError("settings not saved: disk full")
				m.statusFlashClass = flashSettingsSave
			},
		},
		{
			name:  "narrow-20",
			width: 20,
			setup: func(m *ClientModel) {
				m.statusFlash = footerInfo("Tab: switch view | ? for help")
			},
		},
		{
			name:  "narrow-40",
			width: 40,
			setup: func(m *ClientModel) {
				// Unavailable requests stream forces a styled n/a segment that
				// previously truncated into a bare hole at 40 cols.
				m.streamHealth[StreamRequests] = stream.Status{State: stream.StateUnavailable}
				m.statusFlash = footerInfo("Tab: switch view | ? for help")
			},
		},
	}

	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: st.width, Height: 24})
			st.setup(&m)
			applyTextInputTheme(&m.textInput)

			bar := m.statusBar(m.resolveFooterMsg())
			require.Equal(t, st.width, ansi.StringWidth(bar),
				"footer must be exact width")

			assertNoDefaultBGOutsideExempt(t, bar, th, nil)

			// Right-flush: when a right group is present, no trailing spaces.
			plain := ansi.Strip(bar)
			if footerRightGroupPresent(plain) {
				trimmed := strings.TrimRight(plain, " ")
				assert.Equal(t, ansi.StringWidth(plain), ansi.StringWidth(trimmed),
					"right group must end at final column; plain=%q", plain)
			}
		})
	}
}

// TestFooter_PadFlushRightUnit checks padFooterRow inserts mid-pad (not end-pad).
func TestFooter_PadFlushRightUnit(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")

	left := styles.FooterLabel.Render("LEFT")
	right := styles.FooterLabel.Render("RIGHT")
	const width = 20
	row := padFooterRow(left, right, width)
	require.Equal(t, width, ansi.StringWidth(row))
	plain := ansi.Strip(row)
	assert.True(t, strings.HasPrefix(plain, "LEFT"), plain)
	assert.True(t, strings.HasSuffix(plain, "RIGHT"), plain)
	assert.Equal(t, "LEFT           RIGHT", plain)
}

// TestFooter_LegacyFlushRightStillExactWidth pins legacy layout: flush-right
// mid-pad applies (plan 024 F3), width stays exact, byte-shape tests elsewhere
// stay green. FullFill zero-default-bg scan does NOT apply to legacy.
func TestFooter_LegacyFlushRightStillExactWidth(t *testing.T) {
	withTestTheme(t, "legacy")
	require.False(t, CurrentTheme().FullFill)

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.statusFlash = footerInfo("status ok")
	bar := m.statusBar(m.resolveFooterMsg())
	assert.Equal(t, 80, ansi.StringWidth(bar))
	plain := ansi.Strip(bar)
	require.Contains(t, plain, "q quit")
	assert.Equal(t, ansi.StringWidth(plain), ansi.StringWidth(strings.TrimRight(plain, " ")),
		"legacy footer must also flush-right; plain=%q", plain)
}

func footerRightGroupPresent(plain string) bool {
	return strings.Contains(plain, "q quit") ||
		strings.Contains(plain, "? help") ||
		strings.Contains(plain, "m menu") ||
		strings.Contains(plain, "[FOLLOW]") ||
		strings.Contains(plain, "[PAUSED]")
}
