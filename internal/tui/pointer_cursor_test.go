package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func TestOSC22_StringWidthZero(t *testing.T) {
	for _, seq := range []string{osc22Pointer, osc22Default} {
		assert.Equal(t, 0, ansi.StringWidth(seq), "OSC-22 must be zero-width: %q", seq)
	}
	// Prepending to a row must not change display width (frame-contract path).
	row := strings.Repeat("x", 40)
	assert.Equal(t, ansi.StringWidth(row), ansi.StringWidth(osc22Pointer+row))
}

func TestPointerShape_EmitOnlyOnChange(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Process: "web", Line: "hello",
	}))
	cache := m.mustPointerShape()
	cache.record = true
	_ = m.View()
	hits := m.mustHits()
	require.NotEmpty(t, hits.menuCells)
	cell := hits.menuCells[0]

	// First ready frame with no mouse position emits default once.
	require.Len(t, cache.emissions, 1)
	assert.Equal(t, osc22Default, cache.emissions[0])

	// First explicit hover should emit pointer once.
	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	frame := m.View()
	require.Len(t, cache.emissions, 2)
	assert.Equal(t, osc22Pointer, cache.emissions[1])
	assert.True(t, strings.HasPrefix(frame, osc22Pointer))

	// Repeated hover at same cell: no new emission.
	m = clientUpdate(m, motionAt(cell.Rect.X+1, cell.Rect.Y))
	frame2 := m.View()
	assert.Len(t, cache.emissions, 2, "still pointer — dedup")
	assert.False(t, strings.HasPrefix(frame2, osc22Pointer), "no sequence when unchanged")

	// Leave menu bar → default once.
	m = clientUpdate(m, motionAt(40, 15))
	_ = m.View()
	require.Len(t, cache.emissions, 3)
	assert.Equal(t, osc22Default, cache.emissions[2])

	// Stay over content: no further emissions.
	m = clientUpdate(m, motionAt(41, 16))
	_ = m.View()
	assert.Len(t, cache.emissions, 3)

	// Re-enter menu cell → pointer again.
	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	frame3 := m.View()
	require.Len(t, cache.emissions, 4)
	assert.Equal(t, osc22Pointer, cache.emissions[3])
	assert.True(t, strings.HasPrefix(frame3, osc22Pointer))
}

func TestPointerShape_DropdownActivatableOnly(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('v'))
	_ = m.View()
	hits := m.mustHits()
	require.True(t, hits.hasDropdown)

	var sep, activatable menuRowHit
	foundSep := false
	for _, row := range hits.dropdown.Rows {
		if row.Index < 0 {
			sep = row
			foundSep = true
		}
		if row.Index >= 0 {
			activatable = row
		}
	}
	require.True(t, foundSep, "need a non-activatable row")
	require.GreaterOrEqual(t, activatable.Index, 0)

	cache := m.mustPointerShape()
	cache.record = true

	m = clientUpdate(m, motionAt(activatable.Rect.X, activatable.Rect.Y))
	_ = m.View()
	require.NotEmpty(t, cache.emissions)
	assert.Equal(t, osc22Pointer, cache.emissions[len(cache.emissions)-1])

	before := len(cache.emissions)
	m = clientUpdate(m, motionAt(sep.Rect.X, sep.Rect.Y))
	_ = m.View()
	require.Greater(t, len(cache.emissions), before)
	assert.Equal(t, osc22Default, cache.emissions[len(cache.emissions)-1])
}

func TestPointerShape_ProcessChipHover(t *testing.T) {
	m := newTestModel()
	m.settings.ProcessPanel = true
	m.processes = []domain.ProcessInfo{{Name: "web", State: domain.ProcessStateRunning}}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	hits := m.mustHits()
	require.NotEmpty(t, hits.chips)
	chip := hits.chips[0]

	cache := m.mustPointerShape()
	cache.record = true
	m = clientUpdate(m, motionAt(chip.Rect.X, chip.Rect.Y))
	_ = m.View()
	require.NotEmpty(t, cache.emissions)
	assert.Equal(t, osc22Pointer, cache.emissions[len(cache.emissions)-1])
}

func TestPointerShape_HelpModalNoPointer(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)
	_ = m.View()

	cache := m.mustPointerShape()
	cache.record = true
	// Motion over help body (not a menu/chip target).
	m = clientUpdate(m, motionAt(40, 12))
	_ = m.View()
	// Either no emission (still default/unset) or default — never pointer.
	for _, seq := range cache.emissions {
		assert.NotEqual(t, osc22Pointer, seq)
	}

	// Motion over a menu-bar cell while help is open: the click would be a
	// modal no-op, so the shape must stay default (no false affordance).
	mustMenuBarCell := m.mustHits().menuCells
	require.NotEmpty(t, mustMenuBarCell)
	cell := mustMenuBarCell[0]
	m = clientUpdate(m, motionAt(cell.Rect.X, cell.Rect.Y))
	_ = m.View()
	for _, seq := range cache.emissions {
		assert.NotEqual(t, osc22Pointer, seq, "menu cell under modal must not show pointer")
	}
}
