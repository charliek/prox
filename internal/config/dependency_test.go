package config

import (
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Parse matrix ---------------------------------------------------------

func TestParse_DependenciesTasks_FullHappy(t *testing.T) {
	yaml := `
processes:
  worker:
    cmd: ./worker
    depends_on: [postgres, restate-register]
dependencies:
  postgres:
    check:
      tcp: "localhost:5435"
      timeout: 30s
      interval: 1s
    start: docker compose up -d postgres
    on_failure: fail
tasks:
  restate-register:
    cmd: ./scripts/restate register --force
    env:
      FOO: bar
    env_file: .env.task
    depends_on: [postgres]
    timeout: 60s
    stop_timeout: 10s
`
	cfg, err := Parse([]byte(yaml))
	require.NoError(t, err)

	require.Len(t, cfg.Dependencies, 1)
	require.Len(t, cfg.Tasks, 1)
	assert.Equal(t, []string{"postgres", "restate-register"}, cfg.Processes["worker"].DependsOn)

	dep, err := cfg.Dependencies["postgres"].ToDomain("postgres")
	require.NoError(t, err)
	assert.Equal(t, domain.CheckKindTCP, dep.Check.Kind)
	assert.Equal(t, "localhost:5435", dep.Check.Target)
	assert.Equal(t, 30*time.Second, dep.Check.Timeout)
	assert.Equal(t, 1*time.Second, dep.Check.Interval)
	assert.Equal(t, "docker compose up -d postgres", dep.Start)
	assert.Equal(t, domain.FailurePolicyFail, dep.OnFailure)

	task, err := cfg.Tasks["restate-register"].ToDomain("restate-register")
	require.NoError(t, err)
	assert.Equal(t, "./scripts/restate register --force", task.Cmd)
	assert.Equal(t, "bar", task.Env["FOO"])
	assert.Equal(t, ".env.task", task.EnvFile)
	assert.Equal(t, []string{"postgres"}, task.DependsOn)
	assert.Equal(t, 60*time.Second, task.Timeout)
	assert.True(t, task.HasTimeout)
	assert.Equal(t, 10*time.Second, task.StopTimeout)
}

func TestParse_MinimalDependency_CheckOnly(t *testing.T) {
	yaml := `
processes:
  web: ./web
dependencies:
  redis:
    check:
      tcp: "localhost:6379"
`
	cfg, err := Parse([]byte(yaml))
	require.NoError(t, err)

	dep, err := cfg.Dependencies["redis"].ToDomain("redis")
	require.NoError(t, err)
	// Defaults applied.
	assert.Equal(t, 30*time.Second, dep.Check.Timeout)
	assert.Equal(t, 1*time.Second, dep.Check.Interval)
	assert.Equal(t, domain.FailurePolicyFail, dep.OnFailure)
	assert.Empty(t, dep.Start)
}

func TestParse_UrlAndCmdChecks(t *testing.T) {
	yaml := `
processes:
  web: ./web
dependencies:
  api:
    check:
      url: "http://localhost:8080/health"
  migrate:
    check:
      cmd: "pg_isready -h localhost"
`
	cfg, err := Parse([]byte(yaml))
	require.NoError(t, err)

	api, err := cfg.Dependencies["api"].ToDomain("api")
	require.NoError(t, err)
	assert.Equal(t, domain.CheckKindURL, api.Check.Kind)
	assert.Equal(t, "http://localhost:8080/health", api.Check.Target)

	mig, err := cfg.Dependencies["migrate"].ToDomain("migrate")
	require.NoError(t, err)
	assert.Equal(t, domain.CheckKindCmd, mig.Check.Kind)
	assert.Equal(t, "pg_isready -h localhost", mig.Check.Target)
}

func TestParse_TaskTimeout_UnsetVsExplicitZero(t *testing.T) {
	yaml := `
processes:
  web: ./web
tasks:
  unlimited:
    cmd: ./a
    timeout: 0
  defaulted:
    cmd: ./b
  explicit:
    cmd: ./c
    timeout: 15s
`
	cfg, err := Parse([]byte(yaml))
	require.NoError(t, err)

	unlimited, err := cfg.Tasks["unlimited"].ToDomain("unlimited")
	require.NoError(t, err)
	assert.False(t, unlimited.HasTimeout, "explicit timeout: 0 means no limit")
	assert.Equal(t, time.Duration(0), unlimited.Timeout)

	defaulted, err := cfg.Tasks["defaulted"].ToDomain("defaulted")
	require.NoError(t, err)
	assert.True(t, defaulted.HasTimeout, "unset timeout is bounded by the default")
	assert.Equal(t, 60*time.Second, defaulted.Timeout)

	explicit, err := cfg.Tasks["explicit"].ToDomain("explicit")
	require.NoError(t, err)
	assert.True(t, explicit.HasTimeout)
	assert.Equal(t, 15*time.Second, explicit.Timeout)
}

// --- Validation error classes ---------------------------------------------

func TestParse_DependenciesTasks_Errors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "unknown key in dependency",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379"}
    on_failur: warn
`,
			wantSub: `dependencies.redis: unknown field "on_failur"`,
		},
		{
			name: "unknown key in task",
			yaml: `
processes: {web: ./web}
tasks:
  build:
    cmd: ./build
    timeut: 5s
`,
			wantSub: `tasks.build: unknown field "timeut"`,
		},
		{
			name: "depends_on on a dependency",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379"}
    depends_on: [postgres]
`,
			wantSub: "dependencies.redis: depends_on is not allowed on a dependency",
		},
		{
			name: "zero check types",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    start: ./bring-up
`,
			wantSub: "must specify exactly one of tcp, url, or cmd (none set)",
		},
		{
			name: "depends_on inside a check block is a plain unknown field",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379", depends_on: [x]}
`,
			wantSub: `dependencies.redis.check: unknown field "depends_on"`,
		},
		{
			name: "unknown field inside a check block",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379", retries: 3}
`,
			wantSub: `dependencies.redis.check: unknown field "retries"`,
		},
		{
			name: "check field present but empty (url set, cmd blank)",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {url: "http://localhost:8080", cmd: ""}
`,
			wantSub: "dependencies.redis.check.cmd: is present but empty",
		},
		{
			name: "single check field present but empty (tcp blank)",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: ""}
`,
			wantSub: "dependencies.redis.check.tcp: is present but empty",
		},
		{
			name: "task timeout present but empty",
			yaml: `
processes: {web: ./web}
tasks:
  build:
    cmd: ./build
    timeout: ""
`,
			wantSub: "tasks.build.timeout: is present but empty",
		},
		{
			name: "two check types",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379", url: "http://localhost:8080"}
`,
			wantSub: "must specify exactly one of tcp, url, or cmd (2 set)",
		},
		{
			name: "bad url scheme",
			yaml: `
processes: {web: ./web}
dependencies:
  api:
    check: {url: "ftp://localhost:8080"}
`,
			wantSub: "scheme must be http or https",
		},
		{
			name: "bad tcp addr",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "notanaddress"}
`,
			wantSub: "must be host:port with both parts set",
		},
		{
			name: "interval <= 0",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379", interval: 0s}
`,
			wantSub: "check.interval: must be greater than 0",
		},
		{
			name: "timeout < interval",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379", interval: 5s, timeout: 2s}
`,
			wantSub: "check.timeout: must be greater than or equal to interval",
		},
		{
			name: "bad on_failure",
			yaml: `
processes: {web: ./web}
dependencies:
  redis:
    check: {tcp: "localhost:6379"}
    on_failure: maybe
`,
			wantSub: `dependencies.redis.on_failure: must be "fail" or "warn", got "maybe"`,
		},
		{
			name: "invalid dependency name",
			yaml: `
processes: {web: ./web}
dependencies:
  "bad/name":
    check: {tcp: "localhost:6379"}
`,
			wantSub: "dependencies.bad/name:",
		},
		{
			name: "cross-namespace collision",
			yaml: `
processes:
  foo: ./foo
tasks:
  foo: {cmd: ./foo-task}
`,
			wantSub: `name "foo" is defined in multiple namespaces (processes, tasks)`,
		},
		{
			name: "unknown depends_on target",
			yaml: `
processes:
  web:
    cmd: ./web
    depends_on: [nope]
`,
			wantSub: `depends_on: unknown target "nope"`,
		},
		{
			name: "process as target",
			yaml: `
processes:
  web:
    cmd: ./web
    depends_on: [api]
  api: ./api
`,
			wantSub: `depends_on: "api" is a process; processes cannot be dependency targets`,
		},
		{
			name: "duplicate depends_on entry",
			yaml: `
processes:
  web:
    cmd: ./web
    depends_on: [redis, redis]
dependencies:
  redis:
    check: {tcp: "localhost:6379"}
`,
			wantSub: `depends_on: duplicate entry "redis"`,
		},
		{
			name: "self cycle",
			yaml: `
processes: {web: ./web}
tasks:
  a: {cmd: ./a, depends_on: [a]}
`,
			wantSub: "tasks: dependency cycle detected: a",
		},
		{
			name: "two cycle",
			yaml: `
processes: {web: ./web}
tasks:
  a: {cmd: ./a, depends_on: [b]}
  b: {cmd: ./b, depends_on: [a]}
`,
			wantSub: "tasks: dependency cycle detected: a -> b",
		},
		{
			name: "three cycle",
			yaml: `
processes: {web: ./web}
tasks:
  a: {cmd: ./a, depends_on: [b]}
  b: {cmd: ./b, depends_on: [c]}
  c: {cmd: ./c, depends_on: [a]}
`,
			wantSub: "tasks: dependency cycle detected: a -> b -> c",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestParse_TwoDisjointCycles_BothReportedDeterministically(t *testing.T) {
	yaml := `
processes: {web: ./web}
tasks:
  a: {cmd: ./a, depends_on: [b]}
  b: {cmd: ./b, depends_on: [a]}
  c: {cmd: ./c, depends_on: [d]}
  d: {cmd: ./d, depends_on: [c]}
`
	_, err := Parse([]byte(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tasks: dependency cycle detected: a -> b")
	assert.Contains(t, err.Error(), "tasks: dependency cycle detected: c -> d")
}

func TestParse_InvalidConfig_DeterministicErrorString(t *testing.T) {
	yaml := `
processes: {web: ./web}
tasks:
  a: {cmd: ./a, depends_on: [b]}
  b: {cmd: ./b, depends_on: [a]}
  c: {cmd: ./c, depends_on: [d]}
  d: {cmd: ./d, depends_on: [c]}
`
	_, err1 := Parse([]byte(yaml))
	_, err2 := Parse([]byte(yaml))
	require.Error(t, err1)
	require.Error(t, err2)
	assert.Equal(t, err1.Error(), err2.Error())
}

func TestParse_TaskAndProcessDependOnDependency_Valid(t *testing.T) {
	yaml := `
processes:
  web:
    cmd: ./web
    depends_on: [redis, seed]
tasks:
  seed:
    cmd: ./seed
    depends_on: [redis]
dependencies:
  redis:
    check: {tcp: "localhost:6379"}
`
	_, err := Parse([]byte(yaml))
	require.NoError(t, err)
}
