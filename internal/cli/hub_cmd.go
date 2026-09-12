package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/spf13/cobra"
)

// Hub command flags. add|remove|list manage the PUBLISHER's connection
// profiles (plan 031 C2); start|stop|status|token run the hub HOST's own
// control plane (C3, §4.2).
var (
	hubAddToken     string
	hubAddTokenFile string
	hubAddTokenEnv  string
	hubAddOrigin    string
	hubAddDefault   bool

	hubStartDomain    string
	hubStartListen    string
	hubStartHTTPSPort int
	hubStartHTTPPort  int
	hubStartAuth      string
	hubStatusJSON     bool
	hubTokenRotate    bool
)

// hubCmd is the parent command for managing remote-proxy-hub connection
// profiles in ~/.prox/hubs.yaml, in the style of proxyCmd (proxy_cmd.go).
var hubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Manage remote proxy hub connections",
	Long: `Manage the remote proxy hubs this machine can publish through.

A hub is a shared proxy daemon running on another machine (D8, plan 031):
'prox up --hub <alias>' registers this project's services with it and holds
a reverse tunnel, so a hub-published hostname works from any device that can
reach the hub.

Connection profiles live in ~/.prox/hubs.yaml. A project's prox.yaml may also
carry its own hubs: block, which takes precedence over a same-named alias in
the user file.

Examples:
  prox hub add llt http://100.120.127.126:8443 --token-file ~/.prox/hubs/llt.token --default
  prox hub list
  prox hub remove llt`,
}

// hubAddCmd writes one connection profile to ~/.prox/hubs.yaml.
var hubAddCmd = &cobra.Command{
	Use:   "add <alias> <url>",
	Short: "Add or replace a hub connection profile",
	Args:  cobra.ExactArgs(2),
	RunE:  runHubAdd,
}

func runHubAdd(cmd *cobra.Command, args []string) error {
	alias, hubURL := args[0], args[1]

	// "default" is reserved: proxy.hub/--hub use it to mean "the user file's
	// default: value", never a hub literally aliased "default" (plan 031 D8).
	if alias == "default" {
		return fmt.Errorf("hub alias %q is reserved (it selects ~/.prox/hubs.yaml's default: hub); choose another alias", alias)
	}

	tokenSources := 0
	for _, v := range []string{hubAddToken, hubAddTokenFile, hubAddTokenEnv} {
		if v != "" {
			tokenSources++
		}
	}
	if tokenSources > 1 {
		return fmt.Errorf("at most one of --token, --token-file, --token-env may be set")
	}

	if err := config.ValidateHubURL(hubURL); err != nil {
		return fmt.Errorf("url: %w", err)
	}

	userHubs, err := config.LoadUserHubs()
	if err != nil {
		return err
	}
	if userHubs.Hubs == nil {
		userHubs.Hubs = map[string]config.HubConfig{}
	}
	userHubs.Hubs[alias] = config.HubConfig{
		URL:       hubURL,
		Token:     hubAddToken,
		TokenFile: hubAddTokenFile,
		TokenEnv:  hubAddTokenEnv,
		Origin:    hubAddOrigin,
	}
	if hubAddDefault {
		userHubs.Default = alias
	}

	if err := config.SaveUserHubs(userHubs); err != nil {
		return fmt.Errorf("saving hubs file: %w", err)
	}

	fmt.Printf("Added hub %q (%s)\n", alias, hubURL)
	if hubAddDefault {
		fmt.Printf("Set %q as the default hub\n", alias)
	}
	return nil
}

// hubRemoveCmd deletes one connection profile from ~/.prox/hubs.yaml.
var hubRemoveCmd = &cobra.Command{
	Use:   "remove <alias>",
	Short: "Remove a hub connection profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubRemove,
}

func runHubRemove(cmd *cobra.Command, args []string) error {
	alias := args[0]

	userHubs, err := config.LoadUserHubs()
	if err != nil {
		return err
	}
	if _, ok := userHubs.Hubs[alias]; !ok {
		return fmt.Errorf("unknown hub alias %q (defined: %s)", alias, describeHubAliasesForCLI(userHubs.Hubs))
	}

	delete(userHubs.Hubs, alias)
	if userHubs.Default == alias {
		userHubs.Default = ""
	}

	if err := config.SaveUserHubs(userHubs); err != nil {
		return fmt.Errorf("saving hubs file: %w", err)
	}

	fmt.Printf("Removed hub %q\n", alias)
	return nil
}

// hubListCmd lists ~/.prox/hubs.yaml's entries, plus a project's own hubs:
// block (marked "(project)") when run in a directory with a readable
// prox.yaml.
var hubListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured hubs",
	Args:  cobra.NoArgs,
	RunE:  runHubList,
}

// hubListRow is one rendered row of `prox hub list`.
type hubListRow struct {
	alias  string
	url    string
	origin string
	note   string
}

func runHubList(cmd *cobra.Command, args []string) error {
	userHubs, err := config.LoadUserHubs()
	if err != nil {
		return err
	}

	var rows []hubListRow
	for _, alias := range sortedHubAliases(userHubs.Hubs) {
		hub := userHubs.Hubs[alias]
		note := ""
		if alias == userHubs.Default {
			note = "(default)"
		}
		rows = append(rows, hubListRow{alias: alias, url: hub.URL, origin: hub.Origin, note: note})
	}

	// A project's own hubs: block is shown alongside the user file's, marked
	// "(project)", when the current directory has one (§4.2). No config file,
	// or one that fails to load, simply means nothing project-scoped to add
	// -- `prox hub list` still reports the user file's entries.
	if cfg, err := config.Load(configPath); err == nil {
		for _, alias := range sortedHubAliases(cfg.Hubs) {
			hub := cfg.Hubs[alias]
			rows = append(rows, hubListRow{alias: alias, url: hub.URL, origin: hub.Origin, note: "(project)"})
		}
	}

	if len(rows) == 0 {
		fmt.Println("No hubs configured")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tURL\tORIGIN\t")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.alias, r.url, r.origin, r.note)
	}
	w.Flush()
	return nil
}

// sortedHubAliases returns a hubs map's keys sorted, for deterministic
// `prox hub list` output.
func sortedHubAliases(hubs map[string]config.HubConfig) []string {
	names := make([]string, 0, len(hubs))
	for name := range hubs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// describeHubAliasesForCLI renders the sorted list of defined aliases for a
// `prox hub remove` error naming what IS defined, mirroring
// config.describeHubAliases (unexported there, so re-implemented for CLI
// error text).
func describeHubAliasesForCLI(hubs map[string]config.HubConfig) string {
	if len(hubs) == 0 {
		return "none"
	}
	return strings.Join(sortedHubAliases(hubs), ", ")
}

// --- hub HOST commands (plan 031 C3, §4.2) ---

// hubStartCmd turns hub mode on in this machine's shared proxy daemon.
var hubStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Turn on hub mode in the shared proxy daemon",
	Long: `Turn on hub mode so other machines can publish through this daemon.

Flags given here are persisted to ~/.prox/hub.yaml and become the defaults for
later runs; the file is the source of truth. --domain is required the first
time (no default is guessable) and optional afterwards.

The listen address must be a loopback, private (RFC 1918), tailnet (100.64/10),
or IPv6 unique-local address -- never 0.0.0.0 or a public address. Use
'--listen <host>:0' to bind an ephemeral port; the port actually bound is
printed.

Examples:
  prox hub start --domain llt.example.com
  prox hub start --listen 100.82.128.123:8443
  prox hub start --auth none`,
	Args: cobra.NoArgs,
	RunE: runHubStart,
}

// hubStartFlagSet records which `prox hub start` flags the user actually gave,
// separately from their values: an unset flag must leave hub.yaml's stored
// value alone, while an explicit `--http-port 0` must be able to switch the
// HTTP listener off (D14). cobra's Changed() supplies the booleans.
type hubStartFlagSet struct {
	domain    string
	listen    string
	auth      string
	httpsPort int
	httpPort  int

	domainSet    bool
	listenSet    bool
	authSet      bool
	httpsPortSet bool
	httpPortSet  bool
}

// applyHubStartFlags folds the given flags onto the stored config and returns
// the config to persist, whether anything changed, and any error (plan 031
// D14). exists is false on a FIRST start, which is where the documented
// defaults come from: listen = the machine's tailnet address, https_port 443,
// http_port 0 (off), auth token, autostart false -- and where --domain becomes
// mandatory. Pure, so the defaulting rules are table-testable without a daemon.
func applyHubStartFlags(cfg proxyd.HubConfig, exists bool, f hubStartFlagSet) (proxyd.HubConfig, bool, error) {
	changed := !exists
	if !exists {
		listen, tailnet := proxyd.DefaultHubListenAddr()
		cfg = proxyd.HubConfig{
			Listen:    listen,
			HTTPSPort: 443,
			HTTPPort:  0,
			Auth:      proxyd.HubAuthToken,
			Autostart: false,
		}
		_ = tailnet // the caller warns; the config is the same either way
		if !f.domainSet || f.domain == "" {
			return proxyd.HubConfig{}, false, fmt.Errorf("--domain is required the first time hub mode is started (it names the hostnames this hub publishes, e.g. --domain llt.example.com)")
		}
	}

	set := func(cond bool, apply func()) {
		if cond {
			apply()
			changed = true
		}
	}
	set(f.domainSet, func() { cfg.Domain = f.domain })
	set(f.listenSet, func() { cfg.Listen = f.listen })
	set(f.authSet, func() { cfg.Auth = f.auth })
	set(f.httpsPortSet, func() { cfg.HTTPSPort = f.httpsPort })
	set(f.httpPortSet, func() { cfg.HTTPPort = f.httpPort })

	normalized, err := proxyd.NormalizeHubConfig(cfg)
	if err != nil {
		return proxyd.HubConfig{}, false, err
	}
	// Normalization can itself fill in a value (a defaulted auth mode, a listen
	// address that gained the default port), which must be persisted too.
	if normalized.Listen != cfg.Listen || normalized.Auth != cfg.Auth {
		changed = true
	}
	return normalized, changed, nil
}

func runHubStart(cmd *cobra.Command, args []string) error {
	cfg, err := proxyd.LoadHubConfig()
	exists := true
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		exists = false
		cfg = proxyd.HubConfig{}
	}

	flags := hubStartFlagSet{
		domain:    hubStartDomain,
		listen:    hubStartListen,
		auth:      hubStartAuth,
		httpsPort: hubStartHTTPSPort,
		httpPort:  hubStartHTTPPort,
	}
	if cmd != nil && cmd.Flags() != nil {
		flags.domainSet = cmd.Flags().Changed("domain")
		flags.listenSet = cmd.Flags().Changed("listen")
		flags.authSet = cmd.Flags().Changed("auth")
		flags.httpsPortSet = cmd.Flags().Changed("https-port")
		flags.httpPortSet = cmd.Flags().Changed("http-port")
	}

	next, changed, err := applyHubStartFlags(cfg, exists, flags)
	if err != nil {
		return err
	}
	if changed {
		if err := proxyd.SaveHubConfig(next); err != nil {
			return fmt.Errorf("saving %s: %w", proxyd.HubConfigPath(), err)
		}
	}

	// Generate the token BEFORE asking the daemon to start, so this command can
	// print the value for the paste-ready publisher line below. The daemon reads
	// the same file, so there is only ever one token.
	token := ""
	if next.Auth == proxyd.HubAuthToken {
		token, err = proxyd.EnsureHubToken()
		if err != nil {
			return err
		}
	}

	client, err := proxyd.EnsureRunning()
	if err != nil {
		return fmt.Errorf("starting the shared proxy daemon: %w", err)
	}
	status, err := client.HubStart()
	if err != nil {
		return fmt.Errorf("starting hub mode: %w", err)
	}

	printHubStatus(*status)
	if !exists {
		if _, tailnet := proxyd.DefaultHubListenAddr(); !tailnet && !flags.listenSet {
			fmt.Println()
			fmt.Println("Note: no tailnet (100.64/10) address was found, so the hub is bound to loopback")
			fmt.Println("      and only this machine can publish through it. Re-run with --listen <private ip>:8443.")
		}
	}
	fmt.Println()
	printHubPublisherLine(*status, token)
	return nil
}

// hubStopCmd turns hub mode off again.
var hubStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Turn off hub mode",
	Long: `Turn off hub mode: close the network control plane and drop every remote
registration it was serving. The daemon keeps running if local projects are
still registered, and otherwise exits after the usual grace.`,
	Args: cobra.NoArgs,
	RunE: runHubStop,
}

func runHubStop(cmd *cobra.Command, args []string) error {
	client, err := connectDaemon()
	if err != nil {
		return err
	}
	if _, err := client.HubStop(); err != nil {
		return fmt.Errorf("stopping hub mode: %w", err)
	}
	fmt.Println("Hub mode: off")
	return nil
}

// hubHostStatusCmd reports the hub HOST's view: on/off, where it listens, and
// who is publishing through it (plan 031 D20 -- distinct from the publisher's
// own `Hub:` line in `prox status`).
var hubHostStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show hub mode status for this machine",
	Args:  cobra.NoArgs,
	RunE:  runHubHostStatus,
}

func runHubHostStatus(cmd *cobra.Command, args []string) error {
	client, err := connectDaemon()
	if err != nil {
		return err
	}
	status, err := client.HubStatus()
	if err != nil {
		return fmt.Errorf("failed to get hub status: %w", err)
	}
	if hubStatusJSON {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	printHubStatus(*status)
	return nil
}

// hubTokenCmd prints (and optionally rotates) the hub's bearer token.
var hubTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Show or rotate the hub's bearer token",
	Long: `Show the hub's bearer token and the file it lives in.

--rotate writes a new token and tells a running hub to accept only it, with no
grace period: connections already established stay up, but the next call
carrying the old token is rejected. Every publisher must be updated.`,
	Args: cobra.NoArgs,
	RunE: runHubToken,
}

func runHubToken(cmd *cobra.Command, args []string) error {
	var token string
	var err error

	if hubTokenRotate {
		// Rotate through the daemon when one is running, so the live hub starts
		// accepting the new token in the same step (D18). With no daemon, rotate
		// the file directly -- the next `prox hub start` reads it.
		if client, cerr := connectDaemon(); cerr == nil {
			var resp *proxyd.HubTokenResponse
			resp, err = client.HubRotateToken()
			if err == nil {
				token = resp.Token
			}
		} else {
			token, err = proxyd.RotateHubToken()
		}
		if err != nil {
			return fmt.Errorf("rotating hub token: %w", err)
		}
	} else {
		token, err = proxyd.ReadHubToken()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("Token file: %s (not created yet -- run 'prox hub start')\n", proxyd.HubTokenPath())
				return nil
			}
			return err
		}
	}

	fmt.Printf("Token file: %s\n", proxyd.HubTokenPath())
	fmt.Printf("Token:      %s\n", token)
	if hubTokenRotate {
		fmt.Println("Rotated: the previous token is rejected from now on; update every publisher.")
	}
	// The paste-ready line needs the hub's address, which only the config knows.
	if cfg, cerr := proxyd.LoadHubConfig(); cerr == nil {
		fmt.Println()
		printHubPublisherLine(proxyd.HubStatus{Domain: cfg.Domain, Listen: cfg.Listen, Auth: cfg.Auth}, token)
	}
	return nil
}

// printHubStatus renders the hub HOST's status block.
func printHubStatus(status proxyd.HubStatus) {
	if !status.Enabled {
		fmt.Println("Hub mode: off")
		return
	}
	fmt.Println("Hub mode: on")
	fmt.Printf("  Domain:     %s\n", status.Domain)
	fmt.Printf("  Listen:     %s\n", status.Listen)
	fmt.Printf("  HTTPS port: %s\n", formatHubPort(status.HTTPSPort))
	fmt.Printf("  HTTP port:  %s\n", formatHubPort(status.HTTPPort))
	fmt.Printf("  Auth:       %s\n", status.Auth)
	if status.Auth == proxyd.HubAuthToken {
		fmt.Printf("  Token file: %s\n", proxyd.HubTokenPath())
	}

	if len(status.Publishers) == 0 {
		fmt.Println("  Publishers: none")
		return
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORIGIN\tPROJECT\tROUTES\tCONNECTED\tREGISTERED")
	fmt.Fprintln(w, "------\t-------\t------\t---------\t----------")
	for _, p := range status.Publishers {
		connected := "no"
		if p.Connected {
			connected = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			p.Origin, p.ProjectDir, p.Routes, connected,
			p.RegisteredAt.Format("15:04:05"))
	}
	w.Flush()
}

// formatHubPort renders a data-plane port, naming 0 as "off" rather than
// printing a port number that does not exist.
func formatHubPort(port int) string {
	if port <= 0 {
		return "off (0)"
	}
	return strconv.Itoa(port)
}

// printHubPublisherLine prints the paste-ready `prox hub add` command to run on
// a PUBLISHER machine (plan 031 §4.2).
//
// It carries the token VALUE, not the hub-side token path: ~/.prox/hub.token is
// a file on THIS machine and names nothing on the publisher, so printing the
// path there would be actively misleading. The line is therefore a secret, and
// says so.
func printHubPublisherLine(status proxyd.HubStatus, token string) {
	alias := hubAliasFromDomain(status.Domain)
	url := "http://" + status.Listen
	if status.Auth == proxyd.HubAuthNone || token == "" {
		fmt.Println("Run this on each publisher machine:")
		fmt.Printf("  prox hub add %s %s --default\n", alias, url)
		return
	}
	fmt.Println("Run this on each publisher machine (it contains the token VALUE -- treat the")
	fmt.Println("line as a secret; the token file above exists only on this machine):")
	fmt.Printf("  prox hub add %s %s --token %s --default\n", alias, url, token)
}

// hubAliasFromDomain suggests a publisher-side alias from the hub's domain: the
// first label ("llt" for llt.stridelabs.ai), which is what a user would have
// typed anyway.
func hubAliasFromDomain(domain string) string {
	if domain == "" {
		return "hub"
	}
	if i := strings.Index(domain, "."); i > 0 {
		return domain[:i]
	}
	return domain
}

func init() {
	rootCmd.AddCommand(hubCmd)

	hubCmd.AddCommand(hubAddCmd)
	hubCmd.AddCommand(hubRemoveCmd)
	hubCmd.AddCommand(hubListCmd)
	hubCmd.AddCommand(hubStartCmd)
	hubCmd.AddCommand(hubStopCmd)
	hubCmd.AddCommand(hubHostStatusCmd)
	hubCmd.AddCommand(hubTokenCmd)

	hubStartCmd.Flags().StringVar(&hubStartDomain, "domain", "", "Domain remote hostnames are published under (<service>.<domain>)")
	hubStartCmd.Flags().StringVar(&hubStartListen, "listen", "", "Control-plane address (private/tailnet IP; host:0 binds an ephemeral port)")
	hubStartCmd.Flags().IntVar(&hubStartHTTPSPort, "https-port", 0, "Data-plane HTTPS port remote routes are published on")
	hubStartCmd.Flags().IntVar(&hubStartHTTPPort, "http-port", 0, "Data-plane HTTP port remote routes are published on (0 = off)")
	hubStartCmd.Flags().StringVar(&hubStartAuth, "auth", "", "Control-plane auth: token (default) or none")
	hubHostStatusCmd.Flags().BoolVar(&hubStatusJSON, "json", false, "Output as JSON")
	hubTokenCmd.Flags().BoolVar(&hubTokenRotate, "rotate", false, "Write a new token and make the running hub accept only it")

	hubAddCmd.Flags().StringVar(&hubAddToken, "token", "", "Inline bearer token (written into hubs.yaml -- prefer --token-file or --token-env)")
	hubAddCmd.Flags().StringVar(&hubAddTokenFile, "token-file", "", "Path to a file containing the bearer token")
	hubAddCmd.Flags().StringVar(&hubAddTokenEnv, "token-env", "", "Environment variable that holds the bearer token")
	hubAddCmd.Flags().StringVar(&hubAddOrigin, "origin", "", "Override this machine's origin name (default: hostname)")
	hubAddCmd.Flags().BoolVar(&hubAddDefault, "default", false, "Set this alias as the default hub")
}
