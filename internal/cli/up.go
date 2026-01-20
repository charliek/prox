package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
	"github.com/charliek/prox/internal/tui"
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

// cmdUp handles the 'up' command
func (a *App) cmdUp(args []string) int {
	// Parse flags
	useTUI := false
	port := 0
	var processes []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--tui" {
			useTUI = true
		} else if arg == "--port" || arg == "-p" {
			if i+1 < len(args) {
				if p, err := strconv.Atoi(args[i+1]); err == nil && p > 0 && p <= 65535 {
					port = p
				} else {
					fmt.Fprintf(os.Stderr, "Invalid port: %s (must be 1-65535)\n", args[i+1])
					return 1
				}
				i++
			}
		} else if !strings.HasPrefix(arg, "-") {
			processes = append(processes, arg)
		}
	}

	// Load config
	cfg, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	// Override port if specified
	if port > 0 {
		cfg.API.Port = port
	}

	// Create log manager
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         1000,
		SubscriptionBuffer: 100,
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
			fmt.Fprintf(os.Stderr, "Error generating auth token: %v\n", err)
			return 1
		}
		if err := saveToken(token); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving auth token: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "Error starting supervisor: %v\n", err)
			return 1
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Failed to start process %s: %v\n", name, procErr)
			}
		}
	} else {
		result, err := sup.Start(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error starting supervisor: %v\n", err)
			return 1
		}
		if result.HasFailures() {
			for name, procErr := range result.Failed {
				fmt.Fprintf(os.Stderr, "Failed to start process %s: %v\n", name, procErr)
			}
		}
	}

	// Start API server in background
	go func() {
		if err := apiServer.Start(); err != nil {
			// Server closed is expected on shutdown
			if !strings.Contains(err.Error(), "Server closed") {
				fmt.Fprintf(os.Stderr, "API server error: %v\n", err)
			}
		}
	}()

	// Handle TUI vs terminal output
	if useTUI {
		// Run TUI - it blocks until quit
		if err := tui.Run(sup, logMgr); err != nil {
			fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		}
	} else {
		// Subscribe to logs and print to terminal
		go a.printLogs(logMgr)

		// Wait for shutdown signal
		select {
		case sig := <-sigCh:
			fmt.Printf("\nReceived %s, shutting down...\n", sig)
		case <-shutdownCh:
			fmt.Println("\nShutdown requested via API...")
		}
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Stop API server
	apiServer.Shutdown(shutdownCtx)

	// Stop supervisor
	if err := sup.Stop(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "Error stopping supervisor: %v\n", err)
	}

	// Close log manager
	logMgr.Close()

	fmt.Println("Shutdown complete")
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
		// Check if stderr (show in red)
		lineColor := ""
		if entry.Stream == domain.StreamStderr {
			lineColor = constants.ColorBrightRed
		}

		fmt.Printf("%s %s%-8s%s │ %s%s%s\n",
			ts,
			color, entry.Process, constants.ColorReset,
			lineColor, entry.Line, constants.ColorReset)
	}
}
