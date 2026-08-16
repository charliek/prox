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
//  3. a cross-run ledger under ${TMPDIR}/prox-integration-runs-<uid> -- covers
//     SIGKILL, a panic in the runtime, and a power-cycle, by letting the NEXT
//     run reap what this one could not.
//
// Identity is verified by START TOKEN, never by pid alone -- for the daemons
// being killed AND for the owning test binary whose death authorizes the kill.
// A bare pid comparison is a promise to eventually SIGKILL an innocent process
// after pid reuse; daemon.IsProcessAlive(pid, token) makes that unrepresentable
// (internal/daemon/liveness.go).
//
// A token is NECESSARY but not SUFFICIENT for the cross-run sweep (layer 3),
// which is the only layer that signals pids recorded by a test binary that is
// already dead. There the record can be hours old, so pid+token proves only
// "some process by that name is alive", and it proves even that weakly:
// IsProcessAlive also answers "alive" when the CURRENT token cannot be read.
// Before that layer signals anything it therefore requires POSITIVE
// IDENTIFICATION -- the process must answer prox's own API at the recorded
// address AND report the project directory the record names (confirmProxDaemon
// below). A stranger who inherited the pid does not serve prox's API there. An
// unidentifiable row is REPORTED and left alone: a visible stray daemon is
// strictly better than a dead bystander on a developer's machine.

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
	// daemonIdentifyBudget bounds the GET /api/v1/status that positively
	// identifies a recorded pid as our prox before the cross-run sweep signals
	// it. Short on purpose: this runs before every test in the package, against
	// a loopback address, and its failure mode is benign -- an unidentified row
	// is reported and left alone rather than killed.
	daemonIdentifyBudget = 3 * time.Second
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

// killableAcrossRuns is alive() plus the one guarantee alive() cannot make:
// that the pid is still THIS process and not a stranger who inherited it.
//
// daemon.IsProcessAlive deliberately answers "alive" when the token is 0 --
// meaning ProcessStartTime could not be read when the identity was captured --
// so alive() degrades to a bare pid check in that case. Within a single run
// that is fine: the process was launched seconds ago by this test binary, and
// the teardown is aimed at something we are still holding.
//
// Across runs it is not fine. The cross-run sweep signals pids recorded by a
// test binary that is already dead, so the record can be minutes or hours old
// and the pid long since recycled by the OS into somebody's editor. A bare pid
// check there is the difference between reaping our own leak and SIGKILLing an
// unrelated process on a developer's machine. So a tokenless record is never
// signalled -- it is reported instead, which turns an invisible risk into a
// visible, actionable leak.
func (d daemonIdentity) killableAcrossRuns() bool {
	return d.StartToken != 0 && d.alive()
}

func (d daemonIdentity) String() string {
	return fmt.Sprintf("pid=%d token=%d dir=%s", d.PID, d.StartToken, d.StateDir)
}

// daemonStateSnapshot is the subset of internal/daemon.State this file reads.
type daemonStateSnapshot struct {
	PID  int    `json:"pid"`
	Port int    `json:"port"`
	Host string `json:"host"`
	// StartedAt is the daemon's own time.Now() from the moment it wrote this
	// file (internal/cli/up.go, just after the PID file is locked). It is how a
	// launch tells the state file IT caused from one a previous generation left
	// behind in the same directory; see identityFromStateDir.
	StartedAt time.Time `json:"started_at"`
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
// accepting it only if accept vouches that the recorded pid AND the moment the
// daemon recorded it belong to the launch asking.
//
// accept is what keeps a STALE state file from being adopted: a fixture
// directory can host more than one daemon generation (reap_orphans_test.go
// starts a second prox in the first one's directory), and a generation killed
// with SIGKILL leaves its state file behind.
//
// The started-at timestamp is passed alongside the pid because a pid alone
// cannot express freshness. A detached launch cannot check "is this pid the one
// I started" -- the whole point of `up -d` is that it is not -- so without a
// freshness test a launch that FAILED could adopt a dead generation's file whose
// pid the OS has since recycled, capture that innocent process's genuinely valid
// start token, and hand cleanup a bystander to signal.
func identityFromStateDir(stateDir string, accept func(pid int, startedAt time.Time) bool) (daemonIdentity, bool) {
	st, ok := readDaemonState(stateDir)
	if !ok || !accept(st.PID, st.StartedAt) {
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
// handed to something unrelated. Checking pid AND token immediately before the
// syscall shrinks that window to as little as this can portably make it.
//
// It does NOT close it. Verify-then-kill is two syscalls with no portable way to
// fuse them: Linux has pidfd_send_signal, macOS -- a CI target here and the
// platform this suite is developed on -- has no equivalent, so a pid can in
// principle be freed and reused between the check and the kill(2). What remains
// after the caller's own guards is a race that requires ALL of: the daemon
// exiting in that microsecond window, the OS recycling its pid immediately
// (pids are handed out cyclically, so this means wrapping the whole pid space),
// and -- for the cross-run sweep, the only caller that signals a process this
// binary did not start -- that same recycled pid having ALREADY answered prox's
// API at the recorded address and claimed the recorded project directory
// (confirmProxDaemon). Nothing here should be read as claiming atomicity.
//
// It reports whether it actually signalled.
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

// --- positive identification --------------------------------------------------

// proxStatusIdentity is the subset of api.StatusResponse that answers the only
// question this file asks a daemon: "are you the prox for the directory my
// record names?".
//
// ProjectDir is the field that genuinely proves it. It is the daemon's own cwd
// -- the directory whose .prox/prox.state it wrote (internal/api/handlers.go
// GetStatus, internal/cli/up.go) -- so it ties the LIVE process at this address
// back to the recorded state directory. ConfigFile does not: `prox up -c
// ../shared/prox.yaml` lets two project roots share one config, so a matching
// config path would let either one claim the other. APIVersion is checked only
// to reject a non-prox server that happens to return 200 and valid JSON.
type proxStatusIdentity struct {
	ProjectDir string `json:"project_dir"`
	APIVersion string `json:"api_version"`
}

// confirmProxDaemon reports whether the process holding id's pid really is the
// prox daemon that wrote id.StateDir, by asking it.
//
// This is the guarantee pid+token cannot make, and the reason it exists is that
// pid+token is a claim about an ARBITRARILY OLD record. The cross-run sweep acts
// on ledgers written by test binaries that are already dead; by the time it
// runs, the recorded pid may belong to a developer's editor, and
// daemon.IsProcessAlive would still say "alive" whenever the current token
// happens to be unreadable (internal/daemon/liveness.go biases toward alive by
// design, so that a live process is never falsely reaped -- the opposite of the
// bias wanted here).
//
// An HTTP round trip against the recorded address is far stronger: a stranger
// who merely inherited the pid does not serve prox's API there, and a DIFFERENT
// prox that happens to hold the port reports a different project directory. The
// second failure explains itself, which is what the caller prints.
func confirmProxDaemon(id daemonIdentity) (bool, string) {
	if id.Addr == "" {
		return false, "no API address was recorded for it, so it cannot be identified as prox"
	}
	if id.StateDir == "" {
		return false, "no state directory was recorded for it, so there is nothing to match its identity against"
	}

	ctx, cancel := context.WithTimeout(context.Background(), daemonIdentifyBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id.Addr+"/api/v1/status", nil)
	if err != nil {
		return false, fmt.Sprintf("its recorded address %s is unusable: %v", id.Addr, err)
	}
	resp, err := ctxBoundClient.Do(req)
	if err != nil {
		return false, fmt.Sprintf("nothing answered prox's API at %s: %v", id.Addr, err)
	}
	defer resp.Body.Close()
	// Bounded read: whatever is listening on that port need not be prox, and a
	// hostile or merely chatty server must not be able to stream forever inside
	// the request budget.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Sprintf("reading the status response from %s failed: %v", id.Addr, err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("%s answered HTTP %d rather than a prox status", id.Addr, resp.StatusCode)
	}

	var st proxStatusIdentity
	if json.Unmarshal(body, &st) != nil || st.APIVersion == "" {
		return false, fmt.Sprintf("whatever is listening on %s does not answer as prox", id.Addr)
	}
	if st.ProjectDir == "" {
		return false, fmt.Sprintf("the prox at %s reports no project directory, so it cannot be tied to %s",
			id.Addr, id.StateDir)
	}
	want := filepath.Dir(id.StateDir)
	if !samePath(st.ProjectDir, want) {
		return false, fmt.Sprintf("the prox at %s serves project %s, not %s", id.Addr, st.ProjectDir, want)
	}
	return true, ""
}

// samePath reports whether two paths name the same directory, preferring the
// inode over the spelling.
//
// String comparison is not enough here: on macOS a project directory under
// $TMPDIR is spelled /var/folders/... while a daemon started there reports its
// own cwd as the resolved /private/var/folders/... (verified against a real
// daemon's GET /status). Treating those as different would make every
// identification fail on that platform -- turning the safety check into a
// blanket refusal to reap. Mirrors internal/cli/root.go samePath, which compares
// the same two values for the same reason.
//
// When a path no longer exists the fallback is a plain string comparison, which
// can only produce "not the same" for the two spellings above -- i.e. the sweep
// reports the row instead of signalling it. That is the safe direction, and the
// case barely arises: the run this reaper cleans up after was SIGKILLed, so its
// t.TempDir cleanups never ran and its project directories are still on disk.
func samePath(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return cleanAbsPath(a) == cleanAbsPath(b)
}

func cleanAbsPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// --- cross-run ledger -------------------------------------------------------
//
// LOCAL-ONLY VALUE, by construction: CI runners get a fresh TMPDIR (and a fresh
// machine) per run, so a ledger written there is never read by anybody. There is
// nothing to verify in CI and no point trying. What it buys is the developer
// loop, where run N+1 runs minutes after run N was SIGKILLed on the same box.

// ledgerDirBase is the stem of the directory under ${TMPDIR} holding one JSONL
// file per test-binary run, named <owner pid>-<owner start token>.jsonl.
const ledgerDirBase = "prox-integration-runs"

// ledgerDirName is ledgerDirBase with this user's uid appended.
//
// The uid matters because os.TempDir() is usually the world-writable /tmp on
// Linux (CodeRabbit, PR #108). A fixed, predictable name there is a directory
// ANY user on the box can create first and then own, after which this run writes
// pids, API addresses and project paths into a place that user controls, and
// reads pids-to-signal back out of it. Per-uid naming means two users on one
// machine cannot land in the same directory by accident, and ledgerDirUsable
// below refuses one we do not own or that anybody else can write -- which is
// what actually closes the hole, since a name is only a convention and can still
// be squatted.
//
// The signal path was already well defended (killableAcrossRuns requires a start
// token, and confirmProxDaemon makes the process itself confirm, over prox's own
// API, that it is the daemon the row describes). This is hardening of the read
// and write paths, not a missing kill guard.
var ledgerDirName = ledgerDirBase + "-" + strconv.Itoa(os.Getuid())

// ledgerDirUsable reports whether dir is safe for this run to read pids out of
// and write pids into: a real directory (not a symlink to one), owned by us, and
// not writable by any other user.
//
// It never CREATES anything -- the caller decides that -- so a missing directory
// is "usable" and the reason is empty: there is nothing there to distrust.
func ledgerDirUsable(dir string) (bool, string) {
	// Lstat, not Stat: a symlink planted at this path would otherwise be
	// followed to a target whose ownership says nothing about the link.
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, ""
	}
	if err != nil {
		return false, fmt.Sprintf("it cannot be inspected: %v", err)
	}
	if !info.Mode().IsDir() {
		return false, fmt.Sprintf("it is not a directory (mode %s)", info.Mode())
	}
	// WRITABILITY is the property that matters: a directory another user can
	// write is one where they can plant a ledger row naming a pid for this run to
	// signal. Group/other READ is left alone deliberately, because t.TempDir()
	// hands its numbered subdirectories out as 0755 and the reaper's own tests
	// point it at exactly those.
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return false, fmt.Sprintf("it is writable by other users (mode %#o)", perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true, ""
	}
	if int(st.Uid) != os.Getuid() {
		return false, fmt.Sprintf("it is owned by uid %d rather than by this user (uid %d)", st.Uid, os.Getuid())
	}
	return true, ""
}

// ledgerFileName names the ledger belonging to one run GENERATION.
//
// The start token is in the NAME, not just in the rows, so that a run whose pid
// the OS has recycled cannot append to its predecessor's file. With <pid>.jsonl
// alone it could: the new run skips the old ledger at startup (its owner "looks
// alive" -- it IS alive, it is us) and then writes its own rows into it. A
// concurrent sweeper reading that file would take the owner identity from the
// first row, decide the owner is dead, and act on EVERY row -- including the
// live run's, whose daemons it would then reap out from under it. Two
// generations in one file is the whole bug; a token in the name makes it
// unrepresentable.
func ledgerFileName(ownerPID int, ownerToken int64) string {
	return strconv.Itoa(ownerPID) + "-" + strconv.FormatInt(ownerToken, 10) + ".jsonl"
}

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
		path:       filepath.Join(dir, ledgerFileName(ownerPID, token)),
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
	// Checked AFTER MkdirAll, because the case worth catching is a directory
	// somebody else created first: MkdirAll is a no-op on an existing path and
	// silently accepts whatever mode and owner it already had.
	if ok, why := ledgerDirUsable(l.dir); !ok {
		return fmt.Errorf("refusing to write the run ledger into %s: %s", l.dir, why)
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
	// A ledger directory this user does not own is not a ledger directory: its
	// rows are somebody else's claims about which pids to signal, and acting on
	// them is the one thing this reaper must never do on somebody else's say-so.
	if ok, why := ledgerDirUsable(dir); !ok {
		fmt.Fprintf(w, "prox integration: ignoring the run-ledger directory %s: %s. "+
			"No cross-run reaping will happen; if a stray prox is left over, stop it manually.\n", dir, why)
		return 0
	}

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
		// The FILE NAME is the sole authority on which run generation owns this
		// ledger, so the owner identity is read from it -- never from the rows,
		// which is what let a reused owner pid mix two runs into one file.
		ownerPID, ownerToken, ok := ledgerOwnerFromName(name.Name())
		if !ok || ownerPID == selfPID {
			continue
		}
		path := filepath.Join(dir, name.Name())

		entries := readLedgerFile(path, ownerPID, ownerToken)

		// Owner liveness decides everything. A live owner is a test run in
		// progress (this suite is routinely run from two terminals at once), and
		// killing ITS daemons would be a far worse bug than the leak this
		// reaper exists to fix -- so a live owner means hands off the whole file.
		if daemon.IsProcessAlive(ownerPID, ownerToken) {
			continue
		}

		for _, e := range entries {
			id := e.identity()
			if id.StartToken == 0 && id.PID > 0 && daemon.ProcessExists(id.PID) {
				// Deliberately NOT killed: without a start token this pid
				// cannot be proven to still be the daemon we recorded, and the
				// owner of this ledger is already dead, so the record may be
				// arbitrarily old. Say so rather than either killing blind or
				// staying silent.
				fmt.Fprintf(w, "prox integration: NOT reaping pid %d from dead test run %d: "+
					"no start token was captured, so it cannot be distinguished from an unrelated "+
					"process that inherited the pid. If it is a stray prox, stop it manually.\n",
					id.PID, ownerPID)
				continue
			}
			if !id.killableAcrossRuns() {
				continue
			}
			// pid+token got us this far; nothing is signalled until the process
			// itself confirms, over prox's own API, that it is the daemon this
			// row describes. Everything above is a claim about a record written
			// by a test binary that is already dead.
			if ok, why := confirmProxDaemon(id); !ok {
				fmt.Fprintf(w, "prox integration: NOT reaping pid %d from dead test run %d: %s. "+
					"It cannot be proven to be the daemon that run recorded, and a wrong signal here "+
					"would land on an unrelated process. If it is a stray prox, stop it manually.\n",
					id.PID, ownerPID, why)
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

// ledgerOwnerFromName parses "<pid>-<start token>.jsonl" into the run
// generation that owns it. Anything else in the directory -- including a
// bare "<pid>.jsonl" written by an older build of this suite, which names a pid
// but no generation -- is somebody else's business and is left strictly alone.
func ledgerOwnerFromName(name string) (int, int64, bool) {
	base, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return 0, 0, false
	}
	pidPart, tokenPart, ok := strings.Cut(base, "-")
	if !ok {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(pidPart)
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	// A zero token is legitimate: it is what a platform with no
	// ProcessStartTime implementation records, and the sweep already degrades
	// safely there (IsProcessAlive falls back to bare pid existence for the
	// owner, and every tokenless ROW is reported rather than signalled).
	token, err := strconv.ParseInt(tokenPart, 10, 64)
	if err != nil || token < 0 {
		return 0, 0, false
	}
	return pid, token, true
}

// readLedgerFile parses the entries a ledger file holds.
//
// A run that was SIGKILLed mid-write leaves a truncated last line, and that is
// the NORMAL case for the files this reaper reads -- so a malformed line is
// skipped, never fatal. Entries whose owner identity disagrees with the
// filename's are dropped too, on BOTH the pid and the start token: the file's
// name is the authority on which run generation owns it, and a row that
// contradicts it cannot be trusted to name a pid worth signalling. The token
// half is what makes a mixed ledger inert as well as unlikely -- a file that
// somehow contains two generations' rows acts on neither.
func readLedgerFile(path string, ownerPID int, ownerToken int64) []ledgerEntry {
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
		if e.PID <= 0 || e.OwnerPID != ownerPID || e.OwnerStartToken != ownerToken {
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
// answering on the address the file names AND claiming the repo root as its
// project directory -- confirmProxDaemon, the same positive identification the
// cross-run sweep requires before it signals anything.
func warnOnRepoRootDaemon(w io.Writer) bool {
	root, err := repoRootDir()
	if err != nil {
		return false
	}
	stateDir := filepath.Join(root, daemonStateDirName)
	st, ok := readDaemonState(stateDir)
	if !ok || !daemon.ProcessExists(st.PID) {
		return false
	}

	host := st.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := "http://" + net.JoinHostPort(host, strconv.Itoa(st.Port))
	if ok, _ := confirmProxDaemon(daemonIdentity{PID: st.PID, StateDir: stateDir, Addr: addr}); !ok {
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
