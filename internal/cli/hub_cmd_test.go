package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/prox/internal/config"
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

func TestHubCmd_List_ShowsProjectEntriesMarked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetHubAddFlags()
	t.Cleanup(resetHubAddFlags)

	originalConfigPath := configPath
	t.Cleanup(func() { configPath = originalConfigPath })

	require.NoError(t, runHubAdd(hubAddCmd, []string{"home", "http://home.example:8443"}))

	tmpDir := t.TempDir()
	projectConfigPath := filepath.Join(tmpDir, "prox.yaml")
	require.NoError(t, os.WriteFile(projectConfigPath, []byte(`
processes: {web: ./web}
proxy: {enabled: true, domain: local.test.dev, hub: llt}
hubs:
  llt:
    url: http://100.120.127.126:8443
`), 0644))
	configPath = projectConfigPath

	stdout, _ := captureOutput(t, func() {
		require.NoError(t, runHubList(hubListCmd, nil))
	})
	assert.Contains(t, stdout, "home")
	assert.Contains(t, stdout, "llt")
	assert.Contains(t, stdout, "(project)")
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
