package proxyd

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// instantAfter is a forwarderConfig.after that fires immediately, so the loop's
// backoff never costs wall-clock time in tests.
func instantAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// TestShouldHeal pins the D6b timing gates: no heal while connected or before the
// down threshold; the first heal fires once the outage exceeds healAfterDown; a
// second heal is damped until healMinInterval has elapsed since the last attempt.
func TestShouldHeal(t *testing.T) {
	const (
		afterDown   = 15 * time.Second
		minInterval = 30 * time.Second
	)
	base := time.Unix(1_000_000, 0)

	// Connected: never heal.
	assert.False(t, shouldHeal(base, time.Time{}, time.Time{}, afterDown, minInterval))

	// Down but under the threshold: no heal.
	assert.False(t, shouldHeal(base.Add(10*time.Second), base, time.Time{}, afterDown, minInterval))

	// Down past the threshold, no prior heal: first heal fires.
	assert.True(t, shouldHeal(base.Add(15*time.Second), base, time.Time{}, afterDown, minInterval))

	// A heal just happened: damped until healMinInterval elapses.
	last := base.Add(15 * time.Second)
	assert.False(t, shouldHeal(base.Add(40*time.Second), base, last, afterDown, minInterval),
		"second heal must not fire before healMinInterval since the last attempt")
	assert.True(t, shouldHeal(base.Add(45*time.Second), base, last, afterDown, minInterval),
		"second heal fires once healMinInterval has elapsed")
}

// TestForwardRequests_InvokesHealAfterThreshold pins that the forwarder loop
// invokes the heal callback inline once the outage exceeds healAfterDown, using
// an injected clock (no wall-clock 15s wait) and an instant backoff timer. The
// socket points at nothing, so every connect fails and the outage clock advances
// via the injected now().
func TestForwardRequests_InvokesHealAfterThreshold(t *testing.T) {
	deadSocket := filepath.Join(t.TempDir(), "dead.sock") // nothing listens

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// now() advances 20s per call, so the second failed reconnect is >15s after
	// downSince (set on the first) and the heal fires.
	var mu sync.Mutex
	cur := time.Unix(1_000_000, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := cur
		cur = cur.Add(20 * time.Second)
		return t
	}

	healed := make(chan struct{}, 1)
	cfg := forwarderConfig{
		socketPath: deadSocket,
		projectDir: "/p",
		localRM:    proxy.NewRequestManager(100),
		heal: func() bool {
			select {
			case healed <- struct{}{}:
			default:
			}
			cancel() // stop the loop once the heal fired
			return false
		},
		now:             now,
		after:           instantAfter,
		healAfterDown:   15 * time.Second,
		healMinInterval: 30 * time.Second,
	}

	go forwardRequests(ctx, cfg)

	select {
	case <-healed:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder never invoked the heal callback after the down threshold")
	}
}

// TestForwardRequests_HealDampingAcrossOutages pins FIX 4: lastHeal is NOT reset
// when the forwarder reconnects, so the ≥healMinInterval heal spacing holds across
// a brief reconnect. Scenario: heal at t, a successful connect, a drop into a new
// outage that itself exceeds healAfterDown — the next heal must still wait until
// t+healMinInterval (t+30s), NOT re-fire at new-outage-start + healAfterDown.
//
// The stream attempt and clock are injected so the connect/drop/outage sequence
// and the heal timestamps are fully deterministic (no wall-clock, no sockets).
func TestForwardRequests_HealDampingAcrossOutages(t *testing.T) {
	const (
		afterDown   = 15 * time.Second
		minInterval = 30 * time.Second
	)
	t0 := time.Unix(2_000_000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// now() is called exactly once per FAILED connect (never on a connected one).
	// The five failed iterations below observe these instants in order.
	nowVals := []time.Time{
		t0,                       // fail #1: outage starts, under threshold
		t0.Add(20 * time.Second), // fail #2: >15s down, no prior heal -> HEAL #1 at t0+20
		t0.Add(25 * time.Second), // fail #3: drop into a new outage
		t0.Add(40 * time.Second), // fail #4: new outage >15s, but <30s since heal #1 -> damped
		t0.Add(51 * time.Second), // fail #5: >=30s since heal #1 -> HEAL #2
	}
	var mu sync.Mutex
	var nowIdx int
	var lastNow time.Time
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		// Clamp past the script: heal #2 cancels ctx, and the loop's post-stream
		// ctx.Err() check returns before another scripted instant is needed — but the
		// instant-backoff select can race ctx.Done() for one extra iteration.
		if nowIdx >= len(nowVals) {
			return lastNow
		}
		v := nowVals[nowIdx]
		nowIdx++
		lastNow = v
		return v
	}

	// Scripted connect outcomes: fail, fail(heal), connect, fail, fail, fail.
	streamResults := []bool{false, false, true, false, false, false}
	var streamIdx int
	stream := func(context.Context, string, *Client, string, *proxy.RequestManager, ForwarderStatusSink) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		// Clamp: after heal #2 cancels ctx, an extra iteration may fire before the
		// loop observes cancellation. Return a plain failure (the caller returns on
		// its next ctx.Err() check) rather than indexing past the script.
		if streamIdx >= len(streamResults) {
			return false, errors.New("down")
		}
		r := streamResults[streamIdx]
		streamIdx++
		if r {
			return true, nil
		}
		return false, errors.New("down")
	}

	var healTimes []time.Time
	heal := func() bool {
		mu.Lock()
		healTimes = append(healTimes, lastNow)
		n := len(healTimes)
		mu.Unlock()
		if n == 1 {
			return true // first heal "succeeds" -> the next iteration connects
		}
		cancel() // second heal recorded: stop the loop
		return false
	}

	cfg := forwarderConfig{
		socketPath:      filepath.Join(t.TempDir(), "unused.sock"),
		projectDir:      "/p",
		localRM:         proxy.NewRequestManager(100),
		heal:            heal,
		stream:          stream,
		now:             now,
		after:           instantAfter,
		healAfterDown:   afterDown,
		healMinInterval: minInterval,
	}

	done := make(chan struct{})
	go func() { forwardRequests(ctx, cfg); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder loop did not complete the scripted sequence")
	}

	require.Len(t, healTimes, 2, "exactly two heals must fire across the scenario")
	assert.Equal(t, t0.Add(20*time.Second), healTimes[0], "first heal fires once the outage exceeds healAfterDown")
	// The core of FIX 4: the second heal is damped to >= t+healMinInterval despite
	// the new outage independently exceeding healAfterDown at t0+40s.
	assert.True(t, !healTimes[1].Before(healTimes[0].Add(minInterval)),
		"second heal must not fire before healMinInterval after the first (got %v, first %v)", healTimes[1], healTimes[0])
	assert.Equal(t, t0.Add(51*time.Second), healTimes[1],
		"second heal fires only once >=healMinInterval has elapsed since the first, not at the new outage's healAfterDown")
}
