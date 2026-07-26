package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/prox/internal/domain"
)

// colorStr renders a style's foreground as its palette string ("10", "214", ...)
// for comparison. Non-lipgloss.Color foregrounds render empty.
func colorStr(c lipgloss.TerminalColor) string {
	if col, ok := c.(lipgloss.Color); ok {
		return string(col)
	}
	return ""
}

// TestProcessStyle_GatedStates pins the plan 013 D5 state-to-style mapping:
// waiting is a distinct amber, blocked shares the crashed red, completed rests
// gray like stopped, and every prior mapping is unchanged.
func TestProcessStyle_GatedStates(t *testing.T) {
	tests := []struct {
		state domain.ProcessState
		want  lipgloss.Color
	}{
		{domain.ProcessStateRunning, runningColor},
		{domain.ProcessStateStopped, stoppedColor},
		{domain.ProcessStateCrashed, crashedColor},
		{domain.ProcessStateStarting, startingColor},
		{domain.ProcessStateStopping, stoppingColor},
		{domain.ProcessStateWaiting, waitingColor},
		{domain.ProcessStateBlocked, blockedColor},
		{domain.ProcessStateCompleted, completedColor},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			got := colorStr(processStyle(tc.state).GetForeground())
			if got != string(tc.want) {
				t.Errorf("processStyle(%s) foreground = %q, want %q", tc.state, got, tc.want)
			}
		})
	}

	// waiting must be visually distinct from starting (a bright yellow), the whole
	// point of picking a separate amber.
	if colorStr(processStyle(domain.ProcessStateWaiting).GetForeground()) ==
		colorStr(processStyle(domain.ProcessStateStarting).GetForeground()) {
		t.Error("waiting and starting must use distinct colors")
	}
}

// TestGatedDetail pins the inline gated-launch annotation used by the process
// panel (plan 013 D5).
func TestGatedDetail(t *testing.T) {
	tests := []struct {
		name string
		p    domain.ProcessInfo
		want string
	}{
		{"running none", domain.ProcessInfo{State: domain.ProcessStateRunning}, ""},
		{"waiting", domain.ProcessInfo{State: domain.ProcessStateWaiting, WaitingOn: []string{"postgres", "redis"}}, " (waiting on: postgres, redis)"},
		{"blocked", domain.ProcessInfo{State: domain.ProcessStateBlocked, BlockedOn: []string{"restate-register"}}, " (blocked on: restate-register)"},
		{"waiting no targets", domain.ProcessInfo{State: domain.ProcessStateWaiting}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatedDetail(tc.p); got != tc.want {
				t.Errorf("gatedDetail = %q, want %q", got, tc.want)
			}
		})
	}
}
