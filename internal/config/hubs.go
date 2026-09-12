package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"gopkg.in/yaml.v3"
)

// hubsEntryAllowedKeys is the per-entry key set for a hubs: block (plan 031
// C2, D8). hubs: is keyed by user-chosen alias names -- like processes:/
// services: -- so its entries are key-checked here by parseHubs rather than
// by strictyaml.go's fixed-schema walk; see the comment at
// strictyaml.go:11-16, which draws exactly this distinction.
var hubsEntryAllowedKeys = map[string]struct{}{
	"url": {}, "token": {}, "token_file": {}, "token_env": {}, "origin": {},
}

// parseHubs strictly parses a raw hubs: block into out, mirroring
// parseDependencies/parseTasks (dependency.go): unknown keys are rejected by
// inspecting the raw map form (the re-marshal path alone would drop them
// silently), values are extracted via remarshal, and structural errors are
// returned as strings for the caller to batch and sort rather than failing
// fast. Used for both a project's prox.yaml hubs: block (config.go) and
// ~/.prox/hubs.yaml's hubs: block (parseUserHubs below) -- prefixBase lets
// each caller name its own document path in error messages.
func parseHubs(prefixBase string, raw map[string]interface{}, out map[string]HubConfig) []string {
	var errs []string
	for _, name := range sortedMapKeys(raw) {
		prefix := fmt.Sprintf("%s.%s", prefixBase, name)
		m, ok := raw[name].(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: must be a mapping", prefix))
			continue
		}
		errs = append(errs, rejectUnknownKeys(prefix, m, hubsEntryAllowedKeys)...)
		var hub HubConfig
		if err := remarshal(m, &hub); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, err))
			continue
		}
		out[name] = hub
	}
	return errs
}

// UserHubs is the parsed shape of ~/.prox/hubs.yaml (plan 031 D8, D18): a
// default alias plus the same hubs: schema a project's prox.yaml carries
// (Config.Hubs). See LoadUserHubs/SaveUserHubs/ResolveHub.
type UserHubs struct {
	Default string               `yaml:"default,omitempty"`
	Hubs    map[string]HubConfig `yaml:"hubs,omitempty"`
}

// rawUserHubs is UserHubs' raw decode shape: Hubs stays a generic map so
// parseHubs can key-check each entry precisely, exactly as rawConfig.Hubs
// does for a project's prox.yaml (plan 031 C2).
type rawUserHubs struct {
	Default string                 `yaml:"default"`
	Hubs    map[string]interface{} `yaml:"hubs,omitempty"`
}

// userHubsSchemaAllowedKeys is the fixed-schema allow-list for
// ~/.prox/hubs.yaml's single top-level path (plan 031 C2), in the shape
// checkDocumentStructureWithSchema expects. hubs.<alias> entries are
// user-named, exactly like a project's hubs: block, so they are never listed
// here -- they are key-checked by parseHubs instead.
var userHubsSchemaAllowedKeys = map[string]map[string]struct{}{
	"": {"default": {}, "hubs": {}},
}

// userHubsFileName is the per-user hubs file, always under the home
// directory's ".prox" directory -- the same directory the shared daemon uses
// (internal/proxyd.DaemonDirName), but config cannot import proxyd (that
// would be a bad dependency direction, plan 031 C2), so the path is resolved
// independently here.
const userHubsFileName = "hubs.yaml"

// userHubsPath returns ~/.prox/hubs.yaml.
func userHubsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".prox", userHubsFileName), nil
}

// LoadUserHubs reads ~/.prox/hubs.yaml (plan 031 C2, D8). A missing file is
// NOT an error -- most machines have no hubs configured -- and returns an
// empty UserHubs. The file is parsed with the same strict-YAML discipline
// prox.yaml gets: unknown keys, duplicate keys (literal and aliased), `<<`
// merge keys, and multi-document input are all rejected, with errors reported
// in deterministic sorted order.
func LoadUserHubs() (UserHubs, error) {
	path, err := userHubsPath()
	if err != nil {
		return UserHubs{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return UserHubs{Hubs: map[string]HubConfig{}}, nil
		}
		return UserHubs{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := CheckFilePermissions(path); err != nil {
		return UserHubs{}, err
	}
	hubs, err := parseUserHubs(data, path)
	if err != nil {
		return UserHubs{}, err
	}
	// The file is only a credential when it actually holds one. A hubs.yaml
	// whose entries all use token_file or token_env carries no secret and is
	// left alone at whatever mode the user likes; one with an inline token: is
	// held to the private-mode rule (plan 031 D18).
	if hubsHoldInlineToken(hubs.Hubs) {
		if err := CheckCredentialFilePermissions("hubs file", path); err != nil {
			return UserHubs{}, err
		}
	}
	return hubs, nil
}

// hubsHoldInlineToken reports whether any entry carries a token: value, i.e.
// whether the file the entries came from is itself a secret.
func hubsHoldInlineToken(hubs map[string]HubConfig) bool {
	for _, hub := range hubs {
		if hub.Token != "" {
			return true
		}
	}
	return false
}

// parseUserHubs parses the raw bytes of a hubs.yaml file, named by path for
// error messages. Split out from LoadUserHubs so tests can exercise parsing
// without touching the filesystem.
func parseUserHubs(data []byte, path string) (UserHubs, error) {
	var raw rawUserHubs
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return UserHubs{}, fmt.Errorf("%s: parsing yaml: %w", path, err)
	}

	var errs []string
	errs = append(errs, checkDocumentStructureWithSchema(data, userHubsSchemaAllowedKeys)...)

	hubs := make(map[string]HubConfig, len(raw.Hubs))
	errs = append(errs, parseHubs("hubs", raw.Hubs, hubs)...)

	// Alias names are checked HERE as well as in `prox hub add`, because this
	// file is hand-editable: an alias of "default" would be unreachable
	// (ResolveHub always reads that word as indirection into default:) and an
	// empty alias is not addressable at all (plan 031 D8).
	for _, name := range sortedMapKeys(raw.Hubs) {
		if err := ValidateHubAlias(name); err != nil {
			errs = append(errs, fmt.Sprintf("hubs.%s: %s", name, err))
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return UserHubs{}, fmt.Errorf("%w: %s: %s", domain.ErrInvalidConfig, path, strings.Join(errs, "; "))
	}

	return UserHubs{Default: raw.Default, Hubs: hubs}, nil
}

// SaveUserHubs writes ~/.prox/hubs.yaml (plan 031 D18/P8). The file is 0600
// (it can hold an inline token) and written ATOMICALLY: a temp file in the
// same directory, written, fsynced, renamed into place, then the parent
// directory is fsynced too -- unlike proxyd.WriteDaemonState's in-place
// truncate, so a crash mid-write can never leave a half-written token behind.
func SaveUserHubs(hubs UserHubs) error {
	path, err := userHubsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, constants.DirPermissionPrivate); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	data, err := yaml.Marshal(hubs)
	if err != nil {
		return fmt.Errorf("marshaling hubs: %w", err)
	}
	if err := userHubsWriter.WriteFile(path, data, constants.FilePermissionPrivate); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// userHubsWriter is the atomic writer ~/.prox/hubs.yaml is saved through
// (plan 031 D18/P8): temp file in the same directory, fsync, rename, parent-dir
// fsync -- never an in-place truncate, so a crash mid-write cannot leave a
// half-written token behind. The sequence itself lives in domain.AtomicWriter
// because internal/proxyd and internal/tui need exactly the same one and
// neither package can import this one.
//
// It is a var rather than a literal at the call site so hubs_test.go can inject
// a failure at each step and assert that a pre-rename failure leaves the
// PREVIOUS file intact -- the property "no temp file survives a successful
// write" does not actually test.
var userHubsWriter = domain.AtomicWriter{TempPattern: ".prox-hubs-*.tmp"}

// ResolvedHub is the fully-resolved hub selection for one `prox up --hub`/
// `proxy.hub` run (plan 031 D8): a usable base URL, a token value already
// read from whichever of token/token_file/token_env was set (or empty for
// `auth: none`), and an Origin defaulted from os.Hostname() when the config
// left it blank.
type ResolvedHub struct {
	Alias  string
	URL    string
	Token  string
	Origin string
}

// hubEntry is one merged hubs: entry together with WHERE it came from. The
// source directory is what a relative token_file resolves against (plan 031
// D8): a path written in ~/.prox/hubs.yaml means "next to the hubs file", and
// one written in a project's prox.yaml means "next to that prox.yaml". Neither
// means "wherever `prox up` happened to be run from", which is what resolving
// against the process working directory would have meant -- and which breaks
// the moment the daemon or the CLI is started from another directory.
type hubEntry struct {
	hub HubConfig
	// dir is the directory of the file this entry was read from, or "" when it
	// is unknown (no project config path was supplied). A relative token_file
	// under "" falls back to the process working directory, since there is
	// nothing better to resolve it against.
	dir string
}

// ResolveHub resolves alias against cfg's Hubs merged with the user's
// ~/.prox/hubs.yaml, with cfg's entries winning on an alias clash (plan 031
// D8: "a committed alias never breaks prox up elsewhere" -- a project can
// still override a machine-wide alias it does not like). The literal alias
// "default" expands to the user file's default: value, not to a hub entry
// literally named "default". cfg may be nil (equivalent to an empty Hubs
// map), so a bare `--hub <alias>` works with no project config loaded.
//
// configPath is the path cfg was loaded from, and exists so a relative
// token_file in a project's hubs: block resolves against that project file's
// own directory. Pass "" when there is no project config (or it came from
// somewhere without a path); a relative token_file then falls back to the
// process working directory.
func ResolveHub(cfg *Config, configPath, alias string) (ResolvedHub, error) {
	userHubs, err := LoadUserHubs()
	if err != nil {
		return ResolvedHub{}, err
	}
	userDir := ""
	if path, perr := userHubsPath(); perr == nil {
		userDir = filepath.Dir(path)
	}

	merged := make(map[string]hubEntry, len(userHubs.Hubs))
	for name, hub := range userHubs.Hubs {
		merged[name] = hubEntry{hub: hub, dir: userDir}
	}
	if cfg != nil {
		projectDir := ""
		if configPath != "" {
			abs, aerr := filepath.Abs(configPath)
			if aerr != nil {
				abs = configPath
			}
			projectDir = filepath.Dir(abs)
		}
		for name, hub := range cfg.Hubs {
			merged[name] = hubEntry{hub: hub, dir: projectDir} // project wins on an alias clash
		}
	}

	resolvedAlias := alias
	if alias == "default" {
		if userHubs.Default == "" {
			return ResolvedHub{}, fmt.Errorf("hub alias \"default\": no default is set in ~/.prox/hubs.yaml (run 'prox hub add <alias> <url> --default')")
		}
		resolvedAlias = userHubs.Default
	}

	entry, ok := merged[resolvedAlias]
	if !ok {
		return ResolvedHub{}, fmt.Errorf("unknown hub alias %q (defined: %s)", alias, describeHubAliases(merged))
	}
	hub := entry.hub

	token, err := resolveHubToken(hub, entry.dir)
	if err != nil {
		return ResolvedHub{}, fmt.Errorf("hub %q: %w", resolvedAlias, err)
	}

	origin := hub.Origin
	if origin == "" {
		origin, err = os.Hostname()
		if err != nil {
			return ResolvedHub{}, fmt.Errorf("hub %q: resolving local hostname for origin: %w", resolvedAlias, err)
		}
	}

	return ResolvedHub{Alias: resolvedAlias, URL: hub.URL, Token: token, Origin: origin}, nil
}

// describeHubAliases renders the sorted list of defined aliases for an
// "unknown alias" error, so the user can see what IS available.
func describeHubAliases(hubs map[string]hubEntry) string {
	if len(hubs) == 0 {
		return "none"
	}
	return strings.Join(sortedMapKeys(hubs), ", ")
}

// resolveHubToken resolves one hub entry's token under the token >
// token_file > token_env precedence (plan 031 D8, C2). At most one of the
// three may be set -- Validate already enforces this for a project's hubs:
// block, but ResolveHub also merges in ~/.prox/hubs.yaml, which Validate
// never sees, so the check is repeated here as the actual safety net. A
// missing token_file or unset token_env is an error naming which.
//
// sourceDir is the directory of the file this entry was read from, which a
// relative token_file resolves against (see hubEntry).
func resolveHubToken(hub HubConfig, sourceDir string) (string, error) {
	set := 0
	for _, v := range []string{hub.Token, hub.TokenFile, hub.TokenEnv} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		return "", fmt.Errorf("at most one of token, token_file, token_env may be set")
	}

	switch {
	case hub.Token != "":
		return hub.Token, nil
	case hub.TokenFile != "":
		path := resolveHubTokenFilePath(hub.TokenFile, sourceDir)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("token_file %s: not found", path)
			}
			return "", fmt.Errorf("token_file %s: %w", path, err)
		}
		// A token_file IS the credential, so it is held to the private-mode
		// rule ssh applies to a private key -- checked after the read succeeds
		// so a missing file still reports "not found" rather than a stat error
		// (plan 031 D18).
		if err := CheckCredentialFilePermissions("token_file", path); err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	case hub.TokenEnv != "":
		val, ok := os.LookupEnv(hub.TokenEnv)
		if !ok {
			return "", fmt.Errorf("token_env %s: environment variable is not set", hub.TokenEnv)
		}
		return val, nil
	default:
		return "", nil
	}
}

// resolveHubTokenFilePath turns an entry's token_file value into the path to
// actually read (plan 031 D8). "~"/"~/" expands to the home directory and an
// absolute path is taken as written; a RELATIVE path resolves against
// sourceDir -- the directory of the file the entry itself lives in -- so
// `token_file: llt.token` in ~/.prox/hubs.yaml means ~/.prox/llt.token no
// matter where prox is run from. `prox hub add --token-file` absolutizes its
// argument before writing, so only a hand-written path takes this branch.
func resolveHubTokenFilePath(tokenFile, sourceDir string) string {
	expanded := expandHomeTilde(tokenFile)
	if sourceDir == "" || filepath.IsAbs(expanded) {
		return expanded
	}
	return filepath.Join(sourceDir, expanded)
}

// expandHomeTilde expands a leading "~" or "~/" in path to the user's home
// directory. Any other path (including one that merely contains a "~" later
// on) is returned unchanged; a home-directory lookup failure also leaves the
// path unchanged, so the subsequent file read produces its own clear error.
func expandHomeTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
