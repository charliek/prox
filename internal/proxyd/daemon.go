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

// VersionMismatchError is returned by EnsureRunning when a running daemon — or a
// freshly started one — reports a version different from this process. It is
// matched with errors.As so the caller (tryDaemonProxy) can drive version-skew
// recovery (D1) rather than silently falling back to a standalone proxy, which
// used to leave a project running with the proxy disabled.
type VersionMismatchError struct {
	DaemonVersion string
	ClientVersion string
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf(
		"proxy daemon is running version %s, but this process is version %s",
		e.DaemonVersion, e.ClientVersion,
	)
}

// ErrDaemonNotReady is returned by EnsureRunning when a freshly started daemon
// did not answer /health within the startup window. During a version-skew heal
// this also covers the socket-removed-before-PID-lock-released race: the old
// daemon removes its socket (server.Shutdown) before its deferred PID-lock
// release (RunDaemon), so a replacement start can briefly lose the lock and
// report not-ready. The skew recovery retries once after a short delay on this.
// Matched with errors.Is.
var ErrDaemonNotReady = errors.New("proxy daemon did not become ready")

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
			return nil, &VersionMismatchError{DaemonVersion: daemonVersion, ClientVersion: version.Version}
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
				return nil, &VersionMismatchError{DaemonVersion: daemonVersion, ClientVersion: version.Version}
			}
			return client, nil
		}
		time.Sleep(startupRetryInterval)
	}

	return nil, fmt.Errorf("%w within %s", ErrDaemonNotReady, startupTimeout)
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
	_ = cmd.Process.Release()
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
	defer func() { _ = pidFile.Release() }()

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

	// Create the daemon's capture manager, rooted at ~/.prox/capture. Resolve
	// the home directory explicitly; on failure log and run without capture
	// (nil manager, capture disabled) rather than failing the daemon.
	var captureMgr *proxy.CaptureManager
	if homeDir, herr := os.UserHomeDir(); herr != nil {
		logger.Warn("could not resolve home directory; running without request capture", "error", herr)
	} else if cm, cerr := proxy.NewCaptureManagerAt(constants.DaemonCaptureDir(homeDir), constants.DefaultCaptureMaxBodySize); cerr != nil {
		logger.Warn("could not initialize capture manager; running without request capture", "error", cerr)
	} else {
		captureMgr = cm
	}
	// Cleanup runs on return. Because dynamicProxy.Shutdown and server.Shutdown
	// run inline in the function body below, this deferred Cleanup necessarily
	// runs AFTER both — in-flight handlers finish writing their capture files
	// first. Registered immediately after construction so an early startup
	// error still removes the capture dir. Cleanup is RemoveAll, so it is
	// idempotent and safe to leave deferred here.
	var evict proxy.EvictionCallback
	if captureMgr != nil {
		defer func() { _ = captureMgr.Cleanup() }()
		// Evicting a record from a project ring must delete its on-disk body
		// files; the capture manager's per-request cleanup does exactly that.
		// The set wires this onto every per-project ring at creation.
		evict = captureMgr.CleanupRequest
	}

	// Per-project request rings (D13, #49): one full-capacity ring per registered
	// project, created at register time, so one project's flood cannot evict
	// another's records. Shared by the dynamic proxy (hot path) and the socket
	// server (control-plane endpoints).
	managers := NewManagers(constants.DefaultProxyRequestBufferSize, evict)

	dynamicProxy := NewDynamicProxy(registry, certMgr, managers, captureMgr, logger)

	// Create and configure the socket server
	server := NewServer(ServerConfig{
		SocketPath: SocketPath(),
		Logger:     logger,
		Version:    version.Version,
	})
	server.SetRegistry(registry)
	server.SetProxy(dynamicProxy)
	server.SetManagers(managers)
	// Wire the shared capture manager so the server can push the registry's
	// effective capture disk budget onto it after every committed registry
	// mutation (#69). nil when capture is disabled — SetCaptureManager and
	// syncCaptureBudget both tolerate that.
	server.SetCaptureManager(captureMgr)

	// Wire the on-502 dead-owner probe's reap callback (#74). When a route's
	// backend transport fails, the dynamic proxy probes the owning prox up
	// process's liveness off the data plane and, if it is dead, calls this
	// closure. It MIRRORS the stale-PID sweep's epilogue below exactly: reap via
	// the identity-guarded removeStaleProject, and on a real removal log the
	// cleaned-registration line and schedule the empty-daemon shutdown check —
	// without the shutdown scheduling, probe-reaping the last project would
	// strand an idle daemon forever (the sweep can never fire again on an empty
	// registry). The 30s sweep remains the backstop for everything the probe
	// can't see (backend-authored 502s, mid-stream aborts, trafficless deaths).
	dynamicProxy.SetDeadRouteRemover(func(dir string, pid int, startTime int64) {
		removed, hostnames, emptyPorts := server.removeStaleProject(dir, pid, startTime)
		if !removed {
			// Re-registered with a live generation between the 502 and the
			// probe (DeregisterIfIdentity guard) — leave it alone.
			return
		}
		logger.Warn("cleaned stale project registration",
			"project", dir,
			"pid", pid,
			"start_time", startTime,
			"removed_hostnames", hostnames,
			"closed_ports", emptyPorts,
			"trigger", "on-502-probe",
		)
		if registry.IsEmpty() {
			// Graced (not immediate) so a crash restart landing during the probe
			// cancels the shutdown when the re-check sees its registration.
			logger.Info("all routes cleaned up, scheduling shutdown check")
			server.scheduleShutdownWhenEmpty()
		}
	})

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
				stale := registry.StalePIDs()
				for _, sp := range stale {
					removed, hostnames, emptyPorts := server.removeStaleProject(sp.Dir, sp.PID, sp.StartTime)
					if !removed {
						// Re-registered with a live PID between detection and
						// removal — leave the new registration alone.
						continue
					}
					logger.Warn("cleaned stale project registration",
						"project", sp.Dir,
						"pid", sp.PID,
						"start_time", sp.StartTime,
						"removed_hostnames", hostnames,
						"closed_ports", emptyPorts,
					)
				}
				if len(stale) > 0 && registry.IsEmpty() {
					// Graced (not immediate) so a crash restart landing during
					// this sweep — its self-heal replace completing just after —
					// cancels the shutdown when the re-check sees its registration.
					logger.Info("all routes cleaned up, scheduling shutdown check")
					server.scheduleShutdownWhenEmpty()
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

	// Wait for shutdown signal. Every branch falls THROUGH to the unconditional
	// teardown below; a socket-server error records runErr and still tears down
	// (an early return here used to skip dynamicProxy.Shutdown/requestMgr.Close/
	// server.Shutdown entirely, leaking listeners and body files).
	var runErr error
	select {
	case sig := <-sigCh:
		logger.Info("received signal", "signal", sig)
	case <-server.ShutdownCh():
		logger.Info("shutdown requested")
	case err := <-serverErr:
		if err != nil {
			logger.Error("server error", "error", err)
			runErr = err
		}
	}

	// Graceful shutdown. quiesceForTeardown sets the shutdown flag and takes
	// lifecycleMu as a barrier, so teardown is serialized against any in-flight
	// register/deregister/stale-removal: the one transaction already running when
	// the flag was set completes atomically, and every later one self-gates to a
	// no-op before we start closing listeners and records.
	server.quiesceForTeardown()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	cancel() // stop cleanup loop

	if err := dynamicProxy.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down proxy", "error", err)
	}
	// Close every project ring before the socket server so active SSE
	// subscribers observe end-of-stream and release the server, rather than
	// pinning it open through the shutdown grace period.
	managers.closeAll()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down server", "error", err)
	}

	logger.Info("proxy daemon stopped")
	return runErr
}
