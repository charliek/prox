package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

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
	Use:   "stop",
	Short: "Stop running instance",
	Long: `Stop the running prox instance.

This sends a shutdown signal to the daemon, which will gracefully stop
all processes before exiting.

Examples:
  prox stop`,
	RunE: runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
	client := NewClient(apiAddr)

	if err := client.Shutdown(); err != nil {
		return clientError(err, "Is prox running? Try 'prox up' first.")
	}

	fmt.Println("Shutdown initiated")
	return nil
}

// downCmd represents the down command (alias for stop)
var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop running instance (alias for stop)",
	Long: `Stop the running prox instance.

This is an alias for the 'stop' command.

Examples:
  prox down`,
	RunE: runStop,
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

func init() {
	// Register all commands
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(attachCmd)

	// Status command flags
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")

	// Logs command flags
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Stream logs continuously")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", constants.DefaultLogLimit, "Number of lines to show")
	logsCmd.Flags().StringVar(&logsProcess, "process", "", "Filter by process (comma-separated)")
	logsCmd.Flags().StringVar(&logsPattern, "pattern", "", "Filter by pattern")
	logsCmd.Flags().BoolVar(&logsRegex, "regex", false, "Treat pattern as regex")
	logsCmd.Flags().BoolVar(&logsJSON, "json", false, "Output as JSON")

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
