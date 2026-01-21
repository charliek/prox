package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

func TestNewModel(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	assert.Equal(t, ModeNormal, model.mode)
	assert.False(t, model.ready)
	assert.Empty(t, model.logEntries)
}

func TestModel_HandleKey_Quit(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	// Test quit with 'q'
	newModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assert.NotNil(t, cmd)
	_ = newModel
}

func TestModel_HandleKey_ModeSwitch(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	// Test switching to help mode
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m := newModel.(Model)
	assert.Equal(t, ModeHelp, m.mode)

	// Test switching to filter mode
	model = NewModel(sup, logMgr)
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = newModel.(Model)
	assert.Equal(t, ModeFilter, m.mode)

	// Test switching to search mode
	model = NewModel(sup, logMgr)
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = newModel.(Model)
	assert.Equal(t, ModeSearch, m.mode)

	// Test switching to string filter mode
	model = NewModel(sup, logMgr)
	newModel, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = newModel.(Model)
	assert.Equal(t, ModeStringFilter, m.mode)
}

func TestModel_HandleKey_EscClearsFilters(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)
	model.soloProcess = "test"
	model.searchPattern = "pattern"

	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m := newModel.(Model)

	assert.Empty(t, m.soloProcess)
	assert.Empty(t, m.searchPattern)
}

func TestModel_LogEntryMsg(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)
	model.ready = true // Set ready to avoid viewport issues

	entry := domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      "test log line",
	}

	newModel, _ := model.Update(LogEntryMsg(entry))
	m := newModel.(Model)

	assert.Len(t, m.logEntries, 1)
	assert.Equal(t, "test", m.logEntries[0].Process)
	assert.Equal(t, "test log line", m.logEntries[0].Line)
}

func TestModel_LogEntryLimit(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)
	model.ready = true

	// Add more than 1000 entries
	for i := 0; i < 1005; i++ {
		entry := domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "test",
			Stream:    domain.StreamStdout,
			Line:      "test log line",
		}
		newModel, _ := model.Update(LogEntryMsg(entry))
		model = newModel.(Model)
	}

	// Should be capped at 1000
	assert.Len(t, model.logEntries, 1000)
}

func TestFilteredEntries(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	// Add some log entries
	model.logEntries = []domain.LogEntry{
		{Process: "web", Line: "web log 1"},
		{Process: "api", Line: "api log 1"},
		{Process: "web", Line: "web log 2"},
		{Process: "api", Line: "api log 2"},
	}

	// No filter - should return all
	entries := model.filteredEntries()
	assert.Len(t, entries, 4)

	// Solo process filter
	model.soloProcess = "web"
	entries = model.filteredEntries()
	assert.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, "web", e.Process)
	}

	// String filter
	model.soloProcess = ""
	model.searchPattern = "log 1"
	entries = model.filteredEntries()
	assert.Len(t, entries, 2)
	for _, e := range entries {
		assert.Contains(t, e.Line, "log 1")
	}
}

func TestContainsIgnoreCase(t *testing.T) {
	tests := []struct {
		s      string
		substr string
		want   bool
	}{
		{"Hello World", "world", true},
		{"Hello World", "WORLD", true},
		{"Hello World", "hello", true},
		{"Hello World", "xyz", false},
		{"", "", true},
		{"test", "", true},
		{"", "test", false},
	}

	for _, tt := range tests {
		got := containsIgnoreCase(tt.s, tt.substr)
		assert.Equal(t, tt.want, got, "containsIgnoreCase(%q, %q)", tt.s, tt.substr)
	}
}

func TestUpdateSearchMatches(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	model.logEntries = []domain.LogEntry{
		{Line: "error: something failed"},
		{Line: "info: all good"},
		{Line: "error: another failure"},
		{Line: "debug: test message"},
	}

	model.searchPattern = "error"
	model.updateSearchMatches()

	assert.Len(t, model.searchMatches, 2)
	assert.Equal(t, 0, model.searchMatches[0])
	assert.Equal(t, 2, model.searchMatches[1])
}

func TestFollowModeDefaults(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)

	// followMode should default to true
	assert.True(t, model.followMode)
}

func TestFollowModeDisabledOnScrollUp(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	tests := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"k key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}},
		{"up arrow", tea.KeyMsg{Type: tea.KeyUp}},
		{"g key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}},
		{"home key", tea.KeyMsg{Type: tea.KeyHome}},
		{"pgup key", tea.KeyMsg{Type: tea.KeyPgUp}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(sup, logMgr)
			assert.True(t, model.followMode) // starts true

			newModel, _ := model.Update(tt.key)
			m := newModel.(Model)

			assert.False(t, m.followMode, "followMode should be false after %s", tt.name)
		})
	}
}

func TestFollowModeEnabledOnGoToBottom(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	tests := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"G key", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}},
		{"end key", tea.KeyMsg{Type: tea.KeyEnd}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(sup, logMgr)
			model.followMode = false // start with followMode disabled

			newModel, _ := model.Update(tt.key)
			m := newModel.(Model)

			assert.True(t, m.followMode, "followMode should be true after %s", tt.name)
		})
	}
}

func TestFollowModeToggle(t *testing.T) {
	logMgr := logs.NewManager(logs.DefaultManagerConfig())
	sup := supervisor.New(nil, logMgr, nil, supervisor.DefaultSupervisorConfig())

	model := NewModel(sup, logMgr)
	assert.True(t, model.followMode) // starts true

	// First toggle - should disable
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m := newModel.(Model)
	assert.False(t, m.followMode)

	// Second toggle - should enable
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m = newModel.(Model)
	assert.True(t, m.followMode)
}
