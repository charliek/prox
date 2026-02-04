package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
	"github.com/charliek/prox/internal/tui"
	"github.com/spf13/cobra"
)

const (
	// shutdownTimeout is the maximum time to wait for graceful shutdown
	shutdownTimeout = 10 * time.Second
	// logFlushDelay is the time to wait for logs to be printed before closing
	logFlushDelay = 50 * time.Millisecond
)

// Up command flags
var (
	useTUI        bool
	noProxy       bool
	port          int
	enableCapture bool
)

// upCmd represents the up command
var upCmd = &cobra.Command{
	Use:   "up [processes...]",
	Short: "Start processes",
	Long: `Start all or specific processes from the configuration.

By default, processes run in the foreground with logs streaming to the terminal.
Use -d/--detach to run in background (daemon mode), or --tui for interactive mode.

Examples:
  prox up                     # Start all processes (foreground)
  prox up -d                  # Start in background (daemon mode)
  prox up --tui               # Start with interactive TUI
  prox up web api             # Start specific processes
  prox up --no-proxy          # Start without HTTPS proxy
  prox up --capture           # Enable request/response capture`,
	Args:              cobra.ArbitraryArgs,
	RunE:              runUp,
	ValidArgsFunction: completeProcessNames,
}

func init() {
	rootCmd.AddCommand(upCmd)

	upCmd.Flags().BoolVar(&useTUI, "tui", false, "Enable interactive TUI mode")
	upCmd.Flags().BoolVar(&noProxy, "no-proxy", false, "Disable HTTPS proxy even if configured")
	upCmd.Flags().IntVarP(&port, "port", "p", 0, "Override API port (otherwise dynamic)")
	upCmd.Flags().BoolVar(&enableCapture, "capture", false, "Enable request/response body capture")
}

// completeProcessNames provides shell completion for process names
func completeProcessNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	names := getProcessNames()
	return names, cobra.ShellCompDirectiveNoFileComp
}

func runUp(cmd *cobra.Command, args []string) error {
	processes := args

	// Validate mutually exclusive flags
	if useTUI && detach {
		return fmt.Errorf("--tui and --detach are mutually exclusive")
	}

	// Get working directory for state files
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// If daemon mode and we're the parent process, handle daemonization
	if detach && !daemon.IsDaemonChild() {
		if err := ensureNotAlreadyRunning(cwd); err != nil {
			return err
		}

		// Daemonize - this will re-exec and exit the parent
		if err := daemon.Daemonize(); err != nil {
			return fmt.Errorf("failed to daemonize: %w", err)
		}
		// Parent exits in Daemonize(), this is unreachable for parent
	}

	// If we're the daemon child, set up logging
	var logFile *os.File
	if daemon.IsDaemonChild() {
		logFile, err = daemon.SetupLogging(cwd)
		if err != nil {
			// Can't write to stderr in daemon mode, but try anyway
			return fmt.Errorf("failed to setup logging: %w", err)
		}
		defer logFile.Close()
	}

	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Determine port: CLI flag > config > dynamic
	if port > 0 {
		cfg.API.Port = port
	} else if cfg.API.Port == 0 {
		// Dynamic port allocation
		host := cfg.API.Host
		if host == "" {
			host = constants.DefaultAPIHost
		}
		dynamicPort, err := daemon.FindAvailablePort(host)
		if err != nil {
			return fmt.Errorf("failed to find available port: %w", err)
		}
		cfg.API.Port = dynamicPort
	}

	// Enable capture if --capture flag is set
	if enableCapture && cfg.Proxy != nil {
		if cfg.Proxy.Capture == nil {
			cfg.Proxy.Capture = &config.CaptureConfig{}
		}
		cfg.Proxy.Capture.Enabled = true
	}

	// For foreground mode, also check if already running and handle state
	if !detach {
		if err := ensureNotAlreadyRunning(cwd); err != nil {
			return err
		}
	}

	// Create state directory
	if err := daemon.EnsureStateDir(cwd); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Get host for state file
	host := cfg.API.Host
	if host == "" {
		host = constants.DefaultAPIHost
	}

	// Resolve config path to absolute for storage in state file
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		absConfigPath = configPath // Fall back to original if resolution fails
	}

	// Validate config file exists
	if _, err := os.Stat(absConfigPath); err != nil {
		return fmt.Errorf("config file not accessible: %w", err)
	}

	// Create and lock PID file FIRST (before state file) to prevent race conditions
	// Note: Defers execute LIFO, so we register cleanup FIRST, then PID release.
	// This ensures: PID release runs first, then cleanup runs (correct order).
	pidFile := daemon.NewPIDFile(daemon.PIDPath(cwd))
	if err := pidFile.Create(); err != nil {
		if err == daemon.ErrPIDFileLocked {
			return fmt.Errorf("prox is already running (PID file locked)")
		}
		return fmt.Errorf("failed to create PID file: %w", err)
	}

	// Write state file after PID file is locked
	state := &daemon.State{
		PID:        os.Getpid(),
		Port:       cfg.API.Port,
		Host:       host,
		StartedAt:  time.Now(),
		ConfigFile: absConfigPath,
	}
	if err := state.Write(cwd); err != nil {
		// Clean up PID file on state file failure
		_ = pidFile.Release()
		return fmt.Errorf("failed to write state file: %w", err)
	}

	// Register cleanup defer FIRST (will run LAST due to LIFO)
	defer func() {
		_ = daemon.CleanupStateDir(cwd)
	}()

	// Register PID release defer SECOND (will run FIRST due to LIFO)
	defer func() {
		_ = pidFile.Release()
	}()

	// Create log manager
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         1000,
		SubscriptionBuffer: 1000,
	})

	// Get config directory for resolving relative paths in env files
	configDir := filepath.Dir(configPath)
	if configDir == "." {
		// Try to get absolute path
		if absPath, err := filepath.Abs(configPath); err == nil {
			configDir = filepath.Dir(absPath)
		}
	}

	// Create supervisor
	supConfig := supervisor.DefaultSupervisorConfig()
	supConfig.ConfigDir = configDir
	sup := supervisor.New(cfg, logMgr, nil, supConfig)

	// Create shutdown channel
	shutdownCh := make(chan struct{})
	shutdownFn := func() {
		close(shutdownCh)
	}

	// Determine if authentication is required
	authEnabled := isAuthRequired(cfg)
	var token string

	// Generate authentication token only if auth is enabled
	if authEnabled {
		token, err = generateToken()
		if err != nil {
			return fmt.Errorf("failed to generate auth token: %w", err)
		}
		if err := saveToken(token); err != nil {
			return fmt.Errorf("failed to save auth token: %w", err)
		}
	} else if !isLocalhost(cfg.API.Host) && cfg.API.Auth != nil && !*cfg.API.Auth {
		// Warning: auth explicitly disabled on non-localhost
		fmt.Fprintf(os.Stderr, "WARNING: Auth disabled while binding to all interfaces (%s)\n", cfg.API.Host)
		fmt.Fprintf(os.Stderr, "         Any network client can control this supervisor.\n")
	}

	// Create API handlers and server
	handlers := api.NewHandlers(sup, logMgr, configPath, shutdownFn)
	apiServer := api.NewServer(api.ServerConfig{
		Host:        cfg.API.Host,
		Port:        cfg.API.Port,
		AuthEnabled: authEnabled,
		Token:       token,
	}, handlers)

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start supervisor
	fmt.Printf("Starting prox with config: %s\n", configPath)
	if isLocalhost(cfg.API.Host) {
		if authEnabled {
			fmt.Printf("API server: http://%s (local only, auth enabled)\n", apiServer.Addr())
		} else {
			fmt.Printf("API server: http://%s (local only, no auth)\n", apiServer.Addr())
		}
	} else {
		if authEnabled {
			fmt.Printf("API server: http://%s (network accessible, auth enabled)\n", apiServer.Addr())
		} else {
			fmt.Printf("API server: http://%s (network accessible, no auth)\n", apiServer.Addr())
		}
	}
	if authEnabled {
		fmt.Printf("Auth token saved to: %s\n", tokenPath())
	}

	if len(processes) > 0 {
		fmt.Printf("Starting processes: %s\n", strings.Join(processes, ", "))
		result, err := sup.StartProcesses(ctx, processes)
		if err != nil {
			return fmt.Errorf("failed to start processes: %w", err)
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Warning: failed to start process %s: %v\n", name, procErr)
			}
		}
	} else {
		result, err := sup.Start(ctx)
		if err != nil {
			return fmt.Errorf("failed to start supervisor: %w", err)
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Warning: failed to start process %s: %v\n", name, procErr)
			}
		}
	}

	// Start API server in background
	go func() {
		if err := apiServer.Start(); err != nil {
			// Server closed is expected on shutdown
			if !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "API server error: %v\n", err)
			}
		}
	}()

	// Start proxy server if configured and not disabled
	var proxyService *proxy.Service
	if !noProxy && cfg.Proxy != nil && cfg.Proxy.Enabled {
		level := slog.LevelInfo
		if verbose {
			level = slog.LevelDebug
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
		var err error
		proxyService, err = proxy.NewService(cfg.Proxy, cfg.Services, cfg.Certs, logger, cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating proxy service: %v\n", err)
			// Continue without proxy - this is not fatal
		} else if err := proxyService.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting proxy: %v\n", err)
			proxyService = nil
			// Continue without proxy - this is not fatal
		} else {
			fmt.Printf("Proxy server: https://*.%s:%d\n", cfg.Proxy.Domain, cfg.Proxy.HTTPSPort)
			// Wire up request manager and capture manager to API handlers
			handlers.SetRequestManager(proxyService.RequestManager())
			handlers.SetCaptureManager(proxyService.CaptureManager())
		}
	}

	// Handle TUI vs terminal output
	if useTUI {
		// Run TUI - it blocks until quit
		var reqMgr *proxy.RequestManager
		if proxyService != nil {
			reqMgr = proxyService.RequestManager()
		}
		if err := tui.Run(sup, logMgr, reqMgr); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	} else {
		// Subscribe to logs and print to terminal
		go printLogs(logMgr)

		// Wait for shutdown signal
		select {
		case sig := <-sigCh:
			fmt.Println() // Print newline after ^C
			sup.SystemLog("%s received", sig)
		case <-shutdownCh:
			fmt.Println() // Print newline
			sup.SystemLog("shutdown requested via API")
		}
	}

	// Stop signal handler to prevent additional signals during shutdown
	signal.Stop(sigCh)

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// Stop proxy server
	if proxyService != nil {
		if err := proxyService.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "Error stopping proxy: %v\n", err)
		}
	}

	// Stop API server
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}

	// Stop supervisor
	if err := sup.Stop(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}

	// Log shutdown complete before closing the log manager
	sup.SystemLog("shutdown complete")

	// Give a moment for the log to be printed
	time.Sleep(logFlushDelay)

	// Close log manager
	logMgr.Close()
	return nil
}

// proxDir returns the prox config directory path (~/.prox)
func proxDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".prox"
	}
	return filepath.Join(home, ".prox")
}

// tokenPath returns the path to the token file
func tokenPath() string {
	return filepath.Join(proxDir(), "token")
}

// generateToken generates a cryptographically secure random token
func generateToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// saveToken saves the token to ~/.prox/token
func saveToken(token string) error {
	dir := proxDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating prox directory: %w", err)
	}
	// Write token with restrictive permissions (owner read/write only)
	if err := os.WriteFile(tokenPath(), []byte(token), 0600); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	return nil
}

// loadToken loads the token from ~/.prox/token
func loadToken() (string, error) {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// isLocalhost checks if the host is a localhost address
func isLocalhost(host string) bool {
	return host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// isAuthRequired determines if authentication should be enabled based on config
func isAuthRequired(cfg *config.Config) bool {
	// Explicit config takes precedence
	if cfg.API.Auth != nil {
		return *cfg.API.Auth
	}
	// Auto-determine: auth required unless binding to localhost only
	return !isLocalhost(cfg.API.Host)
}

// ensureNotAlreadyRunning checks if prox is already running and cleans up stale files.
// Returns nil if the caller can proceed, or an error describing the problem.
func ensureNotAlreadyRunning(cwd string) error {
	if daemon.IsRunning(cwd) {
		return fmt.Errorf("prox is already running\nUse 'prox status' to check or 'prox stop' to stop it")
	}

	// Clean up any stale files from previous crashed instance
	if err := daemon.CleanupStaleFiles(cwd); err != nil && err != daemon.ErrAlreadyRunning {
		return fmt.Errorf("error cleaning up stale files: %w", err)
	}

	return nil
}

// printLogs subscribes to logs and prints them to terminal
func printLogs(logMgr *logs.Manager) {
	_, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		return
	}

	printer := NewLogPrinter()
	for entry := range ch {
		printer.PrintEntry(entry)
	}
}
