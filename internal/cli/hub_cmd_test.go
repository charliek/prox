package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetHubAddFlags restores the package-level flag vars hubAddCmd's Flags()
// bind to, since they persist across test invocations that call runHubAdd
// directly instead of going through cobra's flag parsing.
func resetHubAddFlags() {
	hubAddToken = ""
	hubAddTokenFile = ""
	hubAddTokenEnv = ""
	hubAddOrigin = ""
	hubAddDefault = false
}

func TestHubCmd_AddRemoveListRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	// add
	hubAddTokenEnv = "PROX_HUB_TOKEN"
	hubAddDefault = true
	require.NoError(t, runHubAdd(hubAddCmd, []string{"llt", "http://100.120.127.126:8443"}))

	userHubs, err := config.LoadUserHubs()
	require.NoError(t, err)
	require.Contains(t, userHubs.Hubs, "llt")
	assert.Equal(t, "http://100.120.127.126:8443", userHubs.Hubs["llt"].URL)
	assert.Equal(t, "PROX_HUB_TOKEN", userHubs.Hubs["llt"].TokenEnv)
	assert.Equal(t, "llt", userHubs.Default)

	// list shows it, marked default
	stdout, _ := captureOutput(t, func() {
		require.NoError(t, runHubList(hubListCmd, nil))
	})
	assert.Contains(t, stdout, "llt")
	assert.Contains(t, stdout, "100.120.127.126:8443")
	assert.Contains(t, stdout, "(default)")

	// add a second hub, not default
	resetHubAddFlags()
	require.NoError(t, runHubAdd(hubAddCmd, []string{"home", "https://home.example:8443"}))

	userHubs, err = config.LoadUserHubs()
	require.NoError(t, err)
	assert.Len(t, userHubs.Hubs, 2)
	// the first hub's default: setting is untouched by adding a second entry
	assert.Equal(t, "llt", userHubs.Default)

	// remove the first
	require.NoError(t, runHubRemove(hubRemoveCmd, []string{"llt"}))

	userHubs, err = config.LoadUserHubs()
	require.NoError(t, err)
	assert.NotContains(t, userHubs.Hubs, "llt")
	assert.Contains(t, userHubs.Hubs, "home")
	// removing the default alias clears default: too
	assert.Empty(t, userHubs.Default)

	// removing an unknown alias is an error naming what IS defined
	err = runHubRemove(hubRemoveCmd, []string{"bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown hub alias "bogus"`)
	assert.Contains(t, err.Error(), "home")
}

func TestHubCmd_Add_RejectsReservedDefaultAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	err := runHubAdd(hubAddCmd, []string{"default", "http://hub.example:8443"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `hub alias "default" is reserved`)
}

func TestHubCmd_Add_RejectsMultipleTokenSources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	hubAddToken = "abc"
	hubAddTokenEnv = "X"
	err := runHubAdd(hubAddCmd, []string{"llt", "http://hub.example:8443"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one of --token, --token-file, --token-env")
}

func TestHubCmd_Add_RejectsBadURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	err := runHubAdd(hubAddCmd, []string{"llt", "ftp://hub.example:8443"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use http:// or https://")
}

// TestHubCmd_List_MergesByAliasLikeResolveHub uses an alias defined in BOTH
// files, which is the case the listing has to get right: a project's hubs:
// entry OVERRIDES a same-named user-file entry (D8), so the alias is one hub
// with one effective URL. Printing the two definitions as separate rows showed
// a URL that will never be used and let "(default)" label the shadowed one --
// the opposite of what `prox up` would do. Disjoint aliases cannot catch that.
func TestHubCmd_List_MergesByAliasLikeResolveHub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	originalConfigPath := configPath
	t.Cleanup(func() { configPath = originalConfigPath })

	// "llt" is the user file's default AND is overridden by the project;
	// "home" exists only in the user file.
	hubAddDefault = true
	require.NoError(t, runHubAdd(hubAddCmd, []string{"llt", "http://user-file.example:8443"}))
	resetHubAddFlags()
	require.NoError(t, runHubAdd(hubAddCmd, []string{"home", "http://home.example:8443"}))

	tmpDir := t.TempDir()
	projectConfigPath := filepath.Join(tmpDir, "prox.yaml")
	require.NoError(t, os.WriteFile(projectConfigPath, []byte(`
processes: {web: ./web}
proxy: {enabled: true, domain: local.test.dev, hub: llt}
hubs:
  llt:
    url: http://project.example:8443
`), 0644))
	configPath = projectConfigPath

	stdout, _ := captureOutput(t, func() {
		require.NoError(t, runHubList(hubListCmd, nil))
	})

	assert.Contains(t, stdout, "home")
	assert.Contains(t, stdout, "http://home.example:8443")

	// One row for llt, carrying the URL ResolveHub would pick.
	var lltRows []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "llt") {
			lltRows = append(lltRows, line)
		}
	}
	require.Len(t, lltRows, 1, "an overridden alias must print once, got:\n%s", stdout)
	assert.Contains(t, lltRows[0], "http://project.example:8443")
	assert.NotContains(t, stdout, "user-file.example",
		"the shadowed user-file URL is never used and must not be shown")

	// Both notes on the surviving row, combined.
	assert.Contains(t, lltRows[0], "(default, project)")
}

// TestHubCmd_Add_AbsolutizesRelativeTokenFile: a relative --token-file is
// relative to the shell the command was typed in, but the entry is read later
// by `prox up` running somewhere else -- so the stored path must be absolute or
// it works exactly once, from the directory it was added in (plan 031 D8).
func TestHubCmd_Add_AbsolutizesRelativeTokenFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	workDir := t.TempDir()
	t.Chdir(workDir)

	hubAddTokenFile = "llt.token"
	require.NoError(t, runHubAdd(hubAddCmd, []string{"llt", "http://hub.example:8443"}))

	userHubs, err := config.LoadUserHubs()
	require.NoError(t, err)
	stored := userHubs.Hubs["llt"].TokenFile
	assert.True(t, filepath.IsAbs(stored), "a relative --token-file must be stored absolute, got %q", stored)
	// t.TempDir() can sit behind a symlink (/tmp -> /private/tmp on macOS), so
	// compare resolved paths rather than raw strings.
	wantDir, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	gotDir, err := filepath.EvalSymlinks(filepath.Dir(stored))
	require.NoError(t, err)
	assert.Equal(t, wantDir, gotDir)
	assert.Equal(t, "llt.token", filepath.Base(stored))
}

// TestHubCmd_Add_KeepsTildeTokenFileAsWritten: a "~" path is already
// machine-absolute and is what the documented examples use, so it is stored
// verbatim (ResolveHub expands it) rather than being pinned to today's home.
func TestHubCmd_Add_KeepsTildeTokenFileAsWritten(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	hubAddTokenFile = "~/.prox/hubs/llt.token"
	require.NoError(t, runHubAdd(hubAddCmd, []string{"llt", "http://hub.example:8443"}))

	userHubs, err := config.LoadUserHubs()
	require.NoError(t, err)
	assert.Equal(t, "~/.prox/hubs/llt.token", userHubs.Hubs["llt"].TokenFile)
}

func TestHubCmd_List_NoHubsConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	originalConfigPath := configPath
	t.Cleanup(func() { configPath = originalConfigPath })
	configPath = "/nonexistent/prox.yaml"

	stdout, _ := captureOutput(t, func() {
		require.NoError(t, runHubList(hubListCmd, nil))
	})
	assert.Contains(t, stdout, "No hubs configured")
}

// resetHubStartFlags restores the package-level `prox hub start` flag vars, for
// tests that drive applyHubStartFlags directly rather than through cobra.
func resetHubStartFlags() {
	hubStartDomain = ""
	hubStartListen = ""
	hubStartHTTPSPort = 0
	hubStartHTTPPort = 0
	hubStartAuth = ""
	hubStatusJSON = false
	hubTokenRotate = false
}

// TestApplyHubStartFlags_FirstStartDefaults pins plan 031 D14's first-start
// defaults: a bare `prox hub start --domain D` must produce a complete, valid
// hub.yaml with no further questions asked.
func TestApplyHubStartFlags_FirstStartDefaults(t *testing.T) {
	resetHubStartFlags()
	t.Cleanup(resetHubStartFlags)

	cfg, changed, err := applyHubStartFlags(proxyd.HubConfig{}, false, hubStartFlagSet{
		domain: "llt.example.com", domainSet: true,
	})
	require.NoError(t, err)
	assert.True(t, changed, "a first start always writes the file")
	assert.Equal(t, "llt.example.com", cfg.Domain)
	assert.Equal(t, 443, cfg.HTTPSPort)
	assert.Equal(t, 0, cfg.HTTPPort)
	assert.Equal(t, proxyd.HubAuthToken, cfg.Auth)
	assert.False(t, cfg.Autostart)

	wantListen, _ := proxyd.DefaultHubListenAddr()
	assert.Equal(t, wantListen, cfg.Listen)
}

// TestApplyHubStartFlags_FirstStartRequiresDomain: no default hostname is
// guessable, so the one flag that cannot be defaulted is required.
func TestApplyHubStartFlags_FirstStartRequiresDomain(t *testing.T) {
	resetHubStartFlags()
	t.Cleanup(resetHubStartFlags)

	_, _, err := applyHubStartFlags(proxyd.HubConfig{}, false, hubStartFlagSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--domain is required")
}

// TestApplyHubStartFlags_LaterStarts covers D14's "the file is the source of
// truth": with no flags nothing changes, and a given flag overwrites exactly
// its own key -- including an explicit --http-port 0, which must be able to
// switch the HTTP listener off rather than reading as "unset".
func TestApplyHubStartFlags_LaterStarts(t *testing.T) {
	resetHubStartFlags()
	t.Cleanup(resetHubStartFlags)

	stored := proxyd.HubConfig{
		Domain:    "llt.example.com",
		Listen:    "127.0.0.1:8443",
		HTTPSPort: 443,
		HTTPPort:  8080,
		Auth:      proxyd.HubAuthToken,
		Autostart: true,
	}

	t.Run("no flags is a no-op", func(t *testing.T) {
		cfg, changed, err := applyHubStartFlags(stored, true, hubStartFlagSet{})
		require.NoError(t, err)
		assert.False(t, changed, "no flags must not rewrite hub.yaml")
		assert.Equal(t, stored, cfg)
	})

	t.Run("a flag overwrites its own key only", func(t *testing.T) {
		cfg, changed, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			listen: "100.64.0.5:9443", listenSet: true,
		})
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, "100.64.0.5:9443", cfg.Listen)
		assert.Equal(t, stored.Domain, cfg.Domain)
		assert.Equal(t, stored.HTTPSPort, cfg.HTTPSPort)
		assert.True(t, cfg.Autostart, "autostart is not a `hub start` flag and must survive")
	})

	t.Run("an explicit zero http-port switches HTTP off", func(t *testing.T) {
		cfg, changed, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			httpPort: 0, httpPortSet: true,
		})
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, 0, cfg.HTTPPort)
	})

	t.Run("auth none is accepted", func(t *testing.T) {
		cfg, _, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			auth: proxyd.HubAuthNone, authSet: true,
		})
		require.NoError(t, err)
		assert.Equal(t, proxyd.HubAuthNone, cfg.Auth)
	})

	t.Run("a public listen address is refused", func(t *testing.T) {
		_, _, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			listen: "93.184.216.34:8443", listenSet: true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	// Plan 031 F8: a plain LAN address needs --allow-unencrypted-lan, and the
	// opt-in is a persisted key like any other, so a later start keeps it.
	t.Run("a plain LAN listen address needs the opt-in", func(t *testing.T) {
		_, _, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			listen: "10.0.0.5:9443", listenSet: true,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--allow-unencrypted-lan")

		cfg, changed, err := applyHubStartFlags(stored, true, hubStartFlagSet{
			listen: "10.0.0.5:9443", listenSet: true,
			allowLAN: true, allowLANSet: true,
		})
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, "10.0.0.5:9443", cfg.Listen)
		assert.True(t, cfg.AllowUnencryptedLAN, "the opt-in is persisted, not per-invocation")

		// And a stored opt-in keeps working with no flag at all.
		lanStored := cfg
		again, _, err := applyHubStartFlags(lanStored, true, hubStartFlagSet{})
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.5:9443", again.Listen)
	})
}

// TestHubAliasFromDomain: the paste-ready publisher line suggests the alias a
// user would have typed anyway -- the domain's first label.
func TestHubAliasFromDomain(t *testing.T) {
	assert.Equal(t, "llt", hubAliasFromDomain("llt.stridelabs.ai"))
	assert.Equal(t, "hub", hubAliasFromDomain(""))
	assert.Equal(t, "internal", hubAliasFromDomain("internal"))
}

// TestPrintHubPublisherLine_CarriesTheTokenValue pins §4.2's requirement that
// the line a user pastes on the PUBLISHER carries the token VALUE: the hub-side
// token path names a file that does not exist over there, so printing it would
// be actively misleading.
func TestPrintHubPublisherLine_CarriesTheTokenValue(t *testing.T) {
	stdout, _ := captureOutput(t, func() {
		printHubPublisherLine(proxyd.HubStatus{
			Domain: "llt.example.com", Listen: "100.82.128.123:8443", Auth: proxyd.HubAuthToken,
		}, "s3cret")
	})
	assert.Contains(t, stdout, "prox hub add llt http://100.82.128.123:8443 --token s3cret --default")
	assert.Contains(t, stdout, "secret")
	assert.NotContains(t, stdout, "hub.token")

	// With auth: none there is no token to carry, and the line says so by
	// simply not having one.
	stdout, _ = captureOutput(t, func() {
		printHubPublisherLine(proxyd.HubStatus{
			Domain: "llt.example.com", Listen: "100.82.128.123:8443", Auth: proxyd.HubAuthNone,
		}, "")
	})
	assert.Contains(t, stdout, "prox hub add llt http://100.82.128.123:8443 --default")
	assert.NotContains(t, stdout, "--token")
}
