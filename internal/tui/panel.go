package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Viewport panel (plan 023 E2 / C7): rounded titled border around the content
// viewport. Costs panelBorder() rows and cols when it fits; degenerate frames
// render borderless rather than corrupting.

const (
	// panelMinWidth is corners + at least one mid cell on top/bottom.
	panelMinWidth = 4
	// panelMinHeight is top border + ≥1 content row + bottom border.
	panelMinHeight = 3
	// panelTitlePrefixW is the "─ " before the title in the top mid segment.
	panelTitlePrefixW = 2
)

// panelBorder returns the row/col cost of the viewport panel (0 or 2).
// The WS-C width chain subtracts this term via viewport.Width (relayout).
func (b *BaseModel) panelBorder() int {
	if b.canDrawPanel() {
		return 2
	}
	return 0
}

// canDrawPanel reports whether contentRect is large enough for a non-corrupting
// rounded border (w≥4, h≥3).
func (b *BaseModel) canDrawPanel() bool {
	if b.width < panelMinWidth {
		return false
	}
	h := b.height - b.chromeHeight()
	return h >= panelMinHeight
}

// panelTitle is the label spliced into the top border (`─ Logs ─` form).
// Detail keeps the in-content "Request: %s" header — the border title is
// "Request <id>" (or "Request" when no id yet).
func (b *BaseModel) panelTitle() string {
	switch b.viewMode {
	case ViewModeRequests:
		return "Requests"
	case ViewModeRequestDetail:
		id := b.selectedRequestID
		if b.requestDetail != nil && b.requestDetail.ID != "" {
			id = b.requestDetail.ID
		}
		if id == "" {
			return "Request"
		}
		return "Request " + id
	default:
		return "Logs"
	}
}

// renderPanelTop builds the top border row with an ANSI-aware truncated title.
// Layout: ╭─ <title> ────╮. Callers guarantee width ≥ panelMinWidth (4).
func (b *BaseModel) renderPanelTop(width int) string {
	br := lipgloss.RoundedBorder()
	return s.Panel.Render(br.TopLeft) + renderPanelTitleMid(b.panelTitle(), width-2) +
		s.Panel.Render(br.TopRight)
}

// renderPanelTitleMid builds the top-border mid segment of display width inner.
// Title truncates with "…" when needed; omitted entirely when inner < 4
// (no room for "─ " + ≥1 title column).
func renderPanelTitleMid(title string, inner int) string {
	br := lipgloss.RoundedBorder()
	if inner <= 0 {
		return ""
	}
	if title == "" || inner < panelTitlePrefixW+1 {
		return s.Panel.Render(strings.Repeat(br.Top, inner))
	}
	budget := inner - panelTitlePrefixW
	trunc := ansi.Truncate(title, budget, "…")
	tw := ansi.StringWidth(trunc)
	if tw == 0 {
		return s.Panel.Render(strings.Repeat(br.Top, inner))
	}
	fill := inner - panelTitlePrefixW - tw // ≥ 0: tw ≤ budget by ansi.Truncate
	return s.Panel.Render(br.Top+" ") + s.PanelTitle.Render(trunc) +
		s.Panel.Render(strings.Repeat(br.Top, fill))
}

// renderPanelBottom builds the bottom border row: ╰────╯. Callers guarantee
// width ≥ panelMinWidth (4).
func (b *BaseModel) renderPanelBottom(width int) string {
	br := lipgloss.RoundedBorder()
	return s.Panel.Render(br.BottomLeft) +
		s.Panel.Render(strings.Repeat(br.Bottom, width-2)) +
		s.Panel.Render(br.BottomRight)
}

// wrapPanelContentRow wraps one viewport content line with left/right borders.
// line must already be display-width == viewport.Width.
func wrapPanelContentRow(line string) string {
	br := lipgloss.RoundedBorder()
	return s.Panel.Render(br.Left) + line + s.Panel.Render(br.Right)
}
