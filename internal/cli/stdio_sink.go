package cli

import (
	"bytes"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

// stdioSink is the switchable io.Writer that keeps unintended diagnostics off
// the TUI's alt screen.
//
// WHY THIS EXISTS. `prox up --tui` runs bubbletea's alt screen (tui/app.go)
// in the SAME process as the supervisor, the API server, the proxy and the
// proxyd client. Those subsystems write to the process's stderr while the alt
// screen owns the terminal, and every such byte lands in the middle of a
// rendered frame:
//
//   - 17 stdlib log.Printf sites in internal/ are reachable mid-session — the
//     shared-daemon lost/reconnect/heal lines (cli/proxy_runtime.go, the common
//     configuration), the API's three SSE streams (api/sse.go — the TUI's own
//     streams), api/handlers.go, logs/subscription.go's overflow line, the
//     proxy's request-subscription overflow line (fired on a DETACHED
//     goroutine, so it can interleave mid-frame), proxy/capture.go and
//     proxyd/forwarder.go.
//   - proxy/proxy.go's ErrorHandler logs through a slog handler bound to
//     os.Stderr once per FAILED UPSTREAM REQUEST — precisely the
//     "backend has not bound its port yet" window.
//   - No http.Server in this repo sets ErrorLog, so net/http's TLS handshake
//     errors and its handler-panic dumps go to the stdlib logger too.
//
// Routing the stdlib logger (log.SetOutput) and the standalone proxy's slog
// handler through one sink turns all of that from destructive terminal noise
// into ordinary log lines the TUI can render.
//
// SCOPE OF THE GUARANTEE. This contains the AUDITED, UNINTENDED writers inside
// this process. It is not universal stdio interception. Explicitly NOT covered:
// direct fd-2 writes, runtime panic/fatal tracebacks outside net/http's handler
// recovery, cgo output, and stdio inherited by children (mkcert is the one
// stdio-inheriting child, and it is startup-only).
//
// THREE TARGETS, ONE TYPE:
//
//	lazy stderr  default, plain mode, and everything outside the TUI window.
//	             Synchronous pass-through; bytes and ordering are preserved.
//	log manager  owner-mode TUI session. ASYNCHRONOUS — see the protocol below.
//	buffer       attach-mode TUI session, which owns no log manager: accumulate
//	             and flush to stderr once the TUI has released the screen.
//
// THE LAZY STDERR TARGET RESOLVES os.Stderr AT WRITE TIME, NEVER AT
// CONSTRUCTION. daemon.SetupLogging reassigns the os.Stderr VARIABLE
// (daemon/daemon.go) from runUp, and a `-d` child's real fd 2 is /dev/null.
// A sink that snapshotted os.Stderr in its constructor would send the child's
// diagnostics to the pre-redirect descriptor — i.e. to /dev/null — instead of
// to .prox/prox.log.
//
// THE MANAGER TARGET MUST BE ASYNCHRONOUS. This is the single most important
// invariant in this file. logs.Manager.Write holds ingestMu across Broadcast
// (logs/manager.go), and Subscription.Send calls log.Printf from INSIDE that
// critical section when a subscriber overflows (logs/subscription.go). A
// synchronous adapter would therefore re-enter Manager.Write on the same
// goroutine — sink.Write → Manager.Write → Broadcast → Send → log.Printf →
// sink.Write → Manager.Write — and deadlock on a non-reentrant sync.Mutex the
// first time any subscriber falls behind. Enqueue-and-drain breaks the cycle:
// the re-entrant write only appends to a channel and returns.
//
// THE DRAIN GOROUTINE HAS THE SYMMETRIC HAZARD, and it is why the drain holds
// NOTHING while it writes. Its own logMgr.Write can overflow a subscription →
// log.Printf → sink.Write on the drain goroutine itself. If the drain held the
// sink's lock across logMgr.Write, that re-entrant Write would block on a
// recursive acquisition — fatal even for an RWMutex read lock, because Go
// blocks a recursive RLock whenever a writer (a concurrent route change) is already
// queued. So the drain goroutine captures its write function once, at session
// construction, and never touches the sink again.
//
// THE FEEDBACK LOOP IS BOUNDED — AND THE DEPENDENCY IS EXTERNAL. A routed line
// becomes a Manager.Write, which can overflow a subscription, which log.Printf's,
// which produces another routed line. That terminates ONLY because
// SubscriptionManager.Broadcast drops the overflowing subscription on its FIRST
// overflow (logs/subscription.go), so the second lap finds no subscriber left to
// overflow. If Broadcast is ever changed to keep an overflowing subscription
// alive — retrying, rate-limiting, "warn once per N" — this loop becomes
// infinite. Any such change must add its own break.
//
// CONCURRENCY PROTOCOL (all six points are load-bearing):
//
//  1. The drain snapshots its target and holds no sink lock while writing (above).
//  2. Stderr-target writes stay synchronous: bytes and ordering are preserved.
//  3. A route change stops new manager enqueues ATOMICALLY (under the same mutex the
//     enqueue takes) before it drains, so "accepted" has a precise meaning.
//  4. The drain barrier is a SEPARATE channel, not an entry in the data queue:
//     a barrier queued behind full data would be dropped by point 5 and the
//     drain would never be told to finish.
//  5. Data records are drop-on-full and NEVER block. A full queue means the log
//     pipeline is already in trouble, and blocking is exactly what the async
//     design exists to avoid. Drops are counted and surfaced (Drops).
//  6. Partial lines are flushed on every route change, the drain goroutine is
//     JOINED before the route change returns, and installStdioSink restores the previous
//     log.Writer()/log.Flags().
type stdioSink struct {
	// routeMu serializes WHOLE route transitions (swap + barrier + join), and
	// is always taken OUTSIDE mu. Point 6 is a claim about the caller's
	// timeline — "when a route change returns, no earlier drain is still
	// running" — and mu alone cannot carry it: the join must happen with mu
	// released (see route), so without this two concurrent route changes can
	// interleave as swap(A→B), swap(B→nil), join(B), return, ... join(A). The
	// second caller would return while A's drain is still writing into a
	// manager it is about to close (codex review finding).
	//
	// Deadlock-free by construction: the drain goroutine can re-enter the sink
	// through logs.Manager's overflow log.Printf, but that path takes mu only.
	// Nothing reachable from a drain ever asks for routeMu.
	routeMu sync.Mutex

	// mu guards target, session, partial and buf. It is never held across a
	// call that can log — that is the whole point of the drain goroutine.
	mu      sync.Mutex
	target  stdioSinkTarget
	session *stdioDrainSession

	// partial holds the tail of a write that did not end in a newline. The
	// stdlib logger emits exactly one line per call, but an http.Server error
	// dump or a slog record with an embedded newline does not, and a writer is
	// free to split a line across calls.
	partial []byte

	// buf accumulates raw bytes for the deferred-buffer target. Bytes, not
	// lines: the buffer is replayed verbatim to stderr, so there is nothing to
	// gain from splitting it.
	buf bytes.Buffer

	drops atomic.Uint64
}

// stdioSinkTarget names where the sink currently sends what it receives.
type stdioSinkTarget uint8

const (
	// stdioTargetStderr is the default: synchronous pass-through to whatever
	// os.Stderr refers to AT WRITE TIME.
	stdioTargetStderr stdioSinkTarget = iota
	// stdioTargetManager routes to a logs.Manager through the drain goroutine.
	stdioTargetManager
	// stdioTargetBuffer accumulates until the sink is routed back to stderr.
	stdioTargetBuffer
)

const (
	// stdioSinkQueueDepth is the per-session record queue. Deep enough that a
	// burst of diagnostics during one render survives, shallow enough that a
	// wedged drain cannot pin unbounded memory. Overflow is a counted drop
	// (protocol point 5), never a block.
	stdioSinkQueueDepth = 1024

	// stdioSinkBufferCap bounds the deferred-buffer target. The attach-mode
	// writers it exists for emit a handful of parse warnings per session; a
	// pathological producer is truncated rather than allowed to grow the heap
	// for the length of a TUI session. Truncated bytes are counted as drops.
	stdioSinkBufferCap = 256 << 10

	// stdioSinkProcess is the log-entry process name these lines carry. It
	// matches Supervisor.SystemLog so redirected diagnostics read as system
	// output everywhere — the TUI log pane, `prox logs`, the daemon log. The
	// TUI does not special-case "system" and its process panel is fed only by
	// /processes/stream, so this cannot manufacture a phantom process row.
	stdioSinkProcess = "system"
)

// stdioRecord is one routed line plus the instant it was accepted. The
// timestamp is taken at ENQUEUE time, not at drain time, so queueing delay
// cannot reorder these entries against the process logs they interleave with.
type stdioRecord struct {
	ts   time.Time
	line string
}

// stdioDrainSession is one manager-target routing epoch: a queue, the goroutine
// draining it, a barrier and a join point. A fresh session is created per
// route change so a finished drain can never observe the next target.
//
// write is captured once, at construction, and is the ONLY thing the drain
// goroutine touches — see the sink's doc comment on why the drain must not hold
// a sink lock.
type stdioDrainSession struct {
	write func(stdioRecord)
	ch    chan stdioRecord
	// stop is the non-droppable barrier (protocol point 4): closing it tells
	// the drain "no further sends are possible, finish the queue and exit".
	stop chan struct{}
	// done is closed by the drain as it returns; route joins on it.
	done chan struct{}
}

// newStdioSink returns a sink writing to os.Stderr, resolved lazily on every
// write. It starts no goroutine: the drain exists only while a manager target
// is routed.
func newStdioSink() *stdioSink {
	return &stdioSink{target: stdioTargetStderr}
}

// Write implements io.Writer. It is the stdlib logger's output and the
// standalone proxy's slog destination, so it is called from arbitrary
// goroutines — including, on the manager target, from inside logs.Manager's
// ingest lock and from the drain goroutine itself.
//
// It never blocks on anything other than the sink's own mutex, and that mutex
// is only ever held across non-blocking work (a channel select with a default,
// a buffer append) or a synchronous stderr write.
func (s *stdioSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.target {
	case stdioTargetManager:
		s.enqueueLinesLocked(p)
		return len(p), nil
	case stdioTargetBuffer:
		s.bufferLocked(p)
		return len(p), nil
	default:
		// os.Stderr is read HERE, not captured at construction — see the type
		// comment on daemon.SetupLogging.
		return os.Stderr.Write(p)
	}
}

// Drops reports how many records this sink has discarded across all routing
// epochs: queue overflows on the manager target and truncation on the buffer
// target. It is exposed so a session can tell the user its log pane was
// incomplete rather than leaving the gap silent.
func (s *stdioSink) Drops() uint64 {
	return s.drops.Load()
}

// RouteToLogManager points the sink at mgr for the duration of an owner-mode
// TUI session. Redirected lines become "system" entries the TUI renders in its
// log pane instead of bytes that corrupt its frame.
//
// A nil manager is treated as "restore the default", so a caller that lost its
// manager degrades to visible stderr rather than to a silent hole.
func (s *stdioSink) RouteToLogManager(mgr *logs.Manager) {
	if mgr == nil {
		s.RestoreStderr()
		return
	}
	s.routeAsync(stdioSinkQueueDepth, func(rec stdioRecord) {
		mgr.Write(domain.LogEntry{
			Timestamp: rec.ts,
			Process:   stdioSinkProcess,
			// These lines were headed for stderr and are diagnostics, so they
			// keep the stderr stream tag; consumers that colour or filter by
			// stream then treat them the way they treat any process's stderr.
			Stream: domain.StreamStderr,
			Line:   rec.line,
		})
	})
}

// RouteToBuffer accumulates everything written until the sink is routed back to
// stderr, which replays the accumulation verbatim. It is the attach-mode target:
// `prox attach` runs the same TUI but owns no log manager, so its only choices
// are "corrupt the frame now" or "print after the screen is released". Buffering
// costs nothing and loses nothing.
func (s *stdioSink) RouteToBuffer() {
	s.route(stdioTargetBuffer, nil)
}

// RestoreStderr returns the sink to the lazy-stderr default, replaying anything
// the buffer target accumulated. It is the counterpart to both RouteToLogManager
// and RouteToBuffer.
//
// It is SYNCHRONOUS by design — barrier, drain, join — and callers must invoke
// it explicitly rather than through a function-scoped defer. In runUp a defer
// would fire after performShutdown has closed the log manager, so every
// teardown-era diagnostic (notably the shared-daemon "lost connection" line,
// the highest-frequency site in the audit) would be written into a manager with
// no subscribers that is about to be discarded, while the terminal showed
// nothing. A defer is worth keeping only as a panic/early-return backstop.
func (s *stdioSink) RestoreStderr() {
	s.route(stdioTargetStderr, nil)
}

// routeAsync installs a manager-style target with an explicit queue depth and
// write function. RouteToLogManager is the only production caller; the depth
// and the function are parameters so tests can drive the drop-on-full path
// deterministically with a blocked drain.
func (s *stdioSink) routeAsync(depth int, write func(stdioRecord)) {
	s.route(stdioTargetManager, &stdioDrainSession{
		write: write,
		ch:    make(chan stdioRecord, depth),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	})
}

// route performs the switch. The ordering here IS the protocol:
//
//	under the lock: flush the partial line into the OUTGOING target, replay any
//	                buffered bytes, swap the target and install the new session
//	                — so the instant the lock is released no further record can
//	                reach the old session (points 3 and 6);
//	after the lock: close the barrier and join the old drain (points 4 and 6).
//
// The join MUST happen with the lock released: the drain it is waiting on can
// re-enter Write through logs.Manager's overflow log.Printf, and that Write
// needs the very mutex a lock-holding join would still own.
//
// Because of that released window, routeMu serializes the whole transition, so
// concurrent route changes cannot leave an older drain running past a later
// caller's return — see routeMu's doc comment for the interleaving this closes.
func (s *stdioSink) route(target stdioSinkTarget, session *stdioDrainSession) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	s.mu.Lock()
	s.flushPartialLocked()
	if target != stdioTargetBuffer {
		s.replayBufferLocked()
	}
	prev := s.session
	s.target = target
	s.session = session
	if session != nil {
		go session.drain()
	}
	s.mu.Unlock()

	if prev != nil {
		close(prev.stop)
		<-prev.done
	}
}

// enqueueLinesLocked splits p into complete lines and hands each one to the
// drain as its own record, carrying any incomplete tail forward to the next
// write. One line == one log entry: the TUI's log pane renders per-entry, so a
// multi-line http.Server dump arriving as a single Write must not become a
// single unreadable entry.
func (s *stdioSink) enqueueLinesLocked(p []byte) {
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		line := rest[:i]
		if len(s.partial) > 0 {
			// The concatenation is converted to a string immediately, so the
			// scratch slice cannot alias the next write's data.
			s.emitLocked(string(append(s.partial, line...)))
			s.partial = nil
		} else {
			s.emitLocked(string(line))
		}
		rest = rest[i+1:]
	}
	if len(rest) > 0 {
		s.partial = append(s.partial, rest...)
	}
}

// emitLocked queues one record, dropping it rather than blocking when the queue
// is full (protocol point 5). A nil session means the target was swapped out
// from under an in-flight write; that record is counted as a drop rather than
// vanishing unaccounted for.
func (s *stdioSink) emitLocked(line string) {
	if s.session == nil {
		s.drops.Add(1)
		return
	}
	select {
	case s.session.ch <- stdioRecord{ts: time.Now(), line: line}:
	default:
		s.drops.Add(1)
	}
}

// bufferLocked appends to the deferred buffer, truncating at the cap. A
// truncated write is counted as one drop so the session can report that the
// replay is incomplete.
func (s *stdioSink) bufferLocked(p []byte) {
	if s.buf.Len() >= stdioSinkBufferCap {
		s.drops.Add(1)
		return
	}
	if remaining := stdioSinkBufferCap - s.buf.Len(); len(p) > remaining {
		s.buf.Write(p[:remaining])
		s.drops.Add(1)
		return
	}
	s.buf.Write(p)
}

// flushPartialLocked pushes a trailing incomplete line into the target being
// left behind (protocol point 6). Without it, a diagnostic that arrived without
// its newline — the last thing a crashing writer manages to emit — would be
// silently discarded by the target swap.
func (s *stdioSink) flushPartialLocked() {
	if len(s.partial) == 0 {
		return
	}
	line := s.partial
	s.partial = nil

	switch s.target {
	case stdioTargetManager:
		s.emitLocked(string(line))
	case stdioTargetBuffer:
		s.buf.Write(line)
		s.buf.WriteByte('\n')
	default:
		_, _ = os.Stderr.Write(append(line, '\n'))
	}
}

// replayBufferLocked writes everything the deferred-buffer target accumulated
// to stderr, resolved now — which is after the TUI has released the screen, the
// whole point of that target.
func (s *stdioSink) replayBufferLocked() {
	if s.buf.Len() == 0 {
		return
	}
	_, _ = os.Stderr.Write(s.buf.Bytes())
	// Assigned rather than Reset: Reset keeps the backing array, and the sink
	// outlives the session, so a buffer that grew toward stdioSinkBufferCap
	// would be pinned for the rest of the process.
	s.buf = bytes.Buffer{}
}

// drain is the manager target's single writer goroutine. It holds no sink lock
// while calling write, because write can log — see the sink's doc comment.
//
// Once stop is closed no further send is possible (route swapped the target
// under the mutex before closing it), so the post-barrier non-blocking loop
// empties the queue exhaustively: everything the sink ACCEPTED is written
// before the route change returns.
func (sess *stdioDrainSession) drain() {
	defer close(sess.done)
	for {
		select {
		case rec := <-sess.ch:
			sess.write(rec)
		case <-sess.stop:
			for {
				select {
				case rec := <-sess.ch:
					sess.write(rec)
				default:
					return
				}
			}
		}
	}
}

// installStdioSink points the stdlib logger at sink and returns the restore
// function (protocol point 6).
//
// log.SetFlags(0) strips the "2026/08/15 14:03:22 " prefix for three reasons:
// the TUI's log pane renders its own timestamp column, prox's own log lines
// carry no stdlib prefix, and a prefix-free line keeps a diagnostic
// byte-identical whether it reaches the terminal directly or through this sink.
//
// The stdlib logger is process-global. prox's CLI is single-purpose — one
// command per process — so a global swap is appropriate here; the previous
// writer and flags are captured and restored regardless.
func installStdioSink(sink *stdioSink) func() {
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(sink)
	log.SetFlags(0)
	return func() {
		sink.RestoreStderr()
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}
