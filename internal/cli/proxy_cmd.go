package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/charliek/prox/internal/proxyd"
	"github.com/spf13/cobra"
)

// Proxy command flags
var (
	proxyJSON  bool
	proxyForce bool
)

// proxyCmd is the parent command for shared proxy daemon management.
var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Manage the shared proxy daemon (advanced)",
	Long: `Manage the shared proxy daemon that handles port sharing between projects.

The proxy daemon is normally started and stopped automatically by 'prox up'
and 'prox down'. These subcommands are for debugging and advanced usage.

Examples:
  prox proxy status    # Show daemon status
  prox proxy routes    # List registered routes
  prox proxy stop      # Stop the daemon`,
}

// proxyStatusCmd shows daemon status.
var proxyStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show proxy daemon status",
	RunE:  runProxyStatus,
}

func runProxyStatus(cmd *cobra.Command, args []string) error {
	client, err := connectDaemon()
	if err != nil {
		return err
	}

	status, err := client.Status()
	if err != nil {
		return fmt.Errorf("failed to get daemon status: %w", err)
	}

	if proxyJSON {
		return json.NewEncoder(os.Stdout).Encode(status)
	}

	fmt.Printf("Proxy Daemon\n")
	fmt.Printf("  Version:    %s\n", status.Version)
	fmt.Printf("  PID:        %d\n", status.PID)
	fmt.Printf("  Uptime:     %s\n", status.Uptime)
	fmt.Printf("  Projects:   %d\n", status.ProjectCount)
	fmt.Printf("  Routes:     %d\n", status.RouteCount)
	if len(status.ListenerPorts) > 0 {
		fmt.Printf("  Ports:      %v\n", status.ListenerPorts)
	}
	if status.CaptureDiskBudget > 0 {
		fmt.Printf("  Capture:    %s used / %s budget on disk\n",
			formatBytes(status.CaptureDiskUsed), formatBytes(status.CaptureDiskBudget))
	}

	if len(status.Routes) > 0 {
		fmt.Println()
		printRoutesTable(status.Routes)
	}
	return nil
}

// proxyRoutesCmd lists all registered routes.
var proxyRoutesCmd = &cobra.Command{
	Use:   "routes",
	Short: "List registered proxy routes",
	RunE:  runProxyRoutes,
}

func runProxyRoutes(cmd *cobra.Command, args []string) error {
	client, err := connectDaemon()
	if err != nil {
		return err
	}

	routes, err := client.Routes()
	if err != nil {
		return fmt.Errorf("failed to get routes: %w", err)
	}

	if proxyJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"routes": routes})
	}

	if len(routes) == 0 {
		fmt.Println("No routes registered")
		return nil
	}

	printRoutesTable(routes)
	return nil
}

// proxyStopCmd stops the proxy daemon.
var proxyStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the proxy daemon",
	Long: `Stop the shared proxy daemon.

By default, this fails if any projects have active routes registered.
Use --force to stop the daemon regardless, which will disconnect all projects.`,
	RunE: runProxyStop,
}

func runProxyStop(cmd *cobra.Command, args []string) error {
	client, err := connectDaemon()
	if err != nil {
		return err
	}

	if err := client.Shutdown(proxyForce); err != nil {
		return fmt.Errorf("%w", err)
	}

	fmt.Println("Proxy daemon shutdown initiated")
	return nil
}

// connectDaemon connects to the running proxy daemon.
func connectDaemon() (*proxyd.Client, error) {
	socketPath := proxyd.SocketPath()
	client := proxyd.NewClient(socketPath)

	// Quick health check
	if _, err := client.Health(); err != nil {
		return nil, fmt.Errorf("proxy daemon is not running\nIt starts automatically when you run 'prox up' with proxy configured")
	}
	return client, nil
}

// formatBytes renders a byte count with a binary (KiB/MiB/GiB) suffix for the
// human `prox proxy status` capture line (#69). Sub-KiB values print as raw
// bytes; larger values use one decimal place.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printRoutesTable prints routes in a tabwriter table.
func printRoutesTable(routes []proxyd.RouteInfo) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tPORT\tPROTOCOL\tTARGET\tPROJECT\tPID")
	fmt.Fprintln(w, "--------\t----\t--------\t------\t-------\t---")

	for _, r := range routes {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s:%d\t%s\t%d\n",
			r.Hostname, r.Port, r.Protocol,
			r.Target.Host, r.Target.Port,
			r.ProjectDir, r.PID)
	}
	w.Flush()
}

func init() {
	rootCmd.AddCommand(proxyCmd)

	proxyCmd.AddCommand(proxyStatusCmd)
	proxyCmd.AddCommand(proxyRoutesCmd)
	proxyCmd.AddCommand(proxyStopCmd)

	// Shared flags
	proxyStatusCmd.Flags().BoolVar(&proxyJSON, "json", false, "Output as JSON")
	proxyRoutesCmd.Flags().BoolVar(&proxyJSON, "json", false, "Output as JSON")
	proxyStopCmd.Flags().BoolVar(&proxyForce, "force", false, "Force stop even with active routes")
}
