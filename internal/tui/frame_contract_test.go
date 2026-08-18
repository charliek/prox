package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// assertFrameContract checks the plan 023 T5 invariant: every rendered frame
// is exactly height rows of exactly width cells (ANSI display width). Shared
// so later geometry commits can re-run the sweep.
func assertFrameContract(t *testing.T, m ClientModel) {
	t.Helper()
	require.True(t, m.ready, "frame contract requires a ready model")
	require.Greater(t, m.width, 0)
	require.Greater(t, m.height, 0)
	frame := m.View()
	lines := strings.Split(frame, "\n")
	require.Equal(t, m.height, len(lines), "frame row count (w=%d h=%d)", m.width, m.height)
	for i, line := range lines {
		assert.Equal(t, m.width, ansi.StringWidth(line),
			"row %d width (w=%d h=%d)", i, m.width, m.height)
	}
}

// TestFrameContract_Sweep is T5 (plan 023 C1): baseline frame-contract sweep
// across views × settings × modal/menu states × sizes. Degenerate frames
// whose chrome alone exceeds height are skipped — mainView does not yet
// clamp to height (geometry commits C6/C7 tighten that); "where feasible"
// per the plan pin.
func TestFrameContract_Sweep(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24},
		{120, 40},
		{40, 12},
		{20, 8},
		{10, 6},
		{2, 6},
		{1, 6},
	}
	settingsCombos := []Settings{
		DefaultSettings(),
		{MenuBar: true, ProcessPanel: false, Timestamps: true, Wrap: false},
		{MenuBar: true, ProcessPanel: true, Timestamps: false, Wrap: true},
		{MenuBar: false, ProcessPanel: false, Timestamps: false, Wrap: false},
		{MenuBar: false, ProcessPanel: true, Timestamps: true, Wrap: true},
	}

	for _, sz := range sizes {
		for _, cfg := range settingsCombos {
			// Feasibility derives from the production chrome formula so C6/C7
			// geometry changes can't silently desync this gate.
			chrome := (&BaseModel{settings: cfg}).chromeHeight()
			if sz.h < chrome+1 {
				continue // not feasible: chrome + min viewport exceed frame
			}
			t.Run(frameSweepName(sz.w, sz.h, cfg, "logs"), func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeLogs)
				assertFrameContract(t, m)
			})
			t.Run(frameSweepName(sz.w, sz.h, cfg, "requests"), func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeRequests)
				assertFrameContract(t, m)
			})
			t.Run(frameSweepName(sz.w, sz.h, cfg, "detail"), func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeRequestDetail)
				assertFrameContract(t, m)
			})
			// The modal overlays any chrome set — no MenuBar precondition.
			// w<20 is skipped: the modal's minimum chrome exceeds the frame
			// (degenerate clamping is C6/C7 scope).
			if sz.w >= 20 {
				t.Run(frameSweepName(sz.w, sz.h, cfg, "help"), func(t *testing.T) {
					m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeLogs)
					m = clientUpdate(m, keyRune('?'))
					require.Equal(t, ModeHelp, m.mode)
					assertFrameContract(t, m)
				})
			}
			if cfg.MenuBar && sz.w >= 20 {
				t.Run(frameSweepName(sz.w, sz.h, cfg, "menu-view"), func(t *testing.T) {
					m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeLogs)
					m = clientUpdate(m, keyRune('v'))
					require.True(t, m.menuOpen())
					assertFrameContract(t, m)
				})
			}
		}
	}
}

func frameSweepName(w, h int, cfg Settings, label string) string {
	return fmt.Sprintf("%s_w%dh%d_menu%t_panel%t_ts%t_wrap%t",
		label, w, h, cfg.MenuBar, cfg.ProcessPanel, cfg.Timestamps, cfg.Wrap)
}

func primedFrameModel(t *testing.T, w, h int, cfg Settings, mode ViewMode) ClientModel {
	t.Helper()
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))
	m := newTestModel()
	m.settings = cfg
	m.projectName = "demo"
	m.processes = []domain.ProcessInfo{
		{Name: "web", State: domain.ProcessStateRunning},
	}
	m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: h})
	m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
		Process: "web", Line: "hello from web",
	}))
	m = clientUpdate(m, ProxyRequestMsg(newArrival("req-001", "/x")))
	switch mode {
	case ViewModeLogs:
		// default
	case ViewModeRequests:
		m.setViewMode(ViewModeRequests)
	case ViewModeRequestDetail:
		m.viewMode = ViewModeRequestDetail
		m.requestDetail = &RequestDetailData{
			ID: "req-001", Method: "GET", URL: "/x", StatusCode: 200,
			RequestBody: &BodyData{Data: "body", ContentType: "text/plain"},
		}
		m.detailLoading = false
		m.updateViewport()
	}
	return m
}

// TestFrameContract_DeadStackSweep re-runs the T5 sweep with the dead-stack
// banner up (plan 028 C4).
//
// The banner is the first chrome row whose height changes on a DATA message
// rather than on a WindowSizeMsg or a settings toggle, so it is the first thing
// that can desync geometry from rendering mid-session -- the exact failure plan
// 024 spent a batch fixing. The bespoke banner tests check its own behaviour;
// this checks that the frame as a WHOLE still obeys the contract with the extra
// row present, across every size, settings combo and view the baseline sweep
// covers.
//
// It drives the banner up through the real ProcessesMsg path, not by setting
// fields, because the relayout hook lives on that path: primed with a running
// process and then given a crashed snapshot, this fails if that hook is ever
// removed.
func TestFrameContract_DeadStackSweep(t *testing.T) {
	sizes := []struct{ w, h int }{
		{80, 24},
		{120, 40},
		{40, 12},
		{20, 8},
		{10, 6},
		{2, 6},
		{1, 6},
	}
	settingsCombos := []Settings{
		DefaultSettings(),
		{MenuBar: true, ProcessPanel: false, Timestamps: true, Wrap: false},
		{MenuBar: true, ProcessPanel: true, Timestamps: false, Wrap: true},
		{MenuBar: false, ProcessPanel: false, Timestamps: false, Wrap: false},
		{MenuBar: false, ProcessPanel: true, Timestamps: true, Wrap: true},
	}
	// Every dead-stack shape that must raise the banner, including the mixed
	// and "nothing live, one failure" cases.
	stacks := map[string][]domain.ProcessInfo{
		"crashed": {
			{Name: "web", State: domain.ProcessStateCrashed},
			{Name: "api", State: domain.ProcessStateCrashed},
		},
		"blocked": {
			{Name: "web", State: domain.ProcessStateBlocked, BlockedOn: []string{"db"}},
		},
		"mixed": {
			{Name: "web", State: domain.ProcessStateCrashed},
			{Name: "api", State: domain.ProcessStateBlocked, BlockedOn: []string{"db"}},
			{Name: "seed", State: domain.ProcessStateCompleted},
		},
	}

	for _, sz := range sizes {
		for _, cfg := range settingsCombos {
			// Same feasibility rule as the baseline sweep, plus the banner's
			// own row -- derived from the production formula so a geometry
			// change cannot silently desync this gate.
			chrome := (&BaseModel{settings: cfg}).chromeHeight() + 1
			if sz.h < chrome+1 {
				continue
			}
			for name, procs := range stacks {
				for _, mode := range []ViewMode{ViewModeLogs, ViewModeRequests, ViewModeRequestDetail} {
					label := fmt.Sprintf("dead-%s-%s", name, viewSweepLabel(mode))
					t.Run(frameSweepName(sz.w, sz.h, cfg, label), func(t *testing.T) {
						m := primedFrameModel(t, sz.w, sz.h, cfg, mode)
						require.False(t, m.showDeadStackBanner(),
							"precondition: the primed model's stack is alive")

						m = clientUpdate(m, ProcessesMsg(procs))
						require.True(t, m.showDeadStackBanner(),
							"the snapshot is a dead stack, so the banner must be up")
						assertFrameContract(t, m)

						// ...and giving the stack back must return the row.
						m = clientUpdate(m, ProcessesMsg([]domain.ProcessInfo{
							{Name: "web", State: domain.ProcessStateRunning},
						}))
						require.False(t, m.showDeadStackBanner())
						assertFrameContract(t, m)
					})
				}
			}
		}
	}
}

func viewSweepLabel(mode ViewMode) string {
	switch mode {
	case ViewModeRequests:
		return "requests"
	case ViewModeRequestDetail:
		return "detail"
	default:
		return "logs"
	}
}

// TestFrameContract_LiveSearchStates covers the state combination plan 030
// introduced and nothing before it could reach: ModeSearch (the typing bar in
// the footer) with the view's committed query ALREADY non-empty, so the logs
// pane carries the 2-column marker gutter, a ❯ cursor row and inline highlight
// runs WHILE the input is focused. Every row of a prox frame is exactly
// terminal-width wide, so a single mis-counted row or over-wide footer wraps,
// scrolls the terminal, and (because the renderer only repaints changed rows)
// never heals — the failure mode the plan-030 PTY smoke went looking for.
//
// TestFrameContract_Sweep does not reach here: it drives views, settings, help
// and menus, but never a text-input mode. Wrap is included because the marker
// gutter is the one thing that genuinely moves wrap points, and the sizes go
// narrow because the footer carries the fixed-width search input next to the
// status bands.
func TestFrameContract_LiveSearchStates(t *testing.T) {
	sizes := []struct{ w, h int }{{120, 35}, {80, 24}, {40, 12}, {20, 8}}
	for _, sz := range sizes {
		for _, wrap := range []bool{false, true} {
			cfg := DefaultSettings()
			cfg.Wrap = wrap
			name := fmt.Sprintf("w%dh%d_wrap%t", sz.w, sz.h, wrap)

			t.Run("logs_"+name, func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeLogs)
				for i := 0; i < 30; i++ {
					m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
						Process: "web", Line: fmt.Sprintf("line %02d hello world", i),
					}))
				}

				// (a) bar open with a live query parked on a match (the smoke's F3).
				m = clientUpdate(m, keyRune('/'))
				m = typeSearch(m, "hello")
				require.Equal(t, ModeSearch, m.mode)
				require.NotEmpty(t, m.logSearchQuery, "the new combo: typing bar AND committed query")
				require.GreaterOrEqual(t, m.logCursorIdx, 0, "a match is parked, so the ❯ gutter is rendered")
				assertFrameContract(t, m)

				// (b) the same, after entries arrive while the bar is open.
				for i := 0; i < 5; i++ {
					m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
						Process: "web", Line: fmt.Sprintf("arrival %02d hello", i),
					}))
				}
				assertFrameContract(t, m)

				// (c) after Esc cancels back to the origin (the smoke's F4).
				m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
				require.Equal(t, ModeNormal, m.mode)
				require.Empty(t, m.logSearchQuery)
				assertFrameContract(t, m)

				// (d) committed query, then reopened seeded (the smoke's F5/F6).
				m = commitSearch(m, "hello")
				require.Equal(t, "hello", m.logSearchQuery)
				assertFrameContract(t, m)
				m = clientUpdate(m, keyRune('/'))
				require.Equal(t, "hello", m.textInput.Value(), "reopened seeded")
				assertFrameContract(t, m)
			})

			t.Run("requests_"+name, func(t *testing.T) {
				m := primedFrameModel(t, sz.w, sz.h, cfg, ViewModeRequests)
				for i := 0; i < 20; i++ {
					m = clientUpdate(m, ProxyRequestMsg(newArrival(
						fmt.Sprintf("req-%03d", i), fmt.Sprintf("/hello/%02d", i))))
				}

				m = clientUpdate(m, keyRune('/'))
				m = typeSearch(m, "hello")
				require.Equal(t, ModeSearch, m.mode)
				require.NotEmpty(t, m.requestSearchQuery)
				assertFrameContract(t, m)

				m = clientUpdate(m, ProxyRequestMsg(newArrival("req-late", "/hello/late")))
				assertFrameContract(t, m)

				m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
				require.Empty(t, m.requestSearchQuery)
				assertFrameContract(t, m)

				m = commitSearch(m, "hello")
				assertFrameContract(t, m)
				m = clientUpdate(m, keyRune('/'))
				assertFrameContract(t, m)
			})
		}
	}
}
