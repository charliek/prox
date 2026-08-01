package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

func TestNormalizeLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		level LogLevel
		found bool
	}{
		{"warning", LogLevelWarn, true},
		{"WARNING", LogLevelWarn, true},
		{"fatal", LogLevelError, true},
		{"FATAL", LogLevelError, true},
		{"notice", LogLevelUnknown, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			level, found := normalizeLevel(tc.in)
			assert.Equal(t, tc.level, level)
			assert.Equal(t, tc.found, found)
		})
	}
}

func TestClassifyLevel_Corpus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		line  string
		level LogLevel
		found bool
	}{
		{"json level error", `{"level":"error","msg":"x"}`, LogLevelError, true},
		{"json lvl warning", `{"lvl":"warning"}`, LogLevelWarn, true},
		{"logfmt info", "level=INFO msg=x", LogLevelInfo, true},
		{"logfmt fatal", "level=fatal", LogLevelError, true},
		{"plain text", "something happened", LogLevelUnknown, false},
		{"mid-line logfmt", "retry level=info weird", LogLevelInfo, true},
		{"json non-string level", `{"level":123}`, LogLevelUnknown, false},
		{"uppercase logfmt", "LEVEL=ERROR", LogLevelError, true},
		{"unsupported level name", "level=notice", LogLevelUnknown, false},
		{"empty", "", LogLevelUnknown, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			level, found := classifyLevel(tc.line)
			assert.Equal(t, tc.level, level, "level")
			assert.Equal(t, tc.found, found, "found")
		})
	}
}

func TestIsJSONObject(t *testing.T) {
	t.Parallel()
	assert.True(t, isJSONObject(`{"level":"error"}`))
	assert.False(t, isJSONObject("not json"))
	assert.False(t, isJSONObject(`[1,2,3]`))
	assert.False(t, isJSONObject(`{"broken"`))
}

func TestLogMeta_AppendLogEntry(t *testing.T) {
	m := &BaseModel{}
	entry := domain.LogEntry{Line: `{"level":"error","msg":"x"}`}
	m.appendLogEntry(entry)

	require.Len(t, m.logEntries, 1)
	seq := m.logEntries[0].DisplaySeq
	meta, ok := m.logMeta[seq]
	require.True(t, ok)
	assert.True(t, meta.hasLevel)
	assert.Equal(t, LogLevelError, meta.level)
	assert.True(t, meta.isJSON)
}

func TestLogMeta_AppendLogEntry_PlainLine(t *testing.T) {
	m := &BaseModel{}
	m.appendLogEntry(domain.LogEntry{Line: "something happened"})

	seq := m.logEntries[0].DisplaySeq
	meta := m.logMeta[seq]
	assert.False(t, meta.hasLevel)
	assert.Equal(t, LogLevelUnknown, meta.level)
	assert.False(t, meta.isJSON)
}

func logMetaKeys(m *BaseModel) map[int64]struct{} {
	keys := make(map[int64]struct{}, len(m.logMeta))
	for k := range m.logMeta {
		keys[k] = struct{}{}
	}
	return keys
}

func survivingDisplaySeqs(entries []domain.LogEntry) map[int64]struct{} {
	keys := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		keys[e.DisplaySeq] = struct{}{}
	}
	return keys
}

func TestLogMeta_EvictionOneAtATime(t *testing.T) {
	m := &BaseModel{}
	for i := 0; i < maxLogEntries+50; i++ {
		m.appendLogEntry(domain.LogEntry{
			Timestamp: time.Now(),
			Line:      "level=info msg",
		})
	}
	require.Len(t, m.logEntries, maxLogEntries)
	assert.Equal(t, survivingDisplaySeqs(m.logEntries), logMetaKeys(m))
}

func TestLogMeta_EvictionSyncBatch(t *testing.T) {
	m := &BaseModel{}
	entries := make([]domain.LogEntry, maxLogEntries+50)
	for i := range entries {
		entries[i] = domain.LogEntry{
			Timestamp: time.Now(),
			Line:      `{"level":"debug"}`,
		}
	}
	for _, e := range entries {
		m.appendLogEntry(e)
	}
	require.Len(t, m.logEntries, maxLogEntries)
	assert.Equal(t, survivingDisplaySeqs(m.logEntries), logMetaKeys(m))

	// Spot-check a surviving entry still has correct meta.
	last := m.logEntries[len(m.logEntries)-1]
	meta := m.logMeta[last.DisplaySeq]
	assert.True(t, meta.hasLevel)
	assert.Equal(t, LogLevelDebug, meta.level)
	assert.True(t, meta.isJSON)
}
