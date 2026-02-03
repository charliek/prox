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

	if statusJSON {
		output := map[string]interface{}{
			"status":    status,
			"processes": processes.Processes,
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode output: %v\n", err)
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
	return nil
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

	// No args: stop the entire supervisor
	if err := client.Shutdown(); err != nil {
		return clientError(err, "Is prox running? Try 'prox up' first.")
	}

	fmt.Println("Shutdown initiated")
	return nil
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
	requestsLimit     int
	requestsJSON      bool
)

// requestsCmd represents the requests command
var requestsCmd = &cobra.Command{
	Use:   "requests",
	Short: "Show proxy requests",
	Long: `Show recent proxy requests or stream them in real-time.

Displays HTTP requests that have been proxied through the HTTPS reverse proxy.
Use filters to narrow down the results.

Examples:
  prox requests                    # Show recent requests
  prox requests -f                 # Stream requests in real-time
  prox requests --subdomain api    # Filter by subdomain
  prox requests --method GET       # Filter by HTTP method
  prox requests --min-status 400   # Show errors only (4xx and 5xx)
  prox requests --json             # Output as JSON`,
	RunE: runRequests,
}

func runRequests(cmd *cobra.Command, args []string) error {
	// Validate min-status is within valid HTTP status code range
	if requestsMinStatus != 0 && (requestsMinStatus < 100 || requestsMinStatus > 599) {
		return fmt.Errorf("invalid --min-status value %d: must be between 100 and 599", requestsMinStatus)
	}

	params := domain.ProxyRequestParams{
		Subdomain: requestsSubdomain,
		Method:    strings.ToUpper(requestsMethod),
		MinStatus: requestsMinStatus,
		Limit:     requestsLimit,
	}

	client := NewClient(apiAddr)

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
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dms\t%s\n",
					req.ID, timeStr, req.Method, req.StatusCode, req.DurationMs, req.URL)
			}
			w.Flush()

			if resp.FilteredCount < resp.TotalCount {
				fmt.Printf("\n(showing %d of %d requests)\n", resp.FilteredCount, resp.TotalCount)
			}
		}
	}
	return nil
}

// isTerminal returns true if stdout is connected to a terminal.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
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

	fmt.Printf("%s %s %s%d%s %s (%dms)\n",
		req.ID, timeStr, statusColor, req.StatusCode, resetColor, req.Method, req.DurationMs)
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
	requestsCmd.Flags().IntVarP(&requestsLimit, "limit", "n", constants.DefaultProxyRequestLimit, "Number of requests to show")
	requestsCmd.Flags().BoolVar(&requestsJSON, "json", false, "Output as JSON")

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
