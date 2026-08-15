package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownerClientOptions mirrors what `prox up` passes RunClient (internal/cli/up.go),
// exactly as attachClientOptions mirrors runAttach. The two live side by side
// because the asymmetry IS the feature: one TUI, one `q` key, opposite
// consequences (plan 026 §3.2).
func ownerClientOptions() ClientOptions {
	return ClientOptions{
		Help: HelpConfig{
			TitleSuffix: "",
			QuitMessage: "Quit (stops processes)",
			QuitNote:    "To keep processes running, start with 'prox up -d' and use 'prox attach'",
			QuitHint:    "stop",
		},
	}
}

func newOwnerModel() ClientModel {
	return NewClientModel(&stubTUIClient{}, ownerClientOptions())
}

// openHelpPlain drives the real entry path (a '?' keypress) and returns the
// rendered frame with styling stripped.
func openHelpPlain(t *testing.T, m ClientModel) string {
	t.Helper()
	m = clientUpdate(m, tea.WindowSizeMsg{Width: 200, Height: 80})
	m = clientUpdate(m, keyRune('?'))
	require.Equal(t, ModeHelp, m.mode)
	return ansi.Strip(m.View())
}

// helpSectionsText flattens every key/desc pair the current view's help lists.
func helpSectionsText(b *BaseModel) string {
	var sb strings.Builder
	for _, sec := range b.helpSections() {
		sb.WriteString(sec.title)
		sb.WriteByte('\n')
		for _, row := range sec.rows {
			sb.WriteString(row.key)
			sb.WriteByte('\t')
			sb.WriteString(row.desc)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func TestQuitLabel_FooterHintStrip(t *testing.T) {
	t.Run("owner mode reads q stop", func(t *testing.T) {
		m := newOwnerModel()
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 24})
		plain := ansi.Strip(m.statusBar(m.resolveFooterMsg()))
		assert.Contains(t, plain, "q stop")
		assert.NotContains(t, plain, "q quit")
		// Everything else about the strip is unchanged.
		assert.Contains(t, plain, "? help")
		assert.Contains(t, plain, "m menu")
	})

	t.Run("attach mode still reads q quit", func(t *testing.T) {
		m := newTestModel() // attachClientOptions
		m = clientUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 24})
		plain := ansi.Strip(m.statusBar(m.resolveFooterMsg()))
		assert.Contains(t, plain, "q quit")
		assert.NotContains(t, plain, "q stop")
	})

	t.Run("empty QuitHint falls back to quit", func(t *testing.T) {
		hints := defaultFooterHints("")
		require.Len(t, hints, 5)
		assert.Equal(t, " quit", hints[4].label)
	})
}

func TestQuitLabel_HelpModal(t *testing.T) {
	t.Run("owner mode names -d and attach", func(t *testing.T) {
		help := openHelpPlain(t, newOwnerModel())
		assert.Contains(t, help, "Quit (stops processes)")
		assert.Contains(t, help, "prox up -d")
		assert.Contains(t, help, "prox attach")
		assert.NotContains(t, help, "daemon continues running")
	})

	t.Run("attach mode help is unchanged", func(t *testing.T) {
		help := openHelpPlain(t, newTestModel())
		assert.Contains(t, help, "Quit (daemon continues running)")
		assert.NotContains(t, help, "stops processes")
		assert.NotContains(t, help, "prox up -d")
		assert.NotContains(t, help, "prox attach")
	})
}

// TestQuitLabel_HelpSectionsEveryView pins the note onto every view's help, not
// just the logs view a user happens to open it from.
func TestQuitLabel_HelpSectionsEveryView(t *testing.T) {
	views := []struct {
		name string
		mode ViewMode
	}{
		{"logs", ViewModeLogs},
		{"requests", ViewModeRequests},
		{"detail", ViewModeRequestDetail},
	}
	for _, v := range views {
		t.Run(v.name, func(t *testing.T) {
			owner := newOwnerModel()
			owner.viewMode = v.mode
			text := helpSectionsText(&owner.BaseModel)
			assert.Contains(t, text, "q/Ctrl+C\tQuit (stops processes)")
			assert.Contains(t, text, "prox up -d")

			attach := newTestModel()
			attach.viewMode = v.mode
			attachText := helpSectionsText(&attach.BaseModel)
			assert.Contains(t, attachText, "q/Ctrl+C\tQuit (daemon continues running)")
			assert.NotContains(t, attachText, "prox up -d")
		})
	}
}

// TestQuitLabel_FooterDegradationOwnerMode re-runs the B2 narrow-width ladder
// with the owner label: `stop` is a character shorter than `quit`, so the
// drop ordering and sticky flags are re-asserted against the shorter strip.
func TestQuitLabel_FooterDegradationOwnerMode(t *testing.T) {
	t.Run("drop order and sticky pair", func(t *testing.T) {
		hints := defaultFooterHints("stop")
		require.Len(t, hints, 5)
		assert.Equal(t, " stop", hints[4].label)

		hints = dropFooterHint(hints) // s filter
		for _, h := range hints {
			assert.NotEqual(t, "s", h.key)
		}
		hints = dropFooterHint(hints) // / search
		for _, h := range hints {
			assert.NotEqual(t, "/", h.key)
		}
		hints = dropFooterHint(hints) // m menu
		require.Len(t, hints, 2)
		assert.Equal(t, "?", hints[0].key)
		assert.Equal(t, "q", hints[1].key)
		assert.Equal(t, " stop", hints[1].label)

		hints = dropFooterHint(hints) // rightmost sticky: q
		require.Len(t, hints, 1)
		assert.Equal(t, "?", hints[0].key)
	})

	for _, w := range []int{20, 40, 80, 120} {
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			m := newOwnerModel()
			m = clientUpdate(m, tea.WindowSizeMsg{Width: w, Height: 24})
			m.statusFlash = footerInfo("status ok")
			bar := m.statusBar(m.resolveFooterMsg())
			assert.LessOrEqual(t, ansi.StringWidth(bar), w,
				"footer must fit width %d (got %d)", w, ansi.StringWidth(bar))
			plain := ansi.Strip(bar)
			if w >= 80 {
				assert.Contains(t, plain, "? help")
				assert.Contains(t, plain, "q stop")
			}
			if w >= 120 {
				assert.Contains(t, plain, "m menu")
				assert.Contains(t, plain, "/ search")
				assert.Contains(t, plain, "s filter")
			}
		})
	}
}
