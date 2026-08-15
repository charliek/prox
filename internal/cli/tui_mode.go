package cli

import (
	"fmt"
	"os"
	"strings"
)

// proxTUIEnvVar is the durable per-shell preference for whether `prox up`
// should open the TUI. It is deliberately NOT prefixed with an underscore like
// _PROX_DAEMON / _PROX_PROXY_DAEMON: those are private re-exec markers, this
// one is a documented user-facing knob.
const proxTUIEnvVar = "PROX_TUI"

// tuiMode is the resolved answer to "does this invocation want the TUI, and how
// badly?" — the output of resolveTUIMode.
//
// The three-way split exists because "asked for it" and "would like it" must
// fail differently when the terminal cannot host a full-screen UI:
//
//   - tuiModeRequired came from an explicit `--tui` on THIS command line. That
//     is an assertion, so an incapable terminal is an error the user must see.
//   - tuiModePreferred came from an environment variable, or from the default
//     a bare foreground `prox up` now carries (plan 026 C7). That is a standing
//     preference rather than a per-invocation assertion, so an incapable
//     terminal silently degrades to plain streaming — making it hard-error
//     would turn one `export PROX_TUI=1`, or the mere fact of a new default,
//     into a booby trap for every piped `prox up`.
//   - tuiModePlain is "stream logs to the terminal".
type tuiMode int

const (
	tuiModePlain tuiMode = iota
	tuiModePreferred
	tuiModeRequired
)

// String makes failing table-driven tests readable ("got plain, want preferred"
// rather than "got 0, want 1").
func (m tuiMode) String() string {
	switch m {
	case tuiModePlain:
		return "plain"
	case tuiModePreferred:
		return "preferred"
	case tuiModeRequired:
		return "required"
	default:
		return fmt.Sprintf("tuiMode(%d)", int(m))
	}
}

// tuiModeInputs is everything resolveTUIMode is allowed to look at. Every field
// is supplied by the caller — the resolver reads no globals, no environment and
// no terminal — so the whole precedence matrix is exercisable from a unit test.
//
// The flag pairs are split into "was it typed" and "what value did it parse to"
// on purpose. cobra's Flags().Changed("tui") is true for `--tui=false` as well
// as `--tui`, so a check that collapses the two breaks `prox up -d --tui=false`
// (valid today) while a check that uses only the value cannot tell an explicit
// `--tui=false` from an absent flag. The bug this guards against is a caller
// passing the resolved value where "was it typed" belongs, so both must be
// inputs the matrix can vary independently.
type tuiModeInputs struct {
	TUISet, TUIVal     bool // Flags().Changed("tui"),    the parsed value
	NoTUISet, NoTUIVal bool // Flags().Changed("no-tui"), the parsed value
	Detach             bool
	Env                string // raw PROX_TUI, "" if unset
	EnvPresent         bool
	// AutoDefault is what a bare `prox up` — no flag, no env — resolves to.
	// runUp passes true (plan 026 C7, the flip); the false rows stay in the
	// matrix because they are the semantics every other caller and every
	// pre-flip release had, and because "nothing asked for anything" is the row
	// most likely to be changed again by accident.
	AutoDefault bool
}

// tuiEnvTrue and tuiEnvFalse are the exact accepted PROX_TUI vocabularies,
// matched after trimming surrounding whitespace and lowercasing.
//
// Deliberately NOT strconv.ParseBool: that accepts "t"/"T"/"TRUE" and rejects
// "yes"/"on", which is neither this list nor a superset of it, and it is free to
// change with the Go release. Pinning the tokens here means the documented
// vocabulary is the implemented vocabulary forever.
var (
	tuiEnvFalse = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	tuiEnvTrue  = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
)

// resolveTUIMode decides whether this `prox up` invocation wants the TUI.
//
// Precedence: explicit flag > PROX_TUI > terminal capability (the caller's job,
// see terminalHostable) > AutoDefault.
//
// It is pure: no *cobra.Command, no os.Getenv, no isatty, no terminal probing.
// Capability is deliberately NOT folded in here — a flag conflict must report
// the conflict rather than the terminal, so the caller resolves the mode first
// and probes the terminal afterwards.
//
// Warnings are RETURNED rather than printed. A warning written to stderr before
// bubbletea enters the alt screen is invisible for the entire session; plan 026
// C4 routes these through the same TUI-visible path as the startup preamble.
func resolveTUIMode(in tuiModeInputs) (tuiMode, []string, error) {
	// --- Conflicts first, before any short-circuit. -------------------------
	//
	// The predicate is `Set && Val`, never a bare Changed(): cobra reports
	// Changed for `--tui=false` too, so a bare check would reject the valid
	// `prox up -d --tui=false` and `prox up --tui=false --no-tui`, which are
	// both requests for the SAME thing (no TUI) and cannot be contradictions.
	tuiAsserted := in.TUISet && in.TUIVal
	if tuiAsserted && in.NoTUISet && in.NoTUIVal {
		return tuiModePlain, nil, fmt.Errorf("--tui and --no-tui are mutually exclusive")
	}
	// Message retained verbatim from the pre-plan-026 check in runUp.
	if tuiAsserted && in.Detach {
		return tuiModePlain, nil, fmt.Errorf("--tui and --detach are mutually exclusive")
	}

	// --- Detach short-circuits everything, INCLUDING env parsing. -----------
	//
	// A daemon has no terminal by construction, so there is nothing to decide.
	// Skipping the env parse here is what stops a `-d` parent and the child it
	// re-execs from emitting the invalid-PROX_TUI warning twice.
	if in.Detach {
		return tuiModePlain, nil, nil
	}

	// --- Explicit flags win outright; the env is never consulted. -----------
	if in.NoTUISet && in.NoTUIVal {
		return tuiModePlain, nil, nil
	}
	if in.TUISet {
		if in.TUIVal {
			return tuiModeRequired, nil, nil
		}
		// `--tui=false` is an explicit negative assertion, not the absence of
		// an opinion: it must beat PROX_TUI=1 and the auto-default alike.
		return tuiModePlain, nil, nil
	}
	// `--no-tui=false` asserts nothing (it is the flag's own default spelled
	// out loud), so it falls through to the env and the default below.

	// --- PROX_TUI. ----------------------------------------------------------
	var warnings []string
	if in.EnvPresent {
		switch v := strings.ToLower(strings.TrimSpace(in.Env)); {
		case tuiEnvFalse[v]:
			return tuiModePlain, nil, nil
		case tuiEnvTrue[v]:
			// preferred, not required: see the tuiMode doc comment.
			return tuiModePreferred, nil, nil
		default:
			// Including the empty string. `PROX_TUI=` is far more likely to be a
			// mistake than a deliberate "no opinion", and an ignored setting the
			// user believes is in force is exactly the thing worth saying out loud.
			warnings = append(warnings, fmt.Sprintf(
				"Warning: ignoring %s=%q: expected one of 0, 1, false, true, no, yes, off, on",
				proxTUIEnvVar, in.Env))
		}
	}

	// --- Nothing asked for anything. ----------------------------------------
	if in.AutoDefault {
		return tuiModePreferred, warnings, nil
	}
	return tuiModePlain, warnings, nil
}

// terminalHostable reports whether this process's terminal can actually host a
// full-screen TUI. It returns nil when it can, and otherwise the error a
// tuiModeRequired session should fail with — naming the single condition that
// failed, so the user is told which thing to fix rather than being handed a
// generic refusal.
//
// Three conditions, all required:
//
//  1. stdin AND stdout are terminals. A TUI needs a keyboard and a screen.
//  2. TERM is set and is not "dumb". isatty happily says yes on a pty where
//     bubbletea has no capabilities to draw with.
//  3. stdin's foreground process group is ours. `prox up &` passes (1) and (2)
//     — both fds are still the terminal — and then the TUI's first read from
//     stdin makes the kernel raise SIGTTIN and the job stops. Today `prox up &`
//     streams logs and works; without this condition making the TUI the default
//     would convert a normal workflow into a wedged job.
//
// Precedence when several fail is most-fundamental-first: not-a-terminal >
// TERM > background job. The condition-1 string is retained verbatim from the
// pre-plan-026 guard — test/integration/up_test.go and tui_pty_test.go assert
// on it.
func terminalHostable() error {
	if !isInteractiveStdio() {
		return fmt.Errorf("--tui requires an interactive terminal")
	}
	term := os.Getenv("TERM")
	if term == "" {
		return fmt.Errorf("--tui requires a capable terminal: TERM is not set")
	}
	if term == "dumb" {
		return fmt.Errorf("--tui requires a capable terminal: TERM=dumb cannot render a full-screen UI")
	}
	if !stdinInForeground() {
		return fmt.Errorf("--tui requires a foreground job: this process is not in the terminal's foreground process group (a backgrounded `prox up &` would stop on SIGTTIN); run it in the foreground, or use `prox up -d`")
	}
	return nil
}
