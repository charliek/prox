package supervisor

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// fakeProcess is a fully controllable Process for deterministic supervisor
// tests. It decouples three things that are entangled in a real process so a
// test can drive each independently:
//
//   - leader liveness: Wait() blocks until the "leader" exits (exitLeader).
//   - group liveness: GroupAlive() reports the alive flag, independent of the
//     leader -- modelling a grandchild that can outlive the leader.
//   - signals: every Signal is recorded (value + timestamp + order) and an
//     optional onSignal hook lets the test react (e.g. die on SIGTERM/SIGKILL).
//
// This lets a single type simulate: (a) a group that dies gracefully on
// SIGTERM, (b) a stubborn group that ignores SIGTERM but dies on SIGKILL, and
// (c) an unreapable group whose leader dies but whose group never does.
type fakeProcess struct {
	pid int

	mu         sync.Mutex
	alive      bool
	aliveErr   error
	signals    []sigRecord
	waitCh     chan struct{}
	waitClosed bool
	waitErr    error

	// onSignal, when set, is invoked (synchronously, outside fp.mu) for every
	// signal received so the test can drive liveness/leader-exit in response.
	onSignal func(fp *fakeProcess, sig os.Signal)

	stdout io.Reader
	stderr io.Reader
}

type sigRecord struct {
	sig os.Signal
	at  time.Time
}

// newFakeProcess returns a fake that starts alive with an open (blocking)
// Wait() and empty output streams.
func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{
		pid:    pid,
		alive:  true,
		waitCh: make(chan struct{}),
		stdout: strings.NewReader(""),
		stderr: strings.NewReader(""),
	}
}

func (fp *fakeProcess) PID() int { return fp.pid }

func (fp *fakeProcess) Wait() error {
	<-fp.waitCh
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.waitErr
}

func (fp *fakeProcess) Signal(sig os.Signal) error {
	fp.mu.Lock()
	fp.signals = append(fp.signals, sigRecord{sig: sig, at: time.Now()})
	cb := fp.onSignal
	fp.mu.Unlock()
	if cb != nil {
		cb(fp, sig)
	}
	return nil
}

func (fp *fakeProcess) GroupAlive() (bool, error) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.alive, fp.aliveErr
}

func (fp *fakeProcess) Stdout() io.Reader { return fp.stdout }
func (fp *fakeProcess) Stderr() io.Reader { return fp.stderr }

// setAlive sets the group liveness flag reported by GroupAlive.
func (fp *fakeProcess) setAlive(a bool) {
	fp.mu.Lock()
	fp.alive = a
	fp.mu.Unlock()
}

// exitLeader makes Wait() return (once) with err, modelling the leader process
// being reaped. It does not by itself change group liveness.
func (fp *fakeProcess) exitLeader(err error) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if !fp.waitClosed {
		fp.waitErr = err
		fp.waitClosed = true
		close(fp.waitCh)
	}
}

// signalsReceived returns a copy of the signals recorded so far.
func (fp *fakeProcess) signalsReceived() []sigRecord {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	out := make([]sigRecord, len(fp.signals))
	copy(out, fp.signals)
	return out
}

// sawSignal reports whether the given signal was ever received.
func (fp *fakeProcess) sawSignal(sig os.Signal) bool {
	for _, r := range fp.signalsReceived() {
		if r.sig == sig {
			return true
		}
	}
	return false
}

// --- behaviour constructors -------------------------------------------------

// gracefulOnTerm makes the group die synchronously on SIGTERM: liveness flips
// false and the leader exits, all within the Signal call, so Stop's very next
// GroupAlive probe observes the group gone (no SIGKILL needed).
func gracefulOnTerm(fp *fakeProcess, sig os.Signal) {
	if sig == sigterm {
		fp.setAlive(false)
		fp.exitLeader(nil)
	}
}

// stubbornOnKill ignores SIGTERM and dies synchronously on SIGKILL.
func stubbornOnKill(fp *fakeProcess, sig os.Signal) {
	if sig == sigkill {
		fp.setAlive(false)
		fp.exitLeader(nil)
	}
}

// unreapableOnKill models a surviving grandchild: the leader exits on SIGKILL
// (so monitor finishes and done closes promptly) but group liveness never
// flips false, so GroupAlive stays true forever.
func unreapableOnKill(fp *fakeProcess, sig os.Signal) {
	if sig == sigkill {
		fp.exitLeader(nil)
	}
}

// newGracefulFake / newStubbornFake / newUnreapableFake are convenience
// wrappers for the three canonical behaviours.
func newGracefulFake(pid int) *fakeProcess {
	fp := newFakeProcess(pid)
	fp.onSignal = gracefulOnTerm
	return fp
}

func newStubbornFake(pid int) *fakeProcess {
	fp := newFakeProcess(pid)
	fp.onSignal = stubbornOnKill
	return fp
}

func newUnreapableFake(pid int) *fakeProcess {
	fp := newFakeProcess(pid)
	fp.onSignal = unreapableOnKill
	return fp
}

// fakeRunner is a ProcessRunner that hands out fakeProcess instances produced
// by factory, one per Start call (call index passed in). It records every
// process it produced.
type fakeRunner struct {
	mu      sync.Mutex
	factory func(call int) *fakeProcess
	procs   []*fakeProcess
	calls   int
}

func newFakeRunner(factory func(call int) *fakeProcess) *fakeRunner {
	return &fakeRunner{factory: factory}
}

func (r *fakeRunner) Start(ctx context.Context, config domain.ProcessConfig, env map[string]string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := r.calls
	r.calls++
	fp := r.factory(call)
	r.procs = append(r.procs, fp)
	return fp, nil
}

// last returns the most recently produced fake process.
func (r *fakeRunner) last() *fakeProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.procs) == 0 {
		return nil
	}
	return r.procs[len(r.procs)-1]
}

// count returns how many processes the runner has produced.
func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.procs)
}
