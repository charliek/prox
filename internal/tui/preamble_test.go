package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
)

// testPreamble is the shape `prox up` hands over: the lines a session prints
// about itself before the alt screen hides the terminal they were printed on.
var testPreamble = []string{
	"Starting prox with config: prox.yaml",
	"API server: http://127.0.0.1:7777 (local only, auth enabled)",
	"Proxy (shared daemon): https://*.test:443",
}

// ownerPreambleModel builds the owner-mode (`prox up --tui`) model: attach's
// options plus a preamble, which is the one ClientOptions field only the
// supervising caller sets.
func ownerPreambleModel(preamble []string) ClientModel {
	opts := attachClientOptions()
	opts.Preamble = preamble
	return NewClientModel(&stubTUIClient{}, opts)
}

// floodEntries builds n process log lines — the chatty startup (a webpack
// build, a migration) that evicts the preamble from every ring it is allowed to
// share with.
func floodEntries(n int) []domain.LogEntry {
	entries := make([]domain.LogEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, domain.LogEntry{
			Timestamp: time.Now(),
			Process:   "web",
			Stream:    domain.StreamStdout,
			Line:      fmt.Sprintf("build step %d", i),
			Seq:       uint64(i + 1),
		})
	}
	return entries
}

func countLines(entries []domain.LogEntry, line string) int {
	n := 0
	for _, e := range entries {
		if e.Line == line {
			n++
		}
	}
	return n
}

func TestClientOptionsPreamble(t *testing.T) {
	t.Run("renders on the first frame", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble)
		require.Len(t, m.logEntries, len(testPreamble))

		view := clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40}).View()
		for _, line := range testPreamble {
			assert.Contains(t, view, line)
		}
	})

	t.Run("renders as session info, not as process output or an error", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble[:1])
		require.Len(t, m.logEntries, 1)

		entry := m.logEntries[0]
		// The neutral "system" column is what separates these from a supervised
		// process's output; stdout is what keeps the ERR badge off a line that
		// is not an error. Both match what supervisor.SystemLog writes for the
		// same line, so the two delivery paths render identically.
		assert.Equal(t, systemProcessName, entry.Process)
		assert.Equal(t, domain.StreamStdout, entry.Stream)
		assert.NotContains(t, m.formatLogEntry(entry), "ERR")
	})

	// The reason ClientOptions.Preamble exists at all: the log ring the
	// SystemLog copy travels through is SHARED with process output, so a chatty
	// startup can evict every preamble line before the TUI connects — and the
	// TUI's own ring would then evict them a second time on backfill. This test
	// fails outright if the preamble is only a log entry like any other.
	t.Run("survives a backfill that floods past the ring capacity", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble)

		m = clientUpdate(m, LogsSyncMsg{Entries: floodEntries(maxLogEntries + 200)})

		require.Len(t, m.logEntries, len(testPreamble)+maxLogEntries,
			"the live stream is capped; the pinned preamble is kept on top of the cap")
		for i, line := range testPreamble {
			assert.Equal(t, line, m.logEntries[i].Line, "preamble must stay at the head")
		}

		view := clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40}).View()
		assert.Contains(t, view, "build step "+fmt.Sprint(maxLogEntries+199), "newest live line still shown")
	})

	t.Run("the log stream's own copy is not shown twice", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble)

		// What the first backfill hands back when the ring did NOT overflow:
		// the supervisor's SystemLog copy of the very same lines.
		echoes := make([]domain.LogEntry, 0, len(testPreamble))
		for _, line := range testPreamble {
			echoes = append(echoes, domain.LogEntry{
				Timestamp: time.Now(),
				Process:   systemProcessName,
				Stream:    domain.StreamStdout,
				Line:      line,
			})
		}
		m = clientUpdate(m, LogsSyncMsg{Entries: echoes})

		require.Len(t, m.logEntries, len(testPreamble))
		for _, line := range testPreamble {
			assert.Equal(t, 1, countLines(m.logEntries, line))
		}
	})

	t.Run("only one echo per pinned line is swallowed", func(t *testing.T) {
		line := testPreamble[0]
		m := ownerPreambleModel([]string{line})

		echo := domain.LogEntry{Process: systemProcessName, Stream: domain.StreamStdout, Line: line}
		m = clientUpdate(m, LogEntryMsg(echo))
		require.Equal(t, 1, countLines(m.logEntries, line))

		// A genuinely repeated system line later in the session is real output
		// and must show up.
		m = clientUpdate(m, LogEntryMsg(echo))
		assert.Equal(t, 2, countLines(m.logEntries, line))
	})

	t.Run("a warning is never mistaken for an echo", func(t *testing.T) {
		line := testPreamble[0]
		m := ownerPreambleModel([]string{line})

		// Same text, stderr stream: a synthetic TUI warning (systemLogEntry),
		// not the SystemLog copy of a preamble line.
		m = clientUpdate(m, LogEntryMsg(systemLogEntry(line)))
		assert.Equal(t, 2, countLines(m.logEntries, line))
	})

	t.Run("process output matching a preamble line is never swallowed", func(t *testing.T) {
		line := testPreamble[0]
		m := ownerPreambleModel([]string{line})

		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Process: "web", Stream: domain.StreamStdout, Line: line,
		}))
		assert.Equal(t, 2, countLines(m.logEntries, line))
	})

	t.Run("echo credits do not outlive the first backfill", func(t *testing.T) {
		// "system" is a LEGAL process name — config.ValidateProcessName rejects
		// only empty and whitespace/separator names. So a credit left standing
		// after the backfill window could later swallow the real output of a
		// process actually called `system` that happens to print the same text.
		// The server's own copies can only ever arrive in a backfill, so credits
		// retire the moment the first one lands (codex review finding).
		line := testPreamble[0]
		m := ownerPreambleModel([]string{line})

		// A backfill that does NOT contain the preamble copy — the server ring
		// evicted it during a chatty startup. The credit must still expire.
		m = clientUpdate(m, LogsSyncMsg{Entries: floodEntries(3)})
		require.Equal(t, 1, countLines(m.logEntries, line))

		// A process genuinely named `system` now prints that exact text.
		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Process: systemProcessName, Stream: domain.StreamStdout, Line: line,
		}))
		assert.Equal(t, 2, countLines(m.logEntries, line),
			"a stale echo credit swallowed real output from a process named 'system'")
	})

	t.Run("echo credits do not survive an EMPTY first sync", func(t *testing.T) {
		// handleLogsSync returns early when a sync carries neither a notice nor
		// entries (a caught-up reconnect must not force a render). That early
		// return skipped the retire, so a first sync that came back empty left
		// credits armed indefinitely — the same swallowing hazard, reached by a
		// different door (CodeRabbit, PR #106).
		line := testPreamble[0]
		m := ownerPreambleModel([]string{line})

		m = clientUpdate(m, LogsSyncMsg{})
		require.Equal(t, 1, countLines(m.logEntries, line))

		m = clientUpdate(m, LogEntryMsg(domain.LogEntry{
			Process: systemProcessName, Stream: domain.StreamStdout, Line: line,
		}))
		assert.Equal(t, 2, countLines(m.logEntries, line),
			"an empty sync left an echo credit armed, and it swallowed real output")
	})

	t.Run("a resolver warning reaches the TUI", func(t *testing.T) {
		// The invalid-PROX_TUI warning rides the same path: the CLI records it
		// into the preamble instead of writing it to a primary screen the alt
		// screen is about to hide.
		warning := `Warning: ignoring PROX_TUI="maybe": expected one of 0, 1, false, true, no, yes, off, on`
		m := ownerPreambleModel([]string{warning})

		view := clientUpdate(m, tea.WindowSizeMsg{Width: 160, Height: 40}).View()
		assert.Contains(t, view, "ignoring PROX_TUI")
	})

	t.Run("attach mode pins nothing and evicts as before", func(t *testing.T) {
		m := newTestModel()
		require.Empty(t, m.logEntries)

		m = clientUpdate(m, LogsSyncMsg{Entries: floodEntries(maxLogEntries + 200)})
		assert.Len(t, m.logEntries, maxLogEntries)
		assert.Equal(t, "build step 200", m.logEntries[0].Line)
	})

	t.Run("an oversized preamble cannot crowd out the live stream", func(t *testing.T) {
		lines := make([]string, maxPinnedLogEntries+10)
		for i := range lines {
			lines[i] = fmt.Sprintf("preamble line %d", i)
		}
		m := ownerPreambleModel(lines)

		assert.Len(t, m.logEntries, maxPinnedLogEntries)
		assert.Equal(t, maxPinnedLogEntries, m.pinnedLogEntries)
	})

	t.Run("evicted live entries release their render metadata", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble)
		m = clientUpdate(m, LogsSyncMsg{Entries: floodEntries(maxLogEntries + 200)})

		// One meta record per surviving entry — the pinned head included, the
		// evicted 200 excluded.
		assert.Len(t, m.logMeta, len(m.logEntries))
		for _, e := range m.logEntries {
			_, ok := m.logMeta[e.DisplaySeq]
			assert.True(t, ok, "surviving entry %q lost its meta", e.Line)
		}
	})

	t.Run("filters still apply to the preamble", func(t *testing.T) {
		m := ownerPreambleModel(testPreamble)
		m = clientUpdate(m, LogsSyncMsg{Entries: floodEntries(1)})
		m.soloProcess = "web"

		view := clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40}).View()
		require.Contains(t, view, "build step 0", "the solo'd process is still shown")
		assert.False(t, strings.Contains(view, "Starting prox with config"),
			"pinning exempts the preamble from eviction, not from the user's own filter")
	})
}
