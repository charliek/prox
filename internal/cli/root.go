package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/version"
	"github.com/spf13/cobra"
)

// Global flags
var (
	configPath           string
	apiAddr              string
	apiAddrExplicitlySet bool
	detach               bool
	verbose              bool
)

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "prox",
	Short: "A modern process manager",
	Long: `prox is a modern process manager that helps you manage multiple
processes for local development. It supports:
  - Process supervision with automatic restarts
  - Real-time log aggregation and filtering
  - HTTP/HTTPS reverse proxy with hostname routing, shared across projects
  - Proxied request inspection (prox requests) with captured bodies
  - Interactive TUI for monitoring
  - Background daemon mode`,
	Version:           version.Version,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRunE: rootPersistentPreRunE,
}

// rootPersistentPreRunE resolves apiAddr before any command runs. It is a
// standalone function (rather than an inline closure) so tests can drive it
// directly against a synthetic *cobra.Command without going through the full
// rootCmd tree.
//
// An explicit --addr (cmd.Flags().Changed("addr")) always wins and skips
// discovery, including the discovery error below. For commands in the
// clientCommands allowlist, a discovery failure is returned as an error —
// there is no silent fallback to constants.DefaultAPIAddress (D3). Commands
// outside the allowlist (version, up, proxy, completion, __complete, help)
// never call discoverAPIAddress and so are unaffected by missing state.
func rootPersistentPreRunE(cmd *cobra.Command, args []string) error {
	// Recompute (assign, don't just latch) on every invocation: the global is
	// also read by commands.go, and a stale true from a prior in-process
	// invocation (tests drive the hook repeatedly) would permanently suppress
	// discovery and its error (CodeRabbit PR #68).
	apiAddrExplicitlySet = cmd.Flags().Changed("addr")

	// For client commands, try to discover API address if not explicitly set.
	if needsAPIDiscovery(cmd) && !apiAddrExplicitlySet {
		addr, err := discoverAPIAddress()
		if err != nil {
			return err
		}
		apiAddr = addr
	}
	return nil
}

// needsAPIDiscovery reports whether cmd is a TOP-LEVEL client command that
// talks to the project daemon API via apiAddr. The allowlist is matched by
// name AND parent: nested subcommands can share a name with a top-level
// client command (`prox proxy status`/`prox proxy stop` vs `prox status`/
// `prox stop`) but talk to the proxy daemon's Unix socket, not apiAddr —
// matching on bare cmd.Name() would wrongly demand (and now fail) discovery
// for them outside a project directory.
func needsAPIDiscovery(cmd *cobra.Command) bool {
	return clientCommands[cmd.Name()] && cmd.Parent() == cmd.Root()
}

// clientCommands is the discovery allowlist: the commands that talk to the
// daemon API via apiAddr and therefore need the address discovered from
// .prox/prox.state (or the config file) when --addr is not given. Every
// command whose handler calls NewClient with apiAddr MUST be listed here, or
// it silently talks to the :5555 default and breaks against dynamic-port
// daemons (the default) — the exact bug 'start' had (#41) and 'requests' had
// (#43). TestClientCommandsDiscoveryAllowlist pins this contract.
var clientCommands = map[string]bool{
	"status":   true,
	"logs":     true,
	"stop":     true,
	"start":    true,
	"restart":  true,
	"down":     true,
	"attach":   true,
	"requests": true,
}

// Execute runs the root command.
// If the process is the proxy daemon child, it runs the daemon instead of
// normal CLI dispatch.
func Execute() {
	if proxyd.IsDaemonProcess() {
		if err := proxyd.RunDaemon(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Proxy daemon error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("prox version %s\n", version.Version)
	},
}

func init() {
	// Persistent flags available to all subcommands
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", constants.DefaultConfigFile, "Config file")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "addr", constants.DefaultAPIAddress, "API address for remote commands (used only when passed explicitly; otherwise discovered from .prox/prox.state or the config file)")
	rootCmd.PersistentFlags().BoolVarP(&detach, "detach", "d", false, "Run in background (daemon mode)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Set version template
	rootCmd.SetVersionTemplate("prox version {{.Version}}\n")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
}

// loadAPIAddrFromConfig attempts to read the API address from the config file.
// Returns empty string if config doesn't exist or can't be read.
func loadAPIAddrFromConfig() string {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "" // Config doesn't exist or is invalid, use default
	}

	host := cfg.API.Host
	if host == "" {
		host = constants.DefaultAPIHost
	}
	port := cfg.API.Port
	if port == 0 {
		return "" // Port is dynamic, must discover from state file
	}

	return fmt.Sprintf("http://%s:%d", host, port)
}

// errNoRunningInstance is returned by discoverAPIAddress when neither the
// state file nor the config file yields an API address and the caller did
// not pass --addr explicitly. There is deliberately no silent fallback to
// constants.DefaultAPIAddress here (see D3 in the hardening plan): dialing
// the compiled-in :5555 default against a dynamic-port daemon silently talks
// to nothing, or worse, to an unrelated daemon.
var errNoRunningInstance = fmt.Errorf(
	"no running prox instance found in this directory (no .prox/prox.state); run from the project directory, or pass --addr",
)

// discoverAPIAddress attempts to discover the API address.
// Priority:
// 1. State file (.prox/prox.state) - for running instances
// 2. Config file (prox.yaml) - for a configured (non-dynamic) port
// It returns an error if neither source yields an address; callers must not
// fall back to constants.DefaultAPIAddress implicitly.
func discoverAPIAddress() (string, error) {
	// First, try to load from state file
	cwd, err := os.Getwd()
	if err == nil {
		state, err := daemon.LoadState(cwd)
		if err == nil {
			return fmt.Sprintf("http://%s:%d", state.Host, state.Port), nil
		}
	}

	// Fall back to config file
	if addr := loadAPIAddrFromConfig(); addr != "" {
		return addr, nil
	}

	return "", errNoRunningInstance
}

// getProcessNames returns process names from config for shell completion
func getProcessNames() []string {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(cfg.Processes))
	for name := range cfg.Processes {
		names = append(names, name)
	}
	return names
}
