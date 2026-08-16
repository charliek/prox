package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShutdownCoordinator_DoubleTriggerNoPanic pins the fix for the latent
// double-close panic: a second Trigger() (a duplicate POST /shutdown, or a
// signal racing an API trigger) must be a no-op, not a close-of-closed-channel
// panic.
func TestShutdownCoordinator_DoubleTriggerNoPanic(t *testing.T) {
	c := newShutdownCoordinator()
	assert.NotPanics(t, func() {
		c.Trigger()
		c.Trigger()
		c.Trigger()
	})

	select {
	case <-c.TriggerCh():
	default:
		t.Fatal("TriggerCh must be closed after Trigger")
	}
}

// TestShutdownCoordinator_WiredShutdownFnDoublePost simulates the daemon wiring:
// the API handler is handed the coordinator and calls Trigger() on POST
// /shutdown. Two POST /shutdown calls invoke it twice; the daemon must not panic
// (regression for the bare close(shutdownCh) bug).
func TestShutdownCoordinator_WiredShutdownFnDoublePost(t *testing.T) {
	c := newShutdownCoordinator()
	shutdownFn := c.Trigger // exactly what api.NewHandlers calls via ShutdownController

	assert.NotPanics(t, func() {
		shutdownFn()
		shutdownFn()
	})
}

// TestShutdownCoordinator_CompleteBroadcastsSameOutcome: Complete once, then many
// waiters each read the identical latched outcome after <-Done().
func TestShutdownCoordinator_CompleteBroadcastsSameOutcome(t *testing.T) {
	c := newShutdownCoordinator()
	want := &domain.ProcessStopError{
		Failures: []domain.ProcessStopFailure{{Name: "web", Err: domain.ErrProcessGroupNotReaped}},
	}

	const waiters = 8
	var wg sync.WaitGroup
	got := make([]*domain.ProcessStopError, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-c.Done()
			got[i] = c.Outcome()
		}(i)
	}

	c.Complete(want)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiters did not observe Complete")
	}

	for i := 0; i < waiters; i++ {
		assert.Same(t, want, got[i], "every waiter must read the same latched outcome")
	}
}

// TestShutdownCoordinator_CompleteOnceLatchesFirst: a second Complete is a no-op;
// the first outcome stays latched.
func TestShutdownCoordinator_CompleteOnceLatchesFirst(t *testing.T) {
	c := newShutdownCoordinator()
	first := &domain.ProcessStopError{
		Failures: []domain.ProcessStopFailure{{Name: "a", Err: domain.ErrProcessGroupNotReaped}},
	}
	second := &domain.ProcessStopError{
		Failures: []domain.ProcessStopFailure{{Name: "b", Err: domain.ErrProcessGroupNotReaped}},
	}

	c.Complete(first)
	c.Complete(second)

	<-c.Done()
	assert.Same(t, first, c.Outcome(), "the first Complete must win")
}

// TestShutdownCoordinator_CompleteNoWaitersDoesNotBlock: the shutdown sequence
// never blocks on a consumer — Complete with zero waiters is store + close.
func TestShutdownCoordinator_CompleteNoWaitersDoesNotBlock(t *testing.T) {
	c := newShutdownCoordinator()
	done := make(chan struct{})
	go func() {
		c.Complete(nil) // clean outcome, no waiters
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete blocked with no waiters")
	}

	select {
	case <-c.Done():
	default:
		t.Fatal("Done must be closed after Complete")
	}
	require.Nil(t, c.Outcome(), "a clean outcome is nil")
}

// TestShutdownCoordinator_OutcomeBeforeDone documents the contract: Outcome() is
// only valid after <-Done(); read before completion it is a nil that a consumer
// must not mistake for a clean verdict — which is exactly why every consumer
// gates on Done() first.
func TestShutdownCoordinator_OutcomeBeforeDone(t *testing.T) {
	c := newShutdownCoordinator()

	select {
	case <-c.Done():
		t.Fatal("Done must not be closed before Complete")
	default:
	}
	assert.Nil(t, c.Outcome(), "Outcome before Done is nil (indistinguishable from clean; do not read it)")
}

// TestShutdownCoordinator_TriggerWithOnlyTheWinnerRecordsItsReason pins the
// arbitration that decides `prox up`'s exit code when a dead stack and a
// deliberate shutdown race (plan 028 C3, codex review finding).
//
// The dead-stack watcher records WHY it ended the session; a Ctrl-C records
// nothing, because an intentional shutdown exits 0. "Latch, then trigger" left
// a window in which a Ctrl-C arriving between the two still exited non-zero, so
// the reason is now decided INSIDE triggerOnce: whoever actually requests the
// shutdown is the only one that gets to say why.
func TestShutdownCoordinator_TriggerWithOnlyTheWinnerRecordsItsReason(t *testing.T) {
	t.Run("a shutdown that lost records nothing", func(t *testing.T) {
		c := newShutdownCoordinator()
		ran := false

		c.Trigger() // the signal handler gets there first
		c.TriggerWith(func() { ran = true })

		assert.False(t, ran,
			"the watcher lost the trigger, so it must not record a reason -- "+
				"an intentional shutdown exits 0")
		assertClosed(t, c.TriggerCh())
	})

	t.Run("a shutdown that won records its reason", func(t *testing.T) {
		c := newShutdownCoordinator()
		ran := false

		c.TriggerWith(func() { ran = true })
		c.Trigger() // a signal arriving afterwards changes nothing

		assert.True(t, ran, "the watcher won the trigger, so its reason stands")
		assertClosed(t, c.TriggerCh())
	})

	t.Run("under contention exactly one caller wins", func(t *testing.T) {
		// The callback must also run BEFORE the channel closes: a reader woken
		// by TriggerCh reads the latched reason immediately, and would race a
		// callback that ran after the close.
		const racers = 64
		for range 32 {
			c := newShutdownCoordinator()
			var mu sync.Mutex
			wins := 0
			closedBeforeCallback := false

			var wg sync.WaitGroup
			wg.Add(racers)
			for i := range racers {
				go func() {
					defer wg.Done()
					if i%2 == 0 {
						c.Trigger()
						return
					}
					c.TriggerWith(func() {
						mu.Lock()
						defer mu.Unlock()
						wins++
						select {
						case <-c.TriggerCh():
							closedBeforeCallback = true
						default:
						}
					})
				}()
			}
			wg.Wait()

			mu.Lock()
			require.LessOrEqual(t, wins, 1, "more than one caller recorded a reason")
			assert.False(t, closedBeforeCallback,
				"the reason must be recorded before TriggerCh closes")
			mu.Unlock()
			assertClosed(t, c.TriggerCh())
		}
	})
}

// assertClosed fails unless ch is already closed.
func assertClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("trigger channel was never closed")
	}
}
