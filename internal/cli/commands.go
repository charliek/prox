package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charliek/prox/internal/api"
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

	if statusJSON {
		output := map[string]interface{}{
			"status":    status,
			"processes": processes.Processes,
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode output: %v\n", err)
		}
		if proxyDown {
			return errSharedProxyDown
		}
		return nil
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
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%s\n",
			p.Name, p.Status, p.PID, uptime, p.Restarts, p.Health)
	}
	w.Flush()

	// Proxy line (D5). Printed after the table so the process status is always
	// visible even when the shared proxy is down (which then forces exit 1).
	renderProxyStatus(status.Proxy)
	if proxyDown {
		return errSharedProxyDown
	}
	return nil
}

// errSharedProxyDown is returned by runStatus when the shared proxy daemon is
// unreachable, so `prox status` exits non-zero (D5). The message has already
// been printed to the user; rootCmd sets SilenceErrors, so cobra does not
// reprint it — the error only drives the exit code.
var errSharedProxyDown = fmt.Errorf("shared proxy daemon is unreachable")

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

	printer := NewLogPrinter()

	if logsFollow {
		// Stream logs via channel
		ch, err := client.StreamLogsChannel(params)
		if err != nil {
			return clientError(err, "Is prox running? Try 'prox up' first.")
		}
		for entry := range ch {
			if logsJSON {
				if err := json.NewEncoder(os.Stdout).Encode(entry); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to encode log entry: %v\n", err)
				}
			} else {
				printer.PrintAPIEntry(entry)
			}
		}
	} else {
		// Get logs
		logs, err := client.GetLogs(params)
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
// process finished tearing down. A timeout here is a warning, not an error.
const stopStateWaitTimeout = 15 * time.Second

func runStop(cmd *cobra.Command, args []string) error {
	client := NewClient(apiAddr)

	// If a process name is provided, stop just that process
	if len(args) > 0 {
		processName := args[0]
		if err := client.StopProcess(processName); err != nil {
			return clientError(err, "Is prox running? Try 'prox up' first.")
		}
		fmt.Printf("Stopped process: %s\n", processName)
		return nil
	}

	// No args: stop the entire supervisor and wait for the outcome.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "" // daemon.LoadState falls back to os.Getwd internally
	}
	return runFullStop(client, cwd, stopStateWaitTimeout)
}

// runFullStop performs a waited full daemon stop and maps the outcome to the CLI
// exit contract (#36, D4):
//   - transport failure: the outcome is unknown daemon-side → error (exit 1);
//   - old daemon (Waited nil): legacy "Shutdown initiated" → exit 0;
//   - survivors present: print each, return a one-line summary error → exit 1;
//   - clean verdict: bounded-wait (stateWaitTimeout) for the daemon's state/PID
//     files to vanish, print a stopped summary → exit 0 (a wait timeout prints a
//     Warning to stderr but stays exit 0 — the process-stop verdict already
//     succeeded). stateWaitTimeout is a parameter so tests can inject a short
//     bound and exercise the poll-timeout branch.
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
	if waitForDaemonExit(cwd, stateWaitTimeout) {
		fmt.Println("Stopped prox")
	} else {
		// Verdict was clean, so exit stays 0; the daemon just hasn't finished its
		// own teardown within the bounded wait. Surface it as a stderr warning.
		fmt.Println("Stopped processes")
		fmt.Fprintln(os.Stderr, "Warning: the daemon is still finishing shutdown")
	}
	return nil
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
		return clientError(err, "Is prox running? Try 'prox up' first.")
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
		return clientError(err, "Is prox running? Try 'prox up' first.")
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
'prox up -d' (daemon mode).

Examples:
  prox attach`,
	RunE: runAttach,
}

func runAttach(cmd *cobra.Command, args []string) error {
	// Get working directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// Check if daemon is running
	state, err := daemon.GetRunningState(cwd)
	if err != nil {
		if err == daemon.ErrNotRunning {
			return fmt.Errorf("prox is not running\nStart it with 'prox up -d' first")
		}
		return fmt.Errorf("failed to get daemon state: %w", err)
	}

	// Use discovered API address or explicitly set one
	addr := apiAddr
	if !apiAddrExplicitlySet {
		addr = fmt.Sprintf("http://%s:%d", state.Host, state.Port)
	}

	// Create client
	client := NewClient(addr)

	// Verify connection
	_, err = client.GetStatus()
	if err != nil {
		return clientError(err, "Is prox running? Try 'prox up -d' first.")
	}

	// Run TUI in client mode
	if err := tui.RunClient(client); err != nil {
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
		// Stream requests via SSE
		ch, err := client.StreamProxyRequestsChannel(params)
		if err != nil {
			return clientError(err, "Is prox running with proxy enabled? Try 'prox up' first.")
		}
		for req := range ch {
			if requestsJSON {
				if err := json.NewEncoder(os.Stdout).Encode(req); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to encode request: %v\n", err)
				}
			} else {
				printProxyRequest(req)
			}
		}
	} else {
		// Get recent requests
		resp, err := client.GetProxyRequests(params)
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
		fmt.Println("\n(capture not enabled - use 'prox up --capture' to enable)")
	}

	return nil
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
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
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

// clientError wraps an error with an optional hint for the user.
// This provides consistent error messages for client commands.
func clientError(err error, hint string) error {
	if hint != "" {
		return fmt.Errorf("%w\n%s", err, hint)
	}
	return err
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
