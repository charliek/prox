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

func benchLogLinesWith(line func(int) string) []domain.LogEntry {
	out := make([]domain.LogEntry, benchLogN)
	for i := 0; i < benchLogN; i++ {
		out[i] = domain.LogEntry{
			Timestamp: time.Unix(0, int64(i)),
			Process:   "api",
			Line:      line(i),
		}
	}
	return out
}

func benchLogLines() []domain.LogEntry {
	return benchLogLinesWith(func(i int) string {
		switch i % 4 {
		case 0:
			return fmt.Sprintf(`{"level":"info","msg":"ok","n":%d,"svc":"api"}`, i)
		case 1:
			return fmt.Sprintf(`{"level":"error","msg":"fail","code":%d}`, 400+i%50)
		case 2:
			return fmt.Sprintf("level=info plain log line %d hello world", i)
		default:
			return fmt.Sprintf("something happened without a level %d", i)
		}
	})
}

func benchPlainLogLines() []domain.LogEntry {
	return benchLogLinesWith(func(i int) string {
		return fmt.Sprintf("level=info plain log line %d hello world", i)
	})
}

func benchJSONLogLines() []domain.LogEntry {
	return benchLogLinesWith(func(i int) string {
		return fmt.Sprintf(`{"level":"info","msg":"ok","n":%d,"svc":"api"}`, i)
	})
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
	benchmarkUpdateViewport(b, benchLogLines())
}

func benchmarkUpdateViewport(b *testing.B, lines []domain.LogEntry) {
	m := newTestModel()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m.settings.Wrap = false
	m.settings.Timestamps = true
	for _, e := range lines {
		m.appendLogEntry(e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.updateViewport()
	}
}

func BenchmarkUpdateViewport_Plain(b *testing.B) {
	benchmarkUpdateViewport(b, benchPlainLogLines())
}

func BenchmarkUpdateViewport_JSON(b *testing.B) {
	benchmarkUpdateViewport(b, benchJSONLogLines())
}

func benchmarkFilteredEntries(b *testing.B, setup func(*BaseModel)) {
	m := newTestBaseModel()
	for _, e := range benchLogLines() {
		m.appendLogEntry(e)
	}
	setup(m)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.filteredEntries()
	}
}

func BenchmarkFilteredEntries(b *testing.B) {
	benchmarkFilteredEntries(b, func(m *BaseModel) {
		m.setLogsFilterQuery(`level:error OR "hello"`)
	})
}

func BenchmarkFilteredEntries_NoFilter(b *testing.B) {
	benchmarkFilteredEntries(b, func(*BaseModel) {})
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
