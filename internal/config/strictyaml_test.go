package config

import (
	"testing"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- yaml.Node structural pass (plan 016 W2) -------------------------------

// TestParse_StructuralWalk_Errors covers the two jobs of the yaml.Node pass:
// unknown keys in the five fixed-schema blocks (including keys smuggled in via
// a `<<` merge) and aliased duplicate keys anywhere in the document.
func TestParse_StructuralWalk_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "top-level typo",
			yaml: `
processes: {web: ./web}
porxy:
  enabled: true
`,
			wantSub: `config: unknown field "porxy"`,
		},
		{
			// The #80 upgrade case: capture redaction was removed, so a config
			// still carrying `redact: true` (or redact_headers/redact_bodies)
			// must now fail at load naming the stale key, not silently ignore it.
			name: "stale redact key under proxy.capture",
			yaml: `
processes: {web: ./web}
proxy:
  enabled: true
  domain: local.test.dev
  capture:
    enabled: true
    redact: true
`,
			wantSub: `proxy.capture: unknown field "redact"`,
		},
		{
			name: "proxy typo",
			yaml: `
processes: {web: ./web}
proxy:
  enabled: true
  domain: local.test.dev
  htp_port: 8080
`,
			wantSub: `proxy: unknown field "htp_port"`,
		},
		{
			name: "api typo",
			yaml: `
processes: {web: ./web}
api:
  prot: 9000
`,
			wantSub: `api: unknown field "prot"`,
		},
		{
			name: "certs typo",
			yaml: `
processes: {web: ./web}
certs:
  dirr: ./certs
`,
			wantSub: `certs: unknown field "dirr"`,
		},
		{
			// A merge cannot be used to smuggle a key past the destination's
			// schema: the merged-in keys are checked against proxy's own set.
			name: "unknown field merged into proxy via an alias",
			yaml: `
processes: {web: ./web}
api: &shared
  port: 9000
proxy:
  <<: *shared
  enabled: true
  domain: local.test.dev
`,
			wantSub: `proxy: unknown field "port"`,
		},
		{
			// A merge does not just import scalars: a whole schema BLOCK
			// arriving through a top-level merge is validated at its
			// destination path like any other proxy: block.
			name: "nested schema block smuggled in via a top-level merge",
			yaml: `
processes: {web: ./web}
<<: {proxy: {enabled: true, domain: local.test.dev, htp_port: 8080}}
`,
			wantSub: `proxy: unknown field "htp_port"`,
		},
		{
			name: "capture block smuggled in via a proxy merge",
			yaml: `
processes: {web: ./web}
proxy:
  enabled: true
  domain: local.test.dev
  <<: {capture: {enabled: true, redact: true}}
`,
			wantSub: `proxy.capture: unknown field "redact"`,
		},
		{
			// The merge source is a mapping in its own right, so its OWN
			// aliased duplicate key is reported -- at the destination path,
			// where the silent collapse is felt.
			name: "alias-key duplicate inside a merge source",
			yaml: `
processes:
  web:
    <<: {cmd: &k cmd, *k : ./other}
`,
			wantSub: `processes.web: duplicate key "cmd"`,
		},
		{
			// A QUOTED "<<" is an ordinary string key, not a merge token.
			name: "quoted << key under proxy",
			yaml: `
processes: {web: ./web}
proxy:
  enabled: true
  domain: local.test.dev
  "<<": 1
`,
			wantSub: `proxy: unknown field "<<"`,
		},
		{
			// The core gap: the generic map form collapses the aliased key onto
			// the literal one (last write wins) with no complaint from yaml.v3.
			name: "alias-key duplicate inside a process entry",
			yaml: `
processes:
  web:
    cmd: ./web
    env_file: &k cmd
    *k : ./other
`,
			wantSub: `processes.web: duplicate key "cmd"`,
		},
		{
			name: "alias-key duplicate at the top level",
			yaml: `
processes: {web: ./web}
env_file: &k porxy
porxy: 1
*k : 2
`,
			wantSub: `config: duplicate key "porxy"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidConfig)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestParse_MergesAndAliases_Valid is the regression guard for spec-valid YAML:
// none of these are duplicates or unknown fields, and all of them must still
// parse exactly as YAML defines them.
func TestParse_MergesAndAliases_Valid(t *testing.T) {
	t.Run("merge with an explicit override under a process", func(t *testing.T) {
		cfg, err := Parse([]byte(`
processes:
  base: &base
    cmd: ./base
    stop_timeout: 30s
  web:
    <<: *base
    cmd: ./web
`))
		require.NoError(t, err)
		assert.Equal(t, "./web", cfg.Processes["web"].Cmd, "explicit key wins over the merge")
		assert.Equal(t, "30s", cfg.Processes["web"].StopTimeout, "merged key survives")
	})

	t.Run("merge into proxy with only allowed keys", func(t *testing.T) {
		cfg, err := Parse([]byte(`
processes: {web: ./web}
proxy:
  <<: {enabled: true, http_port: 8080}
  domain: local.test.dev
`))
		require.NoError(t, err)
		require.NotNil(t, cfg.Proxy)
		assert.True(t, cfg.Proxy.Enabled)
		assert.Equal(t, 8080, cfg.Proxy.HTTPPort)
	})

	t.Run("multi-source merge with overlapping keys", func(t *testing.T) {
		cfg, err := Parse([]byte(`
processes:
  base: &base
    cmd: ./base
    stop_timeout: 30s
  alt: &alt
    cmd: ./alt
    stop_timeout: 45s
  web:
    <<: [*base, *alt]
`))
		require.NoError(t, err)
		assert.Equal(t, "./base", cfg.Processes["web"].Cmd, "first merge source wins")
		assert.Equal(t, "30s", cfg.Processes["web"].StopTimeout)
	})

	t.Run("benign anchors and aliases on values", func(t *testing.T) {
		cfg, err := Parse([]byte(`
processes:
  web:
    cmd: ./web
    env: &common
      FOO: bar
  worker:
    cmd: ./worker
    env: *common
`))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"FOO": "bar"}, cfg.Processes["web"].Env)
		assert.Equal(t, map[string]string{"FOO": "bar"}, cfg.Processes["worker"].Env)
	})
}

// TestParse_ProcessTypo_ReportedExactlyOnce pins that a typo'd process field is
// reported by the process parser ALONE: the structural walk descends into
// processes: for duplicate keys but must never key-check user-named entries.
func TestParse_ProcessTypo_ReportedExactlyOnce(t *testing.T) {
	_, err := Parse([]byte(`
processes:
  web:
    cmd: ./web
    stop_timout: 5s
`))
	require.Error(t, err)
	assert.Equal(t, `invalid configuration: processes.web: unknown field "stop_timout"`, err.Error())
}

// TestCheckDocumentStructure_TagDistinctKeys pins that duplicate identity is
// tag-aware: `"1"` (!!str) and an alias resolving to `1` (!!int) are DISTINCT
// keys per the YAML spec and must NOT be reported. The walk is exercised
// directly here because such a mapping cannot survive the typed decode Parse
// does around it (a non-string key never coerces into map[string]string).
func TestCheckDocumentStructure_TagDistinctKeys(t *testing.T) {
	distinct := `
processes:
  web:
    cmd: ./web
    env:
      n: &n 1
      "1": one
      *n : int-one
`
	assert.Empty(t, checkDocumentStructure([]byte(distinct)))

	// Same shape, but the anchor is a string: now the keys really do collide.
	same := `
processes:
  web:
    cmd: ./web
    env:
      n: &n "1"
      "1": one
      *n : also-one
`
	assert.Equal(t, []string{`processes.web.env: duplicate key "1"`}, checkDocumentStructure([]byte(same)))

	// Identity is canonical, not spelling-based: `01` and `1` are the same
	// !!int key even though the raw scalars differ.
	canonical := `
processes:
  web:
    cmd: ./web
    env:
      n: &n 01
      1: one
      *n : int-one
`
	assert.Equal(t, []string{`processes.web.env: duplicate key "1"`}, checkDocumentStructure([]byte(canonical)))
}

// TestParse_WalkAndEntryErrors_BatchedTogether pins that a structural-walk
// error and a process-entry error from the C1 parsers land in ONE sorted,
// deterministic report rather than shadowing each other.
func TestParse_WalkAndEntryErrors_BatchedTogether(t *testing.T) {
	_, err := Parse([]byte(`
porxy:
  enabled: true
processes:
  web:
    cmd: ./web
    stop_timout: 5s
`))
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidConfig)
	assert.Equal(t,
		`invalid configuration: config: unknown field "porxy"; `+
			`processes.web: unknown field "stop_timout"`,
		err.Error())
}

// TestParse_YAMLLevelDefects pins the defects yaml.v3 itself rejects at the raw
// decode (the W0 ordering contract): they surface as `parsing yaml:` errors,
// NOT as a structural batch, and none of them panics or hangs.
func TestParse_YAMLLevelDefects(t *testing.T) {
	t.Run("literal duplicate key canary", func(t *testing.T) {
		_, err := Parse([]byte("processes:\n  web: ./a\n  web: ./b\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing yaml:")
		assert.Contains(t, err.Error(), `mapping key "web" already defined`)
		assert.NotErrorIs(t, err, domain.ErrInvalidConfig)
	})

	t.Run("alias to a mapping used as a key", func(t *testing.T) {
		_, err := Parse([]byte(`
api: &m
  port: 9000
processes:
  *m : ./web
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing yaml:")
		assert.NotErrorIs(t, err, domain.ErrInvalidConfig)
	})

	t.Run("alias-key duplicate of a known typed field", func(t *testing.T) {
		// A typed struct notices an aliased duplicate of a field it knows and
		// errors at the raw decode, before the walk ever runs.
		_, err := Parse([]byte(`
processes: {web: ./web}
env_file: &k api
api: {port: 9000}
*k : {port: 9001}
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing yaml:")
	})
}

// TestParse_CyclicAlias_Rejected pins that a self-referential anchor -- which
// the raw decode accepts when it lands in a typed or ignored block -- is
// rejected by the structural walk. The active set holds only the current
// descent stack, so hitting a member is a true back-edge; such an alias is
// never a meaningful config and its contents could not be schema-checked, so
// reporting it beats silently skipping it (and trivially cannot hang).
func TestParse_CyclicAlias_Rejected(t *testing.T) {
	t.Run("cycle under an unknown top-level key", func(t *testing.T) {
		_, err := Parse([]byte("processes: {web: ./web}\nnope: &x\n  b: *x\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `config: unknown field "nope"`)
	})

	t.Run("cycle through proxy.capture", func(t *testing.T) {
		// proxy: &x { capture: *x } -- capture resolves back to the proxy
		// mapping itself. yaml.v3 rejects a self-referential anchor when it
		// decodes into a generic map ("anchor 'x' value contains itself"), but
		// not when it lands in a typed block like this one, so the walk owns it.
		_, err := Parse([]byte("processes: {web: ./web}\nproxy: &x\n  enabled: true\n  domain: local.test.dev\n  capture: *x\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `proxy.capture: circular alias`)
	})
}
