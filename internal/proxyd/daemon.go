package proxyd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/version"
)

const (
	// ProxyDaemonEnvVar marks the process as the proxy daemon child.
	ProxyDaemonEnvVar = "_PROX_PROXY_DAEMON"

	// startupRetryInterval is the time between daemon health check retries.
	startupRetryInterval = 100 * time.Millisecond

	// startupTimeout is the maximum time to wait for the daemon to start.
	startupTimeout = 5 * time.Second

	// stalePIDCheckInterval is how often the daemon checks for dead project PIDs.
	stalePIDCheckInterval = 30 * time.Second
)

// IsDaemonProcess returns true if this process is the proxy daemon child.
func IsDaemonProcess() bool {
	return os.Getenv(ProxyDaemonEnvVar) == "1"
}

// EnsureRunning ensures the proxy daemon is running and returns a connected client.
// If the daemon is not running, it starts one. Verifies version compatibility.
func EnsureRunning() (*Client, error) {
	socketPath := SocketPath()
	client := NewClient(socketPath)

	// Try to connect to an existing daemon
	daemonVersion, err := client.Health()
	if err == nil {
		// Daemon is running — check version
		if daemonVersion != version.Version {
			return nil, fmt.Errorf(
				"proxy daemon is running version %s, but this process is version %s.\n"+
					"Stop all projects and restart, or run 'prox proxy stop --force' to reset.",
				daemonVersion, version.Version,
			)
		}
		return client, nil
	}

	// Daemon is not running — start it
	if err := startDaemon(); err != nil {
		return nil, fmt.Errorf("starting proxy daemon: %w", err)
	}

	// Wait for daemon to become ready
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		daemonVersion, err = client.Health()
		if err == nil {
			if daemonVersion != version.Version {
				return nil, fmt.Errorf("started daemon has version %s, expected %s", daemonVersion, version.Version)
			}
			return client, nil
		}
		time.Sleep(startupRetryInterval)
	}

	return nil, fmt.Errorf("proxy daemon did not become ready within %s", startupTimeout)
}

// startDaemon forks the proxy daemon as a background process.
func startDaemon() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting executable path: %w", err)
	}

	// Re-exec with the daemon env var. The daemon will be intercepted in
	// Execute() and run RunDaemon() instead of normal CLI dispatch.
	// We pass a minimal set of args — just the binary name.
	cmd := exec.Command(executable, "up", "--no-proxy")
	cmd.Env = append(os.Environ(), ProxyDaemonEnvVar+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting daemon process: %w", err)
	}

	// Detach — don't wait for the child
	cmd.Process.Release()
	return nil
}

// RunDaemon is the main entry point for the proxy daemon process.
// It sets up logging, the registry, proxy, and socket server, then waits
// for a shutdown signal (no routes remaining or SIGTERM).
func RunDaemon(ctx context.Context) error {
	if err := EnsureDaemonDir(); err != nil {
		return err
	}

	// Set up logging to file
	logPath := DaemonLogPath()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, constants.FilePermissionPrivate)
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}
	defer logFile.Close()

	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("proxy daemon starting", "version", version.Version, "pid", os.Getpid())

	// Create and lock PID file
	pidFile := daemon.NewPIDFile(DaemonPIDPath())
	if err := pidFile.Create(); err != nil {
		if err == daemon.ErrPIDFileLocked {
			logger.Info("another daemon is already running")
			return fmt.Errorf("proxy daemon is already running")
		}
		return fmt.Errorf("creating daemon PID file: %w", err)
	}
	defer pidFile.Release()

	// Write state file
	state := &DaemonState{
		PID:       os.Getpid(),
		Version:   version.Version,
		StartedAt: time.Now(),
	}
	if err := WriteDaemonState(state); err != nil {
		return fmt.Errorf("writing daemon state: %w", err)
	}
	defer CleanupDaemonState()

	// Create core components
	registry := NewRegistry()
	certMgr := NewMultiDomainCertManager(constants.DefaultCertsDir)
	requestMgr := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)
	dynamicProxy := NewDynamicProxy(registry, certMgr, requestMgr, logger)

	// Create and configure the socket server
	server := NewServer(ServerConfig{
		SocketPath: SocketPath(),
		Logger:     logger,
		Version:    version.Version,
	})
	server.SetRegistry(registry)
	server.SetProxy(dynamicProxy)
	server.SetRequestManager(requestMgr)

	// Handle OS signals
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start stale PID cleanup loop
	go func() {
		ticker := time.NewTicker(stalePIDCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stale := registry.CleanStalePIDs()
				for _, dir := range stale {
					logger.Warn("cleaned stale project registration", "project", dir)
				}
				if len(stale) > 0 && registry.IsEmpty() {
					logger.Info("all routes cleaned up, shutting down")
					server.RequestShutdown()
				}
			}
		}
	}()

	// Start socket server in background
	serverErr := make(chan error, 1)
	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// Wait for shutdown signal
	select {
	case sig := <-sigCh:
		logger.Info("received signal", "signal", sig)
	case <-server.ShutdownCh():
		logger.Info("shutdown requested")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server error", "error", err)
			return err
		}
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	cancel() // stop cleanup loop

	if err := dynamicProxy.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down proxy", "error", err)
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down server", "error", err)
	}

	logger.Info("proxy daemon stopped")
	return nil
}
