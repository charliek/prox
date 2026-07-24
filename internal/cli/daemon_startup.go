package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
)

// daemonChild is the parent's handle on the detached daemon process. The real
// implementation (execChild) wraps *exec.Cmd; tests supply a fake so the
// poll/wait/report logic runs without spawning a real process.
type daemonChild interface {
	// Pid is the child's process ID (used to match the state file, staleness guard).
	Pid() int
	// Wait blocks until the child exits and returns its exit status. It is called
	// exactly once, from the reaping goroutine, so the direct child never zombies.
	Wait() error
	// Signal delivers sig to the child (SIGTERM/SIGKILL on the timeout path).
	Signal(sig os.Signal) error
}

// execChild adapts a started *exec.Cmd to daemonChild.
type execChild struct{ cmd *exec.Cmd }

func (c *execChild) Pid() int                   { return c.cmd.Process.Pid }
func (c *execChild) Wait() error                { return c.cmd.Wait() }
func (c *execChild) Signal(sig os.Signal) error { return c.cmd.Process.Signal(sig) }

// daemonStartupOps bundles the injectable operations and timings the parent's
// wait-and-report loop (D2) needs so awaitDaemonStartup can be unit-tested with
// a fake child and fake probes — no real sockets or wall-clock waits. Mirrors
// the skewOps pattern (version_skew.go). defaultDaemonStartupOps wires the
// production implementations.
type daemonStartupOps struct {
	// loadState reads the child's state file for the project dir.
	loadState func(dir string) (*daemon.State, error)
	// healthOK reports whether GET /health on the state's address (host:port) succeeds.
	healthOK func(addr string) bool
	// logTail returns the last n lines of the child's daemon log for diagnostics.
	logTail func(dir string, n int) string

	sleep func(time.Duration)

	readyTimeout time.Duration // total budget for the child to become ready
	pollInterval time.Duration // interval between readiness polls
	killGrace    time.Duration // grace after SIGTERM before escalating to SIGKILL
}

// killWaitBound caps how long the parent waits for the post-SIGKILL reap
// before giving up so the `prox up -d` exit contract stays bounded.
const killWaitBound = 2 * time.Second

// defaultDaemonStartupOps wires daemonStartupOps to the real filesystem and a
// localhost /health probe, with the production timings from D2 (§3): a 15s
// readiness budget polled at 200ms, and a 5s SIGTERM→SIGKILL grace.
func defaultDaemonStartupOps() daemonStartupOps {
	return daemonStartupOps{
		loadState: daemon.LoadState,
		healthOK:  probeHealth,
		logTail:   daemonLogTail,
		sleep:     time.Sleep,

		readyTimeout: 15 * time.Second,
		pollInterval: 200 * time.Millisecond,
		killGrace:    5 * time.Second,
	}
}

// probeHealth issues a single GET /health against the API server at addr
// (host:port) with a short per-probe timeout, reporting whether it answered 200.
func probeHealth(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), constants.DaemonHealthProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// daemonLogTail returns the last n lines of the child's daemon log (.prox/prox.log)
// for failure diagnostics. Returns "" when the log is missing or empty.
func daemonLogTail(dir string, n int) string {
	data, err := os.ReadFile(daemon.LogPath(dir))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// awaitDaemonStartup owns the parent side of `prox up -d` (D2). It reaps the
// direct child on a goroutine (preventing a zombie and surfacing early death),
// then polls up to ops.readyTimeout for the child to become ready:
//
//	(a) .prox/prox.state exists AND state.PID == child PID — the PID match is a
//	    staleness guard so a previous run's leftover state file never fools the
//	    poll; then
//	(b) GET /health on the state's address answers.
//
// Child startup ordering (verified against runUp in internal/cli/up.go and
// internal/daemon/state.go): the child writes .prox/prox.state (state.Write, up.go)
// BEFORE it starts the API server that serves /health (apiServer.Start, launched
// later in a goroutine). So in practice the state file appears first and /health
// second. The loop nonetheless re-checks BOTH conditions every interval, so
// either ordering is tolerated per D2 (a hypothetical future reordering, or a
// slow state fsync racing an early bind, still converges).
//
// Returns nil on success (prints the started line; caller returns 0). On early
// death or never-ready timeout it prints diagnostics (headline + log tail) and
// returns an error (caller exits 1). On timeout with the child still alive it
// SIGTERMs the child and escalates to SIGKILL after killGrace so a child that
// already registered with the shared daemon gets a chance to deregister.
func awaitDaemonStartup(child daemonChild, cwd string, ops daemonStartupOps) error {
	pid := child.Pid()

	// Reap the direct child on a goroutine: this prevents a zombie and signals
	// early death. Buffered so the goroutine never blocks after we stop reading.
	childErr := make(chan error, 1)
	go func() { childErr <- child.Wait() }()

	deadline := time.Now().Add(ops.readyTimeout)
	for {
		// Early death: the child exited before it became ready.
		if err := checkEarlyDeath(childErr, pid, cwd, ops); err != nil {
			return err
		}

		if st, err := ops.loadState(cwd); err == nil && st.PID == pid {
			addr := net.JoinHostPort(st.Host, strconv.Itoa(st.Port))
			if ops.healthOK(addr) {
				// Re-check death before declaring success: a child that died
				// between the probe and here (or whose draining server answered
				// the probe) must be reported as a failure, not a green start.
				// This narrows — it cannot close — the inherent window where
				// the child dies right after the parent exits 0.
				if err := checkEarlyDeath(childErr, pid, cwd, ops); err != nil {
					return err
				}
				fmt.Printf("prox started (pid %d, api http://%s)\n", pid, addr)
				return nil
			}
		}

		if !time.Now().Before(deadline) {
			// Deadline reached. Re-check death first: the child may have exited
			// right at the boundary, in which case report it as early death.
			if err := checkEarlyDeath(childErr, pid, cwd, ops); err != nil {
				return err
			}
			// Child alive but never ready (API bind failures are fatal in the
			// child, so this is a genuinely wedged startup: config load hang,
			// supervisor stall, or similar).
			printDaemonFailure(cwd, ops, fmt.Sprintf(
				"prox daemon (pid %d) did not become ready within %s", pid, ops.readyTimeout))
			terminateChild(child, childErr, ops)
			return fmt.Errorf("prox daemon failed to become ready within %s", ops.readyTimeout)
		}

		ops.sleep(ops.pollInterval)
	}
}

// checkEarlyDeath does a non-blocking check of childErr. If the child has
// already exited, it prints early-death diagnostics and returns a non-nil
// error; awaitDaemonStartup calls this both at the top of each poll iteration
// and again at the deadline boundary, so a death right at the boundary is
// still reported as early death rather than a timeout. Returns nil if the
// child has not exited yet.
func checkEarlyDeath(childErr <-chan error, pid int, cwd string, ops daemonStartupOps) error {
	select {
	case werr := <-childErr:
		printDaemonFailure(cwd, ops, fmt.Sprintf(
			"prox daemon (pid %d) exited before startup completed: %s", pid, exitDesc(werr)))
		return fmt.Errorf("prox daemon failed to start")
	default:
		return nil
	}
}

// terminateChild SIGTERMs a hung child, waits up to killGrace for it to exit,
// then escalates to SIGKILL. It consumes the child's Wait() result (from
// childErr) so the child is reaped either way — but the post-SIGKILL wait is
// bounded: the parent's exit contract (timeout + grace) must hold even if the
// child is wedged in an uninterruptible state, so after killWaitBound the
// parent gives up on the reap (the Wait goroutine drains the channel later if
// the process ever dies; the parent is exiting anyway) and reports the PID.
func terminateChild(child daemonChild, childErr <-chan error, ops daemonStartupOps) {
	_ = child.Signal(syscall.SIGTERM)
	select {
	case <-childErr:
		return // exited within the grace period
	case <-time.After(ops.killGrace):
	}
	_ = child.Signal(syscall.SIGKILL)
	select {
	case <-childErr:
	case <-time.After(killWaitBound):
		fmt.Fprintf(os.Stderr,
			"Warning: child (pid %d) did not exit after SIGKILL; giving up on reap\n", child.Pid())
	}
}

// printDaemonFailure prints a failure headline plus the last ~20 lines of the
// child's daemon log to stderr, for `prox up -d` failure diagnostics.
func printDaemonFailure(dir string, ops daemonStartupOps, headline string) {
	fmt.Fprintln(os.Stderr, headline)
	logPath := daemon.LogPath(dir)
	if tail := ops.logTail(dir, 20); strings.TrimSpace(tail) != "" {
		fmt.Fprintf(os.Stderr, "\nLast lines of %s:\n%s\n", logPath, tail)
	} else {
		fmt.Fprintf(os.Stderr, "\n(no output in %s)\n", logPath)
	}
}

// exitDesc renders a child's Wait() result for a diagnostic message.
func exitDesc(err error) string {
	if err == nil {
		return "exited cleanly (exit 0)"
	}
	return err.Error()
}
