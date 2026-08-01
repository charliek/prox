package tui

import (
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
)

// footerKind distinguishes info notices from errors for footer styling
// (plan 023 B2 — typed message model, no string severity inference).
type footerKind int

const (
	footerKindInfo footerKind = iota
	footerKindError
)

// footerMsg is the typed footer status payload.
type footerMsg struct {
	kind footerKind
	text string
}

func footerInfo(text string) footerMsg {
	return footerMsg{kind: footerKindInfo, text: text}
}

func footerError(text string) footerMsg {
	return footerMsg{kind: footerKindError, text: text}
}

func (m footerMsg) empty() bool { return m.text == "" }

// flashClass ranks stored flashes for precedence (settings save > transient).
type flashClass int

const (
	flashTransient flashClass = iota
	flashSettingsSave
)

// footerHint is one key+label pair. sticky pairs (? help, q quit) drop last.
type footerHint struct {
	key, label string
	sticky     bool
}

// defaultFooterHints is the merged-row key hint strip (plan 023 B2).
func defaultFooterHints() []footerHint {
	return []footerHint{
		{key: "m", label: " menu", sticky: false},
		{key: "/", label: " search", sticky: false},
		{key: "s", label: " filter", sticky: false},
		{key: "?", label: " help", sticky: true},
		{key: "q", label: " quit", sticky: true},
	}
}

// setStatusFlash shows a typed footer flash and returns the clear command
// tagged with the NEW flash generation (StatusFlashClearMsg.Seq).
func (b *BaseModel) setStatusFlash(msg footerMsg, class flashClass, delay time.Duration) tea.Cmd {
	b.statusFlash = msg
	b.statusFlashClass = class
	b.statusFlashSeq++
	seq := b.statusFlashSeq
	return tea.Tick(delay, func(t time.Time) tea.Msg {
		return StatusFlashClearMsg{Seq: seq}
	})
}

// styleFooterMsg renders msg for the footer left band. Errors get `✗ ` +
// Err-bold on FooterBG (s.FooterError — readable on light themes where the
// badge FG blends into FooterBG); info is plain FooterFG on FooterBG.
func styleFooterMsg(msg footerMsg) string {
	if msg.empty() {
		return ""
	}
	if msg.kind == footerKindError {
		return s.FooterError.Render("✗ " + msg.text)
	}
	return s.FooterLabel.Render(msg.text)
}

// renderFooterHints styles key+label pairs (FooterKey+bold / FooterFG) with
// FooterBG throughout. Separators sit between pairs.
func renderFooterHints(hints []footerHint) string {
	if len(hints) == 0 {
		return ""
	}
	var b strings.Builder
	for i, h := range hints {
		if i > 0 {
			b.WriteString(s.FooterLabel.Render(" · "))
		}
		b.WriteString(s.FooterKey.Render(h.key))
		b.WriteString(s.FooterLabel.Render(h.label))
	}
	return b.String()
}

// padFooterRow pads or truncates row to exactly width columns using FooterBG
// spaces (whole footer band — no unstyled holes, plan 023 B2).
func padFooterRow(row string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(row)
	switch {
	case w == width:
		return row
	case w > width:
		return ansi.Cut(row, 0, width)
	default:
		return row + s.FooterLabel.Render(strings.Repeat(" ", width-w))
	}
}

// dropFooterHint removes one hint per the B2 policy: right-to-left among
// non-sticky pairs first, then sticky (? help, q quit) last.
func dropFooterHint(hints []footerHint) []footerHint {
	if len(hints) == 0 {
		return hints
	}
	for i := len(hints) - 1; i >= 0; i-- {
		if !hints[i].sticky {
			return append(append([]footerHint{}, hints[:i]...), hints[i+1:]...)
		}
	}
	return hints[:len(hints)-1]
}

// fitFooterRow applies the narrow-width degradation ladder (plan 023 B2):
// drop hints RTL (whole pairs) → drop count → ANSI-truncate status.
// Returns styled left and right segments (no outer Status padding yet).
func fitFooterRow(width int, leftStyled, countStyled string, hints []footerHint) (left, right string) {
	left = leftStyled
	hints = append([]footerHint(nil), hints...)
	showCount := countStyled != ""

	const gap = 2 // "  " between left and right
	// Leading FooterLabel pad (1); trailing filled by padFooterRow.
	const statusPad = 1

	innerW := width - statusPad
	if innerW < 0 {
		innerW = 0
	}

	buildRight := func() string {
		parts := make([]string, 0, 2)
		if showCount && countStyled != "" {
			parts = append(parts, countStyled)
		}
		hs := renderFooterHints(hints)
		if hs != "" {
			parts = append(parts, hs)
		}
		return strings.Join(parts, " ")
	}

	for {
		right = buildRight()
		need := ansi.StringWidth(left) + gap + ansi.StringWidth(right)
		if need <= innerW {
			break
		}
		if len(hints) > 0 {
			hints = dropFooterHint(hints)
			continue
		}
		if showCount {
			showCount = false
			continue
		}
		// Truncate left to remaining budget.
		budget := innerW - gap - ansi.StringWidth(right)
		if budget < 0 {
			budget = 0
		}
		left = ansi.Truncate(left, budget, "…")
		right = buildRight()
		break
	}
	return left, right
}

// resolveFooterMsg picks the visible footer message by B2 precedence
// (highest first): connection/stream errors → settings save failures →
// restart results → transient flashes → filter status → idle status.
func (m ClientModel) resolveFooterMsg() footerMsg {
	if msg, ok := m.connectionFooterMsg(); ok {
		return msg
	}
	if !m.statusFlash.empty() && m.statusFlashClass == flashSettingsSave {
		return m.statusFlash
	}
	if msg, ok := m.restartFooterMsg(); ok {
		return msg
	}
	if !m.statusFlash.empty() {
		return m.statusFlash
	}
	return m.filterOrIdleFooterMsg()
}

func (m ClientModel) connectionFooterMsg() (footerMsg, bool) {
	if m.connectionError == nil {
		return footerMsg{}, false
	}
	if errors.Is(m.connectionError, errProcessesStreamUnsupported) {
		return footerError(truncateError(m.connectionError, maxErrorDisplayLen)), true
	}
	if m.streamHealth[StreamProcesses].State == stream.StateClosed {
		return footerError("Connection lost: " + truncateError(m.connectionError, maxErrorDisplayLen)), true
	}
	return footerError("Connection error (retrying...)"), true
}

func (m ClientModel) restartFooterMsg() (footerMsg, bool) {
	if m.lastRestartProcess == "" {
		return footerMsg{}, false
	}
	if m.lastRestartError != nil {
		return footerError("Restart failed: " + truncateError(m.lastRestartError, maxErrorDisplayLen)), true
	}
	return footerInfo("Restarted: " + m.lastRestartProcess), true
}

// filterOrIdleFooterMsg returns search/filter/solo status, else the idle hint
// (optionally enriched with ConnectedStatus).
func (m ClientModel) filterOrIdleFooterMsg() footerMsg {
	var requests []proxy.RequestRecord
	var entries []domain.LogEntry
	if m.viewMode == ViewModeRequests {
		requests = m.filteredProxyRequests()
	} else {
		entries = m.filteredEntries()
	}
	// ConnectedStatus rides the idle path only — statusLeftDefault ignores
	// extraInfo when search/filter/solo wins.
	return footerInfo(m.statusLeftDefault(m.opts.ConnectedStatus, requests, entries))
}
