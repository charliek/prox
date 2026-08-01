package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The help box is the whole frame; before renderHelp windowed it, an
// over-tall box scrolled its top sections (title, navigation, query cheat
// sheet) off-screen at small terminal heights (Phase 5 verification find).
func TestHelp_WindowsToTerminalHeight(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m.mode = ModeHelp

	view := m.helpView()
	assert.LessOrEqual(t, len(strings.Split(view, "\n")), 20,
		"help box must not exceed the frame height")
	assert.Contains(t, view, "Prox - Process Manager", "title must stay visible")
	assert.Contains(t, view, "j/k scroll")
}

func TestHelp_ScrollKeysMoveWindow(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m.mode = ModeHelp

	require.Contains(t, m.helpView(), "Prox - Process Manager")

	m.handleHelpKey(keyRune('j'))
	assert.Equal(t, 1, m.helpOffset)
	scrolled := m.helpView()
	assert.NotContains(t, scrolled, "Prox - Process Manager", "top scrolled off")
	assert.Contains(t, scrolled, "lines 2-")

	m.handleHelpKey(keyRune('k'))
	assert.Equal(t, 0, m.helpOffset)

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyPgDown})
	assert.Equal(t, m.helpPageStep(), m.helpOffset)

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEnd})
	assert.Greater(t, m.helpOffset, 0, "sentinel; render clamps")
	endView := m.helpView()
	assert.Contains(t, endView, "closes help", "footer visible at bottom")
	assert.NotContains(t, endView, "Prox - Process Manager")
}

func TestHelp_CloseResetsOffset(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 90, Height: 20})
	m.mode = ModeHelp
	m.helpOffset = 3

	m.handleHelpKey(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, 0, m.helpOffset)
}

func TestHelp_FitsWithoutWindowing(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 60})
	m.mode = ModeHelp

	view := m.helpView()
	assert.Contains(t, view, "Prox - Process Manager")
	assert.Contains(t, view, "closes help")
	assert.NotContains(t, view, "lines 1-", "no scroll indicator when it fits")
}
