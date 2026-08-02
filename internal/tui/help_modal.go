package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Help modal (plan 022 WS4 / strix ui/modal.rs centred overlay):
// View() always builds the live main frame, then splices a bordered help box
// on top via overlayLines. Geometry is a pure function of frame dims — no
// recorded hit-rects (centred box can't drift from content-dependent widths).

const (
	// helpModalFooter is the fixed chrome row inside the box.
	helpModalFooter = "esc/?/q/enter close · j/k scroll"

	// helpModalFrameChrome is lipgloss border (2) + padding vertical (2).
	// MUST match styles.Help (styles.go): border + Padding(1, 2).
	helpModalFrameChrome = 4

	// helpModalHorizChrome is border L/R (2) + padding L/R (4) — styles.Help too.
	// When the outer box is narrower than this, helpHorizChrome degrades:
	// drop side padding first, then the border (plan 023 A2).
	helpModalHorizChrome = 6
)

// helpMemoCache holds the memoized wrapped help body. Heap-allocated and shared
// across ClientModel copies — render-time writes to a value copy are discarded
// (same pattern as hitRegistry — plan 023 B5).
type helpMemoCache struct {
	width     int
	height    int
	viewMode  ViewMode
	themeName string
	wrapped   []string
	wrapCalls int // instrumentation: incremented on each body wrap pass
}

// helpModalRect returns the centred help box geometry (plan 022 WS4).
// Width = clamp(70% of frameW, min(60, frameW-4), min(100, frameW-4)).
// Height = contentLines clamped to frameH-4. Degenerate frames clamp to at
// least 1×1 so callers never panic.
func helpModalRect(frameW, frameH, contentLines int) (x, y, w, h int) {
	maxW := frameW - 4
	if maxW < 1 {
		maxW = 1
		if frameW > 0 {
			maxW = frameW
		}
	}
	lo := min(60, maxW)
	hi := min(100, maxW)
	preferred := (frameW * 7) / 10
	w = preferred
	if w < lo {
		w = lo
	}
	if w > hi {
		w = hi
	}
	if w < 1 {
		w = 1
	}

	maxH := frameH - 4
	if maxH < 1 {
		maxH = 1
		if frameH > 0 {
			maxH = frameH
		}
	}
	h = contentLines
	if h > maxH {
		h = maxH
	}
	if h < 1 {
		h = 1
	}

	if frameW > w {
		x = (frameW - w) / 2
	}
	if frameH > h {
		y = (frameH - h) / 2
	}
	return x, y, w, h
}

// wrapHelpLines wraps each physical line of raw to width visual columns
// (ansi.Wrap). Offset math counts the resulting visual lines.
func wrapHelpLines(raw []string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, line := range raw {
		if line == "" {
			out = append(out, "")
			continue
		}
		wrapped := ansi.Wrap(line, width, "")
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}

// helpHorizChrome is the horizontal chrome for an outer box of width boxW.
// Full chrome is border+side-padding (6). Below that: border only when at
// least one content column remains (≥3), else none — never claim more
// chrome than fits (plan 023 A2 degraded).
func helpHorizChrome(boxW int) int {
	switch {
	case boxW >= helpModalHorizChrome:
		return helpModalHorizChrome
	case boxW >= 3:
		return 2
	default:
		return 0
	}
}

// helpInnerWidth is the content width inside a box of outer width boxW.
func helpInnerWidth(boxW int) int {
	w := boxW - helpHorizChrome(boxW)
	if w < 1 {
		return 1
	}
	return w
}

// helpModalFixedRows is the non-scrollable content rows inside the padded area.
// When a top border exists the title lives in the border; borderless keeps the
// legacy in-content title row.
func helpModalFixedRows(boxW int) int {
	if boxW >= 3 {
		return 1 // footer only
	}
	return 2 // title + footer
}

func (b *BaseModel) mustHelpMemo() *helpMemoCache {
	if b.helpMemo == nil {
		panic("tui: helpMemo is nil; construct via newBaseModel or newTestBaseModel")
	}
	return b.helpMemo
}

func (b *BaseModel) invalidateHelpMemo() {
	b.mustHelpMemo().wrapped = nil
}

func (b *BaseModel) helpMemoValid() bool {
	m := b.helpMemo
	if m == nil || m.wrapped == nil {
		return false
	}
	return m.width == b.width &&
		m.height == b.height &&
		m.viewMode == b.viewMode &&
		m.themeName == CurrentThemeName()
}

// refreshHelpMemo rebuilds the wrapped body on the real model (Update paths).
func (b *BaseModel) refreshHelpMemo() {
	memo := b.mustHelpMemo()
	innerW := helpInnerWidth(b.helpModalOuterWidth())
	memo.wrapCalls++
	memo.wrapped = wrapHelpBody(b.helpSections(), innerW)
	memo.width = b.width
	memo.height = b.height
	memo.viewMode = b.viewMode
	memo.themeName = CurrentThemeName()
}

// enterHelp opens the help modal: closes any menu permanently, clears the
// request double-click tracker, and clamps the scroll offset (plan 022 WS4).
func (b *BaseModel) enterHelp() {
	b.closeMenu()
	b.mode = ModeHelp
	b.helpOffset = 0
	b.clearRequestClickTracker()
	b.refreshHelpMemo()
	b.clampHelpOffset()
}

// exitHelp dismisses the help modal (keys or outside click).
func (b *BaseModel) exitHelp() {
	b.mode = ModeNormal
	b.helpOffset = 0
	b.clearRequestClickTracker()
	b.invalidateHelpMemo()
}

// clampHelpOffset keeps helpOffset in range on the real model (never in View —
// PR #102: after a shrink the first k must work immediately). Call on open,
// after every help key/wheel mutation, and in handleWindowSize when ModeHelp.
func (b *BaseModel) clampHelpOffset() {
	if b.helpOffset < 0 {
		b.helpOffset = 0
	}
	if maxOff := b.helpMaxOffset(); b.helpOffset > maxOff {
		b.helpOffset = maxOff
	}
}

// helpModalOuterWidth is the box width for the current frame.
func (b *BaseModel) helpModalOuterWidth() int {
	_, _, w, _ := helpModalRect(b.width, b.height, 1)
	return w
}

// helpWrappedBody returns the full wrapped body (visual lines) for offset math.
// View() only reads a valid memo; stale/nil memo computes fresh without caching.
func (b *BaseModel) helpWrappedBody() []string {
	if b.helpMemoValid() {
		return b.helpMemo.wrapped
	}
	innerW := helpInnerWidth(b.helpModalOuterWidth())
	return wrapHelpBody(b.helpSections(), innerW)
}

// helpContentBudget is the number of body rows available inside the modal
// (including the indicator slot when windowing). Derived from modal inner
// height — NOT b.height-4 (plan 022 WS4 / panel correction 7).
func (b *BaseModel) helpContentBudget() int {
	boxW := b.helpModalOuterWidth()
	maxOuter := b.height - 4
	if maxOuter < 1 {
		maxOuter = 1
		if b.height > 0 {
			maxOuter = b.height
		}
	}
	// Lines available inside border+padding for fixed chrome + body.
	maxInner := maxOuter - helpModalFrameChrome
	if maxInner < 1 {
		maxInner = 1
	}
	budget := maxInner - helpModalFixedRows(boxW)
	if budget < 1 {
		return 1
	}
	return budget
}

// helpMaxOffset is the largest helpOffset for the wrapped body at the current
// modal inner height (0 when the body fits without windowing).
func (b *BaseModel) helpMaxOffset() int {
	lines := b.helpWrappedBody()
	budget := b.helpContentBudget()
	if len(lines) <= budget {
		return 0
	}
	contentRows := budget - 1 // last row: scroll indicator
	if contentRows < 1 {
		contentRows = 1
	}
	return len(lines) - contentRows
}

// helpPageStep is the scroll step for pgup/pgdn in the help modal.
func (b *BaseModel) helpPageStep() int {
	step := b.helpContentBudget() / 2
	if step < 1 {
		return 1
	}
	return step
}

// helpModalGeometry returns the centred box rect matching the current render.
func (b *BaseModel) helpModalGeometry() HitRect {
	_, desiredH := b.helpModalBoxDims()
	x, y, w, h := helpModalRect(b.width, b.height, desiredH)
	return HitRect{X: x, Y: y, W: w, H: h}
}

// helpModalBoxDims returns outer width and the desired (pre-clamp) outer height.
func (b *BaseModel) helpModalBoxDims() (boxW, desiredH int) {
	boxW = b.helpModalOuterWidth()
	body := b.helpWrappedBody()
	budget := b.helpContentBudget()
	bodyRows := len(body)
	if bodyRows > budget {
		bodyRows = budget // windowed: budget rows (content + indicator)
	}
	// fixed chrome + bodyRows + border+padding.
	desiredH = helpModalFixedRows(boxW) + bodyRows + helpModalFrameChrome
	return boxW, desiredH
}

// renderHelpBorderTitleMid builds the top-border mid segment of display width inner.
func renderHelpBorderTitleMid(title string, inner int) string {
	return renderBorderTitleMid(title, inner, styles.HelpBorder, styles.HelpTitle)
}

// spliceHelpBorderTitle replaces the top border row with a titled variant.
func spliceHelpBorderTitle(topRow string, outerW int, title string) string {
	if outerW < 3 || title == "" {
		return topRow
	}
	br := lipgloss.RoundedBorder()
	inner := outerW - 2
	mid := renderHelpBorderTitleMid(title, inner)
	row := styles.HelpBorder.Render(br.TopLeft) + mid + styles.HelpBorder.Render(br.TopRight)
	return padFrameRow(row, outerW)
}

// helpModalBoxRows builds the bordered help box rows and their top-left anchor.
func (b *BaseModel) helpModalBoxRows() (rows []string, x, y int) {
	if b.width <= 0 || b.height <= 0 {
		return nil, 0, 0
	}
	_, desiredH := b.helpModalBoxDims()
	x, y, w, h := helpModalRect(b.width, b.height, desiredH)
	innerW := helpInnerWidth(w)
	bordered := w >= 3

	body := b.helpWrappedBody()
	budget := b.helpContentBudget()
	var visible []string
	if len(body) <= budget {
		visible = body
	} else {
		contentRows := budget - 1
		if contentRows < 1 {
			contentRows = 1
		}
		// Display-only net for a stale offset between resize and next key;
		// the real clamp is clampHelpOffset on the model (PR #102).
		offset := b.helpOffset
		if offset < 0 {
			offset = 0
		}
		if maxOff := b.helpMaxOffset(); offset > maxOff {
			offset = maxOff
		}
		end := offset + contentRows
		if end > len(body) {
			end = len(body)
		}
		visible = append(visible, body[offset:end]...)
		visible = append(visible, ansi.Cut(styles.Dim.Render(fmt.Sprintf(
			"… lines %d-%d of %d (j/k scroll) …", offset+1, end, len(body))), 0, innerW))
	}

	footer := ansi.Cut(styles.Dim.Render(helpModalFooter), 0, innerW)

	parts := make([]string, 0, 2+len(visible))
	if !bordered {
		title := ansi.Cut(b.helpTitleLine(), 0, innerW)
		parts = append(parts, title)
	}
	parts = append(parts, visible...)
	parts = append(parts, footer)

	// lipgloss Width(n) is padding-INCLUSIVE; border is added after. Passing
	// content width shrinks the drawn box by 4 (B2). For outer w with full
	// chrome, Width(w-2) yields outer w. Degraded: drop side padding, then
	// the border — never render wider than the frame (padFrameRow clamps).
	// The borderless rung keeps Padding(2,0): helpModalFrameChrome budgets
	// 2+2 vertical chrome regardless of the horizontal ladder.
	style := styles.Help
	widthArg := w - 2
	switch {
	case w >= helpModalHorizChrome:
		// Full Padding(1, 2) + border.
	case w >= 3:
		style = style.Padding(1, 0)
	default:
		style = style.Padding(2, 0).UnsetBorderStyle()
		widthArg = w
	}
	if widthArg < 1 {
		widthArg = 1
	}
	box := style.Width(widthArg).Render(strings.Join(parts, "\n"))
	rows = strings.Split(box, "\n")
	if bordered && len(rows) > 0 {
		rows[0] = spliceHelpBorderTitle(rows[0], w, b.helpBorderTitle())
	}
	// Degenerate clamp: never exceed the rect height/width.
	if len(rows) > h {
		rows = rows[:h]
	}
	for i := range rows {
		rows[i] = padFrameRow(rows[i], w)
	}
	return rows, x, y
}

// spliceHelpModal overlays the help box onto a live mainView frame.
func (b *BaseModel) spliceHelpModal(frame string) string {
	if b.width <= 0 || b.height <= 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	for len(lines) < b.height {
		lines = append(lines, padFrameRow("", b.width))
	}
	if len(lines) > b.height {
		lines = lines[:b.height]
	}
	for i := range lines {
		lines[i] = padFrameRow(lines[i], b.width)
	}
	rows, x, y := b.helpModalBoxRows()
	if len(rows) == 0 {
		return strings.Join(lines, "\n")
	}
	return strings.Join(overlayLines(lines, x, y, b.width, rows), "\n")
}

// handleHelpMouse routes mouse while the help modal is open (plan 022 WS4).
// All events are consumed. Wheel over the box scrolls help; wheel elsewhere is
// swallowed (never scrolls the TUI underneath). Left press outside closes and
// clears the tracker; press inside / releases / motion are no-ops.
func (b *BaseModel) handleHelpMouse(msg tea.MouseMsg) {
	box := b.helpModalGeometry()
	inBox := box.Contains(msg.X, msg.Y)

	if msg.Action == tea.MouseActionPress &&
		(msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		if inBox {
			if msg.Button == tea.MouseButtonWheelDown {
				b.helpOffset++
			} else {
				b.helpOffset--
			}
			b.clampHelpOffset()
		}
		return
	}

	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if !inBox {
			b.exitHelp()
		}
		return
	}
	// Releases / motion / other buttons: consumed no-op (hover must not touch
	// menus while help is open).
}
