package logs

import (
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeEntryWithProcess(process, line string) domain.LogEntry {
	return domain.LogEntry{
		Timestamp: time.Now(),
		Process:   process,
		Stream:    domain.StreamStdout,
		Line:      line,
	}
}

func TestFilter_MatchesProcess(t *testing.T) {
	filter, err := NewFilter(domain.LogFilter{
		Processes: []string{"web", "api"},
	})
	require.NoError(t, err)

	assert.True(t, filter.Matches(makeEntryWithProcess("web", "hello")))
	assert.True(t, filter.Matches(makeEntryWithProcess("api", "hello")))
	assert.False(t, filter.Matches(makeEntryWithProcess("worker", "hello")))
}

func TestFilter_MatchesSubstring(t *testing.T) {
	filter, err := NewFilter(domain.LogFilter{
		Pattern: "ERROR",
	})
	require.NoError(t, err)

	assert.True(t, filter.Matches(makeEntryWithProcess("web", "ERROR: something went wrong")))
	assert.True(t, filter.Matches(makeEntryWithProcess("web", "An ERROR occurred")))
	assert.False(t, filter.Matches(makeEntryWithProcess("web", "All good")))
	assert.False(t, filter.Matches(makeEntryWithProcess("web", "error lowercase")))
}

func TestFilter_MatchesRegex(t *testing.T) {
	filter, err := NewFilter(domain.LogFilter{
		Pattern: "(?i)error|warn",
		IsRegex: true,
	})
	require.NoError(t, err)

	assert.True(t, filter.Matches(makeEntryWithProcess("web", "ERROR: something")))
	assert.True(t, filter.Matches(makeEntryWithProcess("web", "error lowercase")))
	assert.True(t, filter.Matches(makeEntryWithProcess("web", "WARN: something")))
	assert.False(t, filter.Matches(makeEntryWithProcess("web", "All good")))
}

func TestFilter_InvalidRegex(t *testing.T) {
	_, err := NewFilter(domain.LogFilter{
		Pattern: "[invalid",
		IsRegex: true,
	})
	require.Error(t, err)
}

func TestFilter_CombinedFilters(t *testing.T) {
	filter, err := NewFilter(domain.LogFilter{
		Processes: []string{"web"},
		Pattern:   "ERROR",
	})
	require.NoError(t, err)

	// Matches both
	assert.True(t, filter.Matches(makeEntryWithProcess("web", "ERROR: fail")))

	// Wrong process
	assert.False(t, filter.Matches(makeEntryWithProcess("api", "ERROR: fail")))

	// Wrong pattern
	assert.False(t, filter.Matches(makeEntryWithProcess("web", "All good")))
}

func TestFilterEntries(t *testing.T) {
	entries := []domain.LogEntry{
		makeEntryWithProcess("web", "request 1"),
		makeEntryWithProcess("api", "ERROR: failed"),
		makeEntryWithProcess("web", "ERROR: timeout"),
		makeEntryWithProcess("worker", "processing"),
	}

	t.Run("empty filter returns all", func(t *testing.T) {
		result, err := FilterEntries(entries, domain.LogFilter{})
		require.NoError(t, err)
		assert.Len(t, result, 4)
	})

	t.Run("filter by process", func(t *testing.T) {
		result, err := FilterEntries(entries, domain.LogFilter{
			Processes: []string{"web"},
		})
		require.NoError(t, err)
		assert.Len(t, result, 2)
	})

	t.Run("filter by pattern", func(t *testing.T) {
		result, err := FilterEntries(entries, domain.LogFilter{
			Pattern: "ERROR",
		})
		require.NoError(t, err)
		assert.Len(t, result, 2)
	})

	t.Run("combined filters", func(t *testing.T) {
		result, err := FilterEntries(entries, domain.LogFilter{
			Processes: []string{"web"},
			Pattern:   "ERROR",
		})
		require.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, "web", result[0].Process)
		assert.Contains(t, result[0].Line, "ERROR")
	})
}

func TestFilterEntriesLimit(t *testing.T) {
	entries := make([]domain.LogEntry, 100)
	for i := 0; i < 100; i++ {
		entries[i] = makeEntryWithProcess("web", "line")
	}

	t.Run("respects limit", func(t *testing.T) {
		result, total, err := FilterEntriesLimit(entries, domain.LogFilter{}, 10)
		require.NoError(t, err)
		assert.Len(t, result, 10)
		assert.Equal(t, 100, total)
	})

	t.Run("returns last entries", func(t *testing.T) {
		numbered := make([]domain.LogEntry, 10)
		for i := 0; i < 10; i++ {
			numbered[i] = makeEntryWithProcess("web", string(rune('0'+i)))
		}

		result, _, err := FilterEntriesLimit(numbered, domain.LogFilter{}, 3)
		require.NoError(t, err)
		assert.Equal(t, "7", result[0].Line)
		assert.Equal(t, "8", result[1].Line)
		assert.Equal(t, "9", result[2].Line)
	})
}
