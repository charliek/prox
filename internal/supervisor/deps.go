package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// This file is the dependency RESOLVER ENGINE (plan 013 C2 / D2). It consumes
// domain.DependencyConfig values and drives each dependency through a readiness
// state machine, exposing an outcome to the graph coordinator that C3 will build
// on top of it. It contains NO supervisor wiring, NO CLI/status surfacing, and
// NO graph/ordering logic -- only the per-dependency resolution machine plus the
// seams (prober, start runner, clock, logging) those layers inject. The real
// check runners and start-command executor live in deps_check.go.
//
// # Demand / Reset contract (what C3 relies on)
//
//	outcome := resolver.Demand(ctx, name)
//
//   - Single-flight per generation. The FIRST Demand for a name (in the current
//     generation) starts exactly one resolution goroutine and, at most, one
//     start: execution. Concurrent Demands for the same name JOIN that in-flight
//     resolution and every one of them receives the SAME outcome. Once resolved,
//     later Demands in the same generation return the cached outcome immediately
//     without re-probing.
//
//   - The caller's ctx bounds only the CALLER's wait, never the shared
//     resolution. If a caller's ctx is canceled while it is joined, that caller
//     gets DepOutcome{State: DepStateCanceled}; the underlying resolution keeps
//     running for the other demanders. To abort the resolution ITSELF (checking,
//     polling, and any in-flight start: process group), use Reset(name) or
//     Close() -- those cancel the resolution's own context, and every joined
//     demander then unblocks with a canceled outcome.
//
//   - Generations. Reset(name) invalidates the current generation: it cancels
//     any in-flight resolution for name (its demanders receive canceled) and
//     drops the cached node, so the NEXT Demand starts a FRESH resolution under
//     a new generation. This is how C3 re-resolves after a stop/restart/reload:
//     an outcome produced under generation N is never observed by a demander that
//     arrives after the Reset that ended generation N. A stale resolution
//     goroutine that finishes after its Reset publishes only to its own retired
//     node, which is no longer reachable from the map, so its outcome is
//     discarded.
//
//   - Close() cancels every in-flight resolution (supervisor shutdown). After
//     Close, Demand returns a canceled outcome without starting new work.
//
// A canceled outcome is DISTINCT from failed/warned: a dependency whose
// resolution was canceled (shutdown, reset) must NOT be counted as a readiness
// failure by C3.

// depAttemptCap bounds a SINGLE readiness-probe attempt (plan 013 D2). Every
// attempt runs under a deadline of min(depAttemptCap, remaining budget) so one
// hung dial / GET / cmd can never eat the whole check budget; the check interval
// still governs spacing between attempts.
const depAttemptCap = 2 * time.Second

// DepState is a dependency's position in the readiness state machine (plan 013
// D2). pending -> checking -> [starting] -> polling -> {healthy|warned|failed},
// with canceled reachable from any non-terminal state when the resolution's
// context is canceled. The three success/exhaustion terminals and canceled are
// the only states a resolved node rests in; the rest are transient and exist for
// status surfacing (C5).
type DepState string

const (
	// DepStatePending is the initial state before any check has run.
	DepStatePending DepState = "pending"
	// DepStateChecking is the one-shot initial readiness check.
	DepStateChecking DepState = "checking"
	// DepStateStarting is the window in which the start: command has been
	// launched (it runs concurrently with polling; see resolve).
	DepStateStarting DepState = "starting"
	// DepStatePolling is the interval-spaced re-check loop after the initial
	// check failed.
	DepStatePolling DepState = "polling"
	// DepStateHealthy is the terminal success state: a check passed.
	DepStateHealthy DepState = "healthy"
	// DepStateWarned is the terminal state when the budget was exhausted and
	// on_failure is warn: the coordinator proceeds anyway.
	DepStateWarned DepState = "warned"
	// DepStateFailed is the terminal state when the budget was exhausted and
	// on_failure is fail: the coordinator aborts.
	DepStateFailed DepState = "failed"
	// DepStateCanceled is the terminal state when the resolution's context was
	// canceled (shutdown/reset). It is NOT a readiness failure.
	DepStateCanceled DepState = "canceled"
)

// DepOutcome is the result a Demand caller receives.
type DepOutcome struct {
	// State is the terminal (or, for a caller whose own ctx died, canceled)
	// state. Healthy/Warned/Failed/Canceled are the only values a Demand caller
	// observes.
	State DepState
	// Err is the last probe error for a non-healthy outcome (nil on healthy),
	// or the context error for a canceled outcome.
	Err error
}

// Ready reports whether the coordinator may proceed past this dependency:
// healthy (check passed) or warned (budget exhausted but on_failure=warn).
func (o DepOutcome) Ready() bool {
	return o.State == DepStateHealthy || o.State == DepStateWarned
}

// Canceled reports whether the resolution (or the caller's wait) was canceled,
// which is distinct from a readiness failure.
func (o DepOutcome) Canceled() bool { return o.State == DepStateCanceled }

// DepSnapshot is a point-in-time view of a dependency's resolution for status
// surfacing (C5). It is derived from the node under lock, so it never races the
// resolution goroutine.
type DepSnapshot struct {
	Name string
	// State is the live state (may be transient, e.g. polling).
	State DepState
	// LastError is the most recent probe error rendered as a string ("" when
	// none), suitable for direct display.
	LastError string
	// StartInvoked records whether the start: command was launched this
	// generation (it never runs when the initial check already passed).
	StartInvoked bool
	// Gen is the node's generation. A caller that wants to Reset a dependency it
	// observed in a terminal (e.g. failed) state passes this to ResetIfGeneration
	// so a NEWER generation another demander just started is never clobbered.
	Gen uint64
}

// LogFunc routes a formatted line to the supervisor's system log. It mirrors
// Supervisor.SystemLog's signature so the supervisor can pass s.SystemLog
// directly; unit tests pass a capturing function instead. Dependency output and
// events are attributed with a "dep:<name>" prefix by the resolver/runner.
//
// Contract: a LogFunc is invoked from inside resolution (including teardown
// paths such as a start-command failure or the warn terminal) and MUST NOT call
// back into the owning Resolver. In particular it must not issue a Demand for a
// dependency currently resolving: that Demand would join the in-flight node and
// block, and because the start goroutine's completion is awaited on startDone
// during teardown, a reentrant call can deadlock. Keep implementations to pure
// logging.
type LogFunc func(format string, args ...interface{})

// Clock is the resolver's time seam (plan 013 D2). It abstracts wall-clock reads
// and stoppable timers so tests drive budgets, intervals, and per-attempt
// deadlines deterministically without real sleeps. Every timer the resolver
// creates is Stopped when it is no longer needed, so a fake clock can account
// for exactly the timers currently pending.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a single stoppable timer (a subset of *time.Timer).
type Timer interface {
	// C is the channel the timer fires on.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it returns false if the timer has
	// already fired or been stopped.
	Stop() bool
}

// Prober runs one readiness-check attempt. The resolver bounds each call with a
// per-attempt deadline by canceling ctx; an implementation MUST honor ctx. A nil
// return means the dependency is ready (the check passed); a non-nil error means
// not-ready-yet (or the attempt was canceled -- ctx.Err()).
type Prober interface {
	Probe(ctx context.Context, check domain.DependencyCheck) error
}

// StartRunner executes a dependency's start: command exactly once. It blocks
// until the command exits or ctx is canceled; on cancellation it kills the
// command's process GROUP (SIGTERM then SIGKILL after a grace). A non-nil error
// (launch failure, non-zero exit, or cancellation) is logged by the resolver,
// which keeps polling -- the check, not the start command's exit code, is the
// source of truth for readiness.
//
// Daemonizing start commands are explicitly SUPPORTED. A start: like
// `docker compose up -d` (or anything that spawns a service and returns) is the
// common case: the shell exits, its detached descendants keep running, and the
// dependency becomes ready via the check. The group kill on cancellation targets
// only processes still in the start command's own group -- i.e. a helper that is
// STILL running when the resolution is torn down -- and never the detached
// effects. prox does not own or tear down external resources a start command
// brought up; stopping the backing service (a container, a system daemon) is the
// operator's responsibility.
type StartRunner interface {
	Run(ctx context.Context, name, cmd string) error
}

// Resolver owns one node per dependency and drives its readiness resolution
// (plan 013 D2). See the Demand/Reset contract at the top of this file.
type Resolver struct {
	deps map[string]domain.DependencyConfig

	prober      Prober
	startRunner StartRunner
	clk         Clock
	log         LogFunc
	attemptCap  time.Duration

	mu      sync.Mutex
	nodes   map[string]*depNode
	nextGen map[string]uint64
	closed  bool

	// testAfterResolve, when non-nil, is invoked in run() AFTER resolve returns
	// its verdict but BEFORE it is published via finish. It is a test-only seam
	// used to deterministically force a Reset/Close into the publish-after-cancel
	// window; production leaves it nil.
	testAfterResolve func()
}

// depNode holds the per-dependency, per-generation resolution state. done is
// closed exactly once when the resolution goroutine finishes; joiners select on
// it. The state fields are guarded by mu for concurrent Snapshot reads.
type depNode struct {
	gen    uint64
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// canceled is set (under mu) by cancelNode when Reset/Close retires this
	// node. finish reads it under the same lock so a terminal outcome computed
	// by resolve is DEMOTED to canceled whenever the retirement won the race:
	// the demanders of a retired generation must never observe a stale
	// healthy/failed/warned verdict. See finish and the Demand/Reset contract.
	canceled     bool
	state        DepState
	lastErr      error
	startInvoked bool
	outcome      DepOutcome
}

// cancelNode marks the node canceled and cancels its resolution context in one
// step. The canceled flag is set under mu BEFORE the context cancel so that any
// finish that acquires mu after this point observes the retirement and publishes
// a canceled outcome. Ordering the flag first is what makes publication and
// retirement atomic with respect to each other.
func (n *depNode) cancelNode() {
	n.mu.Lock()
	n.canceled = true
	n.mu.Unlock()
	n.cancel()
}

// ResolverOption customizes a Resolver's seams (tests inject fakes; the
// supervisor keeps the real defaults).
type ResolverOption func(*Resolver)

// WithProber overrides the readiness prober.
func WithProber(p Prober) ResolverOption { return func(r *Resolver) { r.prober = p } }

// WithStartRunner overrides the start: command runner.
func WithStartRunner(s StartRunner) ResolverOption { return func(r *Resolver) { r.startRunner = s } }

// WithClock overrides the time seam.
func WithClock(c Clock) ResolverOption { return func(r *Resolver) { r.clk = c } }

// WithAttemptCap overrides the per-attempt deadline cap (default depAttemptCap).
func WithAttemptCap(d time.Duration) ResolverOption { return func(r *Resolver) { r.attemptCap = d } }

// NewResolver builds a Resolver for the given dependencies. cwd is the working
// directory for start: commands and cmd: checks (the supervisor's config dir);
// envOverlay is the process-style environment overlay (the top-level env_file
// merge) applied over os.Environ(), exactly as ManagedProcess builds a process's
// environment (see internal/config.LoadProcessEnv + runner.go). log routes
// dependency output/events to the system log; a nil log is a no-op.
//
// By default it wires the real check runners and start executor (deps_check.go)
// and the real clock; options replace any of those seams for tests.
func NewResolver(deps map[string]domain.DependencyConfig, cwd string, envOverlay map[string]string, log LogFunc, opts ...ResolverOption) *Resolver {
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	// Build the full environment once, mirroring ExecRunner.Start: os.Environ()
	// as the base with the overlay appended so later keys win.
	fullEnv := os.Environ()
	for k, v := range envOverlay {
		fullEnv = append(fullEnv, k+"="+v)
	}
	r := &Resolver{
		deps:        deps,
		prober:      newExecProber(cwd, fullEnv),
		startRunner: newExecStartRunner(cwd, fullEnv, log),
		clk:         realClock{},
		log:         log,
		attemptCap:  depAttemptCap,
		nodes:       make(map[string]*depNode),
		nextGen:     make(map[string]uint64),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Demand starts (or joins) the resolution of dependency name and returns its
// outcome. See the Demand/Reset contract at the top of this file for the precise
// single-flight, cancellation, and generation semantics.
func (r *Resolver) Demand(ctx context.Context, name string) DepOutcome {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return DepOutcome{State: DepStateCanceled, Err: context.Canceled}
	}
	cfg, ok := r.deps[name]
	if !ok {
		r.mu.Unlock()
		// Unknown dependency: a programming error in the caller (C3 only demands
		// declared dependencies). Report it as a failure rather than blocking.
		return DepOutcome{State: DepStateFailed, Err: fmt.Errorf("unknown dependency %q", name)}
	}
	node := r.nodes[name]
	if node == nil {
		node = r.startResolutionLocked(name, cfg)
	}
	done := node.done
	r.mu.Unlock()

	select {
	case <-done:
		return node.snapshotOutcome()
	case <-ctx.Done():
		// Only THIS caller's wait ended; the shared resolution continues.
		return DepOutcome{State: DepStateCanceled, Err: ctx.Err()}
	}
}

// startResolutionLocked creates a fresh node for the current generation and
// launches its resolution goroutine. Caller holds r.mu.
func (r *Resolver) startResolutionLocked(name string, cfg domain.DependencyConfig) *depNode {
	// The resolution root is Background, not any caller's ctx: the resolution is
	// shared and its lifetime is governed by Reset/Close (node.cancel), not by
	// whichever caller happened to start it.
	ctx, cancel := context.WithCancel(context.Background())
	node := &depNode{
		gen:    r.nextGen[name],
		cancel: cancel,
		done:   make(chan struct{}),
		state:  DepStatePending,
	}
	r.nodes[name] = node
	go r.run(ctx, name, cfg, node)
	return node
}

// run executes the resolution and publishes the terminal outcome to node.
// Publication (finish) is atomic with retirement: if the node was canceled by a
// Reset/Close that raced resolve's return, finish demotes the outcome to
// canceled so demanders never observe a stale pre-cancel verdict.
func (r *Resolver) run(ctx context.Context, name string, cfg domain.DependencyConfig, node *depNode) {
	defer close(node.done)
	outcome := r.resolve(ctx, name, cfg, node)
	if r.testAfterResolve != nil {
		r.testAfterResolve()
	}
	node.finish(outcome)
}

// resolve is the readiness state machine (plan 013 D2).
//
// Flow: run the initial check once. If it passes, healthy -- and the start:
// command is NEVER run (test-pinned both ways). Otherwise, if a start: command
// is configured, launch it ONCE in the background (its own process group; killed
// when this resolution returns) and then poll the check every interval until it
// passes or the overall budget is exhausted. With no start: command, just poll.
// The budget (check timeout) is a single window that includes the start
// command's execution time.
//
// Budget/boundary semantics (documented + tested): the overall deadline gates
// when an attempt may START, and each attempt is additionally bounded by
// min(attemptCap, remaining). After the initial check, the loop waits one
// interval (capped so it never oversleeps past the deadline) and then, as long
// as the deadline has NOT yet passed, dispatches another check. A check whose
// success is observed strictly before the deadline WINS (healthy); the loop
// reports exhaustion (failed/warned) only once an interval wait completes at or
// after the deadline with no successful check. Equivalently: a probe still
// running when the deadline arrives is canceled and loses; a probe that returns
// success before the deadline wins even if it was the last one dispatched.
func (r *Resolver) resolve(ctx context.Context, name string, cfg domain.DependencyConfig, node *depNode) DepOutcome {
	check := cfg.Check
	timeout := check.Timeout
	if timeout <= 0 {
		timeout = constants.DefaultDependencyCheckTimeout
	}
	interval := check.Interval
	if interval <= 0 {
		interval = constants.DefaultDependencyCheckInterval
	}
	deadline := r.clk.Now().Add(timeout)

	// Initial one-shot check. Guard the bound: if the budget somehow expired
	// between deadline creation and here, attemptFor returns <= 0; dispatching
	// then would arm no per-attempt timer and let a blocking probe run forever, so
	// skip the dispatch and go terminal instead (Fix 4).
	node.setState(DepStateChecking)
	if bound := r.attemptFor(deadline); bound > 0 {
		if outcome, done := r.doAttempt(ctx, check, node, bound, deadline); done {
			return outcome
		}
	} else {
		return r.terminalOrCanceled(ctx, cfg, node)
	}

	// The initial check failed. Launch the start: command ONCE (never when the
	// initial check passed). It runs concurrently with polling in its own
	// process group; a non-zero exit is logged and polling continues. The
	// deferred cancel + wait guarantees the start group is killed (group kill,
	// SIGKILL after grace, in the runner) when this resolution returns for any
	// reason -- success, budget exhaustion, or cancellation.
	if cfg.Start != "" {
		node.setState(DepStateStarting)
		node.markStartInvoked()
		startCtx, startCancel := context.WithCancel(ctx)
		startDone := make(chan struct{})
		go func() {
			defer close(startDone)
			// Log a genuine start failure (launch error or non-zero exit) but NOT
			// a cancellation: when this resolution ends it cancels startCtx to kill
			// the start group, and the runner then returns ctx.Err() -- that is our
			// own teardown, not a failure worth logging. Deciding on the RETURNED
			// error (rather than a live startCtx.Err() read) avoids racing the
			// deferred startCancel() below.
			if err := r.startRunner.Run(startCtx, name, cfg.Start); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				r.log("dep:%s start command failed: %v", name, err)
			}
		}()
		defer func() {
			startCancel()
			<-startDone
		}()
	}

	// Poll the check on the interval until it passes or the budget is spent.
	node.setState(DepStatePolling)
	for {
		// Cancellation is preferred over any terminal verdict: a canceled
		// dependency must not be reported failed/warned (Fix 2).
		if ctx.Err() != nil {
			return canceledOutcome(ctx)
		}
		rem := deadline.Sub(r.clk.Now())
		if rem <= 0 {
			return r.terminalOrCanceled(ctx, cfg, node)
		}
		wait := interval
		if wait > rem {
			wait = rem
		}
		t := r.clk.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return canceledOutcome(ctx)
		case <-t.C():
		}
		// The select can pick the timer even when ctx is also done; re-check and
		// prefer cancellation before any terminal path (Fix 2).
		if ctx.Err() != nil {
			return canceledOutcome(ctx)
		}
		// Dispatch another attempt only while the deadline has not passed. If the
		// interval wait consumed the last of the budget, stop without dispatching.
		if attemptCap := r.attemptFor(deadline); attemptCap > 0 {
			if outcome, done := r.doAttempt(ctx, check, node, attemptCap, deadline); done {
				return outcome
			}
		} else {
			return r.terminalOrCanceled(ctx, cfg, node)
		}
	}
}

// terminalOrCanceled resolves an exhausted budget, but prefers a canceled
// outcome whenever the resolution context is (or has just become) done: a
// canceled dependency is never counted as a readiness failure (Fix 2).
func (r *Resolver) terminalOrCanceled(ctx context.Context, cfg domain.DependencyConfig, node *depNode) DepOutcome {
	if ctx.Err() != nil {
		return canceledOutcome(ctx)
	}
	return r.terminal(cfg, node)
}

// attemptFor returns the per-attempt deadline cap for a probe dispatched now:
// min(attemptCap, remaining budget). A value <= 0 means the budget is spent.
func (r *Resolver) attemptFor(deadline time.Time) time.Duration {
	rem := deadline.Sub(r.clk.Now())
	if rem <= 0 {
		return 0
	}
	if rem < r.attemptCap {
		return rem
	}
	return r.attemptCap
}

// doAttempt runs one probe bounded by bound. It returns (outcome, true) when the
// resolution should terminate now -- either healthy (the check passed) or
// canceled (ctx died mid-attempt). It returns (_, false) when the probe failed,
// or when a success cannot be accepted because it was observed at/after the
// deadline, so polling should continue (the loop then terminates on budget).
//
// Success acceptance is gated twice (Fix 3): cancellation is checked FIRST so a
// probe that returns nil concurrently with Reset/Close yields canceled, not
// healthy; and a nil result is only a win when observed STRICTLY before the
// deadline per the injected clock, upholding the boundary contract even if the
// clock advanced to/past the deadline while the probe was in flight.
func (r *Resolver) doAttempt(ctx context.Context, check domain.DependencyCheck, node *depNode, bound time.Duration, deadline time.Time) (DepOutcome, bool) {
	err := r.attempt(ctx, check, bound)
	if ctx.Err() != nil {
		return canceledOutcome(ctx), true
	}
	if err == nil {
		if r.clk.Now().Before(deadline) {
			return DepOutcome{State: DepStateHealthy}, true
		}
		// Success arrived at/after the deadline: it does not win. Continue so the
		// loop terminates on the exhausted budget with the last recorded error.
		return DepOutcome{}, false
	}
	node.recordErr(err)
	return DepOutcome{}, false
}

// attempt runs a single probe under a per-attempt deadline enforced via the
// clock seam: a stoppable timer cancels the attempt's context after d, so a hung
// probe is aborted without consuming the overall budget. The timer is always
// stopped when the attempt returns.
func (r *Resolver) attempt(ctx context.Context, check domain.DependencyCheck, d time.Duration) error {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	if d > 0 {
		t := r.clk.NewTimer(d)
		defer t.Stop()
		go func() {
			select {
			case <-t.C():
				cancel()
			case <-actx.Done():
			}
		}()
	}
	return r.prober.Probe(actx, check)
}

// terminal resolves an exhausted budget to warned (on_failure=warn: proceed) or
// failed (on_failure=fail: abort), logging the warn case.
func (r *Resolver) terminal(cfg domain.DependencyConfig, node *depNode) DepOutcome {
	err := node.lastError()
	if err == nil {
		// Reachable when the budget was already spent before the first check ran
		// (Fix 4) so no probe error was recorded. Supply a meaningful error.
		err = fmt.Errorf("dependency %q not ready within %s budget", cfg.Name, cfg.Check.Timeout)
	}
	if cfg.OnFailure == domain.FailurePolicyWarn {
		node.setState(DepStateWarned)
		r.log("dep:%s not ready within %s budget; proceeding (on_failure: warn): %v", cfg.Name, cfg.Check.Timeout, err)
		return DepOutcome{State: DepStateWarned, Err: err}
	}
	node.setState(DepStateFailed)
	return DepOutcome{State: DepStateFailed, Err: err}
}

// canceledOutcome builds a canceled outcome carrying ctx's error.
func canceledOutcome(ctx context.Context) DepOutcome {
	return DepOutcome{State: DepStateCanceled, Err: ctx.Err()}
}

// Reset invalidates the current generation for name (plan 013 D2): it cancels
// any in-flight resolution (its demanders receive a canceled outcome) and drops
// the cached node so the NEXT Demand re-resolves fresh under a new generation.
// It is the "NextGeneration" primitive C3 calls after a stop/restart/reload.
func (r *Resolver) Reset(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if node := r.nodes[name]; node != nil {
		node.cancelNode()
		delete(r.nodes, name)
	}
	r.nextGen[name]++
}

// ResetIfGeneration is a generation-conditional Reset (plan 013 D4). It behaves
// exactly like Reset(name) -- cancels the current resolution and drops the cached
// node so the next Demand re-resolves under a fresh generation -- but ONLY when
// the current node's generation still equals gen. It returns whether it acted.
//
// This closes a coordinator race: a caller that Snapshots a node in a terminal
// (e.g. failed) state at generation N and later decides to Reset it must not
// clobber a generation N+1 that some OTHER demander started resolving in the
// meantime -- doing so would cancel that fresh resolution and strand its
// dependents. Passing the snapshot's Gen makes the Reset a no-op once the node
// has moved on. (Reset(name) stays the unconditional primitive.)
func (r *Resolver) ResetIfGeneration(name string, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	node := r.nodes[name]
	if node == nil || node.gen != gen {
		return false
	}
	node.cancelNode()
	delete(r.nodes, name)
	r.nextGen[name]++
	return true
}

// Redefine refreshes the stored definition for a dependency (plan 013 D6). When
// the new config DIFFERS from the current one it swaps it in AND Resets the
// dependency (cancels any in-flight resolution, drops the cached node, bumps the
// generation) so the next Demand re-resolves against the fresh definition; it
// returns whether it changed anything. An unchanged definition is a no-op, so a
// restart that did not touch a dependency still returns its cached healthy
// outcome without a re-probe. A name not previously known is added (a new
// dependency introduced by a reload). Generation-safe: the Reset mirrors the
// public primitive, so a concurrent demander of a retired generation observes a
// canceled outcome exactly as after any Reset. Closed resolvers are a no-op.
func (r *Resolver) Redefine(name string, cfg domain.DependencyConfig) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if existing, ok := r.deps[name]; ok && reflect.DeepEqual(existing, cfg) {
		return false
	}
	r.deps[name] = cfg
	if node := r.nodes[name]; node != nil {
		node.cancelNode()
		delete(r.nodes, name)
	}
	r.nextGen[name]++
	return true
}

// ApplyGraph atomically installs a reload's fresh dependency set (plan 013 D6;
// D5 atomicity fix). Under a SINGLE r.mu critical section it redefines every
// changed or new dependency and drops every dependency whose name is absent from
// fresh, so a concurrent StatusSnapshots (or any other reader) never observes a
// half-applied mixture of the old and new sets -- the flaw of the old
// redefine-each-then-prune sequence, where every step took its own lock. Per-
// entry semantics match Redefine: an unchanged definition is left untouched (its
// cached outcome survives a re-probe), a changed or new one is Reset (in-flight
// resolution canceled, cached node dropped, generation bumped) so the next Demand
// re-resolves it, and a removed/migrated name is Reset and forgotten. A
// concurrent demander of any retired generation observes a canceled outcome
// exactly as after any Reset. Closed resolvers are a no-op.
func (r *Resolver) ApplyGraph(fresh map[string]domain.DependencyConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	// Redefine changed definitions and add new ones.
	for name, cfg := range fresh {
		if existing, ok := r.deps[name]; ok && reflect.DeepEqual(existing, cfg) {
			continue
		}
		r.deps[name] = cfg
		if node := r.nodes[name]; node != nil {
			node.cancelNode()
			delete(r.nodes, name)
		}
		r.nextGen[name]++
	}
	// Retain only the fresh set: forget any dependency the reload removed or
	// migrated to a task.
	for name := range r.deps {
		if _, ok := fresh[name]; ok {
			continue
		}
		delete(r.deps, name)
		if node := r.nodes[name]; node != nil {
			node.cancelNode()
			delete(r.nodes, name)
		}
		r.nextGen[name]++
	}
}

// Close cancels every in-flight resolution (supervisor shutdown). After Close,
// Demand returns a canceled outcome without starting new work.
func (r *Resolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, node := range r.nodes {
		node.cancelNode()
	}
}

// Snapshot returns the current node's state for name, or (_, false) if no
// resolution has been started this generation.
func (r *Resolver) Snapshot(name string) (DepSnapshot, bool) {
	r.mu.Lock()
	node := r.nodes[name]
	r.mu.Unlock()
	if node == nil {
		return DepSnapshot{}, false
	}
	return node.snapshot(name), true
}

// Snapshots returns a snapshot of every dependency with an active resolution
// node this generation (for status surfacing in C5).
func (r *Resolver) Snapshots() []DepSnapshot {
	r.mu.Lock()
	out := make([]DepSnapshot, 0, len(r.nodes))
	for name, node := range r.nodes {
		out = append(out, node.snapshot(name))
	}
	r.mu.Unlock()
	return out
}

// DepStatus combines a configured dependency's stored check definition with its
// current resolution snapshot, for status surfacing (plan 013 D5). Unlike
// DepSnapshot (active nodes only), it is produced for EVERY configured
// dependency: one without an active node this generation reports DepStatePending
// with no error and StartInvoked=false.
type DepStatus struct {
	Name         string
	Check        domain.DependencyCheck
	State        DepState
	LastError    string
	StartInvoked bool
}

// StatusSnapshots returns a DepStatus for every configured dependency, sorted by
// name for stable rendering (plan 013 D5). It reads the stored definitions and
// the per-node snapshots together under r.mu so the check summary and live state
// never race a reload -- ApplyGraph installs the fresh set under the same single
// lock, so a reader observes either the whole old set or the whole new one, never
// a mixture. A dependency with no node this generation is reported as pending.
func (r *Resolver) StatusSnapshots() []DepStatus {
	r.mu.Lock()
	out := make([]DepStatus, 0, len(r.deps))
	for name, cfg := range r.deps {
		ds := DepStatus{Name: name, Check: cfg.Check, State: DepStatePending}
		if node := r.nodes[name]; node != nil {
			snap := node.snapshot(name)
			ds.State = snap.State
			ds.LastError = snap.LastError
			ds.StartInvoked = snap.StartInvoked
		}
		out = append(out, ds)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- depNode state helpers (guarded by node.mu) ---

func (n *depNode) setState(s DepState) {
	n.mu.Lock()
	n.state = s
	n.mu.Unlock()
}

func (n *depNode) recordErr(err error) {
	n.mu.Lock()
	n.lastErr = err
	n.mu.Unlock()
}

func (n *depNode) lastError() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastErr
}

func (n *depNode) markStartInvoked() {
	n.mu.Lock()
	n.startInvoked = true
	n.mu.Unlock()
}

func (n *depNode) finish(o DepOutcome) {
	n.mu.Lock()
	defer n.mu.Unlock()
	// If this generation was retired while resolve was computing (or returning)
	// its verdict, demote the published outcome to canceled: a retired node's
	// demanders must not see a stale healthy/failed/warned result. This check and
	// the cancelNode flag-set are both under n.mu, so exactly one of them wins and
	// the published outcome is always consistent with whether retirement happened
	// before publication.
	if n.canceled && o.State != DepStateCanceled {
		o = DepOutcome{State: DepStateCanceled, Err: context.Canceled}
	}
	n.state = o.State
	if o.Err != nil {
		n.lastErr = o.Err
	}
	n.outcome = o
}

func (n *depNode) snapshotOutcome() DepOutcome {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.outcome
}

func (n *depNode) snapshot(name string) DepSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	var lastErr string
	if n.lastErr != nil {
		lastErr = n.lastErr.Error()
	}
	return DepSnapshot{
		Name:         name,
		State:        n.state,
		LastError:    lastErr,
		StartInvoked: n.startInvoked,
		Gen:          n.gen,
	}
}

// --- real clock ---

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }
