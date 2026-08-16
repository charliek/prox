package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/daemon"
)

// This file is plan 027 C5: making a test RUN incapable of leaking a daemon.
//
// `prox up -d` detaches by design, so nothing about killing `go test` stops the
// daemon it started: the launcher is a short-lived parent CLI that has ALREADY
// EXITED by the time any cleanup runs, and the process that matters is the
// child it recorded in <dir>/.prox/prox.state. Signalling the launcher recovers
// nothing. Hence the two identities this file introduces:
//
//	launcher -- the exec.Cmd the harness started (proxRun.cmd). For a foreground
//	            `prox up` this IS the daemon; for `up -d` it is a corpse.
//	daemon   -- {pid, startToken, stateDir, addr}, read from the state file once
//	            the launch is ready. This is what cleanup targets.
//
// Three layers, each covering what the one before it cannot:
//
//  1. per-launch teardown (shutdownDaemonLaunch below, registered by the
//     harness) -- covers a test that passes, fails, or panics;
//  2. a SIGINT/SIGTERM trap in TestMain -- covers Ctrl-C and a supervising
//     harness's `kill`;
//  3. a cross-run ledger under ${TMPDIR}/prox-integration-runs -- covers
//     SIGKILL, a panic in the runtime, and a power-cycle, by letting the NEXT
//     run reap what this one could not.
//
// Identity is verified by START TOKEN, never by pid alone -- for the daemons
// being killed AND for the owning test binary whose death authorizes the kill.
// A bare pid comparison is a promise to eventually SIGKILL an innocent process
// after pid reuse; daemon.IsProcessAlive(pid, token) makes that unrepresentable
// (internal/daemon/liveness.go).

// Budgets for teardown, named by the role each one plays. Every one of them
// bounds a wait that either completes early or means something is wrong, so
// overshooting costs only how long a genuine failure takes to report.
const (
	// daemonShutdownBudget bounds the waited POST /api/v1/shutdown?wait=true.
	// The daemon's own stop sequence has to fit inside it: it stops every
	// supervised process group and only then answers.
	daemonShutdownBudget = 45 * time.Second
	// daemonExitBudget bounds the wait for the launcher AND the daemon to
	// actually be gone after a successful shutdown response.
	daemonExitBudget = 20 * time.Second
	// daemonSignalGrace bounds the wait between SIGTERM and SIGKILL when
	// escalation is reached. A daemon that ignores SIGTERM for this long is not
	// going to honour it at all.
	daemonSignalGrace = 5 * time.Second
	// daemonUnreachableGrace is the much shorter wait used when the polite
	// request was made and FAILED. Nothing was accepted, so there is no graceful
	// sequence to wait out; the only reason to wait at all is that a daemon
	// already shutting down (which is why its socket refused us) deserves a
	// moment to finish on its own before being signalled. With no address
	// recorded at all, nothing was even attempted and the wait is zero.
	daemonUnreachableGrace = 2 * time.Second
	// launcherExitBudget bounds the wait for the LAUNCHER to be reaped once the
	// daemon is gone. For a foreground run they are the same process, so this
	// only ever absorbs the gap between exit and wait4.
	launcherExitBudget = 5 * time.Second
)

// daemonStateFileName / daemonStateDirName live in fixture_test.go.

// daemonIdentity names one daemon process GENERATION: the pid plus the
// host-and-boot-local start token that distinguishes it from whatever else may
// later hold that pid.
type daemonIdentity struct {
	PID        int
	StartToken int64
	StateDir   string
	Addr       string
}

// alive reports whether this exact generation still exists. Both the pid and
// the token must agree; see daemon.IsProcessAlive for the deliberate
// bias-toward-alive fallbacks when no token could be captured.
func (d daemonIdentity) alive() bool {
	return d.PID > 0 && daemon.IsProcessAlive(d.PID, d.StartToken)
}

func (d daemonIdentity) String() string {
	return fmt.Sprintf("pid=%d token=%d dir=%s", d.PID, d.StartToken, d.StateDir)
}

// daemonStateSnapshot is the subset of internal/daemon.State this file reads.
type daemonStateSnapshot struct {
	PID  int    `json:"pid"`
	Port int    `json:"port"`
	Host string `json:"host"`
}

// readDaemonState reads and parses <stateDir>/prox.state.
//
// The daemon writes that file with O_TRUNC rather than a rename, so a reader
// can legitimately catch it half-written; an unparseable read is reported as
// "not yet", never as an error worth failing on.
func readDaemonState(stateDir string) (daemonStateSnapshot, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, daemonStateFileName))
	if err != nil {
		return daemonStateSnapshot{}, false
	}
	var st daemonStateSnapshot
	if json.Unmarshal(data, &st) != nil || st.PID <= 0 || st.Port <= 0 {
		return daemonStateSnapshot{}, false
	}
	return st, true
}

// identityFromStateDir turns the state file in stateDir into a daemon identity,
// accepting it only if accept(pid) vouches that the recorded pid belongs to the
// launch asking.
//
// accept is what keeps a STALE state file from being adopted: a fixture
// directory can host more than one daemon generation (reap_orphans_test.go
// starts a second prox in the first one's directory), and a generation killed
// with SIGKILL leaves its state file behind.
func identityFromStateDir(stateDir string, accept func(pid int) bool) (daemonIdentity, bool) {
	st, ok := readDaemonState(stateDir)
	if !ok || !accept(st.PID) {
		return daemonIdentity{}, false
	}
	host := st.Host
	if host == "" {
		host = "127.0.0.1"
	}
	// The token is captured AFTER the pid is accepted and while the process is
	// (as far as anything can tell) still the one that wrote the file. A missing
	// token is recorded as 0, which IsProcessAlive reads as "no token captured"
	// and answers with bare pid liveness.
	token, _ := daemon.ProcessStartTime(st.PID)
	return daemonIdentity{
		PID:        st.PID,
		StartToken: token,
		StateDir:   stateDir,
		Addr:       "http://" + net.JoinHostPort(host, strconv.Itoa(st.Port)),
	}, true
}

// ctxBoundClient is for requests whose budget is longer than apiClient's
// one-request ceiling and is decided by the CALLER: a waited shutdown, a
// process stop that has to sit through the SIGTERM grace, the repo-root
// liveness probe.
//
// It carries no client-level Timeout on purpose. Every call passes a context
// that bounds it, and a client Timeout would silently cap the caller's own,
// longer budget -- which is how a healthy 12s stop turns into a 5s "context
// deadline exceeded" that reads like a broken daemon.
var ctxBoundClient = &http.Client{}

// requestWaitedShutdown posts /api/v1/shutdown?wait=true and blocks, bounded by
// ctx, until the daemon reports its stop verdict.
//
// wait=true is the whole point. The bare POST returns as soon as the daemon has
// merely BEEN ASKED (internal/api/handlers.go Shutdown, and the async-return
// test in up_test.go), so a caller that kills the process right after a bare 200
// interrupts the graceful sequence and orphans exactly the children this file
// exists to prevent.
func requestWaitedShutdown(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/shutdown?wait=true", nil)
	if err != nil {
		return err
	}
	resp, err := ctxBoundClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown returned %d", resp.StatusCode)
	}
	return nil
}

// awaitIdentityGone polls until this generation is gone or the budget expires.
func awaitIdentityGone(id daemonIdentity, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if !id.alive() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// terminateDaemon escalates: SIGTERM, then SIGKILL after daemonSignalGrace,
// RE-VERIFYING the identity immediately before each signal.
//
// The re-verification is not paranoia about a race that cannot happen: between
// deciding to kill and signalling, the daemon may exit on its own and its pid be
// handed to something unrelated. Checking pid AND token right before the syscall
// is the only thing standing between this helper and killing a stranger's
// process. It reports whether it actually signalled.
func terminateDaemon(id daemonIdentity) bool {
	if !id.alive() {
		return false
	}
	_ = syscall.Kill(id.PID, syscall.SIGTERM)
	if awaitIdentityGone(id, daemonSignalGrace) {
		return true
	}
	if id.alive() {
		_ = syscall.Kill(id.PID, syscall.SIGKILL)
		awaitIdentityGone(id, daemonSignalGrace)
	}
	return true
}

// stopDaemon is the correct teardown for one daemon generation, in the order
// the panel review demanded: ask politely and WAIT for the answer, then wait for
// the process to actually be gone, and only on expiry escalate to signals aimed
// at the daemon (never at the launcher, which for `up -d` exited long ago).
//
// It is idempotent: a daemon that is already gone short-circuits at the first
// liveness check, so repeated calls -- normal cleanup, the signal handler and
// the end-of-run sweep can all reach the same identity -- cost one syscall.
func stopDaemon(id daemonIdentity, logf func(string, ...any)) {
	if !id.alive() {
		return
	}

	// The budget to wait afterwards depends on whether anything was actually
	// asked. Waiting out the full graceful budget for a daemon that never
	// received the request just delays the signal that will end it.
	budget := time.Duration(0)
	if id.Addr != "" {
		ctx, cancel := context.WithTimeout(context.Background(), daemonShutdownBudget)
		err := requestWaitedShutdown(ctx, id.Addr)
		cancel()
		if err == nil {
			budget = daemonExitBudget
		} else {
			budget = daemonUnreachableGrace
			if logf != nil {
				logf("teardown: waited shutdown of %s failed: %v", id, err)
			}
		}
	}

	if awaitIdentityGone(id, budget) {
		return
	}
	if logf != nil {
		logf("teardown: daemon %s outlived its %v exit budget; escalating to signals", id, budget)
	}
	terminateDaemon(id)
}

// --- cross-run ledger -------------------------------------------------------
//
// LOCAL-ONLY VALUE, by construction: CI runners get a fresh TMPDIR (and a fresh
// machine) per run, so a ledger written there is never read by anybody. There is
// nothing to verify in CI and no point trying. What it buys is the developer
// loop, where run N+1 runs minutes after run N was SIGKILLed on the same box.

// ledgerDirName is the directory under ${TMPDIR} holding one JSONL file per
// test-binary run, named <owner pid>.jsonl.
const ledgerDirName = "prox-integration-runs"

// ledgerEntry is one line of a run ledger: the daemon this run started, plus
// the identity of the run that owns it.
//
// The owner's start token matters as much as the target's. A ledger is only
// swept once its owner is gone, and "gone" decided by pid alone would be
// defeated by pid reuse in BOTH directions: a reused owner pid would make a
// stale ledger look forever-live (never swept), and a dead owner whose pid is
// free would be indistinguishable from one that never existed.
type ledgerEntry struct {
	PID             int       `json:"pid"`
	StartToken      int64     `json:"start_token"`
	StateDir        string    `json:"state_dir"`
	Addr            string    `json:"addr"`
	OwnerPID        int       `json:"owner_pid"`
	OwnerStartToken int64     `json:"owner_start_token"`
	TS              time.Time `json:"ts"`
}

func (e ledgerEntry) identity() daemonIdentity {
	return daemonIdentity{PID: e.PID, StartToken: e.StartToken, StateDir: e.StateDir, Addr: e.Addr}
}

// runLedger is this test binary's append-only record of the daemons it has
// started.
type runLedger struct {
	dir        string
	path       string
	ownerPID   int
	ownerToken int64

	mu      sync.Mutex
	entries []daemonIdentity
}

// newRunLedger builds the ledger for owner pid, under dir.
func newRunLedger(dir string, ownerPID int) *runLedger {
	token, _ := daemon.ProcessStartTime(ownerPID)
	return &runLedger{
		dir:        dir,
		path:       filepath.Join(dir, strconv.Itoa(ownerPID)+".jsonl"),
		ownerPID:   ownerPID,
		ownerToken: token,
	}
}

// record appends id to the ledger file and remembers it in memory.
//
// A failure to write is reported but never fatal: the ledger is a safety net for
// a run that dies violently, and refusing to run tests because ${TMPDIR} is not
// writable would trade a rare leak for a certain outage.
func (l *runLedger) record(id daemonIdentity) error {
	if id.PID <= 0 {
		return nil
	}

	l.mu.Lock()
	l.entries = append(l.entries, id)
	l.mu.Unlock()

	entry := ledgerEntry{
		PID:             id.PID,
		StartToken:      id.StartToken,
		StateDir:        id.StateDir,
		Addr:            id.Addr,
		OwnerPID:        l.ownerPID,
		OwnerStartToken: l.ownerToken,
		TS:              time.Now(),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	// 0600: the file names pids this process will signal, so nothing else on a
	// shared machine gets to add rows to it.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// recorded returns a copy of the identities recorded so far.
func (l *runLedger) recorded() []daemonIdentity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]daemonIdentity(nil), l.entries...)
}

// reapOwn kills any daemon this run started that is somehow still alive, and
// reports how many needed it. Zero is the expected answer -- a non-zero one
// means a per-test teardown did not do its job, which is worth the noise.
func (l *runLedger) reapOwn(w io.Writer) int {
	reaped := 0
	for _, id := range l.recorded() {
		if !id.alive() {
			continue
		}
		fmt.Fprintf(w, "prox integration: daemon %s survived its test's teardown; stopping it now\n", id)
		stopDaemon(id, func(format string, args ...any) {
			fmt.Fprintf(w, "prox integration: "+format+"\n", args...)
		})
		reaped++
	}
	return reaped
}

// remove deletes this run's ledger file. Called once the run has finished (or
// has been torn down by the signal handler) and its daemons are accounted for,
// so no later run wastes time on it.
func (l *runLedger) remove() {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "prox integration: removing run ledger %s: %v\n", l.path, err)
	}
}

// packageLedger is the ledger for this test binary.
var packageLedger = newRunLedger(filepath.Join(os.TempDir(), ledgerDirName), os.Getpid())

// reapStaleLedgers sweeps every ledger in dir whose owning test binary is gone,
// killing the daemons it recorded, and returns how many it killed.
//
// selfPID's own ledger is skipped: this run is, definitionally, alive.
func reapStaleLedgers(dir string, selfPID int, w io.Writer) int {
	names, err := os.ReadDir(dir)
	if err != nil {
		// No directory means no previous run ever recorded anything here.
		return 0
	}

	reaped := 0
	for _, name := range names {
		if name.IsDir() {
			continue
		}
		ownerPID, ok := ledgerOwnerFromName(name.Name())
		if !ok || ownerPID == selfPID {
			continue
		}
		path := filepath.Join(dir, name.Name())

		entries := readLedgerFile(path, ownerPID)

		// Owner liveness decides everything. A live owner is a test run in
		// progress (this suite is routinely run from two terminals at once), and
		// killing ITS daemons would be a far worse bug than the leak this
		// reaper exists to fix -- so a live owner means hands off the whole file.
		ownerToken := int64(0)
		if len(entries) > 0 {
			ownerToken = entries[0].OwnerStartToken
		}
		if daemon.IsProcessAlive(ownerPID, ownerToken) {
			continue
		}

		for _, e := range entries {
			id := e.identity()
			if !id.alive() {
				continue
			}
			fmt.Fprintf(w, "prox integration: reaping leaked daemon %s from dead test run %d\n", id, ownerPID)
			stopDaemon(id, func(format string, args ...any) {
				fmt.Fprintf(w, "prox integration: "+format+"\n", args...)
			})
			reaped++
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(w, "prox integration: removing stale run ledger %s: %v\n", path, err)
		}
	}
	return reaped
}

// ledgerOwnerFromName parses "<pid>.jsonl". Anything else in the directory is
// somebody else's business and is left alone.
func ledgerOwnerFromName(name string) (int, bool) {
	base, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(base)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// readLedgerFile parses the entries a ledger file holds.
//
// A run that was SIGKILLed mid-write leaves a truncated last line, and that is
// the NORMAL case for the files this reaper reads -- so a malformed line is
// skipped, never fatal. Entries whose owner_pid disagrees with the filename are
// dropped too: the file's name is the authority on who owns it, and a row that
// contradicts it cannot be trusted to name a pid worth signalling.
func readLedgerFile(path string, ownerPID int) []ledgerEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []ledgerEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e ledgerEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.PID <= 0 || e.OwnerPID != ownerPID {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// --- signal-safe teardown ---------------------------------------------------

// installSignalTeardown traps SIGINT and SIGTERM so that Ctrl-C, or a `kill`
// from a supervising harness, still tears this run's daemons down.
//
// Without it, the default disposition kills the test binary instantly and every
// t.Cleanup goes unrun -- which is precisely how a developer interrupting a slow
// run ends up with a stranded daemon. Exiting non-zero is deliberate: an
// interrupted run has not passed.
func installSignalTeardown(w io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-ch
		fmt.Fprintf(w, "\nprox integration: interrupted by %v; stopping daemons started by this run\n", sig)
		packageLedger.reapOwn(w)
		packageLedger.remove()
		if sharedBinary.dir != "" {
			_ = os.RemoveAll(sharedBinary.dir)
		}
		// 128+signo, the shell convention, so a supervising harness sees an
		// interrupted run rather than a passing one.
		if sig == syscall.SIGINT {
			os.Exit(130)
		}
		os.Exit(143)
	}()
}

// --- repo-root daemon banner ------------------------------------------------

// warnOnRepoRootDaemon prints an informational banner when a live prox daemon is
// running in the repo root, and then gets out of the way.
//
// INFORMATIONAL ONLY, on purpose. The repo root has a real prox.yaml and
// developers dogfood prox here; after the per-test isolation of C2-C4 a foreign
// daemon cannot collide with anything this suite does (every test owns a private
// directory and a dynamic port), and the acceptance criterion for this plan is
// that the suite passes with a foreign `prox up` running. So this is a hint for
// reading a confusing failure, not a gate.
//
// Liveness, not file existence: an abandoned .prox/prox.state whose pid is dead
// (or has been reused by something unrelated) must NOT print anything. prox.state
// records no start token, so the confirmation here is the daemon's own API
// answering on the address the file names -- a stronger check than a token, since
// a reused pid does not serve prox's API.
func warnOnRepoRootDaemon(w io.Writer) bool {
	root, err := repoRootDir()
	if err != nil {
		return false
	}
	st, ok := readDaemonState(filepath.Join(root, daemonStateDirName))
	if !ok || !daemon.ProcessExists(st.PID) {
		return false
	}

	host := st.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := "http://" + net.JoinHostPort(host, strconv.Itoa(st.Port))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/v1/status", nil)
	if err != nil {
		return false
	}
	resp, err := ctxBoundClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false
	}

	bar := strings.Repeat("=", 72)
	fmt.Fprintf(w, "\n%s\n", bar)
	fmt.Fprintf(w, "prox integration: a live prox daemon (pid %d, api %s) is running in the\n", st.PID, addr)
	fmt.Fprintf(w, "repo root %s. That is fine -- every test here owns a private\n", root)
	fmt.Fprintf(w, "directory and a dynamic port -- and this run will NOT touch it.\n")
	fmt.Fprintf(w, "%s\n\n", bar)
	return true
}

// repoRootDir is repoRoot without a *testing.T, for use from TestMain.
func repoRootDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Join(wd, "..", ".."))
}
