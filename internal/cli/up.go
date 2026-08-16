package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
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
	"github.com/charliek/prox/internal/proxy/certs"
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
	// logRingBufferSize is how many entries this session's log manager keeps.
	// It doubles as the replay bound for a preferred-mode TUI fallback
	// (streamLogsAfterTUIFallback): asking for the whole ring is exactly "give
	// me everything you still have", and naming the two with one constant keeps
	// them from drifting apart.
	logRingBufferSize = 1000
)

// Up command flags
var (
	useTUI        bool
	noTUI         bool
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

In the foreground prox opens the interactive TUI whenever the terminal can host
one, and streams plain logs otherwise (a pipe, a redirect, CI, TERM=dumb, or a
backgrounded 'prox up &'). Quitting the TUI with 'q' stops the processes, just
as Ctrl-C does.

Use --no-tui (or PROX_TUI=0) to stream plain logs on a terminal too, --tui to
require the TUI and fail if the terminal cannot host one, or -d/--detach to run
in the background and watch it later with 'prox attach'.

Examples:
  prox up                     # Start all processes (TUI on a terminal, plain logs otherwise)
  prox up --no-tui            # Start in the foreground with plain log streaming
  prox up -d                  # Start in background (daemon mode)
  prox up --tui               # Require the TUI (error if the terminal cannot host one)
  prox up web api             # Start specific processes
  prox up --no-proxy          # Start without proxy
  prox up --no-capture        # Disable request/response capture for this run`,
	Args:              cobra.ArbitraryArgs,
	RunE:              runUp,
	ValidArgsFunction: completeProcessNames,
}

func init() {
	rootCmd.AddCommand(upCmd)

	upCmd.Flags().BoolVar(&useTUI, "tui", false, "Require the interactive TUI (error if the terminal cannot host one; it is already the default on a capable terminal)")
	upCmd.Flags().BoolVar(&noTUI, "no-tui", false, "Disable interactive TUI mode for this run (overrides PROX_TUI)")
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

	// Resolve how this invocation wants the TUI, FIRST, before any other work:
	// explicit flag > PROX_TUI > terminal capability (see tui_mode.go). This
	// pure step subsumes the old `useTUI && detach` conflict check, with the
	// same message; the terminal probe it used to sit next to happens a few
	// lines below, deliberately after the capture check.
	//
	// The wiring is the one line the pure matrix cannot cover, so read it
	// carefully: Changed() answers "was it typed", the flag var answers "what
	// did it parse to", and swapping the two is exactly the bug that would
	// break `prox up -d --tui=false`.
	envTUI, envTUIPresent := os.LookupEnv(proxTUIEnvVar)
	tuiWantMode, tuiWarnings, err := resolveTUIMode(tuiModeInputs{
		TUISet:     cmd.Flags().Changed("tui"),
		TUIVal:     useTUI,
		NoTUISet:   cmd.Flags().Changed("no-tui"),
		NoTUIVal:   noTUI,
		Detach:     detach,
		Env:        envTUI,
		EnvPresent: envTUIPresent,
		// THE FLIP (plan 026 C7): with nothing asked for either way, a
		// foreground `prox up` now PREFERS the TUI. Preferred, never required —
		// every non-interactive invocation (a pipe, CI, an agent harness,
		// TERM=dumb, a backgrounded `prox up &`) falls back to plain log
		// streaming silently, and `--no-tui` / `PROX_TUI=0` are the explicit
		// opt-outs. `--detach` short-circuits the whole decision, so this flag
		// does not reach `prox up -d` at all.
		AutoDefault: true,
	})
	if err != nil {
		return err
	}
	// captureOverrideSet/captureOverrideEnabled resolve once here (before cfg is
	// even loaded) so the mutual-exclusivity error surfaces immediately, mirroring
	// the --tui/--detach check above; applied to cfg.Proxy.Capture.Enabled below
	// once cfg is loaded.
	captureOverrideEnabled, captureOverrideSet, err := resolveCaptureFlag(enableCapture, noCapture)
	if err != nil {
		return err
	}
	// A TUI needs a keyboard, a screen, a TERM it can draw with, and the
	// terminal's foreground process group. Probe here — before the supervisor,
	// proxy, or API server exist — so a piped or non-interactive invocation
	// (`prox up --tui | tee log`, CI, an agent harness) fails fast instead of
	// starting everything and then handing bubbletea a screen nobody can read and a
	// keyboard nobody can press — a run that can then only be ended by signal.
	// Ordered AFTER the flag-conflict checks above so a genuine misuse of flags
	// reports the conflict rather than the terminal.
	//
	// required (an explicit --tui) errors; preferred (the default, or
	// PROX_TUI=1) degrades silently to plain streaming, with no stderr note —
	// a note on every non-interactive `prox up` would pollute CI output forever
	// for a mode change the caller never asked about. This is the branch that
	// keeps every piped, redirected, CI and agent-harness `prox up` behaving
	// exactly as it did before the TUI became the default.
	tuiEnabled := tuiWantMode != tuiModePlain
	if tuiEnabled {
		if hostErr := terminalHostable(); hostErr != nil {
			if tuiWantMode == tuiModeRequired {
				return hostErr
			}
			tuiEnabled = false
		}
	}
	// Everything this run prints about itself is collected here as well as
	// printed, so a TUI session can render it on a screen the user can actually
	// see (see preamble.go). Disabled — and therefore free — in plain mode.
	preamble := newStartupPreamble(tuiEnabled)
	// The single warning call site (see reportTUIWarnings).
	reportTUIWarnings(tuiWarnings, preamble, tuiEnabled, os.Stderr)

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
		// ready AND its processes have been watched for the settle window
		// without any of them reaching a terminal-failed state (#94), or an
		// error (exit 1) on early death, never-ready timeout, or a process
		// that started and immediately died.
		return startDetachedDaemon(&execChild{cmd: child}, cwd, defaultDaemonStartupOps())
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

	// Route every unintended diagnostic in this process through one switchable
	// sink so a TUI session can render it instead of having its frame corrupted
	// by it (see stdio_sink.go for the full audit and the concurrency protocol).
	// This covers all 17 stdlib log.Printf sites AND every http.Server here,
	// none of which sets ErrorLog — so net/http's TLS handshake errors and
	// handler-panic dumps land in the sink too.
	//
	// A `-d` child's diagnostics reach .prox/prox.log rather than the /dev/null
	// its real fd 2 points at, because the sink resolves os.Stderr at WRITE
	// time and daemon.SetupLogging above reassigned that variable. Until now
	// those lines were lost outright. (Construction order does NOT matter for
	// that — lazy resolution is what makes it work — but the DEFER order does:
	// see below.)
	//
	// Outside a TUI window the sink is a synchronous pass-through to stderr, so
	// plain `prox up`, `-d` and CI behave exactly as before.
	sink := newStdioSink()
	restoreStdio := installStdioSink(sink)
	// This install MUST stay below `defer logFile.Close()` above. Defers are
	// LIFO, so registering the restore later makes it run EARLIER — its final
	// flush to os.Stderr therefore lands in the daemon log file while that file
	// is still open. Move this block above the SetupLogging block and the
	// teardown diagnostics of every `-d` session are silently written to a
	// closed descriptor.
	//
	// The restore itself is a backstop ONLY, for a panic or an early return:
	// the owner-mode TUI path below restores explicitly and synchronously the
	// moment RunClient returns, because this defer fires after performShutdown
	// has already closed the log manager (see stdioSink.RestoreStderr's doc
	// comment). Restoring twice is a no-op.
	defer restoreStdio()

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
		BufferSize:         logRingBufferSize,
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
	// Attach the preamble's second path now that there is a supervisor to log
	// through: every preamble line also becomes a system log entry, so `prox
	// logs`, the daemon log and any other subscriber get it too. SystemLog is
	// safe before sup.Start, and the sup construction here precedes every
	// preamble site below — the warnings recorded above are flushed by this
	// call.
	preamble.logTo(sup.SystemLog)

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
	// so GET /status reports the same file the reload path re-reads (#33, D3),
	// and cwd -- the SAME directory state.Write used above -- so GET /status
	// reports this daemon's project identity and a client can tell whether the
	// prox answering on a discovered port is its own (plan 020 C3).
	handlers := api.NewHandlers(sup, logMgr, absConfigPath, cwd, coordinator)

	// The proxy runtime is the single source of truth for the proxy path (D5):
	// it feeds the `prox status` proxy block via the handlers, receives forwarder
	// connection state, and (C6) holds the client swapped in on heal. Created
	// here (mode defaults to disabled) and resolved to shared/standalone below.
	runtime := newProxyRuntime()
	// This session's user-facing advisories (plan 028 A2). Created before the
	// proxy path resolves, because the shared daemon's register response is the
	// first producer; published on GET /status (which is how the `prox up -d`
	// parent reads them back out of a child whose output goes to a log file) and
	// rendered once at the end of startup below.
	warnings := newWarningSink()
	runtime.SetWarningSink(warnings)
	handlers.SetWarningProvider(warnings)
	// The suite's only way to produce a warning that finishes AFTER the
	// `prox up -d` parent's settle window — the race the completion latch
	// exists for. Unset in every real run.
	registerTestWarningProducer(warnings, os.Getenv(warningTestHookEnvVar))
	// The RESOLVED runtime proxy state for this session — what the proxy path
	// below actually does, not what the file asks for (see
	// resolveProxyRuntimeState). It gates the proxy start, feeds the status
	// block, and tells the TUI why a request list may stay empty.
	proxyFacts := resolveProxyRuntimeState(cfg, noProxy)
	runtime.SetCaptureEnabled(proxyFacts.CaptureEnabled)
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
	if proxyFacts.Configured {
		var proxyErr error
		daemonClient, proxyService, proxyErr = startProxy(cfg, cwd, ctx, handlers, runtime, sink, preamble)
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

	// Subscribe to logs SYNCHRONOUSLY, on this goroutine, before the supervisor
	// starts any process (D2, #92). A process that crashes instantly (typo'd
	// cmd:, missing binary) emits its error the moment the supervisor starts
	// it; if the subscription were created later — even by an already-launched
	// goroutine, since `go f()` gives no ordering guarantee that f has run
	// before the next statement — that error can be broadcast before any
	// subscriber exists and is lost to the terminal (though it remains
	// retrievable via `prox logs`). Subscribing here, before sup.Start/
	// StartProcesses below, closes the race: subscription channels are
	// buffered and Send is a non-blocking select (subscription.go), so
	// subscribing before the consumer goroutine runs is safe.
	//
	// Gated on !tuiEnabled (resolved at the very top of runUp, long before this
	// point): a TUI session must have NO terminal log subscriber, since nothing
	// would ever drain it, guaranteeing an overflow (which permanently closes
	// the subscription, subscription.go) and a spurious "subscription
	// overflowed" line every TUI session.
	//
	// The subscribe and the consumer goroutine start close together and both
	// before supervisor start: Broadcast closes a subscription on its very
	// first overflow, so subscribing early and only later getting around to
	// draining it would itself risk losing the subscription before it is read.
	//
	// The one path that subscribes LATE is a preferred-mode TUI that failed to
	// start (see streamLogsAfterTUIFallback below). It is safe there only
	// because it replays the ring first — everything skipped here is still in
	// the buffer at that point.
	var logCh <-chan domain.LogEntry
	if !tuiEnabled {
		var subErr error
		logCh, subErr = subscribeLogPrinter(logMgr)
		if subErr != nil {
			return fmt.Errorf("failed to subscribe to logs: %w", subErr)
		}
		go printLogEntries(logCh)
	}

	// Start supervisor
	preamble.printf("Starting prox with config: %s", configPath)
	if isLocalhost(cfg.API.Host) {
		if authEnabled {
			preamble.printf("API server: http://%s (local only, auth enabled)", apiServer.Addr())
		} else {
			preamble.printf("API server: http://%s (local only, no auth)", apiServer.Addr())
		}
	} else {
		if authEnabled {
			preamble.printf("API server: http://%s (network accessible, auth enabled)", apiServer.Addr())
		} else {
			preamble.printf("API server: http://%s (network accessible, no auth)", apiServer.Addr())
		}
	}
	if authEnabled {
		preamble.printf("Auth token saved to: %s", tokenPath())
	}

	if len(processes) > 0 {
		preamble.printf("Starting processes: %s", strings.Join(processes, ", "))
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
				// Through the stdlib logger (and therefore the stdio sink), not
				// straight to stderr: this fires on a DETACHED goroutine for the
				// whole life of the session, so a permanent listener error would
				// otherwise be written over the TUI's alt screen. log.SetFlags(0)
				// keeps the plain-mode line byte-identical to the old write.
				log.Printf("API server error: %v", err)
			}
		}
	}()

	// END OF STARTUP: join the asynchronous warning producers, render everything
	// this session has to say, and seal (plan 028 A2).
	//
	// All three steps are here, on runUp's own goroutine, deliberately:
	//
	//   - rendering goes through the preamble, which is unsynchronized by
	//     construction, so it can only happen here;
	//   - ONE render point means the user meets every advisory in one block and
	//     one order, whatever produced it;
	//   - sealing after this point (and after the API server above began
	//     serving) is what makes warnings_sealed worth publishing at all. GET
	//     /status can already be answered while a producer is still running,
	//     which is exactly the window the `prox up -d` parent lands in — it
	//     polls the latch rather than trusting a single fetch.
	//
	// The join is bounded: a warning must never meaningfully delay startup. A
	// producer that misses the budget still reaches GET /status when it lands,
	// it just misses the parent's read.
	if !warnings.Wait(warningProducerJoinTimeout) {
		log.Printf("prox: startup warning checks did not finish within %s; continuing", warningProducerJoinTimeout)
	}
	reportStartupWarnings(warnings.Warnings(), preamble, os.Stderr)
	warnings.Seal()

	// Handle TUI vs terminal output. tuiErr survives the block: a failed TUI
	// session must reach the exit contract below.
	var tuiErr error
	if tuiEnabled {
		// `up --tui` runs the SAME API-client TUI as `prox attach` (plan 018), just
		// pointed at the API server this process started a few lines above instead of
		// another project's daemon. One model, one set of streams, one set of key
		// bindings to maintain — the owner's TUI is no longer a second implementation
		// reading the supervisor and log manager in-process.
		//
		// Two ownership notes:
		//
		//   - ShutdownCh is the coordinator's trigger channel, so POST /shutdown and an
		//     external SIGINT/SIGTERM quit the program exactly as they did before.
		//   - "q stops the supervisor" is owned by the FALLTHROUGH, not by the TUI:
		//     RunClient returning for ANY reason continues into the shutdown sequence
		//     below, which is what takes the processes down. Attach mode gets the
		//     opposite behavior purely by being a different process.
		//
		// ConnectedStatus is deliberately empty: this is the owner's own TUI, not a
		// remote attach, so there is no connection to advertise in the status line.
		// The token goes in through memory (see NewClientWithToken) rather than being
		// re-read from ~/.prox/token.
		client := NewClientWithToken(dialableAPIURL(cfg.API.Host, boundAPIPort(apiListener, cfg.API.Port)), token)
		tuiOpts := tui.ClientOptions{
			// Owner-mode wording. `q` behaves exactly as it always has —
			// RunClient returning falls through into the shutdown sequence
			// below — but this TUI is pixel-for-pixel the attach TUI, where the
			// same key leaves a daemon running, so the label is what keeps the
			// two apart (plan 026 §3.2). The note names the supported way to
			// keep processes alive past quit, since Model A has no detach key.
			Help: tui.HelpConfig{
				TitleSuffix: "",
				QuitMessage: "Quit (stops processes)",
				QuitNote:    "To keep processes running, start with 'prox up -d' and use 'prox attach'",
				QuitHint:    "stop",
			},
			ShutdownCh:  coordinator.TriggerCh(),
			ProjectName: tui.ConfigPathProjectName(absConfigPath),
			// The startup lines this run printed to a terminal the alt screen is
			// about to hide — pinned in the log pane so the proxy URL survives a
			// startup chatty enough to have evicted them from the log ring.
			Preamble: preamble.Lines(),
			// Why the requests pane may stay empty. Sourced from the SAME
			// resolved runtime state that gated the proxy start above, so a
			// `--no-proxy` session reports "no proxy configured" rather than
			// promising traffic this run can never see.
			ProxyConfigured: proxyFacts.Configured,
			CaptureEnabled:  proxyFacts.CaptureEnabled,
		}
		// Ports feed curl-copy only; pass them when a proxy is actually
		// listening (disabled/--no-proxy → port-less https://<host><url>).
		if cfg.Proxy != nil && cfg.Proxy.Enabled {
			tuiOpts.ProxyHTTPSPort = cfg.Proxy.HTTPSPort
			tuiOpts.ProxyHTTPPort = cfg.Proxy.HTTPPort
		}
		// For the length of the alt-screen session, unintended diagnostics
		// become "system" entries in the log pane instead of bytes written over
		// a rendered frame. The route is asynchronous by necessity — a
		// synchronous adapter deadlocks inside logs.Manager the first time a
		// subscriber overflows (see stdio_sink.go).
		sink.RouteToLogManager(logMgr)
		runErr := tui.RunClient(client, tuiOpts)
		// Restore EXPLICITLY and synchronously here, not via a runUp-scoped
		// defer: that defer runs after performShutdown closes logMgr, so every
		// teardown-era line — the shared-daemon "lost connection" line most of
		// all — would be written into a manager with no subscribers that is
		// about to be discarded, while the terminal showed nothing. This call
		// is the barrier + drain + join back to stderr.
		sink.RestoreStderr()
		switch classifyTUIExit(runErr, tuiWantMode) {
		case tuiExitClean:
			// `q`, POST /shutdown, an external signal — including a SIGINT that
			// bubbletea's own handler won the race for (see tui.IsCleanExit).
			// Nothing to report and nothing to fall back to: the fallthrough
			// below runs the shutdown sequence exactly as a `q` quit does.
		case tuiExitFailedRequired:
			// Printed now for the interactive user, and retained: a session
			// whose TUI failed must not exit 0 just because shutdown went
			// cleanly — scripted callers need to tell the two apart
			// (CodeRabbit, PR #88). Folded into the exit contract below.
			//
			// REQUIRED only. The user typed `--tui`, so a TUI that cannot run
			// is a failed request and must fail the command, exactly as it did
			// before the TUI became the default.
			tuiErr = fmt.Errorf("TUI error: %w", runErr)
			fmt.Fprintf(os.Stderr, "Error: %v\n", runErr)
		case tuiExitFailedPreferred:
			// PREFERRED (the default, or PROX_TUI=1): nobody asked for a TUI on
			// this command line, so a bubbletea failure must not turn a working
			// `prox up` into exit 1 (plan 026 §3.1, CodeRabbit finding 10).
			// Degrade to the plain log stream this run would have had if the
			// terminal had been incapable in the first place.
			//
			// ANY failure that is not an orderly quit, not just an
			// initialization failure. bubbletea does separate the two —
			// everything after its event loop starts comes back wrapped in
			// tea.ErrProgramKilled, initialization errors do not — but the safe
			// side of that line is the same either way: falling back leaves the
			// processes running under a terminal log stream and a working
			// Ctrl-C, whereas treating a mid-session failure as fatal tears down
			// a developer's whole stack because a UI they never asked for
			// crashed. What IS separated out (above) is an orderly quit that
			// merely reports an error — an external SIGINT bubbletea won the
			// race for — which is not a failure at all.
			//
			// Said out loud, unlike the silent capability fallback: "this
			// terminal cannot host a TUI" is a routine, expected answer, while
			// "the TUI itself broke" is an anomaly the user is entitled to see
			// the reason for — and by this point the primary screen is back, so
			// it is a line they can actually read.
			fmt.Fprintf(os.Stderr, "Warning: the TUI could not run (%v); falling back to plain log streaming\n", runErr)
			if subErr := streamLogsAfterTUIFallback(logMgr, coordinator.Done()); subErr != nil {
				// Nothing left to fall back TO: without a log subscriber the
				// session would sit on a silent terminal until it was
				// signalled. Fail as a required-mode failure would.
				tuiErr = fmt.Errorf("TUI error: %w", errors.Join(runErr, subErr))
				fmt.Fprintf(os.Stderr, "Error: %v\n", tuiErr)
			} else {
				// This session is now, in every respect the rest of runUp cares
				// about, a plain-mode session: it waits on the coordinator
				// below and prints no post-TUI shutdown summary (its log stream
				// is on the terminal, so performShutdown's own SystemLog lines
				// are already visible there). Any drops from the brief window
				// the sink was routed at the log manager are reported here,
				// since the tuiEnabled-gated report below will no longer fire.
				reportStdioDrops(sink)
				tuiEnabled = false
			}
		}
	}
	// deadVerdict is the dead-stack watcher's verdict, latched only if it fired
	// (plan 028 C3, #96). It survives the block below to reach the exit contract.
	var deadVerdict *settleVerdict
	if !tuiEnabled {
		// Log subscription + consumer were already started above, before the
		// supervisor started any process (D2, #92) — or, for a preferred-mode
		// TUI that failed, a moment ago with the ring replayed behind it.

		// A foreground session with every process dead used to wait here
		// forever: the trigger channel is closed by signals, POST /shutdown and
		// a TUI quit, and by nothing a process does (#96). The watcher supplies
		// the missing wake — and the reason for it, which the coordinator does
		// not carry — so a stack that is entirely down ends the session and
		// exits non-zero.
		//
		// NOT in the detached daemon child: `--detach` short-circuits TUI
		// resolution to plain mode, so it runs this very block, and a watcher
		// here would kill the daemon `prox up -d` just promised was still
		// running. See dead_stack.go.
		//
		// Started here rather than beside sup.Start so that sequential startup
		// can never present a transient "nothing live" window, and so the
		// mid-run TUI-failure fallback above (which flips tuiEnabled and falls
		// into this block) is covered by the same call.
		var deadWatcher *deadStackWatcher
		if !daemon.IsDaemonChild() {
			deadWatcher = startDeadStackWatcher(sup, coordinator.TriggerWith, coordinator.TriggerCh())
		}

		// Wait for shutdown. Both a signal (via forwardShutdownSignal above, which
		// also logs it) and an API/TUI-quit trigger arrive as a coordinator trigger,
		// so this selects on the one channel.
		<-coordinator.TriggerCh()

		if deadWatcher != nil {
			// Stopped BEFORE performShutdown, not merely deferred to runUp's
			// return: teardown drives every process to stopped, and a watcher
			// still running through that would see a crashed-plus-stopped stack
			// and latch a verdict for a shutdown the user asked for. stop()
			// joins the goroutine, so the read below is of a finished decision.
			deadWatcher.stop()
			if v, fired := deadWatcher.latchedVerdict(); fired {
				deadVerdict = &v
			}
		}
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

	// TUI-only: the alt screen is down and sink.RestoreStderr() above already
	// pointed output back at the real terminal, so this is the first point after
	// the verdict lands where a human is looking at the primary screen. Gated on
	// tuiEnabled, not on outcome or drops, so plain mode's output stays exactly
	// what the integration suite already asserts byte-for-byte.
	if tuiEnabled {
		printShutdownSummary(outcome, os.Stdout, os.Stderr)
		// C2's stdio sink counts drops on the manager route this session used
		// (RouteToLogManager above); reuse reportStdioDrops rather than
		// re-deriving the same "n diagnostic line(s) were dropped" report.
		reportStdioDrops(sink)
	}

	// The dead-stack half of the exit contract (plan 028 C3, #96). Printed here,
	// after teardown, so it is the LAST thing on the terminal rather than a line
	// the shutdown log stream scrolls away — and printed through the same
	// formatter `prox up -d`, `start` and `restart` use, so a crash reads
	// identically whichever command reports it. Only ever non-nil in plain,
	// non-daemon-child mode.
	var deadStackErr error
	if deadVerdict != nil {
		deadVerdict.writeTo(os.Stderr, deadStackHint)
		deadStackErr = deadVerdict.err()
	}

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
		// errors.Join keeps a TUI failure — and a dead stack — visible alongside
		// the shutdown verdict; any one alone still makes `up` exit non-zero.
		return errors.Join(tuiErr, deadStackErr, fmt.Errorf("shutdown incomplete: %w", outcome))
	}
	// errors.Join of all-nil is nil, so an ordinary Ctrl-C still exits 0.
	return errors.Join(tuiErr, deadStackErr)
}

// dialableAPIURL builds the base URL a process uses to reach its OWN in-process
// API server (`prox up --tui`). It exists because neither half of the obvious
// answer is usable as-is:
//
//   - api.Server.Addr() reports the CONFIGURED host, which may be a wildcard.
//     0.0.0.0 and :: are bind addresses, not destinations — some stacks route
//     them to localhost, others refuse — so they are normalized to the matching
//     loopback address. Any concrete host (localhost, 127.0.0.1, a LAN IP) is
//     passed through untouched: the server is listening there, so that is exactly
//     where to dial.
//   - the port must come from the BOUND listener, not the config: with dynamic
//     allocation the two can disagree, and only the listener knows the truth.
//
// net.JoinHostPort does the joining so an IPv6 host gets its brackets ("::1"
// becomes "[::1]:9000"); naive host+":"+port produces a URL that never parses.
func dialableAPIURL(host string, port int) string {
	return "http://" + net.JoinHostPort(dialableHost(host), strconv.Itoa(port))
}

// dialableHost maps a configured bind host to a host that can be dialed. Empty
// means "all interfaces" to net.Listen exactly as 0.0.0.0 does, so both — and the
// IPv6 wildcard — collapse to their loopback equivalents.
//
// Brackets are stripped first: the config's Listen call formats "host:port" with
// sprintf, so an IPv6 host only ever BINDS in bracketed form ("[::1]"). Passing
// that bracketed form through JoinHostPort would double-bracket it into a URL
// that never parses (cursor review, C2) — so this normalizes to the bare
// address and lets JoinHostPort re-bracket.
func dialableHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	default:
		return host
	}
}

// boundAPIPort reports the port the API listener actually holds, falling back to
// the configured port. The fallback is unreachable in practice (the listener is
// always TCP) and exists only so a non-TCP listener degrades instead of panicking
// on the type assertion.
func boundAPIPort(ln net.Listener, configured int) int {
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		return tcpAddr.Port
	}
	return configured
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
		// Release any /processes/stream subscribers, for the same reason the log and
		// request managers are closed below: the no-timeout SSE routes only end when
		// their data source closes. Stage 2's sup.Stop already latched the change bus
		// closed (Supervisor.Stop defers CloseEvents on every path), so this is
		// idempotent -- it is kept explicit here so the ordering requirement (every
		// stream source closes BEFORE the stage-5 API shutdown) is visible in the
		// shutdown sequence itself.
		deps.sup.CloseEvents()
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

// printShutdownSummary tells a TUI user, on the now-restored primary screen,
// what happened during shutdown -- detail that performShutdown otherwise sends
// only into SystemLog, a log stream the alt screen has hidden for the whole
// session and that has now disappeared along with the TUI itself (plan 026 C5,
// §3.4).
//
// Call ONLY for a TUI session (runUp gates this on tuiEnabled). In plain mode
// the log stream was on the terminal the whole time and performShutdown's own
// SystemLog lines were already visible there, so a second copy here would be
// duplicate noise and would change today's byte-for-byte-asserted plain-mode
// output (test/integration/up_test.go, api_test.go).
//
// outcome is performShutdown's verdict: nil means a clean stop, printed as one
// short confirmation line so the common case stays quiet. A non-nil outcome
// names each survivor -- still true and still actionable, since the group holds
// whatever ports it bound. The per-survivor lines use the same `name: err` shape
// as `prox stop`'s failure output (commands.go) rather than repeating the
// header's "did not stop cleanly" on every row. out and errOut are threaded in
// (rather than hard-wired to os.Stdout/os.Stderr) so a unit test can capture
// both without a pty.
func printShutdownSummary(outcome *domain.ProcessStopError, out, errOut io.Writer) {
	if outcome == nil {
		fmt.Fprintln(out, "All processes stopped cleanly.")
		return
	}
	fmt.Fprintln(errOut, "Some processes did not stop cleanly (their process groups may still hold ports):")
	for _, f := range outcome.Failures {
		fmt.Fprintf(errOut, "  %s: %v\n", f.Name, f.Err)
	}
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

// resolveProxyRuntimeState answers the two questions the rest of the session
// asks about the proxy path: is a proxy actually running for this project, and
// will requests actually be captured?
//
// Both answers are RUNTIME state, not config state, and the gap between the two
// is the whole point of this function: `prox up --no-proxy` leaves the config's
// `proxy:` block enabled while refusing to start anything, so keying a UI hint
// on cfg.Proxy.Enabled alone would tell the user to wait for traffic that this
// session can never see. captureEnabled is gated on proxyConfigured for the same
// reason — capture cannot happen without a proxy to capture from.
//
// CaptureEffectivelyEnabled is nil-receiver safe, so the second expression is
// safe even with no proxy: block at all.
//
// It returns a STRUCT rather than two bools because the two are adjacent, both
// bool, and consumed at three call sites — the proxy gate, the status block and
// the TUI options. Returned positionally, swapping them at any of those sites
// compiles, passes every test (each is tested in isolation) and silently gives
// a `--no-proxy` session the wrong hint. Named fields make that mistake visible
// at the call site instead (CodeRabbit review finding).
func resolveProxyRuntimeState(cfg *config.Config, noProxy bool) proxyRuntimeFacts {
	configured := !noProxy && cfg.Proxy != nil && cfg.Proxy.Enabled
	return proxyRuntimeFacts{
		Configured:     configured,
		CaptureEnabled: configured && cfg.Proxy.CaptureEffectivelyEnabled(),
	}
}

// proxyRuntimeFacts is the resolved runtime state of this session's proxy path:
// whether a proxy is actually running, and whether requests will actually be
// captured. Both are runtime answers, not config answers.
type proxyRuntimeFacts struct {
	Configured     bool
	CaptureEnabled bool
}

// startProxy attempts to register with the shared proxy daemon. If the daemon
// cannot be reached (e.g., sandboxed environment), it falls back to starting a
// standalone proxy. Returns the daemon client (if using daemon) and/or the
// standalone proxy service (if using fallback).
//
// pre collects the "Proxy (…)" / "Registered domains" lines for the TUI. It is
// threaded IN rather than having these functions return their lines: the lines
// are produced at four points spread across two functions with three error
// paths between them, and printing them where they happen is what keeps the
// terminal output byte- and order-identical to before (preamble.printf prints
// exactly what the fmt.Printf it replaced printed, at the same moment).
func startProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers, rt *proxyRuntime, logOut io.Writer, pre *startupPreamble) (*proxyd.Client, *proxy.Service, error) {
	// Try shared daemon first
	client, ok, fatalErr := tryDaemonProxy(cfg, cwd, ctx, handlers, rt, pre)
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
	//
	// Warnings (plan 028 A2): this mode has no wire to carry them — mkcert runs
	// in THIS process (internal/proxy) — so the producer below reports straight
	// into the session's warning sink and is rendered and sealed by the same
	// end-of-startup step in runUp as the shared-daemon path.
	svc, err := startStandaloneProxy(cfg, cwd, ctx, handlers, logOut, pre)
	if err != nil {
		return nil, nil, err
	}
	rt.SetMode(proxyModeStandalone)

	// Must run AFTER the proxy started: if HTTPS was configured and mkcert
	// actually generated certs just now, that run already answered the
	// CA-trust question and the resolver returns its verdict without probing.
	//
	// Asynchronous (Go, not Add) because the fallback — a probe, when the certs
	// were already warm — shells out to mkcert, and no diagnostic gets to hold
	// up a user's startup. There is no clear-side here: a standalone session's
	// sink starts empty every run, so "trusted" simply means nothing is added.
	//
	// Gated on HTTPS: with no HTTPS listener there is no certificate to be
	// distrusted, so the warning would be true and irrelevant — which is its own
	// kind of untrue. The daemon side needs no equivalent gate: its cert phase
	// only runs for a registration that declares an HTTPS port (server.go).
	if cfg.Proxy.HTTPSPort > 0 {
		rt.WarningSink().Go(func() []domain.Warning {
			return mkcertTrustWarnings(ctx, certs.SharedTrust())
		})
	}
	return nil, svc, nil
}

// caTrustResolver is the CA-trust verdict source startProxy consumes, narrowed
// to the one method so a test can supply a verdict without a real mkcert.
type caTrustResolver interface {
	Resolve(context.Context) certs.TrustVerdict
}

// mkcertTrustWarnings turns the process's CA-trust verdict into the warnings to
// report. It adds nothing for a trusted CA and nothing for an unknown one:
// prox says something here only when mkcert itself said the CA is missing from
// the trust stores.
func mkcertTrustWarnings(ctx context.Context, r caTrustResolver) []domain.Warning {
	if w := r.Resolve(ctx).Warning; w != nil {
		return []domain.Warning{*w}
	}
	return nil
}

// tryDaemonProxy attempts to register with the shared proxy daemon.
// Returns (client, true, nil) on success, (nil, false, nil) when daemon is
// unavailable (fall back to standalone), (nil, false, error) when daemon is
// running but registration failed (don't fall back, fail the command).
func tryDaemonProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers, rt *proxyRuntime, pre *startupPreamble) (*proxyd.Client, bool, error) {
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
		pre.printf("Proxy (shared daemon): %s", strings.Join(proxyAddrs, ", "))
	}
	if len(resp.Registered) > 0 {
		pre.printf("Registered domains: %s", strings.Join(resp.Registered, ", "))
	}

	// Advisories the shared daemon raised while setting this project up (plan
	// 028 A2). They ride the register response — both arms, the first attempt
	// and the post-SHUTTING_DOWN retry (retryRegisterAfterShutdown returns the
	// healed response into `resp` above) — because the daemon's own stdout and
	// stderr are /dev/null, so anything it printed would be seen by nobody.
	//
	// They are only COLLECTED here. Rendering happens once, at the end of
	// startup on runUp's goroutine (see reportStartupWarnings), so a warning
	// from any producer prints in one place and in one order.
	rt.WarningSink().Add(resp.Warnings...)

	// Create a local RequestManager and start the SSE forwarder to bridge
	// daemon proxy requests into this project's TUI and API. The runtime records
	// the shared mode, the active client, the original register request (C6
	// re-registers with it), and the local manager (source of the dropped-events
	// count), and receives forwarder connection state as the status sink (D5).
	// A replica manager: capture lives on the daemon side, but the daemon's
	// body eviction is silent (no event reaches connected projects), so the
	// replica runs its own timestamp-ordered inline-body bound rather than
	// relying on daemon-side stripping to reach live-forwarded records; see
	// NewReplicaRequestManager.
	localRM := proxy.NewReplicaRequestManager(constants.DefaultProxyRequestBufferSize)
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
//
// logOut is where the service's slog handler writes, and it is required. It is
// threaded in rather than hard-wired to os.Stderr because proxy.Service's
// ErrorHandler logs once per FAILED UPSTREAM REQUEST — the "backend has not
// bound its port yet" window — which is mid-session output that would otherwise
// shred a TUI frame. Deliberately NOT nil-tolerant: a nil-means-os.Stderr
// default would let a future call site silently bypass the sink, which is the
// exact frame corruption this plumbing exists to prevent.
func startStandaloneProxy(cfg *config.Config, cwd string, ctx context.Context, handlers *api.Handlers, logOut io.Writer, pre *startupPreamble) (*proxy.Service, error) {
	// Fail here rather than at the first log record. slog would accept a nil
	// writer and only panic when the ErrorHandler fires — i.e. mid-session, on
	// the first failed upstream request, taking the whole daemon down long
	// after the mistake was made (codex review finding).
	if logOut == nil {
		return nil, fmt.Errorf("internal: startStandaloneProxy requires a log destination")
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))

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
		pre.printf("Proxy (standalone): %s", strings.Join(proxyAddrs, ", "))
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

// subscribeLogPrinter subscribes to logMgr and returns the entry channel. It is
// split out from printLogEntries (below) so the caller can subscribe
// SYNCHRONOUSLY, before starting anything that might emit a log line the
// subscription needs to catch (D2, #92) — subscribing inside a goroutine (the
// old shape) gives no guarantee the subscription exists before the next
// statement in the caller runs.
func subscribeLogPrinter(logMgr *logs.Manager) (<-chan domain.LogEntry, error) {
	_, ch, err := logMgr.Subscribe(domain.LogFilter{})
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// printLogEntries drains an already-open log channel (see subscribeLogPrinter)
// and prints each entry to the terminal. It is the consumer half of the
// subscribe/consume split and does no subscribing itself.
func printLogEntries(ch <-chan domain.LogEntry) {
	printer := NewLogPrinter()
	for entry := range ch {
		printer.PrintEntry(entry)
	}
}

// tuiExitKind classifies what tui.RunClient returning means for the session:
// an orderly quit, a failure of a TUI the user REQUIRED, or a failure of a TUI
// that was merely preferred (and so must degrade instead of failing).
type tuiExitKind int

const (
	tuiExitClean tuiExitKind = iota
	tuiExitFailedRequired
	tuiExitFailedPreferred
)

// classifyTUIExit maps (RunClient's error, the resolved mode) onto that
// decision. Split out of runUp so the matrix is unit-testable — the branch it
// replaces was only reachable through a full pty-driven session.
//
// The non-obvious row is a NON-NIL error that is still a clean quit. Both prox
// and bubbletea install SIGINT handlers, so an external `kill -INT` (or a
// Ctrl-C on a terminal that is not in raw mode) is a race: if bubbletea gets
// there first, Run returns an interrupt error even though the user simply
// stopped the session. Reporting that as "the TUI could not run", replaying the
// log ring over it and suppressing the shutdown summary would be a spurious,
// race-dependent failure report — so it is treated exactly like a `q` quit.
// tui.IsCleanExit owns the bubbletea identifiers behind that (verified against
// the vendored v1.3.10).
func classifyTUIExit(runErr error, mode tuiMode) tuiExitKind {
	if tui.IsCleanExit(runErr) {
		return tuiExitClean
	}
	if mode == tuiModeRequired {
		return tuiExitFailedRequired
	}
	return tuiExitFailedPreferred
}

// streamLogsAfterTUIFallback starts the terminal log stream for a session whose
// preferred-mode TUI failed and which is degrading to plain streaming (plan 026
// §3.1). It is the ONLY late subscriber in runUp.
//
// Accepted cosmetic wart: the ring it replays still contains the startup
// preamble lines, which were printed on the primary screen before the TUI
// started — so on this path the user sees them twice. Filtering them out would
// mean teaching the replay which entries the terminal has already shown, i.e.
// new state carried purely for a rare error path; the duplication is harmless
// and deliberately left alone.
//
// It has to replay the ring, not just subscribe. The early subscribe-before-
// start invariant (D2, #92) is deliberately skipped for a TUI session — nothing
// would drain that subscription and its first overflow would close it — so by
// the time the TUI has failed, every startup line and every log a process has
// already emitted is behind us. A bare late subscribe would leave the user
// looking at a terminal that says nothing about the run it just started;
// replaying what the manager still holds is what makes the late subscribe safe.
//
// Order is subscribe-THEN-query, never the reverse: an entry written between
// the two arrives twice this way (dropped below by Seq) but is lost outright
// the other way round. logs.Manager assigns Seq inside the same critical
// section it broadcasts from, so the ring and the channel can never disagree
// about which entries the replay already covered.
func streamLogsAfterTUIFallback(logMgr *logs.Manager, shutdown <-chan struct{}) error {
	printer := NewLogPrinter()
	return streamLogsAfterTUIFallbackTo(logMgr, shutdown, printer.PrintEntry)
}

// streamLogsAfterTUIFallbackTo is streamLogsAfterTUIFallback with the terminal
// write injected, so a unit test can assert the replay order and the
// de-duplication without owning a terminal (LogPrinter writes to stdout).
//
// shutdown is the coordinator's Done channel, used ONLY to tell an expected
// end-of-stream (performShutdown closes the log manager, and it closes it after
// latching the verdict) from a subscription that died early. See
// emitLogEntriesAfter.
func streamLogsAfterTUIFallbackTo(logMgr *logs.Manager, shutdown <-chan struct{}, emit func(domain.LogEntry)) error {
	ch, err := subscribeLogPrinter(logMgr)
	if err != nil {
		return fmt.Errorf("failed to subscribe to logs: %w", err)
	}
	backfill, _, err := logMgr.QueryLast(domain.LogFilter{}, logRingBufferSize)
	if err != nil {
		return fmt.Errorf("failed to read buffered logs: %w", err)
	}
	var lastSeq uint64
	if n := len(backfill); n > 0 {
		lastSeq = backfill[n-1].Seq
	}
	go func() {
		if emitLogEntriesAfter(backfill, lastSeq, ch, shutdown, emit) {
			// Loud on purpose. Everything else this function exists for is
			// diagnostics, and a diagnostic stream that has silently stopped is
			// worse than one that never started: the terminal simply goes quiet
			// and the user reads that as "nothing is happening".
			fmt.Fprintln(os.Stderr, "Warning: the log stream was dropped (subscription overflowed); this terminal will show no further logs — use `prox logs -f` to resume")
		}
	}()
	return nil
}

// emitLogEntriesAfter emits a ring replay and then the live stream, skipping
// live entries the replay already covered. A zero Seq means the entry was never
// stamped by a manager, so it cannot be a duplicate of anything in the ring and
// is always emitted.
//
// The replay and the live channel are INTERLEAVED, which is the whole point:
// emitting a 1000-entry backfill to a terminal takes long enough for a chatty
// process to fill the 1000-slot subscription buffer behind it, and
// SubscriptionManager.Broadcast closes a subscription on its FIRST overflow
// (internal/logs/subscription.go) — permanently, silently, for the rest of the
// run. So after every backfill entry we drain whatever has already arrived,
// non-blockingly, into pending. The subscription is never left unread while the
// replay is in progress. pending is bounded by how much the session logs during
// the replay; it is transient and freed as soon as the replay ends.
//
// It returns true when the live channel closed EARLY — i.e. not as part of
// shutdown. The manager's Close (performShutdown stage 4) runs after the
// coordinator's verdict is latched (stage 3), so a closed shutdown channel at
// that moment means "expected"; anything else is a dropped subscription the
// caller should say out loud.
func emitLogEntriesAfter(backfill []domain.LogEntry, lastSeq uint64, ch <-chan domain.LogEntry, shutdown <-chan struct{}, emit func(domain.LogEntry)) bool {
	live := ch
	var pending []domain.LogEntry

	// drain takes everything already buffered without ever blocking. A closed
	// channel is latched by nil-ing live: a receive on a nil channel is never
	// ready, so the select below simply falls to its default afterwards.
	drain := func() {
		for live != nil {
			select {
			case entry, ok := <-live:
				if !ok {
					live = nil
					return
				}
				pending = append(pending, entry)
			default:
				return
			}
		}
	}

	for _, entry := range backfill {
		emit(entry)
		drain()
	}

	emitAfter := func(entry domain.LogEntry) {
		if entry.Seq != 0 && entry.Seq <= lastSeq {
			return
		}
		emit(entry)
	}
	for _, entry := range pending {
		emitAfter(entry)
	}
	pending = nil

	// Either the drain above already saw the close (live is nil — ranging over
	// a nil channel would block forever, so it is skipped) or this range ends
	// on it: the function only ever returns once the live channel is closed.
	if live != nil {
		for entry := range live {
			emitAfter(entry)
		}
	}

	select {
	case <-shutdown:
		return false
	default:
		return true
	}
}
