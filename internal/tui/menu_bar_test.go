package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func menuBarPlainLine(t *testing.T, m ClientModel) string {
	t.Helper()
	// Deliberately mainView, not View(): View() prepends OSC-22 to the whole
	// frame; this helper needs the first mainView row for menu-bar layout text.
	frame := m.mainView(footerMsg{})
	lines := strings.Split(frame, "\n")
	require.NotEmpty(t, lines)
	return stripANSI(lines[0])
}

func TestMenuBar_LayoutBrandProjectCellsLeftAligned(t *testing.T) {
	for _, w := range []int{80, 40} {
		t.Run(fmtWidth(w), func(t *testing.T) {
			m := newTestModel()
			m.projectName = "demo"
			m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: 24})
			plain := menuBarPlainLine(t, m)

			idxProx := strings.Index(plain, "prox")
			idxDemo := strings.Index(plain, "demo")
			idxView := strings.Index(plain, "View")
			require.GreaterOrEqual(t, idxProx, 0)
			require.GreaterOrEqual(t, idxDemo, 0)
			require.GreaterOrEqual(t, idxView, 0)
			assert.Less(t, idxProx, idxDemo)
			assert.Less(t, idxDemo, idxView)
			assert.Less(t, strings.Index(plain, "Filter"), strings.Index(plain, "Theme"))
		})
	}
}

func TestMenuBar_OmitsEmptyProjectName(t *testing.T) {
	m := newTestModel()
	m.projectName = ""
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	plain := menuBarPlainLine(t, m)
	assert.True(t, strings.HasPrefix(strings.TrimLeft(plain, " "), "prox "))
	assert.NotContains(t, plain, "prox  prox")
}

func TestMenuBar_TruncatesAtNarrowWidth(t *testing.T) {
	m := newTestModel()
	m.projectName = "very-long-project-name"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 24, Height: 12})
	frame := m.View()
	line := strings.Split(frame, "\n")[0]
	assert.Equal(t, 24, ansi.StringWidth(line))
}

func TestMenuBar_CellStylesClosedOpenHover(t *testing.T) {
	withTestTheme(t, "tokyo-night")
	pinTrueColorProfile(t)

	th := CurrentTheme()
	closedStyle := lipgloss.NewStyle().
		Foreground(th.Title).
		Background(th.HeaderBG)
	selStyle := lipgloss.NewStyle().
		Foreground(th.SelectionFG).
		Background(th.SelectionBG)
	closedView := closedStyle.Render(menuCellText(MenuView))
	closedFilter := closedStyle.Render(menuCellText(MenuFilter))
	selView := selStyle.Render(menuCellText(MenuView))

	m := newTestModel()
	m.projectName = "demo"
	m.width = 80
	m.settings.MenuBar = true

	bar := m.renderMenuBar()
	assert.Contains(t, bar, closedView)
	assert.Contains(t, bar, closedFilter)

	m.hoveredMenuCell = int(MenuView)
	bar = m.renderMenuBar()
	assert.Contains(t, bar, selView)
	assert.Contains(t, bar, closedFilter)
	require.NotEqual(t, closedView, selView, "closed vs selection styles must differ")

	m.hoveredMenuCell = -1
	m.openMenuFirst(MenuView)
	bar = m.renderMenuBar()
	assert.Contains(t, bar, selView)
}

func TestMenuBar_HitRectsFollowLeftCells(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	hits := m.mustHits()
	require.Len(t, hits.menuCells, 3)
	assert.Equal(t, MenuView, hits.menuCells[0].ID)

	m = clientUpdate(m, clickAt(hits.menuCells[0].Rect.X, hits.menuCells[0].Rect.Y))
	assert.True(t, m.menuOpen())
	assert.Equal(t, int(MenuView), m.openMenu)
}

func TestConfigPathProjectName(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared", "prox.yaml")
	abs, err := filepath.Abs(shared)
	require.NoError(t, err)
	assert.Equal(t, "shared", ConfigPathProjectName(abs))
	assert.Equal(t, "shared", ConfigPathProjectName(filepath.Join(dir, "shared", "prox.yaml")))
	assert.Empty(t, ConfigPathProjectName(""))
}

func TestStatusProjectName(t *testing.T) {
	assert.Equal(t, "myapp", StatusProjectName("/home/user/projects/myapp"))
	assert.Empty(t, StatusProjectName(""))
}

func TestResolveProjectName_FallbackCwd(t *testing.T) {
	orig := filepathAbs
	t.Cleanup(func() { filepathAbs = orig })
	filepathAbs = func(string) (string, error) {
		return "/tmp/acme-widget", nil
	}
	assert.Equal(t, "acme-widget", resolveProjectName(""))
}

func TestResolveProjectName_ExplicitWins(t *testing.T) {
	assert.Equal(t, "from-cli", resolveProjectName("from-cli"))
}

func TestMenuBarHover_Transitions(t *testing.T) {
	m := newTestModel()
	m.projectName = "demo"
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	_ = m.View()
	viewCell := m.mustHits().menuCells[0]
	filterCell := m.mustHits().menuCells[1]

	assert.Equal(t, -1, m.hoveredMenuCell)

	// Enter closed cell.
	m = clientUpdate(m, motionAt(viewCell.Rect.X, viewCell.Rect.Y))
	assert.Equal(t, int(MenuView), m.hoveredMenuCell)
	assert.False(t, m.menuOpen())

	// Leave to elsewhere.
	m = clientUpdate(m, motionAt(70, 0))
	assert.Equal(t, -1, m.hoveredMenuCell)

	// Re-enter then open via click clears hover.
	m = clientUpdate(m, motionAt(filterCell.Rect.X, filterCell.Rect.Y))
	assert.Equal(t, int(MenuFilter), m.hoveredMenuCell)
	m = clientUpdate(m, clickAt(filterCell.Rect.X, filterCell.Rect.Y))
	assert.True(t, m.menuOpen())
	assert.Equal(t, -1, m.hoveredMenuCell)

	// Close menu.
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, -1, m.hoveredMenuCell)

	// Hide bar clears hover.
	m = clientUpdate(m, motionAt(viewCell.Rect.X, viewCell.Rect.Y))
	assert.Equal(t, int(MenuView), m.hoveredMenuCell)
	m = clientUpdate(m, keyRune('m'))
	assert.False(t, m.settings.MenuBar)
	assert.Equal(t, -1, m.hoveredMenuCell)

	// Show bar again for remaining transitions.
	m = clientUpdate(m, keyRune('m'))
	_ = m.View()
	viewCell = m.mustHits().menuCells[0]
	m = clientUpdate(m, motionAt(viewCell.Rect.X, viewCell.Rect.Y))
	assert.Equal(t, int(MenuView), m.hoveredMenuCell)

	// Resize clears hover.
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 24})
	assert.Equal(t, -1, m.hoveredMenuCell)

	// Help capture clears hover.
	_ = m.View()
	viewCell = m.mustHits().menuCells[0]
	m = clientUpdate(m, motionAt(viewCell.Rect.X, viewCell.Rect.Y))
	require.Equal(t, int(MenuView), m.hoveredMenuCell)
	m = clientUpdate(m, keyRune('?'))
	assert.Equal(t, ModeHelp, m.mode)
	assert.Equal(t, -1, m.hoveredMenuCell)
}

func fmtWidth(w int) string {
	return fmt.Sprintf("w%d", w)
}
