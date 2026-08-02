package tui

import (
	"strings"
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
		{"critical", LogLevelError, true},
		{"CRITICAL", LogLevelError, true},
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
		{"json severity (gcp/python)", `{"severity":"WARNING","message":"x"}`, LogLevelWarn, true},
		{"json severity critical", `{"severity":"CRITICAL","message":"x"}`, LogLevelError, true},
		{"json pino numeric info", `{"level":30,"msg":"x"}`, LogLevelInfo, true},
		{"json pino numeric error", `{"level":50,"msg":"x"}`, LogLevelError, true},
		{"json numeric out of range", `{"level":123}`, LogLevelUnknown, false},
		{"json logfmt in value rejected", `{"msg":"level=info x"}`, LogLevelUnknown, false},
		{"json no level, no heuristic", `{"msg":"hi"}`, LogLevelUnknown, false},
		{"logfmt info", "level=INFO msg=x", LogLevelInfo, true},
		{"logfmt fatal", "level=fatal", LogLevelError, true},
		{"plain text", "something happened", LogLevelUnknown, false},
		{"mid-line logfmt", "retry level=info weird", LogLevelInfo, true},
		{"uppercase logfmt", "LEVEL=ERROR", LogLevelError, true},
		{"unsupported level name", "level=notice", LogLevelUnknown, false},
		{"empty", "", LogLevelUnknown, false},

		// Bare-token path: real dev-format bytes per stack.
		{"python dev info", "2026-08-01 11:39:29,582 INFO demo.app: server starting", LogLevelInfo, true},
		{"python dev warning", "2026-08-01 11:39:29,582 WARNING demo.app: careful now", LogLevelWarn, true},
		{"uvicorn access", `INFO:     127.0.0.1:51234 - "GET / HTTP/1.1" 200 OK`, LogLevelInfo, true},
		{"rust tracing text", "2026-08-01T16:14:05.604587Z  INFO tower_http::trace::on_request: started processing request", LogLevelInfo, true},
		{"rust tracing ansi", "2026-08-01T16:14:05.604587Z \x1b[32m INFO\x1b[0m tower_http::trace: x", LogLevelInfo, true},
		{"rust tracing warn ansi", "2026-08-01T16:14:05.604587Z \x1b[33m WARN\x1b[0m sqlx::query: slow", LogLevelWarn, true},
		{"pino-pretty ansi", "[16:39:31] \x1b[32mINFO\x1b[39m: \x1b[36mserver starting\x1b[39m", LogLevelInfo, true},
		{"pino-pretty error", "[16:39:31] \x1b[31mERROR\x1b[39m: \x1b[36mfailed\x1b[39m", LogLevelError, true},
		{"bracketed token", "[INFO] starting", LogLevelInfo, true},
		{"critical bare", "2026-08-01 11:39:29,582 CRITICAL demo.app: boom", LogLevelError, true},

		// False-positive guards.
		{"lowercase prose", "error handling started ok", LogLevelUnknown, false},
		{"lowercase bare info", "2026-08-01 info lowercase token", LogLevelUnknown, false},
		{"token beyond scan limit", strings.Repeat("x", 90) + " INFO y", LogLevelUnknown, false},
		{"token prefix of word", "INFORMATIONAL notice", LogLevelUnknown, false},
		{"token in url path", "GET /INFO/health HTTP/1.1", LogLevelUnknown, false},
		{"sqlx continuation", `    err: {`, LogLevelUnknown, false},
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

func TestIngestLogMeta_JSONObject(t *testing.T) {
	t.Parallel()
	assert.True(t, ingestLogMeta(`{"level":"error"}`).isJSON)
	assert.False(t, ingestLogMeta("not json").isJSON)
	assert.False(t, ingestLogMeta(`[1,2,3]`).isJSON)
	assert.False(t, ingestLogMeta(`{"broken"`).isJSON)
}

func TestLogMeta_AppendLogEntry(t *testing.T) {
	m := newTestBaseModel()
	entry := domain.LogEntry{Line: `{"level":"error","msg":"x"}`}
	m.appendLogEntry(entry)

	require.Len(t, m.logEntries, 1)
	seq := m.logEntries[0].DisplaySeq
	meta, ok := m.logMeta[seq]
	require.True(t, ok)
	assert.True(t, meta.hasLevel)
	assert.Equal(t, LogLevelError, meta.level)
	assert.True(t, meta.isJSON)
	require.NotEmpty(t, meta.pairs, "ingest caches JSON pairs (C14)")
	paths := make([]string, len(meta.pairs))
	for i, p := range meta.pairs {
		paths[i] = p.path
	}
	assert.Contains(t, paths, "level")
	assert.Contains(t, paths, "msg")
}

func TestLogMeta_AppendLogEntry_PlainLine(t *testing.T) {
	m := newTestBaseModel()
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
	m := newTestBaseModel()
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
	m := newTestBaseModel()
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

// classifyLevel is the test-only wrapper matching the old classifier signature.
func classifyLevel(raw string) (LogLevel, bool) {
	meta := ingestLogMeta(raw)
	return meta.level, meta.hasLevel
}
