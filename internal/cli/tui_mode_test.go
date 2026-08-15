package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveTUIMode_Matrix pins every row of plan 026 §3.1's resolution matrix.
//
// Each row is asserted for BOTH AutoDefault values. AutoDefault=false is what
// C3 wires in (today's semantics, unchanged); AutoDefault=true is what C7 flips
// on. Proving the true case here means the flip is a one-line change to an
// already-tested function rather than a change whose correctness is unknown
// until it ships. Only ONE row is allowed to differ between the two — the
// nothing-set row at the bottom of the precedence chain; every row above it is
// decided by an explicit flag or by PROX_TUI and must be identical.
func TestResolveTUIMode_Matrix(t *testing.T) {
	tests := []struct {
		name string
		in   tuiModeInputs
		// want is the result under AutoDefault=false; wantAuto under
		// AutoDefault=true. They are equal for every row except the one where
		// nothing at all was asked for.
		want     tuiMode
		wantAuto tuiMode
		// wantErr, when non-empty, is a substring the error must contain; the
		// mode is not asserted in that case.
		wantErr string
		// wantWarning, when non-empty, is a substring the single returned
		// warning must contain. Empty means "no warnings at all".
		wantWarning string
	}{
		// --- detach short-circuits everything --------------------------------
		{
			name: "detach alone: plain, no env parse, no warning",
			in:   tuiModeInputs{Detach: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "detach + garbage env: still silent (the -d parent and its re-exec'd child must not double-warn)",
			in:   tuiModeInputs{Detach: true, Env: "banana", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "detach + PROX_TUI=1: plain, env never consulted",
			in:   tuiModeInputs{Detach: true, Env: "1", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--tui=false + --detach: valid today, stays valid",
			in:   tuiModeInputs{TUISet: true, TUIVal: false, Detach: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--no-tui + --detach: no conflict",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: true, Detach: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			// THE regression row for plan 026's highest risk (codex review of
			// C7). TUIVal without TUISet is what a caller passing the RESOLVED
			// value — or a raw `useTUI` read that never asks Changed() — looks
			// like from in here. With the flip on, that value is true for every
			// `prox up -d` typed at a normal terminal, so a conflict predicate
			// reading it would reject essentially every detached start. The
			// pty test (TestUpDetach_OnPTYStartsDaemon) proves the composite
			// property end to end; THIS row is what isolates the predicate.
			name: "--detach + a TUI value that was never typed: no conflict, plain",
			in:   tuiModeInputs{TUISet: false, TUIVal: true, Detach: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--no-tui=false + --detach: asserts nothing, still plain",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: false, Detach: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},

		// --- conflicts --------------------------------------------------------
		{
			name:    "--tui + --detach: the existing error, verbatim",
			in:      tuiModeInputs{TUISet: true, TUIVal: true, Detach: true},
			wantErr: "--tui and --detach are mutually exclusive",
		},
		{
			name:    "--tui + --no-tui (both true): mutually exclusive",
			in:      tuiModeInputs{TUISet: true, TUIVal: true, NoTUISet: true, NoTUIVal: true},
			wantErr: "--tui and --no-tui are mutually exclusive",
		},
		{
			name: "--tui=false + --no-tui: NOT an error, they agree",
			in:   tuiModeInputs{TUISet: true, TUIVal: false, NoTUISet: true, NoTUIVal: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--tui + --no-tui=false: NOT an error, only one assertion made",
			in:   tuiModeInputs{TUISet: true, TUIVal: true, NoTUISet: true, NoTUIVal: false},
			want: tuiModeRequired, wantAuto: tuiModeRequired,
		},

		// --- explicit flags ---------------------------------------------------
		{
			name: "--tui: required (an assertion, so an incapable terminal must error)",
			in:   tuiModeInputs{TUISet: true, TUIVal: true},
			want: tuiModeRequired, wantAuto: tuiModeRequired,
		},
		{
			name: "--tui=false: plain (an explicit negative assertion, not an absent opinion)",
			in:   tuiModeInputs{TUISet: true, TUIVal: false},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--no-tui: plain",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--no-tui=false: asserts nothing, falls through",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: false},
			want: tuiModePlain, wantAuto: tuiModePreferred,
		},
		{
			name: "value without Changed is NOT an assertion (the wiring bug this split exists to catch)",
			in:   tuiModeInputs{TUISet: false, TUIVal: true},
			want: tuiModePlain, wantAuto: tuiModePreferred,
		},

		// --- flag beats env ---------------------------------------------------
		{
			name: "--tui beats PROX_TUI=0",
			in:   tuiModeInputs{TUISet: true, TUIVal: true, Env: "0", EnvPresent: true},
			want: tuiModeRequired, wantAuto: tuiModeRequired,
		},
		{
			name: "--no-tui beats PROX_TUI=1",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: true, Env: "1", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "--tui=false beats PROX_TUI=1",
			in:   tuiModeInputs{TUISet: true, TUIVal: false, Env: "1", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "unrecognized env + an explicit flag: flag wins and there is NO warning (the env is never consulted)",
			in:   tuiModeInputs{TUISet: true, TUIVal: true, Env: "banana", EnvPresent: true},
			want: tuiModeRequired, wantAuto: tuiModeRequired,
		},
		{
			name: "unrecognized env + --no-tui: no warning either",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: true, Env: "banana", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},

		// --- env vocabulary ---------------------------------------------------
		{
			name: "PROX_TUI unset: falls through",
			in:   tuiModeInputs{},
			want: tuiModePlain, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=1: preferred, NOT required (a standing shell preference must not booby-trap a piped run)",
			in:   tuiModeInputs{Env: "1", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=0: plain",
			in:   tuiModeInputs{Env: "0", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "PROX_TUI=true",
			in:   tuiModeInputs{Env: "true", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=false",
			in:   tuiModeInputs{Env: "false", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "PROX_TUI=yes",
			in:   tuiModeInputs{Env: "yes", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=no",
			in:   tuiModeInputs{Env: "no", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "PROX_TUI=on",
			in:   tuiModeInputs{Env: "on", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=off",
			in:   tuiModeInputs{Env: "off", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "PROX_TUI is case-insensitive",
			in:   tuiModeInputs{Env: "TRUE", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI is mixed-case tolerant",
			in:   tuiModeInputs{Env: "Off", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePlain,
		},
		{
			name: "PROX_TUI surrounding whitespace is trimmed",
			in:   tuiModeInputs{Env: "  yes\t", EnvPresent: true},
			want: tuiModePreferred, wantAuto: tuiModePreferred,
		},
		{
			name: "PROX_TUI=t is NOT accepted (the vocabulary is pinned here, not delegated to strconv.ParseBool)",
			in:   tuiModeInputs{Env: "t", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePreferred, wantWarning: `PROX_TUI="t"`,
		},
		{
			name: "PROX_TUI=2 warns and falls through",
			in:   tuiModeInputs{Env: "2", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePreferred, wantWarning: `PROX_TUI="2"`,
		},
		{
			name: "PROX_TUI set to empty warns and falls through",
			in:   tuiModeInputs{Env: "", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePreferred, wantWarning: `PROX_TUI=""`,
		},
		{
			name: "PROX_TUI unrecognized + --no-tui=false: still warns (no assertion was made)",
			in:   tuiModeInputs{NoTUISet: true, NoTUIVal: false, Env: "banana", EnvPresent: true},
			want: tuiModePlain, wantAuto: tuiModePreferred, wantWarning: `PROX_TUI="banana"`,
		},
	}

	for _, tt := range tests {
		for _, auto := range []bool{false, true} {
			label := "AutoDefault=false"
			want := tt.want
			if auto {
				label = "AutoDefault=true"
				want = tt.wantAuto
			}
			t.Run(tt.name+" ["+label+"]", func(t *testing.T) {
				in := tt.in
				in.AutoDefault = auto

				got, warnings, err := resolveTUIMode(in)

				if tt.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tt.wantErr)
					assert.Empty(t, warnings, "an erroring resolution must not also emit warnings")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, want.String(), got.String())

				if tt.wantWarning == "" {
					assert.Empty(t, warnings)
					return
				}
				require.Len(t, warnings, 1)
				assert.Contains(t, warnings[0], tt.wantWarning)
			})
		}
	}
}

// TestResolveTUIMode_AutoDefaultOnlyMovesTheUnaskedRow is the guard that makes
// C7 a safe one-line flip: every input where SOMETHING was asked for — a flag,
// or a recognized PROX_TUI — must resolve identically whether AutoDefault is on
// or off. Only the rows where nothing was asked for may move.
//
// The matrix above encodes the same claim row by row; this states it as a
// property, so a future row added with a careless wantAuto cannot quietly
// weaken it.
func TestResolveTUIMode_AutoDefaultOnlyMovesTheUnaskedRow(t *testing.T) {
	asked := []tuiModeInputs{
		{Detach: true},
		{TUISet: true, TUIVal: true},
		{TUISet: true, TUIVal: false},
		{NoTUISet: true, NoTUIVal: true},
		{Env: "1", EnvPresent: true},
		{Env: "0", EnvPresent: true},
	}
	for _, in := range asked {
		off := in
		off.AutoDefault = false
		on := in
		on.AutoDefault = true

		gotOff, _, errOff := resolveTUIMode(off)
		gotOn, _, errOn := resolveTUIMode(on)

		require.NoError(t, errOff)
		require.NoError(t, errOn)
		assert.Equal(t, gotOff.String(), gotOn.String(), "inputs %+v must not depend on AutoDefault", in)
	}
}

// TestResolveTUIMode_DetachIsUnconditionallyPlain states, as a property, the
// two claims that actually carry `prox up -d`'s immunity to the flip. The pty
// test (test/integration/tui_pty_test.go, TestUpDetach_OnPTYStartsDaemon) pins
// the composite behavior on a real terminal, but it cannot isolate either claim:
// the short-circuit below runs before terminal capability is ever consulted, so
// a conflict check reading the resolved mode could not fire there anyway. This
// is where the isolation lives (codex review of plan 026 C7).
//
//  1. Detach ⇒ plain for EVERY other combination of inputs, AutoDefault
//     included. The resolver is not handed terminal capability at all — runUp
//     probes the terminal only after this returns — so "regardless of terminal
//     capability" is a structural property of the short-circuit, and this
//     enumerates the inputs it must survive.
//  2. The --tui/--detach conflict requires TUISet && TUIVal — the flag TYPED
//     as an assertion. Neither a bare `-d` nor `--tui=false -d` nor
//     `--no-tui=false -d` may error, whatever the value half says.
func TestResolveTUIMode_DetachIsUnconditionallyPlain(t *testing.T) {
	// Every input shape that is NOT an explicit `--tui` assertion. The one
	// combination deliberately absent is TUISet && TUIVal, which is the
	// conflict and is asserted separately below.
	others := []tuiModeInputs{
		{},
		{TUISet: true, TUIVal: false},
		{TUISet: false, TUIVal: true}, // the "resolved value passed as if typed" bug
		{NoTUISet: true, NoTUIVal: true},
		{NoTUISet: true, NoTUIVal: false},
		{Env: "1", EnvPresent: true},
		{Env: "0", EnvPresent: true},
		{Env: "banana", EnvPresent: true},
		{Env: "", EnvPresent: true},
	}
	for _, base := range others {
		for _, auto := range []bool{false, true} {
			in := base
			in.Detach = true
			in.AutoDefault = auto

			got, warnings, err := resolveTUIMode(in)

			require.NoError(t, err, "no non-asserted input may make `prox up -d` an error: %+v", in)
			assert.Equal(t, tuiModePlain.String(), got.String(), "detach must short-circuit to plain: %+v", in)
			assert.Empty(t, warnings, "detach skips the env parse entirely, so the -d parent and its re-exec'd child cannot double-warn: %+v", in)
		}
	}

	// The conflict fires for, and only for, a typed `--tui`.
	for _, auto := range []bool{false, true} {
		_, _, err := resolveTUIMode(tuiModeInputs{TUISet: true, TUIVal: true, Detach: true, AutoDefault: auto})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--tui and --detach are mutually exclusive")
	}
}

// TestTuiModeString keeps the failure output of the matrix above readable: the
// assertions compare String() values, so a missing case would turn every
// mismatch into "tuiMode(1) != tuiMode(2)".
func TestTuiModeString(t *testing.T) {
	assert.Equal(t, "plain", tuiModePlain.String())
	assert.Equal(t, "preferred", tuiModePreferred.String())
	assert.Equal(t, "required", tuiModeRequired.String())
	assert.Equal(t, "tuiMode(9)", tuiMode(9).String())
}

// TestTerminalHostable_NonInteractive pins the message precedence rule for the
// one condition reachable under `go test`, whose stdio is always pipes:
// not-a-terminal outranks TERM and the background-job check, and its string is
// the verbatim one that test/integration/up_test.go and tui_pty_test.go assert
// on. TERM is set to a hostile value to prove the precedence rather than
// accidentally agreeing with it.
//
// Conditions 2 and 3 need a real pty and live in test/integration.
func TestTerminalHostable_NonInteractive(t *testing.T) {
	t.Setenv("TERM", "dumb")

	err := terminalHostable()

	require.Error(t, err)
	assert.Equal(t, "--tui requires an interactive terminal", err.Error(),
		"condition 1 outranks TERM, and its message is asserted on by the integration suite")
}
