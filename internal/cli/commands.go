package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/tui"
)

// cmdStatus handles the 'status' command
func (a *App) cmdStatus(args []string) int {
	jsonOutput := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOutput = true
		}
	}

	client := NewClient(a.apiAddr)

	// Get status
	status, err := client.GetStatus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is prox running? Try 'prox up' first.\n")
		return 1
	}

	// Get processes
	processes, err := client.GetProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if jsonOutput {
		output := map[string]interface{}{
			"status":    status,
			"processes": processes.Processes,
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode output: %v\n", err)
		}
		return 0
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

	return 0
}

// cmdLogs handles the 'logs' command
func (a *App) cmdLogs(args []string) int {
	params := domain.LogParams{
		Lines: constants.DefaultLogLimit,
	}
	follow := false
	jsonOutput := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--follow":
			follow = true
		case arg == "--json":
			jsonOutput = true
		case arg == "-n" || arg == "--lines":
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "Error: invalid lines value %q (must be a positive integer)\n", val)
				return 1
			}
			params.Lines = n
			i = newIdx
		case arg == "--process":
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			params.Process = val
			i = newIdx
		case arg == "--pattern":
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			params.Pattern = val
			i = newIdx
		case arg == "--regex":
			params.Regex = true
		case !strings.HasPrefix(arg, "-"):
			// Treat as process name if not already set
			if params.Process == "" {
				params.Process = arg
			}
		}
	}

	client := NewClient(a.apiAddr)

	if follow {
		// Stream logs
		err := client.StreamLogs(params, func(entry api.LogEntryResponse) {
			if jsonOutput {
				if err := json.NewEncoder(os.Stdout).Encode(entry); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to encode log entry: %v\n", err)
				}
			} else {
				printLogEntry(entry)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	} else {
		// Get logs
		logs, err := client.GetLogs(params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		if jsonOutput {
			if err := json.NewEncoder(os.Stdout).Encode(logs); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to encode logs: %v\n", err)
			}
		} else {
			for _, entry := range logs.Logs {
				printLogEntry(entry)
			}
			if logs.FilteredCount < logs.TotalCount {
				fmt.Printf("\n(showing %d of %d entries)\n", logs.FilteredCount, logs.TotalCount)
			}
		}
	}

	return 0
}

// cmdStop handles the 'stop' command
func (a *App) cmdStop(args []string) int {
	client := NewClient(a.apiAddr)

	if err := client.Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Println("Shutdown initiated")
	return 0
}

// cmdRestart handles the 'restart' command
func (a *App) cmdRestart(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: prox restart <process>\n")
		return 1
	}

	processName := args[0]
	client := NewClient(a.apiAddr)

	if err := client.RestartProcess(processName); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to restart %s: %v\n", processName, err)
		return 1
	}

	fmt.Printf("Restarted process: %s\n", processName)
	return 0
}

// printLogEntry prints a log entry with colors
func printLogEntry(entry api.LogEntryResponse) {
	color := processColor(entry.Process)
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		ts = time.Now()
	}

	fmt.Printf("%s %s%-8s%s │ %s\n",
		ts.Format("15:04:05"),
		color, entry.Process, constants.ColorReset,
		entry.Line)
}

// processColor returns a color for a process name
func processColor(name string) string {
	// Simple hash
	hash := 0
	for _, c := range name {
		hash += int(c)
	}

	return constants.ProcessColors[hash%len(constants.ProcessColors)]
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

// cmdDown handles the 'down' command (alias for stop)
func (a *App) cmdDown(args []string) int {
	return a.cmdStop(args)
}

// cmdAttach handles the 'attach' command - connects TUI to running daemon
func (a *App) cmdAttach(args []string) int {
	// Get working directory
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if daemon is running
	state, err := daemon.GetRunningState(cwd)
	if err != nil {
		if err == daemon.ErrNotRunning {
			fmt.Fprintf(os.Stderr, "Error: prox is not running\n")
			fmt.Fprintf(os.Stderr, "Start it with 'prox up -d' first\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Use discovered API address or explicitly set one
	apiAddr := a.apiAddr
	if !a.apiAddrExplicitlySet {
		apiAddr = fmt.Sprintf("http://%s:%d", state.Host, state.Port)
	}

	// Create client
	client := NewClient(apiAddr)

	// Verify connection
	_, err = client.GetStatus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Run TUI in client mode
	if err := tui.RunClient(client); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	return 0
}
