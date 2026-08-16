package cli

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChild is an in-memory daemonChild so the D2 poll/wait/report logic can be
// tested without spawning a real process. Wait() blocks until the child is made
// to exit (either explicitly or by a signal registered in exitOn).
type fakeChild struct {
	pid    int
	done   chan error
	once   sync.Once
	exitOn map[syscall.Signal]bool
	exitBy error

	mu      sync.Mutex
	signals []os.Signal
}

func newFakeChild(pid int) *fakeChild {
	return &fakeChild{pid: pid, done: make(chan error, 1), exitOn: map[syscall.Signal]bool{}}
}

func (f *fakeChild) Pid() int    { return f.pid }
func (f *fakeChild) Wait() error { return <-f.done }

func (f *fakeChild) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.signals = append(f.signals, sig)
	f.mu.Unlock()
	if s, ok := sig.(syscall.Signal); ok && f.exitOn[s] {
		f.exit(f.exitBy)
	}
	return nil
}

func (f *fakeChild) exit(err error) { f.once.Do(func() { f.done <- err }) }

func (f *fakeChild) sentSignals() []os.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]os.Signal(nil), f.signals...)
}

// fastStartupOps returns ops with instant sleeps and tiny deadlines/grace so the
// timeout path resolves in milliseconds. Defaults model a never-ready child (no
// state, health down); individual tests override fields.
func fastStartupOps() daemonStartupOps {
	return daemonStartupOps{
		loadState: func(string) (*daemon.State, error) { return nil, errors.New("no state") },
		healthOK:  func(string) bool { return false },
		logTail:   func(string, int, int) string { return "" },
		sleep:     func(time.Duration) {},

		readyTimeout: 40 * time.Millisecond,
		pollInterval: time.Millisecond,
		killGrace:    30 * time.Millisecond,
	}
}

// TestAwaitDaemonStartup_Success: state file with the child's PID plus a healthy
// /health probe → nil (exit 0), and the running child is never signaled.
func TestAwaitDaemonStartup_Success(t *testing.T) {
	child := newFakeChild(4242)
	// Release the (never-exiting) Wait goroutine at test end.
	t.Cleanup(func() { child.exit(nil) })

	ops := fastStartupOps()
	ops.loadState = func(string) (*daemon.State, error) {
		return &daemon.State{PID: 4242, Host: "127.0.0.1", Port: 12345}, nil
	}
	healthCalls := 0
	ops.healthOK = func(addr string) bool {
		healthCalls++
		assert.Equal(t, "127.0.0.1:12345", addr)
		return true
	}

	err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.NoError(t, err)
	assert.Equal(t, 1, healthCalls, "health should be probed once")
	assert.Empty(t, child.sentSignals(), "a ready child must not be signaled")
}

// TestAwaitDaemonStartup_EarlyDeath: the child exits (non-zero) before becoming
// ready → error, no signals (nothing to kill).
func TestAwaitDaemonStartup_EarlyDeath(t *testing.T) {
	child := newFakeChild(4243)
	child.exit(&exitStatusError{code: 1}) // dead before we start polling

	tailUsed := false
	var gotPid int
	ops := fastStartupOps()
	ops.logTail = func(_ string, pid int, _ int) string {
		tailUsed = true
		gotPid = pid
		return "boom: config invalid"
	}

	err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start")
	assert.True(t, tailUsed, "log tail should be gathered for diagnostics")
	assert.Equal(t, child.Pid(), gotPid, "log tail must be scoped to the failed child's own pid")
	assert.Empty(t, child.sentSignals(), "a dead child must not be signaled")
}

// TestAwaitDaemonStartup_NeverReadyTimeout: child stays alive but never writes
// state / answers health → SIGTERM then, after the grace, SIGKILL (escalation),
// and an error is returned.
func TestAwaitDaemonStartup_NeverReadyTimeout(t *testing.T) {
	child := newFakeChild(4244)
	// Only SIGKILL makes it exit → forces the escalation path.
	child.exitOn[syscall.SIGKILL] = true

	ops := fastStartupOps()

	start := time.Now()
	err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")
	assert.GreaterOrEqual(t, time.Since(start), ops.readyTimeout, "must poll until the deadline")

	sigs := child.sentSignals()
	require.Len(t, sigs, 2, "expected SIGTERM then SIGKILL, got %v", sigs)
	assert.Equal(t, syscall.SIGTERM, sigs[0])
	assert.Equal(t, syscall.SIGKILL, sigs[1])
}

// TestAwaitDaemonStartup_TermSuffices: a hung child that exits on SIGTERM is not
// escalated to SIGKILL.
func TestAwaitDaemonStartup_TermSuffices(t *testing.T) {
	child := newFakeChild(4245)
	child.exitOn[syscall.SIGTERM] = true

	err := awaitDaemonStartup(child, t.TempDir(), fastStartupOps())
	require.Error(t, err)

	sigs := child.sentSignals()
	require.Len(t, sigs, 1, "expected only SIGTERM, got %v", sigs)
	assert.Equal(t, syscall.SIGTERM, sigs[0])
}

// TestAwaitDaemonStartup_StaleStateIgnored: a pre-existing state file with the
// WRONG PID (a previous run's leftover) must not satisfy the poll. Health is
// never probed, and the child is timed out and killed.
func TestAwaitDaemonStartup_StaleStateIgnored(t *testing.T) {
	child := newFakeChild(4246)
	child.exitOn[syscall.SIGKILL] = true
	child.exitOn[syscall.SIGTERM] = true

	ops := fastStartupOps()
	ops.loadState = func(string) (*daemon.State, error) {
		// Stale: belongs to some other/previous process, not our child.
		return &daemon.State{PID: 9999, Host: "127.0.0.1", Port: 22222}, nil
	}
	healthCalls := 0
	ops.healthOK = func(string) bool { healthCalls++; return true }

	err := awaitDaemonStartup(child, t.TempDir(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")
	assert.Zero(t, healthCalls, "stale state (wrong PID) must not advance to the health probe")
	assert.NotEmpty(t, child.sentSignals(), "a never-ready child must be signaled")
}

// exitStatusError is a stand-in for an *exec.ExitError so early-death diagnostics
// render a non-zero status without spawning a real process.
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string { return "exit status " + strconv.Itoa(e.code) }
