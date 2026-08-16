package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// recordingLog stands in for supervisor.SystemLog where the test only cares
// about what was handed to it, in what order.
type recordingLog struct {
	lines []string
}

func (r *recordingLog) logf(format string, args ...interface{}) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func TestStartupPreamble(t *testing.T) {
	t.Run("prints and collects when the TUI will host the session", func(t *testing.T) {
		pre := newStartupPreamble(true)

		stdout, stderr := captureOutput(t, func() {
			pre.printf("Proxy (shared daemon): %s", "https://*.test:443")
			pre.printf("Starting processes: %s", "web, api")
		})

		assert.Equal(t, "Proxy (shared daemon): https://*.test:443\nStarting processes: web, api\n", stdout)
		assert.Empty(t, stderr)
		assert.Equal(t, []string{
			"Proxy (shared daemon): https://*.test:443",
			"Starting processes: web, api",
		}, pre.Lines())
	})

	// Plain `prox up`, `-d` and CI must see byte-identical output to the bare
	// fmt.Printf calls printf replaced, and must pay for nothing else: in plain
	// mode the log subscriber prints every system entry to this same terminal,
	// so recording would be a duplicate as well as a waste.
	t.Run("plain mode prints the same bytes and collects nothing", func(t *testing.T) {
		pre := newStartupPreamble(false)
		rec := &recordingLog{}
		pre.logTo(rec.logf)

		stdout, stderr := captureOutput(t, func() {
			pre.printf("Starting prox with config: %s", "prox.yaml")
			pre.note("Warning: ignored")
		})

		want, _ := captureOutput(t, func() {
			fmt.Printf("Starting prox with config: %s\n", "prox.yaml")
		})
		assert.Equal(t, want, stdout)
		assert.Empty(t, stderr)
		assert.Empty(t, pre.Lines())
		assert.Empty(t, rec.lines)
	})

	t.Run("note records without touching the terminal", func(t *testing.T) {
		pre := newStartupPreamble(true)

		stdout, stderr := captureOutput(t, func() {
			pre.note("Warning: ignoring PROX_TUI=%q", "maybe")
		})

		assert.Empty(t, stdout, "a line written before the alt screen opens is a line nobody sees")
		assert.Empty(t, stderr)
		assert.Equal(t, []string{`Warning: ignoring PROX_TUI="maybe"`}, pre.Lines())
	})

	t.Run("lines recorded before the supervisor existed are flushed on attach", func(t *testing.T) {
		pre := newStartupPreamble(true)
		rec := &recordingLog{}

		// The mode resolver's warnings land here, long before there is a
		// supervisor to log through.
		pre.note("Warning: ignoring PROX_TUI=%q", "maybe")
		require.Empty(t, rec.lines)

		pre.logTo(rec.logf)
		_, _ = captureOutput(t, func() {
			pre.printf("API server: http://%s (local only, no auth)", "127.0.0.1:7777")
		})

		assert.Equal(t, []string{
			`Warning: ignoring PROX_TUI="maybe"`,
			"API server: http://127.0.0.1:7777 (local only, no auth)",
		}, rec.lines, "every line, in order, exactly once")
		assert.Equal(t, rec.lines, pre.Lines(), "both paths carry the same lines")
	})

	t.Run("an already-formatted line is not re-interpreted as a format", func(t *testing.T) {
		pre := newStartupPreamble(true)
		rec := &recordingLog{}
		pre.logTo(rec.logf)

		_, _ = captureOutput(t, func() {
			pre.printf("Starting prox with config: %s", "/tmp/100%discount/prox.yaml")
		})

		assert.Equal(t, []string{"Starting prox with config: /tmp/100%discount/prox.yaml"}, rec.lines)
	})

	// The crux of plan 026 C4: the supervisor's log ring is SHARED with process
	// output, so a chatty startup evicts the preamble before the TUI can ever
	// back it up. Everything the TUI is given through ClientOptions survives
	// that; everything it would have had to read back from the ring does not.
	t.Run("survives a ring flooded past capacity before the TUI connects", func(t *testing.T) {
		const bufferSize = 1000
		logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: bufferSize, SubscriptionBuffer: 1000})
		defer logMgr.Close()
		sup := supervisor.New(&config.Config{}, logMgr, nil, supervisor.DefaultSupervisorConfig())

		pre := newStartupPreamble(true)
		pre.logTo(sup.SystemLog)
		_, _ = captureOutput(t, func() {
			pre.printf("Proxy (shared daemon): %s", "https://*.test:443")
		})

		// A webpack build, a migration — enough output to roll the ring.
		for i := 0; i < bufferSize; i++ {
			logMgr.Write(domain.LogEntry{Process: "web", Stream: domain.StreamStdout, Line: fmt.Sprintf("build step %d", i)})
		}

		buffered, _, err := logMgr.Query(domain.LogFilter{}, 0)
		require.NoError(t, err)
		for _, e := range buffered {
			require.NotContains(t, e.Line, "Proxy (shared daemon)",
				"precondition: the ring must have evicted the system-log copy")
		}

		assert.Equal(t, []string{"Proxy (shared daemon): https://*.test:443"}, pre.Lines(),
			"the ClientOptions copy is what makes the proxy URL a guarantee rather than a hope")
	})
}

func TestReportTUIWarnings(t *testing.T) {
	// The real thing: an unrecognized PROX_TUI value, warned about by the
	// resolver rather than printed by it (tui_mode.go).
	mode, warnings, err := resolveTUIMode(tuiModeInputs{Env: "maybe", EnvPresent: true})
	require.NoError(t, err)
	require.Equal(t, tuiModePlain, mode)
	require.Len(t, warnings, 1)

	t.Run("a TUI session gets it in the preamble AND on stderr", func(t *testing.T) {
		pre := newStartupPreamble(true)
		var stderr bytes.Buffer

		reportTUIWarnings(warnings, pre, true, &stderr)

		// The preamble copy is what the user sees DURING the session. The stderr
		// copy is the safety net: if startup fails before the TUI ever opens (bad
		// config, port bind, proxy or supervisor start), the preamble is never
		// rendered and the log manager is discarded, so a preamble-only warning
		// would be lost entirely (codex review finding).
		assert.Equal(t, warnings, pre.Lines())
		assert.Contains(t, stderr.String(), warnings[0])
	})

	t.Run("plain mode keeps the stderr line exactly", func(t *testing.T) {
		pre := newStartupPreamble(false)
		var stderr bytes.Buffer

		reportTUIWarnings(warnings, pre, false, &stderr)

		assert.Equal(t, warnings[0]+"\n", stderr.String())
		assert.Empty(t, pre.Lines())
	})
}
