package tui

import (
	"strconv"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestStateLabel_GatedDetailPinned pins the two gated-launch annotation
// strings (plan 013 D5's gatedDetail, now folded into stateLabel) byte-for-byte:
// " (waiting on: X, Y)" and " (blocked on: X)" with targets in declaration
// order. Nothing else in the package pins these strings now that gatedDetail
// itself is gone, so this test is their only guard (issue #92 bug 1 / plan
// 028 C6).
func TestStateLabel_GatedDetailPinned(t *testing.T) {
	tests := []struct {
		name string
		p    domain.ProcessInfo
		want string
	}{
		{"waiting on targets", domain.ProcessInfo{State: domain.ProcessStateWaiting, WaitingOn: []string{"postgres", "redis"}}, " (waiting on: postgres, redis)"},
		{"blocked on targets", domain.ProcessInfo{State: domain.ProcessStateBlocked, BlockedOn: []string{"restate-register"}}, " (blocked on: restate-register)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stateLabel(tc.p); got != tc.want {
				t.Errorf("stateLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStateLabel_EnumExhaustive iterates domain.AllProcessStates() so a 9th
// state added later fails this test until it is given a label (issue #92 bug
// 1 pin table). running is the one state that must render nothing; every
// other state must render a non-empty parenthesized word.
func TestStateLabel_EnumExhaustive(t *testing.T) {
	want := map[domain.ProcessState]string{
		domain.ProcessStateRunning:   "",
		domain.ProcessStateWaiting:   " (waiting)",
		domain.ProcessStateBlocked:   " (blocked)",
		domain.ProcessStateCrashed:   " (crashed)",
		domain.ProcessStateCompleted: " (done)",
		domain.ProcessStateStopped:   " (stopped)",
		domain.ProcessStateStarting:  " (starting)",
		domain.ProcessStateStopping:  " (stopping)",
	}
	states := domain.AllProcessStates()
	require.Len(t, states, len(want), "a state was added/removed in domain — update this pin table")
	for _, s := range states {
		t.Run(string(s), func(t *testing.T) {
			wantLabel, ok := want[s]
			require.True(t, ok, "state %s has no pinned label — add one to this test AND to stateLabel", s)
			got := stateLabel(domain.ProcessInfo{State: s})
			assert.Equal(t, wantLabel, got)
			if s == domain.ProcessStateRunning {
				assert.Empty(t, got, "running must pay nothing")
			} else {
				assert.NotEmpty(t, got, "every non-running state must carry a colour-independent label")
			}
		})
	}
}

// TestStateLabel_WaitingBlockedNoTargets pins the no-target forms of waiting
// and blocked: these differ from running gatedDetail (which rendered "" here)
// because a colour-independent reader still needs SOME word even without
// named targets (issue #92 bug 1).
func TestStateLabel_WaitingBlockedNoTargets(t *testing.T) {
	assert.Equal(t, " (waiting)", stateLabel(domain.ProcessInfo{State: domain.ProcessStateWaiting}))
	assert.Equal(t, " (blocked)", stateLabel(domain.ProcessInfo{State: domain.ProcessStateBlocked}))
}

// TestStateLabel_ColourIndependence is the actual point of issue #92 bug 1:
// with ANSI stripped (piped output, TERM=dumb, screenshots, colour-blind
// readers), a crashed process must still read differently from a running one.
// Before this change, styles.Crashed's bold-red was the ONLY distinguishing
// signal and healthDot renders nothing without a configured healthcheck, so
// the two rows were byte-identical once stripped.
func TestStateLabel_ColourIndependence(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	b := newTestBaseModel()
	b.viewMode = ViewModeLogs
	b.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
		{Name: "worker", State: domain.ProcessStateCrashed},
	}
	plain := ansi.Strip(b.processPanel())
	assert.Contains(t, plain, "1:web", "running carries no state label")
	assert.NotContains(t, plain, "web (")
	assert.Contains(t, plain, "2:worker (crashed)")
}

// TestStateLabel_FrameContractHolds re-runs the plan 023 T5 frame contract
// (every rendered row exactly frame width) with state labels present,
// including a width narrow enough that the panel's ansi.Cut truncates the
// label mid-string (issue #92 bug 1 / plan 028 C6). The panel already handles
// this for the gated-launch strings; this pins that the added label suffixes
// don't reopen it.
func TestStateLabel_FrameContractHolds(t *testing.T) {
	for _, w := range []int{80, 40, 20} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			m := newTestModel()
			m.processes = []domain.ProcessInfo{
				{Name: "web", State: domain.ProcessStateRunning},
				{Name: "worker", State: domain.ProcessStateCrashed},
				{Name: "db", State: domain.ProcessStateBlocked, BlockedOn: []string{"postgres", "redis"}},
			}
			m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: 24})
			assertFrameContract(t, m)
		})
	}
}

// TestHealthDotStyles_ReuseProcessColors pins that the health-dot styles
// (plan 018 D13) reuse OK/Err rather than introducing a new palette entry.
func TestHealthDotStyles_ReuseProcessColors(t *testing.T) {
	th := CurrentTheme()
	assert.Equal(t, string(th.OK), colorStr(styles.HealthyDot.GetForeground()))
	assert.Equal(t, string(th.Err), colorStr(styles.UnhealthyDot.GetForeground()))
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
		{"healthy", domain.HealthStatusHealthy, styles.HealthyDot.Render(" ●")},
		// Distinct glyph, not just a distinct color: monochrome/NO_COLOR and
		// red-green color-blind readers must still tell the states apart.
		{"unhealthy", domain.HealthStatusUnhealthy, styles.UnhealthyDot.Render(" ✗")},
		{"unknown", domain.HealthStatusUnknown, ""},
		// No healthcheck configured (#100): the panel adds nothing, exactly as it
		// did when this case reported "unknown" — the TUI needed no change.
		{"none", domain.HealthStatusNone, ""},
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
