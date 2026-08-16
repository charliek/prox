package cli

import (
	"sync"

	"github.com/charliek/prox/internal/domain"
)

// shutdownCoordinator mediates between the two independent sides of a daemon
// shutdown:
//
//   - the trigger side (Ctrl-C, POST /shutdown, or a hand-quit --tui) requests
//     shutdown via Trigger(); and
//   - the outcome side (the foreground exit contract today; C5's wait=true
//     handlers next) reads the process-stop verdict via Done()/Outcome() after
//     performShutdown latches it with Complete().
//
// Both transitions are guarded by sync.Once. Trigger() fixes the latent
// double-close panic that a second POST /shutdown used to cause (shutdownFn was
// a bare close(shutdownCh)). Complete() is a latched broadcast: it stores the
// outcome and closes done exactly once, so any number of waiters -- zero, one,
// or many concurrent wait=true handlers -- all read the same stored outcome
// after <-Done(); the shutdown sequence never blocks on a consumer (a Ctrl-C
// with no waiter is just store + close).
type shutdownCoordinator struct {
	triggerOnce sync.Once
	triggerCh   chan struct{}

	completeOnce sync.Once
	doneCh       chan struct{}
	outcome      *domain.ProcessStopError
}

// newShutdownCoordinator returns a coordinator with fresh, un-fired trigger and
// done channels.
func newShutdownCoordinator() *shutdownCoordinator {
	return &shutdownCoordinator{
		triggerCh: make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Trigger requests shutdown. It is idempotent: a second call (a duplicate POST
// /shutdown, or a signal racing an API trigger) is a no-op rather than a
// double-close panic.
func (c *shutdownCoordinator) Trigger() {
	c.TriggerWith(nil)
}

// TriggerWith requests shutdown and, if THIS call is the one that actually
// requested it, runs onWin first -- inside the same sync.Once, so it happens
// before triggerCh closes and cannot interleave with another trigger.
//
// It exists because one trigger source needs to record WHY: the dead-stack
// watcher (dead_stack.go, #96) latches a verdict that turns into `prox up`'s
// non-zero exit code. "Latch, then trigger" is not enough on its own -- a
// Ctrl-C landing between the two would make an intentional shutdown exit
// non-zero (codex review finding). Deciding the reason inside triggerOnce
// makes the exit code follow whoever genuinely ended the session: if a signal
// or POST /shutdown got here first, onWin never runs and the session exits 0.
func (c *shutdownCoordinator) TriggerWith(onWin func()) {
	c.triggerOnce.Do(func() {
		if onWin != nil {
			onWin()
		}
		close(c.triggerCh)
	})
}

// TriggerCh is closed once shutdown has been requested. runUp selects on it (in
// both the non-TUI wait loop and, via a quit goroutine, the TUI path).
func (c *shutdownCoordinator) TriggerCh() <-chan struct{} { return c.triggerCh }

// Complete latches the shutdown outcome and unblocks every waiter. The first
// call wins; later calls are no-ops. The store happens-before the close, so a
// waiter that reads Outcome() after <-Done() observes the stored value without
// any additional synchronization.
func (c *shutdownCoordinator) Complete(outcome *domain.ProcessStopError) {
	c.completeOnce.Do(func() {
		c.outcome = outcome
		close(c.doneCh)
	})
}

// Done is closed once Complete has latched the outcome.
func (c *shutdownCoordinator) Done() <-chan struct{} { return c.doneCh }

// Outcome returns the latched process-stop verdict (nil = clean stop). It is
// ONLY valid after <-Done() has observed the close: called earlier it returns a
// nil that is indistinguishable from a clean outcome, so every consumer MUST
// select on Done() (or receive from it) before reading Outcome().
func (c *shutdownCoordinator) Outcome() *domain.ProcessStopError { return c.outcome }
