package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charliek/prox/internal/config"
	"github.com/spf13/cobra"
)

// Hub command flags (plan 031 C2). Only add|remove|list exist in this
// commit; start|stop|status|token arrive in C3 and are deliberately not
// stubbed here.
var (
	hubAddToken     string
	hubAddTokenFile string
	hubAddTokenEnv  string
	hubAddOrigin    string
	hubAddDefault   bool
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

func init() {
	rootCmd.AddCommand(hubCmd)

	hubCmd.AddCommand(hubAddCmd)
	hubCmd.AddCommand(hubRemoveCmd)
	hubCmd.AddCommand(hubListCmd)

	hubAddCmd.Flags().StringVar(&hubAddToken, "token", "", "Inline bearer token (written into hubs.yaml -- prefer --token-file or --token-env)")
	hubAddCmd.Flags().StringVar(&hubAddTokenFile, "token-file", "", "Path to a file containing the bearer token")
	hubAddCmd.Flags().StringVar(&hubAddTokenEnv, "token-env", "", "Environment variable that holds the bearer token")
	hubAddCmd.Flags().StringVar(&hubAddOrigin, "origin", "", "Override this machine's origin name (default: hostname)")
	hubAddCmd.Flags().BoolVar(&hubAddDefault, "default", false, "Set this alias as the default hub")
}
