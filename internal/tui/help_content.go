package tui

import (
	"github.com/charmbracelet/x/ansi"
)

// helpKeyRow is one keybinding line in the help modal body.
type helpKeyRow struct {
	key  string
	desc string
}

// helpSection groups related bindings under a titled heading.
type helpSection struct {
	title string
	rows  []helpKeyRow
}

// helpBorderTitle is the label spliced into the top border (`─ Help — Logs ─`).
func (b *BaseModel) helpBorderTitle() string {
	switch b.viewMode {
	case ViewModeRequests:
		return "Help — Requests"
	case ViewModeRequestDetail:
		return "Help — Request Detail"
	default:
		return "Help — Logs"
	}
}

// helpTitleLine is the in-content title fallback when the modal has no border.
func (b *BaseModel) helpTitleLine() string {
	switch b.viewMode {
	case ViewModeRequests:
		return b.helpTitle("[Requests View]")
	case ViewModeRequestDetail:
		return b.helpTitle("[Request Detail]")
	default:
		return b.helpTitle("[Logs View]")
	}
}

func (b *BaseModel) helpSections() []helpSection {
	switch b.viewMode {
	case ViewModeRequests:
		return b.requestsHelpSections()
	case ViewModeRequestDetail:
		return b.detailHelpSections()
	default:
		return b.logsHelpSections()
	}
}

func (b *BaseModel) logsHelpSections() []helpSection {
	return []helpSection{
		{
			title: "Navigation",
			rows: []helpKeyRow{
				{key: "j/k, ↑/↓", desc: "Scroll (up pauses auto-follow)"},
				{key: "PgUp/PgDn", desc: "Half-page scroll"},
				{key: "g/Home", desc: "Jump to top (pauses follow)"},
				{key: "G/End", desc: "Jump to bottom (resumes follow)"},
				{key: "F", desc: "Toggle auto-follow"},
				{key: "Tab", desc: "Switch to Requests view"},
				{key: "scroll wheel", desc: "Scroll (3 lines per notch; up pauses follow)"},
			},
		},
		{
			title: "Filter & search",
			rows: []helpKeyRow{
				{key: "s", desc: "Filter bar (query language, live)"},
				{key: "", desc: "e.g. proc:api level:error -health"},
				{key: "f", desc: "Filter menu (process + level checks)"},
				{key: "/", desc: "Search (live) — cursor jumps as you type (does not hide lines)"},
				{key: "n/N", desc: "Next/previous search match"},
				{key: "Esc", desc: "Clear filters, search, and solo (while typing: cancel & restore)"},
			},
		},
		{
			title: "Processes",
			rows: []helpKeyRow{
				{key: "1-9", desc: "Solo a process (toggle); click panel chip too"},
			},
		},
		{
			title: "View & chrome",
			rows: []helpKeyRow{
				{key: "p", desc: "Toggle process panel"},
				{key: "T", desc: "Toggle timestamps in log lines"},
				{key: "w", desc: "Toggle soft-wrap"},
				{key: "m", desc: "Toggle menu bar"},
				{key: "v", desc: "Open View menu (bar visible)"},
				{key: "t", desc: "Cycle theme"},
				{key: "?", desc: "This help"},
			},
		},
		{
			title: "Copy (grab-for-agent)",
			rows: []helpKeyRow{
				{key: "y", desc: "Copy parked search line (when cursor set)"},
			},
		},
		{
			title: "Actions",
			rows: append([]helpKeyRow{
				{key: "r", desc: "Restart soloed process"},
			}, b.helpQuitRows()...),
		},
		{
			title: "Mouse",
			rows: []helpKeyRow{
				{key: "wheel", desc: "Scroll logs; scroll open dropdown (not viewport) when menu open"},
				{key: "click line", desc: "Park cursor on that entry (disengages follow)"},
				{key: "click chip", desc: "Solo/unsolo process"},
				{key: "menu bar", desc: "Click or hover cells; click dropdown rows; ←/→ switch open menus"},
				{key: "help open", desc: "Wheel scrolls when content exceeds the modal; click outside closes"},
			},
		},
	}
}

func (b *BaseModel) requestsHelpSections() []helpSection {
	return []helpSection{
		{
			title: "Navigation (cursor row ❯)",
			rows: []helpKeyRow{
				{key: "j/k, ↑/↓", desc: "Move cursor (up pauses follow; onto newest resumes)"},
				{key: "PgUp/PgDn", desc: "Move cursor half a page"},
				{key: "g/Home", desc: "Cursor to top (pauses follow)"},
				{key: "G/End", desc: "Cursor to bottom (resumes follow)"},
				{key: "F", desc: "Toggle auto-follow"},
				{key: "Tab", desc: "Switch to Logs view"},
				{key: "scroll wheel", desc: "Move cursor (3 rows/notch; follow rules match j/k)"},
			},
		},
		{
			title: "Filter & search",
			rows: []helpKeyRow{
				{key: "s", desc: "Filter bar (query language, live)"},
				{key: "", desc: "e.g. method:GET status:5xx host:api url:/orders"},
				{key: "f", desc: "Filter menu (status class, methods)"},
				{key: "/", desc: "Search visible columns (live, navigate — not filter)"},
				{key: "n/N", desc: "Next/previous search match"},
				{key: "Esc", desc: "Back from detail, clear filters/search, or cancel while typing"},
			},
		},
		{
			title: "Requests",
			rows: []helpKeyRow{
				{key: "Enter", desc: "Open detail for cursor row"},
				{key: "click row", desc: "Move cursor; double-click opens detail"},
			},
		},
		{
			title: "View & chrome",
			rows: []helpKeyRow{
				{key: "p", desc: "Toggle process panel"},
				{key: "T", desc: "Toggle timestamps in log lines"},
				{key: "w", desc: "Toggle soft-wrap"},
				{key: "m", desc: "Toggle menu bar"},
				{key: "v", desc: "Open View menu (Columns section in Requests)"},
				{key: "t", desc: "Cycle theme"},
				{key: "?", desc: "This help"},
			},
		},
		{
			title: "Copy (grab-for-agent)",
			rows: []helpKeyRow{
				{key: "y", desc: "Copy full request ID (cursor row)"},
				{key: "c", desc: "Copy as curl"},
				{key: "Y", desc: "Copy detail JSON (detail view)"},
			},
		},
		{
			title: "Actions",
			rows:  b.helpQuitRows(),
		},
		{
			title: "Mouse",
			rows: []helpKeyRow{
				{key: "wheel", desc: "Move cursor; scroll open dropdown (not viewport) when menu open"},
				{key: "click row", desc: "Move cursor; double-click opens detail"},
				{key: "click chip", desc: "Solo/unsolo process"},
				{key: "menu bar", desc: "Click or hover cells; click dropdown rows; ←/→ between menus"},
				{key: "help open", desc: "Wheel scrolls when content exceeds the modal; click outside closes"},
			},
		},
	}
}

func (b *BaseModel) detailHelpSections() []helpSection {
	return []helpSection{
		{
			title: "Navigation",
			rows: []helpKeyRow{
				{key: "j/k, ↑/↓", desc: "Scroll detail content"},
				{key: "PgUp/PgDn", desc: "Page scroll"},
				{key: "scroll wheel", desc: "Scroll (3 lines per notch)"},
				{key: "Esc", desc: "Back to requests list"},
			},
		},
		{
			title: "Copy (grab-for-agent)",
			rows: []helpKeyRow{
				{key: "y", desc: "Copy full request ID"},
				{key: "c", desc: "Copy as curl"},
				{key: "Y", desc: "Copy wire JSON (exact API payload)"},
			},
		},
		{
			title: "View & chrome",
			rows: append([]helpKeyRow{
				{key: "p", desc: "Toggle process panel"},
				{key: "T", desc: "Toggle timestamps in log lines"},
				{key: "w", desc: "Toggle soft-wrap"},
				{key: "m", desc: "Toggle menu bar"},
				{key: "t", desc: "Cycle theme"},
				{key: "?", desc: "This help"},
			}, b.helpQuitRows()...),
		},
		{
			title: "Mouse",
			rows: []helpKeyRow{
				{key: "wheel", desc: "Scroll detail; scroll open dropdown when menu open"},
				{key: "menu bar", desc: "Click or hover cells; click dropdown rows"},
				{key: "help open", desc: "Wheel scrolls when content exceeds the modal; click outside closes"},
			},
		},
	}
}

// helpKeyColumnWidth is the display width of the widest key column across sections.
func helpKeyColumnWidth(sections []helpSection) int {
	maxW := 0
	for _, sec := range sections {
		for _, row := range sec.rows {
			if w := ansi.StringWidth(row.key); w > maxW {
				maxW = w
			}
		}
	}
	return maxW
}

const helpKeyDescGap = "  "

func renderHelpKeyRow(row helpKeyRow, keyW int) string {
	return helpKeyPrefix(row, keyW) + styles.Base.Render(row.desc)
}

// helpKeyPrefix returns the styled key column + gap (or desc-column indent for
// empty-key example rows). Display width is always keyW + len(helpKeyDescGap).
func helpKeyPrefix(row helpKeyRow, keyW int) string {
	descStart := keyW + len(helpKeyDescGap)
	if row.key == "" {
		return fillPad(descStart)
	}
	keyPart := styles.HelpKey.Render(row.key)
	pad := keyW - ansi.StringWidth(row.key)
	if pad < 0 {
		pad = 0
	}
	return keyPart + fillPad(pad) + fillPad(len(helpKeyDescGap))
}

// wrapHelpKeyRow wraps a key+desc row with hanging indent on the description
// column (plan 024 F5). Continuations are fillPad(hangCols) + desc fragment.
func wrapHelpKeyRow(row helpKeyRow, keyW, width int) []string {
	prefix := helpKeyPrefix(row, keyW)
	hangCols := ansi.StringWidth(prefix)
	styledDesc := styles.Base.Render(row.desc)
	return hangIndentWrap(prefix, styledDesc, width-hangCols, width, hangCols)
}

// renderHelpBodyLines builds styled physical lines (pre-wrap) for the modal body.
func renderHelpBodyLines(sections []helpSection) []string {
	if len(sections) == 0 {
		return nil
	}
	keyW := helpKeyColumnWidth(sections)
	var out []string
	for i, sec := range sections {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, styles.HelpSection.Render(sec.title))
		for _, row := range sec.rows {
			out = append(out, renderHelpKeyRow(row, keyW))
		}
	}
	return out
}

// wrapHelpBody wraps section titles at full width and key rows with hanging
// description indent (plan 024 F5). Used by the help memo path.
func wrapHelpBody(sections []helpSection, width int) []string {
	if width < 1 {
		width = 1
	}
	if len(sections) == 0 {
		return nil
	}
	keyW := helpKeyColumnWidth(sections)
	var out []string
	for i, sec := range sections {
		if i > 0 {
			out = append(out, "")
		}
		title := styles.HelpSection.Render(sec.title)
		out = append(out, wrapHelpLines([]string{title}, width)...)
		for _, row := range sec.rows {
			out = append(out, wrapHelpKeyRow(row, keyW, width)...)
		}
	}
	return out
}
