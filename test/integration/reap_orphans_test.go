package integration

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestReap_OrphanedGrandchildAfterKill(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	f := newFixture(t, "stubborn")

	var firstGrandchildPID, secondGrandchildPID int
	t.Cleanup(func() {
		// Never leak a stubborn listener into CI, and remove the ledger so it
		// cannot drive a reap in a later test sharing this fixture's dir.
		killIfAlive(secondGrandchildPID)
		killIfAlive(firstGrandchildPID)
		_ = os.Remove(filepath.Join(f.dir, ".prox", "prox.children"))
	})

	// --- Generation 1 -------------------------------------------------------
	// f.Start registers its own t.Cleanup(first.Kill), so an assertion that
	// aborts before the explicit Kill below still can't leak gen 1.
	first := f.Start(t, binary, "up", "-c", f.configPath)

	waitForAPI(t, first.Addr(), apiReadyTimeout)

	pidStr := waitForMarkerValue(t, first.Addr(), "worker", "GRANDCHILD_PID=", "", 5*time.Second)
	pid, err := strconv.Atoi(pidStr)
	requireNoError(t, err, "parsing first GRANDCHILD_PID")
	firstGrandchildPID = pid
	waitForMarkerValue(t, first.Addr(), "worker", "LISTENING=", "", 5*time.Second)

	// SIGKILL the prox process. Its child group is in its own PGID, so SIGKILL does
	// NOT cascade to it -- the leader shell + grandchild orphan and keep running.
	first.Kill()

	// The orphaned grandchild survived and still holds the port.
	if !processAlive(firstGrandchildPID) {
		t.Fatalf("grandchild pid %d should survive prox SIGKILL (orphaned), but it is gone", firstGrandchildPID)
	}
	// The port is genuinely held: a fresh bind (SO_REUSEADDR off in the fixture)
	// must fail with the orphan still listening. f.StubbornPort() is memoized per
	// fixture (allocated once, on first use, and cached -- see fixture_test.go),
	// and this fixture's config was rendered exactly once, so generation 1 and
	// generation 2 are guaranteed the SAME port here: if they weren't, this
	// negative assertion (bind must fail) would pass for the wrong reason, and
	// the later rebind assertion would prove nothing.
	stubbornPort := strconv.Itoa(f.StubbornPort())
	if ln, bindErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", stubbornPort)); bindErr == nil {
		_ = ln.Close()
		t.Fatalf("expected port %s to be held by the orphaned grandchild, but it was free", stubbornPort)
	}

	// --- Generation 2: reaps the orphan on startup, then rebinds the port ----
	second := f.Start(t, binary, "up", "-c", f.configPath)

	// Safe despite generation 1 having left its state file behind: Addr() only
	// accepts a state file whose pid is this run's own.
	addr := second.Addr()

	// Startup includes the reap grace window (~2s) before the supervisor starts.
	waitForAPI(t, addr, 20*time.Second)

	// The first generation's orphaned group must be gone -- reaped from the ledger
	// during gen-2 startup.
	if !waitForPIDGone(firstGrandchildPID, 15*time.Second) {
		t.Fatalf("first-generation grandchild pid %d still alive after generation-2 startup reap", firstGrandchildPID)
	}

	// The port rebinds: gen-2's worker launches a NEW grandchild that binds the
	// same port. The marker only prints after a successful bind+listen, so its mere
	// presence proves the rebind succeeded (a failed rebind would crash the python
	// script before printing GRANDCHILD_PID/LISTENING).
	newPIDStr := waitForMarkerValue(t, addr, "worker", "GRANDCHILD_PID=", pidStr, 15*time.Second)
	newPID, err := strconv.Atoi(newPIDStr)
	requireNoError(t, err, "parsing second GRANDCHILD_PID")
	secondGrandchildPID = newPID
	if newPID == firstGrandchildPID {
		t.Fatalf("expected a new grandchild pid distinct from %d, got the same pid", firstGrandchildPID)
	}

	listeningPort := waitForMarkerValue(t, addr, "worker", "LISTENING=", "", 5*time.Second)
	if listeningPort != stubbornPort {
		t.Fatalf("expected the rebound grandchild to listen on port %s, got %q", stubbornPort, listeningPort)
	}

	// The replacement worker should be running (not crashed on EADDRINUSE).
	waitForProcessState(t, addr, "worker", "running", 5*time.Second)
}
