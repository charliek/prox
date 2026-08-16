package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/tui"
	"github.com/spf13/cobra"
)

// Status command flags
var statusJSON bool

// statusCmd represents the status command
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show process status",
	Long: `Show the status of all running processes.

Displays process names, status, PIDs, uptime, restart counts, and health checks.

Examples:
  prox status          # Show status in table format
  prox status --json   # Output as JSON`,
	RunE: runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	client := NewClient(apiAddr)

	// Get status
	status, err := client.GetStatus()
	if err != nil {
		return clientError(err, "Is prox running? Try 'prox up' first.")
	}

	// Get processes
	processes, err := client.GetProcesses()
	if err != nil {
		return fmt.Errorf("failed to get processes: %w", err)
	}

	// A shared proxy that is down makes `prox status` exit non-zero even when
	// every child process is healthy (D5, breaking change): the proxied routes
	// are dead, so a green table alone would mislead. Computed here so both the
	// JSON and table paths honor it.
	proxyDown := sharedProxyDown(status.Proxy)

	// A crashed child likewise fails `prox status` (D1, #72, breaking change):
	// exit 0 must not mask a dead process. Computed once so both the JSON and
	// table paths honor it. Precedence: when both a crash and a proxy-down hold,
	// both signals are printed but the proxy sentinel wins the return (see below).
	crashed := crashedProcesses(processes.Processes)

	// Gated-launch failure signals (plan 013 D5). A process left `blocked` by a
	// failed dependency, and any dependency in the terminal `failed` state, each
	// fail `prox status` too — a green table must not mask a launch that will
	// never happen. `completed` tasks and `warned`/`waiting`/`pending`
	// dependencies deliberately never trip exit 1. Computed once so both paths
	// honor them.
	blocked := blockedProcesses(processes.Processes)
	failedDeps := failedDependencies(status.Dependencies)

	if statusJSON {
		output := map[string]interface{}{
			"status":    status,
			"processes": processes.Processes,
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode output: %v\n", err)
		}
		// The JSON payload already carries each process's status, so no extra
		// line is printed here — only the exit code changes.
		return statusExitError(proxyDown, crashed, blocked, failedDeps)
	}

	// Print status
	fmt.Printf("Status: %s\n", status.Status)
	fmt.Printf("Uptime: %s\n", formatDuration(time.Duration(status.UptimeSeconds)*time.Second))
	fmt.Printf("Config: %s\n", status.ConfigFile)
	fmt.Println()

	// Print processes table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tPID\tUPTIME\tRESTARTS\tHEALTH")
	fmt.Fprintln(w, "----\t------\t---\t------\t--------\t------")

	for _, p := range processes.Processes {
		uptime := formatDuration(time.Duration(p.UptimeSeconds) * time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			p.Name, statusField(p), pidField(p), uptime, p.Restarts, healthField(p))
	}
	w.Flush()

	// Proxy line (D5). Printed after the table so the process status is always
	// visible even when the shared proxy is down (which then forces exit 1).
	renderProxyStatus(status.Proxy)

	// Crashed line (D1, #72). Printed after the proxy line, naming the crashed
	// process(es) in the order the supervisor reported them (no sort), with a
	// pointer at their logs. Printed even when the proxy is also down so neither
	// signal is hidden.
	if len(crashed) > 0 {
		fmt.Printf("\nCrashed: %s — check 'prox logs %s'.\n", strings.Join(crashed, ", "), crashed[0])
	}

	// Blocked line (plan 013 D5). Mirrors the Crashed line: names each blocked
	// process with its failed dependency targets in declaration order. Printed
	// alongside any crashed/proxy-down signal so none is hidden.
	if len(blocked) > 0 {
		fmt.Printf("\nBlocked: %s\n", strings.Join(blockedSummaries(processes.Processes), ", "))
	}

	// Dependencies section (plan 013 D5). Only when dependencies are configured.
	renderDependencies(status.Dependencies)

	// All applicable signals print above; the exit code follows the shared
	// precedence in statusExitError.
	return statusExitError(proxyDown, crashed, blocked, failedDeps)
}

// statusField renders the STATUS column for a process row. It shows the bare
// state enum, except for the two gated-launch states where the unsatisfied
// depends_on targets are appended render-side (plan 013 D5): `waiting(a, b)` and
// `blocked(a)`, comma+space separated in declaration order. The JSON status
// stays the bare enum; only this human table decorates it.
func statusField(p api.ProcessResponse) string {
	switch p.Status {
	case string(domain.ProcessStateWaiting):
		if len(p.WaitingOn) > 0 {
			return fmt.Sprintf("waiting(%s)", strings.Join(p.WaitingOn, ", "))
		}
	case string(domain.ProcessStateBlocked):
		if len(p.BlockedOn) > 0 {
			return fmt.Sprintf("blocked(%s)", strings.Join(p.BlockedOn, ", "))
		}
	}
	return p.Status
}

// pidField renders the PID column. A process with no live PID — a completed
// task, or any terminal/gated state — reports "-" rather than 0 (plan 013 D5),
// so a frozen completed task reads as finished rather than "pid 0".
func pidField(p api.ProcessResponse) string {
	if p.PID == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", p.PID)
}

// healthField renders the HEALTH column. A process with no healthcheck
// configured reports "none" on the wire and renders as "-" here (#100),
// matching the pidField convention right beside it: nothing was ever checked,
// so the column is empty rather than claiming an inconclusive check. "unknown"
// is left verbatim — that means a check IS configured and has not reported yet.
// Any other value (including one from a newer daemon) passes through unchanged.
func healthField(p api.ProcessResponse) string {
	if p.Health == string(domain.HealthStatusNone) || p.Health == "" {
		return "-"
	}
	return p.Health
}

// blockedSummaries returns one "name(target1, target2)" entry per blocked
// process, in the order the supervisor reported them, for the Blocked line.
func blockedSummaries(procs []api.ProcessResponse) []string {
	var out []string
	for _, p := range procs {
		if p.Status == string(domain.ProcessStateBlocked) {
			out = append(out, fmt.Sprintf("%s(%s)", p.Name, strings.Join(p.BlockedOn, ", ")))
		}
	}
	return out
}

// renderDependencies prints the Dependencies section (plan 013 D5): one row per
// configured dependency with its state, check summary, and — for a failed or
// warned dependency — the last probe error as detail. Nothing prints when no
// dependencies are configured.
func renderDependencies(deps []api.DependencyStatusResponse) {
	if len(deps) == 0 {
		return
	}
	fmt.Println("\nDependencies:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tCHECK\tDETAIL")
	fmt.Fprintln(w, "----\t-----\t-----\t------")
	for _, d := range deps {
		detail := ""
		if d.State == string(supervisorDepFailed) || d.State == string(supervisorDepWarned) {
			detail = d.LastError
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Name, d.State, d.Check, detail)
	}
	w.Flush()
}

// statusExitError maps runStatus's exit-1 conditions to the error it returns
// (nil = exit 0), keeping the precedence in one place for both the JSON and
// table paths (plan 013 D5). Exit 1 holds when ANY condition holds; the returned
// (primary) error follows the precedence proxy-down > crashed > blocked >
// failed-dependency. All applicable human-readable table lines are printed by
// the caller regardless of which one wins the return.
func statusExitError(proxyDown bool, crashed, blocked, failedDeps []string) error {
	switch {
	case proxyDown:
		return errSharedProxyDown
	case len(crashed) > 0:
		return errProcessesCrashed(len(crashed))
	case len(blocked) > 0:
		return errProcessesBlocked(len(blocked))
	case len(failedDeps) > 0:
		return errDependenciesFailed(len(failedDeps))
	default:
		return nil
	}
}

// supervisorDepFailed / supervisorDepWarned are the dependency states whose
// last error is worth surfacing as row detail. They mirror the resolver's
// terminal DepState strings without importing the supervisor package into the
// CLI's render path.
const (
	supervisorDepFailed = "failed"
	supervisorDepWarned = "warned"
)

// errSharedProxyDown is returned by runStatus when the shared proxy daemon is
// unreachable, so `prox status` exits non-zero (D5). Its text IS user-visible:
// Execute() prints `Error: %v` to stderr for any non-nil RunE error
// (root.go:113-116). rootCmd sets SilenceErrors, which only stops cobra from
// reprinting the error itself — it does not suppress that `Error:` line.
var errSharedProxyDown = fmt.Errorf("shared proxy daemon is unreachable")

// errProcessesCrashed is returned by runStatus when one or more child processes
// are in the crashed state, so `prox status` exits non-zero (D1, #72). Like
// errSharedProxyDown its text is user-visible (Execute prints `Error: %v`); the
// count is baked in so the stderr line names how many processes crashed.
func errProcessesCrashed(n int) error {
	return fmt.Errorf("%d process(es) crashed", n)
}

// errProcessesBlocked is returned by runStatus when one or more child processes
// are blocked on a failed dependency (plan 013 D5), so `prox status` exits
// non-zero. Its text is user-visible (Execute prints `Error: %v`); the count is
// baked in so the stderr line names how many processes are blocked.
func errProcessesBlocked(n int) error {
	return fmt.Errorf("%d process(es) blocked on failed dependencies", n)
}

// errDependenciesFailed is returned by runStatus when one or more dependencies
// are in the terminal failed state (fail policy) but no crashed/blocked/proxy
// signal outranks them (plan 013 D5). Its text is user-visible; the singular
// "dependency" / plural "dependencies" agrees with the count.
func errDependenciesFailed(n int) error {
	if n == 1 {
		return fmt.Errorf("1 dependency failed")
	}
	return fmt.Errorf("%d dependencies failed", n)
}

// crashedProcesses returns the names of processes in the crashed state, in the
// order the supervisor reported them (no sort). `prox status` exits non-zero
// when this is non-empty (D1, #72): exit 0 must not mask a dead child. Only
// domain.ProcessStateCrashed counts — starting/stopping/deliberately-stopped
// and running-but-unhealthy processes are NOT failures for this contract, and
// health is a separate axis kept out of it. A completed task is NOT crashed, so
// it never counts here (plan 013 D5). (Do not use ProcessState.IsStopped, which
// collapses stopped, crashed, blocked, and completed.)
func crashedProcesses(procs []api.ProcessResponse) []string {
	var crashed []string
	for _, p := range procs {
		if p.Status == string(domain.ProcessStateCrashed) {
			crashed = append(crashed, p.Name)
		}
	}
	return crashed
}

// blockedProcesses returns the names of processes in the blocked state, in the
// order the supervisor reported them (plan 013 D5): a gated process a failed
// required dependency will never let launch. Only exact
// domain.ProcessStateBlocked counts — waiting (still resolving) is not a
// failure.
func blockedProcesses(procs []api.ProcessResponse) []string {
	var blocked []string
	for _, p := range procs {
		if p.Status == string(domain.ProcessStateBlocked) {
			blocked = append(blocked, p.Name)
		}
	}
	return blocked
}

// failedDependencies returns the names of dependencies in the terminal failed
// state (plan 013 D5): fail-policy dependencies that exhausted their budget.
// Only exact "failed" counts — a warned dependency proceeded, and
// pending/checking/polling/healthy/canceled are not failures — so those never
// trip the exit contract.
func failedDependencies(deps []api.DependencyStatusResponse) []string {
	var failed []string
	for _, d := range deps {
		if d.State == supervisorDepFailed {
			failed = append(failed, d.Name)
		}
	}
	return failed
}

// sharedProxyDown is the single "down" predicate behind both the DOWN line and
// the exit-1 contract (D5): a shared proxy whose daemon is unreachable. Sourcing
// it once keeps the rendered message and the exit code from drifting apart. A
// nil block (pre-D5 daemons, test handlers) is never down.
func sharedProxyDown(p *api.ProxyStatusResponse) bool {
	return p != nil && p.Mode == proxyModeShared && !p.DaemonReachable
}

// renderProxyStatus prints the Proxy line for `prox status` (D5). Disabled mode
// (and an absent block, for pre-D5 daemons) prints nothing.
func renderProxyStatus(p *api.ProxyStatusResponse) {
	if p == nil {
		return
	}
	switch p.Mode {
	case proxyModeShared:
		if sharedProxyDown(p) {
			fmt.Println("\nProxy: DOWN — shared proxy daemon unreachable (proxied routes are dead). Check 'prox proxy status'.")
		} else {
			fmt.Printf("\nProxy: shared (running, v%s)\n", p.DaemonVersion)
		}
	case proxyModeStandalone:
		fmt.Println("\nProxy: standalone")
	}
}

// Logs command flags
var (
	logsFollow  bool
	logsLines   int
	logsProcess string
	logsPattern string
	logsRegex   bool
	logsJSON    bool
)

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:   "logs [process]",
	Short: "Show recent logs",
	Long: `Show recent logs from all or specific processes.

Logs can be filtered by process name, pattern, or regex. Use -f to stream
logs continuously.

Examples:
  prox logs                    # All logs
  prox logs web                # Logs from web process
  prox logs -f                 # Stream logs continuously
  prox logs --process web -n 50 # Last 50 lines from web
  prox logs --pattern error    # Filter by pattern
  prox logs --pattern "err.*" --regex  # Filter by regex`,
	Args:              cobra.MaximumNArgs(1),
	RunE:              runLogs,
	ValidArgsFunction: completeProcessNames,
}

func runLogs(cmd *cobra.Command, args []string) error {
	params := domain.LogParams{
		Lines:   logsLines,
		Process: logsProcess,
		Pattern: logsPattern,
		Regex:   logsRegex,
	}

	// If a positional argument is provided, use it as the process filter
	if len(args) > 0 && params.Process == "" {
		params.Process = args[0]
	}

	client := NewClient(apiAddr)

	// Reject a process name the daemon does not have BEFORE the request, for
	// both input paths (positional and --process) and both modes (--follow
	// branches away below). An unknown name is otherwise silently reduced to a
	// filter that matches nothing: no output, exit 0. Best-effort by design —
	// see validateLogProcesses.
	if err := validateLogProcesses(client, params.Process); err != nil {
		return err
	}

	printer := NewLogPrinter()

	if logsFollow {
		return followLogs(cmd, client, params, logsJSON, printer)
	}

	// Get logs
	logs, err := client.GetLogs(commandContext(cmd), params)
	if err != nil {
		return clientError(err, "Is prox running? Try 'prox up' first.")
	}

	if logsJSON {
		if err := json.NewEncoder(os.Stdout).Encode(logs); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode logs: %v\n", err)
		}
	} else {
		for _, entry := range logs.Logs {
			printer.PrintAPIEntry(entry)
		}
		if logs.FilteredCount < logs.TotalCount {
			fmt.Printf("\n(showing %d of %d entries)\n", logs.FilteredCount, logs.TotalCount)
		}
	}
	return nil
}

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [process]",
	Short: "Stop running instance or a single process",
	Long: `Stop the running prox instance or a specific process.

Without arguments, this sends a shutdown signal to the daemon, which will
gracefully stop all processes before exiting.

With a process name, this stops only the specified process while keeping
prox and other processes running.

Examples:
  prox stop          # Stop the entire prox instance
  prox stop api      # Stop only the api process`,
	Args:              cobra.MaximumNArgs(1),
	RunE:              runStop,
	ValidArgsFunction: completeProcessNames,
}

// stopStateWaitTimeout bounds how long `prox stop` waits, after a clean
// process-stop verdict, for the daemon's own exit (state + PID files gone). The
// verdict already confirms the processes stopped; this only confirms the daemon
// process finished tearing down. A timeout here now returns a non-zero error
// (exit 1): an unconfirmed daemon teardown joins the survivors-branch
// "shutdown incomplete" error family for exit-code consistency (v0.1.4, #36;
// plan 011 D2, #73).
const stopStateWaitTimeout = 15 * time.Second

func runStop(cmd *cobra.Command, args []string) error {
	client := NewClient(apiAddr)

	// If a process name is provided, stop just that process
	if len(args) > 0 {
		processName := args[0]
		if err := client.StopProcess(processName); err != nil {
			return processClientError(client, processName, err, "Is prox running? Try 'prox up' first.")
		}
		fmt.Printf("Stopped process: %s\n", processName)
		return nil
	}

	// No args: stop the entire supervisor and wait for the outcome.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "" // daemon.LoadState falls back to os.Getwd internally
	}
	stateWait := stopStateWaitTimeout
	if apiAddrExplicitlySet {
		// --addr deliberately targets a daemon that need not be this
		// directory's, so this directory's state/PID files say nothing about
		// ITS teardown -- and when this directory has its own (possibly stale)
		// state file, waiting on it turns a clean remote stop into a 15s hang
		// and a bogus "shutdown incomplete" exit 1. That is precisely the path
		// the ownership refusal in root.go sends people down, so it must not
		// be a trap. 0 skips the local-exit confirmation (see runFullStop).
		stateWait = 0
	}
	return runFullStop(client, cwd, stateWait)
}

// runFullStop performs a waited full daemon stop and maps the outcome to the CLI
// exit contract (#36, D4; timeout branch revised by plan 011 D2, #73):
//   - transport failure: the outcome is unknown daemon-side → error (exit 1);
//   - old daemon (Waited nil): legacy "Shutdown initiated" → exit 0;
//   - survivors present: print each, return a one-line summary error → exit 1;
//   - clean verdict, daemon confirmed gone: bounded-wait (stateWaitTimeout) for
//     the daemon's state/PID files to vanish succeeds, print a stopped summary
//     → exit 0;
//   - clean verdict, wait times out: print the stopped summary plus a stderr
//     Warning, but still return a "shutdown incomplete" error → exit 1 — the
//     daemon's own teardown was never confirmed, so this joins the
//     survivors-branch error family instead of the exit-0 old-daemon branch.
//     stateWaitTimeout is a parameter so tests can inject a short bound and
//     exercise the poll-timeout branch. A stateWaitTimeout of 0 skips the
//     local-exit confirmation entirely, for callers (explicit --addr) whose
//     target daemon is not this directory's.
func runFullStop(client *Client, cwd string, stateWaitTimeout time.Duration) error {
	result, err := client.Shutdown(true)
	if err != nil {
		// Transport failure mid-wait: the daemon may still be completing its
		// shutdown; we cannot read the verdict from here (#36, D4).
		return shutdownUnknownOutcomeError(cwd, err)
	}

	// Old daemon: it ignored wait=true and acked immediately with no verdict.
	if result.Waited == nil {
		fmt.Println("Shutdown initiated")
		return nil
	}

	// Survivors: print one line each (loud, systemd/docker-style), then return a
	// short summary error so cobra exits 1 WITHOUT reprinting the whole list.
	if len(result.Failures) > 0 {
		for _, f := range result.Failures {
			fmt.Fprintf(os.Stderr, "%s: %s\n", f.Process, f.Error)
		}
		return fmt.Errorf("shutdown incomplete: %d process group(s) did not terminate", len(result.Failures))
	}

	// Clean process-stop verdict. Confirm the daemon itself has exited by waiting
	// (bounded) for the state + PID files to disappear.
	if stateWaitTimeout <= 0 || waitForDaemonExit(cwd, stateWaitTimeout) {
		fmt.Println("Stopped prox")
		return nil
	}
	// Verdict was clean, but the daemon hasn't finished its own teardown within
	// the bounded wait: the process-stop succeeded, yet the daemon's exit is
	// unconfirmed, so this is no longer a clean stop. Surface the stderr warning
	// as before and return a non-zero error joining the survivors-branch
	// "shutdown incomplete" family (v0.1.4, #36; plan 011 D2, #73).
	fmt.Println("Stopped processes")
	fmt.Fprintln(os.Stderr, "Warning: the daemon is still finishing shutdown")
	return fmt.Errorf("shutdown incomplete: daemon still finishing after %s", stateWaitTimeout)
}

// waitForDaemonExit polls until the daemon's state and PID files are both gone
// (its exit cleanup ran) or the timeout elapses. An IsNotExist stat on each path
// means that file is gone. Returns true once both are absent, false on timeout.
// Existence is checked with os.Stat rather than daemon.LoadState so a poll
// iteration never pays for a JSON parse of content that isn't needed; once the
// state file is confirmed gone, later iterations stop re-checking it.
func waitForDaemonExit(cwd string, timeout time.Duration) bool {
	statePath := daemon.StatePath(cwd)
	pidPath := daemon.PIDPath(cwd)
	deadline := time.Now().Add(timeout)
	stateGone := false
	for {
		if !stateGone {
			_, stateErr := os.Stat(statePath)
			stateGone = os.IsNotExist(stateErr)
		}
		if stateGone {
			if _, pidErr := os.Stat(pidPath); os.IsNotExist(pidErr) {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// shutdownUnknownOutcomeError wraps a transport failure that struck while waiting
// for the shutdown verdict. The daemon may still be completing its teardown, so
// the outcome is unknown; when a daemon log file exists (detached mode) we point
// the user at it for the authoritative result.
func shutdownUnknownOutcomeError(cwd string, err error) error {
	msg := "shutdown may still be completing daemon-side; outcome unknown"
	if _, statErr := os.Stat(daemon.LogPath(cwd)); statErr == nil {
		msg = fmt.Sprintf("%s (see %s)", msg, daemon.LogPath(cwd))
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// downCmd represents the down command (alias for stop without arguments)
var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop running instance (alias for stop)",
	Long: `Stop the running prox instance.

This is an alias for the 'stop' command (without arguments).

Examples:
  prox down`,
	Args: cobra.NoArgs,
	RunE: runStop,
}

// startProcessCmd represents the start command for individual processes
var startProcessCmd = &cobra.Command{
	Use:   "start <process>",
	Short: "Start a stopped process",
	Long: `Start a specific process that is currently stopped.

Examples:
  prox start web
  prox start worker`,
	Args:              cobra.ExactArgs(1),
	RunE:              runStartProcess,
	ValidArgsFunction: completeProcessNames,
}

func runStartProcess(cmd *cobra.Command, args []string) error {
	processName := args[0]
	client := NewClient(apiAddr)

	if err := client.StartProcess(processName); err != nil {
		return processClientError(client, processName, err, "Is prox running? Try 'prox up' first.")
	}

	fmt.Printf("Started process: %s\n", processName)
	return nil
}

// restartCmd represents the restart command
var restartCmd = &cobra.Command{
	Use:   "restart <process>",
	Short: "Restart a process",
	Long: `Restart a specific process by name.

The process will be stopped and then started again.

Examples:
  prox restart web
  prox restart worker`,
	Args:              cobra.ExactArgs(1),
	RunE:              runRestart,
	ValidArgsFunction: completeProcessNames,
}

func runRestart(cmd *cobra.Command, args []string) error {
	processName := args[0]
	client := NewClient(apiAddr)

	if err := client.RestartProcess(processName); err != nil {
		return processClientError(client, processName, err, "Is prox running? Try 'prox up' first.")
	}

	fmt.Printf("Restarted process: %s\n", processName)
	return nil
}

// attachCmd represents the attach command
var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach TUI to running daemon",
	Long: `Attach the interactive TUI to a running prox daemon.

This allows you to monitor and interact with processes started with
'prox up -d' (daemon mode). A foreground 'prox up' already shows the same TUI
itself, so attach is for daemons you started detached.

The two differ only in ownership: quitting attach with 'q' leaves the daemon
and its processes running, while quitting a foreground 'prox up' stops them.

Examples:
  prox attach`,
	RunE: runAttach,
}

func runAttach(cmd *cobra.Command, args []string) error {
	// apiAddr is authoritative here, exactly as it is for every other client
	// command: the root PersistentPreRunE hook has already either left an
	// explicitly-passed --addr untouched, or resolved the address from
	// .prox/prox.state AND verified the prox answering there belongs to this
	// project (attach is in the clientCommands allowlist).
	//
	// Attach used to re-derive all of that itself via daemon.GetRunningState,
	// and returned "prox is not running" BEFORE it ever consulted
	// apiAddrExplicitlySet -- which broke --addr, the documented escape hatch,
	// for the one command most likely to want it (plan 020 C3 part D).
	client := NewClient(apiAddr)

	// Verify connection and learn the daemon's registered project directory
	// for the menu-bar label (plan 023 B3).
	status, err := client.GetStatus()
	if err != nil {
		return clientError(err, "Is prox running? Try 'prox up -d' first.")
	}

	// Run TUI in client mode
	// Attach supervises nothing, so it has no out-of-band shutdown to honor:
	// ShutdownCh stays nil and quitting is the user's keypress alone.
	opts := tui.ClientOptions{
		Help: tui.HelpConfig{
			TitleSuffix: "(Client Mode)",
			QuitMessage: "Quit (daemon continues running)",
		},
		ConnectedStatus: "Connected via API",
		ProjectName:     tui.StatusProjectName(status.ProjectDir),
		ShutdownCh:      nil,
	}
	// Attach owns no log manager, so its alt screen gets the deferred-buffer
	// target: mid-session diagnostics (the shared SSE-parse warnings in
	// client.go, an http.Server dump, anything else reaching the stdlib logger)
	// accumulate and replay verbatim to stderr the instant RunClient returns
	// rather than being written over a rendered frame. See
	// runBufferedStdioSession.
	if err := runBufferedStdioSession(func() error {
		return tui.RunClient(client, opts)
	}); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}

// Requests command flags
var (
	requestsFollow    bool
	requestsSubdomain string
	requestsMethod    string
	requestsMinStatus int
	requestsMaxStatus int
	requestsSince     string
	requestsURL       string
	requestsLimit     int
	requestsJSON      bool
	requestsBody      bool
)

// requestsCmd represents the requests command
var requestsCmd = &cobra.Command{
	Use:   "requests [id]",
	Short: "Show proxy requests",
	Long: `Show recent proxy requests or stream them in real-time.

Displays HTTP requests that have been proxied through the HTTPS reverse proxy.
Use filters to narrow down the results. Pass a request ID to show details.

Examples:
  prox requests                    # Show recent requests
  prox requests -f                 # Stream requests in real-time
  prox requests --subdomain api    # Filter by subdomain
  prox requests --method GET       # Filter by HTTP method
  prox requests --min-status 400   # Show errors only (4xx and 5xx)
  prox requests --min-status 400 --max-status 499   # Show client errors only (4xx)
  prox requests --since 5m         # Show requests from the last 5 minutes
  prox requests --url /api         # Filter by URL substring (path+query)
  prox requests --json             # Output as JSON
  prox requests abc1234def56       # Show details for request abc1234def56
  prox requests abc1234def56 --body # Include captured request/response bodies`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRequests,
}

func runRequests(cmd *cobra.Command, args []string) error {
	client := NewClient(apiAddr)

	// If an ID is provided, show request details
	if len(args) > 0 {
		return showRequestDetail(client, args[0], requestsBody, requestsJSON)
	}

	// Validate min-status is within valid HTTP status code range
	if requestsMinStatus != 0 && (requestsMinStatus < 100 || requestsMinStatus > 599) {
		return fmt.Errorf("invalid --min-status value %d: must be between 100 and 599", requestsMinStatus)
	}

	// Validate max-status is within valid HTTP status code range
	if requestsMaxStatus != 0 && (requestsMaxStatus < 100 || requestsMaxStatus > 599) {
		return fmt.Errorf("invalid --max-status value %d: must be between 100 and 599", requestsMaxStatus)
	}

	// When both bounds are set, min must not exceed max
	if requestsMinStatus != 0 && requestsMaxStatus != 0 && requestsMinStatus > requestsMaxStatus {
		return fmt.Errorf("invalid status range: --min-status %d is greater than --max-status %d", requestsMinStatus, requestsMaxStatus)
	}

	since, err := parseSinceFlag(requestsSince)
	if err != nil {
		return err
	}

	params := domain.ProxyRequestParams{
		Subdomain:   requestsSubdomain,
		Method:      strings.ToUpper(requestsMethod),
		MinStatus:   requestsMinStatus,
		MaxStatus:   requestsMaxStatus,
		Since:       since,
		URLContains: requestsURL,
		Limit:       requestsLimit,
	}

	if requestsFollow {
		return followRequests(cmd, client, params, requestsJSON)
	}

	// Get recent requests
	resp, err := client.GetProxyRequests(commandContext(cmd), params)
	if err != nil {
		return clientError(err, "Is prox running with proxy enabled? Try 'prox up' first.")
	}

	if requestsJSON {
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode requests: %v\n", err)
		}
	} else {
		if len(resp.Requests) == 0 {
			fmt.Println("No proxy requests recorded")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTIME\tMETHOD\tSTATUS\tDURATION\tURL")
		fmt.Fprintln(w, "-------\t--------\t------\t------\t--------\t---")

		for _, req := range resp.Requests {
			ts, _ := time.Parse(time.RFC3339Nano, req.Timestamp)
			timeStr := ts.Format("15:04:05")
			duration := fmt.Sprintf("%dms", req.DurationMs)
			if req.InFlight {
				duration = "..."
				if req.Stale {
					// Completion event may have been lost; true outcome
					// unknown (D8, #53). Long-lived streams/transfers can
					// legitimately still be live past this point.
					duration = "stale?"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
				req.ID, timeStr, req.Method, req.StatusCode, duration, req.URL)
		}
		w.Flush()

		if resp.FilteredCount < resp.TotalCount {
			fmt.Printf("\n(showing %d of %d requests)\n", resp.FilteredCount, resp.TotalCount)
		}
	}
	return nil
}

// showRequestDetail displays details for a specific request
func showRequestDetail(client *Client, id string, includeBody, jsonOutput bool) error {
	resp, err := client.GetProxyRequest(id, includeBody)
	if err != nil {
		return clientError(err, "Is prox running with proxy enabled? Try 'prox up' first.")
	}

	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode request: %v\n", err)
		}
		return nil
	}

	// Print formatted output
	ts, _ := time.Parse(time.RFC3339Nano, resp.Timestamp)

	fmt.Printf("Request: %s\n", resp.ID)
	fmt.Printf("Time:    %s\n", ts.Format("2006-01-02 15:04:05.000"))
	fmt.Printf("Method:  %s\n", resp.Method)
	fmt.Printf("URL:     %s\n", resp.URL)
	fmt.Printf("Status:  %d\n", resp.StatusCode)
	switch {
	case resp.InFlight && resp.Stale:
		fmt.Printf("Duration: (in flight, stale?)\n")
	case resp.InFlight:
		fmt.Printf("Duration: (in flight)\n")
	default:
		fmt.Printf("Duration: %dms\n", resp.DurationMs)
	}
	fmt.Printf("Remote:  %s\n", resp.RemoteAddr)

	if resp.Details != nil {
		// Print request headers
		if len(resp.Details.RequestHeaders) > 0 {
			fmt.Println("\n--- Request Headers ---")
			printHeaders(resp.Details.RequestHeaders)
		}

		// Print response headers
		if len(resp.Details.ResponseHeaders) > 0 {
			fmt.Println("\n--- Response Headers ---")
			printHeaders(resp.Details.ResponseHeaders)
		}

		// Print request body
		if resp.Details.RequestBody != nil {
			printCapturedBody("Request Body", resp.Details.RequestBody, includeBody)
		}

		// Print response body
		if resp.Details.ResponseBody != nil {
			printCapturedBody("Response Body", resp.Details.ResponseBody, includeBody)
		}
	} else if resp.InFlight && resp.Stale {
		fmt.Println("\n(request in flight, stale? — the completion event may have been lost; true outcome unknown)")
	} else if resp.InFlight {
		fmt.Println("\n(request in flight — details arrive on completion)")
	} else {
		fmt.Println("\n" + captureHint(resp.StatusCode, projectCaptureEnabledHint(configPath)))
	}

	return nil
}

// captureHint explains why a completed request record has no Details (plan
// 012 D1, C4). It only has two RELIABLE signals to work from -- the record's
// own status code, and a static config file read from the CLI side -- so it
// resolves down to two confident tiers and one honest catch-all rather than
// trying to name the exact cause (a Codex review of the original three-way
// version found it guessing wrong in several real cases: --no-capture is an
// in-memory-only override the static config can't see, and a legitimately
// metadata-only routing-error record -- proxy.go's unknown-subdomain path,
// not a capture failure -- has a non-101 status too):
//
//   - a metadata-only WebSocket/101 upgrade record: the proxy's hijack path
//     (internal/proxyd/dynamic_proxy.go, internal/proxy/proxy.go) records only
//     the protocol switch, regardless of capture config, because all traffic
//     after the upgrade bypasses the capture writer entirely. Detected from
//     the record itself: StatusCode == http.StatusSwitchingProtocols. Reliable.
//   - the static config says capture is disabled: reliable ONLY in the
//     direction "config says off" (an on-disk proxy.capture.enabled: false, or
//     the proxy itself disabled) -- captureEnabled being true does NOT mean
//     capture actually ran (see below), so this tier is one-way.
//   - otherwise: a neutral catch-all naming every real possibility --
//     --no-capture for this run, a metadata-only record that isn't a 101
//     (e.g. a routing error), or the daemon's capture manager being
//     unavailable -- rather than confidently misdiagnosing one of them.
//
// captureEnabled is the caller's best static read of whether capture is
// configured on for this project (config.ProxyConfig.CaptureEffectivelyEnabled);
// pass false when no project config could be loaded so the message degrades to
// the config-disabled tier rather than the catch-all.
func captureHint(statusCode int, captureEnabled bool) string {
	if statusCode == http.StatusSwitchingProtocols {
		return "(metadata only - WebSocket/101 upgrade traffic is never captured, regardless of capture config)"
	}
	if !captureEnabled {
		return "(capture not enabled - proxy.capture.enabled is false or --no-capture was used; run 'prox up --capture' or drop --no-capture to enable)"
	}
	return "(no captured details for this record - the run may have used --no-capture, the record may be metadata-only (e.g. a proxy routing error), or capture may be unavailable in the daemon; check 'prox proxy status')"
}

// projectCaptureEnabledHint best-effort determines whether capture is
// statically configured on for the current project, to pick the right
// captureHint tier. It prefers the config file path recorded in
// .prox/prox.state (daemon.State.ConfigFile) -- the actual file the RUNNING
// daemon loaded via its own -c/--config -- over fallbackConfigPath (the CLI
// invocation's own --config/default), because those can differ (a Codex
// review finding: `prox requests <id> -c other.yaml` or a daemon started with
// a non-default config would otherwise read the wrong file). Falls back to
// fallbackConfigPath when no state file is found (mirrors discoverAPIAddress's
// state-then-config-file priority). Any load/parse failure degrades to false
// -- this is a hint, not a control-plane decision, so it must never fail the
// whole `prox requests <id>` command.
func projectCaptureEnabledHint(fallbackConfigPath string) bool {
	path := fallbackConfigPath
	if cwd, err := os.Getwd(); err == nil {
		if state, serr := daemon.LoadState(cwd); serr == nil && state.ConfigFile != "" {
			path = state.ConfigFile
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return false
	}
	return cfg.Proxy.CaptureEffectivelyEnabled()
}

// printCapturedBody prints one captured body section: a header line with size,
// truncation, content type and content encoding, followed by the body content
// (or a note that it is unavailable / must be requested with --body).
func printCapturedBody(label string, body *api.CapturedBodyResponse, includeBody bool) {
	fmt.Printf("\n--- %s (%d bytes", label, body.Size)
	if body.Truncated {
		fmt.Printf(", %d bytes captured, truncated", body.CapturedSize)
	}
	if body.ContentType != "" {
		fmt.Printf(", %s", body.ContentType)
	}
	if body.ContentEncoding != "" {
		fmt.Printf(", encoding: %s", body.ContentEncoding)
	}
	fmt.Println(") ---")

	if body.UnavailableReason != "" {
		fmt.Println("(body no longer available)")
		return
	}

	if includeBody && body.Data != "" {
		if body.IsBinary {
			fmt.Println("[binary data, base64 encoded]")
		}
		fmt.Println(body.Data)
	} else if !includeBody && body.Size > 0 {
		fmt.Println("(use --body to show content)")
	}
}

// printHeaders prints HTTP headers in a readable format
func printHeaders(headers map[string][]string) {
	for name, values := range headers {
		for _, value := range values {
			fmt.Printf("  %s: %s\n", name, value)
		}
	}
}

// isTerminal returns true if stdout is connected to a terminal.
func isTerminal() bool {
	return isTTY(os.Stdout)
}

// isInteractiveStdio reports whether BOTH stdin and stdout are terminals, i.e.
// whether a full-screen TUI can actually be driven here. isTerminal above only
// asks about output, which is the right question for "may I colorize / draw a
// table"; a TUI additionally needs keyboard input, so `prox up --tui` under
// `prox up --tui | tee log`, a CI runner, or an agent harness has to be refused
// rather than left drawing an alt-screen nobody can quit.
func isInteractiveStdio() bool {
	return isTTY(os.Stdin) && isTTY(os.Stdout)
}

// isTTY reports whether f is a real terminal. The probe is isatty, not
// the ModeCharDevice bit: /dev/null is a character device too, so the mode-bit
// check would wave through `--tui </dev/null` and leave a TUI nobody can drive
// (cursor review, C2). go-isatty is already in the module graph via bubbletea —
// this is the same answer the TUI itself would get. A nil file counts as "not a
// terminal" — the conservative answer for every caller.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	// IsCygwinTerminal covers mintty/MSYS2 on Windows, where the terminal is a
	// pipe that IsTerminal alone rejects (CodeRabbit, PR #88).
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// parseSinceFlag parses the --since flag value, accepting either an RFC3339
// timestamp or a Go duration (e.g. "5m", "1h"). A duration is treated as
// "ago from now": time.Now() is captured exactly once here so a single
// invocation (list or stream) uses one consistent cutoff instant rather than
// re-evaluating "now" per record. Empty input returns the zero time (no
// filter). A malformed value that matches neither format is a clear error.
func parseSinceFlag(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("invalid --since value %q: duration must not be negative", value)
		}
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q: must be an RFC3339 timestamp or a duration (e.g. 5m, 1h)", value)
}

func printProxyRequest(req api.ProxyRequestResponse) {
	ts, _ := time.Parse(time.RFC3339Nano, req.Timestamp)
	timeStr := ts.Format("15:04:05")

	// Only use colors if stdout is a terminal
	statusColor := ""
	resetColor := ""
	if isTerminal() {
		resetColor = constants.ColorReset
		switch {
		case req.StatusCode >= 500:
			statusColor = constants.ColorStatusServer
		case req.StatusCode >= 400:
			statusColor = constants.ColorStatusClient
		case req.StatusCode >= 300:
			statusColor = constants.ColorStatusRedirect
		case req.StatusCode >= 200:
			statusColor = constants.ColorStatusSuccess
		}
	}

	duration := fmt.Sprintf("(%dms)", req.DurationMs)
	if req.InFlight {
		duration = "(in flight)"
		if req.Stale {
			duration = "(in flight, stale?)"
		}
	}
	fmt.Printf("%s %s %s%d%s %s %s\n",
		req.ID, timeStr, statusColor, req.StatusCode, resetColor, req.Method, duration)
	fmt.Printf("       %s\n", req.URL)
}

func init() {
	// Register all commands
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(startProcessCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(attachCmd)
	rootCmd.AddCommand(requestsCmd)

	// Status command flags
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")

	// Logs command flags
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Stream logs continuously")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", constants.DefaultLogLimit, "Number of lines to show")
	logsCmd.Flags().StringVar(&logsProcess, "process", "", "Filter by process (comma-separated)")
	logsCmd.Flags().StringVar(&logsPattern, "pattern", "", "Filter by pattern")
	logsCmd.Flags().BoolVar(&logsRegex, "regex", false, "Treat pattern as regex")
	logsCmd.Flags().BoolVar(&logsJSON, "json", false, "Output as JSON")

	// Requests command flags
	requestsCmd.Flags().BoolVarP(&requestsFollow, "follow", "f", false, "Stream requests continuously")
	requestsCmd.Flags().StringVar(&requestsSubdomain, "subdomain", "", "Filter by subdomain")
	requestsCmd.Flags().StringVar(&requestsMethod, "method", "", "Filter by HTTP method (GET, POST, etc.)")
	requestsCmd.Flags().IntVar(&requestsMinStatus, "min-status", 0, "Filter by minimum status code (e.g., 400 for errors)")
	requestsCmd.Flags().IntVar(&requestsMaxStatus, "max-status", 0, "Filter by maximum status code (combine with --min-status for ranges)")
	requestsCmd.Flags().StringVar(&requestsSince, "since", "", "Filter to requests since this time (RFC3339 timestamp or duration like 5m, 1h)")
	requestsCmd.Flags().StringVar(&requestsURL, "url", "", "Filter by URL substring (path+query, case-insensitive)")
	requestsCmd.Flags().IntVarP(&requestsLimit, "limit", "n", constants.DefaultProxyRequestLimit, "Number of requests to show")
	requestsCmd.Flags().BoolVar(&requestsJSON, "json", false, "Output as JSON")
	requestsCmd.Flags().BoolVar(&requestsBody, "body", false, "Include request/response bodies when showing details")

	// Register completion for --process flag
	// Error is ignored as it only fails for invalid flag names, which would be a programming error
	_ = logsCmd.RegisterFlagCompletionFunc("process", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return getProcessNames(), cobra.ShellCompDirectiveNoFileComp
	})
}

// commandContext returns the command's context, falling back to Background when
// the command was invoked outside Execute (e.g. a test calling RunE directly),
// which leaves cobra's context nil.
func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// formatDuration formats a duration nicely
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
