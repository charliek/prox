package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/domain"
)

// Benchmark surfaces for plan 023 C14 (WS-C). Target stable entry points
// (appendLogEntry, updateViewport, filteredEntries, menuItems) so the same
// file compares pre- and post-change.

const benchLogN = 500

func benchLogLines() []domain.LogEntry {
	out := make([]domain.LogEntry, benchLogN)
	for i := 0; i < benchLogN; i++ {
		var line string
		switch i % 4 {
		case 0:
			line = fmt.Sprintf(`{"level":"info","msg":"ok","n":%d,"svc":"api"}`, i)
		case 1:
			line = fmt.Sprintf(`{"level":"error","msg":"fail","code":%d}`, 400+i%50)
		case 2:
			line = fmt.Sprintf("level=info plain log line %d hello world", i)
		default:
			line = fmt.Sprintf("something happened without a level %d", i)
		}
		out[i] = domain.LogEntry{
			Timestamp: time.Unix(0, int64(i)),
			Process:   "api",
			Line:      line,
		}
	}
	return out
}

func BenchmarkLogIngest(b *testing.B) {
	lines := benchLogLines()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := newTestBaseModel()
		for _, e := range lines {
			m.appendLogEntry(e)
		}
	}
}

func BenchmarkLogRowRender(b *testing.B) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m.settings.Wrap = false
	m.settings.Timestamps = true
	for _, e := range benchLogLines() {
		m.appendLogEntry(e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.updateViewport()
	}
}

func BenchmarkFilteredEntries(b *testing.B) {
	m := newTestBaseModel()
	for _, e := range benchLogLines() {
		m.appendLogEntry(e)
	}
	m.setLogsFilterQuery(`level:error OR "hello"`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.filteredEntries()
	}
}

func BenchmarkThemeMenuItems(b *testing.B) {
	dir := b.TempDir()
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("user-%02d.toml", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`base = "dark"`), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	prev := themesDirFunc
	themesDirFunc = func() string { return dir }
	defer func() { themesDirFunc = prev }()

	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.openMenuFirst(MenuTheme)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.menuItems(MenuTheme)
	}
}
