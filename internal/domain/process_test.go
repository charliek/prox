package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessState_String(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  string
	}{
		{ProcessStateRunning, "running"},
		{ProcessStateStopped, "stopped"},
		{ProcessStateStarting, "starting"},
		{ProcessStateStopping, "stopping"},
		{ProcessStateCrashed, "crashed"},
		{ProcessStateWaiting, "waiting"},
		{ProcessStateBlocked, "blocked"},
		{ProcessStateCompleted, "completed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.String())
		})
	}
}

func TestProcessState_IsRunning(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  bool
	}{
		{ProcessStateRunning, true},
		{ProcessStateStopped, false},
		{ProcessStateStarting, false},
		{ProcessStateStopping, false},
		{ProcessStateCrashed, false},
		// A waiting process is NOT running (no live instance yet); blocked and
		// completed are terminal, also not running (plan 013 D4).
		{ProcessStateWaiting, false},
		{ProcessStateBlocked, false},
		{ProcessStateCompleted, false},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.IsRunning())
		})
	}
}

func TestProcessState_IsStopped(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  bool
	}{
		{ProcessStateRunning, false},
		{ProcessStateStopped, true},
		{ProcessStateStarting, false},
		{ProcessStateStopping, false},
		{ProcessStateCrashed, true},
		// waiting is deliberately NOT stopped (it is scheduled to launch);
		// blocked and completed are terminal not-running states (plan 013 D4).
		{ProcessStateWaiting, false},
		{ProcessStateBlocked, true},
		{ProcessStateCompleted, true},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.IsStopped())
		})
	}
}

func TestAllProcessStates(t *testing.T) {
	want := []ProcessState{
		ProcessStateRunning,
		ProcessStateStopped,
		ProcessStateStarting,
		ProcessStateStopping,
		ProcessStateCrashed,
		ProcessStateWaiting,
		ProcessStateBlocked,
		ProcessStateCompleted,
	}
	got := AllProcessStates()
	assert.ElementsMatch(t, want, got)
	assert.Len(t, got, 8)

	seen := make(map[ProcessState]bool, len(got))
	for _, s := range got {
		assert.False(t, seen[s], "duplicate state %q", s)
		seen[s] = true
	}

	// The returned slice must be a fresh copy each call: mutating it must not
	// affect a later call.
	got[0] = "mutated"
	assert.NotEqual(t, ProcessState("mutated"), AllProcessStates()[0])
}

// TestProcessState_IsLiveAndIsTerminalFailure pins IsLive and
// IsTerminalFailure against every member of AllProcessStates, keyed by state
// so an unlisted new state fails the length assertion rather than silently
// passing with a default zero value.
func TestProcessState_IsLiveAndIsTerminalFailure(t *testing.T) {
	cases := map[ProcessState]struct {
		wantLive            bool
		wantTerminalFailure bool
	}{
		ProcessStateRunning:   {wantLive: true, wantTerminalFailure: false},
		ProcessStateStopped:   {wantLive: false, wantTerminalFailure: false},
		ProcessStateStarting:  {wantLive: true, wantTerminalFailure: false},
		ProcessStateStopping:  {wantLive: true, wantTerminalFailure: false},
		ProcessStateCrashed:   {wantLive: false, wantTerminalFailure: true},
		ProcessStateWaiting:   {wantLive: true, wantTerminalFailure: false},
		ProcessStateBlocked:   {wantLive: false, wantTerminalFailure: true},
		ProcessStateCompleted: {wantLive: false, wantTerminalFailure: false},
	}
	require.Len(t, cases, len(AllProcessStates()), "every state in AllProcessStates must be classified here")

	for _, state := range AllProcessStates() {
		tt, ok := cases[state]
		require.True(t, ok, "state %q not classified", state)
		t.Run(state.String(), func(t *testing.T) {
			assert.Equal(t, tt.wantLive, state.IsLive(), "IsLive")
			assert.Equal(t, tt.wantTerminalFailure, state.IsTerminalFailure(), "IsTerminalFailure")

			// The two predicates must never both be true for the same state: a
			// state cannot be both "still changing on its own" and
			// "definitively failed".
			//
			// Asserted on the METHOD results, not on the table literals above.
			// Reading tt.wantLive && tt.wantTerminalFailure here would only
			// prove the table is self-consistent, and would pass against any
			// implementation at all (CodeRabbit, PR #110).
			assert.False(t, state.IsLive() && state.IsTerminalFailure(),
				"state %q cannot be both live and a terminal failure", state)
		})
	}
}

func TestProcessInfo_UptimeSeconds(t *testing.T) {
	t.Run("zero when not started", func(t *testing.T) {
		info := ProcessInfo{}
		assert.Equal(t, int64(0), info.UptimeSeconds())
	})

	t.Run("calculates uptime", func(t *testing.T) {
		info := ProcessInfo{
			StartedAt: time.Now().Add(-10 * time.Second),
		}
		uptime := info.UptimeSeconds()
		assert.GreaterOrEqual(t, uptime, int64(9))
		assert.LessOrEqual(t, uptime, int64(11))
	})
}
