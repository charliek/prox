package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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

	MenuCmdToggleColTime     MenuCommand = "toggle-col-time"
	MenuCmdToggleColHost     MenuCommand = "toggle-col-host"
	MenuCmdToggleColMethod   MenuCommand = "toggle-col-method"
	MenuCmdToggleColStatus   MenuCommand = "toggle-col-status"
	MenuCmdToggleColDuration MenuCommand = "toggle-col-duration"
	MenuCmdToggleColID       MenuCommand = "toggle-col-id"

	menuCmdSetThemePrefix = "set-theme:"
)

// MenuItem is one dropdown row. Checked/Selected are nil for plain rows;
// Separator skips highlight navigation (strix on_key_menu). Hint is an
// optional right-aligned keyboard shortcut (plan 023 B4); empty when none.
type MenuItem struct {
	Label     string
	Hint      string
	Checked   *bool // checkbox marker
	Selected  *bool // radio marker
	Separator bool
	Cmd       MenuCommand
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
	b.themeMenuNames = nil
	// Immediate invalidation: a menu can close in Update before the next
	// render, so frame-top resetFrame alone is not enough (plan 023 A1).
	h := b.mustHits()
	h.dropdown = menuDropdownHit{}
	h.hasDropdown = false
}

// openMenuFirst opens menu with its first activatable row highlighted.
// Theme open refreshes the user-theme directory listing (plan 023 C14);
// keyboard open, hover-slide into Theme, and close/reopen all pass through here.
func (b *BaseModel) openMenuFirst(id MenuID) {
	b.openMenu = int(id)
	if id == MenuTheme {
		b.themeMenuNames = AvailableThemes()
	} else {
		b.themeMenuNames = nil
	}
	b.menuHighlight = b.menuFirstSelectable(id)
	b.menuWindow = 0
	b.hoveredMenuCell = -1
}

func (b *BaseModel) menuFirstSelectable(id MenuID) int {
	items := b.menuItems(id)
	for i, it := range items {
		if menuItemSelectable(it) {
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
// Wrap lines / Follow checks; in Requests view, a Columns checkbox section
// (plan 023 B7). Filter menu (WS8): per-view check/radio rows. Theme menu
// (WS5): preset radios then user stems.
func (b *BaseModel) menuItems(id MenuID) []MenuItem {
	switch id {
	case MenuView:
		logsOn := b.viewMode == ViewModeLogs
		reqsOn := b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail
		panel := b.settings.ProcessPanel
		timestamps := b.settings.Timestamps
		wrap := b.settings.Wrap
		follow := b.followMode
		items := []MenuItem{
			{Label: "Logs", Hint: "Tab", Selected: &logsOn, Cmd: MenuCmdSetLogs},
			{Label: "Requests", Hint: "Tab", Selected: &reqsOn, Cmd: MenuCmdSetRequests},
			{Separator: true},
			{Label: "Process panel", Hint: "p", Checked: &panel, Cmd: MenuCmdToggleProcessPanel},
			{Label: "Timestamps", Hint: "T", Checked: &timestamps, Cmd: MenuCmdToggleTimestamps},
			{Label: "Wrap lines", Hint: "w", Checked: &wrap, Cmd: MenuCmdToggleWrap},
			{Label: "Follow", Hint: "F", Checked: &follow, Cmd: MenuCmdToggleFollow},
		}
		if reqsOn {
			cols := b.settings.RequestsColumns
			timeOn := cols.Time
			hostOn := cols.Host
			methodOn := cols.Method
			statusOn := cols.Status
			durationOn := cols.Duration
			idOn := cols.ID
			items = append(items,
				MenuItem{Separator: true},
				MenuItem{Label: "Columns", Cmd: ""}, // section header (non-activatable)
				MenuItem{Label: "Time", Checked: &timeOn, Cmd: MenuCmdToggleColTime},
				MenuItem{Label: "Host", Checked: &hostOn, Cmd: MenuCmdToggleColHost},
				MenuItem{Label: "Method", Checked: &methodOn, Cmd: MenuCmdToggleColMethod},
				MenuItem{Label: "Status", Checked: &statusOn, Cmd: MenuCmdToggleColStatus},
				MenuItem{Label: "Duration", Checked: &durationOn, Cmd: MenuCmdToggleColDuration},
				MenuItem{Label: "ID", Checked: &idOn, Cmd: MenuCmdToggleColID},
			)
		}
		return items
	case MenuTheme:
		names := b.themeMenuNames
		if names == nil {
			// Fallback for callers that build Theme items without opening
			// (tests); populate the cache so render never repeats ReadDir.
			names = AvailableThemes()
			b.themeMenuNames = names
		}
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

// menuItemSelectable reports whether a dropdown row can take highlight / activate
// (skips separators and section headers with empty Cmd).
func menuItemSelectable(it MenuItem) bool {
	return !it.Separator && it.Cmd != ""
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
		b.menuHighlight = b.menuStepDir(id, b.menuHighlight, false, true)
		b.followMenuWindow(prev, false)
	case "down", "j":
		prev := b.menuHighlight
		b.menuHighlight = b.menuStepDir(id, b.menuHighlight, true, true)
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
	case MenuCmdToggleColTime:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.Time = !c.Time })
	case MenuCmdToggleColHost:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.Host = !c.Host })
	case MenuCmdToggleColMethod:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.Method = !c.Method })
	case MenuCmdToggleColStatus:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.Status = !c.Status })
	case MenuCmdToggleColDuration:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.Duration = !c.Duration })
	case MenuCmdToggleColID:
		return b.toggleRequestsColumn(func(c *RequestsColumns) { c.ID = !c.ID })
	}
	if b.activateFilterMenuCommand(cmd) {
		return nil
	}
	return nil
}

// toggleRequestsColumn flips one RequestsColumns field, relayouts (row width
// changes), re-renders, and persists the whole [requests] table (plan 023 B7).
func (b *BaseModel) toggleRequestsColumn(flip func(*RequestsColumns)) tea.Cmd {
	flip(&b.settings.RequestsColumns)
	b.relayout()
	b.updateViewport()
	if err := SaveSettingsChanged(b.settings, settingRequestsColumns); err != nil {
		return b.setStatusFlash(footerError(formatSettingsSaveError(err)), flashSettingsSave, statusFlashClearDelay)
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
	if err := SaveSettingsChanged(b.settings, settingViewMenuBar); err != nil {
		return b.setStatusFlash(footerError(formatSettingsSaveError(err)), flashSettingsSave, statusFlashClearDelay)
	}
	return nil
}
