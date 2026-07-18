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
