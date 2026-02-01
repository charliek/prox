package cli

import (
	"fmt"
	"os"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
)

// Version is set during build
var Version = "dev"

// App represents the CLI application
type App struct {
	configPath           string
	apiAddr              string
	apiAddrExplicitlySet bool
	detach               bool
}

// NewApp creates a new CLI application
func NewApp() *App {
	return &App{
		configPath: constants.DefaultConfigFile,
		apiAddr:    constants.DefaultAPIAddress,
	}
}

// Run executes the CLI application
func (a *App) Run(args []string) int {
	if len(args) < 2 {
		a.printUsage()
		return 1
	}

	// Parse global flags
	remainingArgs := a.parseGlobalFlags(args[1:])
	if remainingArgs == nil {
		// Flag parsing error (already printed to stderr)
		return 1
	}

	if len(remainingArgs) == 0 {
		a.printUsage()
		return 1
	}

	cmd := remainingArgs[0]
	cmdArgs := remainingArgs[1:]

	// For client commands, try to discover API address
	switch cmd {
	case "status", "logs", "stop", "restart", "down", "attach":
		if !a.apiAddrExplicitlySet {
			a.apiAddr = a.discoverAPIAddress()
		}
	}

	switch cmd {
	case "up":
		return a.cmdUp(cmdArgs)
	case "status":
		return a.cmdStatus(cmdArgs)
	case "logs":
		return a.cmdLogs(cmdArgs)
	case "stop":
		return a.cmdStop(cmdArgs)
	case "down":
		return a.cmdDown(cmdArgs)
	case "attach":
		return a.cmdAttach(cmdArgs)
	case "restart":
		return a.cmdRestart(cmdArgs)
	case "certs":
		return a.cmdCerts(cmdArgs)
	case "hosts":
		return a.cmdHosts(cmdArgs)
	case "version":
		return a.cmdVersion(cmdArgs)
	case "help", "-h", "--help":
		a.printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		a.printUsage()
		return 1
	}
}

// parseGlobalFlags parses global flags and returns remaining args.
// Returns nil if there's a flag parsing error (error already printed to stderr).
func (a *App) parseGlobalFlags(args []string) []string {
	var remaining []string
	i := 0

	for i < len(args) {
		arg := args[i]

		if arg == "-c" || arg == "--config" {
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return nil
			}
			a.configPath = val
			i = newIdx + 1
			continue
		} else if arg == "--addr" {
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return nil
			}
			a.apiAddr = val
			a.apiAddrExplicitlySet = true
			i = newIdx + 1
			continue
		} else if arg == "-d" || arg == "--detach" {
			a.detach = true
			i++
			continue
		} else if arg == "-h" || arg == "--help" {
			remaining = append(remaining, "help")
			i++
			continue
		} else {
			remaining = append(remaining, arg)
		}
		i++
	}

	return remaining
}

// printUsage prints usage information
func (a *App) printUsage() {
	fmt.Print(`prox - a modern process manager

Usage: prox [options] <command> [arguments]

Commands:
  up [processes...]     Start processes (foreground by default)
  up -d [processes...]  Start processes in background (daemon mode)
  up --tui [processes...] Start processes with TUI
  attach               Attach TUI to running daemon
  status               Show process status
  logs [process]       Show recent logs
  stop                 Stop running instance (via API)
  down                 Alias for stop
  restart <process>    Restart a process (via API)
  certs                Manage HTTPS certificates
  hosts                Manage /etc/hosts entries
  version              Show version
  help                 Show this help

Global Options:
  -c, --config FILE    Config file (default: prox.yaml)
  --addr URL           API address for remote commands (auto-discovered from .prox/prox.state)
  -d, --detach         Run in background (daemon mode)

Up Options:
  --tui                Enable interactive TUI mode (mutually exclusive with --detach)
  --port PORT          Override API port (otherwise dynamic)
  --no-proxy           Disable HTTPS proxy even if configured

Logs Options:
  -f, --follow         Stream logs continuously
  -n, --lines N        Number of lines (default: 100)
  --process NAMES      Filter by process (comma-separated)
  --pattern PATTERN    Filter by pattern
  --regex              Treat pattern as regex
  --json               Output as JSON

Status Options:
  --json               Output as JSON

Certs Options:
  --regenerate         Force regenerate certificates

Hosts Options:
  --add                Add entries to /etc/hosts (requires sudo)
  --remove             Remove entries from /etc/hosts (requires sudo)
  --show               Show entries that would be added

Examples:
  prox up                     # Start all processes (foreground)
  prox up -d                  # Start in background (daemon mode)
  prox up --no-proxy          # Start without HTTPS proxy
  prox attach                 # Attach TUI to running daemon
  prox up --tui               # Start with TUI (foreground)
  prox up web api             # Start specific processes
  prox logs --process web -n 50  # Last 50 lines from web
  prox logs -f                # Stream all logs
  prox restart worker         # Restart worker process
  prox down                   # Stop the daemon
  prox certs                  # Show certificate status
  prox hosts --add            # Add proxy hosts to /etc/hosts
`)
}

// cmdVersion shows version
func (a *App) cmdVersion(args []string) int {
	fmt.Printf("prox version %s\n", Version)
	return 0
}

// loadAPIAddrFromConfig attempts to read the API address from the config file.
// Returns empty string if config doesn't exist or can't be read.
func (a *App) loadAPIAddrFromConfig() string {
	cfg, err := config.Load(a.configPath)
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
func (a *App) discoverAPIAddress() string {
	// First, try to load from state file
	cwd, err := os.Getwd()
	if err == nil {
		state, err := daemon.LoadState(cwd)
		if err == nil {
			return fmt.Sprintf("http://%s:%d", state.Host, state.Port)
		}
	}

	// Fall back to config file
	if addr := a.loadAPIAddrFromConfig(); addr != "" {
		return addr
	}

	// Fall back to default
	return constants.DefaultAPIAddress
}
