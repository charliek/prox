package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/supervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStopProcess is a minimal supervisor.Process for driving performShutdown
// through the real Supervisor without spawning OS processes. Two behaviours are
// modelled, mirroring the supervisor package's own fakes:
//
//   - survivor: leader exits on SIGTERM (so the run monitor finishes) but the
//     process group's first liveness probe reports gone and every later probe
//     reports alive, yielding a deterministic ErrProcessGroupNotReaped — the
//     verdict a real leaked grandchild produces.
//   - clean: the group dies on SIGTERM (liveness flips false, leader exits), so
//     Stop reaps it with no error.
type fakeStopProcess struct {
	pid      int
	survivor bool

	mu         sync.Mutex
	waitCh     chan struct{}
	waitClosed bool
	aliveCalls int
	alive      bool
}

func newFakeStopProcess(pid int, survivor bool) *fakeStopProcess {
	return &fakeStopProcess{pid: pid, survivor: survivor, waitCh: make(chan struct{}), alive: true}
}

func (p *fakeStopProcess) PID() int { return p.pid }

func (p *fakeStopProcess) PGID() int         { return p.pid }
func (p *fakeStopProcess) StartToken() int64 { return int64(p.pid) }

func (p *fakeStopProcess) Wait() error {
	<-p.waitCh
	return nil
}

func (p *fakeStopProcess) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM {
		// SIGTERM: the leader always exits so the run monitor completes promptly.
		p.exitLeader()
		if !p.survivor {
			p.mu.Lock()
			p.alive = false
			p.mu.Unlock()
		}
	}
	return nil
}

func (p *fakeStopProcess) exitLeader() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waitClosed {
		p.waitClosed = true
		close(p.waitCh)
	}
}

func (p *fakeStopProcess) GroupAlive() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := p.aliveCalls
	p.aliveCalls++
	if p.survivor {
		// Gone to the graceful poll (call 0), alive to the authoritative re-probe.
		return call >= 1, nil
	}
	return p.alive, nil
}

func (p *fakeStopProcess) Stdout() io.Reader { return strings.NewReader("") }
func (p *fakeStopProcess) Stderr() io.Reader { return strings.NewReader("") }

// fakeStopRunner hands out a pre-built fakeStopProcess per process name. The
// fakes map is populated once at construction and never mutated afterward, so
// concurrent reads from Start need no locking.
type fakeStopRunner struct {
	fakes map[string]*fakeStopProcess
}

func (r *fakeStopRunner) Start(_ context.Context, cfg domain.ProcessConfig, _ map[string]string) (supervisor.Process, error) {
	fp, ok := r.fakes[cfg.Name]
	if !ok {
		panic("fakeStopRunner: no fake for " + cfg.Name)
	}
	return fp, nil
}

// newStartedSupervisor builds and starts a Supervisor over the given named
// fakes, returning it alongside its log manager (so a test can pass logMgr into
// performShutdown and observe the flush/close stage). Closing the manager twice
// is safe, so the returned handle also carries a t.Cleanup close.
func newStartedSupervisor(t *testing.T, fakes map[string]*fakeStopProcess) (*supervisor.Supervisor, *logs.Manager) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 200})
	t.Cleanup(func() { logMgr.Close() })

	cfg := &config.Config{
		API:       config.APIConfig{Port: 5599, Host: "127.0.0.1"},
		Processes: make(map[string]config.ProcessConfig, len(fakes)),
	}
	for name := range fakes {
		cfg.Processes[name] = config.ProcessConfig{Cmd: "irrelevant", StopTimeout: "3s"}
	}
	sup := supervisor.New(cfg, logMgr, &fakeStopRunner{fakes: fakes}, supervisor.DefaultSupervisorConfig())
	_, err := sup.Start(context.Background())
	require.NoError(t, err)
	return sup, logMgr
}

// TestPerformShutdown_SurvivorNamesProcess: performShutdown over a supervisor
// with a leaked group returns a *ProcessStopError naming that process (the value
// runUp returns to make `prox up` exit 1), and latches it on the coordinator.
func TestPerformShutdown_SurvivorNamesProcess(t *testing.T) {
	sup, logMgr := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4101, true),
		"api": newFakeStopProcess(4102, false),
	})
	coord := newShutdownCoordinator()

	// API-server/proxy deps are nil — the helper unit test needs no sockets.
	outcome := performShutdown(shutdownDeps{
		sup:          sup,
		coordinator:  coord,
		logMgr:       logMgr,
		stageTimeout: teardownStageTimeout,
	})

	require.NotNil(t, outcome, "a surviving group must yield a non-nil outcome")
	require.Len(t, outcome.Failures, 1)
	assert.Equal(t, "web", outcome.Failures[0].Name)
	assert.ErrorIs(t, outcome, domain.ErrProcessGroupNotReaped)
	assert.Contains(t, outcome.Error(), "web", "the returned error must name the survivor")

	// The verdict is latched for waiters (C5's wait=true handlers).
	select {
	case <-coord.Done():
	default:
		t.Fatal("performShutdown must Complete the coordinator")
	}
	assert.Same(t, outcome, coord.Outcome(), "coordinator must latch the same outcome")

	// Fix 3 (survivor exit contract): runUp wraps the aggregate into one concise,
	// errors.Is/As-preserving summary line for the exit code. Pin that shape here.
	wrapped := fmt.Errorf("shutdown incomplete: %w", outcome)
	assert.ErrorIs(t, wrapped, domain.ErrProcessGroupNotReaped, "wrap must preserve errors.Is")
	var agg *domain.ProcessStopError
	assert.ErrorAs(t, wrapped, &agg, "wrap must preserve errors.As")
	assert.Contains(t, wrapped.Error(), "shutdown incomplete")
	assert.Contains(t, wrapped.Error(), "web", "the summary line must name the survivor")
}

// TestPerformShutdown_CleanReturnsNil: a clean stop returns nil and latches a nil
// (clean) outcome on the coordinator.
func TestPerformShutdown_CleanReturnsNil(t *testing.T) {
	sup, logMgr := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4201, false),
		"api": newFakeStopProcess(4202, false),
	})
	coord := newShutdownCoordinator()

	outcome := performShutdown(shutdownDeps{
		sup:          sup,
		coordinator:  coord,
		logMgr:       logMgr,
		stageTimeout: teardownStageTimeout,
	})

	assert.Nil(t, outcome, "a clean stop must return a nil outcome")
	select {
	case <-coord.Done():
	default:
		t.Fatal("performShutdown must Complete the coordinator even on a clean stop")
	}
	assert.Nil(t, coord.Outcome())
}

// TestPerformShutdown_NilDepsNoPanic: nil proxy/API/coordinator deps are all
// tolerated (the helper guards each), so a caller can drive just the supervisor.
func TestPerformShutdown_NilDepsNoPanic(t *testing.T) {
	sup, _ := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4301, false),
	})

	var outcome *domain.ProcessStopError
	require.NotPanics(t, func() {
		outcome = performShutdown(shutdownDeps{sup: sup, stageTimeout: teardownStageTimeout})
	})
	assert.Nil(t, outcome)
}

// TestPerformShutdown_CompletesWithinBound is a coarse guard that the extracted
// sequence does not hang for a clean stop.
func TestPerformShutdown_CompletesWithinBound(t *testing.T) {
	sup, logMgr := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4401, false),
	})
	done := make(chan struct{})
	go func() {
		performShutdown(shutdownDeps{sup: sup, coordinator: newShutdownCoordinator(), logMgr: logMgr, stageTimeout: teardownStageTimeout})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("performShutdown did not complete a clean stop in time")
	}
}

// TestPerformShutdown_ClosesLogMgrBeforeAPIStage pins Fix 2: performShutdown
// closes the log manager (releasing SSE subscribers) as part of the sequence, and
// Write-after-Close is a no-op rather than a panic. A subscriber's channel must be
// closed once performShutdown returns — the property that lets apiServer.Shutdown
// return promptly instead of waiting out its stage on a held stream.
func TestPerformShutdown_ClosesLogMgrBeforeAPIStage(t *testing.T) {
	sup, logMgr := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4501, false),
	})

	// Stand in for an in-flight SSE /logs/stream handler: it ranges over the
	// subscription channel and returns when the channel closes.
	subID, ch, err := logMgr.Subscribe(domain.LogFilter{})
	require.NoError(t, err)
	defer logMgr.Unsubscribe(subID)

	streamReturned := make(chan struct{})
	go func() {
		defer close(streamReturned)
		for range ch { //nolint:revive // draining until close is the point
		}
	}()

	require.NotPanics(t, func() {
		performShutdown(shutdownDeps{
			sup:          sup,
			coordinator:  newShutdownCoordinator(),
			logMgr:       logMgr,
			stageTimeout: teardownStageTimeout,
		})
	})

	// The subscriber channel was closed by the flush/close stage, so the stand-in
	// SSE handler returned (it would otherwise hold the API open).
	select {
	case <-streamReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("log subscriber was not released by performShutdown's close stage")
	}

	// Write-after-Close must be safe (SystemLog runs through the closed manager).
	require.NotPanics(t, func() { sup.SystemLog("post-close write must not panic") })
}

// TestPerformShutdown_RefusesLaunchesDuringDeregister pins Fix 1: RefuseLaunches
// runs at the very top of performShutdown, so a launch arriving while an earlier
// stage (here the deregister stage, parked on a hung daemon socket) is still
// running is refused via the launch gate — even though supervisor state is still
// "running" (so the #41 state pre-check does not fire; the refusal comes from the
// gate). RestartProcess is used because its start half reaches the gate after its
// stop half moves the process out of the "running" state (a StartProcess on the
// still-running process would short-circuit on the already-running guard, before
// the gate); this also exercises the accepted residual — the restart stops its
// process and is then refused at the start half.
func TestPerformShutdown_RefusesLaunchesDuringDeregister(t *testing.T) {
	sup, logMgr := newStartedSupervisor(t, map[string]*fakeStopProcess{
		"web": newFakeStopProcess(4601, false),
	})

	// A unix socket that accepts the deregister connection and then hangs (never
	// responds), so performShutdown parks in its deregister select until the stage
	// timeout. accepted fires once the connection lands — by then RefuseLaunches has
	// already run (it is stage 0, before the deregister goroutine starts).
	// Short base dir: a unix socket path must fit the platform's sun_path limit
	// (~104 bytes on macOS), which the long default t.TempDir() path overflows.
	sockDir, err := os.MkdirTemp("/tmp", "px")
	require.NoError(t, err)
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	stopHold := make(chan struct{})
	defer close(stopHold)
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			// Hold the connection open without responding until the test ends.
			go func(c net.Conn) { <-stopHold; c.Close() }(conn)
		}
	}()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		performShutdown(shutdownDeps{
			sup:          sup,
			daemonClient: proxyd.NewClient(sockPath),
			coordinator:  newShutdownCoordinator(),
			logMgr:       logMgr,
			stageTimeout: 2 * time.Second, // short so the parked deregister proceeds soon
		})
	}()

	// Once the deregister connection is accepted, RefuseLaunches has run: a launch
	// must be refused by the gate.
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("deregister did not reach the daemon socket")
	}
	restartErr := sup.RestartProcess(context.Background(), "web")
	assert.ErrorIs(t, restartErr, domain.ErrShutdownInProgress,
		"a launch during the deregister stage must be refused by the pre-closed gate")

	select {
	case <-shutdownDone:
	case <-time.After(15 * time.Second):
		t.Fatal("performShutdown did not finish after the deregister stage timed out")
	}
}

// TestForwardShutdownSignal_TriggersOnSignal pins the forwarder wiring: a signal
// delivered on the (fake) signal channel must request shutdown via the
// coordinator. This is the regression guard for the pre-existing bug where a
// --tui daemon queued an external SIGTERM forever because only the non-TUI branch
// consumed sigCh. The full TUI+SIGTERM path stays untestable headless.
func TestForwardShutdownSignal_TriggersOnSignal(t *testing.T) {
	coordinator := newShutdownCoordinator()
	sigCh := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logged string
	go forwardShutdownSignal(ctx, sigCh, coordinator, func(format string, args ...interface{}) {
		logged = fmt.Sprintf(format, args...)
	})

	sigCh <- syscall.SIGTERM

	select {
	case <-coordinator.TriggerCh():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not trigger shutdown on a signal")
	}
	assert.Contains(t, logged, "received")
}

// TestForwardShutdownSignal_ExitsOnContextCancel confirms the forwarder does not
// leak: with no signal, canceling ctx returns it (and it must NOT trigger).
func TestForwardShutdownSignal_ExitsOnContextCancel(t *testing.T) {
	coordinator := newShutdownCoordinator()
	sigCh := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		forwardShutdownSignal(ctx, sigCh, coordinator, nil)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not exit on context cancel")
	}

	select {
	case <-coordinator.TriggerCh():
		t.Fatal("forwarder must not trigger shutdown when it exits on ctx cancel")
	default:
	}
}

// TestResolveCaptureFlag_Matrix pins the --capture/--no-capture precedence
// matrix (plan 012 D1, C4): flags win over the materialized config default,
// passing both is an error mirroring the --tui/--detach exclusivity check,
// and passing neither leaves the config's own value untouched (ok=false).
func TestResolveCaptureFlag_Matrix(t *testing.T) {
	t.Run("--capture forces on", func(t *testing.T) {
		enabled, ok, err := resolveCaptureFlag(true, false)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, enabled)
	})

	t.Run("--no-capture forces off", func(t *testing.T) {
		enabled, ok, err := resolveCaptureFlag(false, true)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, enabled)
	})

	t.Run("both flags is an error", func(t *testing.T) {
		_, ok, err := resolveCaptureFlag(true, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
		assert.False(t, ok)
	})

	t.Run("neither flag: config wins (ok=false)", func(t *testing.T) {
		_, ok, err := resolveCaptureFlag(false, false)
		require.NoError(t, err)
		assert.False(t, ok, "caller must leave the config's own Enabled value untouched")
	})
}

// TestDialableAPIURL pins the URL `up --tui` dials to reach its own in-process
// API server. The two failure modes this helper exists for are both covered: a
// wildcard bind host is not a destination (it must become the matching loopback
// address), and an IPv6 host must be bracketed so the result is a parseable URL.
func TestDialableAPIURL(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{"IPv4 wildcard becomes loopback", "0.0.0.0", 9000, "http://127.0.0.1:9000"},
		{"empty host is the IPv4 wildcard too", "", 9000, "http://127.0.0.1:9000"},
		{"IPv6 wildcard becomes bracketed loopback", "::", 9000, "http://[::1]:9000"},
		{"localhost passes through", "localhost", 4000, "http://localhost:4000"},
		{"explicit 127.0.0.1 passes through", "127.0.0.1", 4000, "http://127.0.0.1:4000"},
		{"explicit IPv6 host is bracketed", "::1", 4000, "http://[::1]:4000"},
		// Bracketed forms are how IPv6 hosts actually BIND today (Listen formats
		// host:port with sprintf, so only "[::1]" survives it) — they must not be
		// double-bracketed into an unparseable URL (cursor review, C2).
		{"bracketed IPv6 host is not double-bracketed", "[::1]", 4000, "http://[::1]:4000"},
		{"bracketed IPv6 wildcard becomes bracketed loopback", "[::]", 9000, "http://[::1]:9000"},
		{"a LAN bind host is dialed as configured", "192.168.1.10", 8080, "http://192.168.1.10:8080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dialableAPIURL(tc.host, tc.port)
			assert.Equal(t, tc.want, got)
			// Whatever we build must survive url.Parse — the bracketing bug this
			// helper prevents shows up as a parse failure, not a wrong string.
			u, err := url.Parse(got)
			require.NoError(t, err)
			assert.Equal(t, strconv.Itoa(tc.port), u.Port())
		})
	}
}

// TestBoundAPIPort_UsesListenerNotConfig pins that the client is built against
// the port the listener actually holds: with dynamic allocation (:0) the config
// value is 0 and only the listener knows the truth.
func TestBoundAPIPort_UsesListenerNotConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	actual := ln.Addr().(*net.TCPAddr).Port
	require.NotZero(t, actual)

	assert.Equal(t, actual, boundAPIPort(ln, 0), "the dynamic port must propagate")
	assert.Equal(t, "http://127.0.0.1:"+strconv.Itoa(actual), dialableAPIURL("0.0.0.0", boundAPIPort(ln, 0)))
}

// TestIsTTY pins the terminal probe isInteractiveStdio is built on. The
// interactive case cannot be tested without a PTY (plan 018 C9 covers
// `up --tui` end to end under one); what is testable here is the negative
// answer the --tui guard depends on: a pipe, a regular file, and — the reason
// the probe is isatty rather than the ModeCharDevice bit — /dev/null are not
// terminals.
func TestIsTTY(t *testing.T) {
	t.Run("a pipe is not a terminal", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()
		defer w.Close()
		assert.False(t, isTTY(r))
		assert.False(t, isTTY(w))
	})

	t.Run("a regular file is not a terminal", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "notty")
		require.NoError(t, err)
		defer f.Close()
		assert.False(t, isTTY(f))
	})

	t.Run("a nil file is not a terminal", func(t *testing.T) {
		assert.False(t, isTTY(nil))
	})

	t.Run("/dev/null is not a terminal", func(t *testing.T) {
		f, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer f.Close()
		assert.False(t, isTTY(f), "a char device that is not a tty must be refused (cursor review, C2)")
	})
}

// TestSubscribeLogPrinter_HappensBeforeWrite pins the deterministic half of the
// D2/F2 fix (plan 020 C1, #92): subscribeLogPrinter must register the
// subscription and its buffered channel SYNCHRONOUSLY, on the caller's
// goroutine, before the caller does anything that could emit a log line the
// subscription needs to catch.
//
// This is deliberately NOT a scheduler race: subscribeLogPrinter is called and
// returns on this goroutine, and logMgr.Write then runs on that SAME goroutine
// immediately after -- exactly the sequencing runUp relies on (subscribe, then
// sup.Start/StartProcesses, both on the main goroutine, in that order, before
// the consumer goroutine is even launched). Because Subscribe has already
// registered the subscription's buffered channel by the time Write runs, the
// entry is guaranteed to be sitting in the channel however late
// printLogEntries' goroutine gets scheduled afterward -- covering the case a
// process crashes and logs its error before the consumer goroutine has run
// even once.
//
// The old code (printLogs) called logMgr.Subscribe as the first line INSIDE
// the consumer goroutine (`go printLogs(logMgr)`), so nothing ordered that
// Subscribe call before a Write happening on the caller's goroutine -- a
// version of this test against that shape is racy by construction (confirmed
// manually against a temporary revert; see the C1 commit report for the
// observed failure).
func TestSubscribeLogPrinter_HappensBeforeWrite(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100, SubscriptionBuffer: 10})
	defer logMgr.Close()

	ch, err := subscribeLogPrinter(logMgr)
	require.NoError(t, err)

	// Simulate an instantly-crashing process's error landing immediately after
	// subscribeLogPrinter returns but before any consumer goroutine has even
	// been launched -- exactly what sup.Start()/StartProcesses() can produce
	// for a typo'd cmd or missing binary (F2).
	logMgr.Write(domain.LogEntry{
		Process: "ghost",
		Stream:  domain.StreamStderr,
		Line:    "exited unexpectedly (rc=127)",
	})

	// Only now start the consumer -- mirroring `go printLogEntries(ch)` running
	// well after the write, which is safe precisely because the subscribe
	// already happened first.
	received := make(chan domain.LogEntry, 1)
	go printLogEntriesInto(ch, received)

	select {
	case entry := <-received:
		assert.Equal(t, "ghost", entry.Process)
		assert.Equal(t, "exited unexpectedly (rc=127)", entry.Line)
	case <-time.After(2 * time.Second):
		t.Fatal("log entry written immediately after subscribeLogPrinter returned was never delivered")
	}
}

// printLogEntriesInto mirrors printLogEntries' drain loop but forwards entries
// to a test channel instead of the terminal, so the test above can assert on
// the delivered entry without depending on stdout formatting.
func printLogEntriesInto(ch <-chan domain.LogEntry, out chan<- domain.LogEntry) {
	for entry := range ch {
		out <- entry
	}
}
