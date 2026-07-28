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
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/supervisor"
	"github.com/charliek/prox/internal/tui"
	"github.com/charliek/prox/internal/version"
	"github.com/spf13/cobra"
)

const (
	// teardownStageTimeout bounds each fixed daemon-shutdown teardown stage
	// (proxy deregister/shutdown, API-server shutdown) independently. The
	// supervisor stage is bounded separately by MaxStopBudget (see the shutdown
	// path below) so a large configured stop budget is never truncated by proxy
	// or API teardown time (#35, D2).
	teardownStageTimeout = 5 * time.Second
	// logFlushDelay is the time to wait for logs to be printed before closing
	logFlushDelay = 50 * time.Millisecond
)

// Up command flags
var (
	useTUI        bool
	noProxy       bool
	apiPort       int
	httpPort      int
	httpsPort     int
	enableCapture bool
	noCapture     bool
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
  prox up --no-proxy          # Start without proxy
  prox up --no-capture        # Disable request/response capture for this run`,
	Args:              cobra.ArbitraryArgs,
	RunE:              runUp,
	ValidArgsFunction: completeProcessNames,
}

func init() {
	rootCmd.AddCommand(upCmd)

	upCmd.Flags().BoolVar(&useTUI, "tui", false, "Enable interactive TUI mode")
	upCmd.Flags().BoolVar(&noProxy, "no-proxy", false, "Disable proxy even if configured")
	upCmd.Flags().IntVarP(&apiPort, "api-port", "p", 0, "Override API server port (otherwise dynamic)")
	upCmd.Flags().IntVar(&httpPort, "http-port", 0, "Override proxy HTTP port")
	upCmd.Flags().IntVar(&httpsPort, "https-port", 0, "Override proxy HTTPS port")
	upCmd.Flags().BoolVar(&enableCapture, "capture", false, "Force request/response body capture on (default: on when the proxy is enabled; kept for explicitness/compat)")
	upCmd.Flags().BoolVar(&noCapture, "no-capture", false, "Disable request/response body capture for this run")
}

// completeProcessNames provides shell completion for process names
func completeProcessNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	names := getProcessNames()
	return names, cobra.ShellCompDirectiveNoFileComp
}

func runUp(cmd *cobra.Command, args []string) (err error) {
	processes := args

	// Validate mutually exclusive flags
	if useTUI && detach {
		return fmt.Errorf("--tui and --detach are mutually exclusive")
	}
	// captureOverrideSet/captureOverrideEnabled resolve once here (before cfg is
	// even loaded) so the mutual-exclusivity error surfaces immediately, mirroring
	// the --tui/--detach check above; applied to cfg.Proxy.Capture.Enabled below
	// once cfg is loaded.
	captureOverrideEnabled, captureOverrideSet, err := resolveCaptureFlag(enableCapture, noCapture)
	if err != nil {
		return err
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

		// Re-exec ourselves detached; the parent no longer blindly exits 0.
		child, err := daemon.Daemonize()
		if err != nil {
			return fmt.Errorf("failed to daemonize: %w", err)
		}
		// The parent owns wait-and-report (D2): it never runs the supervisor
		// itself. It returns here — nil (exit 0) once the child is confirmed
		// ready, or an error (exit 1) on early death / never-ready timeout.
		return awaitDaemonStartup(&execChild{cmd: child}, cwd, defaultDaemonStartupOps())
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
		// Flush a fatal startup error INTO the log before the Close above
		// runs (defers are LIFO): Execute() prints the returned error to
		// os.Stderr only after runUp's defers have closed the redirected
		// log file, so without this the child dies with its reason lost —
		// the parent's log tail (D2) would show only stale content.
		defer func() {
			if err != nil {
				fmt.Fprintf(logFile, "Error: %v\n", err)
			}
		}()
	}

	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate port flags
	if apiPort < 0 {
		return fmt.Errorf("--api-port cannot be negative, got %d", apiPort)
	}
	if httpPort < 0 {
		return fmt.Errorf("--http-port cannot be negative, got %d", httpPort)
	}
	if httpsPort < 0 {
		return fmt.Errorf("--https-port cannot be negative, got %d", httpsPort)
	}

	// Determine API port: CLI flag > config > dynamic
	if apiPort > 0 {
		cfg.API.Port = apiPort
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

	// Override proxy ports if CLI flags are set
	if cfg.Proxy != nil {
		if httpPort > 0 {
			cfg.Proxy.HTTPPort = httpPort
		}
		if httpsPort > 0 {
			cfg.Proxy.HTTPSPort = httpsPort
		}

		// Auto-enable proxy if CLI port flags provided
		if !cfg.Proxy.Enabled && (httpPort > 0 || httpsPort > 0) {
			cfg.Proxy.Enabled = true
		}

		// Apply cert defaults for HTTPS if CLI overrides enabled it.
		if cfg.Proxy.Enabled && cfg.Proxy.HTTPSPort > 0 && cfg.Certs == nil {
			cfg.Certs = &config.CertsConfig{AutoGenerate: true}
		}
	} else if httpPort > 0 || httpsPort > 0 {
		fmt.Fprintf(os.Stderr, "Warning: --http-port/--https-port flags ignored (no proxy section in config)\n")
	}
	if cfg.Certs != nil && cfg.Certs.Dir == "" {
		cfg.Certs.Dir = constants.DefaultCertsDir
	}

	// Re-validate after applying CLI overrides.
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("invalid runtime configuration after CLI overrides: %w", err)
	}

	// Apply --capture/--no-capture overrides on top of the materialized config
	// default (capture-by-default whenever the proxy is enabled, plan 012 D1).
	// Precedence: flags > config > default-on (captureOverrideSet is false when
	// neither flag was passed, leaving the config's own value untouched). Parse
	// always materializes cfg.Proxy.Capture whenever cfg.Proxy is non-nil (see
	// config.materializeCapture), so the defensive nil-check here only guards a
	// hand-built Config bypassing Parse (e.g. a future direct construction in
	// tests).
	if cfg.Proxy != nil {
		if cfg.Proxy.Capture == nil {
			cfg.Proxy.Capture = &config.CaptureConfig{}
		}
		if captureOverrideSet {
			cfg.Proxy.Capture.Enabled = captureOverrideEnabled
		}
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

	// Reap any orphaned backend process GROUPS left by a previous generation that
	// was killed with SIGKILL: its graceful shutdown never ran, so its supervised
	// child groups outlived it and may still hold ports (#59). The per-project
	// PID-file lock acquired above serializes this across concurrent `prox up`
	// invocations. Only groups positively identified as belonging to the prior
	// generation are signaled; unverifiable leftovers are left for the operator.
	reapLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if reaped, skipped, rerr := supervisor.ReapOrphans(daemon.StateDir(cwd), reapLogger); rerr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to reap orphaned child groups: %v\n", rerr)
	} else if len(reaped) > 0 || len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "Reaped %d orphaned child group(s) from a previous run; skipped %d (unverifiable or not confirmed gone)\n", len(reaped), len(skipped))
	}

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
	// The absolute config path lets an API-driven (re)start re-read prox.yaml and
	// apply the target process's current config (#33, D3).
	supConfig.ConfigPath = absConfigPath
	// The state dir lets the supervisor persist the orphan-reaping ownership
	// ledger on every launch, so a later `prox up` can reap groups a SIGKILL'd
	// generation orphaned (#59). ReapOrphans (above) reads it from the same path.
	supConfig.StateDir = daemon.StateDir(cwd)
	// cfg.ShutdownTimeout was already validated by config.Load above, so this
	// only errors for a hand-built Config bypassing Load/Validate.
	// ShutdownTimeoutDuration's error is already field-prefixed
	// ("shutdown_timeout: ..."), so it's returned as-is rather than wrapped again.
	globalShutdownTimeout, err := cfg.ShutdownTimeoutDuration()
	if err != nil {
		return err
	}
	if globalShutdownTimeout > 0 {
		supConfig.ShutdownTimeout = globalShutdownTimeout
	}
	sup := supervisor.New(cfg, logMgr, nil, supConfig)

	// Create the shutdown coordinator. Its Trigger()/TriggerCh() replace the raw
	// shutdownCh (a bare close was a latent double-close panic on a second POST
	// /shutdown); its Done()/Outcome() latch the process-stop verdict for the
	// foreground exit contract and for POST /shutdown?wait=true. The API handlers
	// receive the coordinator itself (as api.ShutdownController) so a wait=true
	// request can Trigger() the sequence and then read Done()/Outcome().
	coordinator := newShutdownCoordinator()

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

	// Create API handlers and server. The handlers get the absolute config path
	// so GET /status reports the same file the reload path re-reads (#33, D3).
	handlers := api.NewHandlers(sup, logMgr, absConfigPath, coordinator)

	// The proxy runtime is the single source of truth for the proxy path (D5):
	// it feeds the `prox status` proxy block via the handlers, receives forwarder
	// connection state, and (C6) holds the client swapped in on heal. Created
	// here (mode defaults to disabled) and resolved to shared/standalone below.
	runtime := newProxyRuntime()
	handlers.SetProxyStatusProvider(runtime)

	apiServer := api.NewServer(api.ServerConfig{
		Host:        cfg.API.Host,
		Port:        cfg.API.Port,
		AuthEnabled: authEnabled,
		Token:       token,
	}, handlers)

	// Bind the API listener BEFORE the supervisor starts any processes: a
	// bind failure (port taken) must fail the daemon while nothing is running
	// yet — binding later would leak an already-started supervisor on the
	// early error return (CodeRabbit PR #68). Serve starts further down.
	apiListener, err := apiServer.Listen()
	if err != nil {
		return err
	}

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Forward an external SIGINT/SIGTERM into the shutdown coordinator so BOTH the
	// TUI and non-TUI daemons honor it. signal.Notify above disables Go's default
	// signal handler, so without a consumer a --tui daemon would queue an external
	// SIGTERM forever and never tear down. Routing the signal through Trigger()
	// gives it the same path as POST /shutdown. The goroutine consumes sigCh
	// exactly once, then exits on ctx cancellation (defer cancel at runUp return)
	// so it never leaks.
	go forwardShutdownSignal(ctx, sigCh, coordinator, sup.SystemLog)

	// Start proxy — either via shared daemon or standalone fallback.
	var proxyService *proxy.Service
	var daemonClient *proxyd.Client
	if !noProxy && cfg.Proxy != nil && cfg.Proxy.Enabled {
		var proxyErr error
		daemonClient, proxyService, proxyErr = startProxy(cfg, cwd, ctx, handlers, runtime)
		if proxyErr != nil {
			return proxyErr
		}
	}
	// Ensure standalone proxy is cleaned up on any subsequent error. This defer
	// only tears down the proxy listeners (never processes), so it gets the
	// short proxy stage budget, matching the normal shutdown path. On the
	// normal shutdown path proxyService is set to nil first, so this becomes a
	// no-op there.
	defer func() {
		if proxyService != nil {
			sCtx, sCancel := context.WithTimeout(context.Background(), teardownStageTimeout)
			defer sCancel()
			_ = proxyService.Shutdown(sCtx)
		}
	}()

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

	// Serve on the listener bound before supervisor startup (see above).
	go func() {
		if err := apiServer.Serve(apiListener); err != nil {
			// Server closed is expected on shutdown
			if !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "API server error: %v\n", err)
			}
		}
	}()

	// Handle TUI vs terminal output
	if useTUI {
		// Run TUI - it blocks until quit. Passing the coordinator's trigger channel
		// lets POST /shutdown quit the program (a goroutine inside tui.Run calls
		// p.Quit() on trigger), so a TUI daemon honors the API shutdown and runs the
		// same shutdown sequence + exit contract below. Quitting the TUI by hand
		// still returns from tui.Run into that same sequence, unchanged.
		reqMgr := localRequestManager(proxyService, handlers)
		if err := tui.Run(sup, logMgr, reqMgr, coordinator.TriggerCh()); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	} else {
		// Subscribe to logs and print to terminal
		go printLogs(logMgr)

		// Wait for shutdown. Both a signal (via forwardShutdownSignal above, which
		// also logs it) and an API/TUI-quit trigger arrive as a coordinator trigger,
		// so this selects on the one channel.
		<-coordinator.TriggerCh()
		fmt.Println() // Print newline (past any echoed ^C)
	}

	// Stop signal handler to prevent additional signals during shutdown. The
	// forwarder goroutine, if still blocked on sigCh, exits when ctx is canceled at
	// runUp return.
	signal.Stop(sigCh)

	// Run the extracted shutdown sequence (same for the signal and API paths).
	// It tears down proxy + supervisor + API server in order, latches the
	// process-stop verdict on the coordinator, and flushes+closes the log manager
	// (before the API stage — see performShutdown).
	outcome := performShutdown(shutdownDeps{
		sup:          sup,
		runtime:      runtime,
		daemonClient: daemonClient,
		proxyService: proxyService,
		apiServer:    apiServer,
		coordinator:  coordinator,
		logMgr:       logMgr,
		reqMgr:       localRequestManager(proxyService, handlers),
		cwd:          cwd,
		stageTimeout: teardownStageTimeout,
	})
	// performShutdown already tore the standalone proxy down; nil the local so the
	// early-error cleanup defer (registered above) stays a no-op.
	proxyService = nil

	// Foreground exit contract: a surviving process group makes `prox up` exit
	// non-zero (cobra maps a non-nil RunE error to exit 1). Intentional split:
	// per-process detail already went to the log stream via SystemLog inside
	// performShutdown (the only record in detached mode), so here we return ONE
	// concise summary line rather than also letting cobra reprint every survivor —
	// wrapping the aggregate with %w keeps errors.Is/errors.As intact on the
	// returned error. Ctrl-C and API-triggered shutdowns behave identically.
	// Breaking change: foreground `prox up` previously always exited 0 (CHANGELOG
	// in C5).
	if outcome != nil {
		return fmt.Errorf("shutdown incomplete: %w", outcome)
	}
	return nil
}

// forwardShutdownSignal blocks until an external SIGINT/SIGTERM arrives on sigCh
// or ctx is canceled. On a signal it logs the receipt via logf and requests
// shutdown through the coordinator (the same path as POST /shutdown), so a daemon
// in either TUI or non-TUI mode honors the signal. It consumes at most one signal
// and returns on ctx cancellation, so it never outlives runUp.
func forwardShutdownSignal(ctx context.Context, sigCh <-chan os.Signal, coordinator *shutdownCoordinator, logf func(string, ...interface{})) {
	select {
	case sig := <-sigCh:
		if sig != nil && logf != nil {
			logf("%s received", sig)
		}
		coordinator.Trigger()
	case <-ctx.Done():
	}
}

// shutdownDeps bundles everything performShutdown needs. The supervisor is the
// real concrete type (unit tests drive it through its fake-runner seams); the
// proxy/API/logMgr deps are nil-able so a helper unit test needs no sockets or
// daemon.
type shutdownDeps struct {
	sup *supervisor.Supervisor
	// runtime is the proxy runtime (D5/D6). When set, performShutdown latches its
	// shutdown flag before deregister and reads the CURRENT daemon client through
	// it (a C6-healed client, not the one captured at startup). daemonClient is
	// the legacy fallback used only when runtime is nil (helper unit tests).
	runtime      *proxyRuntime
	daemonClient *proxyd.Client
	proxyService *proxy.Service
	apiServer    *api.Server
	coordinator  *shutdownCoordinator
	logMgr       *logs.Manager
	reqMgr       *proxy.RequestManager
	cwd          string
	stageTimeout time.Duration
}

// performShutdown runs the daemon-side shutdown sequence and returns the
// process-stop verdict (nil = clean). It is called from both the signal and API
// paths (they already share the wait select) and is extracted from runUp so unit
// tests can drive it with fakes.
//
// Stage order (each stage gets its own deadline so a slow stage never eats
// another's budget, #35/D2):
//
//  0. RefuseLaunches — close the launch gate immediately (see below);
//  1. proxy deregister (shared daemon) / standalone-proxy shutdown — stageTimeout;
//  2. sup.Stop — StopWaitBound()+stageTimeout, sized from the live per-process
//     budgets so hot-reloaded stop_timeouts are honored, and capturing the
//     aggregate survivor verdict;
//  3. coordinator.Complete(outcome) — publish the verdict to any waiter;
//  4. flush + logMgr.Close() — closes SSE log subscribers so they stop holding the
//     API open (see below);
//  5. API-server shutdown — stageTimeout.
//
// The launch gate is closed FIRST (RefuseLaunches), before the deregister stage,
// because the API keeps serving through deregister (which can take seconds) and
// Stop's own gate flip only happens in stage 2 — without this, a StartProcess/
// RestartProcess arriving during deregister would launch a process shutdown is
// about to orphan.
//
// The API server is shut down AFTER the supervisor stage (not before, as it was
// pre-C4) so it outlives sup.Stop: a future wait=true response (C5) must still be
// deliverable when the verdict lands. http.Server.Shutdown never force-closes
// in-flight requests — it stops accepting new connections, then waits for active
// handlers to return and leaves anything still running when the deadline passes
// for the client to see as a dropped connection. Lifecycle launches during this
// window are refused (the #41 state gate + the C2 launch gate).
//
// Deviation from the plan's D4 stage order (API shutdown → flush): the log manager
// is closed BEFORE the API stage. GET /logs/stream (SSE) handlers range over a log
// subscription channel and carry no route timeout at all (#42), so they would
// otherwise hold their connection indefinitely, making apiServer.Shutdown sit out
// its full 5s stage on every shutdown with a stream attached. Closing the manager
// closes those subscriber channels so the handlers return at once; the API stage
// then only waits for any small wait=true JSON write. This is safe because
// Write-after-Close is a no-op (RingBuffer.Write always succeeds; Broadcast/Send
// short-circuit on a closed subscription) and non-streaming GET /logs still reads
// the intact ring buffer, so no handler panics.
//
// The request manager is closed here for the same reason: GET
// /proxy/requests/stream (SSE) handlers range over a request subscription channel
// with no route timeout. In standalone-proxy mode the proxy stage already closed
// it (Close is idempotent — subscriptions are removed as they close); in
// shared-daemon mode this is the only close of the local forwarding manager,
// which nothing else tears down.
func performShutdown(deps shutdownDeps) *domain.ProcessStopError {
	// Stage 0: refuse new launches for the whole shutdown, including the deregister
	// stage below (the API still serves there, before Stop's own gate flip).
	if deps.sup != nil {
		deps.sup.RefuseLaunches()
	}

	// D6c shutdown ordering. Latch the shutdown flag FIRST so an in-flight or next
	// heal no-ops, then cancel the forwarder BEFORE deregister so its reconnect
	// loop cannot fire a heal that re-registers the project mid-teardown. Resolve
	// the daemon client through clientAfterHealBarrier AFTER both: it blocks on the
	// heal mutex until any heal that began before the latch has finished swapping
	// the client, so we deregister through the HEALED client, never the one captured
	// at startup (FIX 3). daemonClient is the fallback for helper tests with no runtime.
	daemonClient := deps.daemonClient
	if deps.runtime != nil {
		deps.runtime.MarkShuttingDown()
		deps.runtime.CancelForwarder()
		daemonClient = deps.runtime.clientAfterHealBarrier()
	}

	// Stage 1a: deregister from the shared proxy daemon. The proxyd client carries
	// its own 30s HTTP timeout and takes no ctx, so bound it to the stage here:
	// run it on a goroutine and proceed once the stage deadline passes, leaving the
	// call to finish or fail in the background (harmless — the daemon is exiting).
	if daemonClient != nil {
		derr := make(chan error, 1) // buffered so an abandoned goroutine never leaks
		go func() {
			derr <- daemonClient.Deregister(proxyd.DeregisterRequest{
				ProjectDir: deps.cwd,
				PID:        os.Getpid(),
			})
		}()
		timer := time.NewTimer(deps.stageTimeout)
		select {
		case err := <-derr:
			timer.Stop()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to deregister from proxy daemon: %v\n", err)
			}
		case <-timer.C:
			fmt.Fprintf(os.Stderr, "Warning: proxy daemon deregister exceeded %s; abandoning in background\n", deps.stageTimeout)
		}
	}

	// Stage 1b: stop the standalone proxy (listeners only, never processes).
	// Accepted pre-existing behavior (out of scope for #36): proxyService.Shutdown
	// performs an unbounded os.RemoveAll of on-disk capture bodies before this
	// function reaches the verdict publish. It is small in practice (capture bodies
	// are size-capped) and the daemon is exiting anyway, so it is left as-is rather
	// than bounded here.
	if deps.proxyService != nil {
		proxyCtx, proxyCancel := context.WithTimeout(context.Background(), deps.stageTimeout)
		if err := deps.proxyService.Shutdown(proxyCtx); err != nil {
			fmt.Fprintf(os.Stderr, "Error stopping proxy: %v\n", err)
		}
		proxyCancel()
	}

	// Stage 2: stop the supervisor and capture the aggregate verdict.
	supCtx, supCancel := context.WithTimeout(context.Background(), deps.sup.StopWaitBound()+deps.stageTimeout)
	stopErr := deps.sup.Stop(supCtx)
	supCancel()

	var outcome *domain.ProcessStopError
	if stopErr != nil {
		if !errors.As(stopErr, &outcome) {
			// Invariant: today sup.Stop returns only *ProcessStopError or nil, so this
			// branch is defensive. But an acknowledged non-aggregate error (e.g. a ctx
			// expiry, or a future error type) must NOT read as a clean stop: leaving
			// outcome nil here would Complete(nil), hand waited clients success, and
			// exit the foreground 0 despite the failure. Synthesize a single-failure
			// aggregate so the error latches, reaches waited clients, and fails the
			// foreground exit contract.
			deps.sup.SystemLog("supervisor stop error: %v", stopErr)
			outcome = &domain.ProcessStopError{
				Failures: []domain.ProcessStopFailure{{Name: "supervisor", Err: stopErr}},
			}
		}
	}

	// Stage 3: publish the verdict. A latched broadcast — waiters (zero today, C5's
	// wait=true handlers next) read the same stored outcome after <-Done().
	if deps.coordinator != nil {
		deps.coordinator.Complete(outcome)
	}

	// Stage 4: log the final lines, then flush and close the log manager BEFORE the
	// API stage. Per-survivor detail goes to the log stream here (the only record in
	// detached mode); runUp returns a one-line summary for the exit code. Closing the
	// manager closes every SSE subscriber channel so /logs/stream handlers return and
	// do not pin the API server open through the next stage (see the doc comment).
	if deps.sup != nil {
		if outcome != nil {
			for _, f := range outcome.Failures {
				deps.sup.SystemLog("process %s did not stop cleanly: %v", f.Name, f.Err)
			}
		}
		deps.sup.SystemLog("shutdown complete")
	}
	// Give the terminal log printer a moment to drain the final lines before Close.
	time.Sleep(logFlushDelay)
	if deps.logMgr != nil {
		deps.logMgr.Close()
	}
	// Release any /proxy/requests/stream subscribers for the same reason (see the
	// doc comment); without this, a connected stream pins the API stage to its
	// full deadline in shared-daemon mode (#42 codex review finding).
	if deps.reqMgr != nil {
		deps.reqMgr.Close()
	}

	// Stage 5: stop the API server, now that the verdict is published, the log
	// subscribers are released, and any waited response can drain.
	if deps.apiServer != nil {
		apiCtx, apiCancel := context.WithTimeout(context.Background(), deps.stageTimeout)
		if err := deps.apiServer.Shutdown(apiCtx); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		apiCancel()
	}

	return outcome
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

// resolveCaptureFlag applies --capture/--no-capture precedence over the
// materialized config default (plan 012 D1, C4): flags always win over
// config, which in turn defaults on whenever the proxy is enabled. Passing
// both flags is an error, mirroring the --tui/--detach exclusivity check.
// ok reports whether either flag was passed at all; when ok is false, the
// caller must leave the config's own Enabled value untouched (neither flag
// set means "config wins / default-on", not "force off").
func resolveCaptureFlag(enableCapture, noCapture bool) (enabled, ok bool, err error) {
	if enableCapture && noCapture {
		return false, false, fmt.Errorf("--capture and --no-capture are mutually exclusive")
	}
	switch {
	case enableCapture:
		return true, true, nil
	case noCapture:
		return false, true, nil
	default:
		return false, false, nil
	}
}

// proxyStartError formats an actionable error message for proxy startup failures.
// For port conflicts it includes the port number and a hint to identify the process.
func proxyStartError(err error) error {
	var portErr *proxy.PortConflictError
	if errors.As(err, &portErr) {
		return fmt.Errorf("%w\n\nAnother process is listening on this port. To identify it:\n  lsof -i :%d\n\nTo start without the proxy:\n  prox up --no-proxy", portErr, portErr.Port)
	}
	return fmt.Errorf("proxy failed to start: %w\n\nTo start without the proxy:\n  prox up --no-proxy", err)
}

// captureMaxBodySize resolves the project's configured capture cap to bytes for
// the register wire (D13, #49). It parses cfg.Proxy.Capture.MaxBodySize (e.g.
// "1MB", "512KB") with the same parser the standalone capture manager uses. An
// unset or unparseable value yields 0, which the daemon reads as "use the
// default cap" — a bad string degrades gracefully rather than failing `prox up`.
func captureMaxBodySize(cfg *config.Config) int64 {
	if cfg.Proxy.Capture == nil || cfg.Proxy.Capture.MaxBodySize == "" {
		return 0
	}
	n, err := config.ParseSize(cfg.Proxy.Capture.MaxBodySize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: invalid proxy.capture.max_body_size %q: %v — using the daemon default capture cap\n",
			cfg.Proxy.Capture.MaxBodySize, err)
		return 0
	}
	return n
}

// captureDiskBudget resolves the project's configured capture disk budget to
// bytes for the register wire (#69). It parses cfg.Proxy.Capture.DiskBudget with
// the same parser the capture accountant uses. An unset or unparseable value
// yields 0, which the daemon reads as "use the default budget" — a bad string
// degrades gracefully rather than failing `prox up`. Validate has normally
// already rejected an unparseable value; this is the belt-and-suspenders path.
func captureDiskBudget(cfg *config.Config) int64 {
	if cfg.Proxy.Capture == nil || cfg.Proxy.Capture.DiskBudget == "" {
		return 0
	}
	n, err := config.ParseSize(cfg.Proxy.Capture.DiskBudget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: invalid proxy.capture.disk_budget %q: %v — using the daemon default capture disk budget\n",
			cfg.Proxy.Capture.DiskBudget, err)
		return 0
	}
	return n
}

// startProxy attempts to register with the shared proxy daemon. If the daemon
// cannot be reached (e.g., sandboxed environment), it falls back to starting a
// standalone proxy. Returns the daemon client (if using daemon) and/or the
// standalone proxy service (if using fallback).
func startProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers, rt *proxyRuntime) (*proxyd.Client, *proxy.Service, error) {
	// Try shared daemon first
	client, ok, fatalErr := tryDaemonProxy(cfg, cwd, ctx, handlers, rt)
	if ok {
		return client, nil, nil
	}
	if fatalErr != nil {
		// Daemon is running but registration failed (conflict, version mismatch, etc.)
		return nil, nil, fatalErr
	}

	// Fallback to standalone proxy (daemon unavailable or sandboxed). A proxy is
	// configured, so a create/start failure here is fatal (D1): `prox up` must
	// never start a project with the proxy silently disabled. --no-proxy is the
	// escape hatch (named in the returned error).
	svc, err := startStandaloneProxy(cfg, cwd, ctx, handlers)
	if err != nil {
		return nil, nil, err
	}
	rt.SetMode(proxyModeStandalone)
	return nil, svc, nil
}

// tryDaemonProxy attempts to register with the shared proxy daemon.
// Returns (client, true, nil) on success, (nil, false, nil) when daemon is
// unavailable (fall back to standalone), (nil, false, error) when daemon is
// running but registration failed (don't fall back, fail the command).
func tryDaemonProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers, rt *proxyRuntime) (*proxyd.Client, bool, error) {
	// Check if we can access the daemon directory
	if err := proxyd.EnsureDaemonDir(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v — running proxy in standalone mode\n", err)
		return nil, false, nil
	}

	// Ensure daemon is running
	client, err := proxyd.EnsureRunning()
	if err != nil {
		var vme *proxyd.VersionMismatchError
		if !errors.As(err, &vme) {
			// Connection-refused / no socket (e.g. a sandboxed environment that
			// legitimately cannot reach the daemon): fall back to standalone.
			fmt.Fprintf(os.Stderr, "Warning: proxy daemon unavailable: %v — running proxy in standalone mode\n", err)
			return nil, false, nil
		}
		// Version skew: the shared daemon runs a different version. Never fall
		// back to standalone (its ports are daemon-held, and a silent
		// proxy-less start is exactly what D1 closes). Recover or fail fatally.
		healed, ferr := recoverFromVersionSkew(vme, defaultSkewOps())
		if ferr != nil {
			return nil, false, ferr
		}
		client = healed
	}

	// Build registration request
	services := make(map[string]proxyd.ServiceTarget, len(cfg.Services))
	for name, svc := range cfg.Services {
		services[name] = proxyd.ServiceTarget{Host: svc.Host, Port: svc.Port}
	}

	// Read our own process start token so the daemon can tell a genuine
	// same-dir conflict from a crashed generation whose PID has been reused.
	startToken, ok := daemon.ProcessStartTime(os.Getpid())
	if !ok {
		fmt.Fprintln(os.Stderr, "Warning: could not read process start token; proxy PID-reuse protection is degraded to bare-PID")
	}

	req := proxyd.RegisterRequest{
		ProjectDir: cwd,
		PID:        os.Getpid(),
		Version:    version.Version,
		Domain:     cfg.Proxy.Domain,
		Services:   services,
		HTTPPort:   cfg.Proxy.HTTPPort,
		HTTPSPort:  cfg.Proxy.HTTPSPort,
		// CaptureEffectivelyEnabled re-checks the proxy-enabled gate (plan 012 D1,
		// C4): tryDaemonProxy is only ever reached when cfg.Proxy.Enabled is
		// already true (see the call site in runUp), so this is equivalent to the
		// prior "Capture != nil && Capture.Enabled" here, but it is the single
		// helper every gated use site shares rather than re-deriving the check.
		CaptureEnabled: cfg.Proxy.CaptureEffectivelyEnabled(),
		MaxBodySize:    captureMaxBodySize(cfg),
		DiskBudget:     captureDiskBudget(cfg),
		StartTime:      startToken,
	}

	resp, err := client.Register(req)
	if err != nil {
		if !isShuttingDownError(err) {
			return nil, false, fmt.Errorf("failed to register with proxy daemon: %w", err)
		}
		// D4: the daemon was mid graceful-shutdown when the register queued
		// behind it. Wait for it to drain, start a fresh daemon, and
		// re-register once — a second SHUTTING_DOWN or any other error is
		// fatal exactly as an unrecovered register failure is today.
		healedClient, healedResp, rerr := retryRegisterAfterShutdown(req, defaultRegisterRetryOps())
		if rerr != nil {
			return nil, false, fmt.Errorf("failed to register with proxy daemon: %w", rerr)
		}
		client = healedClient
		resp = healedResp
	}

	// Print registered routes
	var proxyAddrs []string
	if cfg.Proxy.HTTPPort > 0 {
		proxyAddrs = append(proxyAddrs, fmt.Sprintf("http://*.%s:%d", cfg.Proxy.Domain, cfg.Proxy.HTTPPort))
	}
	if cfg.Proxy.HTTPSPort > 0 {
		proxyAddrs = append(proxyAddrs, fmt.Sprintf("https://*.%s:%d", cfg.Proxy.Domain, cfg.Proxy.HTTPSPort))
	}
	if len(proxyAddrs) > 0 {
		fmt.Printf("Proxy (shared daemon): %s\n", strings.Join(proxyAddrs, ", "))
	}
	if len(resp.Registered) > 0 {
		fmt.Printf("Registered domains: %s\n", strings.Join(resp.Registered, ", "))
	}

	// Create a local RequestManager and start the SSE forwarder to bridge
	// daemon proxy requests into this project's TUI and API. The runtime records
	// the shared mode, the active client, the original register request (C6
	// re-registers with it), and the local manager (source of the dropped-events
	// count), and receives forwarder connection state as the status sink (D5).
	localRM := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)
	handlers.SetRequestManager(localRM)
	rt.SetMode(proxyModeShared)
	rt.SetClient(client)
	rt.SetRegisterRequest(req)
	rt.SetLocalRequestManager(localRM)

	// Run the forwarder on a context derived from ctx but cancellable on its own,
	// so performShutdown can stop it BEFORE deregister (D6c) without disturbing the
	// supervisor (which shares ctx). Its heal callback (D6b) re-ensures and
	// re-registers against a fresh daemon after a prolonged outage; it no-ops once
	// shutdown is latched. Deriving from ctx guarantees the forwarder still stops
	// on any runUp return even if performShutdown never runs.
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	rt.SetForwarderCancel(fwdCancel)
	heal := func() bool { return rt.heal(defaultHealOps()) }
	go proxyd.ForwardRequests(fwdCtx, proxyd.SocketPath(), cwd, localRM, rt, heal)

	return client, true, nil
}

// startStandaloneProxy creates and starts a standalone proxy. When a proxy is
// configured, create/start failures are fatal (D1): they return an error so
// `prox up` exits non-zero rather than continuing with the proxy silently
// disabled. The returned error carries the proxyStartError hint (port-conflict
// detail plus the --no-proxy escape hatch).
func startStandaloneProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers) (*proxy.Service, error) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	proxyService, err := proxy.NewService(cfg.Proxy, cfg.Services, cfg.Certs, logger, cwd)
	if err != nil {
		return nil, proxyStartError(err)
	}
	if err := proxyService.Start(ctx); err != nil {
		return nil, proxyStartError(err)
	}

	var proxyAddrs []string
	if cfg.Proxy.HTTPPort > 0 {
		proxyAddrs = append(proxyAddrs, fmt.Sprintf("http://*.%s:%d", cfg.Proxy.Domain, cfg.Proxy.HTTPPort))
	}
	if cfg.Proxy.HTTPSPort > 0 {
		proxyAddrs = append(proxyAddrs, fmt.Sprintf("https://*.%s:%d", cfg.Proxy.Domain, cfg.Proxy.HTTPSPort))
	}
	if len(proxyAddrs) > 0 {
		fmt.Printf("Proxy (standalone): %s\n", strings.Join(proxyAddrs, ", "))
	}
	handlers.SetRequestManager(proxyService.RequestManager())
	handlers.SetCaptureManager(proxyService.CaptureManager())

	return proxyService, nil
}

// localRequestManager returns the RequestManager for use by the TUI.
// In daemon mode, a local request manager is created (will be fed via SSE in Phase 5).
// In standalone mode, the proxy service's request manager is returned.
func localRequestManager(proxyService *proxy.Service, handlers *api.Handlers) *proxy.RequestManager {
	if proxyService != nil {
		return proxyService.RequestManager()
	}
	// In daemon mode, we return the request manager from the handlers
	// (which may be nil if no proxy is configured, which is fine for TUI)
	return handlers.GetRequestManager()
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
