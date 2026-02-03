package cli

import (
	"fmt"
	"os"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/spf13/cobra"
)

// Version is set during build
var Version = "dev"

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
  - HTTPS reverse proxy with subdomain routing
  - Interactive TUI for monitoring
  - Background daemon mode`,
	Version:       Version,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Check if --addr was explicitly provided
		if cmd.Flags().Changed("addr") {
			apiAddrExplicitlySet = true
		}

		// For client commands, try to discover API address if not explicitly set
		clientCommands := map[string]bool{
			"status":  true,
			"logs":    true,
			"stop":    true,
			"restart": true,
			"down":    true,
			"attach":  true,
		}
		if clientCommands[cmd.Name()] && !apiAddrExplicitlySet {
			apiAddr = discoverAPIAddress()
		}
	},
}

// Execute runs the root command
func Execute() {
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
		fmt.Printf("prox version %s\n", Version)
	},
}

func init() {
	// Persistent flags available to all subcommands
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", constants.DefaultConfigFile, "Config file")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "addr", constants.DefaultAPIAddress, "API address for remote commands")
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
		port = constants.DefaultAPIPort
	}

	return fmt.Sprintf("http://%s:%d", host, port)
}

// discoverAPIAddress attempts to discover the API address.
// Priority:
// 1. State file (.prox/prox.state) - for running instances
// 2. Config file (prox.yaml) - for configured port
// 3. Default address
func discoverAPIAddress() string {
	// First, try to load from state file
	cwd, err := os.Getwd()
	if err == nil {
		state, err := daemon.LoadState(cwd)
		if err == nil {
			return fmt.Sprintf("http://%s:%d", state.Host, state.Port)
		}
	}

	// Fall back to config file
	if addr := loadAPIAddrFromConfig(); addr != "" {
		return addr
	}

	// Fall back to default
	return constants.DefaultAPIAddress
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
