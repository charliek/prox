package tui

import (
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

func readyFilterMenuModel() ClientModel {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m.processes = []domain.ProcessInfo{
		{Name: "web"},
		{Name: "api"},
	}
	m.logEntries = []domain.LogEntry{
		{Process: "web", Line: "err", DisplaySeq: 1},
		{Process: "api", Line: "ok", DisplaySeq: 2},
	}
	m.logMeta = map[int64]logMeta{
		1: {hasLevel: true, level: LogLevelError},
		2: {hasLevel: true, level: LogLevelInfo},
	}
	return m
}

func filterMenuRowIndex(t *testing.T, m ClientModel, label string) int {
	t.Helper()
	items := m.menuItems(MenuFilter)
	for i, it := range items {
		if it.Label == label {
			return i
		}
	}
	t.Fatalf("filter menu row %q not found in %d items", label, len(items))
	return -1
}

func activateFilterMenuRow(m ClientModel, row int) ClientModel {
	m.openMenuFirst(MenuFilter)
	m.menuHighlight = row
	return clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestFilterMenu_LogsRowsReflectProcessesAndLevels(t *testing.T) {
	m := readyFilterMenuModel()
	m.setLogsFilterQuery("level:error")

	items := m.menuItems(MenuFilter)
	require.GreaterOrEqual(t, len(items), 8)

	webIdx := filterMenuRowIndex(t, m, "web")
	apiIdx := filterMenuRowIndex(t, m, "api")
	errIdx := filterMenuRowIndex(t, m, "error")
	infoIdx := filterMenuRowIndex(t, m, "info")

	require.NotNil(t, items[webIdx].Checked)
	require.NotNil(t, items[apiIdx].Checked)
	require.NotNil(t, items[errIdx].Checked)
	require.NotNil(t, items[infoIdx].Checked)
	assert.True(t, *items[webIdx].Checked)
	assert.True(t, *items[apiIdx].Checked)
	assert.True(t, *items[errIdx].Checked, "level:error marks error checked")
	assert.False(t, *items[infoIdx].Checked)
}

func TestFilterMenu_LogsProcUncheckMatchesTypedQuery(t *testing.T) {
	m := readyFilterMenuModel()

	row := filterMenuRowIndex(t, m, "api")
	m = activateFilterMenuRow(m, row)

	want, err := ParseLogsFilter("-proc:api")
	require.NoError(t, err)
	assert.Equal(t, want, m.logsFilter.LastGood)
	assert.Equal(t, want.Serialize(), m.logsFilter.RawQuery)

	m = activateFilterMenuRow(m, row)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
	assert.Empty(t, m.logsFilter.RawQuery)
}

func TestFilterMenu_LogsLevelToggleMatchesTypedQuery(t *testing.T) {
	m := readyFilterMenuModel()

	row := filterMenuRowIndex(t, m, "warn")
	m = activateFilterMenuRow(m, row)

	want, err := ParseLogsFilter("level:warn")
	require.NoError(t, err)
	assert.Equal(t, want, m.logsFilter.LastGood)
	assert.Equal(t, want.Serialize(), m.logsFilter.RawQuery)

	m = activateFilterMenuRow(m, row)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
}

func TestFilterMenu_LogsMenuEditCanonicalizesBarText(t *testing.T) {
	m := readyFilterMenuModel()
	m.logsFilter.RawQuery = `proc:api level:error`
	m.logsFilter.LastGood, _ = ParseLogsFilter(m.logsFilter.RawQuery)

	row := filterMenuRowIndex(t, m, "warn")
	m = activateFilterMenuRow(m, row)

	assert.Equal(t, m.logsFilter.LastGood.Serialize(), m.logsFilter.RawQuery)
	assert.Nil(t, m.logsFilter.ParseErr)
}

func TestFilterMenu_LogsClearAllResetsActiveView(t *testing.T) {
	m := readyFilterMenuModel()
	m.setLogsFilterQuery("proc:web level:error")

	row := filterMenuRowIndex(t, m, "Clear all filters")
	m = activateFilterMenuRow(m, row)

	assert.Empty(t, m.logsFilter.RawQuery)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
	assert.Nil(t, m.logsFilter.ParseErr)
}

func TestFilterMenu_LogsWorksFromLastGoodOnParseError(t *testing.T) {
	m := readyFilterMenuModel()
	m.logsFilter.RawQuery = "level:chatty"
	m.logsFilter.ParseErr = &FilterQueryError{Msg: "bad level"}
	m.logsFilter.LastGood, _ = ParseLogsFilter("level:error")

	row := filterMenuRowIndex(t, m, "warn")
	m = activateFilterMenuRow(m, row)

	want, err := ParseLogsFilter("level:error level:warn")
	require.NoError(t, err)
	assert.Equal(t, want, m.logsFilter.LastGood)
	assert.Equal(t, want.Serialize(), m.logsFilter.RawQuery)
	assert.Nil(t, m.logsFilter.ParseErr)
}

func requestsFilterMenuModel() ClientModel {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m.setViewMode(ViewModeRequests)
	m.proxyRequests = []proxy.RequestRecord{
		{ID: "1", Method: "GET", StatusCode: 200, InFlight: false, Timestamp: time.Now()},
		{ID: "2", Method: "POST", StatusCode: 500, InFlight: true, Timestamp: time.Now()},
		{ID: "3", Method: "get", StatusCode: 404, InFlight: false, Timestamp: time.Now()},
	}
	return m
}

func TestFilterMenu_RequestsMethodAndStatusToggles(t *testing.T) {
	m := requestsFilterMenuModel()

	postRow := filterMenuRowIndex(t, m, "POST")
	m = activateFilterMenuRow(m, postRow)

	want, err := ParseRequestsFilter("method:POST")
	require.NoError(t, err)
	assert.Equal(t, want, m.requestsFilter.LastGood)
	assert.Equal(t, want.Serialize(), m.requestsFilter.RawQuery)

	fourRow := filterMenuRowIndex(t, m, "4xx")
	m = activateFilterMenuRow(m, fourRow)

	want, err = ParseRequestsFilter("method:POST status:4xx")
	require.NoError(t, err)
	assert.Equal(t, want, m.requestsFilter.LastGood)
	assert.Equal(t, want.Serialize(), m.requestsFilter.RawQuery)
}

func TestFilterMenu_RequestsInFlightRadiosMutuallyExclusive(t *testing.T) {
	m := requestsFilterMenuModel()

	inFlightRow := filterMenuRowIndex(t, m, "In flight")
	m = activateFilterMenuRow(m, inFlightRow)
	assert.Equal(t, "in_flight:true", m.requestsFilter.RawQuery)

	items := m.menuItems(MenuFilter)
	anyIdx := filterMenuRowIndex(t, m, "Any")
	inIdx := filterMenuRowIndex(t, m, "In flight")
	doneIdx := filterMenuRowIndex(t, m, "Completed")
	require.NotNil(t, items[anyIdx].Selected)
	require.NotNil(t, items[inIdx].Selected)
	require.NotNil(t, items[doneIdx].Selected)
	assert.False(t, *items[anyIdx].Selected)
	assert.True(t, *items[inIdx].Selected)
	assert.False(t, *items[doneIdx].Selected)

	doneRow := filterMenuRowIndex(t, m, "Completed")
	m = activateFilterMenuRow(m, doneRow)
	assert.Equal(t, "in_flight:false", m.requestsFilter.RawQuery)

	items = m.menuItems(MenuFilter)
	assert.False(t, *items[anyIdx].Selected)
	assert.False(t, *items[inIdx].Selected)
	assert.True(t, *items[doneIdx].Selected)

	anyRow := filterMenuRowIndex(t, m, "Any")
	m = activateFilterMenuRow(m, anyRow)
	assert.True(t, m.requestsFilter.LastGood.IsEmpty())
}

func TestFilterMenu_RequestsClearAllResetsActiveView(t *testing.T) {
	m := requestsFilterMenuModel()
	m.setRequestsFilterQuery("method:GET status:5xx")

	row := filterMenuRowIndex(t, m, "Clear all filters")
	m = activateFilterMenuRow(m, row)

	assert.Empty(t, m.requestsFilter.RawQuery)
	assert.True(t, m.requestsFilter.LastGood.IsEmpty())
}

func TestMenu_FOpensFilterDropdownWhenBarVisible(t *testing.T) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.True(t, m.settings.MenuBar)

	m = clientUpdate(m, keyRune('f'))
	assert.True(t, m.menuOpen())
	assert.Equal(t, int(MenuFilter), m.openMenu)
	assert.Equal(t, ModeNormal, m.mode)
}

func TestMenu_FNoOpWhenMenuBarHidden(t *testing.T) {
	dir := t.TempDir()
	withTestSettingsPath(t, filepath.Join(dir, "config.toml"))

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = clientUpdate(m, keyRune('m'))
	require.False(t, m.settings.MenuBar)

	m = clientUpdate(m, keyRune('f'))
	assert.False(t, m.menuOpen())
	assert.Equal(t, ModeNormal, m.mode)
}

func TestMenu_FOpenerDoesNotFireInTextModes(t *testing.T) {
	for _, mode := range []Mode{ModeSearch, ModeStringFilter} {
		m := newTestModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		m.mode = mode
		m.textInput.Focus()
		m.textInput.SetValue("")

		m = clientUpdate(m, keyRune('f'))
		assert.False(t, m.menuOpen(), "mode=%v", mode)
		assert.Equal(t, mode, m.mode)
		assert.Equal(t, "f", m.textInput.Value())
	}
}

func TestFilterMenu_PositiveProcListShowsOnlyThoseChecked(t *testing.T) {
	m := readyFilterMenuModel()
	m.setLogsFilterQuery("proc:web")

	items := m.menuItems(MenuFilter)
	webIdx := filterMenuRowIndex(t, m, "web")
	apiIdx := filterMenuRowIndex(t, m, "api")
	assert.True(t, *items[webIdx].Checked)
	assert.False(t, *items[apiIdx].Checked)
}
