package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/constants"
)

// fakeClock is a controllable time source for accountant first-spill ordering.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// newBudgetManager builds an enabled CaptureManager over a temp dir with the
// given disk budget and a controllable clock wired onto its accountant (#69).
func newBudgetManager(t *testing.T, budget int64) (*CaptureManager, *fakeClock) {
	t.Helper()
	cm, err := NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	cm.SetDiskBudget(budget)
	clk := &fakeClock{now: time.Unix(0, 0)}
	cm.acct.now = clk.Now
	return cm, clk
}

func spillPath(cm *CaptureManager, requestID, suffix string) string {
	return filepath.Join(cm.captureDir, requestID+suffix+".bin")
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	require.True(t, os.IsNotExist(err), "unexpected stat error: %v", err)
	return false
}

// TestAccountant_GroupAgeIsFirstSpill pins that a record's group age is the FIRST
// spill time and is not bumped by the second (response) spill, and that the two
// files are accounted under one group (#69).
func TestAccountant_GroupAgeIsFirstSpill(t *testing.T) {
	cm, clk := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	t1 := time.Unix(100, 0)
	t2 := time.Unix(500, 0)
	clk.set(t1)
	_, err := cm.acct.store("A", "_req", make([]byte, 10))
	require.NoError(t, err)
	clk.set(t2)
	_, err = cm.acct.store("A", "_res", make([]byte, 20))
	require.NoError(t, err)

	g := cm.acct.groups["A"]
	require.NotNil(t, g)
	assert.Equal(t, t1, g.firstSpill, "group age must stay the first spill time")
	assert.Equal(t, int64(30), cm.DiskUsed(), "both files accounted under one group")
}

// TestAccountant_TwoFilesEvictTogether pins that evicting a record removes BOTH
// its request and response spill files as one group (#69).
func TestAccountant_TwoFilesEvictTogether(t *testing.T) {
	cm, clk := newBudgetManager(t, 100)

	clk.set(time.Unix(1, 0))
	_, _ = cm.acct.store("A", "_req", make([]byte, 40))
	clk.set(time.Unix(2, 0))
	_, _ = cm.acct.store("A", "_res", make([]byte, 40)) // A = 80
	clk.set(time.Unix(3, 0))
	_, _ = cm.acct.store("B", "_req", make([]byte, 40)) // total 120 > 100 -> evict A

	_, ok := cm.acct.groups["A"]
	assert.False(t, ok, "oldest group A must be evicted")
	assert.False(t, fileExists(t, spillPath(cm, "A", "_req")), "A req file deleted")
	assert.False(t, fileExists(t, spillPath(cm, "A", "_res")), "A res file deleted")
	assert.True(t, fileExists(t, spillPath(cm, "B", "_req")), "B survives")
	assert.Equal(t, int64(40), cm.DiskUsed())
}

// TestAccountant_IdempotentDoubleCleanup pins that a second cleanup (budget-evicted
// then ring-evicted/purged) is a safe no-op that does not double-count (#69).
func TestAccountant_IdempotentDoubleCleanup(t *testing.T) {
	cm, _ := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	_, _ = cm.acct.store("A", "_req", make([]byte, 40))
	_, _ = cm.acct.store("A", "_res", make([]byte, 40))
	require.Equal(t, int64(80), cm.DiskUsed())

	cm.CleanupRequest("A")
	assert.Equal(t, int64(0), cm.DiskUsed())
	// Second cleanup must not underflow or panic.
	cm.CleanupRequest("A")
	assert.Equal(t, int64(0), cm.DiskUsed())
}

// TestAccountant_FailedDeleteDoesNotDecrement pins that a genuinely failed delete
// leaves the byte accounting intact so diskUsed still matches disk (#69).
func TestAccountant_FailedDeleteDoesNotDecrement(t *testing.T) {
	cm, _ := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	_, _ = cm.acct.store("A", "_req", make([]byte, 40))
	require.Equal(t, int64(40), cm.DiskUsed())

	// Inject a remove seam that always fails with a non-NotExist error.
	cm.acct.remove = func(string) error { return fmt.Errorf("boom: permission denied") }
	cm.CleanupRequest("A")

	assert.Equal(t, int64(40), cm.DiskUsed(), "failed delete must not decrement")
	g, ok := cm.acct.groups["A"]
	require.True(t, ok, "group kept so accounting still reflects disk")
	assert.Equal(t, int64(40), g.files["_req"])
}

// TestAccountant_ExactBoundary pins that total == budget does NOT evict but total
// == budget+1 does (#69).
func TestAccountant_ExactBoundary(t *testing.T) {
	cm, clk := newBudgetManager(t, 100)

	clk.set(time.Unix(1, 0))
	_, _ = cm.acct.store("A", "_req", make([]byte, 60))
	clk.set(time.Unix(2, 0))
	_, _ = cm.acct.store("B", "_req", make([]byte, 40)) // total == 100, no evict
	require.Equal(t, int64(100), cm.DiskUsed())
	assert.True(t, fileExists(t, spillPath(cm, "A", "_req")), "exact-boundary keeps A")
	assert.True(t, fileExists(t, spillPath(cm, "B", "_req")), "exact-boundary keeps B")

	clk.set(time.Unix(3, 0))
	_, _ = cm.acct.store("C", "_req", make([]byte, 1)) // total 101 -> evict oldest A
	_, ok := cm.acct.groups["A"]
	assert.False(t, ok, "one byte over budget evicts the oldest group")
	assert.Equal(t, int64(41), cm.DiskUsed())
}

// TestAccountant_OversizedSingleSpill pins that a single spill larger than the
// whole budget is evicted by the general loop as the oldest-and-only group; the
// returned path then points at a deleted file (evicted-file handling) (#69).
func TestAccountant_OversizedSingleSpill(t *testing.T) {
	cm, _ := newBudgetManager(t, 100)

	path, err := cm.acct.store("A", "_req", make([]byte, 500))
	require.NoError(t, err)
	assert.Equal(t, int64(0), cm.DiskUsed(), "oversized spill evicted immediately")
	_, ok := cm.acct.groups["A"]
	assert.False(t, ok)
	assert.False(t, fileExists(t, path), "the just-written file was evicted")
}

// TestAccountant_CrossProjectOldestFirst pins oldest-record-group-first eviction
// order irrespective of which project (request ID) owns the group, with
// interleaved spills through the one shared dir (#69).
func TestAccountant_CrossProjectOldestFirst(t *testing.T) {
	cm, clk := newBudgetManager(t, 100)

	// A (proj1) @1, B (proj2) @2, C (proj1) @3 — 40 bytes each.
	clk.set(time.Unix(1, 0))
	_, _ = cm.acct.store("A", "_req", make([]byte, 40))
	clk.set(time.Unix(2, 0))
	_, _ = cm.acct.store("B", "_req", make([]byte, 40))
	clk.set(time.Unix(3, 0))
	_, _ = cm.acct.store("C", "_req", make([]byte, 40)) // 120 -> evict A (oldest)

	_, okA := cm.acct.groups["A"]
	assert.False(t, okA, "A is oldest and evicted first")
	assert.True(t, fileExists(t, spillPath(cm, "B", "_req")))
	assert.True(t, fileExists(t, spillPath(cm, "C", "_req")))

	clk.set(time.Unix(4, 0))
	_, _ = cm.acct.store("D", "_req", make([]byte, 40)) // 120 -> evict B (next oldest)
	_, okB := cm.acct.groups["B"]
	assert.False(t, okB, "B is evicted next, irrespective of project")
	assert.True(t, fileExists(t, spillPath(cm, "C", "_req")))
	assert.True(t, fileExists(t, spillPath(cm, "D", "_req")))
}

// TestAccountant_ConcurrentSpills exercises concurrent spills, budget-lowering,
// and reads together; it is meaningful under -race (make test-race) and asserts
// convergence to <= budget after everything settles (#69).
func TestAccountant_ConcurrentSpills(t *testing.T) {
	cm, _ := newBudgetManager(t, 4096)
	cm.acct.now = time.Now // real clock is fine; ordering isn't asserted here

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("rec-%d", n)
			_, _ = cm.acct.store(id, "_req", make([]byte, 256))
			_, _ = cm.acct.store(id, "_res", make([]byte, 256))
			_ = cm.DiskUsed()
		}(i)
	}
	// Concurrent budget lowering + reads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			cm.SetDiskBudget(2048)
			_ = cm.DiskBudget()
		}
	}()
	wg.Wait()

	// Final enforcement pass, then assert convergence.
	cm.SetDiskBudget(2048)
	assert.LessOrEqual(t, cm.DiskUsed(), int64(2048), "converges to <= budget after settle")
}

// TestAccountant_FailedWriteFailedRemoveIsTracked pins issue #1 (#69): a response
// spill whose write fails partway AND whose partial-cleanup remove ALSO fails
// must stat + TRACK the orphan so it is not invisible to accounting/eviction, and
// a later CleanupRequest with a working remove retries and deletes both files.
func TestAccountant_FailedWriteFailedRemoveIsTracked(t *testing.T) {
	cm, _ := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	// _req spills normally so the group exists.
	_, err := cm.acct.store("A", "_req", make([]byte, 40))
	require.NoError(t, err)

	// Simulate an ENOSPC-style response write that lands a partial on disk but
	// reports an error, plus a partial-cleanup remove that also fails.
	realWrite := cm.acct.writeFile
	cm.acct.writeFile = func(name string, data []byte, perm os.FileMode) error {
		_ = realWrite(name, data, perm) // partial actually hits disk
		return fmt.Errorf("boom: no space left on device")
	}
	cm.acct.remove = func(string) error { return fmt.Errorf("boom: cannot unlink") }

	path, err := cm.acct.store("A", "_res", make([]byte, 20))
	require.Error(t, err)
	require.Empty(t, path, "failed store returns no path")

	// The un-removable partial is tracked and accounted, and exists on disk.
	g := cm.acct.groups["A"]
	require.NotNil(t, g)
	assert.Equal(t, int64(20), g.files["_res"], "un-removable partial is tracked")
	assert.Equal(t, int64(60), cm.DiskUsed())
	assert.True(t, fileExists(t, spillPath(cm, "A", "_res")))

	// A later cleanup with a working remove retries and deletes BOTH files.
	cm.acct.remove = os.Remove
	cm.CleanupRequest("A")
	assert.Equal(t, int64(0), cm.DiskUsed())
	assert.False(t, fileExists(t, spillPath(cm, "A", "_req")))
	assert.False(t, fileExists(t, spillPath(cm, "A", "_res")))
}

// TestAccountant_RemoveGroupUnlinksUntrackedOrphan pins that removeGroupLocked
// attempts BOTH canonical spill paths regardless of what is tracked, so a purely
// untracked orphan on disk is still unlinked (#69).
func TestAccountant_RemoveGroupUnlinksUntrackedOrphan(t *testing.T) {
	cm, _ := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	// Tracked _req plus an untracked _res orphan written directly to disk.
	_, err := cm.acct.store("A", "_req", make([]byte, 40))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(spillPath(cm, "A", "_res"), make([]byte, 99), 0600))

	cm.CleanupRequest("A")
	assert.Equal(t, int64(0), cm.DiskUsed(), "tracked bytes subtracted")
	assert.False(t, fileExists(t, spillPath(cm, "A", "_req")))
	assert.False(t, fileExists(t, spillPath(cm, "A", "_res")), "untracked orphan is still unlinked")
}

// TestAccountant_CleanupResetsAccounting pins issue #2 (#69): Cleanup removes the
// dir and resets accounting to zero under the accountant lock, and a store racing
// after cleanup fails its write (dir gone) rather than tracking a dangling file.
func TestAccountant_CleanupResetsAccounting(t *testing.T) {
	cm, _ := newBudgetManager(t, constants.DefaultCaptureDiskBudget)

	_, _ = cm.acct.store("A", "_req", make([]byte, 40))
	_, _ = cm.acct.store("B", "_req", make([]byte, 40))
	require.Equal(t, int64(80), cm.DiskUsed())

	require.NoError(t, cm.Cleanup())
	assert.Equal(t, int64(0), cm.DiskUsed(), "Cleanup resets accounting")
	assert.Empty(t, cm.acct.groups)
	assert.False(t, fileExists(t, spillPath(cm, "A", "_req")))

	// A store racing after Cleanup (dir gone) fails its write -> inline fallback,
	// never a tracked file into a removed dir.
	path, err := cm.acct.store("C", "_req", make([]byte, 40))
	require.Error(t, err)
	assert.Empty(t, path)
	assert.Equal(t, int64(0), cm.DiskUsed(), "post-cleanup store must not track")
}
