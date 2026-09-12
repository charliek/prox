package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charliek/prox/internal/domain"
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
			// The reserved-alias rule is not just `prox hub add`'s: a committed
			// hubs: block can define "default" too, and that entry would be
			// unreachable because ResolveHub reads the word as indirection
			// (plan 031 D8).
			name: "alias default is reserved",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"default": {URL: "http://hub.example:8443"}},
			},
			wantErr: true,
			wantSub: `hubs.default: hub alias "default" is reserved`,
		},
		{
			name: "empty alias",
			cfg: &Config{
				Processes: baseProcesses,
				Hubs:      map[string]HubConfig{"": {URL: "http://hub.example:8443"}},
			},
			wantErr: true,
			wantSub: "hub alias is empty",
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

// TestSaveUserHubs_AtomicWrite pins what "written atomically" actually
// promises (plan 031 D18/P8): a failure at ANY step before the rename leaves
// the previous file byte-identical, and a failure after it means the new file
// is in place but its durability is unconfirmed. Asserting only that a
// SUCCESSFUL save leaves no temp file behind would pass against a plain
// os.WriteFile, which is exactly the writer this must not be.
//
// Failures are injected through userHubsWriter's syscall seams rather than by
// filling a disk.
func TestSaveUserHubs_AtomicWrite(t *testing.T) {
	boom := errors.New("injected failure")

	// seedOldFile points HOME at a fresh dir and saves a hubs file there,
	// returning its path and exact bytes -- the "previous file" every failure
	// case must leave untouched.
	seedOldFile := func(t *testing.T) (string, []byte) {
		t.Setenv("HOME", t.TempDir())
		require.NoError(t, SaveUserHubs(UserHubs{
			Default: "old",
			Hubs:    map[string]HubConfig{"old": {URL: "http://old.example:8443"}},
		}))
		path, err := userHubsPath()
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		return path, data
	}

	injectWriter := func(t *testing.T, mutate func(w *domain.AtomicWriter)) {
		prev := userHubsWriter
		next := prev
		mutate(&next)
		userHubsWriter = next
		t.Cleanup(func() { userHubsWriter = prev })
	}

	assertNoTempFiles := func(t *testing.T, path string) {
		entries, err := os.ReadDir(filepath.Dir(path))
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), ".tmp", "temp file leaked: %s", e.Name())
		}
	}

	newHubs := UserHubs{
		Default: "new",
		Hubs:    map[string]HubConfig{"new": {URL: "http://new.example:8443"}},
	}

	t.Run("a successful save replaces the file and leaves no temp file", func(t *testing.T) {
		path, old := seedOldFile(t)
		require.NoError(t, SaveUserHubs(newHubs))

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotEqual(t, string(old), string(data))
		assert.Contains(t, string(data), "new.example")
		assertNoTempFiles(t, path)
	})

	preRename := []struct {
		name   string
		mutate func(w *domain.AtomicWriter)
	}{
		{"write fails", func(w *domain.AtomicWriter) {
			w.WriteFn = func(*os.File, []byte) (int, error) { return 0, boom }
		}},
		{"temp-file sync fails", func(w *domain.AtomicWriter) {
			w.SyncFn = func(*os.File) error { return boom }
		}},
		{"close fails", func(w *domain.AtomicWriter) {
			w.CloseFn = func(f *os.File) error {
				_ = f.Close() // still release the descriptor
				return boom
			}
		}},
		{"rename fails", func(w *domain.AtomicWriter) {
			w.RenameFn = func(string, string) error { return boom }
		}},
	}

	for _, tc := range preRename {
		t.Run(tc.name+" leaves the old file intact", func(t *testing.T) {
			path, old := seedOldFile(t)
			injectWriter(t, tc.mutate)

			err := SaveUserHubs(newHubs)
			require.Error(t, err)
			assert.ErrorIs(t, err, boom)

			data, readErr := os.ReadFile(path)
			require.NoError(t, readErr, "the previous file must still exist")
			assert.Equal(t, string(old), string(data),
				"a failure before the rename must not modify the target at all")

			var writeErr *domain.AtomicWriteError
			require.ErrorAs(t, err, &writeErr)
			assert.False(t, writeErr.Renamed, "the rename must not be reported as done")

			assertNoTempFiles(t, path)
		})
	}

	t.Run("a post-rename sync failure keeps the NEW file", func(t *testing.T) {
		path, old := seedOldFile(t)
		// Syncs run in order: temp file, then (post-rename) the target and its
		// directory. Failing from the second on exercises the post-rename half.
		calls := 0
		injectWriter(t, func(w *domain.AtomicWriter) {
			w.SyncFn = func(f *os.File) error {
				calls++
				if calls >= 2 {
					return boom
				}
				return f.Sync()
			}
		})

		err := SaveUserHubs(newHubs)
		require.Error(t, err)

		var writeErr *domain.AtomicWriteError
		require.ErrorAs(t, err, &writeErr)
		assert.True(t, writeErr.Renamed,
			"the rename already happened: this is a durability failure, not a lost write")

		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.NotEqual(t, string(old), string(data))
		assert.Contains(t, string(data), "new.example")
		assertNoTempFiles(t, path)
	})
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

	resolved, err := ResolveHub(cfg, "", "llt")
	require.NoError(t, err)
	assert.Equal(t, "http://project.example:8443", resolved.URL)
	assert.Equal(t, "project", resolved.Origin)
}

func TestResolveHub_UserFileWhenProjectAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://user-file.example:8443", Origin: "user-file"}},
	}))

	resolved, err := ResolveHub(&Config{}, "", "llt")
	require.NoError(t, err)
	assert.Equal(t, "http://user-file.example:8443", resolved.URL)
}

func TestResolveHub_DefaultExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Default: "llt",
		Hubs:    map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	resolved, err := ResolveHub(&Config{}, "", "default")
	require.NoError(t, err)
	assert.Equal(t, "llt", resolved.Alias)
	assert.Equal(t, "http://hub.example:8443", resolved.URL)
}

func TestResolveHub_DefaultUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	_, err := ResolveHub(&Config{}, "", "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no default is set")
}

// TestResolveHub_DefaultPointingAtAMissingAliasNamesTheResolvedOne is plan 031
// D8's diagnostic rule: when `default:` names an alias nobody defines, the
// failure must name THAT alias. Reporting "default" sends the user to add a
// reserved name that would never be consulted.
func TestResolveHub_DefaultPointingAtAMissingAliasNamesTheResolvedOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs:    map[string]HubConfig{"home": {URL: "http://b.example:8443"}},
		Default: "llt",
	}))

	_, err := ResolveHub(&Config{}, "", "default")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnknownHub, "callers still split on the sentinel")

	var unknown *UnknownHubError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, "llt", unknown.Alias, "the alias that is actually missing")
	assert.Equal(t, "default", unknown.Requested)
	assert.Equal(t, "prox hub add llt <url>", unknown.AddCommand())

	assert.Contains(t, err.Error(), `unknown hub alias "llt"`)
	assert.Contains(t, err.Error(), "home", "and it still lists what IS defined")
}

// TestResolveHub_DefaultUnsetCarriesNoAlias: "default" with no default set has
// no resolved alias to name, so the remediation is about the default key.
func TestResolveHub_DefaultUnsetCarriesNoAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{"llt": {URL: "http://hub.example:8443"}},
	}))

	_, err := ResolveHub(&Config{}, "", "default")
	require.Error(t, err)
	var unknown *UnknownHubError
	require.ErrorAs(t, err, &unknown)
	assert.Empty(t, unknown.Alias)
	assert.Equal(t, "prox hub add <alias> <url> --default", unknown.AddCommand())
}

func TestResolveHub_UnknownAliasNamesDefined(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, SaveUserHubs(UserHubs{
		Hubs: map[string]HubConfig{
			"llt":  {URL: "http://a.example:8443"},
			"home": {URL: "http://b.example:8443"},
		},
	}))

	_, err := ResolveHub(&Config{}, "", "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown hub alias "bogus"`)
	assert.Contains(t, err.Error(), "home")
	assert.Contains(t, err.Error(), "llt")
}

func TestResolveHub_UnknownAliasNoneDefined(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := ResolveHub(&Config{}, "", "bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defined: none")
}

func TestResolveHub_TokenResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Run("inline token", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", Token: "inline-token"},
		}}
		resolved, err := ResolveHub(cfg, "", "llt")
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
		resolved, err := ResolveHub(cfg, "", "llt")
		require.NoError(t, err)
		assert.Equal(t, "file-token", resolved.Token)
	})

	t.Run("token file missing", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenFile: "/no/such/file"},
		}}
		_, err := ResolveHub(cfg, "", "llt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token_file /no/such/file: not found")
	})

	t.Run("token env", func(t *testing.T) {
		t.Setenv("PROX_TEST_HUB_TOKEN", "env-token")
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenEnv: "PROX_TEST_HUB_TOKEN"},
		}}
		resolved, err := ResolveHub(cfg, "", "llt")
		require.NoError(t, err)
		assert.Equal(t, "env-token", resolved.Token)
	})

	t.Run("token env unset", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenEnv: "PROX_TEST_HUB_TOKEN_UNSET_XYZ"},
		}}
		_, err := ResolveHub(cfg, "", "llt")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PROX_TEST_HUB_TOKEN_UNSET_XYZ: environment variable is not set")
	})

	t.Run("no token", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443"},
		}}
		resolved, err := ResolveHub(cfg, "", "llt")
		require.NoError(t, err)
		assert.Empty(t, resolved.Token)
	})

	t.Run("more than one set", func(t *testing.T) {
		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", Token: "a", TokenEnv: "B"},
		}}
		_, err := ResolveHub(cfg, "", "llt")
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
	resolved, err := ResolveHub(cfg, "", "llt")
	require.NoError(t, err)
	assert.Equal(t, hostname, resolved.Origin)
}

func TestResolveHub_OriginExplicitOverridesHostname(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &Config{Hubs: map[string]HubConfig{
		"llt": {URL: "http://hub.example:8443", Origin: "custom-origin"},
	}}
	resolved, err := ResolveHub(cfg, "", "llt")
	require.NoError(t, err)
	assert.Equal(t, "custom-origin", resolved.Origin)
}

// --- token_file path resolution and credential permissions (plan 031) ------

// TestResolveHub_RelativeTokenFileResolvesAgainstItsOwnFile pins that a
// hand-written relative token_file means "next to the file the entry is
// written in", not "next to wherever prox happened to be started". Resolving
// against the process working directory is what makes an entry work in the
// shell it was added from and nowhere else.
func TestResolveHub_RelativeTokenFileResolvesAgainstItsOwnFile(t *testing.T) {
	t.Run("user hubs file entry resolves against ~/.prox", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, SaveUserHubs(UserHubs{
			Hubs: map[string]HubConfig{
				"llt": {URL: "http://hub.example:8443", TokenFile: "llt.token"},
			},
		}))
		// The token sits next to hubs.yaml; nothing named llt.token exists in
		// the test's working directory, so a CWD-relative read would fail.
		require.NoError(t, os.WriteFile(filepath.Join(home, ".prox", "llt.token"), []byte("user-file-token\n"), 0600))

		resolved, err := ResolveHub(nil, "", "llt")
		require.NoError(t, err)
		assert.Equal(t, "user-file-token", resolved.Token)
	})

	t.Run("project entry resolves against the project config's directory", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		projectDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "llt.token"), []byte("project-token\n"), 0600))

		cfg := &Config{Hubs: map[string]HubConfig{
			"llt": {URL: "http://hub.example:8443", TokenFile: "llt.token"},
		}}
		resolved, err := ResolveHub(cfg, filepath.Join(projectDir, "prox.yaml"), "llt")
		require.NoError(t, err)
		assert.Equal(t, "project-token", resolved.Token)
	})
}

// TestResolveHub_TokenFileMustBePrivate: a token_file IS the credential, so it
// is held to ssh's private-key rule rather than CheckFilePermissions' merely
// "not world-writable", which accepts a 0644 token every local user can read.
func TestResolveHub_TokenFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "llt.token")
	cfg := &Config{Hubs: map[string]HubConfig{
		"llt": {URL: "http://hub.example:8443", TokenFile: tokenPath},
	}}

	require.NoError(t, os.WriteFile(tokenPath, []byte("s3cr3t\n"), 0644))
	_, err := ResolveHub(cfg, "", "llt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be readable by other users")
	assert.Contains(t, err.Error(), "chmod 600 "+tokenPath)

	require.NoError(t, os.Chmod(tokenPath, 0600))
	resolved, err := ResolveHub(cfg, "", "llt")
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", resolved.Token)
}

// TestLoadUserHubs_InlineTokenRequiresAPrivateFile: the hubs file is only a
// credential when an entry actually carries a token: value. One that does is
// held to the private-mode rule; one that does not is left alone, so a
// token-less hubs.yaml at 0644 keeps working.
func TestLoadUserHubs_InlineTokenRequiresAPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}

	write := func(t *testing.T, body string, perm os.FileMode) string {
		t.Setenv("HOME", t.TempDir())
		path, err := userHubsPath()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(body), perm))
		require.NoError(t, os.Chmod(path, perm)) // umask must not soften the case
		return path
	}

	const withToken = "hubs:\n  llt: {url: 'http://hub.example:8443', token: s3cr3t}\n"
	const withoutToken = "hubs:\n  llt: {url: 'http://hub.example:8443', token_env: PROX_HUB_TOKEN}\n"

	t.Run("world-readable file holding an inline token is refused", func(t *testing.T) {
		path := write(t, withToken, 0644)
		_, err := LoadUserHubs()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be readable by other users")
		assert.Contains(t, err.Error(), "chmod 600 "+path)
	})

	t.Run("0600 file holding an inline token is accepted", func(t *testing.T) {
		write(t, withToken, 0600)
		hubs, err := LoadUserHubs()
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t", hubs.Hubs["llt"].Token)
	})

	t.Run("a token-less file is not second-guessed about its mode", func(t *testing.T) {
		write(t, withoutToken, 0644)
		hubs, err := LoadUserHubs()
		require.NoError(t, err)
		assert.Equal(t, "PROX_HUB_TOKEN", hubs.Hubs["llt"].TokenEnv)
	})
}

// TestLoadUserHubs_RejectsUnusableAliases: the reserved-alias rule has to hold
// for a hand-edited hubs.yaml too, not only for `prox hub add` (plan 031 D8).
func TestLoadUserHubs_RejectsUnusableAliases(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "alias default is unreachable",
			yaml:    "hubs:\n  default: {url: 'http://hub.example:8443'}\n",
			wantSub: `hubs.default: hub alias "default" is reserved`,
		},
		{
			name:    "empty alias",
			yaml:    "hubs:\n  '': {url: 'http://hub.example:8443'}\n",
			wantSub: "hub alias is empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseUserHubs([]byte(tc.yaml), "/home/u/.prox/hubs.yaml")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}
