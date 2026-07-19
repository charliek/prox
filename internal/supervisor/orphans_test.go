package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger is a slog logger that drops everything, so the reaper's WARN/INFO
// lines never pollute test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// deadPID returns a PID that is guaranteed dead: it runs `true` to completion and
// returns its (now-exited) PID. ProcessStartTime for it returns (0, false), so it
// can never be positively identified by sameGeneration.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run(), "running true")
	return cmd.Process.Pid
}

// killpgRecorder captures the (pgid, sig) of every killpg call so a test can
// assert the escalation order without touching a real process group.
type killpgRecorder struct {
	mu    sync.Mutex
	calls []syscall.Signal
}

func (k *killpgRecorder) killpg(_ int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, sig)
	return nil
}

func (k *killpgRecorder) signals() []syscall.Signal {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]syscall.Signal, len(k.calls))
	copy(out, k.calls)
	return out
}

// --- sameGeneration truth table ---------------------------------------------

func TestSameGeneration_TruthTable(t *testing.T) {
	self := os.Getpid()
	selfToken, ok := daemon.ProcessStartTime(self)
	require.True(t, ok, "reading self start token")

	t.Run("token zero is never positive", func(t *testing.T) {
		assert.False(t, sameGeneration(self, 0), "a zero (never-captured) token must never match")
	})

	t.Run("non-positive pid is never positive", func(t *testing.T) {
		assert.False(t, sameGeneration(0, selfToken))
		assert.False(t, sameGeneration(-1, selfToken))
	})

	t.Run("dead pid is never positive", func(t *testing.T) {
		assert.False(t, sameGeneration(deadPID(t), selfToken),
			"a dead pid has no readable token, so identity cannot be confirmed")
	})

	t.Run("live self with matching token is positive", func(t *testing.T) {
		assert.True(t, sameGeneration(self, selfToken),
			"the live self with its own captured token must match")
	})

	t.Run("live self with mismatched token is not positive", func(t *testing.T) {
		assert.False(t, sameGeneration(self, selfToken+1),
			"a token that does not match the current generation must be rejected")
	})
}

// --- ledger round-trip -------------------------------------------------------

func TestWriteLoadChildren_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	recs := []ChildRecord{
		{Name: "web", PID: 4321, PGID: 4321, StartToken: 111},
		{Name: "worker", PID: 8765, PGID: 8765, StartToken: 222},
	}
	require.NoError(t, WriteChildren(dir, recs))

	// The ledger is written 0600.
	info, err := os.Stat(filepath.Join(dir, daemon.ChildrenFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "ledger must be 0600")

	loaded, err := LoadChildren(dir)
	require.NoError(t, err)
	assert.Equal(t, recs, loaded, "round-trip must preserve every record")
}

func TestLoadChildren_MissingIsNil(t *testing.T) {
	loaded, err := LoadChildren(t.TempDir())
	require.NoError(t, err, "a missing ledger must not error")
	assert.Nil(t, loaded, "a missing ledger loads as nil")
}

func TestRemoveChildren_MissingTolerated(t *testing.T) {
	assert.NoError(t, RemoveChildren(t.TempDir()), "removing a missing ledger must be tolerated")
}

// --- reaper escalation via injected seams ------------------------------------

// newTestReaper builds a reaper wired to the given seams with a short, fast grace
// window so escalation tests run quickly and NEVER signal a real process group.
func newTestReaper(killpg func(int, syscall.Signal) error, groupAlive func(int) bool, startTime func(int) (int64, bool)) *reaper {
	return &reaper{
		killpg:     killpg,
		groupAlive: groupAlive,
		startTime:  startTime,
		grace:      40 * time.Millisecond,
		poll:       5 * time.Millisecond,
		logger:     discardLogger(),
	}
}

// alwaysStartTime returns a startTime seam that reports token for pid == wantPID
// (positively identifying it) and (0,false) for anything else.
func alwaysStartTime(wantPID int, token int64) func(int) (int64, bool) {
	return func(pid int) (int64, bool) {
		if pid == wantPID {
			return token, true
		}
		return 0, false
	}
}

func TestReaper_EscalatesToSigkill_WhenGroupSurvivesSigterm(t *testing.T) {
	rec := ChildRecord{Name: "worker", PID: 12345, PGID: 12345, StartToken: 999}
	rk := &killpgRecorder{}

	// The stubborn shape: the leader "exits" on SIGTERM (unverifiable afterward)
	// and the group survives the grace window, so the reaper must escalate to
	// SIGKILL; the group then dies on SIGKILL (uncatchable). groupAlive stays true
	// until SIGKILL is sent, then false. reap runs synchronously in this goroutine,
	// so a plain bool needs no synchronization.
	sigkilled := false
	killpg := func(pgid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			sigkilled = true
		}
		return rk.killpg(pgid, sig)
	}
	groupAlive := func(int) bool { return !sigkilled }
	r := newTestReaper(killpg, groupAlive, alwaysStartTime(rec.PID, rec.StartToken))

	reaped, skipped := r.reap([]ChildRecord{rec})

	assert.Equal(t, []ChildRecord{rec}, reaped, "a group confirmed gone after SIGKILL is reaped")
	assert.Empty(t, skipped)
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, rk.signals(),
		"escalation must send SIGTERM then, when the group survives the grace window, SIGKILL")
}

func TestReaper_SurvivorNotReported_WhenGroupPersistsAfterSigkill(t *testing.T) {
	// Codex review: a group that CANNOT be confirmed gone (e.g. SIGKILL errored, or
	// the group is wedged in uninterruptible sleep) must be reported SKIPPED, never
	// reaped — otherwise ReapOrphans drops the still-port-holding orphan from the
	// ledger and up.go prints a false reaped count.
	rec := ChildRecord{Name: "wedged", PID: 32100, PGID: 32100, StartToken: 999}
	rk := &killpgRecorder{}
	groupAlive := func(int) bool { return true } // never confirmed gone, even after SIGKILL
	r := newTestReaper(rk.killpg, groupAlive, alwaysStartTime(rec.PID, rec.StartToken))

	reaped, skipped := r.reap([]ChildRecord{rec})

	assert.Empty(t, reaped, "a group not confirmed gone must NOT be reported reaped")
	assert.Equal(t, []ChildRecord{rec}, skipped, "an unconfirmed survivor is surfaced as skipped")
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, rk.signals(),
		"escalation still sent both signals before giving up")
}

func TestReaper_NoSigkill_WhenGroupExitsOnSigterm(t *testing.T) {
	rec := ChildRecord{Name: "worker", PID: 22222, PGID: 22222, StartToken: 999}
	rk := &killpgRecorder{}

	// Well-behaved group: gone immediately after SIGTERM.
	groupAlive := func(int) bool { return false }
	r := newTestReaper(rk.killpg, groupAlive, alwaysStartTime(rec.PID, rec.StartToken))

	reaped, skipped := r.reap([]ChildRecord{rec})

	assert.Equal(t, []ChildRecord{rec}, reaped)
	assert.Empty(t, skipped)
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM}, rk.signals(),
		"a group gone after SIGTERM must NOT be SIGKILLed")
}

func TestReaper_SkipsCorruptRecord_PidPgidMismatch(t *testing.T) {
	// PGID != PID: corrupt/planted -- must be skipped before any signal, even
	// though PGID > 0.
	rec := ChildRecord{Name: "evil", PID: 100, PGID: 200, StartToken: 999}
	rk := &killpgRecorder{}
	// startTime would positively identify it if it were ever consulted -- it must
	// not be.
	r := newTestReaper(rk.killpg, func(int) bool { return true }, alwaysStartTime(100, 999))

	reaped, skipped := r.reap([]ChildRecord{rec})

	assert.Empty(t, reaped)
	assert.Equal(t, []ChildRecord{rec}, skipped)
	assert.Empty(t, rk.signals(), "a corrupt record must never be signaled")
}

func TestReaper_SkipsUnidentifiable(t *testing.T) {
	rk := &killpgRecorder{}
	// startTime that can never positively identify anything (models a stale ledger
	// whose leaders are all gone / tokens unreadable).
	deadStartTime := func(int) (int64, bool) { return 0, false }
	r := newTestReaper(rk.killpg, func(int) bool { return true }, deadStartTime)

	recs := []ChildRecord{
		{Name: "zero-token", PID: 300, PGID: 300, StartToken: 0},   // token 0
		{Name: "unreadable", PID: 400, PGID: 400, StartToken: 777}, // startTime !ok
	}
	reaped, skipped := r.reap(recs)

	assert.Empty(t, reaped)
	assert.ElementsMatch(t, recs, skipped, "unverifiable records are skipped")
	assert.Empty(t, rk.signals(), "no unverifiable record may be signaled (no SIGKILL of a stale group)")
}

func TestReaper_SkipsMismatchedToken(t *testing.T) {
	rec := ChildRecord{Name: "worker", PID: 555, PGID: 555, StartToken: 111}
	rk := &killpgRecorder{}
	// Current token differs from the recorded one (PID reuse) -> not our generation.
	r := newTestReaper(rk.killpg, func(int) bool { return true }, alwaysStartTime(555, 222))

	reaped, skipped := r.reap([]ChildRecord{rec})

	assert.Empty(t, reaped)
	assert.Equal(t, []ChildRecord{rec}, skipped)
	assert.Empty(t, rk.signals())
}

// --- ReapOrphans (real syscalls, never signals: all records unidentifiable) ---

func TestReapOrphans_MissingLedgerIsNoOp(t *testing.T) {
	reaped, skipped, err := ReapOrphans(t.TempDir(), discardLogger())
	require.NoError(t, err)
	assert.Nil(t, reaped)
	assert.Nil(t, skipped)
}

func TestReapOrphans_StaleLedger_SkipsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	// A well-formed but stale record: PID==PGID, but the pid is dead, so it can
	// never be positively identified and ReapOrphans (real syscalls) never signals
	// anything -- a clean no-op with NO SIGKILL.
	dp := deadPID(t)
	require.NoError(t, WriteChildren(dir, []ChildRecord{
		{Name: "gone", PID: dp, PGID: dp, StartToken: 123456789},
	}))

	reaped, skipped, err := ReapOrphans(dir, discardLogger())
	require.NoError(t, err)
	assert.Empty(t, reaped, "a stale group must not be reaped")
	assert.Len(t, skipped, 1, "the stale record is skipped")

	// The old ledger is removed after the pass.
	_, statErr := os.Stat(filepath.Join(dir, daemon.ChildrenFileName))
	assert.True(t, os.IsNotExist(statErr), "the ledger must be removed after the reap pass")
}

// --- persistence concurrency -------------------------------------------------

// newPersistSupervisor builds a supervisor over n graceful fake processes with a
// real StateDir so launches persist the ownership ledger.
func newPersistSupervisor(t *testing.T, n int, runner ProcessRunner) (*Supervisor, string, []string) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 1000})
	t.Cleanup(func() { logMgr.Close() })

	procs := make(map[string]config.ProcessConfig, n)
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("proc%d", i)
		procs[name] = config.ProcessConfig{Cmd: "irrelevant", StopTimeout: "5s"}
		names = append(names, name)
	}
	cfg := &config.Config{
		API:       config.APIConfig{Port: 5599, Host: "127.0.0.1"},
		Processes: procs,
	}
	stateDir := t.TempDir()
	supCfg := DefaultSupervisorConfig()
	supCfg.StateDir = stateDir
	return New(cfg, logMgr, runner, supCfg), stateDir, names
}

// TestPersistChildren_ConcurrentLaunches drives the persist callback from the
// parallel initial-start goroutines and asserts the final ledger has every live
// child exactly once (last-writer-correctness under childrenMu + the s.mu
// snapshot).
func TestPersistChildren_ConcurrentLaunches(t *testing.T) {
	const n = 8
	// Distinct pids per launch; graceful so a later Stop reaps them cleanly.
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(20000 + call) })
	sup, stateDir, names := newPersistSupervisor(t, n, runner)

	result, err := sup.Start(context.Background())
	require.NoError(t, err)
	require.Empty(t, result.Failed, "all fake processes should start")

	// Assert the ledger BEFORE Stop (a clean Stop removes it).
	recs, err := LoadChildren(stateDir)
	require.NoError(t, err)
	require.Len(t, recs, n, "the final ledger must record every live child exactly once")

	gotNames := make([]string, 0, len(recs))
	seenPID := make(map[int]bool)
	for _, r := range recs {
		gotNames = append(gotNames, r.Name)
		assert.Greater(t, r.PID, 0, "each record must carry a positive pid")
		assert.Equal(t, r.PID, r.PGID, "PGID must equal the leader PID by construction")
		assert.False(t, seenPID[r.PID], "no pid may appear twice in the ledger")
		seenPID[r.PID] = true
	}
	sort.Strings(gotNames)
	sort.Strings(names)
	assert.Equal(t, names, gotNames, "every configured process must appear exactly once")

	// A clean full Stop reaps the graceful fakes and removes the ledger.
	require.NoError(t, sup.Stop(context.Background()))
	_, statErr := os.Stat(filepath.Join(stateDir, daemon.ChildrenFileName))
	assert.True(t, os.IsNotExist(statErr), "a clean Stop must remove the ledger")
}

// TestPersistChildren_SkippedAfterRefuseLaunches pins the fix for the codex
// finding that a launch reaching the persist callback LATE (after Stop began
// managing the ledger) could clobber a RETAINED ledger (dropping a surviving
// group) or recreate a removed one. Once launches are refused, persistChildren
// must no-op.
func TestPersistChildren_SkippedAfterRefuseLaunches(t *testing.T) {
	runner := newFakeRunner(func(call int) *fakeProcess { return newGracefulFake(21000 + call) })
	sup, stateDir, _ := newPersistSupervisor(t, 1, runner)

	retained := []ChildRecord{{Name: "survivor", PID: 40000, PGID: 40000, StartToken: 123}}

	// Positive control: with launches OPEN, persistChildren rewrites the ledger
	// (no child is running yet, so it writes an empty set). This proves the no-op
	// below is due to the gate, not some other reason.
	require.NoError(t, WriteChildren(stateDir, retained))
	sup.launchable.Store(true)
	sup.persistChildren()
	open, err := LoadChildren(stateDir)
	require.NoError(t, err)
	assert.Empty(t, open, "with launches open, persistChildren rewrites the ledger (no live children -> empty)")

	// Shutdown has begun: launches refused, Stop owns the ledger. A late callback
	// must NOT touch the retained ledger.
	require.NoError(t, WriteChildren(stateDir, retained))
	sup.RefuseLaunches()
	sup.persistChildren()
	got, err := LoadChildren(stateDir)
	require.NoError(t, err)
	assert.Equal(t, retained, got, "after RefuseLaunches, persistChildren must not clobber the retained ledger")
}
