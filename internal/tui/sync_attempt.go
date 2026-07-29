package tui

import (
	"context"
	"sync/atomic"
)

// syncFetch is the machinery both attach-stream sync protocols share: the
// once-per-attempt REST fetch (the requests snapshot, the logs backfill) that a
// connect-and-consume attempt runs alongside its SSE read loop, plus the
// teardown that keeps it inside its attempt.
//
// It must run on its own goroutine: the reader goroutine is busy reading, and
// the live events the protocol buffers meanwhile would deadlock if the fetch
// ran on it. It must be joined before the attempt returns, which is what makes
// calling markSynced from it safe despite stream.Config.Attempt's "call it from
// the attempt itself" rule: that rule exists to stop a STRAGGLING goroutine
// from flipping a LATER attempt's state, and a fetch that cannot straggle
// cannot break it (the loop's epoch guard is the second line of defense).
type syncFetch struct {
	cancel  context.CancelFunc
	done    chan struct{}
	started atomic.Bool

	// err is written by the fetch goroutine and read only after join.
	err error
}

// newSyncFetch derives the per-attempt context the fetch and the stream both
// run under; cancelling it ends both. The caller must `defer f.join()`
// immediately, which is what cancels it.
func newSyncFetch(ctx context.Context) (context.Context, *syncFetch) {
	attemptCtx, cancel := context.WithCancel(ctx)
	return attemptCtx, &syncFetch{cancel: cancel, done: make(chan struct{})}
}

// start is the body of an attempt's onConnect hook: it runs the fetch once, on
// its own goroutine. A failure aborts the attempt so the loop reconnects and
// the next attempt re-syncs. A second call is a no-op — a client that fired
// onConnect twice would otherwise start a second goroutine and double-close
// done.
func (f *syncFetch) start(run func() error) {
	if !f.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(f.done)
		f.err = run()
		if f.err != nil {
			f.abort()
		}
	}()
}

// abort cancels the attempt context. It is how a buffering onEvent hook — which
// has no other way to end an attempt — reports that this attempt cannot
// complete, and how the fetch gives up on its own.
func (f *syncFetch) abort() { f.cancel() }

// join ends the fetch goroutine and waits for it, so it can never outlive its
// attempt, and makes err readable. Idempotent (cancel is; a closed channel
// stays receivable), so it is safe both as the deferred backstop and as the
// explicit join that must precede reading err.
func (f *syncFetch) join() {
	f.cancel()
	if f.started.Load() {
		<-f.done
	}
}

// attemptError picks which of an attempt's three possible causes to report,
// once it has been joined. The sync-protocol cause (abortErr, then the fetch's
// own failure) wins: aborting a sync cancels the attempt context, which makes
// the stream consumer report a generic cancellation that would otherwise hide
// why the attempt is recycling. A cancellation of the PARENT context (the TUI
// quitting) is not ours to reinterpret — the loop ends on it regardless, so
// streamErr is reported as-is.
func (f *syncFetch) attemptError(ctx context.Context, abortErr, streamErr error) error {
	if ctx.Err() == nil {
		if abortErr != nil {
			return abortErr
		}
		if f.err != nil {
			return f.err
		}
	}
	return streamErr
}
