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
	// Shared by every titled border (viewport panel, help modal — plan 023 B5).
	borderTitlePrefixW = 2
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
	return styles.Panel.Render(br.TopLeft) + renderPanelTitleMid(b.panelTitle(), width-2) +
		styles.Panel.Render(br.TopRight)
}

// renderBorderTitleMid builds the top-border mid segment of display width
// inner with the label spliced in: `─ label ───`. Label truncates with "…"
// when needed; omitted entirely when inner < borderTitlePrefixW+1 (no room for
// "─ " + ≥1 label column). Shared by the viewport panel (C7) and the help
// modal border title (plan 023 B5).
func renderBorderTitleMid(label string, inner int, borderStyle, labelStyle lipgloss.Style) string {
	br := lipgloss.RoundedBorder()
	if inner <= 0 {
		return ""
	}
	if label == "" || inner < borderTitlePrefixW+1 {
		return borderStyle.Render(strings.Repeat(br.Top, inner))
	}
	budget := inner - borderTitlePrefixW
	trunc := ansi.Truncate(label, budget, "…")
	tw := ansi.StringWidth(trunc)
	if tw == 0 {
		return borderStyle.Render(strings.Repeat(br.Top, inner))
	}
	fill := inner - borderTitlePrefixW - tw // ≥ 0: tw ≤ budget by ansi.Truncate
	return borderStyle.Render(br.Top+" ") + labelStyle.Render(trunc) +
		borderStyle.Render(strings.Repeat(br.Top, fill))
}

// renderPanelTitleMid builds the top-border mid segment of display width inner.
func renderPanelTitleMid(title string, inner int) string {
	return renderBorderTitleMid(title, inner, styles.Panel, styles.PanelTitle)
}

// renderPanelBottom builds the bottom border row: ╰────╯. Callers guarantee
// width ≥ panelMinWidth (4).
func (b *BaseModel) renderPanelBottom(width int) string {
	br := lipgloss.RoundedBorder()
	return styles.Panel.Render(br.BottomLeft) +
		styles.Panel.Render(strings.Repeat(br.Bottom, width-2)) +
		styles.Panel.Render(br.BottomRight)
}

// wrapPanelContentRow wraps one viewport content line with left/right borders.
// line must already be display-width == viewport.Width.
func wrapPanelContentRow(line string) string {
	br := lipgloss.RoundedBorder()
	return styles.Panel.Render(br.Left) + line + styles.Panel.Render(br.Right)
}
