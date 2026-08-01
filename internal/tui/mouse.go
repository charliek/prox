package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/proxy"
)

// nowFunc is the clock for double-click detection (stubbed in tests).
var nowFunc = time.Now

const (
	wheelScrollRows        = 3
	mouseDoubleClickWindow = 500 * time.Millisecond
)

// clearRequestClickTracker disarms the manual requests double-click detector
// (plan 022 §2 — wheel, menu capture, help open/dismiss clear; free motion preserves).
func (b *BaseModel) clearRequestClickTracker() {
	b.lastRequestClickIdx = -1
}

// handleContentMouse routes non-menu mouse input after handleMenuMouse. Returns
// whether the event was consumed and an optional command (requests paging).
// Call only in ModeNormal with menu closed — handleMenuMouse consumes wheel
// whenever a menu is open (plan 022 WS3), so this never sees menu-open wheels.
func (m *ClientModel) handleContentMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return false, nil
	}

	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		m.clearRequestClickTracker()
		dir := wheelScrollRows
		if msg.Button == tea.MouseButtonWheelUp {
			dir = -dir
		}
		return m.handleMouseWheel(dir)
	}

	if msg.Button != tea.MouseButtonLeft {
		return false, nil
	}

	// Footer band: consumed no-op — never opens menus or moves the cursor
	// (plan 023 B2). Menu-open outside clicks are handled above in
	// handleMenuMouse before this runs.
	if m.isFooterY(msg.Y) {
		return true, nil
	}

	if idx := m.processPanelHit(msg.X, msg.Y); idx >= 0 {
		m.toggleSoloProcess(idx)
		m.updateViewport()
		return true, nil
	}

	if local, ok := m.viewportLocalRow(msg.Y); ok {
		contentRow := m.viewport.YOffset + local
		switch m.viewMode {
		case ViewModeLogs:
			m.clickLogRow(contentRow)
			return true, nil // consume all logs-viewport clicks (WS11)
		case ViewModeRequests:
			if cmd, ok := m.clickRequestRow(contentRow); ok {
				return true, cmd
			}
		case ViewModeRequestDetail:
			// Clicks in the detail viewport are ignored (scroll is wheel-only).
		}
	}

	return false, nil
}

func (m *ClientModel) handleMouseWheel(delta int) (bool, tea.Cmd) {
	switch m.viewMode {
	case ViewModeRequests:
		m.moveRequestCursor(delta)
		return true, nil
	case ViewModeRequestDetail:
		if delta < 0 {
			for i := 0; i < -delta; i++ {
				m.viewport.LineUp(1)
			}
		} else {
			for i := 0; i < delta; i++ {
				m.viewport.LineDown(1)
			}
		}
		return true, nil
	default: // ViewModeLogs
		if delta < 0 {
			for i := 0; i < -delta; i++ {
				m.viewport.LineUp(1)
			}
			m.followMode = false
		} else {
			for i := 0; i < delta; i++ {
				m.viewport.LineDown(1)
			}
			if m.viewport.AtBottom() {
				m.followMode = true
			}
		}
		return true, nil
	}
}

// viewportLocalRow maps a frame Y coordinate to a 0-based row within the
// viewport window. ok is false when Y is outside the viewport chrome.
func (b *BaseModel) viewportLocalRow(y int) (local int, ok bool) {
	r := b.contentRect()
	if y < r.Y || y >= r.Y+r.H {
		return 0, false
	}
	return y - r.Y, true
}

// processPanelRowY is the frame Y of the process-panel content row.
func (b *BaseModel) processPanelRowY() (int, bool) {
	if !b.settings.ProcessPanel {
		return 0, false
	}
	y := 0
	if b.settings.MenuBar {
		y++
	}
	return y, true
}

// processPanelHit returns the process index under (x,y), or -1. Solo toggles
// mirror the 1-9 keys (logs view only). Rects are recorded per frame in
// processPanel; the ProcessPanel gate is belt-and-braces with resetFrame
// (plan 023 A1 / B1 — stale chips after `p` hid the panel).
func (b *BaseModel) processPanelHit(x, y int) int {
	if !b.settings.ProcessPanel {
		return -1
	}
	if b.viewMode != ViewModeLogs {
		return -1
	}
	for _, h := range b.mustHits().chips {
		if h.Rect.Contains(x, y) {
			return h.Index
		}
	}
	return -1
}

func (b *BaseModel) toggleSoloProcess(idx int) {
	if idx < 0 || idx >= len(b.processes) {
		return
	}
	name := b.processes[idx].Name
	if b.soloProcess == name {
		b.soloProcess = ""
	} else {
		b.soloProcess = name
	}
}

// clickLogRow parks the logs cursor on the entry at display contentRow and
// disengages follow (the parked cursor is also the y-copy target — C10).
// Strict span lookup, no clamp fallback: clicks on blank viewport area below
// the last entry must not park the cursor on it.
func (b *BaseModel) clickLogRow(contentRow int) {
	entries := b.filteredEntries()
	if len(entries) == 0 {
		return
	}
	idx := -1
	for i, e := range entries {
		if sp, ok := b.logRowSpans[e.DisplaySeq]; ok && contentRow >= sp.First && contentRow <= sp.Last {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	b.setLogCursor(entries, idx)
	b.followMode = false
	b.updateViewport()
}

// clickRequestRow handles a single or double click on a requests row. Returns
// a detail-fetch command on double-click (manual 500ms timing — bubbletea has
// no double-click event; plan 021 WS11 / Codex #5).
func (m *ClientModel) clickRequestRow(contentRow int) (tea.Cmd, bool) {
	requests := m.filteredProxyRequests()
	if contentRow < 0 || contentRow >= len(requests) {
		return nil, false
	}
	now := nowFunc()
	if m.lastRequestClickIdx == contentRow && now.Sub(m.lastRequestClickAt) <= mouseDoubleClickWindow {
		m.lastRequestClickIdx = -1
		return m.beginRequestDetail(requests[contentRow].ID), true
	}
	m.lastRequestClickIdx = contentRow
	m.lastRequestClickAt = now
	m.placeRequestCursor(requests, contentRow)
	return nil, true
}

// placeRequestCursor moves the requests cursor with the same follow semantics
// as j/k: landing on the newest row re-engages follow, any other row disengages.
func (b *BaseModel) placeRequestCursor(requests []proxy.RequestRecord, idx int) {
	n := len(requests)
	b.setRequestCursor(requests, idx)
	if n > 0 && idx == n-1 {
		b.followMode = true
	} else {
		b.followMode = false
	}
	b.updateViewport()
}
