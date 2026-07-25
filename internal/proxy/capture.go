// Package proxy provides an HTTPS reverse proxy with subdomain-based routing.
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
)

// CaptureManager handles request/response body capture with hybrid memory/disk storage.
type CaptureManager struct {
	mu              sync.RWMutex
	enabled         bool
	maxBodySize     int64
	inlineThreshold int64
	captureDir      string
	workDir         string
	// acct is the disk-budget accountant (#69): it is the single chokepoint every
	// spilled body file passes through, tracking logical spill bytes and evicting
	// the oldest record groups (oldest-first FIFO by first-spill time) whenever the
	// daemon-wide budget would be exceeded. Always non-nil (both constructors
	// initialize it); on a disabled manager it simply never sees a spill.
	acct *diskAccountant
}

// spillGroup accounts one record's spilled body files (#69). A record spills its
// request and response bodies at two INDEPENDENT times on concurrent goroutines,
// so the group is created on whichever spills first and grows to hold both. Its
// age is the FIRST spill time (never bumped by the second file, and never
// promoted by reads — this is FIFO, deliberately NOT LRU), which is what
// eviction orders by. seq is a strictly-monotonic creation stamp used only as a
// deterministic tiebreaker when two groups share a first-spill instant.
type spillGroup struct {
	firstSpill time.Time
	seq        uint64
	files      map[string]int64 // suffix ("_req"/"_res") -> accounted byte length
}

// diskAccountant enforces the capture disk budget across ALL records sharing one
// capture dir (the daemon's flat ~/.prox/capture, or a standalone project's dir).
// capture_disk_used is the sum of managed file lengths; the budget bounds it.
// Its own mutex guards every field because spills land on concurrent request
// goroutines (#69). Ring records, in-memory metadata, and inline bodies are NEVER
// touched here — only spilled files and their accounting.
type diskAccountant struct {
	mu         sync.Mutex
	dir        string
	budget     int64
	diskUsed   int64
	seqCounter uint64
	groups     map[string]*spillGroup

	// Injectable os seams so tests can force write/remove/stat failures without
	// real permission juggling; default to the real os funcs.
	writeFile func(name string, data []byte, perm os.FileMode) error
	remove    func(name string) error
	stat      func(name string) (os.FileInfo, error)
	now       func() time.Time
}

// spillSuffixes are the two canonical spill-file suffixes for a record. Cleanup
// and eviction unlink BOTH regardless of which are tracked (#69), so a partial
// file that never made it into accounting is still removed.
var spillSuffixes = []string{"_req", "_res"}

// spillFilePath is the single source of truth for a spilled body file's on-disk
// name: dir/<requestID><suffix>.bin (suffix is "_req"/"_res"). Centralizing it
// keeps the accountant's write, delete, and error-message paths from drifting.
func spillFilePath(dir, requestID, suffix string) string {
	return filepath.Join(dir, requestID+suffix+".bin")
}

// newDiskAccountant builds an accountant rooted at dir with the given budget
// (a non-positive budget falls back to the default). (#69)
func newDiskAccountant(dir string, budget int64) *diskAccountant {
	if budget <= 0 {
		budget = constants.DefaultCaptureDiskBudget
	}
	return &diskAccountant{
		dir:       dir,
		budget:    budget,
		groups:    make(map[string]*spillGroup),
		writeFile: os.WriteFile,
		remove:    os.Remove,
		stat:      os.Stat,
		now:       time.Now,
	}
}

// ensureGroupLocked returns requestID's group, creating it (stamping the
// first-spill time and monotonic seq) on first use. a.mu must be held.
func (a *diskAccountant) ensureGroupLocked(requestID string) *spillGroup {
	g := a.groups[requestID]
	if g == nil {
		a.seqCounter++
		g = &spillGroup{firstSpill: a.now(), seq: a.seqCounter, files: make(map[string]int64)}
		a.groups[requestID] = g
	}
	return g
}

// trackLocked records that suffix's on-disk file for requestID is size bytes,
// adjusting diskUsed by the delta from any prior tracked size (a rewrite is not
// double-counted). a.mu must be held.
func (a *diskAccountant) trackLocked(requestID, suffix string, size int64) {
	g := a.ensureGroupLocked(requestID)
	a.diskUsed += size - g.files[suffix]
	g.files[suffix] = size
}

// store writes data to the spill file for (requestID, suffix), updates
// accounting, and enforces the budget, returning the file path on success. On a
// write failure it removes any partial file so accounting still matches disk and
// returns the error (the caller falls back to inline storage). A repeated suffix
// (rewrite) adjusts the delta rather than double-counting. (#69)
func (a *diskAccountant) store(requestID, suffix string, data []byte) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	path := spillFilePath(a.dir, requestID, suffix)
	if err := a.writeFile(path, data, constants.FilePermissionPrivate); err != nil {
		// Remove any partial write so a failed WRITE never leaves bytes on disk
		// that accounting does not know about. If that removal ALSO fails (e.g. the
		// partial is there but unlinkable), stat it and TRACK it into the group so
		// budget eviction / CleanupRequest can retry the delete — otherwise the
		// orphan is invisible to accounting and eviction forever (#69).
		if rmErr := a.remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			if fi, statErr := a.stat(path); statErr == nil {
				a.trackLocked(requestID, suffix, fi.Size())
			}
		}
		return "", err
	}

	a.trackLocked(requestID, suffix, int64(len(data)))
	a.enforceLocked()
	return path, nil
}

// removeGroupLocked deletes a record's spill files and subtracts their accounted
// bytes. Bytes are subtracted (and the file dropped from the group) only after a
// successful delete or os.IsNotExist, so a genuinely failed delete keeps the
// accounting matching disk. Idempotent: an absent group, or files already gone,
// is a no-op — making a double cleanup (budget-evicted then ring-evicted/purged)
// safe. a.mu must be held. (#69)
func (a *diskAccountant) removeGroupLocked(requestID string) {
	g := a.groups[requestID]
	// Attempt to unlink BOTH canonical spill paths regardless of what is tracked,
	// so a partial file that never made it into accounting (a failed write whose
	// partial-cleanup also failed, then a later untracked attempt) is still
	// cleaned. IsNotExist counts as a successful delete. Only TRACKED bytes are
	// subtracted from accounting (#69).
	for _, suffix := range spillSuffixes {
		path := spillFilePath(a.dir, requestID, suffix)
		err := a.remove(path)
		deleted := err == nil || os.IsNotExist(err)
		if g == nil || !deleted {
			// A real delete failure of a tracked file keeps its byte accounting
			// AND its group entry so diskUsed still reflects what is on disk.
			continue
		}
		if size, tracked := g.files[suffix]; tracked {
			a.diskUsed -= size
			delete(g.files, suffix)
		}
	}
	if g != nil && len(g.files) == 0 {
		delete(a.groups, requestID)
	}
}

// removeGroup is the lock-taking wrapper around removeGroupLocked, used by
// CleanupRequest so the manager never reaches into a.mu itself. (#69)
func (a *diskAccountant) removeGroup(requestID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removeGroupLocked(requestID)
}

// enforceLocked evicts the oldest record groups (by first-spill time, seq as
// tiebreaker) until diskUsed <= budget. Oldest-record-group-first FIFO across
// ALL records in the shared dir — a single spill larger than the whole budget is
// evicted by this same loop as the oldest-and-only group (no special case). Only
// spilled files + accounting are touched; a body fetch after eviction hits the
// existing evicted-file handling. a.mu must be held. (#69)
func (a *diskAccountant) enforceLocked() {
	if a.budget <= 0 {
		return
	}
	for a.diskUsed > a.budget && len(a.groups) > 0 {
		var oldestID string
		var oldest *spillGroup
		for id, g := range a.groups {
			if oldest == nil || g.firstSpill.Before(oldest.firstSpill) ||
				(g.firstSpill.Equal(oldest.firstSpill) && g.seq < oldest.seq) {
				oldestID, oldest = id, g
			}
		}
		before := a.diskUsed
		a.removeGroupLocked(oldestID)
		if a.diskUsed >= before {
			// No progress — every file in the oldest group failed to delete.
			// Stop rather than spin; the bounded overshoot is acceptable and the
			// next spill/mutation re-attempts convergence.
			break
		}
	}
}

// setBudget updates the budget (a non-positive value resets to the default) and
// enforces it immediately, so a budget-lowering re-register converges without
// waiting for the next spill. (#69)
func (a *diskAccountant) setBudget(budget int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if budget <= 0 {
		budget = constants.DefaultCaptureDiskBudget
	}
	a.budget = budget
	a.enforceLocked()
}

func (a *diskAccountant) used() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.diskUsed
}

func (a *diskAccountant) currentBudget() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.budget
}

// stats returns disk-used and budget read under ONE lock acquisition, so a
// consumer (daemon /status) never publishes a used/budget pair that never
// coexisted — e.g. a used from before an eviction with a budget from after (#69).
func (a *diskAccountant) stats() (used, budget int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.diskUsed, a.budget
}

// NewCaptureManager creates a new capture manager.
// If cfg is nil or capture is not enabled, returns a manager that does nothing.
//
// This constructor treats workDir as a WORK directory: the capture directory is
// derived as workDir/.prox/capture. Callers that already hold an exact capture
// directory (e.g. the shared daemon, whose capture dir is ~/.prox/capture) must
// use NewCaptureManagerAt instead to avoid a doubled ".prox/capture" suffix.
func NewCaptureManager(cfg *config.CaptureConfig, workDir string) (*CaptureManager, error) {
	if cfg == nil || !cfg.Enabled {
		return &CaptureManager{
			workDir:         workDir,
			enabled:         false,
			maxBodySize:     constants.DefaultCaptureMaxBodySize,
			inlineThreshold: constants.DefaultCaptureInlineThreshold,
			// A disabled manager still carries an accountant so budget accessors
			// are always safe; it simply never sees a spill.
			acct: newDiskAccountant("", constants.DefaultCaptureDiskBudget),
		}, nil
	}

	maxBodySize := int64(constants.DefaultCaptureMaxBodySize)

	// Parse max body size if configured
	if cfg.MaxBodySize != "" {
		size, err := config.ParseSize(cfg.MaxBodySize)
		if err != nil {
			return nil, err
		}
		if size > 0 {
			maxBodySize = size
		}
	}

	captureDir := filepath.Join(workDir, constants.CaptureDirectory)
	cm, err := NewCaptureManagerAt(captureDir, maxBodySize)
	if err != nil {
		return nil, err
	}
	cm.workDir = workDir

	// Standalone per-project disk budget (#69): honor the project's configured
	// disk_budget, falling back to the default when unset or unparseable. Unlike
	// the shared daemon's cross-project min, a single standalone project's
	// explicit value is used as-is (it is the only writer to its own dir).
	if cfg.DiskBudget != "" {
		if budget, berr := config.ParseSize(cfg.DiskBudget); berr == nil && budget > 0 {
			cm.SetDiskBudget(budget)
		}
	}
	return cm, nil
}

// NewCaptureManagerAt creates an enabled capture manager rooted at an EXACT
// capture directory (no ".prox/capture" suffix is appended). It is the shared
// setup that NewCaptureManager delegates to once it has resolved the capture
// directory and body-size limit. Any existing files under captureDir are removed
// (previous-run cleanup) and the directory is created.
func NewCaptureManagerAt(captureDir string, maxBodySize int64) (*CaptureManager, error) {
	if maxBodySize <= 0 {
		maxBodySize = constants.DefaultCaptureMaxBodySize
	}

	cm := &CaptureManager{
		enabled:         true,
		maxBodySize:     maxBodySize,
		inlineThreshold: constants.DefaultCaptureInlineThreshold,
		captureDir:      captureDir,
		acct:            newDiskAccountant(captureDir, constants.DefaultCaptureDiskBudget),
	}

	// Clean up any existing capture files from a previous run.
	if err := cm.Cleanup(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Create capture directory.
	if err := os.MkdirAll(cm.captureDir, constants.DirPermissionPrivate); err != nil {
		return nil, err
	}

	return cm, nil
}

// CaptureDir returns the directory where captured body files are stored, or the
// empty string when capture is disabled. Used by consumers building the
// LoadCapturedBody allowlist.
func (cm *CaptureManager) CaptureDir() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.captureDir
}

// Enabled returns whether capture is enabled.
func (cm *CaptureManager) Enabled() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.enabled
}

// SetDiskBudget updates the capture disk budget and enforces it immediately (#69):
// a budget-lowering re-register (or standalone construction) evicts oldest record
// groups until the total spilled bytes fit, without waiting for the next spill. A
// non-positive budget resets to DefaultCaptureDiskBudget.
func (cm *CaptureManager) SetDiskBudget(budget int64) {
	cm.acct.setBudget(budget)
}

// DiskUsed returns the total logical bytes of spilled capture body files
// currently accounted (capture_disk_used, #69).
func (cm *CaptureManager) DiskUsed() int64 {
	return cm.acct.used()
}

// DiskBudget returns the current effective capture disk budget in bytes (#69).
func (cm *CaptureManager) DiskBudget() int64 {
	return cm.acct.currentBudget()
}

// DiskStats returns capture_disk_used and capture_disk_budget read under ONE
// accountant lock (#69), so a consumer never publishes a used/budget pair that
// never coexisted. The daemon /status handler uses this rather than separate
// DiskUsed()/DiskBudget() calls.
func (cm *CaptureManager) DiskStats() (used, budget int64) {
	return cm.acct.stats()
}

// effectiveLimit resolves a per-call capture cap: a positive limit is used
// verbatim, and 0 (or negative) falls back to the manager's configured
// maxBodySize (which itself defaults to DefaultCaptureMaxBodySize). The daemon
// passes each route's own cap here via CapturePolicy.MaxBodySize (D13, #49); the
// standalone in-process proxy passes 0, keeping the project's configured cap.
// maxBodySize is set once at construction and never mutated, so no lock is
// needed.
func (cm *CaptureManager) effectiveLimit(limit int64) int64 {
	if limit > 0 {
		return limit
	}
	return cm.maxBodySize
}

// CaptureRequest captures the request body using a TeeReader bounded by
// policy.MaxBodySize (D13, #49) and returns the REDACTED, cloned request headers
// per the policy (plan 012 D4): the daemon passes the matched route's policy so
// each project honors its own cap and redaction rules through the one shared
// capture dir; the standalone proxy passes its project policy. A MaxBodySize of
// 0 falls back to the manager's configured cap (see effectiveLimit).
//
// A DISABLED manager returns the original body and a plain (unredacted) header
// clone — nothing is stored, so there is nothing to redact. When enabled, even a
// bodyless request (every GET/HEAD, whose headers still reach the stored
// Details) has its headers redacted here (plan 012 D4).
//
// Returns the captured body info and a new ReadCloser to use in place of the
// original body; reading the returned ReadCloser also captures the data.
func (cm *CaptureManager) CaptureRequest(requestID string, r *http.Request, policy CapturePolicy) (*CapturedBody, io.ReadCloser, http.Header) {
	if !cm.enabled {
		return nil, r.Body, cloneHeaders(r.Header)
	}
	if r.Body == nil {
		return nil, r.Body, policy.redactHeaders(r.Header)
	}

	headers := policy.redactHeaders(r.Header)
	contentType := r.Header.Get("Content-Type")

	// Create a buffer to capture the body
	captured := &captureBuffer{
		maxSize:   cm.effectiveLimit(policy.MaxBodySize),
		requestID: requestID,
		suffix:    "_req",
		cm:        cm,
	}

	// Wrap the body with a TeeReader
	teeReader := io.TeeReader(r.Body, captured)
	wrappedBody := &captureReadCloser{
		Reader:   teeReader,
		Closer:   r.Body,
		captured: captured,
	}

	// We return a placeholder body info; the actual data will be filled after reading completes
	body := &CapturedBody{
		ContentType:     contentType,
		ContentEncoding: r.Header.Get("Content-Encoding"),
	}

	captured.body = body
	return body, wrappedBody, headers
}

// WrapResponseWriter wraps w in a CaptureResponseWriter that records up to
// policy.MaxBodySize bytes (D13, #49) while forwarding all writes downstream: the
// daemon passes the matched route's policy so each project honors its own quota,
// and a MaxBodySize of 0 falls back to the manager's configured cap (see
// effectiveLimit). The policy's redaction fields are consulted later, at
// FinalizeResponse — the writer only needs the byte cap. The returned writer
// preserves http.Flusher/Hijacker/Pusher/Unwrap behavior.
func (cm *CaptureManager) WrapResponseWriter(w http.ResponseWriter, policy CapturePolicy) *CaptureResponseWriter {
	return newCaptureResponseWriter(w, cm.effectiveLimit(policy.MaxBodySize))
}

// FinalizeResponse captures the response body from a CaptureResponseWriter and
// returns the REDACTED, cloned response headers per the policy (plan 012 D4).
// Should be called after the response has been fully written. A disabled manager
// returns a plain (unredacted) header clone — nothing is stored.
func (cm *CaptureManager) FinalizeResponse(requestID string, crw *CaptureResponseWriter, policy CapturePolicy) (*CapturedBody, http.Header) {
	if !cm.enabled {
		return nil, cloneHeaders(crw.Header())
	}

	headers := policy.redactHeaders(crw.Header())
	contentType := crw.Header().Get("Content-Type")
	data := crw.CapturedBody()

	body := &CapturedBody{
		Size:            crw.TotalSeen(),
		CapturedSize:    int64(len(data)),
		Truncated:       crw.Truncated(),
		ContentType:     contentType,
		ContentEncoding: crw.Header().Get("Content-Encoding"),
		IsBinary:        isBinaryContent(data, contentType),
	}

	// Determine if we should store inline or on disk. Spills route through the
	// accountant (#69) so the write is byte-accounted and budget-enforced.
	if int64(len(data)) <= cm.inlineThreshold {
		body.Data = data
	} else if filePath, err := cm.acct.store(requestID, "_res", data); err == nil {
		body.FilePath = filePath
	} else {
		// Fall back to inline if disk write fails.
		body.Data = data
	}

	return body, headers
}

// LoadBody loads a captured body's data, reading from disk if necessary.
// Returns a copy of the data to prevent callers from modifying the original.
// FilePath bodies are constrained to the manager's own capture directory via
// LoadCapturedBody's allowlist.
func (cm *CaptureManager) LoadBody(body *CapturedBody) ([]byte, error) {
	return LoadCapturedBody(body, []string{cm.CaptureDir()})
}

// CleanupRequest removes disk files associated with a specific request, routing
// through the accountant (#69) so the freed bytes are subtracted from
// capture_disk_used. Idempotent — a record already budget-evicted is a safe
// no-op — so a ring eviction or PurgeByProject following a budget eviction does
// not double-count.
func (cm *CaptureManager) CleanupRequest(requestID string) {
	if !cm.enabled || cm.captureDir == "" {
		return
	}
	cm.acct.removeGroup(requestID)
}

// Cleanup removes the entire capture directory. It takes the accountant lock so
// the whole-dir removal and the accounting reset are atomic against concurrent
// spills (#69): a store() that raced in first is fully accounted, and one that
// races AFTER this fails its write with ENOENT (the dir is gone) → the
// failed-write inline fallback, never a tracked FilePath into a directory that no
// longer exists. groups/diskUsed are reset to zero so DiskUsed() does not go
// stale after cleanup.
func (cm *CaptureManager) Cleanup() error {
	if cm.captureDir == "" {
		return nil
	}
	cm.acct.mu.Lock()
	defer cm.acct.mu.Unlock()
	err := os.RemoveAll(cm.captureDir)
	cm.acct.groups = make(map[string]*spillGroup)
	cm.acct.diskUsed = 0
	return err
}

// captureBuffer is a write buffer that captures up to maxSize bytes.
// It is safe for concurrent use via the embedded mutex.
type captureBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	maxSize   int64
	truncated bool
	finalized bool
	totalSeen int64 // total bytes observed across all writes, counting past truncation
	requestID string
	suffix    string
	cm        *CaptureManager
	body      *CapturedBody
}

func (cb *captureBuffer) Write(p []byte) (n int, err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Once finalized, the CapturedBody snapshot is frozen: late writes from a
	// transport goroutine still draining a canceled request must not mutate
	// state that a recorded (and possibly already-serialized) body points at.
	if cb.finalized {
		return len(p), nil
	}

	// Count every byte observed, including data discarded after truncation.
	cb.totalSeen += int64(len(p))

	if cb.truncated || len(p) == 0 {
		return len(p), nil // Discard but pretend we wrote it
	}

	remaining := cb.maxSize - int64(cb.buf.Len())
	if remaining <= 0 {
		cb.truncated = true
		return len(p), nil
	}

	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
		cb.truncated = true
	}

	n, err = cb.buf.Write(toWrite)
	if err != nil {
		return n, err
	}

	// Return full length even if we truncated
	return len(p), nil
}

func (cb *captureBuffer) finalize() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.body == nil || cb.finalized {
		return nil
	}
	cb.finalized = true

	data := cb.buf.Bytes()
	cb.body.Size = cb.totalSeen
	cb.body.CapturedSize = int64(len(data))
	cb.body.Truncated = cb.truncated
	cb.body.IsBinary = isBinaryContent(data, cb.body.ContentType)

	// Determine storage location
	if int64(len(data)) <= cb.cm.inlineThreshold {
		cb.body.Data = data
		return nil
	}

	if cb.cm.captureDir != "" {
		// Store on disk through the accountant (#69) so the spill is byte-accounted
		// and budget-enforced.
		filePath, err := cb.cm.acct.store(cb.requestID, cb.suffix, data)
		if err != nil {
			// Fall back to inline if disk write fails, but return error for caller awareness
			cb.body.Data = data
			return fmt.Errorf("failed to write capture file %s: %w",
				spillFilePath(cb.cm.captureDir, cb.requestID, cb.suffix), err)
		}
		cb.body.FilePath = filePath
		return nil
	}

	cb.body.Data = data
	return nil
}

// captureReadCloser wraps a reader to finalize capture when closed.
// It combines a TeeReader with the original body's Closer, ensuring that
// captured data is finalized (written to disk or stored inline) when the
// request body is closed.
type captureReadCloser struct {
	io.Reader
	io.Closer
	captured *captureBuffer
}

func (crc *captureReadCloser) Close() error {
	// Finalize the capture
	if crc.captured != nil {
		if err := crc.captured.finalize(); err != nil {
			// Log the error but don't fail the close - the data is still captured inline
			log.Printf("Warning: capture finalize failed: %v", err)
		}
	}
	return crc.Closer.Close()
}

// FinalizeRequestBody forces finalization of a request body previously wrapped
// by CaptureRequest. Idempotent; a non-wrapped body is a no-op. Proxy handlers
// call this after the reverse proxy returns and BEFORE recording, so the
// CapturedBody snapshot is complete when the record is published (SSE
// subscribers serialize records at notify time). Without it, a canceled
// request's transport goroutine may still be draining the body, and its later
// Close-triggered finalize would race the serialization; after this call that
// finalize is a no-op and late writes are discarded.
func FinalizeRequestBody(rc io.ReadCloser) {
	if crc, ok := rc.(*captureReadCloser); ok && crc.captured != nil {
		if err := crc.captured.finalize(); err != nil {
			log.Printf("Warning: capture finalize failed: %v", err)
		}
	}
}

// CaptureResponseWriter wraps an http.ResponseWriter to capture the response body.
// It intercepts writes to capture up to maxBodySize bytes while still forwarding
// all data to the underlying ResponseWriter. It also implements http.Flusher,
// http.Hijacker, and http.Pusher for compatibility with streaming and WebSocket
// connections.
type CaptureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	body        bytes.Buffer
	maxBodySize int64
	truncated   bool
	wroteHeader bool
	hijacked    bool
	totalSeen   int64 // total bytes observed across all writes, counting past truncation

	// firstResponseHook fires the registered callback once at the first final
	// response event (see fireFirstResponse).
	firstResponseHook
}

// newCaptureResponseWriter creates a new capturing response writer.
func newCaptureResponseWriter(w http.ResponseWriter, maxBodySize int64) *CaptureResponseWriter {
	return &CaptureResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		maxBodySize:    maxBodySize,
	}
}

func (crw *CaptureResponseWriter) WriteHeader(code int) {
	// 1xx provisional responses (e.g. 103 Early Hints) are not the final
	// status: they neither latch the recorded status nor fire the hook.
	// ReverseProxy may forward them via WriteHeader before the real status.
	if code >= 200 {
		if !crw.wroteHeader {
			crw.statusCode = code
			crw.wroteHeader = true
		}
		// Order: latch status → invoke callback → delegate, so the in-flight
		// record exists before any response bytes hit the wire.
		crw.fireFirstResponse(code)
	}
	crw.ResponseWriter.WriteHeader(code)
}

func (crw *CaptureResponseWriter) Write(p []byte) (int, error) {
	// Count every byte observed, including data not retained after truncation.
	crw.totalSeen += int64(len(p))

	// Capture up to maxBodySize
	if !crw.truncated && len(p) > 0 {
		remaining := crw.maxBodySize - int64(crw.body.Len())
		if remaining > 0 {
			toCapture := p
			if int64(len(p)) > remaining {
				toCapture = p[:remaining]
				crw.truncated = true
			}
			crw.body.Write(toCapture)
		} else {
			crw.truncated = true
		}
	}

	return crw.ResponseWriter.Write(p)
}

// StatusCode returns the captured status code.
func (crw *CaptureResponseWriter) StatusCode() int {
	return crw.statusCode
}

// CapturedBody returns the captured response body.
func (crw *CaptureResponseWriter) CapturedBody() []byte {
	return crw.body.Bytes()
}

// Truncated returns whether the body was truncated.
func (crw *CaptureResponseWriter) Truncated() bool {
	return crw.truncated
}

// TotalSeen returns the total number of bytes observed by Write, counting
// bytes that were not retained after truncation.
func (crw *CaptureResponseWriter) TotalSeen() int64 {
	return crw.totalSeen
}

// Flush implements http.Flusher for streaming responses (SSE).
func (crw *CaptureResponseWriter) Flush() {
	if f, ok := crw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (crw *CaptureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := crw.ResponseWriter.(http.Hijacker); ok {
		conn, rw, err := h.Hijack()
		if err == nil {
			crw.hijacked = true
			// A successful upgrade never calls WriteHeader (the 101 is written
			// raw to the hijacked conn), so fire the hook here with 101.
			crw.fireFirstResponse(http.StatusSwitchingProtocols)
		}
		return conn, rw, err
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Hijacked reports whether the connection was taken over (WebSocket upgrade).
// After a hijack all traffic bypasses this writer, so the captured status/body
// do not describe the response — callers should record metadata only rather
// than finalize garbage Details. Single-goroutine access per the
// http.ResponseWriter contract.
func (crw *CaptureResponseWriter) Hijacked() bool {
	return crw.hijacked
}

// Push implements http.Pusher for HTTP/2 server push.
func (crw *CaptureResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := crw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter for Go 1.20+ http.ResponseController compatibility.
func (crw *CaptureResponseWriter) Unwrap() http.ResponseWriter {
	return crw.ResponseWriter
}

// cloneHeaders creates a shallow copy of HTTP headers.
func cloneHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	clone := make(http.Header, len(h))
	for k, v := range h {
		clone[k] = v
	}
	return clone
}

// isBinaryContent determines if content appears to be binary based on data and
// content type.
//
// Integrity-first rule (D9): content is never classified as text unless the
// COMPLETE retained data is valid UTF-8. Known-binary content types are always
// binary; a text-y Content-Type never short-circuits to text — data validity
// decides. The full-buffer scan (no 512-byte sampling) is bounded by the 1MB
// capture cap.
func isBinaryContent(data []byte, contentType string) bool {
	// Known-binary content types are binary regardless of the data.
	if contentType != "" {
		ct := strings.ToLower(contentType)
		if strings.HasPrefix(ct, "image/") ||
			strings.HasPrefix(ct, "audio/") ||
			strings.HasPrefix(ct, "video/") ||
			strings.Contains(ct, "octet-stream") ||
			strings.Contains(ct, "zip") ||
			strings.Contains(ct, "gzip") ||
			strings.Contains(ct, "tar") ||
			strings.Contains(ct, "pdf") {
			return true
		}
	}

	// Empty data is not binary.
	if len(data) == 0 {
		return false
	}

	// The entire retained buffer must be valid UTF-8 to be considered text.
	if !utf8.Valid(data) {
		return true
	}

	// Scan the entire buffer for non-printable control characters.
	// Allow common control characters: tab, newline, carriage return.
	for _, b := range data {
		if b < 32 && b != '\t' && b != '\n' && b != '\r' {
			return true
		}
	}

	return false
}
