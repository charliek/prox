package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// linuxBootIDPath is the kernel-provided per-boot UUID. It changes on every
// reboot, so it is the boot marker used to detect a cross-boot orphan ledger
// (plan 010 D7, #67).
const linuxBootIDPath = "/proc/sys/kernel/random/boot_id"

// bootMarkerFor reads this host's boot marker with the platform and file reader
// injected, matching the sameGenerationWith seam so tests can drive both
// platforms without touching the real /proc.
//
//   - Linux: the trimmed contents of /proc/sys/kernel/random/boot_id, which
//     changes on every reboot. A read failure (minimal containers lacking that
//     file) returns ("", err) so the caller can log once and degrade to today's
//     markerless behavior.
//   - Darwin (and anything else): "". ProcessStartTime tokens there are
//     wall-clock microseconds and so are already cross-boot-unique; no marker is
//     needed and none is a positive signal.
func bootMarkerFor(goos string, readFile func(string) ([]byte, error)) (string, error) {
	if goos != "linux" {
		return "", nil
	}
	data, err := readFile(linuxBootIDPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// currentBootMarker returns this host's boot marker for the running platform,
// logging one line via logger (may be nil) if the Linux boot_id is unreadable.
// It is used by ReapOrphans to compare against the recorded marker.
func currentBootMarker(logger *slog.Logger) string {
	marker, err := bootMarkerFor(runtime.GOOS, os.ReadFile)
	if err != nil && logger != nil {
		logger.Warn("could not read boot marker; orphan ledger uses markerless behavior",
			"path", linuxBootIDPath, "err", err)
	}
	return marker
}

// childrenLedger is the on-disk envelope for the ownership ledger (plan 010 D7,
// #67). The boot marker lets the next generation detect a cross-boot ledger whose
// boot-relative start tokens (Linux) can no longer be trusted and discard it
// without signaling. An empty marker means "unknown": a legacy bare-array ledger,
// a marker that could not be read, or Darwin (where it is deliberately unused).
type childrenLedger struct {
	BootMarker string        `json:"boot_marker"`
	Children   []ChildRecord `json:"children"`
}

// ChildRecord is one entry in the ownership ledger: a process GROUP the running
// generation supervises.
type ChildRecord struct {
	Name       string `json:"name"`
	PID        int    `json:"pid"`         // group leader PID; == PGID by construction
	PGID       int    `json:"pgid"`        // == PID (Setpgid, no explicit Pgid)
	StartToken int64  `json:"start_token"` // daemon.ProcessStartTime(PID)
}

// WriteChildren marshals recs into the {boot_marker,children} envelope and writes
// it to <stateDir>/prox.children via a temp file + atomic rename (0600). The
// rename is atomic so a torn ledger read at the next startup can never mis-drive
// the reap. bootMarker is threaded in from the supervisor (read once at init) so
// the next generation can detect and safely discard a cross-boot ledger (D7, #67).
func WriteChildren(stateDir, bootMarker string, recs []ChildRecord) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	data, err := json.MarshalIndent(childrenLedger{BootMarker: bootMarker, Children: recs}, "", "  ")
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

// loadLedger reads the ownership ledger from <stateDir>/prox.children, accepting
// BOTH the current envelope ({"boot_marker":...,"children":[...]}) and the legacy
// bare JSON array written by pre-D7 generations. For a legacy array (or any
// envelope with no marker) the boot marker is unknown and returned as "". A
// missing ledger is not an error: it returns ("", nil, nil).
//
// Downgrade safety (D7): the inverse holds for a pre-D7 binary. Its LoadChildren
// unmarshaled straight into []ChildRecord; handed a D7 envelope (a JSON OBJECT)
// that fails ("cannot unmarshal object into Go value of type []ChildRecord"), so
// its ReapOrphans returns the error and SKIPS the reap entirely -- never a
// mis-signal. The envelope is therefore forward-incompatible with downgrades but
// never unsafe.
//
// A syntactically-valid object missing both fields parses as an empty markerless
// ledger. That cannot mis-signal, and losing records that way would require a
// hand-edited file: prox itself only ever writes complete envelopes atomically
// (temp + rename), so no partial write can surface here.
func loadLedger(stateDir string) (bootMarker string, recs []ChildRecord, err error) {
	data, rerr := os.ReadFile(filepath.Join(stateDir, daemon.ChildrenFileName))
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("reading children ledger: %w", rerr)
	}

	// Try the envelope first (a JSON object); an array fails to unmarshal into a
	// struct, so fall back to the legacy bare array. Only if BOTH fail is the
	// ledger genuinely corrupt.
	var env childrenLedger
	if err := json.Unmarshal(data, &env); err == nil {
		return env.BootMarker, env.Children, nil
	}
	var arr []ChildRecord
	if err := json.Unmarshal(data, &arr); err != nil {
		return "", nil, fmt.Errorf("unmarshaling children ledger: %w", err)
	}
	return "", arr, nil
}

// LoadChildren reads the ownership ledger and returns its records, accepting both
// the current envelope and the legacy bare array (see loadLedger). A missing
// ledger is not an error: it returns (nil, nil). It exists for callers that only
// need the records; ReapOrphans uses loadLedger to also obtain the boot marker
// for the cross-boot guard.
func LoadChildren(stateDir string) ([]ChildRecord, error) {
	_, recs, err := loadLedger(stateDir)
	return recs, err
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
// Cross-boot collision (#67), Linux-only: daemon.ProcessStartTime is boot-scoped
// on Linux (clock ticks since boot), while the ledger can survive a reboot, so an
// unrelated process that reused both the PID AND the same boot-relative start tick
// could collide here and be signaled. This gate does NOT defend against that on
// its own; the boot-marker envelope (plan 010 D7, ledgerDisposition) does, by
// discarding a cross-boot or legacy-markerless Linux ledger BEFORE any record
// reaches sameGeneration. Darwin tokens are wall-clock microseconds (P_starttime)
// and so are cross-boot-unique -- no collision, so Darwin keeps reaping markerless
// ledgers.
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

// ledgerAction is the disposition ReapOrphans applies to a loaded ledger.
type ledgerAction int

const (
	// ledgerReap runs the identity-checked reap over the records (same boot on
	// Linux; any boot on Darwin, where tokens are cross-boot-unique).
	ledgerReap ledgerAction = iota
	// ledgerDiscard signals NOTHING and just removes the ledger file: the recorded
	// boot-relative tokens can no longer be trusted, so the safety bias forbids
	// signaling any of them.
	ledgerDiscard
)

// ledgerDisposition decides how ReapOrphans treats a loaded ledger from the
// recorded boot marker, the current boot marker, and the platform (plan 010 D7,
// #67). It is the whole cross-boot decision matrix in one pure function:
//
//   - recorded marker non-empty and != current   -> discard (cross-boot ledger).
//   - recorded marker empty/legacy AND on Linux   -> discard (a pre-D7 or
//     markerless ledger's boot-relative tokens are unsafe through the upgrade;
//     the panel accepted one missed reap of pre-upgrade orphans, recovered by the
//     port-conflict UX, over leaving #67's collision alive).
//   - recorded marker empty/legacy on Darwin      -> reap (no collision exists;
//     P_starttime tokens are cross-boot-unique).
//   - markers match                               -> reap (same boot, unchanged).
//
// Note recorded!="" && current=="" (e.g. an unreadable current marker on Linux)
// falls to the cross-boot discard branch: unable to confirm same-boot, the safety
// bias discards rather than signals.
func ledgerDisposition(recorded, current string, isLinux bool) (action ledgerAction, reason string) {
	if recorded == "" {
		if isLinux {
			return ledgerDiscard, "legacy/markerless ledger on Linux; boot-relative start tokens are unsafe across the upgrade (#67)"
		}
		return ledgerReap, ""
	}
	if recorded == current {
		return ledgerReap, ""
	}
	return ledgerDiscard, "boot marker changed since the ledger was written (cross-boot)"
}

// ReapOrphans loads the ownership ledger written by the previous prox generation
// and reaps any process group still positively identifiable as belonging to it
// (see the package doc above). It returns the reaped and skipped records. The
// ledger is removed after the pass regardless of outcome: it has served its
// purpose, and the new generation rewrites it from its first successful launch.
//
// Before any reaping, the D7 boot-marker guard (ledgerDisposition) may discard a
// cross-boot or legacy-markerless-on-Linux ledger unsignaled (#67): its recorded
// records are surfaced as skipped so up.go still reports them, but NO signal is
// ever sent.
//
// This must be called AFTER the per-project PID-file lock is held and BEFORE the
// supervisor starts, so concurrent `prox up` invocations are serialized by that
// lock.
func ReapOrphans(stateDir string, logger *slog.Logger) (reaped, skipped []ChildRecord, err error) {
	return reapOrphansWith(stateDir, newReaper(logger), currentBootMarker(logger), runtime.GOOS == "linux")
}

// reapOrphansWith is ReapOrphans with the reaper, current boot marker, and
// platform flag injected so tests can drive the D7 decision matrix without a real
// reboot and WITHOUT ever signaling the test runner's own process group.
func reapOrphansWith(stateDir string, r *reaper, currentMarker string, isLinux bool) (reaped, skipped []ChildRecord, err error) {
	recorded, recs, err := loadLedger(stateDir)
	if err != nil {
		return nil, nil, err
	}

	switch action, reason := ledgerDisposition(recorded, currentMarker, isLinux); action {
	case ledgerDiscard:
		// Signal NOTHING. Surface the discarded records as skipped so up.go's count
		// still reflects that a port-holding orphan may remain, but the reaper is
		// never even constructed against them.
		r.logger.Info("discarding children ledger without signaling",
			"reason", reason, "recorded_marker", recorded, "current_marker", currentMarker,
			"records", len(recs))
		skipped = recs
	default: // ledgerReap
		reaped, skipped = r.reap(recs)
	}

	// Remove the old ledger after the pass (tolerates a missing file). A removal
	// failure is RETURNED to the caller (up.go logs it non-fatally) rather than
	// swallowed: a readable-but-unremovable ledger would otherwise drive a spurious
	// reap pass on every subsequent startup with no signal. The reaped/skipped
	// classification is still valid, so it is returned alongside.
	if rmErr := RemoveChildren(stateDir); rmErr != nil {
		r.logger.Warn("failed to remove children ledger after reap", "err", rmErr)
		return reaped, skipped, rmErr
	}

	return reaped, skipped, nil
}
