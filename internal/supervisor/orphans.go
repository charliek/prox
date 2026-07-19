package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/daemon"
)

// Orphan reaping (plan 009 D11-D13, issue #59).
//
// The graceful shutdown path (ManagedProcess.Stop) already escalates SIGTERM ->
// SIGKILL over the whole process group and reaps stubborn grandchildren. But
// SIGKILL is uncatchable: when a `prox up` generation is killed with `kill -9`,
// its graceful Stop never runs, so the backend process GROUPS it supervised
// orphan and keep holding their ports -- the next `prox up` then hits EADDRINUSE
// / 502s.
//
// To close that gap the running supervisor persists an ownership ledger of the
// group-leader PIDs it supervises (see Supervisor.persistChildren). On the next
// startup, ReapOrphans reads that ledger and reaps any group still positively
// identifiable as belonging to the prior generation. The ledger is the ONLY
// record that survives a SIGKILL, since PGIDs are otherwise held only in memory.
//
// Safety bias: the reaper never signals a group it cannot positively identify as
// ours. Identity is verified ONCE, up front, via a strict start-token match
// (sameGeneration); everything else is skipped and left for the operator.

const (
	// reapGraceWindow is how long ReapOrphans waits for a positively-identified
	// group to exit after SIGTERM before escalating to SIGKILL.
	reapGraceWindow = 2 * time.Second
	// reapPollInterval is how often the grace window re-probes group liveness.
	reapPollInterval = 50 * time.Millisecond
)

// ChildRecord is one entry in the ownership ledger: a process GROUP the running
// generation supervises.
type ChildRecord struct {
	Name       string `json:"name"`
	PID        int    `json:"pid"`         // group leader PID; == PGID by construction
	PGID       int    `json:"pgid"`        // == PID (Setpgid, no explicit Pgid)
	StartToken int64  `json:"start_token"` // daemon.ProcessStartTime(PID)
}

// WriteChildren marshals recs to JSON and writes them to
// <stateDir>/prox.children via a temp file + atomic rename (0600). The rename
// is atomic so a torn ledger read at the next startup can never mis-drive the
// reap.
func WriteChildren(stateDir string, recs []ChildRecord) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling children ledger: %w", err)
	}

	tmp, err := os.CreateTemp(stateDir, daemon.ChildrenFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp children ledger: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: after a successful rename tmpName no longer exists and
	// this Remove is a harmless no-op; on any failure below it removes the stray.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp children ledger: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing children ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing children ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing children ledger: %w", err)
	}

	if err := os.Rename(tmpName, filepath.Join(stateDir, daemon.ChildrenFileName)); err != nil {
		return fmt.Errorf("renaming children ledger: %w", err)
	}
	return nil
}

// LoadChildren reads the ownership ledger from <stateDir>/prox.children. A
// missing ledger is not an error: it returns (nil, nil).
func LoadChildren(stateDir string) ([]ChildRecord, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, daemon.ChildrenFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading children ledger: %w", err)
	}

	var recs []ChildRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("unmarshaling children ledger: %w", err)
	}
	return recs, nil
}

// RemoveChildren deletes the ownership ledger. A missing ledger is tolerated.
func RemoveChildren(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, daemon.ChildrenFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing children ledger: %w", err)
	}
	return nil
}

// sameGeneration is the STRICT positive identity predicate for a ledger record:
// it reports true only when pid still names the SAME process generation that
// recorded token. It biases hard toward NOT killing -- a non-positive pid, a
// zero (never-captured) token, an unreadable current token, or any mismatch all
// return false. This is the single gate that decides whether the reaper is
// allowed to signal a group at all.
//
// Accepted residual (plan §9), Linux-only: daemon.ProcessStartTime is boot-scoped
// on Linux (clock ticks since boot), while the ledger can survive a reboot. After
// a reboot the recorded orphans are already dead (nothing survives a reboot), so
// the reap has no upside; but an unrelated process that reused both the PID AND
// the same boot-relative start tick would collide here and be signaled. Darwin
// tokens are wall-clock microseconds (P_starttime) and so are cross-boot-unique --
// no collision. Persisting a boot marker to discard a cross-boot ledger is a
// recommended follow-up.
func sameGeneration(pid int, token int64) bool {
	return sameGenerationWith(daemon.ProcessStartTime, pid, token)
}

// sameGenerationWith is sameGeneration with an injectable start-token reader so
// the reaper (and its tests) can verify identity without touching real PIDs.
func sameGenerationWith(startTime func(pid int) (int64, bool), pid int, token int64) bool {
	if pid <= 0 || token == 0 {
		return false
	}
	cur, ok := startTime(pid)
	return ok && cur == token
}

// reaper performs the ledger-driven orphan reap. Its signal/liveness/identity
// operations are injectable function fields so unit tests can drive the full
// escalation WITHOUT ever signaling the test runner's own process group.
type reaper struct {
	// killpg signals the process group led by pgid. The real implementation is
	// syscall.Kill(-pgid, sig).
	killpg func(pgid int, sig syscall.Signal) error
	// groupAlive reports whether any member of the group led by pgid is still
	// alive (signal-0 probe; conservative "alive" on ambiguous errors).
	groupAlive func(pgid int) bool
	// startTime reads a process's opaque generation token.
	startTime func(pid int) (int64, bool)

	grace  time.Duration
	poll   time.Duration
	logger *slog.Logger
}

// newReaper returns a reaper wired to the real syscalls and daemon token reader.
func newReaper(logger *slog.Logger) *reaper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &reaper{
		killpg:     func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) },
		groupAlive: realGroupAlive,
		startTime:  daemon.ProcessStartTime,
		grace:      reapGraceWindow,
		poll:       reapPollInterval,
		logger:     logger,
	}
}

// realGroupAlive probes the group led by pgid with signal 0. It mirrors
// execProcess.GroupAlive's mapping: ESRCH => gone, anything else (including
// EPERM and transient errors) => conservatively alive so a probe failure never
// masquerades as a confirmed reap.
func realGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, syscall.Signal(0))
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.ESRCH):
		return false
	default:
		return true
	}
}

// reap processes every ledger record, returning the groups it confirmed reaped
// and the ones it did NOT reap ("skipped"). A skipped record is EITHER one that
// was never signaled (not positively identified as belonging to the prior
// generation) OR one that was positively ours but could not be confirmed gone
// after the SIGTERM->SIGKILL escalation (a rare wedged survivor). It never
// signals a group that is not positively identified.
func (r *reaper) reap(recs []ChildRecord) (reaped, skipped []ChildRecord) {
	for _, rec := range recs {
		// Reject corrupt/planted records before any signal. PGID must equal the
		// leader PID by construction (Setpgid without an explicit Pgid), so a
		// record whose PGID != PID -- or whose PID is non-positive -- is untrusted
		// and never signaled. PGID > 0 alone is INSUFFICIENT.
		if rec.PID <= 0 || rec.PGID != rec.PID {
			r.logger.Warn("skipping corrupt child ledger record; not reaping",
				"name", rec.Name, "pid", rec.PID, "pgid", rec.PGID)
			skipped = append(skipped, rec)
			continue
		}

		// Verify identity ONCE, immediately before the first signal. If it does
		// not positively match, skip -- we never signal a group we cannot prove is
		// ours.
		if !sameGenerationWith(r.startTime, rec.PID, rec.StartToken) {
			r.logger.Warn("leftover child could not be positively identified; not reaping — if a port is wedged, kill it manually",
				"name", rec.Name, "pid", rec.PID)
			skipped = append(skipped, rec)
			continue
		}

		// Positively ours: escalate SIGTERM -> (grace) -> SIGKILL over the group.
		// We deliberately do NOT re-verify sameGeneration before SIGKILL: the
		// stubborn-grandchild leader EXITS on SIGTERM, making its token
		// unverifiable, so re-verifying would skip the SIGKILL and leave the port
		// wedged. Ownership rests on this ONE up-front identity check plus the
		// group's continued liveness through the escalation -- exactly how the
		// graceful ManagedProcess.Stop already escalates. Accepted residual (plan
		// §9): liveness is SAMPLED, so if the owned group fully exits and an
		// unrelated process reuses the exact PGID within a grace-window gap, the
		// escalation could signal the replacement. This is the same unavoidable
		// user-space check-to-signal TOCTOU as the graceful path; the strict
		// up-front identity check minimizes but cannot eliminate it.
		if r.reapGroup(rec) {
			reaped = append(reaped, rec)
		} else {
			// Escalation did not confirm the group gone (e.g. SIGKILL errored and
			// the group is still present). Do NOT report it reaped or drop it
			// silently -- surface it as skipped so the operator and the up.go count
			// see that a port-holding orphan may remain.
			skipped = append(skipped, rec)
		}
	}
	return reaped, skipped
}

// reapGroup runs the SIGTERM -> SIGKILL escalation for one positively-identified
// group. If the whole group disappears during the grace window it stops without
// SIGKILL. It returns true iff the group was CONFIRMED gone -- callers must treat
// a false return as "the orphan may still be holding its port" (not reaped).
func (r *reaper) reapGroup(rec ChildRecord) bool {
	if err := r.killpg(rec.PGID, syscall.SIGTERM); err != nil {
		r.logger.Warn("SIGTERM to leftover child group failed",
			"name", rec.Name, "pgid", rec.PGID, "err", err)
	}

	if r.waitGroupGone(rec.PGID) {
		r.logger.Info("reaped leftover child group (exited on SIGTERM)",
			"name", rec.Name, "pgid", rec.PGID)
		return true
	}

	if err := r.killpg(rec.PGID, syscall.SIGKILL); err != nil {
		r.logger.Warn("SIGKILL to leftover child group failed",
			"name", rec.Name, "pgid", rec.PGID, "err", err)
	}
	// Confirm the group is actually gone (SIGKILL is uncatchable, so this returns
	// promptly) before the caller removes the ledger and the new generation
	// launches -- otherwise a still-dying group could still hold a port the
	// replacement needs to rebind.
	if r.waitGroupGone(rec.PGID) {
		r.logger.Info("reaped leftover child group (escalated to SIGKILL)",
			"name", rec.Name, "pgid", rec.PGID)
		return true
	}
	r.logger.Warn("leftover child group still present after SIGKILL",
		"name", rec.Name, "pgid", rec.PGID)
	return false
}

// waitGroupGone polls group liveness on r.poll until the group is gone or the
// grace window passes. It returns true iff the group is gone.
func (r *reaper) waitGroupGone(pgid int) bool {
	deadline := time.Now().Add(r.grace)
	for {
		if !r.groupAlive(pgid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(r.poll)
	}
}

// ReapOrphans loads the ownership ledger written by the previous prox generation
// and reaps any process group still positively identifiable as belonging to it
// (see the package doc above). It returns the reaped and skipped records. The
// ledger is removed after the pass regardless of outcome: it has served its
// purpose, and the new generation rewrites it from its first successful launch.
//
// This must be called AFTER the per-project PID-file lock is held and BEFORE the
// supervisor starts, so concurrent `prox up` invocations are serialized by that
// lock.
func ReapOrphans(stateDir string, logger *slog.Logger) (reaped, skipped []ChildRecord, err error) {
	recs, err := LoadChildren(stateDir)
	if err != nil {
		return nil, nil, err
	}

	r := newReaper(logger)
	reaped, skipped = r.reap(recs)

	// Remove the old ledger after the pass (tolerates a missing file). A removal
	// failure is RETURNED to the caller (up.go logs it non-fatally) rather than
	// swallowed: a readable-but-unremovable ledger would otherwise drive a spurious
	// reap pass on every subsequent startup with no signal (codex review). The
	// reaped/skipped classification is still valid, so it is returned alongside.
	if rmErr := RemoveChildren(stateDir); rmErr != nil {
		r.logger.Warn("failed to remove children ledger after reap", "err", rmErr)
		return reaped, skipped, rmErr
	}

	return reaped, skipped, nil
}
