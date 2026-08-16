package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/prox/internal/domain"
)

// The dead-stack banner (plan 028 C4, #96).
//
// A foreground `prox up` whose every process has died used to sit there
// supervising nothing, silent, until the user thought to press Ctrl-C. C3 fixed
// that for plain/piped mode by ending the session non-zero. THE TUI IS
// DELIBERATELY DIFFERENT: it does NOT auto-exit. The user is present and
// reading, and pulling the screen out from under them would take the crash
// output -- the very thing they need to see -- with it. So instead of exiting,
// the TUI states the situation in a persistent, unmissable row and says how to
// leave.
//
// Because it is computed from the process snapshots the TUI already receives
// (ConsumeProcesses -> ProcessesMsg -> BaseModel.processes), nothing new is
// plumbed for it, and it works in `prox attach` too.

// deadStackProcs reports whether procs describe a session with nothing left to
// supervise and a failure to answer for.
//
// This is the same rule the CLI's dead-stack watcher applies -- see deadStack in
// internal/cli/dead_stack.go, which carries the full rationale for both gates.
// The two are siblings, not a copy: internal/tui must not import internal/cli,
// so each spells the predicate out locally, and BOTH derive it from the same
// domain predicates (ProcessState.IsLive / IsTerminalFailure, domain/process.go).
// That shared derivation is what keeps them honest -- widen the domain
// predicates and both move together; widen one of these functions alone and the
// banner and the exit code start disagreeing about what "dead" means.
//
// IsStopped is NOT usable here: it also covers `completed`, a task's terminal
// SUCCESS, so an all-tasks-finished config would be branded a crash.
func deadStackProcs(procs []domain.ProcessInfo) bool {
	if len(procs) == 0 {
		// Nothing configured (or nothing known yet) is not a dead stack.
		return false
	}
	failed := false
	for _, p := range procs {
		if p.State.IsLive() {
			return false
		}
		if p.State.IsTerminalFailure() {
			failed = true
		}
	}
	return failed
}

// deadStackBannerText is the banner's sentence, or "" when the stack is not
// dead.
//
// The counts name BOTH halves of IsTerminalFailure, not just crashes: a stack
// that never launched because a dependency failed is every bit as dead as one
// that crashed, and "crashed" would be a lie about it.
//
// The KEY is `q` in both modes and is not derived from HelpConfig:
// defaultFooterHints (footer.go) fixes the key and varies only its LABEL
// (HelpConfig.QuitHint -- "stop" for `prox up`, "quit" for `prox attach`), so
// naming the key here cannot drift from the footer. The VERB is deliberately
// "quit" in both modes rather than the footer's label: owner mode's "stop"
// means "quit, taking the processes down with you", and at the moment this
// banner appears there is nothing left to take down -- "press q to stop" would
// name an action that has already happened. Quitting is what q does here.
func deadStackBannerText(procs []domain.ProcessInfo) string {
	if !deadStackProcs(procs) {
		return ""
	}
	var crashed, blocked int
	for _, p := range procs {
		switch p.State {
		case domain.ProcessStateCrashed:
			crashed++
		case domain.ProcessStateBlocked:
			blocked++
		}
	}
	var parts []string
	if crashed > 0 {
		parts = append(parts, fmt.Sprintf("%d crashed", crashed))
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	// deadStackProcs guarantees at least one terminal failure, so parts is
	// never empty and the sentence never degenerates to "stopped — .".
	return fmt.Sprintf("All processes have stopped — %s. Nothing is running. Press q to quit.",
		strings.Join(parts, ", "))
}

// showDeadStackBanner reports whether the banner occupies a chrome row.
//
// It must NOT call chromeAbove: chromeAbove counts this row, so the room check
// below reads the fixed chrome directly. The check mirrors requestsHeaderRows —
// a chrome row never takes the last content row, because a frame with zero
// viewport rows overflows its own height.
func (b *BaseModel) showDeadStackBanner() bool {
	if deadStackBannerText(b.processes) == "" {
		return false
	}
	if b.width <= 0 {
		return false // nothing renderable; geometry is stale anyway (relayout no-ops)
	}
	if b.height > 0 && b.height-(b.fixedChromeAbove()+b.chromeBelow()) < 2 {
		return false
	}
	return true
}

// deadStackBannerRows is the banner's row cost (0 or 1), for chromeAbove.
func (b *BaseModel) deadStackBannerRows() int {
	if b.showDeadStackBanner() {
		return 1
	}
	return 0
}

// renderDeadStackBanner builds the full-width banner row.
//
// styles.Err is the theme's error palette (ErrBadgeFG on ErrBadgeBG, bold) and
// it paints the trailing fill too, so the row is a solid band under every theme
// — no default-background hole (plan 024's frame-fill law), FullFill or legacy.
// The colour is emphasis ONLY: strip every escape and the sentence still says
// what happened and how to leave, which is the whole point of the feature.
func (b *BaseModel) renderDeadStackBanner(width int) string {
	text := deadStackBannerText(b.processes)
	if text == "" || width <= 0 {
		return ""
	}
	// Leading gutter matches the footer/header left inset.
	row := styles.Err.Render(" " + text)
	w := ansi.StringWidth(row)
	switch {
	case w > width:
		return ansi.Cut(row, 0, width)
	case w < width:
		return row + styles.Err.Render(strings.Repeat(" ", width-w))
	default:
		return row
	}
}
