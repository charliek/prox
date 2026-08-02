package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// handleMenuMouse handles menu-bar / dropdown clicks, menu-open wheel, and
// free-motion hover (plan 022 WS2). Returns whether the event is fully
// consumed and an optional command from an activated item. Wheel with a menu
// open is always consumed (plan 022 WS3).
func (b *BaseModel) handleMenuMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if msg.Action == tea.MouseActionPress &&
		(msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		if !b.menuOpen() {
			return false, nil
		}
		b.clearRequestClickTracker()
		id := MenuID(b.openMenu)
		down := msg.Button == tea.MouseButtonWheelDown
		prev := b.menuHighlight
		b.menuHighlight = b.menuStepDir(id, b.menuHighlight, down, false)
		b.followMenuWindow(prev, down)
		return true, nil
	}

	// Drag: Motion with a real button — ignore entirely (no hover, no tracker touch).
	if msg.Action == tea.MouseActionMotion && msg.Button != tea.MouseButtonNone {
		return false, nil
	}

	if msg.Action == tea.MouseActionMotion {
		return b.handleMenuMotion(msg), nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return false, nil
	}
	x, y := msg.X, msg.Y
	hits := b.mustHits()

	// Mouse-open while a textinput mode is active: blur → ModeNormal first (Codex #4).
	blurTextMode := func() {
		switch b.mode {
		case ModeSearch, ModeStringFilter:
			b.mode = ModeNormal
			b.textInput.Blur()
		}
	}

	if b.settings.MenuBar {
		for _, h := range hits.menuCells {
			if h.Rect.Contains(x, y) {
				blurTextMode()
				b.clearRequestClickTracker()
				if b.menuOpen() && MenuID(b.openMenu) == h.ID {
					b.closeMenu()
				} else {
					b.openMenuFirst(h.ID)
				}
				return true, nil
			}
		}
	}

	if hits.hasDropdown && hits.dropdown.Bounds.Contains(x, y) {
		b.clearRequestClickTracker()
		d := &hits.dropdown
		fresh := b.menuOpen() && MenuID(b.openMenu) == d.Menu
		if !fresh {
			b.closeMenu()
			return true, nil
		}
		for _, row := range d.Rows {
			if row.Rect.Contains(x, y) {
				if row.Index >= 0 {
					cmd := b.activateMenuItem(d.Menu, row.Index)
					b.closeMenu()
					return true, cmd
				}
				return true, nil
			}
		}
		// Click on dropdown padding/border: consume, stay open.
		return true, nil
	}

	if b.menuOpen() {
		b.clearRequestClickTracker()
		b.closeMenu()
		return true, nil
	}
	return false, nil
}

// handleMenuMotion routes free hover (strix parity: the highlight IS the
// hover — no separate hover state). Mutations are guarded per site, so a
// no-op motion leaves an identical frame for the renderer's tty-write skip.
func (b *BaseModel) handleMenuMotion(msg tea.MouseMsg) bool {
	x, y := msg.X, msg.Y
	hits := b.mustHits()
	consumed := false

	if b.settings.MenuBar {
		if b.menuOpen() {
			if b.hoveredMenuCell >= 0 {
				b.hoveredMenuCell = -1
				consumed = true
			}
		} else {
			hovered := -1
			for _, h := range hits.menuCells {
				if h.Rect.Contains(x, y) {
					hovered = int(h.ID)
					break
				}
			}
			if hovered != b.hoveredMenuCell {
				b.hoveredMenuCell = hovered
				consumed = true
			}
		}
		for _, h := range hits.menuCells {
			if h.Rect.Contains(x, y) {
				consumed = true
				// Hover never opens a menu (strix); slide only when one is already open.
				if b.menuOpen() && MenuID(b.openMenu) != h.ID {
					b.openMenuFirst(h.ID)
				}
				break
			}
		}
	} else if b.hoveredMenuCell >= 0 {
		b.hoveredMenuCell = -1
		consumed = true
	}

	if hits.hasDropdown && hits.dropdown.Bounds.Contains(x, y) {
		consumed = true
		d := hits.dropdown
		if b.menuOpen() && MenuID(b.openMenu) == d.Menu {
			for _, row := range d.Rows {
				if row.Rect.Contains(x, y) {
					if row.Index >= 0 && row.Index != b.menuHighlight {
						prev := b.menuHighlight
						b.menuHighlight = row.Index
						b.followMenuWindow(prev, row.Index > prev)
					}
					break
				}
			}
		}
	}

	return consumed
}
