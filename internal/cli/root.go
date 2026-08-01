package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/version"
	"github.com/spf13/cobra"
)

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
  - HTTP/HTTPS reverse proxy with hostname routing, shared across projects
  - Proxied request inspection (prox requests) with captured bodies
  - Interactive TUI for monitoring
  - Background daemon mode`,
	Version:           version.Version,
	SilenceUsage:      true,
	SilenceErrors:     true,
	PersistentPreRunE: rootPersistentPreRunE,
}

// rootPersistentPreRunE resolves apiAddr before any command runs. It is a
// standalone function (rather than an inline closure) so tests can drive it
// directly against a synthetic *cobra.Command without going through the full
// rootCmd tree.
//
// An explicit --addr (cmd.Flags().Changed("addr")) always wins and skips
// discovery, including the discovery error below. For commands in the
// clientCommands allowlist, a discovery failure is returned as an error —
// there is no silent fallback to constants.DefaultAPIAddress (D3). Commands
// outside the allowlist (version, up, proxy, completion, __complete, help)
// never call discoverAPIAddress and so are unaffected by missing state.
func rootPersistentPreRunE(cmd *cobra.Command, args []string) error {
	// Recompute (assign, don't just latch) on every invocation: the global is
	// also read by commands.go, and a stale true from a prior in-process
	// invocation (tests drive the hook repeatedly) would permanently suppress
	// discovery and its error (CodeRabbit PR #68).
	apiAddrExplicitlySet = cmd.Flags().Changed("addr")

	// For client commands, try to discover API address if not explicitly set.
	if needsAPIDiscovery(cmd) && !apiAddrExplicitlySet {
		addr, err := discoverAPIAddress()
		if err != nil {
			return err
		}
		apiAddr = addr
	}
	return nil
}

// needsAPIDiscovery reports whether cmd is a TOP-LEVEL client command that
// talks to the project daemon API via apiAddr. The allowlist is matched by
// name AND parent: nested subcommands can share a name with a top-level
// client command (`prox proxy status`/`prox proxy stop` vs `prox status`/
// `prox stop`) but talk to the proxy daemon's Unix socket, not apiAddr —
// matching on bare cmd.Name() would wrongly demand (and now fail) discovery
// for them outside a project directory.
func needsAPIDiscovery(cmd *cobra.Command) bool {
	return clientCommands[cmd.Name()] && cmd.Parent() == cmd.Root()
}

// clientCommands is the discovery allowlist: the commands that talk to the
// daemon API via apiAddr and therefore need the address discovered from
// .prox/prox.state when --addr is not given. Every
// command whose handler calls NewClient with apiAddr MUST be listed here, or
// it silently talks to the :5555 default and breaks against dynamic-port
// daemons (the default) — the exact bug 'start' had (#41) and 'requests' had
// (#43). TestClientCommandsDiscoveryAllowlist pins this contract.
var clientCommands = map[string]bool{
	"status":   true,
	"logs":     true,
	"stop":     true,
	"start":    true,
	"restart":  true,
	"down":     true,
	"attach":   true,
	"requests": true,
}

// Execute runs the root command.
// If the process is the proxy daemon child, it runs the daemon instead of
// normal CLI dispatch.
func Execute() {
	if proxyd.IsDaemonProcess() {
		if err := proxyd.RunDaemon(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Proxy daemon error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

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
		fmt.Printf("prox version %s\n", version.Version)
	},
}

func init() {
	// Persistent flags available to all subcommands
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", constants.DefaultConfigFile, "Config file")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "addr", constants.DefaultAPIAddress, "API address for remote commands (used only when passed explicitly, which also skips the project-ownership check; otherwise discovered from .prox/prox.state)")
	rootCmd.PersistentFlags().BoolVarP(&detach, "detach", "d", false, "Run in background (daemon mode)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Set version template
	rootCmd.SetVersionTemplate("prox version {{.Version}}\n")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
}

// errNoRunningInstance is returned by discoverAPIAddress when this directory
// has no .prox/prox.state and the caller did not pass --addr explicitly. There
// is deliberately no silent fallback to constants.DefaultAPIAddress here (see
// D3 in the hardening plan): dialing the compiled-in :5555 default against a
// dynamic-port daemon silently talks to nothing, or worse, to an unrelated
// daemon.
//
// The config file is deliberately NOT a second discovery source (plan 020 C3).
// `prox up` writes .prox/prox.state (a FATAL step) strictly before it binds the
// API listener, so a listening daemon always has a state file; the old
// api.port-from-prox.yaml fallback could therefore only ever fire when the
// state file had been deleted out from under a running prox — a case --addr
// already answers — and it was inert for dynamic ports (the default) anyway.
// Removing it also removes the only route by which the CLIENT's own --config
// resolution could feed the ownership decision below.
var errNoRunningInstance = fmt.Errorf(
	"no running prox instance found in this directory (no .prox/prox.state); run from the project directory, or pass --addr",
)

// discoverAPIAddress resolves the API address from this directory's
// .prox/prox.state -- the single authoritative discovery source -- and then
// VERIFIES that the prox answering there belongs to this project before
// returning it (plan 020 C3).
//
// The verification is not paranoia: two projects that pin the same api.port,
// or one stale state file left by a dead daemon whose port has since been
// reused, previously made `prox status` report another project's processes and
// `prox down` stop another project's daemon. Confirmed destructive.
//
// It returns an error whenever it cannot positively establish ownership;
// callers must not fall back to constants.DefaultAPIAddress implicitly.
func discoverAPIAddress() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine the current directory to find a running prox: %w", err)
	}

	state, err := daemon.LoadState(cwd)
	if err != nil {
		return "", errNoRunningInstance
	}

	// net.JoinHostPort, not fmt.Sprintf("%s:%d"): an IPv6 host (api.host: "::1")
	// must be bracketed or the URL parses as garbage ("http://::1:5552").
	hostPort := net.JoinHostPort(state.Host, strconv.Itoa(state.Port))
	addr := "http://" + hostPort

	if err := verifyProjectOwnership(addr, hostPort, cwd, state); err != nil {
		return "", err
	}
	return addr, nil
}

// verifyProjectOwnership probes GET /status at addr and decides whether the
// responder is this project's prox. Identity is the PROJECT DIRECTORY, compared
// with os.SameFile.
//
// Why the project directory and not the config file: two project roots can
// legitimately share one config (`prox up -c ../shared/prox.yaml`), and a
// config-path comparison would then declare each the owner of the other -- the
// exact cross-project control this function exists to prevent. It would also
// falsely refuse in the common cases where the two paths merely differ in FORM:
// a symlinked project root, `-c $(pwd)/prox.yaml` vs a bare `prox status`,
// prox.yml vs prox.yaml.
//
// Why os.SameFile and not string comparison: SameFile compares device+inode, so
// /tmp vs /private/tmp on Darwin, a symlinked root, and every other path-form
// difference collapse in one call. String comparison would falsely refuse for
// all of them -- and a false refusal here locks a user out of their own project.
//
// Failure policy is per-cause rather than blanket fail-closed; see the branches
// below. In particular an auth failure PASSES THROUGH: the probe and the real
// request that follows share one Client and one token, so a 401 here guarantees
// a 401 there, and refusing would only replace a precise UNAUTHORIZED with a
// vague "could not verify owner".
func verifyProjectOwnership(addr, hostPort, cwd string, state *daemon.State) error {
	ctx, cancel := context.WithTimeout(context.Background(), constants.OwnershipProbeTimeout)
	defer cancel()

	status, err := NewClient(addr).GetStatusWithContext(ctx)
	if err != nil {
		return classifyOwnershipProbeFailure(err, hostPort, state)
	}

	// Require a POSITIVE prox marker. json.Unmarshal ignores unknown fields and
	// tolerates missing ones, so an unrelated service answering `{}` (or any
	// other JSON object) decodes cleanly into an all-zero StatusResponse.
	// api_version is non-omitempty in the wire format, so every real prox sends
	// it and its absence means "not a prox".
	if status.APIVersion == "" {
		return errNotAProx(hostPort)
	}

	// Authoritative check.
	if status.ProjectDir != "" {
		if samePath(status.ProjectDir, cwd) {
			return nil
		}
		// A reported project directory that no longer EXISTS proves nothing:
		// the overwhelmingly likely cause is that this very project was renamed
		// or moved while its daemon kept running (the state file travels with
		// the directory, so it is still discovered here, but the daemon still
		// reports the path it started in). samePath would then fall back to a
		// string comparison and refuse -- locking the owner out of their own
		// running daemon, including out of `prox down`.
		//
		// So: absent evidence, do not refuse. This mirrors the rule one branch
		// below for a config file deleted out from under a live prox. A genuine
		// FOREIGN project is a live project, and a live project's directory
		// exists -- which is exactly the case samePath already decided above.
		if _, err := os.Stat(status.ProjectDir); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr,
				"Warning: the prox on %s reports project directory %s, which no longer exists (renamed or moved?); cannot verify it belongs to this project\n",
				hostPort, status.ProjectDir)
			return nil
		}
		return errOwnedByAnotherProject(
			fmt.Sprintf("A prox for %s is listening on %s.", status.ProjectDir, hostPort),
			"Run commands from that directory, or target it deliberately with", addr)
	}

	// Older daemon with no project_dir: compare config paths. BOTH sides here
	// were written by daemons (the responder's own -c, and the -c recorded in
	// the state file THIS directory's daemon wrote), never by this client's
	// --config resolution, so a form divergence between them is impossible and
	// the comparison cannot false-refuse on `-c`/symlink/prox.yml differences.
	if status.ConfigFile != "" && state.ConfigFile != "" {
		if samePath(status.ConfigFile, state.ConfigFile) {
			return nil
		}
		return errOwnedByAnotherProject(
			fmt.Sprintf("A prox using config %s is listening on %s.", status.ConfigFile, hostPort),
			"Run commands from that project's directory, or target it deliberately with", addr)
	}

	// Neither identity field: an older daemon that reports nothing to match on.
	// ALLOW, loudly. Refusing here would lock a user out of a daemon they could
	// then no longer stop -- exactly mid-upgrade, when it is least welcome.
	fmt.Fprintf(os.Stderr,
		"Warning: the prox on %s reports no project identity (older version); cannot verify it belongs to this project\n",
		hostPort)
	return nil
}

// classifyOwnershipProbeFailure maps a failed /status probe to a policy. The
// causes are deliberately NOT collapsed into one blanket refusal: which of them
// occurred changes both what is safe to do and what the user must be told.
func classifyOwnershipProbeFailure(err error, hostPort string, state *daemon.State) error {
	// Answered with an HTTP error status.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden {
			// PASS THROUGH, so the real request produces the authoritative auth
			// error instead of a vague "could not verify owner". Auth is off for
			// loopback binds, so this only fires when the user enabled auth or
			// bound a non-local host -- typically with a ~/.prox/token another
			// project's `prox up` overwrote (that file is one global slot).
			//
			// Safety rests on the real request carrying the SAME credentials, so
			// a rejection here means a rejection there too. Note it is the same
			// token, not the same *Client: the probe and the command each build
			// their own client, both reading that one token file moments apart
			// (codex review finding -- the original comment overclaimed).
			//
			// Residual, accepted: a 401 body carries no api_version, so this
			// branch cannot confirm the responder is a prox at all. Acting on an
			// unverified responder is bounded -- reaching a destructive outcome
			// would need an unrelated service on exactly this port that both
			// 401s /api/v1/status AND implements a prox lifecycle endpoint
			// compatibly. Anything short of that fails the real request too.
			return nil
		}
		// Any other status: something is there, but it does not behave like a
		// prox's /status.
		return errNotAProx(hostPort)
	}

	// Accepted the connection but never (fully) answered: a wedged or
	// blackholed listener. FAIL CLOSED -- we learned nothing about who owns it.
	if isProbeTimeout(err) {
		return fmt.Errorf(
			"could not verify which project owns %s (timed out).\nTarget it deliberately with\n  --addr http://%s",
			hostPort, hostPort)
	}

	// Answered, but with something that is not a status payload.
	if isDecodeFailure(err) {
		return errNotAProx(hostPort)
	}

	// Nothing is listening (connection refused, no route, ...). The state file
	// is stale: its daemon is gone and nothing took the port. This is the only
	// branch that reports the ordinary "not running" outcome, and it does so
	// instead of surfacing a raw dial error.
	if isUnreachable(err) {
		hint := ""
		if state.PID > 0 && !daemon.IsProcessAlive(state.PID, 0) {
			// A liveness HINT only: State carries no start-time token, so this
			// cannot detect PID reuse and is never used to skip the probe.
			hint = fmt.Sprintf(" (pid %d is gone)", state.PID)
		}
		return fmt.Errorf(
			"no running prox instance found in this directory: .prox/prox.state points at %s%s, but nothing is listening there.\nStart it with 'prox up', or pass --addr to target a prox elsewhere",
			hostPort, hint)
	}

	// Unclassifiable: fail closed rather than act on an unverified daemon.
	return fmt.Errorf(
		"could not verify which project owns %s: %w.\nTarget it deliberately with\n  --addr http://%s",
		hostPort, err, hostPort)
}

// errNotAProx is deliberately distinct from the "no running prox instance"
// wording: telling a user their prox is not running, when in fact an unrelated
// service has taken the port, sends them off debugging the wrong thing.
func errNotAProx(hostPort string) error {
	return fmt.Errorf(
		"something is listening on %s but it is not a prox for this project.\n.prox/prox.state is stale or another service took the port; stop that service and run 'prox up', or pass --addr to target a prox elsewhere",
		hostPort)
}

// errOwnedByAnotherProject names the owner AND a copy-pasteable escape hatch.
// Naming the owner is the point: "prox is not running" alone, while another
// project's daemon sits on the recorded port, is actively misleading.
func errOwnedByAnotherProject(ownerLine, escapeLead, addr string) error {
	return fmt.Errorf("prox is not running for this project.\n%s\n%s\n  --addr %s", ownerLine, escapeLead, addr)
}

// samePath reports whether a and b name the same filesystem object, by
// device+inode. It falls back to comparing cleaned absolute paths only when a
// Stat fails -- a config file deleted while its prox still runs must not make
// that healthy daemon uncontrollable.
func samePath(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return cleanAbs(a) == cleanAbs(b)
}

func cleanAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// isProbeTimeout reports whether err ended the probe at its deadline rather
// than by a refused/failed connection.
func isProbeTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isDecodeFailure reports whether err came from parsing the response body
// rather than from reaching the server. Checked BEFORE isUnreachable because a
// body that is empty or truncated surfaces as io.EOF / io.ErrUnexpectedEOF,
// which must read as "responded, but not a prox", not "nothing is listening".
func isDecodeFailure(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) ||
		errors.As(err, &typeErr) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// isUnreachable reports whether err came from the transport (dial refused, DNS,
// reset, ...). http.Client.Do wraps every such failure in *url.Error.
func isUnreachable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// getProcessNames returns the names that are valid targets for start/stop/restart
// completion. It includes both processes and tasks (plan 013 D5): a task is a
// run-to-completion child that `prox start`/`stop`/`restart` operate on exactly
// like a process, so its name must complete too.
func getProcessNames() []string {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(cfg.Processes)+len(cfg.Tasks))
	for name := range cfg.Processes {
		names = append(names, name)
	}
	for name := range cfg.Tasks {
		names = append(names, name)
	}
	return names
}
