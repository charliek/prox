package cli

import (
	"fmt"
	"os"

	"github.com/charliek/prox/internal/constants"
)

// Version is set during build
var Version = "dev"

// App represents the CLI application
type App struct {
	configPath string
	apiAddr    string
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

	if len(remainingArgs) == 0 {
		a.printUsage()
		return 1
	}

	cmd := remainingArgs[0]
	cmdArgs := remainingArgs[1:]

	switch cmd {
	case "up":
		return a.cmdUp(cmdArgs)
	case "status":
		return a.cmdStatus(cmdArgs)
	case "logs":
		return a.cmdLogs(cmdArgs)
	case "stop":
		return a.cmdStop(cmdArgs)
	case "restart":
		return a.cmdRestart(cmdArgs)
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

// parseGlobalFlags parses global flags and returns remaining args
func (a *App) parseGlobalFlags(args []string) []string {
	var remaining []string
	i := 0

	for i < len(args) {
		arg := args[i]

		if arg == "-c" || arg == "--config" {
			if i+1 < len(args) {
				a.configPath = args[i+1]
				i += 2
				continue
			}
		} else if arg == "--addr" {
			if i+1 < len(args) {
				a.apiAddr = args[i+1]
				i += 2
				continue
			}
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
  up [processes...]     Start processes (foreground)
  up --tui [processes...] Start processes with TUI
  status               Show process status
  logs [process]       Show recent logs
  stop                 Stop running instance (via API)
  restart <process>    Restart a process (via API)
  version              Show version
  help                 Show this help

Global Options:
  -c, --config FILE    Config file (default: prox.yaml)
  --addr URL           API address for remote commands (default: http://127.0.0.1:5555)

Up Options:
  --tui                Enable interactive TUI mode
  --port PORT          Override API port

Logs Options:
  -f, --follow         Stream logs continuously
  -n, --lines N        Number of lines (default: 100)
  --process NAMES      Filter by process (comma-separated)
  --pattern PATTERN    Filter by pattern
  --regex              Treat pattern as regex
  --json               Output as JSON

Status Options:
  --json               Output as JSON

Examples:
  prox up                     # Start all processes
  prox up web api             # Start specific processes
  prox up --tui               # Start with TUI
  prox logs --process web -n 50  # Last 50 lines from web
  prox logs -f                # Stream all logs
  prox restart worker         # Restart worker process
`)
}

// cmdVersion shows version
func (a *App) cmdVersion(args []string) int {
	fmt.Printf("prox version %s\n", Version)
	return 0
}
