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
	"strconv"
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
)

const (
	// shutdownTimeout is the maximum time to wait for graceful shutdown
	shutdownTimeout = 10 * time.Second
	// logFlushDelay is the time to wait for logs to be printed before closing
	logFlushDelay = 50 * time.Millisecond
)

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

// cmdUp handles the 'up' command
func (a *App) cmdUp(args []string) int {
	// Parse flags
	useTUI := false
	noProxy := false
	port := 0
	var processes []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--tui" {
			useTUI = true
		} else if arg == "--no-proxy" {
			noProxy = true
		} else if arg == "--port" || arg == "-p" {
			val, newIdx, err := parseFlagValue(args, i, arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			p, err := strconv.Atoi(val)
			if err != nil || p < 1 || p > 65535 {
				fmt.Fprintf(os.Stderr, "Error: invalid port %q (must be 1-65535)\n", val)
				return 1
			}
			port = p
			i = newIdx
		} else if arg == "-d" || arg == "--detach" {
			a.detach = true
		} else if !strings.HasPrefix(arg, "-") {
			processes = append(processes, arg)
		}
	}

	// Validate mutually exclusive flags
	if useTUI && a.detach {
		fmt.Fprintf(os.Stderr, "Error: --tui and --detach are mutually exclusive\n")
		return 1
	}

	// Get working directory for state files
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// If daemon mode and we're the parent process, handle daemonization
	if a.detach && !daemon.IsDaemonChild() {
		if err := ensureNotAlreadyRunning(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Daemonize - this will re-exec and exit the parent
		if err := daemon.Daemonize(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		// Parent exits in Daemonize(), this is unreachable for parent
	}

	// If we're the daemon child, set up logging
	var logFile *os.File
	if daemon.IsDaemonChild() {
		var err error
		logFile, err = daemon.SetupLogging(cwd)
		if err != nil {
			// Can't write to stderr in daemon mode, but try anyway
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		defer logFile.Close()
	}

	// Load config
	cfg, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		cfg.API.Port = dynamicPort
	}

	// For foreground mode, also check if already running and handle state
	if !a.detach {
		if err := ensureNotAlreadyRunning(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	}

	// Create state directory
	if err := daemon.EnsureStateDir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Get host for state file
	host := cfg.API.Host
	if host == "" {
		host = constants.DefaultAPIHost
	}

	// Resolve config path to absolute for storage in state file
	absConfigPath, err := filepath.Abs(a.configPath)
	if err != nil {
		absConfigPath = a.configPath // Fall back to original if resolution fails
	}

	// Validate config file exists
	if _, err := os.Stat(absConfigPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: config file not accessible: %v\n", err)
		return 1
	}

	// Create and lock PID file FIRST (before state file) to prevent race conditions
	// Note: Defers execute LIFO, so we register cleanup FIRST, then PID release.
	// This ensures: PID release runs first, then cleanup runs (correct order).
	pidFile := daemon.NewPIDFile(daemon.PIDPath(cwd))
	if err := pidFile.Create(); err != nil {
		if err == daemon.ErrPIDFileLocked {
			fmt.Fprintf(os.Stderr, "Error: prox is already running (PID file locked)\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
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
	configDir := filepath.Dir(a.configPath)
	if configDir == "." {
		// Try to get absolute path
		if absPath, err := filepath.Abs(a.configPath); err == nil {
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
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if err := saveToken(token); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
	} else if !isLocalhost(cfg.API.Host) && cfg.API.Auth != nil && !*cfg.API.Auth {
		// Warning: auth explicitly disabled on non-localhost
		fmt.Fprintf(os.Stderr, "WARNING: Auth disabled while binding to all interfaces (%s)\n", cfg.API.Host)
		fmt.Fprintf(os.Stderr, "         Any network client can control this supervisor.\n")
	}

	// Create API handlers and server
	handlers := api.NewHandlers(sup, logMgr, a.configPath, shutdownFn)
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
	fmt.Printf("Starting prox with config: %s\n", a.configPath)
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
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Error: failed to start process %s: %v\n", name, procErr)
			}
		}
	} else {
		result, err := sup.Start(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Error: failed to start process %s: %v\n", name, procErr)
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
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		var err error
		proxyService, err = proxy.NewService(cfg.Proxy, cfg.Services, cfg.Certs, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating proxy service: %v\n", err)
			// Continue without proxy - this is not fatal
		} else if err := proxyService.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting proxy: %v\n", err)
			proxyService = nil
			// Continue without proxy - this is not fatal
		} else {
			fmt.Printf("Proxy server: https://*.%s:%d\n", cfg.Proxy.Domain, cfg.Proxy.HTTPSPort)
			// Wire up request manager to API handlers for request inspection
			handlers.SetRequestManager(proxyService.RequestManager())
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
		go a.printLogs(logMgr)

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

	return 0
}

// printLogs subscribes to logs and prints them to terminal
func (a *App) printLogs(logMgr *logs.Manager) {
	_, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		return
	}

	processColors := make(map[string]string)
	colorIndex := 0

	for entry := range ch {
		// Assign color to process
		color, ok := processColors[entry.Process]
		if !ok {
			color = constants.ProcessColors[colorIndex%len(constants.ProcessColors)]
			processColors[entry.Process] = color
			colorIndex++
		}

		// Format timestamp
		ts := entry.Timestamp.Format("15:04:05")

		// Format output
		fmt.Printf("%s %s%-8s%s │ %s\n",
			ts,
			color, entry.Process, constants.ColorReset,
			entry.Line)
	}
}
