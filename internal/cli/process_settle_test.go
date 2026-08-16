package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsTerminalFailureState_CoversEveryState is the guard on the ONE decision
// this commit makes: which process states mean "the start failed".
//
// It iterates domain.AllProcessStates() rather than a literal map, so the
// table cannot silently fall behind the enum — a require.Len(cases, 8) guard
// against a hand-written map does NOT fail when a 9th state is added (the map
// and the literal both stay at 8 unless someone remembers to touch both);
// iterating the enum's own enumeration does, because the length assertion is
// against len(AllProcessStates()), not a number typed by hand. That matters
// because widening this set silently changes the exit code of `prox up -d`,
// `prox start` and `prox restart`.
//
// The specific trap it exists to prevent: reaching for ProcessState.IsStopped
// as the predicate. IsStopped is true for completed — a task's terminal SUCCESS
// — so a project with a migration task would have started reporting a failed
// start for a migration that worked.
func TestIsTerminalFailureState_CoversEveryState(t *testing.T) {
	cases := map[domain.ProcessState]bool{
		domain.ProcessStateRunning:   false, // the goal
		domain.ProcessStateStarting:  false, // transient
		domain.ProcessStateStopping:  false, // transient
		domain.ProcessStateWaiting:   false, // limbo: gated, still scheduled to launch
		domain.ProcessStateStopped:   false, // deliberate
		domain.ProcessStateCompleted: false, // a task's terminal SUCCESS
		domain.ProcessStateCrashed:   true,
		domain.ProcessStateBlocked:   true,
	}

	// If this fails, domain.ProcessState gained (or lost) a member: decide
	// explicitly whether it is a terminal FAILURE and add it above. Do not just
	// bump the number.
	require.Len(t, cases, len(domain.AllProcessStates()), "every domain.ProcessState must be classified here")

	for _, state := range domain.AllProcessStates() {
		wantFailure, ok := cases[state]
		require.True(t, ok, "state %q not classified above", state)

		got := isTerminalFailureState(string(state))
		assert.Equal(t, wantFailure, got, "state %q", state)

		// The evaluator must agree with the predicate for every state, so the
		// two can never diverge.
		v := evaluateProcessSettle([]settleProcess{{Name: "p", Status: string(state)}})
		assert.Equal(t, wantFailure, v.failed(), "evaluator disagrees for state %q", state)
	}

	// An unknown status from a newer daemon is not a failure: an exit code must
	// never be invented from a value this binary cannot interpret.
	assert.False(t, isTerminalFailureState("some-future-state"))
}

// TestEvaluateProcessSettle_SplitsAndOrders: crashed and blocked are reported
// separately, in the daemon's own order, and both are surfaced when both apply.
func TestEvaluateProcessSettle_SplitsAndOrders(t *testing.T) {
	v := evaluateProcessSettle([]settleProcess{
		{Name: "web", Status: string(domain.ProcessStateRunning)},
		{Name: "worker", Status: string(domain.ProcessStateCrashed)},
		{Name: "gated", Status: string(domain.ProcessStateBlocked), BlockedOn: []string{"pg", "redis"}},
		{Name: "api", Status: string(domain.ProcessStateCrashed)},
		{Name: "migrate", Status: string(domain.ProcessStateCompleted)},
	})

	require.True(t, v.failed())
	assert.Equal(t, []string{"worker", "api"}, v.crashed, "reported order, not sorted")
	require.Len(t, v.blocked, 1)
	assert.Equal(t, "gated", v.blocked[0].Name)

	// Crashed outranks blocked in the returned sentinel, matching statusExitError.
	assert.Equal(t, "2 process(es) crashed", v.err().Error())

	var sb strings.Builder
	v.writeTo(&sb, "hint line")
	out := sb.String()
	assert.Contains(t, out, "Crashed: worker, api — check 'prox logs worker'.")
	assert.Contains(t, out, "Blocked: gated(pg, redis)")
	assert.Contains(t, out, "hint line")
}

// TestSettleVerdict_BlockedWithoutTargets: GET /processes/{name} carries no
// blocked_on, so the start/restart path has no targets to name. It must render
// the bare name rather than an empty "gated()".
func TestSettleVerdict_BlockedWithoutTargets(t *testing.T) {
	v := evaluateProcessSettle([]settleProcess{{Name: "gated", Status: string(domain.ProcessStateBlocked)}})

	var sb strings.Builder
	v.writeTo(&sb, "")
	assert.Contains(t, sb.String(), "Blocked: gated\n")
	assert.NotContains(t, sb.String(), "gated()")
}

// TestSettleVerdict_CleanIsSilent: a clean verdict prints nothing and returns
// no error, so a successful command gains no noise from this check.
func TestSettleVerdict_CleanIsSilent(t *testing.T) {
	v := evaluateProcessSettle([]settleProcess{{Name: "web", Status: string(domain.ProcessStateRunning)}})
	assert.False(t, v.failed())
	assert.NoError(t, v.err())

	var sb strings.Builder
	v.writeTo(&sb, "hint")
	assert.Equal(t, "", sb.String(), "a clean verdict must print nothing, not even the hint")
}

// TestAwaitProcessSettle_RunsTheWholeWindowWhenClean pins the cost this design
// accepts: concluding that NOTHING failed requires observing the whole window,
// so a clean settle polls repeatedly and takes the full budget. It is not a
// single request, and it is not free.
func TestAwaitProcessSettle_RunsTheWholeWindowWhenClean(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]settleProcess, error) {
		calls++
		return []settleProcess{{Name: "web", Status: string(domain.ProcessStateRunning)}}, nil
	}

	start := time.Now()
	v, err := awaitProcessSettle(context.Background(), fetch, 100*time.Millisecond, 10*time.Millisecond)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, v.failed())
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "the window must be run out, not short-circuited")
	assert.Greater(t, calls, 1, "a clean settle observes repeatedly")
}

// TestAwaitProcessSettle_ReturnsOnFirstFailure: a crash is stable (prox has no
// restart policy), so once one is seen there is nothing to gain from polling
// on — the user gets their answer immediately.
func TestAwaitProcessSettle_ReturnsOnFirstFailure(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]settleProcess, error) {
		calls++
		if calls < 3 {
			return []settleProcess{{Name: "web", Status: string(domain.ProcessStateStarting)}}, nil
		}
		return []settleProcess{{Name: "web", Status: string(domain.ProcessStateCrashed)}}, nil
	}

	start := time.Now()
	v, err := awaitProcessSettle(context.Background(), fetch, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, err)
	require.True(t, v.failed())
	assert.Equal(t, []string{"web"}, v.crashed)
	assert.Less(t, time.Since(start), 5*time.Second, "must not wait out the window after a verdict")
	assert.Equal(t, 3, calls)
}

// TestAwaitProcessSettle_TransientErrorIsNotAFailure: an error on one poll,
// followed by clean observations, is a clean settle. The verification's own
// flakiness is not the process's failure.
func TestAwaitProcessSettle_TransientErrorIsNotAFailure(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]settleProcess, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection reset")
		}
		return []settleProcess{{Name: "web", Status: string(domain.ProcessStateRunning)}}, nil
	}

	v, err := awaitProcessSettle(context.Background(), fetch, 60*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, err, "a later successful observation answers the question")
	assert.False(t, v.failed())
}

// TestAwaitProcessSettle_UnobservableReportsTheError: when NO observation ever
// succeeded, the caller is told so — and must then keep its pre-existing exit
// code rather than pretend either outcome.
func TestAwaitProcessSettle_UnobservableReportsTheError(t *testing.T) {
	fetch := func(context.Context) ([]settleProcess, error) {
		return nil, errors.New("connection refused")
	}

	v, err := awaitProcessSettle(context.Background(), fetch, 40*time.Millisecond, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.False(t, v.failed(), "an unobservable settle is not a failed one")
}

// TestAwaitProcessSettle_HonorsCallerCancellation: the window is a ceiling, not
// a floor — a cancelled caller (Ctrl-C) stops immediately.
func TestAwaitProcessSettle_HonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fetch := func(context.Context) ([]settleProcess, error) {
		cancel()
		return []settleProcess{{Name: "web", Status: string(domain.ProcessStateRunning)}}, nil
	}

	start := time.Now()
	_, err := awaitProcessSettle(ctx, fetch, 10*time.Second, 5*time.Millisecond)
	require.NoError(t, err, "one clean observation was made, so nothing failed")
	assert.Less(t, time.Since(start), 10*time.Second)
}

// TestSettleProjections: both endpoints feed the same evaluator. The list
// endpoint carries blocked_on; the detail endpoint does not, and that
// difference must not become a nil-pointer or a lost name.
func TestSettleProjections(t *testing.T) {
	list := settleProcessesFromList([]api.ProcessResponse{
		{Name: "web", Status: "running"},
		{Name: "gated", Status: "blocked", BlockedOn: []string{"pg"}},
	})
	require.Len(t, list, 2)
	assert.Equal(t, []string{"pg"}, list[1].BlockedOn)

	detail := settleProcessFromDetail(&api.ProcessDetailResponse{Name: "web", Status: "crashed"})
	require.Len(t, detail, 1)
	assert.Equal(t, "web", detail[0].Name)
	assert.Empty(t, detail[0].BlockedOn)
	assert.True(t, evaluateProcessSettle(detail).failed())
}
