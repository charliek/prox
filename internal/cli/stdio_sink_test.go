package cli

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redirectStderr points the os.Stderr VARIABLE at a temp file for the duration
// of the test and returns a reader for what was written to it. The sink
// resolves os.Stderr on every write, so this both keeps test diagnostics out of
// the test log and lets a test assert on what reached the fallback writer.
func redirectStderr(t *testing.T) func() string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	require.NoError(t, err)

	prev := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = prev
		_ = f.Close()
	})

	return func() string {
		t.Helper()
		data, readErr := os.ReadFile(f.Name())
		require.NoError(t, readErr)
		return string(data)
	}
}

// managerLines returns every line the manager holds, oldest first.
func managerLines(t *testing.T, mgr *logs.Manager) []string {
	t.Helper()

	entries, _, err := mgr.Query(domain.LogFilter{}, 0)
	require.NoError(t, err)

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Line)
	}
	return lines
}

// installSinkForTest points the stdlib logger at sink and registers a BOUNDED
// restore.
//
// The restore path (RestoreStderr: barrier, drain, join) is itself blocked by
// the very deadlocks this file exists to catch, so an unbounded cleanup would
// turn a clean assertion failure into a suite-wide hang at the go test timeout.
//
// The timeout branch deliberately does NOT try to force the stdlib logger back
// to os.Stderr: a goroutine wedged inside log.Printf holds log.Logger's own
// mutex, so log.SetOutput would block too. In that state the failure is already
// reported and the process is unrecoverable by design.
func installSinkForTest(t *testing.T, sink *stdioSink) {
	t.Helper()

	restore := installStdioSink(sink)
	t.Cleanup(func() {
		runWithin(t, 2*time.Second, "restoring the stdlib logger", restore)
	})
}

// runWithin fails the test if fn has not returned within d. Every deadlock this
// file guards against manifests as a hang, and a hang inside `go test` is a
// ten-minute timeout with an unreadable dump rather than a failing assertion.
func runWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not complete within %s — deadlock", what, d)
	}
}

// TestStdioSink_LazyStderrResolution pins the reason newStdioSink must not
// snapshot os.Stderr: daemon.SetupLogging reassigns that variable AFTER the
// process has started, and a snapshotting sink would keep writing to the
// pre-redirect descriptor — /dev/null in a `prox up -d` child.
func TestStdioSink_LazyStderrResolution(t *testing.T) {
	sink := newStdioSink()

	// Constructed BEFORE the redirect, exactly as runUp constructs it before
	// (in the daemon child's case, around) the logging swap.
	readStderr := redirectStderr(t)

	n, err := sink.Write([]byte("after the redirect\n"))
	require.NoError(t, err)
	assert.Equal(t, len("after the redirect\n"), n)

	assert.Equal(t, "after the redirect\n", readStderr(),
		"the sink must resolve os.Stderr at write time, not at construction")
}

// TestStdioSink_TargetMatrix covers the three targets: synchronous stderr
// pass-through, the asynchronous log-manager target (including line splitting
// and partial-line flushing), and the deferred buffer used by attach mode.
func TestStdioSink_TargetMatrix(t *testing.T) {
	t.Run("stderr target passes bytes through verbatim", func(t *testing.T) {
		readStderr := redirectStderr(t)
		sink := newStdioSink()

		_, err := sink.Write([]byte("one\ntwo\n"))
		require.NoError(t, err)
		_, err = sink.Write([]byte("no trailing newline"))
		require.NoError(t, err)

		assert.Equal(t, "one\ntwo\nno trailing newline", readStderr(),
			"the stderr target must not reframe or buffer anything")
	})

	t.Run("manager target splits lines and flushes the partial tail", func(t *testing.T) {
		readStderr := redirectStderr(t)
		mgr := logs.NewManager(logs.DefaultManagerConfig())
		defer mgr.Close()

		sink := newStdioSink()
		sink.RouteToLogManager(mgr)

		// Two lines in one write, one line split across two writes, and a
		// trailing fragment with no newline at all.
		_, err := sink.Write([]byte("alpha\nbeta\n"))
		require.NoError(t, err)
		_, err = sink.Write([]byte("gam"))
		require.NoError(t, err)
		_, err = sink.Write([]byte("ma\ndelta"))
		require.NoError(t, err)

		// RestoreStderr is the barrier: everything accepted above is written before
		// it returns, including the partial "delta".
		sink.RestoreStderr()

		assert.Equal(t, []string{"alpha", "beta", "gamma", "delta"}, managerLines(t, mgr))
		assert.Empty(t, readStderr(), "nothing may reach the terminal while a TUI owns the screen")
		assert.Zero(t, sink.Drops())
	})

	t.Run("manager entries are tagged as system stderr", func(t *testing.T) {
		mgr := logs.NewManager(logs.DefaultManagerConfig())
		defer mgr.Close()

		sink := newStdioSink()
		sink.RouteToLogManager(mgr)
		_, err := sink.Write([]byte("prox: lost connection to shared proxy daemon\n"))
		require.NoError(t, err)
		sink.RestoreStderr()

		entries, _, err := mgr.Query(domain.LogFilter{}, 0)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, stdioSinkProcess, entries[0].Process)
		assert.Equal(t, domain.StreamStderr, entries[0].Stream)
		assert.False(t, entries[0].Timestamp.IsZero(), "records carry their enqueue time")
	})

	t.Run("buffer target holds everything until the route back to stderr", func(t *testing.T) {
		readStderr := redirectStderr(t)
		sink := newStdioSink()

		sink.RouteToBuffer()
		_, err := sink.Write([]byte("warning: failed to parse SSE event\n"))
		require.NoError(t, err)
		assert.Empty(t, readStderr(), "the attach TUI still owns the screen")

		sink.RestoreStderr()
		assert.Equal(t, "warning: failed to parse SSE event\n", readStderr(),
			"the buffer must replay verbatim once the screen is released")
	})

	t.Run("a routed log.Printf reaches the manager and not stderr", func(t *testing.T) {
		readStderr := redirectStderr(t)
		mgr := logs.NewManager(logs.DefaultManagerConfig())
		defer mgr.Close()

		sink := newStdioSink()
		installSinkForTest(t, sink)

		sink.RouteToLogManager(mgr)
		log.Printf("SSE write error (client likely disconnected): %v", fmt.Errorf("boom"))
		sink.RestoreStderr()

		assert.Equal(t,
			[]string{"SSE write error (client likely disconnected): boom"},
			managerLines(t, mgr),
			"log.SetFlags(0) must leave the line free of the stdlib date/time prefix")
		assert.Empty(t, readStderr())
	})
}

// TestStdioSink_DropOnFull pins protocol point 5: a full queue drops records
// and counts them, and Write never blocks. The drain is deliberately wedged so
// the queue fills deterministically rather than by racing the drain.
func TestStdioSink_DropOnFull(t *testing.T) {
	sink := newStdioSink()

	release := make(chan struct{})
	var written atomic.Int64
	sink.routeAsync(2, func(stdioRecord) {
		<-release
		written.Add(1)
	})

	const lines = 200
	runWithin(t, 5*time.Second, "writes against a wedged drain", func() {
		for i := range lines {
			_, err := fmt.Fprintf(sink, "line %d\n", i)
			require.NoError(t, err)
		}
	})

	assert.Positive(t, sink.Drops(), "a full queue must drop and count, never block")

	close(release)
	runWithin(t, 5*time.Second, "RestoreStderr after unwedging the drain", func() {
		sink.RestoreStderr()
	})
	assert.Equal(t, int64(lines)-int64(sink.Drops()), written.Load(),
		"everything not dropped must be written before RestoreStderr returns")
}

// TestStdioSink_RestoreStderrDrainsAndJoins pins protocol point 6: RestoreStderr writes
// everything it accepted and joins the drain goroutine before returning, so no
// entry can land in a log manager that the caller is about to close.
func TestStdioSink_RestoreStderrDrainsAndJoins(t *testing.T) {
	readStderr := redirectStderr(t)
	mgr := logs.NewManager(logs.DefaultManagerConfig())
	defer mgr.Close()

	sink := newStdioSink()
	sink.RouteToLogManager(mgr)

	sink.mu.Lock()
	session := sink.session
	sink.mu.Unlock()
	require.NotNil(t, session)

	const lines = 500
	for i := range lines {
		_, err := fmt.Fprintf(sink, "entry %d\n", i)
		require.NoError(t, err)
	}

	runWithin(t, 5*time.Second, "RestoreStderr()", func() { sink.RestoreStderr() })

	assert.Zero(t, sink.Drops(), "a live drain must keep up with a 500-line burst")
	assert.Len(t, managerLines(t, mgr), lines)

	select {
	case <-session.done:
	default:
		t.Fatal("RestoreStderr returned without joining the drain goroutine")
	}

	sink.mu.Lock()
	assert.Nil(t, sink.session, "the manager session must be cleared on restore")
	sink.mu.Unlock()

	// Back on the synchronous fallback.
	_, err := sink.Write([]byte("after restore\n"))
	require.NoError(t, err)
	assert.Equal(t, "after restore\n", readStderr())
}

// TestStdioSink_OverflowDeadlockRegression is the reason the manager target is
// asynchronous.
//
// logs.Manager.Write holds ingestMu across Broadcast, and Subscription.Send
// calls log.Printf from inside that critical section when a subscriber
// overflows. With the stdlib logger routed at the same manager, a SYNCHRONOUS
// adapter re-enters Manager.Write on the writing goroutine and deadlocks on a
// non-reentrant mutex. This test drives exactly that path — an undrained
// subscription with a one-entry channel — and fails on a hang rather than
// hanging the suite.
func TestStdioSink_OverflowDeadlockRegression(t *testing.T) {
	readStderr := redirectStderr(t)

	// Deliberately NOT closed: if this test ever fails, a goroutine is wedged
	// inside Broadcast holding the subscription read lock, and Manager.Close
	// would then block forever — turning one failing test into a hung binary.
	mgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100, SubscriptionBuffer: 1})

	// A subscriber nobody drains: the second entry overflows it, and the
	// overflow is reported with log.Printf from inside Manager.Write.
	_, ch, err := mgr.Subscribe(domain.LogFilter{})
	require.NoError(t, err)
	require.NotNil(t, ch)

	sink := newStdioSink()
	installSinkForTest(t, sink)
	sink.RouteToLogManager(mgr)

	runWithin(t, 10*time.Second, "overflow while routed at the same manager", func() {
		for i := range 25 {
			mgr.Write(domain.LogEntry{
				Timestamp: time.Now(),
				Process:   "app",
				Stream:    domain.StreamStdout,
				Line:      fmt.Sprintf("noisy line %d", i),
			})
		}
		// The barrier also has to survive: it joins a drain goroutine that is
		// itself feeding the manager that is logging at the sink.
		sink.RestoreStderr()
	})

	// The feedback loop terminates because Broadcast drops the overflowing
	// subscription on its first overflow; if that contract ever changes, this
	// assertion is the canary alongside the timeout above.
	lines := managerLines(t, mgr)
	assert.Contains(t, fmt.Sprint(lines), "overflowed",
		"the overflow diagnostic must be captured, not lost")
	assert.Empty(t, readStderr(), "the overflow diagnostic must not reach the terminal")
}

// TestStdioSink_DrainReentrancyUnderConcurrentRouting pins protocol point 1 —
// the drain goroutine must hold no sink lock while writing.
//
// The drain's own logMgr.Write can overflow a subscription, whose log.Printf
// re-enters sink.Write on the drain goroutine. If the drain held the sink lock
// across that write, the re-entrant acquisition would deadlock — and with an
// RWMutex it would deadlock precisely when a route change is queued as a writer,
// since Go blocks recursive RLock in that situation. So this hammers writes,
// overflows and route changes concurrently. Run under -race.
func TestStdioSink_DrainReentrancyUnderConcurrentRouting(t *testing.T) {
	redirectStderr(t)

	// Deliberately NOT closed — see the regression test above.
	mgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200, SubscriptionBuffer: 1})

	sink := newStdioSink()
	installSinkForTest(t, sink)
	sink.RouteToLogManager(mgr)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writers: the 17 audited log.Printf sites, in miniature.
	for id := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				log.Printf("prox: writer %d diagnostic %d", id, n)
			}
		}()
	}

	// Overflow pressure: Broadcast drops a subscription on its first overflow,
	// so fresh undrained subscriptions keep the in-ingest log.Printf firing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := mgr.Subscribe(domain.LogFilter{}); err != nil {
				return
			}
			mgr.Write(domain.LogEntry{Timestamp: time.Now(), Process: "app", Stream: domain.StreamStdout, Line: "flood"})
			mgr.Write(domain.LogEntry{Timestamp: time.Now(), Process: "app", Stream: domain.StreamStdout, Line: "flood"})
		}
	}()

	// The route flipper is the writer that a naive RWMutex-based drain would
	// deadlock against, and it is what bounds the test: it runs a fixed number
	// of flips and then stops everyone else.
	//
	// Bounded by WORK, not by a wall-clock sleep. Every flip allocates a fresh
	// stdioSinkQueueDepth-deep queue plus a goroutine, so an unbounded flipper
	// spinning for a fixed duration churns hundreds of megabytes — which buys
	// nothing, because the race detector needs interleavings rather than volume.
	// It also makes the test's cost depend on how fast the machine is.
	const routeFlips = 500
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for range routeFlips {
			sink.RestoreStderr()
			sink.RouteToLogManager(mgr)
		}
	}()

	runWithin(t, 15*time.Second, "concurrent writers, overflows and route changes", func() {
		wg.Wait()
		sink.RestoreStderr()
	})
}

// TestInstallStdioSink_RestoresLogger pins the process-global swap: the stdlib
// logger writes to the sink with no date/time prefix while installed, and the
// previous writer and flags come back on restore.
// TestStdioSink_ConcurrentRouteChangesJoinEveryDrain pins protocol point 6 as a
// claim about the CALLER'S timeline: when a route change returns, no earlier
// drain may still be running.
//
// The sibling test above drives all its route changes from a single goroutine,
// which cannot expose this: route must release the sink lock before joining (a
// drain can re-enter Write), and that released window lets two concurrent route
// changes interleave as swap(A→B), swap(B→nil), join(B), return, ... join(A).
// The second caller then returns while A is still writing into a logs.Manager
// its caller is about to close — silently losing teardown-era diagnostics, the
// exact failure the explicit restore in runUp exists to prevent (codex review
// finding). routeMu closes it by serializing whole transitions.
//
// The injected writer is deliberately slow so an unjoined drain is still
// observably in flight when the assertion runs.
func TestStdioSink_ConcurrentRouteChangesJoinEveryDrain(t *testing.T) {
	redirectStderr(t)

	sink := newStdioSink()
	installSinkForTest(t, sink)

	var inFlight atomic.Int64
	slowWrite := func(stdioRecord) {
		inFlight.Add(1)
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
	}

	// Asserting on the END state cannot work: every session is joined
	// eventually, just possibly too late, so a post-storm check is zero either
	// way. The violation is an ORDERING one, so the assertion has to be made at
	// the instant ONE route change returns — hence a two-goroutine scenario
	// rather than a storm.
	const (
		attempts   = 20
		queued     = 40 // 40 * 2ms = ~80ms of drain work to be caught mid-flight
		queueDepth = 64
	)

	for attempt := range attempts {
		// Session A, loaded with enough queued work that an unjoined drain is
		// unmistakably still running.
		sink.routeAsync(queueDepth, slowWrite)
		for range queued {
			_, _ = sink.Write([]byte("diagnostic\n"))
		}

		// G1 installs session B, which makes it responsible for closing and
		// joining A. Its join is the slow part.
		swapped := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(swapped)
			// An empty session: B itself has nothing to drain, so anything still
			// in flight when the restore below returns can only be A.
			sink.routeAsync(queueDepth, slowWrite)
		}()
		<-swapped
		runtime.Gosched() // bias G1 into route() ahead of the restore below

		// This is the assertion point. Protocol point 6 says that when a route
		// change returns, no EARLIER drain is still running — including A, which
		// this goroutine never owned.
		sink.RestoreStderr()
		require.Zero(t, inFlight.Load(),
			"attempt %d: a drain was still writing after RestoreStderr returned — an older session outlived a later route change", attempt)

		wg.Wait()
		sink.RestoreStderr()
	}
}

func TestInstallStdioSink_RestoresLogger(t *testing.T) {
	prevOut, prevFlags := log.Writer(), log.Flags()

	sink := newStdioSink()
	restore := installStdioSink(sink)

	assert.Same(t, sink, log.Writer())
	assert.Equal(t, 0, log.Flags(), "the TUI renders its own timestamp column")

	restore()

	assert.Same(t, prevOut, log.Writer())
	assert.Equal(t, prevFlags, log.Flags())
}
