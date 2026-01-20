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
		fmt.Fprintf(os.Stderr, "Error connecting to prox: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is prox running? Try 'prox up' first.\n")
		return 1
	}

	// Get processes
	processes, err := client.GetProcesses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting processes: %v\n", err)
		return 1
	}

	if jsonOutput {
		output := map[string]interface{}{
			"status":    status,
			"processes": processes.Processes,
		}
		json.NewEncoder(os.Stdout).Encode(output)
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
	params := LogParams{
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
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					params.Lines = n
				} else {
					fmt.Fprintf(os.Stderr, "Invalid lines value: %s (must be a positive integer)\n", args[i+1])
					return 1
				}
				i++
			}
		case arg == "--process":
			if i+1 < len(args) {
				params.Process = args[i+1]
				i++
			}
		case arg == "--pattern":
			if i+1 < len(args) {
				params.Pattern = args[i+1]
				i++
			}
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
				json.NewEncoder(os.Stdout).Encode(entry)
			} else {
				printLogEntry(entry)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error streaming logs: %v\n", err)
			return 1
		}
	} else {
		// Get logs
		logs, err := client.GetLogs(params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting logs: %v\n", err)
			return 1
		}

		if jsonOutput {
			json.NewEncoder(os.Stdout).Encode(logs)
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
		fmt.Fprintf(os.Stderr, "Error stopping prox: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "Error restarting %s: %v\n", processName, err)
		return 1
	}

	fmt.Printf("Restarted process: %s\n", processName)
	return 0
}

// printLogEntry prints a log entry with colors
func printLogEntry(entry api.LogEntryResponse) {
	// Parse timestamp
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		ts = time.Now()
	}

	// Color for process
	color := processColor(entry.Process)

	// Red for stderr
	lineColor := ""
	if entry.Stream == "stderr" {
		lineColor = constants.ColorBrightRed
	}

	fmt.Printf("%s %s%-8s%s │ %s%s%s\n",
		ts.Format("15:04:05"),
		color, entry.Process, constants.ColorReset,
		lineColor, entry.Line, constants.ColorReset)
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
