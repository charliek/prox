package cli

import (
	"context"
	"fmt"
	"io"
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
	// logTail returns the child's daemon log content for diagnostics, scoped to
	// pid's run (via the run marker) when one is found, falling back to the
	// last n lines otherwise. See daemonLogTail.
	logTail func(dir string, pid int, n int) string
	// settle observes the now-ready daemon's processes for the settle window and
	// reports what it saw (plan 027 C13, #94). Its error return is the
	// OBSERVATION's failure, never a process's -- see awaitProcessSettle.
	settle func(addr string) (settleVerdict, error)

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
		settle:    settleDaemonProcesses,
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

// daemonLogFallbackLines is the last-N-lines fallback used when no run marker
// matching the child's pid is found in the log: a legacy log predating this
// feature, the pre-SetupLogging window (checkEarlyDeath can fire before the
// child ever reaches SetupLogging), or the shared-proxy log, which gains no
// marker at all. This is the pre-#99 behavior, preserved as the fallback.
const daemonLogFallbackLines = 20

// daemonLogReadLimit caps how many BYTES of .prox/prox.log are read for
// failure diagnostics. The file is never truncated (#99) and grows without
// bound across runs, so os.ReadFile on it is an unbounded allocation -- several
// times over, once the content is split, sliced and re-joined -- to produce a
// tail that is capped at a couple of hundred lines anyway. 256 KiB is far more
// than any single run's startup diagnostics and is read from the END of the
// file, where the current run is.
const daemonLogReadLimit = 256 << 10

// readLogTail returns the last daemonLogReadLimit bytes of the file at path,
// starting at a line boundary: when the file is longer than the limit the
// window opens mid-line, and that fragment is dropped rather than handed on --
// half a line is not a log line, and half a RUN MARKER is a damaged marker
// (daemon.FindRunMarkerTail) that we would have manufactured ourselves.
func readLogTail(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > limit {
		if _, err := f.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return "", err
		}
	}

	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	if info.Size() <= limit {
		return string(data), nil
	}

	content := string(data)
	i := strings.IndexByte(content, '\n')
	if i < 0 {
		// A single line longer than the whole window: there is no complete line
		// to report at all.
		return "", nil
	}
	return content[i+1:], nil
}

// daemonLogTail returns the child's daemon log (.prox/prox.log) content for
// failure diagnostics, scoped to THIS run when possible: it looks for the run
// marker daemon.SetupLogging wrote for pid and, if found, returns only the
// content after it (see daemon.FindRunMarkerTail) -- otherwise the log is
// never truncated (#99), so with no marker a user iterating on a broken
// config would see every past failure stacked up with the current one buried
// last. Falls back to the last n lines when no matching marker is found.
// Returns "" when the log is missing.
//
// Every stage is bounded: at most daemonLogReadLimit bytes are read, and the
// scoped tail is capped at daemon.MaxRunTailLines lines. A run whose segment
// exceeds that cap is reported as capped -- printing its last 200 lines under
// a heading that implies they are all of it would misrepresent the run just as
// surely as showing another run's output would.
func daemonLogTail(dir string, pid int, n int) string {
	content, err := readLogTail(daemon.LogPath(dir), daemonLogReadLimit)
	if err != nil {
		return ""
	}

	if tail, truncated, ok := daemon.FindRunMarkerTail(content, pid); ok {
		if truncated {
			return fmt.Sprintf("(earlier lines of this run omitted; showing the last %d)\n%s",
				daemon.MaxRunTailLines, tail)
		}
		return tail
	}

	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
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
// Returns the daemon's resolved API address (host:port) on success. It does NOT
// print the started line: readiness is no longer the whole success condition
// (#94), so the headline belongs to startDetachedDaemon, after the processes
// have been observed. Returning the address rather than keeping it local to the
// loop is what lets that caller talk to the daemon it just waited for.
//
// On early death or never-ready timeout it prints diagnostics (headline + log
// tail) and returns an error (caller exits 1). On timeout with the child still
// alive it SIGTERMs the child and escalates to SIGKILL after killGrace so a
// child that already registered with the shared daemon gets a chance to
// deregister.
func awaitDaemonStartup(child daemonChild, cwd string, ops daemonStartupOps) (string, error) {
	pid := child.Pid()

	// Reap the direct child on a goroutine: this prevents a zombie and signals
	// early death. Buffered so the goroutine never blocks after we stop reading.
	childErr := make(chan error, 1)
	go func() { childErr <- child.Wait() }()

	deadline := time.Now().Add(ops.readyTimeout)
	for {
		// Early death: the child exited before it became ready.
		if err := checkEarlyDeath(childErr, pid, cwd, ops); err != nil {
			return "", err
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
					return "", err
				}
				return addr, nil
			}
		}

		if !time.Now().Before(deadline) {
			// Deadline reached. Re-check death first: the child may have exited
			// right at the boundary, in which case report it as early death.
			if err := checkEarlyDeath(childErr, pid, cwd, ops); err != nil {
				return "", err
			}
			// Child alive but never ready (API bind failures are fatal in the
			// child, so this is a genuinely wedged startup: config load hang,
			// supervisor stall, or similar).
			printDaemonFailure(cwd, pid, ops, fmt.Sprintf(
				"prox daemon (pid %d) did not become ready within %s", pid, ops.readyTimeout))
			terminateChild(child, childErr, ops)
			return "", fmt.Errorf("prox daemon failed to become ready within %s", ops.readyTimeout)
		}

		ops.sleep(ops.pollInterval)
	}
}

// startDetachedDaemon is the whole parent side of `prox up -d`: wait for the
// child to become ready, then watch what its processes actually DO before
// claiming the start succeeded (plan 027 C13, #94).
//
// The two halves answer different questions and neither subsumes the other.
// awaitDaemonStartup asks "is the daemon accepting requests"; this asks "did
// the things it was started for survive being started". Before #94 only the
// first was asked, so `prox up -d` exited 0 for a project whose every process
// was already dead — the daemon was fine, and that was all anyone checked.
//
// A non-zero exit from HERE means something quite different from a non-zero
// exit from awaitDaemonStartup: the daemon IS up and stays up. Nothing is
// rolled back — tearing down a healthy daemon because one process died would
// destroy the logs the user needs — so the failure message points at
// `prox down` rather than pretending nothing started.
func startDetachedDaemon(child daemonChild, cwd string, ops daemonStartupOps) error {
	addr, err := awaitDaemonStartup(child, cwd, ops)
	if err != nil {
		return err
	}
	pid := child.Pid()

	verdict, settleErr := ops.settle(addr)
	switch {
	case settleErr != nil:
		// The OBSERVATION failed, not the processes. Readiness was already
		// established, so a flaky follow-up request must not invent a failure:
		// keep the pre-existing exit code and say once that state is unconfirmed.
		printDaemonStarted(pid, addr)
		fmt.Fprintf(os.Stderr,
			"Warning: could not confirm process state after startup: %v\n", settleErr)
		return nil
	case verdict.failed():
		// No success headline: printing "prox started" and then exiting 1 is
		// the same lie #94 is about, relocated.
		fmt.Fprintf(os.Stderr,
			"prox daemon started (pid %d, api http://%s), but its processes did not.\n", pid, addr)
		verdict.writeTo(os.Stderr, "The daemon is still running; stop it with 'prox down'.")
		return verdict.err()
	default:
		printDaemonStarted(pid, addr)
		return nil
	}
}

// printDaemonStarted prints the one success line `prox up -d` has always
// printed. Its position is the load-bearing part: it now happens after the
// settle window, so it is never followed by a non-zero exit.
func printDaemonStarted(pid int, addr string) {
	fmt.Printf("prox started (pid %d, api http://%s)\n", pid, addr)
}

// settleDaemonProcesses is the production settle step: talk to the daemon that
// just became ready and watch every process it manages for the settle window.
//
// The client reads ~/.prox/token, which the child wrote before it started
// serving — so by the time /health has answered (which is the only way we get
// here) the token is on disk.
func settleDaemonProcesses(addr string) (settleVerdict, error) {
	client := NewClient("http://" + addr)
	return awaitProcessSettle(
		context.Background(), settleAllProcesses(client), processSettleTimeout, processSettlePollInterval)
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
		printDaemonFailure(cwd, pid, ops, fmt.Sprintf(
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

// printDaemonFailure prints a failure headline plus this run's tail of the
// child's daemon log (see daemonLogTail) to stderr, for `prox up -d` failure
// diagnostics. pid is the failed child's pid, used to scope the tail to its
// own run marker rather than the log's full, never-truncated history (#99).
func printDaemonFailure(dir string, pid int, ops daemonStartupOps, headline string) {
	fmt.Fprintln(os.Stderr, headline)
	logPath := daemon.LogPath(dir)
	if tail := ops.logTail(dir, pid, daemonLogFallbackLines); strings.TrimSpace(tail) != "" {
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
