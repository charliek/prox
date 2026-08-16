package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/daemon"
)

// Unit tests for the run ledger and the reaper (plan 027 C5, leakguard_test.go).
//
// This is code that sends SIGTERM and SIGKILL to pids it reads out of a file in
// ${TMPDIR}. "I ran it once and it worked" is not an acceptable level of
// evidence for that, so every branch that decides whether to signal is pinned
// here: owner alive / dead / pid-reused, target dead / pid-reused, malformed
// input, and concurrent sweeps.
//
// Two rules the tests themselves obey:
//
//   - nothing is ever signalled that this test did not create. Every target is a
//     `sleep` this process started, and the reaper is only ever pointed at a
//     ledger directory under t.TempDir().
//   - a killed sleeper is REAPED promptly by its own waiter goroutine, because a
//     zombie answers kill(pid, 0) and would make the reaper's liveness wait run
//     its whole budget.

// sleeper is a throwaway process to stand in for a leaked daemon.
type sleeper struct {
	cmd   *exec.Cmd
	pid   int
	token int64
	done  chan struct{}
}

func startSleeper(t *testing.T) *sleeper {
	t.Helper()

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	s := &sleeper{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	s.token, _ = daemon.ProcessStartTime(s.pid)

	// The ONE Wait, so a killed sleeper stops being a zombie immediately. The
	// hygiene rule's subject is proxRun's single-owner invariant for prox
	// LAUNCHES; a sleeper is this file's own throwaway `sleep`, and this
	// goroutine is its single owner, which mirrors that invariant exactly.
	go func() {
		//prox:allow-cmd-wait single owner of this file's own throwaway sleeper
		_ = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // no-op once it has been reaped
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	})
	return s
}

// identity is the sleeper as the reaper sees it. Addr is deliberately empty:
// these processes serve no API, and stopDaemon must skip the HTTP step when
// there is no address rather than waiting out a connection.
//
// An identity in this shape can never be reaped across runs -- the cross-run
// sweep signals nothing it cannot positively identify as prox, and a bare
// `sleep` is not prox. That is the point for the NEGATIVE cases; a case that
// expects a reap uses asLeakedDaemon below.
func (s *sleeper) identity() daemonIdentity {
	return daemonIdentity{PID: s.pid, StartToken: s.token, StateDir: "/nonexistent"}
}

// asLeakedDaemon dresses a sleeper up as something the cross-run sweep is
// allowed to signal: a private project directory, and a stand-in API on
// loopback that identifies itself as the prox for exactly that directory.
//
// The suite cannot use a real leaked daemon here -- the whole point of these
// cases is a target that OUTLIVES a dead owner, and a real prox left running
// past the test that made it is the leak this file exists to prevent. So the
// two halves the reaper actually inspects are supplied separately: the process
// identity comes from a `sleep` this test started (so every signal lands on
// something we own), and the API identity from an httptest server.
//
// Only GET /api/v1/status is served. Everything else -- above all the waited
// POST /api/v1/shutdown that stopDaemon tries first -- 404s, which is what a
// wedged daemon looks like and drops teardown straight through to signals.
func (s *sleeper) asLeakedDaemon(t *testing.T) daemonIdentity {
	t.Helper()

	project := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "running",
			"api_version": "v1",
			"project_dir": project,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return daemonIdentity{
		PID:        s.pid,
		StartToken: s.token,
		StateDir:   filepath.Join(project, daemonStateDirName),
		Addr:       srv.URL,
	}
}

// kill terminates the sleeper and waits until it has been reaped, so its pid is
// genuinely gone (not a zombie) before the test continues.
func (s *sleeper) kill(t *testing.T) {
	t.Helper()
	_ = s.cmd.Process.Kill()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("sleeper %d did not exit", s.pid)
	}
}

func (s *sleeper) alive() bool { return daemon.IsProcessAlive(s.pid, s.token) }

// requireGone gives the sleeper a moment to disappear and then insists it has.
func (s *sleeper) requireGone(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("sleeper %d should have been reaped but is still alive", s.pid)
	}
}

// requireTokens skips a test that cannot distinguish a pid from a pid
// GENERATION -- the whole subject of the pid-reuse cases -- on a platform with
// no ProcessStartTime implementation (internal/daemon/process_other.go).
func requireTokens(t *testing.T) {
	t.Helper()
	if _, ok := daemon.ProcessStartTime(os.Getpid()); !ok {
		t.Skip("no process start token on this platform; pid-reuse cases are not expressible")
	}
}

// ledgerEntryFor builds a well-formed entry: target owned by owner.
func ledgerEntryFor(owner, target daemonIdentity) ledgerEntry {
	return ledgerEntry{
		PID:             target.PID,
		StartToken:      target.StartToken,
		StateDir:        target.StateDir,
		Addr:            target.Addr,
		OwnerPID:        owner.PID,
		OwnerStartToken: owner.StartToken,
		TS:              time.Now(),
	}
}

// writeLedgerFile writes entries as JSONL at the ledger path owner names.
//
// The owner is a full identity, not a pid, because the file NAME carries the
// owner's start token as well as its pid -- that is what keeps a run whose pid
// has been recycled from appending to its predecessor's file.
func writeLedgerFile(t *testing.T, dir string, owner daemonIdentity, entries ...ledgerEntry) string {
	t.Helper()

	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal ledger entry: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeRawLedgerFile(t, dir, owner, buf.String())
}

func writeRawLedgerFile(t *testing.T, dir string, owner daemonIdentity, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	path := filepath.Join(dir, ledgerFileName(owner.PID, owner.StartToken))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write ledger file: %v", err)
	}
	return path
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestReap_OwnerAliveLeavesLedgerAlone is the most important negative case in
// this file: running this suite from two terminals at once is routine, and a
// reaper that killed a LIVE run's daemons would be far worse than the leak it
// exists to fix.
func TestReap_OwnerAliveLeavesLedgerAlone(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)  // stands in for another test binary, still running
	target := startSleeper(t) // its daemon

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, owner.identity(),
		ledgerEntryFor(owner.identity(), target.asLeakedDaemon(t)))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Fatalf("a live owner's daemons must not be reaped, got %d\n%s", n, log.String())
	}
	if !target.alive() {
		t.Fatal("the live owner's daemon was killed")
	}
	if !fileExists(t, path) {
		t.Error("a live owner's ledger must be left in place for it to keep appending to")
	}
}

// TestReap_DeadOwnerKillsRecordedDaemon is the case the whole layer exists for:
// a previous run was SIGKILLed, so its per-test cleanups never ran and its
// daemon is still up -- and it still answers as the prox for the directory the
// ledger names, which is what authorizes the signal.
func TestReap_DeadOwnerKillsRecordedDaemon(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t) // the test binary died without cleaning up

	target := startSleeper(t)

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, target.asLeakedDaemon(t)))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 1 {
		t.Fatalf("expected 1 reaped daemon, got %d\n%s", n, log.String())
	}
	target.requireGone(t)
	if fileExists(t, path) {
		t.Error("a swept ledger must be removed so the next run does not re-examine it")
	}
	if !strings.Contains(log.String(), "reaping leaked daemon") {
		t.Errorf("the reaper must say what it killed, got:\n%s", log.String())
	}
	if !strings.Contains(log.String(), strconv.Itoa(target.pid)) {
		t.Errorf("the reaper must name the pid it killed, got:\n%s", log.String())
	}
}

// TestReap_OwnerPIDReusedIsTreatedAsDead pins why the OWNER's start token is
// recorded. With pid comparison alone, a dead run whose pid has been handed to
// something else looks alive forever and its leaked daemon is never reaped.
func TestReap_OwnerPIDReusedIsTreatedAsDead(t *testing.T) {
	startTest(t, defaultTestBudget)
	requireTokens(t)

	// A live process standing in for the unrelated program that inherited the
	// dead run's pid, recorded with the ORIGINAL run's token.
	impostor := startSleeper(t)
	staleOwner := daemonIdentity{PID: impostor.pid, StartToken: impostor.token + 1}

	target := startSleeper(t)

	dir := t.TempDir()
	writeLedgerFile(t, dir, staleOwner, ledgerEntryFor(staleOwner, target.asLeakedDaemon(t)))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 1 {
		t.Fatalf("a reused owner pid must not pass for the owner still running, got %d reaped\n%s", n, log.String())
	}
	target.requireGone(t)
	if !impostor.alive() {
		t.Fatal("the process that merely inherited the owner's pid was signalled")
	}
}

// TestReap_TargetPIDReusedIsNotSignalled is the mirror image, and the reason
// every kill re-verifies pid AND token: by the time a later run reads a ledger,
// a recorded daemon pid may belong to something else entirely.
//
// The recorded ADDRESS here identifies itself as prox for the recorded
// directory, so this pins that positive identification did not demote the token
// check to optional: both are necessary.
func TestReap_TargetPIDReusedIsNotSignalled(t *testing.T) {
	startTest(t, defaultTestBudget)
	requireTokens(t)

	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t)

	// A live, innocent process holding the pid the dead run recorded, but with a
	// different generation token.
	bystander := startSleeper(t)
	stale := bystander.asLeakedDaemon(t)
	stale.StartToken = bystander.token + 1

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, stale))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Fatalf("a reused target pid must never be signalled, got %d reaped\n%s", n, log.String())
	}
	// Give any (wrongly) delivered signal time to land before asserting.
	time.Sleep(200 * time.Millisecond)
	if !bystander.alive() {
		t.Fatal("the reaper killed a process that merely inherited a recorded pid")
	}
	if fileExists(t, path) {
		t.Error("the ledger should still be removed once its owner is gone")
	}
}

// TestReap_DeadTargetSignalsNothing: the ordinary case, where the previous run's
// teardown DID work and the ledger is just bookkeeping to clear away.
func TestReap_DeadTargetSignalsNothing(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t)

	target := startSleeper(t)
	targetID := target.identity()
	target.kill(t)

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, targetID))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Fatalf("nothing was alive to reap, got %d\n%s", n, log.String())
	}
	if fileExists(t, path) {
		t.Error("a fully accounted-for ledger must still be removed")
	}
}

// TestLedger_MalformedLinesAreSkipped: a run killed mid-append leaves a
// truncated last line, which is the NORMAL state of the files this reaper reads.
// A malformed line must cost that line and nothing else.
func TestLedger_MalformedLinesAreSkipped(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t)

	target := startSleeper(t)
	valid, err := json.Marshal(ledgerEntryFor(ownerID, target.asLeakedDaemon(t)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A foreign row: well-formed JSON claiming a different owner than the file
	// name. It names a live pid, and must not be signalled.
	foreignOwner := daemonIdentity{PID: ownerID.PID + 1, StartToken: 1}
	bystander := startSleeper(t)
	foreign, err := json.Marshal(ledgerEntryFor(foreignOwner, bystander.identity()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	body := strings.Join([]string{
		"this is not json at all",
		"",
		string(foreign),
		string(valid),
		`{"pid": 0, "start_token": 5}`, // parses, but names no process
		`{"pid": 12345, "start_to`,     // truncated by a SIGKILL mid-write
	}, "\n")

	dir := t.TempDir()
	path := writeRawLedgerFile(t, dir, ownerID, body)

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 1 {
		t.Fatalf("expected exactly the one valid entry to be reaped, got %d\n%s", n, log.String())
	}
	target.requireGone(t)
	if !bystander.alive() {
		t.Fatal("a row whose owner_pid contradicts the file name must not be acted on")
	}
	if fileExists(t, path) {
		t.Error("ledger should have been removed")
	}
}

// TestReap_ConcurrentSweepsAreIdempotent: two runs starting at the same moment
// sweep the same directory. Neither may panic, double-signal a stranger, or
// leave the file behind.
func TestReap_ConcurrentSweepsAreIdempotent(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t)

	target := startSleeper(t)

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, target.asLeakedDaemon(t)))

	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var log bytes.Buffer
			n := reapStaleLedgers(dir, os.Getpid(), &log)
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()

	target.requireGone(t)
	if total < 1 {
		t.Fatalf("at least one sweeper must have reaped the daemon, total=%d", total)
	}
	if fileExists(t, path) {
		t.Error("ledger should have been removed by whichever sweeper got there first")
	}

	// And a sweep of the now-empty directory is a no-op rather than an error.
	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Errorf("re-sweeping must be a no-op, got %d\n%s", n, log.String())
	}
}

// TestReap_IgnoresOwnLedgerAndForeignFiles: the sweeper shares ${TMPDIR} with
// this very run and with whatever else lives there.
func TestReap_IgnoresOwnLedgerAndForeignFiles(t *testing.T) {
	startTest(t, defaultTestBudget)
	self := startSleeper(t) // this run's "own" daemon
	selfPID := os.Getpid()

	dir := t.TempDir()
	own := writeLedgerFile(t, dir, daemonIdentity{PID: selfPID}, ledgerEntryFor(daemonIdentity{PID: selfPID}, self.identity()))

	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("nothing to do with prox\n"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, selfPID, &log); n != 0 {
		t.Fatalf("the sweeper must skip its own ledger, got %d\n%s", n, log.String())
	}
	if !self.alive() {
		t.Fatal("the sweeper killed a daemon belonging to the running process")
	}
	if !fileExists(t, own) || !fileExists(t, notes) {
		t.Error("neither this run's ledger nor an unrelated file may be removed")
	}
}

// TestLedger_RecordAppendsAndRemoveDeletes covers the write side: file mode,
// one line per daemon, and a remove that is safe to repeat.
func TestLedger_RecordAppendsAndRemoveDeletes(t *testing.T) {
	startTest(t, defaultTestBudget)
	dir := filepath.Join(t.TempDir(), "runs")
	l := newRunLedger(dir, os.Getpid())

	ids := []daemonIdentity{
		{PID: 4242, StartToken: 11, StateDir: "/tmp/a/.prox", Addr: "http://127.0.0.1:1"},
		{PID: 4243, StartToken: 12, StateDir: "/tmp/b/.prox", Addr: "http://127.0.0.1:2"},
	}
	for _, id := range ids {
		if err := l.record(id); err != nil {
			t.Fatalf("record %v: %v", id, err)
		}
	}
	// A pid-less identity is not worth a row.
	if err := l.record(daemonIdentity{}); err != nil {
		t.Fatalf("recording an empty identity must be a silent no-op: %v", err)
	}

	info, err := os.Stat(l.path)
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("ledger must be 0600 (it names pids that will be signalled), got %o", mode)
	}

	entries := readLedgerFile(l.path, l.ownerPID, l.ownerToken)
	if len(entries) != len(ids) {
		t.Fatalf("expected %d entries, got %d", len(ids), len(entries))
	}
	for i, e := range entries {
		if e.PID != ids[i].PID || e.StartToken != ids[i].StartToken || e.StateDir != ids[i].StateDir {
			t.Errorf("entry %d round-tripped as %+v, want %+v", i, e, ids[i])
		}
		if e.OwnerPID != os.Getpid() {
			t.Errorf("entry %d owner pid = %d, want %d", i, e.OwnerPID, os.Getpid())
		}
		if e.TS.IsZero() {
			t.Errorf("entry %d has no timestamp", i)
		}
	}
	if got := l.recorded(); len(got) != len(ids) {
		t.Errorf("in-memory record has %d identities, want %d", len(got), len(ids))
	}

	l.remove()
	if fileExists(t, l.path) {
		t.Fatal("remove must delete the ledger file")
	}
	l.remove() // idempotent: the signal handler and the normal exit path both call it
}

// TestLedger_ConcurrentRecordsAllLand: launches happen from parallel tests, so
// the append path is contended.
func TestLedger_ConcurrentRecordsAllLand(t *testing.T) {
	startTest(t, defaultTestBudget)
	dir := filepath.Join(t.TempDir(), "runs")
	l := newRunLedger(dir, os.Getpid())

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.record(daemonIdentity{PID: 9000 + i, StartToken: int64(i + 1)}); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
	}
	wg.Wait()

	if entries := readLedgerFile(l.path, l.ownerPID, l.ownerToken); len(entries) != n {
		t.Fatalf("expected %d entries, got %d", n, len(entries))
	}
	if got := l.recorded(); len(got) != n {
		t.Fatalf("in-memory record has %d identities, want %d", len(got), n)
	}
}

// TestLedger_ReapOwnStopsSurvivors covers the end-of-run sweep: whatever a
// test's own teardown missed dies before the process exits.
func TestLedger_ReapOwnStopsSurvivors(t *testing.T) {
	startTest(t, defaultTestBudget)
	dir := filepath.Join(t.TempDir(), "runs")
	l := newRunLedger(dir, os.Getpid())

	survivor := startSleeper(t)
	departed := startSleeper(t)
	departedID := departed.identity()
	departed.kill(t)

	if err := l.record(survivor.identity()); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := l.record(departedID); err != nil {
		t.Fatalf("record: %v", err)
	}

	var log bytes.Buffer
	if n := l.reapOwn(&log); n != 1 {
		t.Fatalf("expected exactly the survivor to be stopped, got %d\n%s", n, log.String())
	}
	survivor.requireGone(t)

	// Idempotent: a second pass has nothing left to do, which is what makes the
	// signal handler safe to run alongside the normal exit path.
	log.Reset()
	if n := l.reapOwn(&log); n != 0 {
		t.Errorf("a second sweep must be a no-op, got %d\n%s", n, log.String())
	}
}

// TestReap_StopDaemonToleratesAnUnreachableAddr: a leaked daemon that is wedged
// (or whose API port has been taken over by something else) must not make
// teardown hang -- it must fall through to signals.
func TestReap_StopDaemonToleratesAnUnreachableAddr(t *testing.T) {
	startTest(t, defaultTestBudget)
	target := startSleeper(t)
	id := target.identity()
	// A port nothing is listening on: the POST fails immediately with connection
	// refused, and the identity is then signalled.
	id.Addr = "http://127.0.0.1:1"

	start := time.Now()
	stopDaemon(id, nil)
	target.requireGone(t)
	if elapsed := time.Since(start); elapsed > daemonExitBudget {
		t.Errorf("stopDaemon took %v; an unreachable addr must not consume the exit budget", elapsed)
	}

	// Idempotent on an already-dead identity, and it must not signal the pid
	// again (there is nothing left to signal).
	stopDaemon(id, nil)
}

// TestLedger_StateFileMismatchIsNotAdopted covers identity resolution, the step
// that decides WHICH pid the rest of this machinery will eventually signal. A
// state file left behind by a previous generation must not be adopted just
// because it parses.
func TestLedger_StateFileMismatchIsNotAdopted(t *testing.T) {
	startTest(t, defaultTestBudget)
	stateDir := filepath.Join(t.TempDir(), daemonStateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	statePath := filepath.Join(stateDir, daemonStateFileName)

	writeState := func(body string) {
		t.Helper()
		if err := os.WriteFile(statePath, []byte(body), 0o600); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}

	live := startSleeper(t)
	// This callback stands in for a FOREGROUND launch's rule -- the state pid is
	// the process we started -- and records what it was told about the daemon's
	// start time, which is the input a DETACHED launch judges freshness on
	// (TestLeak_StaleStateFileIsNotAdoptedByFailedDetachedLaunch covers that end
	// to end).
	var sawStartedAt time.Time
	accept := func(pid int, startedAt time.Time) bool {
		sawStartedAt = startedAt
		return pid == live.pid
	}

	// A state file naming a DIFFERENT pid than the launch owns: rejected.
	writeState(`{"pid": ` + strconv.Itoa(live.pid+1) + `, "port": 12345, "host": "127.0.0.1"}`)
	if id, ok := identityFromStateDir(stateDir, accept); ok {
		t.Fatalf("a state file naming another process must not be adopted, got %s", id)
	}

	// Half-written (the file is truncated in place, not renamed): rejected, and
	// reported as "not yet" rather than as an error.
	writeState(`{"pid": ` + strconv.Itoa(live.pid) + `, "por`)
	if _, ok := identityFromStateDir(stateDir, accept); ok {
		t.Fatal("a half-written state file must not be adopted")
	}

	// Parseable but portless: rejected (there would be no address to talk to).
	writeState(`{"pid": ` + strconv.Itoa(live.pid) + `, "port": 0, "host": "127.0.0.1"}`)
	if _, ok := identityFromStateDir(stateDir, accept); ok {
		t.Fatal("a state file with no port must not be adopted")
	}

	// The real thing: adopted, with the token captured and the address built.
	const startedAt = "2026-01-02T03:04:05.123456789Z"
	writeState(`{"pid": ` + strconv.Itoa(live.pid) + `, "port": 12345, "host": "", "started_at": "` + startedAt + `"}`)
	id, ok := identityFromStateDir(stateDir, accept)
	if !ok {
		t.Fatal("a state file naming this launch's process must be adopted")
	}
	if id.PID != live.pid {
		t.Errorf("identity pid = %d, want %d", id.PID, live.pid)
	}
	if id.StartToken != live.token {
		t.Errorf("identity token = %d, want %d", id.StartToken, live.token)
	}
	if id.Addr != "http://127.0.0.1:12345" {
		t.Errorf("identity addr = %q, want the loopback default filled in", id.Addr)
	}
	if id.StateDir != stateDir {
		t.Errorf("identity state dir = %q, want %q", id.StateDir, stateDir)
	}
	if !id.alive() {
		t.Error("the adopted identity should report itself alive")
	}
	// The daemon's own start time must reach the accept callback with full
	// precision: it is the only evidence a detached launch has that the file
	// describes the generation IT caused.
	if got := sawStartedAt.UTC().Format(time.RFC3339Nano); got != startedAt {
		t.Errorf("accept saw started_at %q, want %q", got, startedAt)
	}
}

// TestLedger_TokenlessEntryIsNotSignalled pins the one guarantee the pid check
// alone cannot make.
//
// daemon.IsProcessAlive answers "alive" when the recorded start token is 0 --
// the deliberate fallback for "no token could be captured" -- so a tokenless
// ledger row would otherwise be reaped on bare pid liveness. The owner of a
// swept ledger is by definition already dead, so such a row can be arbitrarily
// old and its pid long since recycled by the OS. Killing on pid alone there is
// the difference between reaping our own leak and SIGKILLing an unrelated
// process on a developer's machine.
//
// The row must be reported and left alone, not silently skipped and not killed.
func TestLedger_TokenlessEntryIsNotSignalled(t *testing.T) {
	startTest(t, defaultTestBudget)
	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t)

	// A live process recorded WITHOUT a start token: exactly what a failed
	// ProcessStartTime capture leaves behind. It stands in for whatever now
	// holds that pid.
	bystander := startSleeper(t)
	tokenless := bystander.identity()
	tokenless.StartToken = 0

	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, tokenless))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Fatalf("a tokenless entry must not be reaped, got %d reaped\n%s", n, log.String())
	}
	if !bystander.alive() {
		t.Fatal("the reaper signalled a pid it could not prove was its own daemon")
	}
	if out := log.String(); !strings.Contains(out, "NOT reaping") ||
		!strings.Contains(out, strconv.Itoa(tokenless.PID)) {
		t.Errorf("the skip must be reported with its pid so the leak is visible, got:\n%s", out)
	}
	if fileExists(t, path) {
		t.Error("ledger should still have been removed")
	}
}

// TestReap_UnidentifiedProcessIsNotSignalled is the core of plan 027 C15: pid
// and start token are NECESSARY to signal a process across runs, but they are
// not SUFFICIENT.
//
// Both are claims about a record written by a test binary that is already dead,
// so the recorded pid can be arbitrarily stale; and daemon.IsProcessAlive is
// deliberately biased toward "alive" (it answers yes whenever the CURRENT token
// cannot be read), which is the opposite of the bias wanted before a SIGKILL.
// So nothing is signalled until the process itself answers prox's API at the
// recorded address AND claims the recorded project directory. Each case here is
// a live process this test started that fails that test in a different way, and
// none of them may be touched.
func TestReap_UnidentifiedProcessIsNotSignalled(t *testing.T) {
	startTest(t, defaultTestBudget)

	serve := func(t *testing.T, h http.HandlerFunc) string {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}

	// sweep points the reaper at a dead run's ledger naming target, and insists
	// that nothing was signalled and that the skip was REPORTED -- an invisible
	// stray daemon is the failure mode this whole layer exists to remove, so
	// declining to kill must never be silent.
	sweep := func(t *testing.T, target *sleeper, id daemonIdentity) {
		t.Helper()
		owner := startSleeper(t)
		ownerID := owner.identity()
		owner.kill(t)

		dir := t.TempDir()
		path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(ownerID, id))

		var log bytes.Buffer
		if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
			t.Fatalf("an unidentified process must not be reaped, got %d\n%s", n, log.String())
		}
		// Give any (wrongly) delivered signal time to land before asserting.
		time.Sleep(200 * time.Millisecond)
		if !target.alive() {
			t.Fatal("the reaper signalled a process that never identified itself as prox")
		}
		out := log.String()
		if !strings.Contains(out, "NOT reaping") || !strings.Contains(out, strconv.Itoa(target.pid)) ||
			!strings.Contains(out, "cannot be proven to be the daemon") {
			t.Errorf("the skip must be reported with its pid and a reason, got:\n%s", out)
		}
		if fileExists(t, path) {
			t.Error("the ledger should still be removed once its owner is gone")
		}
	}

	// Nothing at all answers: the ordinary shape of a recycled pid, whose new
	// owner serves no API on the port the dead run wrote down.
	t.Run("nothing listening at the recorded address", func(t *testing.T) {
		target := startSleeper(t)
		id := target.asLeakedDaemon(t)
		dead := httptest.NewServer(http.NotFoundHandler())
		id.Addr = dead.URL
		dead.Close() // the port is now refused, exactly as a departed daemon's is
		sweep(t, target, id)
	})

	// Something answers 200 with valid JSON, but it is not prox. A liveness
	// check that stopped at "the port is open" would kill this.
	t.Run("something answers but it is not prox", func(t *testing.T) {
		target := startSleeper(t)
		id := target.asLeakedDaemon(t)
		id.Addr = serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hello":"world"}`))
		})
		sweep(t, target, id)
	})

	// A real prox answers -- but it is another project's, which is what a
	// developer running prox in two directories at once looks like when one of
	// them inherits the other's port.
	t.Run("a prox for a different project", func(t *testing.T) {
		target := startSleeper(t)
		id := target.asLeakedDaemon(t)
		id.StateDir = filepath.Join(t.TempDir(), daemonStateDirName)
		sweep(t, target, id)
	})
}

// TestLedger_RowsFromAnotherGenerationAreNotActedOn pins the second half of the
// mixed-ledger fix.
//
// With ledgers named <pid>.jsonl, a run whose owner pid the OS had recycled
// would skip its predecessor's file at startup (the owner "looks alive" -- it IS
// alive, it is us) and then append its OWN rows to it. A concurrent sweeper read
// the owner identity from the first row, concluded the owner was dead, and acted
// on every row in the file -- including the live run's, whose daemons it would
// reap out from under it.
//
// The file name now carries the owner's start token, so two generations cannot
// share a file. This is the belt to that braces: a row whose owner identity
// disagrees with the name it was found under is inert, whatever put it there.
func TestLedger_RowsFromAnotherGenerationAreNotActedOn(t *testing.T) {
	startTest(t, defaultTestBudget)
	requireTokens(t)

	owner := startSleeper(t)
	ownerID := owner.identity()
	owner.kill(t) // the generation the FILE belongs to: dead, so the file is swept

	// The rows a second run wrote: same pid, recycled by the OS, different
	// generation -- and that run may well still be running.
	reusedOwner := daemonIdentity{PID: ownerID.PID, StartToken: ownerID.StartToken + 1}

	target := startSleeper(t)
	dir := t.TempDir()
	path := writeLedgerFile(t, dir, ownerID, ledgerEntryFor(reusedOwner, target.asLeakedDaemon(t)))

	var log bytes.Buffer
	if n := reapStaleLedgers(dir, os.Getpid(), &log); n != 0 {
		t.Fatalf("rows from another generation must not be acted on, got %d reaped\n%s", n, log.String())
	}
	// Give any (wrongly) delivered signal time to land before asserting.
	time.Sleep(200 * time.Millisecond)
	if !target.alive() {
		t.Fatal("the reaper killed a daemon belonging to a different run generation")
	}
	if fileExists(t, path) {
		t.Error("the ledger should still be removed once the generation that owns it is gone")
	}
}

// TestLedger_OwnerFromFileName pins what the sweeper is willing to treat as a
// ledger at all, since the file NAME is the sole authority on which run
// generation owns the rows inside it.
func TestLedger_OwnerFromFileName(t *testing.T) {
	startTest(t, defaultTestBudget)

	cases := []struct {
		name  string
		pid   int
		token int64
		ok    bool
	}{
		{name: "1234-99.jsonl", pid: 1234, token: 99, ok: true},
		{name: "1234-0.jsonl", pid: 1234, token: 0, ok: true},
		// A ledger written by an older build of this suite: it names a pid but
		// no generation, so it can never be proven stale. Left strictly alone.
		{name: "1234.jsonl"},
		{name: "1234-99.txt"},
		{name: "notes.txt"},
		{name: "-99.jsonl"},
		{name: "1234-.jsonl"},
		{name: "0-99.jsonl"},
		{name: "-1234-99.jsonl"},
		{name: "1234-abc.jsonl"},
		{name: "1234-99-7.jsonl"},
	}
	for _, tc := range cases {
		pid, token, ok := ledgerOwnerFromName(tc.name)
		if ok != tc.ok {
			t.Errorf("ledgerOwnerFromName(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && (pid != tc.pid || token != tc.token) {
			t.Errorf("ledgerOwnerFromName(%q) = (%d, %d), want (%d, %d)", tc.name, pid, token, tc.pid, tc.token)
		}
	}

	// And the name a real ledger is written under parses back to the run that
	// owns it -- the round trip the sweeper depends on.
	l := newRunLedger(t.TempDir(), os.Getpid())
	pid, token, ok := ledgerOwnerFromName(filepath.Base(l.path))
	if !ok || pid != l.ownerPID || token != l.ownerToken {
		t.Fatalf("this run's own ledger %q parsed as (%d, %d, %v), want (%d, %d, true)",
			filepath.Base(l.path), pid, token, ok, l.ownerPID, l.ownerToken)
	}
}

// TestLeak_StaleStateFileIsNotAdoptedByFailedDetachedLaunch is the H1 case: the
// step BEFORE any of the reaper's guards, where a launch decides which pid it
// will later ask cleanup to signal.
//
// A detached launch cannot recognize its daemon by pid -- the whole point of
// `up -d` is that the daemon is not the process the harness started. If it
// accepts any live pid a state file names, then a launch that FAILS (and so
// never writes a state file) will adopt whatever the directory already held. A
// pid in an hours-old file may since have been recycled by the OS, and adopting
// it captures that innocent process's genuinely valid start token -- after
// which every pid+token check downstream passes, on the wrong process, and
// teardown signals a bystander.
//
// The stand-in for the recycled pid is a `sleep` this test started, so even the
// failing version of this test cannot signal anything it does not own.
func TestLeak_StaleStateFileIsNotAdoptedByFailedDetachedLaunch(t *testing.T) {
	startTest(t, defaultTestBudget)

	bystander := startSleeper(t)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve project dir: %v", err)
	}
	stateDir := filepath.Join(dir, daemonStateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// A state file left behind by a generation that died an hour ago (SIGKILL
	// leaves it in place), naming a pid something unrelated now holds.
	state := fmt.Sprintf(`{"pid": %d, "port": 12345, "host": "127.0.0.1", "started_at": %q}`,
		bystander.pid, time.Now().Add(-time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(stateDir, daemonStateFileName), []byte(state), 0o600); err != nil {
		t.Fatalf("write stale state file: %v", err)
	}

	// A detaching launch that fails outright. /bin/sh rather than the prox
	// binary because the subject is the harness's adoption rule, not prox's
	// startup: what matters is a launcher that exits non-zero without ever
	// writing a state file, which is exactly what a refused `up -d` is.
	run, err := tryStartDetachedIn(t, "/bin/sh", dir, nil, "-c", "exit 1")
	if err == nil {
		t.Fatal("the launch was supposed to fail; this test's premise is a launch that wrote no state file")
	}

	if id, ok := run.tryDaemonIdentity(); ok {
		t.Fatalf("a failed launch adopted a state file that predates it: %s", id)
	}

	// Teardown therefore has nothing daemon-shaped to stop, and the process
	// holding the recycled pid is left alone.
	run.teardown()
	time.Sleep(200 * time.Millisecond)
	if !bystander.alive() {
		t.Fatal("teardown signalled a process that merely inherited a pid named by a stale state file")
	}
}
