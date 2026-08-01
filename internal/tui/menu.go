package tui

import (
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// MenuID identifies a top-level menu-bar cell (plan 021 WS3).
type MenuID int

const (
	MenuView MenuID = iota
	MenuFilter
	MenuTheme
)

// menuOrder is left-to-right draw / sibling-switch order.
var menuOrder = []MenuID{MenuView, MenuFilter, MenuTheme}

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

// MenuCommand is dispatched by the model to the SAME functions the keys call
// (no behavior duplication — plan 021 WS3).
type MenuCommand string

const (
	MenuCmdSetLogs            MenuCommand = "set-logs"
	MenuCmdSetRequests        MenuCommand = "set-requests"
	MenuCmdToggleFollow       MenuCommand = "toggle-follow"
	MenuCmdToggleProcessPanel MenuCommand = "toggle-process-panel"
	MenuCmdToggleTimestamps   MenuCommand = "toggle-timestamps"
	MenuCmdToggleWrap         MenuCommand = "toggle-wrap"

	menuCmdSetThemePrefix = "set-theme:"
)

// MenuItem is one dropdown row. Checked/Selected are nil for plain rows;
// Separator skips highlight navigation (strix on_key_menu).
type MenuItem struct {
	Label     string
	Checked   *bool // checkbox marker
	Selected  *bool // radio marker
	Separator bool
	Cmd       MenuCommand
}

// HitRect is a mouse hit target in frame coordinates (0-based, inclusive origin).
type HitRect struct {
	X, Y, W, H int
}

func (r HitRect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

// menuOpen reports whether a dropdown is open.
func (b *BaseModel) menuOpen() bool {
	return b.openMenu >= 0
}

func (b *BaseModel) closeMenu() {
	b.openMenu = -1
	b.menuHighlight = 0
	b.ensureHits().hasDropdown = false
}

func (b *BaseModel) clearMenuHitRects() {
	h := b.ensureHits()
	h.menuCells = h.menuCells[:0]
	// Stale dropdown hits must never match after close (strix app.rs:1152).
	if !b.menuOpen() {
		h.hasDropdown = false
	}
}

// openMenuFirst opens menu with its first activatable row highlighted.
func (b *BaseModel) openMenuFirst(id MenuID) {
	b.openMenu = int(id)
	b.menuHighlight = b.menuFirstSelectable(id)
}

func (b *BaseModel) menuFirstSelectable(id MenuID) int {
	items := b.menuItems(id)
	for i, it := range items {
		if !it.Separator {
			return i
		}
	}
	return 0
}

func menuCmdSetTheme(name string) MenuCommand {
	return MenuCommand(menuCmdSetThemePrefix + name)
}

func parseSetThemeCmd(cmd MenuCommand) (name string, ok bool) {
	s := string(cmd)
	if !strings.HasPrefix(s, menuCmdSetThemePrefix) {
		return "", false
	}
	return strings.TrimPrefix(s, menuCmdSetThemePrefix), true
}

// menuItems builds rows live from state so markers never drift (WS3).
// View menu (WS4): Logs/Requests radios, sep, Process panel / Timestamps /
// Wrap lines / Follow checks. Filter menu (WS8): per-view check/radio rows.
// Theme menu (WS5): preset radios then user stems.
func (b *BaseModel) menuItems(id MenuID) []MenuItem {
	switch id {
	case MenuView:
		logsOn := b.viewMode == ViewModeLogs
		reqsOn := b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail
		panel := b.settings.ProcessPanel
		timestamps := b.settings.Timestamps
		wrap := b.settings.Wrap
		follow := b.followMode
		return []MenuItem{
			{Label: "Logs", Selected: &logsOn, Cmd: MenuCmdSetLogs},
			{Label: "Requests", Selected: &reqsOn, Cmd: MenuCmdSetRequests},
			{Separator: true},
			{Label: "Process panel", Checked: &panel, Cmd: MenuCmdToggleProcessPanel},
			{Label: "Timestamps", Checked: &timestamps, Cmd: MenuCmdToggleTimestamps},
			{Label: "Wrap lines", Checked: &wrap, Cmd: MenuCmdToggleWrap},
			{Label: "Follow", Checked: &follow, Cmd: MenuCmdToggleFollow},
		}
	case MenuTheme:
		names := AvailableThemes()
		current := CurrentThemeName()
		items := make([]MenuItem, len(names))
		selected := make([]bool, len(names))
		for i, name := range names {
			selected[i] = name == current
			items[i] = MenuItem{
				Label:    name,
				Selected: &selected[i],
				Cmd:      menuCmdSetTheme(name),
			}
		}
		return items
	case MenuFilter:
		return b.filterMenuItems()
	default:
		return nil
	}
}

func (b *BaseModel) menuStep(id MenuID, item int, down bool) int {
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
	for range n {
		if down {
			idx = (idx + 1) % n
		} else {
			idx = (idx + n - 1) % n
		}
		if !items[idx].Separator {
			break
		}
	}
	return idx
}

func (b *BaseModel) openMenuSibling(next bool) {
	if !b.menuOpen() {
		return
	}
	pos := 0
	for i, id := range menuOrder {
		if int(id) == b.openMenu {
			pos = i
			break
		}
	}
	n := len(menuOrder)
	if next {
		pos = (pos + 1) % n
	} else {
		pos = (pos + n - 1) % n
	}
	b.openMenuFirst(menuOrder[pos])
}

// handleMenuKey routes keys while a dropdown is open (strix on_key_menu exactly).
// Every key is consumed; non-nav keys close without re-dispatch (Codex #4).
// Returns a command from an activated item (e.g. settings-save flash clear).
func (b *BaseModel) handleMenuKey(msg tea.KeyMsg) tea.Cmd {
	if !b.menuOpen() {
		return nil
	}
	id := MenuID(b.openMenu)
	switch msg.String() {
	case "left", "shift+tab":
		b.openMenuSibling(false)
	case "right", "tab":
		b.openMenuSibling(true)
	case "up", "k":
		b.menuHighlight = b.menuStep(id, b.menuHighlight, false)
	case "down", "j":
		b.menuHighlight = b.menuStep(id, b.menuHighlight, true)
	case "enter", " ":
		cmd := b.activateMenuItem(id, b.menuHighlight)
		b.closeMenu()
		return cmd
	default:
		// Esc and every other key: close, consumed, never re-dispatched.
		b.closeMenu()
	}
	return nil
}

func (b *BaseModel) activateMenuItem(id MenuID, item int) tea.Cmd {
	items := b.menuItems(id)
	if item < 0 || item >= len(items) {
		return nil
	}
	it := items[item]
	if it.Separator || it.Cmd == "" {
		return nil
	}
	return b.activateMenuCommand(it.Cmd)
}

func (b *BaseModel) activateMenuCommand(cmd MenuCommand) tea.Cmd {
	if name, ok := parseSetThemeCmd(cmd); ok {
		return b.setThemeByName(name)
	}
	switch cmd {
	case MenuCmdSetLogs:
		b.setViewMode(ViewModeLogs)
	case MenuCmdSetRequests:
		b.setViewMode(ViewModeRequests)
	case MenuCmdToggleFollow:
		b.toggleFollow()
	case MenuCmdToggleProcessPanel:
		return b.toggleProcessPanel()
	case MenuCmdToggleTimestamps:
		return b.toggleTimestamps()
	case MenuCmdToggleWrap:
		return b.toggleWrap()
	}
	if b.activateFilterMenuCommand(cmd) {
		return nil
	}
	return nil
}

// toggleMenuBar flips settings.MenuBar, persists, and relayouts (Codex #8).
func (b *BaseModel) toggleMenuBar() tea.Cmd {
	b.settings.MenuBar = !b.settings.MenuBar
	if !b.settings.MenuBar {
		b.closeMenu()
	}
	b.relayout()
	if err := SaveSettings(b.settings); err != nil {
		return b.setStatusFlash("settings not saved: "+err.Error(), statusFlashClearDelay)
	}
	return nil
}

// resolveProjectName returns opts.ProjectName, or the cwd base as fallback (WS3).
func resolveProjectName(name string) string {
	if name != "" {
		return name
	}
	cwd, err := filepathAbs(".")
	if err != nil || cwd == "" {
		return "prox"
	}
	base := filepath.Base(cwd)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "prox"
	}
	return base
}

// filepathAbs is os.Getwd-backed; separated for tests that want to stub later.
var filepathAbs = func(path string) (string, error) {
	return filepath.Abs(path)
}

// renderMenuBar draws the menu row and records cell hit-rects.
// Layout (WS3): left `prox <project>`, right-aligned View/Filter/Theme cells.
func (b *BaseModel) renderMenuBar() string {
	th := CurrentTheme()
	rowBG := lipgloss.NewStyle().Background(th.HeaderBG).Foreground(th.HeaderFG)

	project := b.projectName
	if project == "" {
		project = "prox"
	}
	left := " prox  " + project + " "

	type cell struct {
		id   MenuID
		text string
	}
	cells := make([]cell, 0, len(menuOrder))
	cellsW := 0
	for _, id := range menuOrder {
		text := menuCellText(id)
		cells = append(cells, cell{id: id, text: text})
		cellsW += ansi.StringWidth(text)
	}

	leftW := ansi.StringWidth(left)
	gap := b.width - leftW - cellsW
	if gap < 1 {
		gap = 1
	}

	var bld strings.Builder
	bld.WriteString(rowBG.Render(left))
	bld.WriteString(rowBG.Render(strings.Repeat(" ", gap)))

	x := leftW + gap
	y := 0 // menu bar is always row 0 when visible; caller places it first
	hits := b.ensureHits()
	for _, c := range cells {
		w := ansi.StringWidth(c.text)
		style := rowBG
		if b.menuOpen() && MenuID(b.openMenu) == c.id {
			style = lipgloss.NewStyle().
				Background(th.SelectionBG).
				Foreground(th.SelectionFG)
		}
		bld.WriteString(style.Render(c.text))
		hits.menuCells = append(hits.menuCells, menuCellHit{
			ID:   c.id,
			Rect: HitRect{X: x, Y: y, W: w, H: 1},
		})
		x += w
	}

	line := bld.String()
	return padFrameRow(line, b.width)
}

// renderKeyHints is the dim footer under the status bar (plan 021 WS11).
func (b *BaseModel) renderKeyHints() string {
	return padFrameRow(s.Dim.Render("m menu · / search · s filter · ? help · q quit"), b.width)
}

// dropdownBoxRows builds the opaque themed dropdown rows (uniform width) and
// records row hit-rects. Returns box rows, anchor x, and box width.
func (b *BaseModel) dropdownBoxRows() (rows []string, boxX, boxW int) {
	if !b.menuOpen() || !b.settings.MenuBar {
		return nil, 0, 0
	}
	id := MenuID(b.openMenu)
	items := b.menuItems(id)

	th := CurrentTheme()
	const pad = 1 // 1-space padding (strix plain look / user brief)

	// Inner content width: marker gutter + label.
	maxInner := 0
	for _, it := range items {
		if it.Separator {
			continue
		}
		w := ansi.StringWidth(menuMarker(it) + " " + it.Label)
		if w > maxInner {
			maxInner = w
		}
	}
	if maxInner == 0 {
		maxInner = ansi.StringWidth("(coming soon)")
	}
	innerW := maxInner + pad*2
	boxW = innerW
	if boxW > b.width {
		boxW = b.width
		innerW = boxW
	}

	// Anchor under the cell; clamp so the box stays on-screen.
	boxX = 0
	for _, h := range b.ensureHits().menuCells {
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

	boxTop := 1 // immediately under the menu bar
	if !b.settings.MenuBar {
		boxTop = 0
	}

	hitRows := make([]menuRowHit, 0, len(items))
	rows = make([]string, 0, len(items))
	for i, it := range items {
		var content string
		switch {
		case it.Separator:
			content = strings.Repeat("─", max(1, innerW-pad*2))
			content = strings.Repeat(" ", pad) + content + strings.Repeat(" ", pad)
			content = ansi.Cut(content, 0, innerW)
			style := lipgloss.NewStyle().Foreground(th.Dim).Background(th.BG)
			row := padFrameRow(style.Render(content), boxW)
			rows = append(rows, row)
			hitRows = append(hitRows, menuRowHit{
				Rect: HitRect{X: boxX, Y: boxTop + i, W: boxW, H: 1},
			})
		default:
			label := menuMarker(it) + " " + it.Label
			if it.Cmd == "" && it.Label == "(coming soon)" {
				label = it.Label
			}
			content = strings.Repeat(" ", pad) + label
			content = padFrameRow(content, innerW)

			var style lipgloss.Style
			if i == b.menuHighlight && it.Cmd != "" {
				style = lipgloss.NewStyle().
					Background(th.SelectionBG).
					Foreground(th.SelectionFG)
			} else if it.Cmd == "" {
				style = lipgloss.NewStyle().Foreground(th.Dim).Background(th.BG)
			} else {
				style = lipgloss.NewStyle().Foreground(th.FG).Background(th.BG)
			}
			row := padFrameRow(style.Render(ansi.Cut(content, 0, innerW)), boxW)
			rows = append(rows, row)
			hitRows = append(hitRows, menuRowHit{
				Cmd:  it.Cmd,
				Rect: HitRect{X: boxX, Y: boxTop + i, W: boxW, H: 1},
			})
		}
	}

	hits := b.ensureHits()
	hits.dropdown = menuDropdownHit{
		Menu:   id,
		Bounds: HitRect{X: boxX, Y: boxTop, W: boxW, H: len(rows)},
		Rows:   hitRows,
	}
	hits.hasDropdown = true
	return rows, boxX, boxW
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
	boxTop := 1
	if !b.settings.MenuBar {
		boxTop = 0
	}
	return overlayLines(lines, boxX, boxTop, b.width, rows)
}

// handleMenuMouse handles menu-bar / dropdown clicks. Returns whether the event
// is fully consumed and an optional command from an activated item.
func (b *BaseModel) handleMenuMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return false, nil
	}
	x, y := msg.X, msg.Y
	hits := b.ensureHits()

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
		d := &hits.dropdown
		fresh := b.menuOpen() && MenuID(b.openMenu) == d.Menu
		if !fresh {
			b.closeMenu()
			return true, nil
		}
		for _, row := range d.Rows {
			if row.Rect.Contains(x, y) {
				if row.Cmd != "" {
					cmd := b.activateMenuCommand(row.Cmd)
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
		b.closeMenu()
		return true, nil
	}
	return false, nil
}
