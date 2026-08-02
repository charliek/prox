package tui

import "os"

// OSC 22 mouse pointer shapes (strix parity — ../strix/src/terminal.rs).
// Terminals that support OSC 22 (kitty, WezTerm, Alacritty, Ghostty, …)
// honour the CSS cursor names; others ignore the sequence harmlessly.
const (
	osc22Pointer = "\x1b]22;pointer\x1b\\"
	osc22Default = "\x1b]22;default\x1b\\"
)

type cursorShape int

const (
	cursorShapeUnset cursorShape = iota
	cursorShapeDefault
	cursorShapePointer
)

// pointerShapeCache tracks the last emitted OSC-22 shape behind a shared
// pointer (View is a value receiver — plan 023 C17 / value-copy law).
type pointerShapeCache struct {
	current cursorShape
	// emissions records every sequence returned from takeSequence when record
	// is set (test seam — off in prod so long sessions don't accumulate).
	record    bool
	emissions []string
}

func (c *pointerShapeCache) takeSequence(want cursorShape) string {
	if c.current == want {
		return ""
	}
	c.current = want
	seq := osc22ForShape(want)
	if c.record {
		c.emissions = append(c.emissions, seq)
	}
	return seq
}

func osc22ForShape(shape cursorShape) string {
	switch shape {
	case cursorShapePointer:
		return osc22Pointer
	case cursorShapeDefault:
		return osc22Default
	default: // cursorShapeUnset is never a `want`
		return ""
	}
}

func (b *BaseModel) mustPointerShape() *pointerShapeCache {
	if b.pointerShape == nil {
		panic("tui: pointerShape is nil; construct via newBaseModel or newTestBaseModel")
	}
	return b.pointerShape
}

// noteMousePosition records the last pointer cell for cursor-shape computation
// in View(). Updated on every mouse message before routing handlers.
func (b *BaseModel) noteMousePosition(x, y int) {
	b.lastMouseX = x
	b.lastMouseY = y
}

// desiredCursorShape returns the OSC-22 shape for the current hover state.
// Activatable targets only: menu-bar cells, dropdown rows with Index >= 0,
// and process chips. Everything else (separators, overflow indicators, help
// box, content rows) stays default.
func (b *BaseModel) desiredCursorShape() cursorShape {
	// Modal modes make chrome clicks no-ops — a hand there is a false
	// affordance (pin's help-box rationale covers chrome under the modal).
	if b.mode == ModeHelp {
		return cursorShapeDefault
	}
	x, y := b.lastMouseX, b.lastMouseY
	if x < 0 || y < 0 {
		return cursorShapeDefault
	}

	hits := b.mustHits()

	if b.settings.MenuBar {
		for _, h := range hits.menuCells {
			if h.Rect.Contains(x, y) {
				return cursorShapePointer
			}
		}
	}

	if hits.hasDropdown && b.menuOpen() && MenuID(b.openMenu) == hits.dropdown.Menu {
		for _, row := range hits.dropdown.Rows {
			if row.Rect.Contains(x, y) && row.Index >= 0 {
				return cursorShapePointer
			}
		}
	}

	// Chips are clickable only in normal mode (text-entry modes drop the
	// press); menu cells above stay activatable in every mode.
	if b.mode == ModeNormal && b.processPanelHit(x, y) >= 0 {
		return cursorShapePointer
	}

	return cursorShapeDefault
}

// appendCursorShape prepends an OSC-22 sequence when the desired pointer shape
// changed. bubbletea owns stdout during the session, so View emits the escape
// as part of the frame (verified zero-width under x/ansi.StringWidth).
func (b *BaseModel) appendCursorShape(frame string) string {
	seq := b.mustPointerShape().takeSequence(b.desiredCursorShape())
	if seq == "" {
		return frame
	}
	return seq + frame
}

// resetTerminalPointer restores the default mouse pointer after the TUI exits.
// Called from RunClient's defer — outside bubbletea, direct to os.Stdout.
func resetTerminalPointer() {
	_, _ = os.Stdout.WriteString(osc22Default)
}
