package tui

// Frame-fill SGR scanner (plan 023 B1 / C5).
//
// Lives in a _test.go file: C16's selection-band assertions are also
// test-only, so sharing via the tui_test package surface is enough — no
// runtime export needed.

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// pinTrueColorProfile forces TrueColor so FullFill BG sequences are RGB
// (the B1 guarantee does not apply to NO_COLOR/ASCII output).
func pinTrueColorProfile(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// cellBG is one display cell's active background after walking preceding SGR.
// BG == "" means the terminal default background (unset / SGR 0 / SGR 49).
type cellBG struct {
	Row, Col int
	BG       string
}

// scanFrameCellBGs walks frame with x/ansi.DecodeSequence, tracking the
// active background per display cell. Newlines start a new row. Available to
// other _test.go files in this package for C16 reuse.
func scanFrameCellBGs(frame string) [][]cellBG {
	p := ansi.NewParser()
	var state byte
	active := ""
	var rows [][]cellBG
	var row []cellBG
	col := 0

	flush := func() {
		rows = append(rows, row)
		row = nil
		col = 0
	}

	input := frame
	for len(input) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(input, state, p)
		state = newState

		if width > 0 {
			for i := 0; i < width; i++ {
				row = append(row, cellBG{Row: len(rows), Col: col, BG: active})
				col++
			}
			input = input[n:]
			continue
		}

		if len(seq) == 1 && seq[0] == '\n' {
			flush()
			input = input[n:]
			continue
		}

		if ansi.Cmd(p.Command()).Final() == 'm' {
			active = applySGRBackground(active, p.Params())
		}
		input = input[n:]
	}
	if len(row) > 0 || len(rows) == 0 {
		rows = append(rows, row)
	}
	return rows
}

// applySGRBackground updates the active BG key from an SGR parameter list.
func applySGRBackground(cur string, params ansi.Params) string {
	if len(params) == 0 {
		return ""
	}
	i := 0
	for i < len(params) {
		val, _, ok := params.Param(i, 0)
		if !ok {
			break
		}
		switch {
		case val == 0:
			cur = ""
			i++
		case val == 49:
			cur = ""
			i++
		case val >= 40 && val <= 47:
			cur = fmt.Sprintf("ansi:%d", val)
			i++
		case val >= 100 && val <= 107:
			cur = fmt.Sprintf("ansi:%d", val)
			i++
		case val == 48:
			var co color.Color
			n := ansi.ReadStyleColor(params[i:], &co)
			if n <= 0 {
				i++
				continue
			}
			cur = colorBGKey(co)
			i += n
		case val == 38 || val == 58:
			var co color.Color
			n := ansi.ReadStyleColor(params[i:], &co)
			if n <= 0 {
				i++
				continue
			}
			i += n
		default:
			i++
		}
	}
	return cur
}

func colorBGKey(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return ""
	}
	return fmt.Sprintf("rgb:%d,%d,%d", r>>8, g>>8, b>>8)
}

// themeChromeBGKeys returns the set of background keys a FullFill theme may
// paint on TUI-generated cells (theme surface + chrome slots).
func themeChromeBGKeys(t *Theme) map[string]bool {
	keys := map[string]bool{}
	for _, c := range []lipgloss.Color{
		t.BG, t.HeaderBG, t.FooterBG, t.SelectionBG, t.SearchHitBG, t.ErrBadgeBG,
	} {
		keys[lipglossColorBGKey(c)] = true
	}
	return keys
}

func lipglossColorBGKey(c lipgloss.Color) string {
	st := lipgloss.NewStyle().Background(c)
	rendered := st.Render(" ")
	cells := scanFrameCellBGs(rendered)
	if len(cells) == 0 || len(cells[0]) == 0 {
		return ""
	}
	return cells[0][0].BG
}

// assertNoDefaultBGOutsideExempt fails on any cell whose BG is default unless
// exempt(row,col) is true (child-emitted content regions).
func assertNoDefaultBGOutsideExempt(t *testing.T, frame string, th *Theme, exempt func(row, col int) bool) {
	t.Helper()
	require.True(t, th.FullFill, "scanner assertion is for FullFill themes only")
	allowed := themeChromeBGKeys(th)
	rows := scanFrameCellBGs(frame)
	var holes []string
	for _, row := range rows {
		for _, cell := range row {
			if cell.BG == "" {
				if exempt != nil && exempt(cell.Row, cell.Col) {
					continue
				}
				holes = append(holes, fmt.Sprintf("r%d:c%d", cell.Row, cell.Col))
				if len(holes) >= 12 {
					break
				}
				continue
			}
			if !allowed[cell.BG] {
				holes = append(holes, fmt.Sprintf("r%d:c%d unexpected BG %s", cell.Row, cell.Col, cell.BG))
				if len(holes) >= 12 {
					break
				}
			}
		}
		if len(holes) >= 12 {
			break
		}
	}
	if len(holes) > 0 {
		t.Fatalf("TUI-generated cells missing theme/chrome BG (showing ≤12): %s", strings.Join(holes, ", "))
	}
}

func TestFrameFill_LightTheme_ChromePaddingOverlays(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "light")
	th := CurrentTheme()
	require.True(t, th.FullFill)

	dir := t.TempDir()
	withTestSettingsPath(t, dir+"/config.toml")

	m := newTestModel()
	m.settings = DefaultSettings()
	m.projectName = "demo"
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning, Health: domain.HealthStatusHealthy},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Nested 0m/49m inside child log content — those cells are exempt.
	const nested = "before\x1b[0mMID\x1b[49mAFTER"
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC),
		Process:   "web", Line: nested, DisplaySeq: 1,
	}))
	m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
		ID: "r1", Method: "GET", URL: "/unstyled-url", StatusCode: 301,
		Subdomain: "api", Timestamp: time.Now(),
		Duration: 150 * time.Millisecond, // mid band → Base
	}))

	// Corpus A: normal logs frame (padding + process spacer + unstyled URL path
	// exercised after switching views below).
	frame := m.View()
	assertFrameContract(t, m)

	prefixW := logPrefixWidth()
	vpTop := m.chromeAbove()
	vpBot := m.height - m.chromeBelow() - 1
	exemptLog := func(row, col int) bool {
		if row < vpTop || row > vpBot {
			return false
		}
		// First viewport row holds our single log entry's content cells.
		if row == vpTop && col >= prefixW {
			// Trailing padFrameRow spaces are TUI-generated — not exempt.
			contentW := ansi.StringWidth(nested)
			return col < prefixW+contentW
		}
		return false
	}
	assertNoDefaultBGOutsideExempt(t, frame, th, exemptLog)

	// Corpus B: requests view — unstyled URL + 3xx status + mid-band duration.
	m.setViewMode(ViewModeRequests)
	m.updateViewport()
	frame = m.View()
	assertFrameContract(t, m)
	// Request row content after the marker is TUI-formatted; URL/status are
	// Base-wrapped under FullFill, so no content exemption needed — every cell
	// must carry a chrome/theme BG.
	assertNoDefaultBGOutsideExempt(t, frame, th, nil)

	// Corpus C: menu overlay.
	m = clientUpdate(m, keyRune('v'))
	require.True(t, m.menuOpen())
	frame = m.View()
	assertFrameContract(t, m)
	assertNoDefaultBGOutsideExempt(t, frame, th, nil)

	// Corpus D: help overlay (closes menu first).
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)
	frame = m.View()
	assertFrameContract(t, m)
	assertNoDefaultBGOutsideExempt(t, frame, th, nil)
}

// logPrefixWidth is the display width of formatLogEntry chrome before content
// (timestamp "15:04:05" + two one-space separators + %-10s process name; no
// stderr badge in the corpus). Mirrors the literals in formatLogEntry.
func logPrefixWidth() int {
	return ansi.StringWidth("15:04:05") + 2 + 10
}

func TestLegacy_FullFillExempt(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "legacy")
	th := CurrentTheme()
	require.False(t, th.FullFill, "legacy must keep FullFill=false")

	ss := buildStyleSet(th)

	// Base must be a no-op (no BG) so raw segments stay byte-identical.
	_, isNo := ss.Base.GetBackground().(lipgloss.NoColor)
	assert.True(t, isNo, "legacy Base must not set Background")

	// Header keeps MarginBottom (pre-C5 escape shape).
	assert.Equal(t, 1, ss.Header.GetMarginBottom())

	// Styles that already had BG pre-C5 still do; FG-only styles must NOT
	// gain a FullFill Background(t.BG).
	assertHasBG := func(name string, st lipgloss.Style) {
		t.Helper()
		_, no := st.GetBackground().(lipgloss.NoColor)
		assert.False(t, no, "%s should keep its pre-C5 Background", name)
	}
	assertNoBG := func(name string, st lipgloss.Style) {
		t.Helper()
		_, no := st.GetBackground().(lipgloss.NoColor)
		assert.True(t, no, "%s must not gain FullFill Background under legacy", name)
	}

	assertHasBG("Header", ss.Header)
	assertHasBG("Status", ss.Status)
	assertHasBG("Help", ss.Help)
	assertHasBG("Err", ss.Err)
	assertHasBG("SearchHighlight", ss.SearchHighlight)

	assertNoBG("Running", ss.Running)
	assertNoBG("Dim", ss.Dim)
	assertNoBG("Cursor", ss.Cursor)
	assertNoBG("HTTPGet", ss.HTTPGet)
	assertNoBG("LogError", ss.LogError)
	assertNoBG("Bold", ss.Bold)
	require.NotEmpty(t, ss.ProcessColors)
	assertNoBG("ProcessColors[0]", ss.ProcessColors[0])

	// Help must NOT gain BorderBackground under legacy (pre-C5 had none).
	_, borderNo := ss.Help.GetBorderTopBackground().(lipgloss.NoColor)
	assert.True(t, borderNo, "legacy Help must not set BorderBackground")
}

func TestLegacy_ProcessPanelByteIdentity(t *testing.T) {
	// Pin the legacy panel escape shape: Header MarginBottom still emits the
	// unstyled spacer row inside Header.Render (FullFill replaces that path).
	pinANSIProfile(t)
	withTestTheme(t, "legacy")

	b := newTestBaseModel()
	b.viewMode = ViewModeLogs
	b.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
	}
	out := b.processPanel()
	require.Contains(t, out, "\n", "legacy Header MarginBottom must still embed a spacer newline")
	want := s.Header.Render(lipgloss.JoinHorizontal(lipgloss.Top,
		processStyle(domain.ProcessStateRunning).Render("1:web")))
	assert.Equal(t, want, out)
}
