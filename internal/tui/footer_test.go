package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/stream"
)

func TestFooter_ChromeBelowIsOne(t *testing.T) {
	m := newTestModel()
	assert.Equal(t, 1, m.chromeBelow())
	assert.Equal(t, 1, menuReservedBottom)
	// DefaultSettings: menu + panel(2) + footer(1) = 4
	assert.Equal(t, 4, defaultChromeHeight())
}

func TestFooter_ContentRect(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	r := m.contentRect()
	assert.Equal(t, 0, r.X)
	assert.Equal(t, m.chromeAbove(), r.Y)
	assert.Equal(t, 80, r.W)
	assert.Equal(t, 24-m.chromeHeight(), r.H)
	require.True(t, m.canDrawPanel())
	ox, oy := m.viewportOrigin()
	assert.Equal(t, r.X+1, ox, "panel insets viewport origin by 1 col")
	assert.Equal(t, r.Y+1, oy, "panel insets viewport origin by 1 row")
	assert.Equal(t, r.W-2, m.viewport.Width)
	assert.Equal(t, r.H-2, m.viewport.Height)
	assert.Equal(t, 23, m.footerRowY()) // height 24, chromeBelow 1
}

func TestFooter_ErrorFlashShape(t *testing.T) {
	styled := styleFooterMsg(footerError("boom"))
	plain := ansi.Strip(styled)
	assert.True(t, strings.HasPrefix(plain, "✗ "), "error flash needs ✗ prefix, got %q", plain)
	assert.Contains(t, plain, "boom")
	// Info has no marker.
	info := ansi.Strip(styleFooterMsg(footerInfo("theme: dark")))
	assert.False(t, strings.HasPrefix(info, "✗"))
	assert.Equal(t, "theme: dark", info)
}

func TestFooter_Precedence(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 24})
	m.opts.ConnectedStatus = "idle-ok"

	// Idle
	assert.Contains(t, m.resolveFooterMsg().text, "idle-ok")

	// Filter beats idle
	m.setLogsFilterQuery("proc:api")
	assert.Contains(t, m.resolveFooterMsg().text, "Filter:")
	assert.NotContains(t, m.resolveFooterMsg().text, "idle-ok")

	// Transient flash beats filter
	m.setStatusFlash(footerInfo("theme: dark"), flashTransient, statusFlashClearDelay)
	assert.Equal(t, "theme: dark", m.resolveFooterMsg().text)

	// Restart beats transient flash
	m.lastRestartProcess = "api"
	m.lastRestartError = nil
	assert.Equal(t, "Restarted: api", m.resolveFooterMsg().text)

	// Settings save beats restart
	m.setStatusFlash(footerError("settings not saved: disk full"), flashSettingsSave, statusFlashClearDelay)
	assert.Equal(t, footerKindError, m.resolveFooterMsg().kind)
	assert.Contains(t, m.resolveFooterMsg().text, "settings not saved")

	// Connection beats settings save
	m.connectionError = errProcessesStreamLost
	m.streamHealth[StreamProcesses] = stream.Status{State: stream.StateReconnecting}
	msg := m.resolveFooterMsg()
	assert.Equal(t, footerKindError, msg.kind)
	assert.Contains(t, msg.text, "Connection error")
}

func TestFooter_WidthDegradation(t *testing.T) {
	for _, w := range []int{20, 40, 80, 120} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: 24})
			m.statusFlash = footerInfo("status ok")
			bar := m.statusBar(m.resolveFooterMsg())
			assert.LessOrEqual(t, ansi.StringWidth(bar), w,
				"footer must fit width %d (got %d)", w, ansi.StringWidth(bar))
			plain := ansi.Strip(bar)
			// Wide frames keep sticky hints; very narrow may drop them.
			if w >= 80 {
				assert.Contains(t, plain, "? help")
				assert.Contains(t, plain, "q quit")
				assert.Contains(t, plain, "[FOLLOW]")
			}
			if w >= 120 {
				assert.Contains(t, plain, "m menu")
				assert.Contains(t, plain, "/ search")
				assert.Contains(t, plain, "s filter")
			}
		})
	}
}

func TestFooter_WideCharStatus(t *testing.T) {
	for _, w := range []int{20, 40, 80, 120} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			m := newTestModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: 24})
			// CJK status text — display width ≠ byte length.
			m.statusFlash = footerInfo("日本語テスト状態メッセージ")
			bar := m.statusBar(m.resolveFooterMsg())
			assert.LessOrEqual(t, ansi.StringWidth(bar), w)
			assert.Equal(t, 1, strings.Count(bar, "\n")+1, "footer must stay one row")
		})
	}
}

func TestFooter_HintDropOrder(t *testing.T) {
	hints := defaultFooterHints()
	require.Len(t, hints, 5)
	hints = dropFooterHint(hints)
	// Non-sticky dropped from the right first: s filter gone
	for _, h := range hints {
		assert.NotEqual(t, "s", h.key)
	}
	hints = dropFooterHint(hints) // drops /
	for _, h := range hints {
		assert.NotEqual(t, "/", h.key)
	}
	hints = dropFooterHint(hints) // drops m
	require.Len(t, hints, 2)
	assert.Equal(t, "?", hints[0].key)
	assert.Equal(t, "q", hints[1].key)
	hints = dropFooterHint(hints) // rightmost sticky: q
	require.Len(t, hints, 1)
	assert.Equal(t, "?", hints[0].key)
}

func TestFooter_ClickNonInterference(t *testing.T) {
	m := newTestModel()
	m.logEntries = []domain.LogEntry{
		{Process: "api", Line: "one"},
		{Process: "api", Line: "two"},
		{Process: "api", Line: "three"},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.updateViewport()
	_ = m.View() // record hits

	beforeCursor := m.logCursorSeq
	beforeFollow := m.followMode
	fy := m.footerRowY()

	// Click footer with menu closed: consumed no-op.
	m2 := clientUpdate(m, clickAt(10, fy))
	assert.False(t, m2.menuOpen())
	assert.Equal(t, beforeCursor, m2.logCursorSeq)
	assert.Equal(t, beforeFollow, m2.followMode)
	assert.Equal(t, ModeNormal, m2.mode)

	// Open a menu, click footer: menu closes (outside-click), no cursor move.
	m3 := clientUpdate(m, keyRune('v'))
	require.True(t, m3.menuOpen())
	_ = m3.View()
	m3 = clientUpdate(m3, clickAt(10, fy))
	assert.False(t, m3.menuOpen())
	assert.Equal(t, beforeCursor, m3.logCursorSeq)
}
