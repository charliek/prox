package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"

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
//
// Uses the legacy preset so the ANSI-256 values match the pre-theme palette.
// Theme-mutating: must not call t.Parallel (package-global style set).
func TestProcessStyle_GatedStates(t *testing.T) {
	withTestTheme(t, "legacy")
	th := CurrentTheme()
	tests := []struct {
		state domain.ProcessState
		want  lipgloss.Color
	}{
		{domain.ProcessStateRunning, th.OK},
		{domain.ProcessStateStopped, th.Dim},
		{domain.ProcessStateCrashed, th.Err},
		{domain.ProcessStateStarting, th.Warn},
		{domain.ProcessStateStopping, th.Warn},
		{domain.ProcessStateWaiting, th.Waiting},
		{domain.ProcessStateBlocked, th.Err},
		{domain.ProcessStateCompleted, th.Dim},
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

// TestHealthDotStyles_ReuseProcessColors pins that the health-dot styles
// (plan 018 D13) reuse OK/Err rather than introducing a new palette entry.
func TestHealthDotStyles_ReuseProcessColors(t *testing.T) {
	th := CurrentTheme()
	assert.Equal(t, string(th.OK), colorStr(s.HealthyDot.GetForeground()))
	assert.Equal(t, string(th.Err), colorStr(s.UnhealthyDot.GetForeground()))
}

// TestHealthDot pins the process panel's health indicator (plan 018 D13):
// healthy renders a styled " ●", unhealthy a styled " ✗" (distinct glyphs for monochrome/color-blind readability), while unknown and unset
// (the zero value) render nothing.
func TestHealthDot(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prev)

	tests := []struct {
		name   string
		status domain.HealthStatus
		want   string
	}{
		{"healthy", domain.HealthStatusHealthy, s.HealthyDot.Render(" ●")},
		// Distinct glyph, not just a distinct color: monochrome/NO_COLOR and
		// red-green color-blind readers must still tell the states apart.
		{"unhealthy", domain.HealthStatusUnhealthy, s.UnhealthyDot.Render(" ✗")},
		{"unknown", domain.HealthStatusUnknown, ""},
		{"unset", domain.HealthStatus(""), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthDot(tc.status); got != tc.want {
				t.Errorf("healthDot(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}

	// The healthy and unhealthy dots must actually carry distinct ANSI color
	// codes, not just distinct text, to prove they're separately styled.
	healthy := healthDot(domain.HealthStatusHealthy)
	unhealthy := healthDot(domain.HealthStatusUnhealthy)
	assert.Contains(t, healthy, "\x1b[")
	assert.Contains(t, unhealthy, "\x1b[")
	assert.NotEqual(t, healthy, unhealthy)
}
