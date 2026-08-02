package tui

// menuReservedBottom is the footer band — dropdowns never cover it (plan 023 B2).
const menuReservedBottom = 1

func (b *BaseModel) menuBoxTop() int {
	if b.settings.MenuBar {
		return 1
	}
	return 0
}

// menuAvail is the inner (content) row budget for the open dropdown — frame
// height minus bar, footer, and the top+bottom border (plan 023 B4).
func (b *BaseModel) menuAvail() int {
	return b.height - b.menuBoxTop() - menuReservedBottom - menuBorderSize
}

// menuWindowMaxOffset is the largest menuWindow for n item rows and avail.
func menuWindowMaxOffset(n, avail int) int {
	if avail < 1 || n <= avail {
		return 0
	}
	if avail < 4 {
		return n - avail
	}
	// End window: top indicator only → avail-1 item rows.
	return n - (avail - 1)
}

// menuWindowLayout returns the visible item slice [visStart, visEnd) and whether
// top/bottom "… N more …" indicator rows are shown for the given window start.
func menuWindowLayout(n, avail, start int) (visStart, visEnd int, topInd, botInd bool) {
	if avail < 1 || n == 0 {
		return 0, 0, false, false
	}
	if n <= avail {
		return 0, n, false, false
	}
	maxStart := menuWindowMaxOffset(n, avail)
	if start < 0 {
		start = 0
	}
	if start > maxStart {
		start = maxStart
	}
	if avail < 4 {
		return start, start + avail, false, false
	}
	topInd = start > 0
	contentCap := avail
	if topInd {
		contentCap--
	}
	if start+contentCap < n {
		botInd = true
		contentCap--
	}
	end := start + contentCap
	if end > n {
		end = n
	}
	return start, end, topInd, botInd
}

// followMenuWindow adjusts menuWindow after a highlight move so the highlight
// stays visible. Wrap last→first resets to 0; first→last jumps to maxOffset
// (strix derived-window semantics — plan 022 WS3).
func (b *BaseModel) followMenuWindow(prevHighlight int, movedDown bool) {
	if !b.menuOpen() {
		return
	}
	id := MenuID(b.openMenu)
	items := b.menuItems(id)
	n := len(items)
	avail := b.menuAvail()
	h := b.menuHighlight

	if movedDown && h < prevHighlight {
		b.menuWindow = 0
		return
	}
	if !movedDown && h > prevHighlight {
		b.menuWindow = menuWindowMaxOffset(n, avail)
		return
	}
	b.menuWindow = deriveMenuWindowStart(n, avail, b.menuWindow, h)
}

// clampMenuWindow keeps menuWindow in range and showing the highlight. Call
// after resize / highlight moves (not from View — value receiver).
func (b *BaseModel) clampMenuWindow() {
	if !b.menuOpen() {
		return
	}
	items := b.menuItems(MenuID(b.openMenu))
	b.menuWindow = deriveMenuWindowStart(len(items), b.menuAvail(),
		b.menuWindow, b.menuHighlight)
}

// deriveMenuWindowStart is THE window-start algorithm (plan 022 WS3): clamp to
// [0, maxOffset], then adjust minimally so highlight is visible. Input handlers
// persist its result into menuWindow; dropdownBoxRows re-derives per frame so
// View (a value receiver) never depends on the stored offset being fresh.
func deriveMenuWindowStart(n, avail, window, highlight int) int {
	if avail < 1 || n == 0 {
		return 0
	}
	maxStart := menuWindowMaxOffset(n, avail)
	if window > maxStart {
		window = maxStart
	}
	if window < 0 {
		window = 0
	}
	start, end, _, _ := menuWindowLayout(n, avail, window)
	if highlight >= start && highlight < end {
		return start
	}
	if highlight < start {
		return highlight
	}
	for start < maxStart {
		start++
		_, end, _, _ = menuWindowLayout(n, avail, start)
		if highlight < end {
			return start
		}
	}
	return maxStart
}

// menuStepDir moves the highlight by one selectable row. wrap=true (keyboard)
// wraps modulo n skipping separators; wrap=false (wheel) clamps at the ends.
func (b *BaseModel) menuStepDir(id MenuID, item int, down bool, wrap bool) int {
	items := b.menuItems(id)
	n := len(items)
	if n == 0 {
		return 0
	}
	idx := item
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	if !wrap {
		step := -1
		if down {
			step = 1
		}
		for j := idx + step; j >= 0 && j < n; j += step {
			if menuItemSelectable(items[j]) {
				return j
			}
		}
		return idx
	}
	for range n {
		if down {
			idx = (idx + 1) % n
		} else {
			idx = (idx + n - 1) % n
		}
		if menuItemSelectable(items[idx]) {
			break
		}
	}
	return idx
}
