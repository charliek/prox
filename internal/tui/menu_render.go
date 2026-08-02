package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func menuLabel(id MenuID) string {
	switch id {
	case MenuView:
		return "View"
	case MenuFilter:
		return "Filter"
	case MenuTheme:
		return "Theme"
	default:
		return ""
	}
}

func menuCellText(id MenuID) string {
	return " " + menuLabel(id) + " ▾ "
}

// menuBorderSize is the rounded-border cost in rows and columns (plan 023 B4).
const menuBorderSize = 2

// renderMenuBar draws the menu row and records cell hit-rects.
// Layout (plan 023 B3): `prox` (bold HeaderFG) + Dim project name + cells,
// left-aligned; the remainder of the row is HeaderBG fill.
func (b *BaseModel) renderMenuBar() string {
	var bld strings.Builder
	bld.WriteString(styles.MenuBarFill.Render(" "))
	bld.WriteString(styles.MenuBrand.Render("prox"))
	if b.projectName != "" {
		bld.WriteString(styles.MenuBarFill.Render(" "))
		bld.WriteString(styles.MenuHint.Render(b.projectName))
	}
	bld.WriteString(styles.MenuBarFill.Render(" "))

	x := ansi.StringWidth(bld.String())
	y := 0 // menu bar is always row 0 when visible; caller places it first
	hits := b.mustHits()
	for _, id := range menuOrder {
		text := menuCellText(id)
		w := ansi.StringWidth(text)
		style := styles.MenuCell
		if b.menuOpen() && MenuID(b.openMenu) == id {
			style = styles.MenuCellOpen
		} else if !b.menuOpen() && b.hoveredMenuCell == int(id) {
			style = styles.MenuCellHover
		}
		bld.WriteString(style.Render(text))
		hits.menuCells = append(hits.menuCells, menuCellHit{
			ID:   id,
			Rect: HitRect{X: x, Y: y, W: w, H: 1},
		})
		x += w
	}

	line := bld.String()
	w := ansi.StringWidth(line)
	switch {
	case w < b.width:
		line += styles.MenuBarFill.Render(strings.Repeat(" ", b.width-w))
	case w > b.width:
		line = ansi.Cut(line, 0, b.width)
	}
	return line
}

// dropdownBoxRows builds the windowed dropdown (rounded border + indicators +
// visible items) and records hit-rects for content rows only; activatable rows
// carry their full-list Index so hover/click map visible → item (plan 022 WS3 /
// plan 023 B4). Bounds include the border so border clicks are consumed no-ops
// (menu stays open), not click-away closes.
func (b *BaseModel) dropdownBoxRows() (rows []string, boxX, boxW int) {
	if !b.menuOpen() || !b.settings.MenuBar {
		return nil, 0, 0
	}
	id := MenuID(b.openMenu)
	items := b.menuItems(id)

	const pad = 1 // leading edge pad inside the border
	const hintGap = 1
	n := len(items)

	// Inner content width: marker+label (+ hint column) and indicator text when
	// the list scrolls. Outer box = inner + left/right border.
	maxInner := 0
	for _, it := range items {
		if it.Separator {
			continue
		}
		w := menuItemInnerWidth(it, pad, hintGap)
		if w > maxInner {
			maxInner = w
		}
	}
	if maxInner == 0 {
		maxInner = ansi.StringWidth("(coming soon)") + pad*2
	}
	if avail := b.menuAvail(); n > avail && avail >= 4 {
		indW := ansi.StringWidth(fmt.Sprintf("… %d more …", n)) + pad*2
		if indW > maxInner {
			maxInner = indW
		}
	}
	innerW := maxInner
	boxW = innerW + menuBorderSize
	if boxW > b.width {
		boxW = b.width
		innerW = boxW - menuBorderSize
	}
	if b.width < 3 {
		// Too narrow for a non-corrupting rounded border.
		h := b.mustHits()
		h.dropdown = menuDropdownHit{}
		h.hasDropdown = false
		return nil, 0, 0
	}

	// Anchor under the cell; clamp so the box stays on-screen.
	boxX = 0
	for _, h := range b.mustHits().menuCells {
		if h.ID == id {
			boxX = h.Rect.X
			break
		}
	}
	if boxX+boxW > b.width {
		boxX = b.width - boxW
	}
	if boxX < 0 {
		boxX = 0
	}

	boxTop := b.menuBoxTop()
	avail := b.menuAvail()
	if avail < 1 || n == 0 {
		h := b.mustHits()
		h.dropdown = menuDropdownHit{}
		h.hasDropdown = false
		return nil, boxX, boxW
	}

	visStart, visEnd, topInd, botInd := menuWindowLayout(n, avail,
		deriveMenuWindowStart(n, avail, b.menuWindow, b.menuHighlight))

	br := lipgloss.RoundedBorder()

	hitRows := make([]menuRowHit, 0, avail)
	rows = make([]string, 0, avail+menuBorderSize)
	contentX := boxX + 1
	rowY := boxTop + 1

	// Top border.
	rows = append(rows, styles.Dropdown.Render(br.TopLeft)+
		styles.Dropdown.Render(strings.Repeat(br.Top, innerW))+
		styles.Dropdown.Render(br.TopRight))

	renderInd := func(hidden int) {
		label := fmt.Sprintf("… %d more …", hidden)
		content := strings.Repeat(" ", pad) + label
		content = padFrameRow(content, innerW)
		inner := styles.DropdownDim.Render(ansi.Cut(content, 0, innerW))
		row := styles.Dropdown.Render(br.Left) + padFrameRow(inner, innerW) + styles.Dropdown.Render(br.Right)
		rows = append(rows, row)
		hitRows = append(hitRows, menuRowHit{
			Index: -1,
			Rect:  HitRect{X: contentX, Y: rowY, W: innerW, H: 1},
		})
		rowY++
	}

	if topInd {
		renderInd(visStart)
	}

	for i := visStart; i < visEnd; i++ {
		it := items[i]
		switch {
		case it.Separator:
			content := strings.Repeat("─", max(1, innerW-pad*2))
			content = strings.Repeat(" ", pad) + content + strings.Repeat(" ", pad)
			content = ansi.Cut(content, 0, innerW)
			inner := styles.DropdownDim.Render(padFrameRow(content, innerW))
			row := styles.Dropdown.Render(br.Left) + padFrameRow(inner, innerW) + styles.Dropdown.Render(br.Right)
			rows = append(rows, row)
			hitRows = append(hitRows, menuRowHit{
				Index: -1,
				Rect:  HitRect{X: contentX, Y: rowY, W: innerW, H: 1},
			})
		default:
			highlighted := i == b.menuHighlight && it.Cmd != ""
			inner := renderMenuItemInner(it, innerW, pad, hintGap, highlighted)
			row := styles.Dropdown.Render(br.Left) + padFrameRow(inner, innerW) + styles.Dropdown.Render(br.Right)
			rows = append(rows, row)
			idx := i
			if it.Cmd == "" {
				idx = -1
			}
			hitRows = append(hitRows, menuRowHit{
				Index: idx,
				Rect:  HitRect{X: contentX, Y: rowY, W: innerW, H: 1},
			})
		}
		rowY++
	}

	if botInd {
		renderInd(n - visEnd)
	}

	// Bottom border.
	rows = append(rows, styles.Dropdown.Render(br.BottomLeft)+
		styles.Dropdown.Render(strings.Repeat(br.Bottom, innerW))+
		styles.Dropdown.Render(br.BottomRight))

	hits := b.mustHits()
	hits.dropdown = menuDropdownHit{
		Menu:   id,
		Bounds: HitRect{X: boxX, Y: boxTop, W: boxW, H: len(rows)},
		Rows:   hitRows,
	}
	hits.hasDropdown = true
	return rows, boxX, boxW
}

// menuItemInnerWidth is the preferred inner width for one item (pads + label +
// optional hint column with a minimum gap).
func menuItemInnerWidth(it MenuItem, pad, hintGap int) int {
	left := menuMarker(it) + " " + it.Label
	w := pad + ansi.StringWidth(left)
	if it.Hint != "" {
		w += hintGap + ansi.StringWidth(it.Hint) + pad
	} else {
		w += pad
	}
	return w
}

// renderMenuItemInner paints one dropdown content row of exactly innerW columns:
// left-aligned marker+label, right-aligned Hint in Dim, ANSI-aware label
// truncation when tight (plan 023 B4).
func renderMenuItemInner(it MenuItem, innerW, pad, hintGap int, highlighted bool) string {
	label := it.Label
	marker := menuMarker(it)
	hint := it.Hint

	var leftStyle, hintStyle, gapStyle lipgloss.Style
	if highlighted {
		leftStyle = styles.DropdownItemSelected
		gapStyle = styles.DropdownSelectedGap
		hintStyle = styles.DropdownItemSelectedHint
	} else if it.Cmd == "" {
		leftStyle = styles.DropdownItemMuted
		gapStyle = styles.DropdownGap
		hintStyle = leftStyle
	} else {
		leftStyle = styles.DropdownItem
		gapStyle = styles.DropdownGap
		hintStyle = styles.DropdownItemHint
	}

	hintW := 0
	if hint != "" {
		hintW = ansi.StringWidth(hint) + pad // hint + trailing edge pad
	}
	// Budget for leading pad + marker + space + label (+ min gap when hint).
	leftBudget := innerW - hintW
	if hint != "" {
		leftBudget -= hintGap
	}
	if leftBudget < 1 {
		leftBudget = 1
	}

	prefix := strings.Repeat(" ", pad) + marker + " "
	prefixW := ansi.StringWidth(prefix)
	labelBudget := leftBudget - prefixW
	if labelBudget < 1 {
		labelBudget = 1
	}
	truncLabel := ansi.Truncate(label, labelBudget, "…")
	leftPlain := prefix + truncLabel
	// If still over budget (tiny innerW), cut hard.
	if ansi.StringWidth(leftPlain) > leftBudget {
		leftPlain = ansi.Cut(leftPlain, 0, leftBudget)
	}

	gapCols := innerW - ansi.StringWidth(leftPlain) - hintW
	if gapCols < 0 {
		gapCols = 0
	}
	if hint != "" && gapCols < hintGap {
		// Prefer keeping the hint: shrink left further.
		shrink := hintGap - gapCols
		lw := ansi.StringWidth(leftPlain)
		if lw > shrink {
			leftPlain = ansi.Cut(leftPlain, 0, lw-shrink)
			gapCols = hintGap
		}
	}

	var bld strings.Builder
	bld.WriteString(leftStyle.Render(leftPlain))
	if gapCols > 0 {
		bld.WriteString(gapStyle.Render(strings.Repeat(" ", gapCols)))
	}
	if hint != "" {
		bld.WriteString(hintStyle.Render(hint))
		bld.WriteString(gapStyle.Render(strings.Repeat(" ", pad)))
	}
	return padFrameRow(bld.String(), innerW)
}

func menuMarker(it MenuItem) string {
	switch {
	case it.Selected != nil:
		if *it.Selected {
			return " ● "
		}
		return "   "
	case it.Checked != nil:
		if *it.Checked {
			return "[x]"
		}
		return "[ ]"
	default:
		return "   "
	}
}

// applyMenuOverlay splices the open dropdown onto the composed frame lines.
func (b *BaseModel) applyMenuOverlay(lines []string) []string {
	rows, boxX, _ := b.dropdownBoxRows()
	if len(rows) == 0 {
		return lines
	}
	return overlayLines(lines, boxX, b.menuBoxTop(), b.width, rows)
}
