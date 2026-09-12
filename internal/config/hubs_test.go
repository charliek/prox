package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Parse: hubs: block strict-key handling and values (plan 031 C2) -------

func TestParse_Hubs_StrictKeyErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "unknown entry key",
			yaml: `
processes: {web: ./web}
hubs:
  llt:
    url: http://hub.example:8443
    toke: abc
`,
			wantSub: `hubs.llt: unknown field "toke"`,
		},
		{
			name: "top-level typo is still caught with hubs present",
			yaml: `
processes: {web: ./web}
hubz:
  llt:
    url: http://hub.example:8443
`,
			wantSub: `config: unknown field "hubz"`,
		},
		{
			name: "duplicate alias (literal)",
			yaml: `
processes: {web: ./web}
hubs:
  llt:
    url: http://a.example:8443
  llt:
    url: http://b.example:8443
`,
			wantSub: `mapping key "llt" already defined`,
		},
		{
			name: "entry is not a mapping",
			yaml: `
processes: {web: ./web}
hubs:
  llt: http://hub.example:8443
`,
			wantSub: `hubs.llt: must be a mapping`,
		},
		{
			name: "aliased duplicate key inside an entry",
			yaml: `
processes: {web: ./web}
hubs:
  llt:
    url: http://hub.example:8443
    origin: &o mac
    <<: {origin: *o}
`,
			// The explicit origin: key wins and the merge source's origin is
			// shadowed, not duplicated -- this is legitimate YAML (the
			// defaults idiom), so it must NOT produce a duplicate-key error.
			// This case documents that expectation rather than asserting an
			// error string.
			wantSub: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.wantSub == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestParse_Hubs_Values(t *testing.T) {
	yamlDoc := `
processes: {web: ./web}
proxy:
  enabled: true
  domain: local.test.dev
  hub: llt
hubs:
  llt:
    url: http://100.120.127.126:8443
    token_env: PROX_HUB_TOKEN
    origin: mac
`
	cfg, err := Parse([]byte(yamlDoc))
	require.NoError(t, err)
	require.Contains(t, cfg.Hubs, "llt")

	hub := cfg.Hubs["llt"]
	assert.Equal(t, "http://100.120.127.126:8443", hub.URL)
	assert.Equal(t, "PROX_HUB_TOKEN", hub.TokenEnv)
	assert.Equal(t, "mac", hub.Origin)
	assert.Empty(t, hub.Token)
	assert.Empty(t, hub.TokenFile)

	require.NotNil(t, cfg.Proxy)
	assert.Equal(t, "llt", cfg.Proxy.Hub)
}

func TestParse_Hubs_AbsentIsEmptyNotNil(t *testing.T) {
	cfg, err := Parse([]byte(`processes: {web: ./web}`))
	require.NoError(t, err)
	assert.NotNil(t, cfg.Hubs)
	assert.Empty(t, cfg.Hubs)
}

// --- Validate: hubs: entries and proxy.hub (plan 031 C2) -------------------

func TestValidate_Hubs(t *testing.T) {
	baseProcesses := map[string]ProcessConfig{"web": {Cmd: "npm run dev"}}

	cases := []struct {
		name    string
		cfg     *Config
		wantErr bool
		wantSub string
	}{
		{
			name: "valid hub entry",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs: map[string]HubConfig{
					"llt": {URL: "http://100.120.127.126:8443", TokenEnv: "PROX_HUB_TOKEN"},
				},
			},
			wantErr: false,
		},
		{
			name: "missing url",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: is empty",
		},
		{
			name: "non-http scheme",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {URL: "ftp://hub.example:8443"}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: must use http:// or https://",
		},
		{
			name: "url has query string",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {URL: "http://hub.example:8443?x=1"}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: must not contain a query string",
		},
		{
			name: "url has userinfo",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {URL: "http://user:pass@hub.example:8443"}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: must not contain userinfo",
		},
		{
			name: "url has fragment",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {URL: "http://hub.example:8443#frag"}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: must not contain a fragment",
		},
		{
			name: "no host",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"llt": {URL: "http:///path"}},
			},
			wantErr: true,
			wantSub: "hubs.llt.url: has no host",
		},
		{
			name: "more than one token source",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs: map[string]HubConfig{
					"llt": {URL: "http://hub.example:8443", Token: "abc", TokenEnv: "X"},
				},
			},
			wantErr: true,
			wantSub: "hubs.llt: at most one of token, token_file, token_env may be set",
		},
		{
			name: "proxy.hub without proxy.enabled",
			cfg: &Config{
				Processes: baseProcesses,
				Proxy:     &ProxyConfig{Enabled: false, Hub: "llt"},
			},
			wantErr: true,
			wantSub: "proxy.hub: requires proxy.enabled to be true",
		},
		{
			name: "proxy.hub with proxy.enabled",
			cfg: &Config{
				Processes: baseProcesses,
				Proxy:     &ProxyConfig{Enabled: true, HTTPSPort: 443, Domain: "local.test.dev", Hub: "llt"},
				Certs:     &CertsConfig{Dir: "/tmp/certs"},
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// --- LoadUserHubs / SaveUserHubs (plan 031 C2, D18) -------------------------

func TestLoadUserHubs_MissingFileIsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	hubs, err := LoadUserHubs()
	require.NoError(t, err)
	assert.Empty(t, hubs.Default)
	assert.NotNil(t, hubs.Hubs)
	assert.Empty(t, hubs.Hubs)
}

func TestSaveUserHubs_RoundTripAndPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	t.Setenv("HOME", t.TempDir())

	want := UserHubs{
		Default: "llt",
		Hubs: map[string]HubConfig{
			"llt":  {URL: "http://100.120.127.126:8443", TokenFile: "~/.prox/hubs/llt.token"},
			"home": {URL: "https://home.example:8443", Token: "s3cr3t"},
		},
	}
	require.NoError(t, SaveUserHubs(want))

	path, err := userHubsPath()
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	got, err := LoadUserHubs()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSaveUserHubs_AtomicWriteLeavesNoTempFileBehind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	path, err := userHubsPath()
	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "temp file leaked: %s", e.Name())
	}
}

func TestLoadUserHubs_StrictParsing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := userHubsPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))

	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "unknown top-level key",
			yaml: `
default: llt
hub_default: llt
hubs:
  llt: {url: http://hub.example:8443}
`,
			wantSub: `unknown field "hub_default"`,
		},
		{
			name: "unknown entry key",
			yaml: `
hubs:
  llt: {url: http://hub.example:8443, tokn: x}
`,
			wantSub: `hubs.llt: unknown field "tokn"`,
		},
		{
			name: "multiple documents",
			yaml: `
hubs:
  llt: {url: http://hub.example:8443}
---
hubs:
  other: {url: http://other.example:8443}
`,
			wantSub: "multiple YAML documents are not supported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0600))
			_, err := LoadUserHubs()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// --- ResolveHub: merge precedence, default expansion, token resolution -----

func TestResolveHub_ProjectWinsOnAliasClash(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://user-file.example:8443", Origin: "user-file"}},
	}))

	cfg := &Config{Hubs: map[string]HubConfig{
		"llt": {URL: "http://project.example:8443", Origin: "project"},
	}}

	resolved, err := ResolveHub(cfg, "llt")
	require.NoError(t, err)
	assert.Equal(t, "http://project.example:8443", resolved.URL)
	assert.Equal(t, "project", resolved.Origin)
}

func TestResolveHub_UserFileWhenProjectAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://user-file.example:8443", Origin: "user-file"}},
	}))

	resolved, err := ResolveHub(&Config{}, "llt")
	require.NoError(t, err)
	assert.Equal(t, "http://user-file.example:8443", resolved.URL)
}

func TestResolveHub_DefaultExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Default: "llt",
		Hubs:    map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	resolved, err := ResolveHub(&Config{}, "default")
	require.NoError(t, err)
	assert.Equal(t, "llt", resolved.Alias)
	assert.Equal(t, "http://hub.example:8443", resolved.URL)
}

func TestResolveHub_DefaultUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	_, err := ResolveHub(&Config{}, "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no default is set")
}

func TestResolveHub_UnknownAliasNamesDefined(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{
			"llt":  {URL: "http://a.example:8443"},
			"home": {URL: "http://b.example:8443"},
		},
	}))

	_, err := ResolveHub(&Config{}, "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown hub alias "bogus"`)
	assert.Contains(t, err.Error(), "home")
	assert.Contains(t, err.Error(), "llt")
}

func TestResolveHub_UnknownAliasNoneDefined(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := ResolveHub(&Config{}, "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defined: none")
}

func TestResolveHub_TokenResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Run("inline token", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", Token: "inline-token"},
		}}
		resolved, err := ResolveHub(cfg, "llt")
		require.NoError(t, err)
		assert.Equal(t, "inline-token", resolved.Token)
	})

	t.Run("token file", func(t *testing.T) {
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token")
		require.NoError(t, os.WriteFile(tokenPath, []byte("file-token\n"), 0600))

		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenFile: tokenPath},
		}}
		resolved, err := ResolveHub(cfg, "llt")
		require.NoError(t, err)
		assert.Equal(t, "file-token", resolved.Token)
	})

	t.Run("token file missing", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenFile: "/no/such/file"},
		}}
		_, err := ResolveHub(cfg, "llt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token_file /no/such/file: not found")
	})

	t.Run("token env", func(t *testing.T) {
		t.Setenv("PROX_TEST_HUB_TOKEN", "env-token")
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenEnv: "PROX_TEST_HUB_TOKEN"},
		}}
		resolved, err := ResolveHub(cfg, "llt")
		require.NoError(t, err)
		assert.Equal(t, "env-token", resolved.Token)
	})

	t.Run("token env unset", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenEnv: "PROX_TEST_HUB_TOKEN_UNSET_XYZ"},
		}}
		_, err := ResolveHub(cfg, "llt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PROX_TEST_HUB_TOKEN_UNSET_XYZ: environment variable is not set")
	})

	t.Run("no token", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443"},
		}}
		resolved, err := ResolveHub(cfg, "llt")
		require.NoError(t, err)
		assert.Empty(t, resolved.Token)
	})

	t.Run("more than one set", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", Token: "a", TokenEnv: "B"},
		}}
		_, err := ResolveHub(cfg, "llt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most one of token, token_file, token_env may be set")
	})
}

func TestResolveHub_OriginDefaultsToHostname(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hostname, err := os.Hostname()
	require.NoError(t, err)

	cfg := &Config{Hubs: map[string]HubConfig{
		"llt": {URL: "http://hub.example:8443"},
	}}
	resolved, err := ResolveHub(cfg, "llt")
	require.NoError(t, err)
	assert.Equal(t, hostname, resolved.Origin)
}

func TestResolveHub_OriginExplicitOverridesHostname(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &Config{Hubs: map[string]HubConfig{
		"llt": {URL: "http://hub.example:8443", Origin: "custom-origin"},
	}}
	resolved, err := ResolveHub(cfg, "llt")
	require.NoError(t, err)
	assert.Equal(t, "custom-origin", resolved.Origin)
}
