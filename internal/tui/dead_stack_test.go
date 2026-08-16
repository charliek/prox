package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// Dead-stack banner tests (plan 028 C4, #96). Every assertion here reads the
// REAL rendered frame with styling stripped: the banner's contract is that its
// text carries the meaning, so a test that inspected the model's fields instead
// would pass on a banner nobody can read.

// deadStackProcsOf builds a snapshot with one process per state (p0, p1, ...).
func deadStackProcsOf(states ...domain.ProcessState) []domain.ProcessInfo {
	out := make([]domain.ProcessInfo, 0, len(states))
	for i, s := range states {
		out = append(out, domain.ProcessInfo{Name: fmt.Sprintf("p%d", i), State: s})
	}
	return out
}

// deadStackModel drives the REAL message path: a sized model plus one
// ProcessesMsg, exactly as the processes stream delivers it.
func deadStackModel(t *testing.T, w, h int, states ...domain.ProcessState) ClientModel {
	t.Helper()
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = clientUpdate(m, ProcessesMsg(deadStackProcsOf(states...)))
	return m
}

const deadStackBannerLead = "All processes have stopped"

// deadStackBannerRow returns the frame row (styling intact) holding the banner,
// its index, and whether it was found.
func deadStackBannerRow(m ClientModel) (string, int, bool) {
	for i, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(ansi.Strip(line), deadStackBannerLead) {
			return line, i, true
		}
	}
	return "", -1, false
}

func TestDeadStackBanner_AbsentWhenStackIsNotDead(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	cases := []struct {
		name   string
		states []domain.ProcessState
	}{
		{"no processes", nil},
		{"all running", []domain.ProcessState{domain.ProcessStateRunning, domain.ProcessStateRunning}},
		{"partial crash", []domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateRunning}},
		{"all completed", []domain.ProcessState{domain.ProcessStateCompleted, domain.ProcessStateCompleted}},
		{"all stopped", []domain.ProcessState{domain.ProcessStateStopped, domain.ProcessStateStopped}},
		// Live-but-not-running states are still live: nothing has settled yet.
		{"crashed plus starting", []domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateStarting}},
		{"crashed plus waiting", []domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateWaiting}},
		{"crashed plus stopping", []domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateStopping}},
		{"completed and stopped", []domain.ProcessState{domain.ProcessStateCompleted, domain.ProcessStateStopped}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := deadStackModel(t, 100, 24, tc.states...)
			assert.False(t, m.showDeadStackBanner())
			_, _, found := deadStackBannerRow(m)
			assert.False(t, found, "banner must not render for %s", tc.name)
			assertFrameContract(t, m)
		})
	}
}

func TestDeadStackBanner_PresentAndWorded(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	cases := []struct {
		name   string
		states []domain.ProcessState
		want   string
	}{
		{
			"one crashed",
			[]domain.ProcessState{domain.ProcessStateCrashed},
			"All processes have stopped — 1 crashed. Nothing is running. Press q to quit.",
		},
		{
			"all crashed",
			[]domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateCrashed},
			"All processes have stopped — 2 crashed. Nothing is running. Press q to quit.",
		},
		{
			"one blocked",
			[]domain.ProcessState{domain.ProcessStateBlocked},
			"All processes have stopped — 1 blocked. Nothing is running. Press q to quit.",
		},
		{
			"all blocked",
			[]domain.ProcessState{domain.ProcessStateBlocked, domain.ProcessStateBlocked},
			"All processes have stopped — 2 blocked. Nothing is running. Press q to quit.",
		},
		{
			"mixed crashed and blocked",
			[]domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateBlocked},
			"All processes have stopped — 1 crashed, 1 blocked. Nothing is running. Press q to quit.",
		},
		{
			"plural of both",
			[]domain.ProcessState{
				domain.ProcessStateCrashed, domain.ProcessStateCrashed,
				domain.ProcessStateBlocked, domain.ProcessStateBlocked,
			},
			"All processes have stopped — 2 crashed, 2 blocked. Nothing is running. Press q to quit.",
		},
		{
			// Nothing live and one failure: a finished task does not excuse the
			// crash, and completed is NOT counted as a failure.
			"crashed plus completed",
			[]domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateCompleted},
			"All processes have stopped — 1 crashed. Nothing is running. Press q to quit.",
		},
		{
			"crashed plus stopped",
			[]domain.ProcessState{domain.ProcessStateCrashed, domain.ProcessStateStopped},
			"All processes have stopped — 1 crashed. Nothing is running. Press q to quit.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := deadStackModel(t, 100, 24, tc.states...)
			require.True(t, m.showDeadStackBanner())
			row, _, found := deadStackBannerRow(m)
			require.True(t, found, "banner row missing")
			assert.Contains(t, ansi.Strip(row), tc.want)
			assertFrameContract(t, m)
		})
	}
}

// TestDeadStackBanner_LegibleWithColourStripped is the point of the feature:
// the row is styled with the error palette, but strip every escape byte and the
// whole sentence — what happened AND how to leave — is still there.
func TestDeadStackBanner_LegibleWithColourStripped(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := deadStackModel(t, 100, 24, domain.ProcessStateCrashed, domain.ProcessStateBlocked)
	row, _, found := deadStackBannerRow(m)
	require.True(t, found)

	require.NotEqual(t, row, ansi.Strip(row), "banner is expected to carry styling at all")
	plain := ansi.Strip(row)
	assert.Contains(t, plain, "All processes have stopped")
	assert.Contains(t, plain, "1 crashed, 1 blocked")
	assert.Contains(t, plain, "Nothing is running")
	assert.Contains(t, plain, "Press q to quit")

	// The same holds under a second theme: no theme may encode the meaning in
	// colour alone.
	withTestTheme(t, "legacy")
	row2, _, found2 := deadStackBannerRow(m)
	require.True(t, found2)
	assert.Equal(t, plain, ansi.Strip(row2))
}

// TestDeadStackBanner_RelayoutBothDirections is the load-bearing test: the
// viewport must shrink by exactly one row when the banner appears and grow back
// when it goes, driven only by ProcessesMsg. Deleting the relayout() call in
// the ProcessesMsg handler must fail this.
func TestDeadStackBanner_RelayoutBothDirections(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := deadStackModel(t, 100, 24, domain.ProcessStateRunning, domain.ProcessStateRunning)
	baseH := m.viewport.Height
	baseChrome := m.chromeAbove()
	_, baseOY := m.viewportOrigin()
	require.False(t, m.showDeadStackBanner())
	assertFrameContract(t, m)

	// false -> true
	m = clientUpdate(m, ProcessesMsg(deadStackProcsOf(
		domain.ProcessStateCrashed, domain.ProcessStateCrashed)))
	require.True(t, m.showDeadStackBanner())
	assert.Equal(t, baseChrome+1, m.chromeAbove(), "banner is counted in chromeAbove")
	assert.Equal(t, baseH-1, m.viewport.Height, "banner costs exactly one viewport row")
	_, oy := m.viewportOrigin()
	assert.Equal(t, baseOY+1, oy, "viewport origin shifts down by the banner row")
	assertFrameContract(t, m)

	// true -> false (a `prox restart` revives the stack)
	m = clientUpdate(m, ProcessesMsg(deadStackProcsOf(
		domain.ProcessStateRunning, domain.ProcessStateRunning)))
	require.False(t, m.showDeadStackBanner())
	assert.Equal(t, baseChrome, m.chromeAbove())
	assert.Equal(t, baseH, m.viewport.Height, "the row is given back")
	_, oy = m.viewportOrigin()
	assert.Equal(t, baseOY, oy)
	assertFrameContract(t, m)
}

// TestDeadStackBanner_FrameContract sweeps sizes and views with the banner up:
// every row of the frame is exactly the frame width (no default-background
// hole, no overflow), including narrow frames where the sentence truncates.
func TestDeadStackBanner_FrameContract(t *testing.T) {
	pinANSIProfile(t)

	for _, theme := range []string{"tokyo-night", "legacy"} {
		for _, sz := range []struct{ w, h int }{
			{120, 40}, {100, 24}, {40, 12}, {20, 8}, {10, 6}, {6, 6}, {2, 6}, {1, 6},
		} {
			name := fmt.Sprintf("%s_w%dh%d", theme, sz.w, sz.h)
			t.Run(name, func(t *testing.T) {
				withTestTheme(t, theme)
				m := deadStackModel(t, sz.w, sz.h,
					domain.ProcessStateCrashed, domain.ProcessStateBlocked)
				assertFrameContract(t, m)

				m.setViewMode(ViewModeRequests)
				assertFrameContract(t, m)

				if row, _, found := deadStackBannerRow(m); found {
					assert.Equal(t, sz.w, ansi.StringWidth(row))
				}
			})
		}
	}
}

// TestDeadStackBanner_RowPosition pins where the row sits: after the process
// panel, immediately above the panel's top border.
func TestDeadStackBanner_RowPosition(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := deadStackModel(t, 100, 24, domain.ProcessStateCrashed)
	lines := strings.Split(m.View(), "\n")
	_, idx, found := deadStackBannerRow(m)
	require.True(t, found)

	panelRowY, ok := m.processPanelRowY()
	require.True(t, ok)
	assert.Greater(t, idx, panelRowY, "banner sits below the process panel")
	assert.Equal(t, m.chromeAbove()-1, idx, "banner is the last chrome row above the panel")
	require.Less(t, idx+1, len(lines))
	assert.True(t, strings.HasPrefix(ansi.Strip(lines[idx+1]), "╭"),
		"the panel top border follows the banner, got %q", ansi.Strip(lines[idx+1]))
}

// TestDeadStackBanner_HitRectsUnaffected proves the banner does not corrupt
// mouse targets: the process chips keep their row (the banner is BELOW them),
// clicking one still solos, and a click on the first viewport row still lands
// on log row 0 at the shifted origin.
func TestDeadStackBanner_HitRectsUnaffected(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 24})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "p0", Line: "alpha"}))
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{Process: "p0", Line: "beta"}))
	m = clientUpdate(m, ProcessesMsg(deadStackProcsOf(
		domain.ProcessStateRunning, domain.ProcessStateRunning)))
	_ = m.View()
	rowY, ok := m.processPanelRowY()
	require.True(t, ok)
	require.Len(t, m.mustHits().chips, 2)
	chipsBefore := append([]processChipHit(nil), m.mustHits().chips...)

	m = clientUpdate(m, ProcessesMsg(deadStackProcsOf(
		domain.ProcessStateCrashed, domain.ProcessStateCrashed)))
	require.True(t, m.showDeadStackBanner())
	_ = m.View()

	rowYAfter, ok := m.processPanelRowY()
	require.True(t, ok)
	assert.Equal(t, rowY, rowYAfter, "the banner is below the chips; their row cannot move")
	// Chip Y/H (and count) are unaffected by the banner, which is what this
	// test actually pins. X/W now widen going running->crashed: stateLabel
	// (issue #92 bug 1 / plan 028 C6) appends " (crashed)" to the chip text,
	// so exact rect equality across that state change is no longer the right
	// assertion — the earlier chips-before snapshot predates the label.
	require.Len(t, m.mustHits().chips, len(chipsBefore))
	for i, c := range m.mustHits().chips {
		assert.Equal(t, chipsBefore[i].Rect.Y, c.Rect.Y, "chip %d row unchanged by the banner", i)
		assert.Equal(t, chipsBefore[i].Rect.H, c.Rect.H, "chip %d height unchanged by the banner", i)
	}
	assert.Contains(t, ansi.Strip(m.processPanel()), "(crashed)")

	// The chip still solos on click.
	m = clientUpdate(m, clickAt(2, rowYAfter))
	assert.Equal(t, "p0", m.soloProcess)
	m = clientUpdate(m, clickAt(2, rowYAfter))
	assert.Empty(t, m.soloProcess)

	// A click on the banner row itself must not be read as a log-pane click.
	_, bannerIdx, found := deadStackBannerRow(m)
	require.True(t, found)
	_, oy := m.viewportOrigin()
	require.Less(t, bannerIdx, oy)
	before := m.logCursorSeq
	m = clientUpdate(m, clickAt(5, bannerIdx))
	assert.Equal(t, before, m.logCursorSeq, "banner row is not a log row")

	// ...while the first real viewport row still is, at the shifted origin.
	m = clientUpdate(m, clickAt(5, oy))
	assert.NotEqual(t, before, m.logCursorSeq)
}

// TestDeadStackBanner_YieldsToTheLastContentRow pins the room check: on a frame
// with no row to spare the banner stands down rather than overflowing the
// frame, exactly as the requests header row does.
func TestDeadStackBanner_YieldsToTheLastContentRow(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	// Default chrome above is 3 (menu + panel + spacer) and 1 below, so h=5
	// leaves exactly one content row: the banner must not take it.
	tight := deadStackModel(t, 60, 5, domain.ProcessStateCrashed)
	assert.False(t, tight.showDeadStackBanner())
	assertFrameContract(t, tight)

	roomy := deadStackModel(t, 60, 6, domain.ProcessStateCrashed)
	assert.True(t, roomy.showDeadStackBanner())
	assertFrameContract(t, roomy)
}

// TestDeadStackBanner_TextIsPureFunctionOfTheSnapshot keeps the predicate
// itself covered directly, including the two states the CLI sibling
// (internal/cli/dead_stack.go) also refuses to treat as failures.
func TestDeadStackBanner_TextIsPureFunctionOfTheSnapshot(t *testing.T) {
	assert.Empty(t, deadStackBannerText(nil))
	assert.Empty(t, deadStackBannerText(deadStackProcsOf(domain.ProcessStateCompleted)))
	assert.Empty(t, deadStackBannerText(deadStackProcsOf(domain.ProcessStateStopped)))
	assert.False(t, deadStackProcs(deadStackProcsOf(domain.ProcessStateCompleted)),
		"completed is a task's terminal SUCCESS, never a dead stack")
	assert.True(t, deadStackProcs(deadStackProcsOf(domain.ProcessStateBlocked)))

	// Exhaustive over the domain enum: a 9th state must be triaged here too.
	for _, s := range domain.AllProcessStates() {
		single := deadStackProcsOf(s)
		assert.Equal(t, s.IsTerminalFailure(), deadStackProcs(single),
			"single-process stack in state %s", s)
	}
}
