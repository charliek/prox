package cli

import (
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// This file implements the dead-stack watcher (plan 028 C3, #96).
//
// A foreground `prox up` used to wait on the shutdown coordinator's trigger
// channel and nothing else. That channel is closed by a signal, by POST
// /shutdown, and by a TUI quit -- never by a process dying. So the single most
// common first-run mistake, a typo in `cmd:`, left the user holding a terminal
// that supervised nothing at all, with no output, until they thought to press
// Ctrl-C. The session is now torn down (and `prox up` exits non-zero, printing
// the same crashed summary `prox up -d` prints) once every process is dead and
// at least one of them died a FAILURE.
//
// TWO GATES, AND BOTH MATTER (see deadStack):
//
//   - "no live process" rather than "any crashed process": a partial crash --
//     one of three processes down, two still serving -- is emphatically not a
//     reason to tear a developer's stack down.
//   - "at least one terminal failure" rather than "nothing live": a config of
//     run-to-completion tasks that all finished is SUCCESS, and a user who
//     stopped every process through the API expressed an INTENT. Neither may
//     end the session, and both are states where nothing is live.
//
// WHERE IT DOES NOT RUN: the detached daemon child. `--detach` short-circuits
// TUI resolution to plain mode, so the daemon child runs the very same wait
// block a foreground `prox up` does -- and a watcher there would kill the
// daemon moments after `prox up -d` told the user "The daemon is still running;
// stop it with 'prox down'", taking the API and the logs of the crash with it.
// `prox up -d` reports crashes at settle time (process_settle.go) and exits
// non-zero on its own; the daemon staying up is the contract. See the
// daemon.IsDaemonChild() gate at the call site in up.go.

// deadStackSource is the slice of *supervisor.Supervisor the watcher needs: a
// snapshot reader and the change bus. An interface rather than the concrete
// type so the unit tests can drive the state machine deterministically, without
// launching processes.
type deadStackSource interface {
	Processes() []domain.ProcessInfo
	Subscribe() (string, <-chan struct{})
	Unsubscribe(id string)
}

// deadStack reports whether procs describe a session with nothing left to
// supervise and a failure to answer for.
//
// It is pure, and it is the whole decision -- read the two-gate note at the top
// of this file before widening either half. IsLive/IsTerminalFailure are the
// domain predicates (domain/process.go); IsStopped is NOT usable here, because
// it also covers `completed`, which is a task's terminal SUCCESS.
func deadStack(procs []domain.ProcessInfo) bool {
	if len(procs) == 0 {
		// No processes at all is not a dead stack: an empty config has nothing
		// to fail, and tearing the session down would break `prox up` as a way
		// to run the proxy and API alone.
		return false
	}
	failed := false
	for _, p := range procs {
		if p.State.IsLive() {
			return false
		}
		if p.State.IsTerminalFailure() {
			failed = true
		}
	}
	return failed
}

// settleProcessesFromInfo projects the supervisor's own ProcessInfo onto the
// settle check's wire-shaped view.
//
// It exists so this path reuses evaluateProcessSettle and settleVerdict.writeTo
// rather than growing a second implementation of "which of these failed, and
// how do I say so": `prox up`, `prox up -d`, `prox start` and `prox restart`
// then report a crash in one voice, with one exit-code vocabulary.
func settleProcessesFromInfo(procs []domain.ProcessInfo) []settleProcess {
	out := make([]settleProcess, 0, len(procs))
	for _, p := range procs {
		out = append(out, settleProcess{
			Name:      p.Name,
			Status:    string(p.State),
			BlockedOn: p.BlockedOn,
		})
	}
	return out
}

// deadStackWatcher is one running watcher: the goroutine, the stop handle, and
// the verdict it latched (if it fired).
type deadStackWatcher struct {
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

	// mu guards verdict. The goroutine writes it before Trigger, and runUp
	// reads it after the wait loop; stop() joins the goroutine, so the mutex is
	// belt and braces rather than the only ordering -- but it makes reading the
	// latch safe from anywhere, which is what keeps a future caller honest.
	mu      sync.Mutex
	verdict *settleVerdict
}

// startDeadStackWatcher subscribes to sup's change bus and starts watching, at
// the production timings.
//
// trigger is the coordinator's TriggerWith: it requests shutdown and runs the
// callback ONLY if this call is the one that actually requested it, so the
// watcher's verdict and the shutdown it causes are decided together (see run).
// triggered is the coordinator's channel, which tells the watcher a shutdown is
// already under way and it has nothing left to decide.
func startDeadStackWatcher(sup deadStackSource, trigger func(onWin func()), triggered <-chan struct{}) *deadStackWatcher {
	return startDeadStackWatcherWithTiming(sup, trigger, triggered, processSettleTimeout, processSettlePollInterval)
}

// startDeadStackWatcherWithTiming is startDeadStackWatcher with the window and
// poll interval supplied, so tests can run the same state machine in
// milliseconds.
func startDeadStackWatcherWithTiming(
	sup deadStackSource,
	trigger func(onWin func()),
	triggered <-chan struct{},
	window, interval time.Duration,
) *deadStackWatcher {
	w := &deadStackWatcher{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	// Subscribe BEFORE the goroutine starts, so that a caller which stops the
	// watcher immediately still unsubscribes exactly one subscription, and so
	// that no transition between the caller's decision to watch and the
	// goroutine's first snapshot can be missed.
	id, wake := sup.Subscribe()
	go func() {
		defer close(w.doneCh)
		defer sup.Unsubscribe(id)
		w.run(sup, wake, trigger, triggered, window, interval)
	}()
	return w
}

// run is the watcher's state machine.
//
// It evaluates a snapshot FIRST, before waiting on any wake. Subscribe is a
// change latch, not a replay stream, so a stack that was already dead before
// the watcher existed -- which is exactly the shape of the mid-run TUI-failure
// fallback, and of a stack that dies during a slow startup -- would otherwise
// wait forever for a transition that already happened.
func (w *deadStackWatcher) run(
	sup deadStackSource,
	wake <-chan struct{},
	trigger func(onWin func()),
	triggered <-chan struct{},
	window, interval time.Duration,
) {
	for {
		if v, ok := w.confirmDeadStack(sup, triggered, window, interval); ok {
			// The verdict is built from the CONFIRMING snapshot, because
			// shutdown drives every process to stopped: read afterwards, the
			// evidence of what failed is gone.
			//
			// It is latched INSIDE the trigger's sync.Once, not before it. The
			// window between confirming and triggering is small but real, and a
			// Ctrl-C arriving inside it would otherwise make an intentional
			// shutdown exit non-zero (codex review finding). Handing the latch
			// to TriggerWith makes the two decisions one atomic step: the
			// verdict is recorded if and only if this watcher is what actually
			// ended the session.
			trigger(func() { w.latch(v) })
			return
		}
		select {
		case _, open := <-wake:
			if !open {
				// CloseEvents (Supervisor.Stop's deferred call) closed the bus:
				// the supervisor is going away. That is "stop watching", never
				// "fire" -- the session is already ending, and the exit code it
				// ends with is not this watcher's to decide.
				return
			}
		case <-triggered:
			// Shutdown is already under way (Ctrl-C, POST /shutdown, a TUI
			// quit). Everything is about to become stopped; an intentional
			// shutdown latches nothing and exits 0.
			return
		case <-w.stopCh:
			return
		}
	}
}

// confirmDeadStack answers "is the stack dead, and did it stay dead".
//
// It returns immediately when the current snapshot is not a dead stack.
// Otherwise it POLLS for the whole window rather than trusting further wakes,
// and that is deliberate: the change bus is a capacity-1 coalescing latch, so a
// transition that arrives while a wake is already pending is dropped, and a
// purely wake-driven design could both miss a recovery and fire on a stale
// sample after a `restart` had already cleared the condition.
func (w *deadStackWatcher) confirmDeadStack(
	sup deadStackSource,
	triggered <-chan struct{},
	window, interval time.Duration,
) (settleVerdict, bool) {
	procs := sup.Processes()
	if !deadStack(procs) {
		return settleVerdict{}, false
	}

	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Held for the full window, on every observation in it.
			return evaluateProcessSettle(settleProcessesFromInfo(procs)), true
		}
		sleep := interval
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-time.After(sleep):
		case <-triggered:
			return settleVerdict{}, false
		case <-w.stopCh:
			return settleVerdict{}, false
		}

		procs = sup.Processes()
		if !deadStack(procs) {
			// A `prox start`/`restart` (or a slow process finally coming up)
			// broke the condition: abandon the window entirely and go back to
			// waiting on wakes. Nothing partial is remembered.
			return settleVerdict{}, false
		}
	}
}

// latch stores the verdict the watcher fired on.
func (w *deadStackWatcher) latch(v settleVerdict) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.verdict = &v
}

// latchedVerdict reports the verdict the watcher fired on, and whether it fired
// at all. A false second return means the session ended for some other reason
// (a signal, an API shutdown, a TUI quit), which must not change the exit code.
func (w *deadStackWatcher) latchedVerdict() (settleVerdict, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.verdict == nil {
		return settleVerdict{}, false
	}
	return *w.verdict, true
}

// stop ends the watch and waits for the goroutine to unsubscribe and return.
// Idempotent, and safe to call after the watcher has already fired.
//
// The join matters: callers read latchedVerdict straight after stop, and the
// watcher must be finished deciding by then.
func (w *deadStackWatcher) stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

// deadStackHint is the remediation line printed under the crashed/blocked
// summary when the watcher ends a foreground session. It answers the question
// the user actually has at that moment -- "why did my terminal come back?" --
// and mirrors the shape of the hint `prox up -d` prints for the same failure.
const deadStackHint = "No processes are left running, so prox exited. Fix the command above and run 'prox up' again."
