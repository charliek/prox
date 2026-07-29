// Package stream provides the generic reconnect runner shared by every prox
// stream consumer: the TUI's attach-mode SSE streams and the CLI's --follow
// commands. It owns the connect/backoff/reconnect state machine and the status
// reporting; callers supply only a single "connect and consume until the stream
// ends" attempt function plus a policy for classifying attempt errors.
//
// This package is a leaf on purpose. internal/cli imports internal/tui, so the
// runner has to be importable by both; it therefore depends on nothing beyond
// the standard library and internal/constants.
package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/charliek/prox/internal/constants"
)

// State is the reconnect loop's externally visible condition. Consumers render
// it directly (a TUI status line, a CLI stderr notice).
//
// The constants are prefixed State* because Classification below needs an
// Unavailable member too, and the two enums share this package's namespace.
type State int

const (
	// StateConnecting means the very first attempt is in progress. Nothing has
	// been delivered yet and no reconnect has happened.
	StateConnecting State = iota

	// StateSyncing means an attempt is in progress and has not yet reported
	// that it is synchronized. It deliberately covers dial, handshake AND
	// snapshot synchronization as one phase: the loop only learns about the
	// attempt's progress through markSynced, and consumers render Connecting
	// and Syncing identically anyway.
	StateSyncing

	// StateOK means the current attempt called markSynced: it is connected and
	// whatever synchronization the consumer defines (usually a snapshot fetch
	// followed by live events) has completed.
	StateOK

	// StateReconnecting means an attempt ended and a backoff wait is in
	// progress. Status.Err carries the error that ended it, or nil when the
	// attempt ended cleanly (the server closed the stream).
	StateReconnecting

	// StateUnavailable means the policy classified the attempt's error as not
	// worth retrying aggressively (for example: the server is not running at
	// all). The loop parks — no timed retries — and stays latched in this state
	// until an external re-probe via (*Loop).Probe or context cancellation.
	StateUnavailable

	// StateClosed means the loop has ended, either because the context was
	// cancelled or because the policy classified an error as terminal. It is
	// always the last status a Loop reports, and it is reported exactly once.
	StateClosed
)

// String implements fmt.Stringer for readable logs and test failures.
func (s State) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateSyncing:
		return "syncing"
	case StateOK:
		return "ok"
	case StateReconnecting:
		return "reconnecting"
	case StateUnavailable:
		return "unavailable"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Status is one state transition delivered to Config.OnStatus.
type Status struct {
	State State

	// Err carries the cause for StateReconnecting (the error that ended the
	// attempt, nil for a clean end), StateUnavailable (the error the policy
	// declined to retry aggressively) and StateClosed (the terminal error, or
	// the context's error when the loop was cancelled). It is always nil for
	// StateConnecting, StateSyncing and StateOK.
	Err error
}

// Classification decides how the loop treats an attempt error.
type Classification int

const (
	// ClassTransient retries with the exponential backoff. This is the default
	// for every error when Config.Classify is nil, and for an attempt that
	// returned nil (a cleanly ended stream is still something to reconnect).
	ClassTransient Classification = iota

	// ClassUnavailable parks the loop in StateUnavailable with no timed retry
	// until (*Loop).Probe or context cancellation.
	ClassUnavailable

	// ClassTerminal ends the loop: it reports StateClosed carrying the error
	// and Run returns.
	ClassTerminal
)

// Config configures a Loop. Only Attempt is required.
type Config struct {
	// Attempt connects and consumes the stream until it ends, calling
	// markSynced once the caller-defined synchronization completes (a pure
	// stream consumer, with no snapshot to reconcile, calls it immediately
	// after connecting). It must honor ctx: the loop only observes
	// cancellation between attempts, so an attempt that ignores ctx delays
	// shutdown for as long as it runs.
	//
	// The return value is the error that ended the attempt; nil means the
	// stream ended cleanly, which the loop treats as a transient end and
	// reconnects from. A cancelled context ends the loop regardless of what
	// the attempt returns.
	//
	// markSynced may be called more than once (repeats are deduplicated) and
	// is ignored once the attempt has returned, so a straggling goroutine
	// cannot flip a later attempt's state. It must be called from the attempt
	// itself rather than from a goroutine that outlives it; see the OnStatus
	// serialization note below.
	Attempt func(ctx context.Context, markSynced func()) error

	// Classify maps an attempt error to a Classification. It is called only on
	// non-nil errors. A nil Classify treats everything as ClassTransient.
	Classify func(error) Classification

	// OnStatus receives every state transition. It is never called
	// concurrently, and in normal use every call comes from the goroutine
	// running Run (markSynced is expected to be called synchronously from
	// within Attempt). Consecutive duplicates are suppressed: the loop never
	// delivers the same State twice in a row with the same error text. A nil
	// OnStatus discards the transitions.
	OnStatus func(Status)

	// Now and After inject time for tests (nil means real time), following the
	// forwarderConfig idiom in internal/proxyd. Now is sampled exactly twice
	// per attempt (immediately before and immediately after) to drive the flap
	// guard; After is called once per backoff wait and never while parked in
	// StateUnavailable.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// errNoAttempt is reported as the terminal error when a Loop is run without an
// Attempt function, rather than panicking inside a consumer's goroutine.
var errNoAttempt = errors.New("stream: Config.Attempt is nil")

// Loop is a reconnect runner. Create one with NewLoop, drive it with Run, and
// wake a parked one with Probe. A Loop is single-use: Run must be called at
// most once.
type Loop struct {
	cfg Config

	// probe is a capacity-1 channel, so Probe is non-blocking and coalescing:
	// N probes delivered before the loop consumes one wake it exactly once.
	probe chan struct{}

	// mu serializes status delivery and the attempt-epoch bookkeeping, so a
	// markSynced call is safe (and correctly ordered) no matter which
	// goroutine makes it.
	mu       sync.Mutex
	epoch    uint64 // bumped when an attempt starts and again when it returns
	last     Status // previous delivered status, for consecutive-duplicate suppression
	hasLast  bool
	finished bool // set once StateClosed is delivered; suppresses anything later
}

// NewLoop returns a Loop for cfg.
func NewLoop(cfg Config) *Loop {
	return &Loop{
		cfg:   cfg,
		probe: make(chan struct{}, 1),
	}
}

// Probe wakes a loop parked in StateUnavailable for one immediate retry, and
// short-circuits an in-progress backoff wait for the same effect. It is safe to
// call from any goroutine, never blocks, and coalesces: probes delivered while
// the loop is busy collapse into a single retry. A probe raised while an
// attempt is in flight is retained and short-circuits the next backoff wait. A
// probe does not reset the backoff — an external nudge should not let a caller
// defeat the backoff by probing in a tight loop.
func (l *Loop) Probe() {
	select {
	case l.probe <- struct{}{}:
	default:
	}
}

// Run drives the reconnect loop — attempt, flap-guarded backoff, attempt — and
// blocks until ctx is done or an error is classified ClassTerminal. It reports
// exactly one StateClosed status before returning.
func (l *Loop) Run(ctx context.Context) {
	if l.cfg.Attempt == nil {
		l.emit(Status{State: StateClosed, Err: errNoAttempt})
		return
	}

	now := l.cfg.Now
	if now == nil {
		now = time.Now
	}
	after := l.cfg.After
	if after == nil {
		after = time.After
	}
	classify := l.cfg.Classify
	if classify == nil {
		classify = func(error) Classification { return ClassTransient }
	}

	backoff := constants.StreamReconnectBaseBackoff
	first := true

	for {
		if ctx.Err() != nil {
			l.emit(Status{State: StateClosed, Err: ctx.Err()})
			return
		}

		if first {
			l.emit(Status{State: StateConnecting})
			first = false
		} else {
			l.emit(Status{State: StateSyncing})
		}

		start := now()
		err := l.attempt(ctx)
		if ctx.Err() != nil {
			l.emit(Status{State: StateClosed, Err: ctx.Err()})
			return
		}
		lived := now().Sub(start)

		class := ClassTransient
		if err != nil {
			class = classify(err)
		}
		if class == ClassTerminal {
			l.emit(Status{State: StateClosed, Err: err})
			return
		}

		// Flap guard (mirrors internal/proxyd/forwarder.go): only an attempt
		// that survived the threshold counts as a recovery. A connect that
		// dies instantly leaves the backoff where it was, so a crash-looping
		// server still backs off instead of being retried at the base rate.
		if lived >= constants.StreamReconnectFlapThreshold {
			backoff = constants.StreamReconnectBaseBackoff
		}

		if class == ClassUnavailable {
			l.emit(Status{State: StateUnavailable, Err: err})
			// Parked: no timer is armed at all. Only an external Probe or
			// cancellation gets us out of here.
			select {
			case <-ctx.Done():
				l.emit(Status{State: StateClosed, Err: ctx.Err()})
				return
			case <-l.probe:
			}
			continue
		}

		l.emit(Status{State: StateReconnecting, Err: err})
		select {
		case <-ctx.Done():
			l.emit(Status{State: StateClosed, Err: ctx.Err()})
			return
		case <-l.probe:
			// Retry now, but still let the backoff grow: a probe skips the
			// wait, it does not declare the server healthy.
		case <-after(backoff):
		}
		// Double, then clamp — deliberately not the forwarder's "double only
		// while under the cap", which overshoots and settles at 8s. Here the
		// steady-state re-probe rate is exactly StreamReconnectMaxBackoff.
		backoff *= 2
		if backoff > constants.StreamReconnectMaxBackoff {
			backoff = constants.StreamReconnectMaxBackoff
		}
	}
}

// Run drives a one-shot reconnect loop for cfg. It is the convenience form of
// NewLoop(cfg).Run(ctx) for consumers that never need to Probe.
func Run(ctx context.Context, cfg Config) {
	NewLoop(cfg).Run(ctx)
}

// attempt runs one connect-and-consume cycle, handing the attempt a markSynced
// closure bound to this cycle's epoch. Bumping the epoch on return makes a late
// markSynced from a straggling goroutine a no-op instead of a spurious
// StateOK on top of the next attempt.
func (l *Loop) attempt(ctx context.Context) error {
	epoch := l.nextEpoch()
	defer l.nextEpoch() // invalidates this cycle's markSynced closure
	return l.cfg.Attempt(ctx, func() { l.markSynced(epoch) })
}

// nextEpoch bumps the attempt epoch and returns the new value. markSynced
// compares the epoch its closure captured against the current one, so any bump
// invalidates every outstanding closure.
func (l *Loop) nextEpoch() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.epoch++
	return l.epoch
}

func (l *Loop) markSynced(epoch uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.epoch != epoch {
		return // the attempt that owned this callback has already ended
	}
	l.emitLocked(Status{State: StateOK})
}

func (l *Loop) emit(s Status) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emitLocked(s)
}

// emitLocked delivers s under l.mu, which is what keeps OnStatus from ever
// running concurrently, and drops consecutive duplicates plus anything that
// follows the single StateClosed.
func (l *Loop) emitLocked(s Status) {
	if l.finished {
		return
	}
	if l.hasLast && sameStatus(l.last, s) {
		return
	}
	l.last = s
	l.hasLast = true
	if s.State == StateClosed {
		l.finished = true
	}
	if l.cfg.OnStatus != nil {
		l.cfg.OnStatus(s)
	}
}

// sameStatus reports whether two statuses are indistinguishable for
// duplicate-suppression purposes. Errors are compared by message rather than by
// ==, because an arbitrary error's dynamic type need not be comparable and ==
// on a non-comparable dynamic type panics at runtime.
func sameStatus(a, b Status) bool {
	if a.State != b.State {
		return false
	}
	if (a.Err == nil) != (b.Err == nil) {
		return false
	}
	return a.Err == nil || a.Err.Error() == b.Err.Error()
}
