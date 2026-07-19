package integration

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestReap_OrphanedGrandchildAfterKill (issue #59) proves the ownership-ledger
// orphan reap end to end through the real binary:
//
//  1. Generation 1 starts the stubborn fixture (a leader shell whose backgrounded
//     python grandchild ignores SIGTERM and holds a real TCP port).
//  2. Generation 1 is killed with SIGKILL -- its graceful shutdown NEVER runs, so
//     the supervised child group orphans and keeps holding the port (the classic
//     EADDRINUSE-on-next-up scenario).
//  3. Generation 2 starts in the same project dir. On startup it reads the ledger
//     the SIGKILL'd generation left behind and reaps the orphaned group, so its
//     own worker rebinds the same port (SO_REUSEADDR is deliberately off in the
//     fixture, so a successful rebind proves the orphan genuinely released it).
//
// This is the core acceptance for C5: first-generation group GONE + port REBINDS,
// with no EADDRINUSE crashloop.
func TestReap_OrphanedGrandchildAfterKill(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	addr := stubbornAPIAddr

	var firstGrandchildPID, secondGrandchildPID int
	t.Cleanup(func() {
		// Never leak a stubborn listener into CI, and remove the ledger so it
		// cannot drive a reap in a later test sharing this project dir.
		killIfAlive(secondGrandchildPID)
		killIfAlive(firstGrandchildPID)
		_ = os.Remove(filepath.Join(projectRoot(t), ".prox", "prox.children"))
	})

	// --- Generation 1 -------------------------------------------------------
	first := startProxWithOutput(t, binary, "up", "-c", configPath("stubborn"))
	// If an assertion below aborts before gen-1 is SIGKILL'd, make sure it dies.
	firstKilled := false
	defer func() {
		if !firstKilled {
			killProx(first.cmd)
		}
	}()

	waitForAPI(t, addr, 10*time.Second)

	pidStr := waitForMarkerValue(t, addr, "worker", "GRANDCHILD_PID=", "", 5*time.Second)
	pid, err := strconv.Atoi(pidStr)
	requireNoError(t, err, "parsing first GRANDCHILD_PID")
	firstGrandchildPID = pid
	waitForMarkerValue(t, addr, "worker", "LISTENING=", "", 5*time.Second)

	// SIGKILL the prox process. Its child group is in its own PGID, so SIGKILL does
	// NOT cascade to it -- the leader shell + grandchild orphan and keep running.
	killProx(first.cmd)
	firstKilled = true

	// The orphaned grandchild survived and still holds the port.
	if !processAlive(firstGrandchildPID) {
		t.Fatalf("grandchild pid %d should survive prox SIGKILL (orphaned), but it is gone", firstGrandchildPID)
	}
	// The port is genuinely held: a fresh bind (SO_REUSEADDR off in the fixture)
	// must fail with the orphan still listening.
	if ln, bindErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", stubbornListenerPort)); bindErr == nil {
		_ = ln.Close()
		t.Fatalf("expected port %s to be held by the orphaned grandchild, but it was free", stubbornListenerPort)
	}

	// --- Generation 2: reaps the orphan on startup, then rebinds the port ----
	second := startProxWithOutput(t, binary, "up", "-c", configPath("stubborn"))
	defer killProx(second.cmd)

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
	if listeningPort != stubbornListenerPort {
		t.Fatalf("expected the rebound grandchild to listen on port %s, got %q", stubbornListenerPort, listeningPort)
	}

	// The replacement worker should be running (not crashed on EADDRINUSE).
	waitForProcessState(t, addr, "worker", "running", 5*time.Second)
}
