package tui

import (
	"fmt"
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
	b.menuWindow = 0
	b.hoveredMenuCell = -1
	// Immediate invalidation: a menu can close in Update before the next
	// render, so frame-top resetFrame alone is not enough (plan 023 A1).
	h := b.mustHits()
	h.dropdown = menuDropdownHit{}
	h.hasDropdown = false
}

// openMenuFirst opens menu with its first activatable row highlighted.
func (b *BaseModel) openMenuFirst(id MenuID) {
	b.openMenu = int(id)
	b.menuHighlight = b.menuFirstSelectable(id)
	b.menuWindow = 0
	b.hoveredMenuCell = -1
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

// menuStep moves the highlight by one selectable row. wrap=true (keyboard)
// wraps modulo n skipping separators; wrap=false (wheel) clamps at the ends.
func (b *BaseModel) menuStep(id MenuID, item int, down bool) int {
	return b.menuStepDir(id, item, down, true)
}

func (b *BaseModel) menuStepClamp(id MenuID, item int, down bool) int {
	return b.menuStepDir(id, item, down, false)
}

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
			if !items[j].Separator {
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
		if !items[idx].Separator {
			break
		}
	}
	return idx
}

// menuReservedBottom is the footer band — dropdowns never cover it (plan 023 B2).
const menuReservedBottom = 1

func (b *BaseModel) menuBoxTop() int {
	if b.settings.MenuBar {
		return 1
	}
	return 0
}

func (b *BaseModel) menuAvail() int {
	return b.height - b.menuBoxTop() - menuReservedBottom
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
// Every key is consumed; non-nav keys close without re-dispatch (Codex #4),
// except "?" which closes the menu AND opens help atomically (plan 022 WS4 /
// panel correction 2 — closing help does NOT restore the menu).
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
		prev := b.menuHighlight
		b.menuHighlight = b.menuStep(id, b.menuHighlight, false)
		b.followMenuWindow(prev, false)
	case "down", "j":
		prev := b.menuHighlight
		b.menuHighlight = b.menuStep(id, b.menuHighlight, true)
		b.followMenuWindow(prev, true)
	case "enter", " ":
		cmd := b.activateMenuItem(id, b.menuHighlight)
		b.closeMenu()
		return cmd
	case "?":
		b.enterHelp()
		return nil
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
		return b.setStatusFlash(footerError("settings not saved: "+err.Error()), flashSettingsSave, statusFlashClearDelay)
	}
	return nil
}

// ConfigPathProjectName derives the menu-bar project label from the absolute
// path of the resolved config file (plan 023 B3: basename of its directory).
func ConfigPathProjectName(absConfigPath string) string {
	if absConfigPath == "" {
		return ""
	}
	return dirBaseName(filepath.Dir(absConfigPath))
}

// StatusProjectName derives the menu-bar project label from GET /status
// project_dir (plan 023 B3). Empty project_dir returns "" so callers fall
// back to the cwd basename via resolveProjectName.
func StatusProjectName(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	return dirBaseName(projectDir)
}

func dirBaseName(dir string) string {
	base := filepath.Base(dir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
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
	if base := dirBaseName(cwd); base != "" {
		return base
	}
	return "prox"
}

// filepathAbs is os.Getwd-backed; separated for tests that want to stub later.
var filepathAbs = func(path string) (string, error) {
	return filepath.Abs(path)
}

// renderMenuBar draws the menu row and records cell hit-rects.
// Layout (plan 023 B3): `prox` (bold HeaderFG) + Dim project name + cells,
// left-aligned; the remainder of the row is HeaderBG fill.
func (b *BaseModel) renderMenuBar() string {
	th := CurrentTheme()
	rowBG := lipgloss.NewStyle().Background(th.HeaderBG)
	brandStyle := lipgloss.NewStyle().
		Foreground(th.HeaderFG).
		Background(th.HeaderBG).
		Bold(true)
	dimStyle := lipgloss.NewStyle().
		Foreground(th.Dim).
		Background(th.HeaderBG)
	closedStyle := lipgloss.NewStyle().
		Foreground(th.Title).
		Background(th.HeaderBG)
	selStyle := lipgloss.NewStyle().
		Foreground(th.SelectionFG).
		Background(th.SelectionBG)

	var bld strings.Builder
	bld.WriteString(rowBG.Render(" "))
	bld.WriteString(brandStyle.Render("prox"))
	if b.projectName != "" {
		bld.WriteString(rowBG.Render(" "))
		bld.WriteString(dimStyle.Render(b.projectName))
	}
	bld.WriteString(rowBG.Render(" "))

	x := ansi.StringWidth(bld.String())
	y := 0 // menu bar is always row 0 when visible; caller places it first
	hits := b.mustHits()
	for _, id := range menuOrder {
		text := menuCellText(id)
		w := ansi.StringWidth(text)
		style := closedStyle
		if b.menuOpen() && MenuID(b.openMenu) == id {
			style = selStyle
		} else if !b.menuOpen() && b.hoveredMenuCell == int(id) {
			style = selStyle
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
		line += rowBG.Render(strings.Repeat(" ", b.width-w))
	case w > b.width:
		line = ansi.Cut(line, 0, b.width)
	}
	return line
}

// dropdownBoxRows builds the windowed dropdown rows (indicators + visible
// items) and records hit-rects matching EXACTLY those rows; activatable rows
// carry their full-list Index so hover/click map visible → item (plan 022 WS3).
func (b *BaseModel) dropdownBoxRows() (rows []string, boxX, boxW int) {
	if !b.menuOpen() || !b.settings.MenuBar {
		return nil, 0, 0
	}
	id := MenuID(b.openMenu)
	items := b.menuItems(id)

	th := CurrentTheme()
	const pad = 1 // 1-space padding (strix plain look / user brief)
	n := len(items)

	// Inner content width: marker gutter + label (and indicator text when the
	// list actually scrolls — indicators are a prox addition, plan 022 WS3).
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
	if avail := b.menuAvail(); n > avail && avail >= 4 {
		// Widest possible "… N more …" for this list.
		indW := ansi.StringWidth(fmt.Sprintf("… %d more …", n))
		if indW > maxInner {
			maxInner = indW
		}
	}
	innerW := maxInner + pad*2
	boxW = innerW
	if boxW > b.width {
		boxW = b.width
		innerW = boxW
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
		// Degenerate: menu state open, no rows / no hits (plan 022 WS3).
		h := b.mustHits()
		h.dropdown = menuDropdownHit{}
		h.hasDropdown = false
		return nil, boxX, boxW
	}

	visStart, visEnd, topInd, botInd := menuWindowLayout(n, avail,
		deriveMenuWindowStart(n, avail, b.menuWindow, b.menuHighlight))

	dimStyle := lipgloss.NewStyle().Foreground(th.Dim).Background(th.BG)
	hitRows := make([]menuRowHit, 0, avail)
	rows = make([]string, 0, avail)
	rowY := boxTop

	renderInd := func(hidden int) {
		label := fmt.Sprintf("… %d more …", hidden)
		content := strings.Repeat(" ", pad) + label
		content = padFrameRow(content, innerW)
		row := padFrameRow(dimStyle.Render(ansi.Cut(content, 0, innerW)), boxW)
		rows = append(rows, row)
		hitRows = append(hitRows, menuRowHit{
			Index: -1,
			Rect:  HitRect{X: boxX, Y: rowY, W: boxW, H: 1},
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
			row := padFrameRow(dimStyle.Render(content), boxW)
			rows = append(rows, row)
			hitRows = append(hitRows, menuRowHit{
				Index: -1,
				Rect:  HitRect{X: boxX, Y: rowY, W: boxW, H: 1},
			})
		default:
			label := menuMarker(it) + " " + it.Label
			if it.Cmd == "" && it.Label == "(coming soon)" {
				label = it.Label
			}
			content := strings.Repeat(" ", pad) + label
			content = padFrameRow(content, innerW)

			var style lipgloss.Style
			if i == b.menuHighlight && it.Cmd != "" {
				style = lipgloss.NewStyle().
					Background(th.SelectionBG).
					Foreground(th.SelectionFG)
			} else if it.Cmd == "" {
				style = dimStyle
			} else {
				style = lipgloss.NewStyle().Foreground(th.FG).Background(th.BG)
			}
			row := padFrameRow(style.Render(ansi.Cut(content, 0, innerW)), boxW)
			rows = append(rows, row)
			cmd := it.Cmd
			idx := i
			if cmd == "" {
				idx = -1
			}
			hitRows = append(hitRows, menuRowHit{
				Cmd:   cmd,
				Index: idx,
				Rect:  HitRect{X: boxX, Y: rowY, W: boxW, H: 1},
			})
		}
		rowY++
	}

	if botInd {
		renderInd(n - visEnd)
	}

	hits := b.mustHits()
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
	return overlayLines(lines, boxX, b.menuBoxTop(), b.width, rows)
}

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
		b.menuHighlight = b.menuStepClamp(id, b.menuHighlight, down)
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
				if row.Cmd != "" && row.Index >= 0 {
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
