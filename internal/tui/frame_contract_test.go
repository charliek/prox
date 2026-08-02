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
