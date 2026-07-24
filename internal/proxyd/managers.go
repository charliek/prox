package proxyd

import (
	"sync"

	"github.com/charliek/prox/internal/proxy"
)

// Managers holds the daemon's per-project request rings (D13, #49): one
// full-capacity *proxy.RequestManager per registered project dir, so one
// project's flood cannot evict another project's records. It replaces the single
// global RequestManager the daemon used to share across every project.
//
// Its own RWMutex guards the map ONLY. The data plane (the proxy hot path's
// get lookup, and status reads) takes this mutex — never lifecycleMu — honoring
// the server's data-plane rule (server.go): the hot path must not contend on the
// control-plane lifecycle lock. All map mutations (ensure/remove) are still made
// under the caller's lifecycleMu so a lifecycle transaction and a shutdown sweep
// can't race a manager into or out of existence mid-flow, but the map's own lock
// is what the lock-free-of-lifecycleMu hot path synchronizes against.
type Managers struct {
	mu       sync.RWMutex
	managers map[string]*proxy.RequestManager
	capacity int
	// onEvict is wired onto every manager at creation (mirroring what the daemon
	// wired onto the single global manager): evicting a record with captured
	// Details deletes its on-disk body files. nil when capture is disabled.
	onEvict proxy.EvictionCallback
}

// NewManagers creates an empty set whose managers are each created with the
// given ring capacity and eviction callback.
func NewManagers(capacity int, onEvict proxy.EvictionCallback) *Managers {
	if capacity <= 0 {
		capacity = 1
	}
	return &Managers{
		managers: make(map[string]*proxy.RequestManager),
		capacity: capacity,
		onEvict:  onEvict,
	}
}

// ensure returns the manager for projectDir, creating it (with the eviction
// callback wired) if absent. It is IDEMPOTENT: an existing manager is returned
// as-is, preserving its record history and SSE subscriptions across
// re-registers — the property that lets an idempotent or config-changed
// re-register keep the same project's ring rather than churning it. Called only
// under the server's lifecycleMu.
func (ms *Managers) ensure(projectDir string) *proxy.RequestManager {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if m, ok := ms.managers[projectDir]; ok {
		return m
	}
	m := proxy.NewRequestManager(ms.capacity)
	if ms.onEvict != nil {
		m.SetEvictionCallback(ms.onEvict)
	}
	ms.managers[projectDir] = m
	return m
}

// get returns the manager for projectDir, or nil when the project has no ring
// (never registered, or already removed). This is the hot-path RLock lookup: a
// nil result means a completion arriving after its project deregistered, which
// the caller drops safely.
func (ms *Managers) get(projectDir string) *proxy.RequestManager {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.managers[projectDir]
}

// remove Closes the project's manager FIRST — releasing any daemon-side SSE
// handler blocked on its subscription (Close latches, so a handler racing the
// removal also ends cleanly) — then deletes it from the map so subsequent hot
// path lookups return nil. It returns the removed manager (nil when absent) so
// the caller can purge its capture files. Called only under lifecycleMu.
func (ms *Managers) remove(projectDir string) *proxy.RequestManager {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	m, ok := ms.managers[projectDir]
	if !ok {
		return nil
	}
	m.Close()
	delete(ms.managers, projectDir)
	return m
}

// purge drops a project's records and their on-disk body files from its ring,
// without removing the manager itself — the records-only cleanup shared by
// finishRemoval and rollbackRegistration. No-op when the set is nil (capture
// disabled) or the project has no ring, so callers needn't guard either, and it
// takes only the map RLock (never lifecycleMu). Teardown that also detaches the
// manager uses remove/destroyProjectManager instead.
func (ms *Managers) purge(projectDir string) {
	if ms == nil {
		return
	}
	if m := ms.get(projectDir); m != nil {
		m.PurgeByProject(projectDir)
	}
}

// closeAll Closes every manager (daemon teardown), releasing all SSE handlers so
// they don't pin the socket server open through the shutdown grace. The map is
// left intact — the daemon is exiting.
func (ms *Managers) closeAll() {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	for _, m := range ms.managers {
		m.Close()
	}
}

// droppedTotal sums DroppedEvents across every live project ring (D9/D13). The
// daemon status reports this as the single daemon-wide dropped-events total.
func (ms *Managers) droppedTotal() int64 {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	var total int64
	for _, m := range ms.managers {
		total += m.DroppedEvents()
	}
	return total
}

// recordCounts returns the per-project in-memory record count keyed by project
// dir, for daemon status memory diagnosability (D13). Returns nil when no
// project is registered.
func (ms *Managers) recordCounts() map[string]int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if len(ms.managers) == 0 {
		return nil
	}
	counts := make(map[string]int, len(ms.managers))
	for dir, m := range ms.managers {
		counts[dir] = m.Count()
	}
	return counts
}
