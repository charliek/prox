package domain

import "time"

// CheckKind discriminates the readiness-probe form a dependency's check uses
// (plan 013 D1). Exactly one kind is set per dependency; the config layer
// rejects zero or multiple check forms before a DependencyCheck is ever built,
// so the resolver can switch on Kind without re-validating.
type CheckKind string

const (
	// CheckKindTCP probes a "host:port" TCP endpoint (Target is the address).
	CheckKindTCP CheckKind = "tcp"
	// CheckKindURL probes an http/https URL (Target is the URL).
	CheckKindURL CheckKind = "url"
	// CheckKindCmd runs a shell command; readiness is a zero exit (Target is
	// the command string).
	CheckKindCmd CheckKind = "cmd"
)

// FailurePolicy is what the supervisor does when a dependency never becomes
// ready within its check budget (plan 013 D1). Defaults to FailurePolicyFail.
type FailurePolicy string

const (
	// FailurePolicyFail aborts startup when the dependency is not ready.
	FailurePolicyFail FailurePolicy = "fail"
	// FailurePolicyWarn logs and proceeds when the dependency is not ready.
	FailurePolicyWarn FailurePolicy = "warn"
)

// DependencyCheck is the resolved readiness probe for a dependency (plan 013
// D1). Kind selects the probe form and Target carries its parameter (address,
// URL, or command). Timeout and Interval always carry their effective values
// with defaults already applied (see config.CheckConfig.toDomainCheck).
type DependencyCheck struct {
	Kind     CheckKind
	Target   string
	Timeout  time.Duration
	Interval time.Duration
}

// DependencyConfig is the resolved configuration for a dependencies: entry
// (plan 013 D1). Dependencies are dependency-graph ROOTS: they have no
// depends_on of their own. Start is an optional command run once to bring the
// dependency up before probing; OnFailure carries the effective policy with
// its default applied.
type DependencyConfig struct {
	Name      string
	Check     DependencyCheck
	Start     string
	OnFailure FailurePolicy
}

// TaskConfig is the resolved configuration for a tasks: entry (plan 013 D1). A
// task is a run-to-completion command that may depend on dependencies and
// other tasks (never processes). Env/EnvFile mirror process semantics.
type TaskConfig struct {
	Name      string
	Cmd       string
	Env       map[string]string
	EnvFile   string
	DependsOn []string
	// Timeout is the run budget. It is meaningful only when HasTimeout is true;
	// when HasTimeout is false the task runs with no time limit.
	Timeout time.Duration
	// HasTimeout distinguishes a time-bounded task (unset -> default
	// DefaultTaskTimeout, or an explicit positive value) from an explicitly
	// unbounded one (timeout: 0). See config.TaskConfig.ToDomain: unset resolves
	// to HasTimeout=true with the default; an explicit zero resolves to
	// HasTimeout=false. This is why a bare time.Duration cannot model the field
	// -- zero-the-default and zero-means-unlimited would collide.
	HasTimeout bool
	// StopTimeout is this task's own SIGTERM->SIGKILL escalation budget, parsed
	// under the same policy as a process's stop_timeout. Zero means unset.
	StopTimeout time.Duration
}
