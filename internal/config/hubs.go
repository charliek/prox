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
	return parseUserHubs(data, path)
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
	if err := atomicWriteFile(path, data, constants.FilePermissionPrivate); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file created in path's own
// directory, fsynced and renamed into place, with the parent directory
// fsynced afterward so the rename itself is durable (plan 031 D18/P8). Never
// used for an in-place truncate: on any failure before the rename, path is
// left completely untouched.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".prox-hubs-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup: a no-op once the rename below succeeds, since
	// nothing named tmpPath exists anymore.
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}

	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening directory %s for sync: %w", dir, err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, err)
	}
	return nil
}

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

// ResolveHub resolves alias against cfg's Hubs merged with the user's
// ~/.prox/hubs.yaml, with cfg's entries winning on an alias clash (plan 031
// D8: "a committed alias never breaks prox up elsewhere" -- a project can
// still override a machine-wide alias it does not like). The literal alias
// "default" expands to the user file's default: value, not to a hub entry
// literally named "default". cfg may be nil (equivalent to an empty Hubs
// map), so a bare `--hub <alias>` works with no project config loaded.
func ResolveHub(cfg *Config, alias string) (ResolvedHub, error) {
	userHubs, err := LoadUserHubs()
	if err != nil {
		return ResolvedHub{}, err
	}

	merged := make(map[string]HubConfig, len(userHubs.Hubs))
	for name, hub := range userHubs.Hubs {
		merged[name] = hub
	}
	if cfg != nil {
		for name, hub := range cfg.Hubs {
			merged[name] = hub // project wins on an alias clash
		}
	}

	resolvedAlias := alias
	if alias == "default" {
		if userHubs.Default == "" {
			return ResolvedHub{}, fmt.Errorf("hub alias \"default\": no default is set in ~/.prox/hubs.yaml (run 'prox hub add <alias> <url> --default')")
		}
		resolvedAlias = userHubs.Default
	}

	hub, ok := merged[resolvedAlias]
	if !ok {
		return ResolvedHub{}, fmt.Errorf("unknown hub alias %q (defined: %s)", alias, describeHubAliases(merged))
	}

	token, err := resolveHubToken(hub)
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
func describeHubAliases(hubs map[string]HubConfig) string {
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
func resolveHubToken(hub HubConfig) (string, error) {
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
		path := expandHomeTilde(hub.TokenFile)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("token_file %s: not found", hub.TokenFile)
			}
			return "", fmt.Errorf("token_file %s: %w", hub.TokenFile, err)
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
