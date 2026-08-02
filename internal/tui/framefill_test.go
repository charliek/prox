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

// childSpanExemptCells marks log-content cells whose background was cleared or
// recolored by an embedded (child-originated) SGR after the TUI had already
// painted theme/chrome BG on that row's content (plan 024 F1 / N1).
//
// Plain ANSI-free lines never set these bits: they stay fully asserted. Cells
// that never saw chrome BG in the content region are also NOT exempt — that is
// a TUI hole, not a child span. Trailing padFrameRow spaces re-enter chrome BG
// and clear the child-span state.
func childSpanExemptCells(frame string, allowed map[string]bool, inLogContent func(row, col int) bool) map[[2]int]bool {
	p := ansi.NewParser()
	var state byte
	active := ""
	rowIdx := 0
	col := 0
	sawChrome := false
	inChild := false
	exempt := map[[2]int]bool{}

	flush := func() {
		rowIdx++
		col = 0
		// SGR state carries across rows in real terminals, but each log line
		// is its own lipgloss.Render run — reset per-row tracking.
		sawChrome = false
		inChild = false
	}

	isChrome := func(bg string) bool {
		return bg != "" && allowed[bg]
	}

	input := frame
	for len(input) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(input, state, p)
		state = newState

		if width > 0 {
			for i := 0; i < width; i++ {
				if inLogContent != nil && inLogContent(rowIdx, col) {
					if isChrome(active) {
						sawChrome = true
						inChild = false
					} else if sawChrome {
						// Emitted a non-chrome cell after chrome was painted in
						// this content region → child reset/recolor mid-span.
						inChild = true
					}
					if inChild {
						exempt[[2]int{rowIdx, col}] = true
					}
				}
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
	return exempt
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

	// Nested 0m/49m inside child log content — mid-span cells are exempt.
	// Plain level-less line must be fully asserted (plan 024 F1 / N1).
	const nested = "before\x1b[0mMID\x1b[49mAFTER"
	const plain = "plain line no level"
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC),
		Process:   "web", Line: nested, DisplaySeq: 1,
	}))
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Timestamp: time.Date(2026, 8, 1, 15, 4, 6, 0, time.UTC),
		Process:   "web", Line: plain, DisplaySeq: 2,
	}))
	m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
		ID: "r1", Method: "GET", URL: "/unstyled-url", StatusCode: 301,
		Subdomain: "api", Timestamp: time.Now(),
		Duration: 150 * time.Millisecond, // mid band → Base
	}))

	// Corpus A: normal logs frame (padding + process spacer + plain level-less
	// line must carry theme BG; nested child escapes stay exempt mid-span).
	frame := m.View()
	assertFrameContract(t, m)

	prefixW := logPrefixWidth()
	ox, oy := m.viewportOrigin()
	vpTop := oy
	vpBot := oy + m.viewport.Height - 1
	inLogContent := func(row, col int) bool {
		if row < vpTop || row > vpBot {
			return false
		}
		// Content columns only (past ts/process prefix). Trailing pad is outside.
		if col < ox+prefixW {
			return false
		}
		var contentW int
		switch row {
		case vpTop: // nested entry
			contentW = ansi.StringWidth(nested)
		case vpTop + 1: // plain entry
			contentW = ansi.StringWidth(plain)
		default:
			return false
		}
		return col < ox+prefixW+contentW
	}
	allowed := themeChromeBGKeys(th)
	childExempt := childSpanExemptCells(frame, allowed, inLogContent)
	assertNoDefaultBGOutsideExempt(t, frame, th, func(row, col int) bool {
		return childExempt[[2]int{row, col}]
	})

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
	assertNoBG("Panel", ss.Panel)
	assertNoBG("PanelTitle", ss.PanelTitle)
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
	want := styles.Header.Render(lipgloss.JoinHorizontal(lipgloss.Top,
		processStyle(domain.ProcessStateRunning).Render("1:web")))
	assert.Equal(t, want, out)
}

// --- plan 023 E1 / C16: selection-band SGR scanner assertions ---

// assertBandRowBG checks every viewport content cell on frame row frameRow
// carries wantBG (SelectionBG or a mix checked by the caller per-col).
func assertViewportRowBG(t *testing.T, frame string, m ClientModel, contentRow int, wantBG string, allowHit string) {
	t.Helper()
	ox, oy := m.viewportOrigin()
	local := contentRow - m.viewport.YOffset
	require.GreaterOrEqual(t, local, 0)
	require.Less(t, local, m.viewport.Height)
	frameRow := oy + local
	rows := scanFrameCellBGs(frame)
	require.Greater(t, len(rows), frameRow)
	row := rows[frameRow]
	require.GreaterOrEqual(t, len(row), ox+m.viewport.Width,
		"frame row %d too short for viewport (len=%d need %d)", frameRow, len(row), ox+m.viewport.Width)
	var bad []string
	for col := ox; col < ox+m.viewport.Width; col++ {
		bg := row[col].BG
		if bg == wantBG || (allowHit != "" && bg == allowHit) {
			continue
		}
		bad = append(bad, fmt.Sprintf("c%d=%s", col, bg))
		if len(bad) >= 8 {
			break
		}
	}
	if len(bad) > 0 {
		t.Fatalf("contentRow %d (frame r%d) cells missing band BG %s (hit ok=%q): %s",
			contentRow, frameRow, wantBG, allowHit, strings.Join(bad, ", "))
	}
}

func TestSelectionBand_SingleLineLogRow(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")
	th := CurrentTheme()
	require.True(t, th.FullFill)
	selKey := lipglossColorBGKey(th.SelectionBG)
	hitKey := lipglossColorBGKey(th.SearchHitBG)

	m := newLogsModel(20, []string{"alpha line", "beta target here", "gamma"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "target")
	require.Equal(t, 1, m.logCursorIdx)
	require.True(t, m.inSelectionBand(m.logCursorIdx))

	frame := m.View()
	assertFrameContract(t, m)
	assertViewportRowBG(t, frame, m, m.logCursorIdx, selKey, hitKey)
	// ❯ marker retained on the band row.
	assert.Contains(t, m.viewport.View(), "❯")
}

func TestSelectionBand_WrappedLogEntry(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")
	th := CurrentTheme()
	selKey := lipglossColorBGKey(th.SelectionBG)
	hitKey := lipglossColorBGKey(th.SearchHitBG)

	long := "WRAPNEEDLE " + strings.Repeat("abcd ", 40)
	m := newLogsModel(20, []string{"short", long, "tail"})
	m.settings.Wrap = true
	m.settings.Timestamps = true
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 60, Height: 24})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "WRAPNEEDLE")
	require.Equal(t, 1, m.logCursorIdx)
	sp := m.logRowSpans[m.logCursorSeq]
	require.Greater(t, sp.Last-sp.First, 0, "selected entry must wrap across display rows")

	frame := m.View()
	assertFrameContract(t, m)
	for r := sp.First; r <= sp.Last; r++ {
		if r < m.viewport.YOffset || r >= m.viewport.YOffset+m.viewport.Height {
			continue
		}
		assertViewportRowBG(t, frame, m, r, selKey, hitKey)
	}
}

func TestSelectionBand_SearchHitPrecedence(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")
	th := CurrentTheme()
	selKey := lipglossColorBGKey(th.SelectionBG)
	hitKey := lipglossColorBGKey(th.SearchHitBG)
	require.NotEqual(t, selKey, hitKey)

	m := newLogsModel(20, []string{"nope", "find HITHERE please", "other"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "HITHERE")
	require.Equal(t, 1, m.logCursorIdx)

	frame := m.View()
	assertFrameContract(t, m)
	assertViewportRowBG(t, frame, m, m.logCursorIdx, selKey, hitKey)

	// At least one cell on the band carries SearchHitBG.
	ox, oy := m.viewportOrigin()
	rows := scanFrameCellBGs(frame)
	frameRow := rows[oy+(m.logCursorIdx-m.viewport.YOffset)]
	hitCount := 0
	selCount := 0
	for col := ox; col < ox+m.viewport.Width; col++ {
		switch frameRow[col].BG {
		case hitKey:
			hitCount++
		case selKey:
			selCount++
		}
	}
	assert.Greater(t, hitCount, 0, "search hit cells must keep SearchHitBG inside the band")
	assert.Greater(t, selCount, 0, "non-hit band cells must carry SelectionBG")
}

func TestSelectionBand_RequestsCursorRow(t *testing.T) {
	pinTrueColorProfile(t)
	withTestTheme(t, "tokyo-night")
	th := CurrentTheme()
	selKey := lipglossColorBGKey(th.SelectionBG)

	m := newTestModel()
	m.settings = DefaultSettings()
	dir := t.TempDir()
	withTestSettingsPath(t, dir+"/config.toml")
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.processes = []domain.ProcessInfo{{Name: "web", State: domain.ProcessStateRunning}}
	now := time.Now()
	for i := 0; i < 3; i++ {
		m = clientUpdate(m, ProxyRequestMsg(proxy.RequestRecord{
			ID: fmt.Sprintf("r%d", i), Method: "GET", URL: fmt.Sprintf("/p/%d", i),
			StatusCode: 200, Subdomain: "api", Timestamp: now,
			Duration: 10 * time.Millisecond,
		}))
	}
	m.setViewMode(ViewModeRequests)
	m.followMode = false
	m.setRequestCursor(m.filteredProxyRequests(), 1)
	m.updateViewport()
	require.Equal(t, 1, m.cursorIdx)
	require.True(t, m.inSelectionBand(1))

	frame := m.View()
	assertFrameContract(t, m)
	assertViewportRowBG(t, frame, m, 1, selKey, "")
	assert.Contains(t, m.viewport.View(), "❯")
}

func TestLegacy_SelectionBandDisabled(t *testing.T) {
	// C5 byte-identity: legacy keeps marker-only cursor rendering — no
	// SelectionBG band on cursor rows (FullFill=false).
	pinTrueColorProfile(t)
	withTestTheme(t, "legacy")
	th := CurrentTheme()
	require.False(t, th.FullFill)
	selKey := lipglossColorBGKey(th.SelectionBG)

	m := newLogsModel(20, []string{"alpha", "beta target", "gamma"})
	m = clientUpdate(m, keyRune('g'))
	m = commitSearch(m, "target")
	require.Equal(t, 1, m.logCursorIdx)
	assert.False(t, m.inSelectionBand(m.logCursorIdx), "legacy must not activate the band")

	frame := m.View()
	ox, oy := m.viewportOrigin()
	rows := scanFrameCellBGs(frame)
	frameRow := rows[oy+(m.logCursorIdx-m.viewport.YOffset)]
	for col := ox; col < ox+m.viewport.Width; col++ {
		assert.NotEqual(t, selKey, frameRow[col].BG,
			"legacy cursor row must not paint SelectionBG at c%d", col)
	}
	// Marker still present (old behavior).
	assert.Contains(t, m.viewport.View(), "❯")

	// Cursor style itself must remain FG-only under legacy (C5 pin).
	_, no := styles.Cursor.GetBackground().(lipgloss.NoColor)
	assert.True(t, no, "legacy Cursor must not gain FullFill/Selection Background")
}
