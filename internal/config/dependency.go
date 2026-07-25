package config

import (
	"fmt"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"gopkg.in/yaml.v3"
)

// CheckConfig is the raw YAML shape of a dependency's check: block (plan 013
// D1). Exactly one of TCP/URL/Cmd must be set (enforced in validation);
// Timeout/Interval are duration strings parsed at ToDomain time. Field
// presence, not order, selects the probe kind.
type CheckConfig struct {
	TCP      string `yaml:"tcp"`
	URL      string `yaml:"url"`
	Cmd      string `yaml:"cmd"`
	Timeout  string `yaml:"timeout"`
	Interval string `yaml:"interval"`
	// present records which check keys were literally present in the YAML,
	// captured from the raw map before value coercion (plan 013 D1, C1). It lets
	// validation tell an ABSENT field from one explicitly set to the empty
	// string -- the coerced string is "" in both cases, but only the latter is a
	// "present but empty" error and only a present kind field counts toward the
	// exactly-one rule. Populated by parseDependencies; nil for a directly-built
	// config (which never reaches Validate).
	present map[string]bool
}

// DependencyConfig is the raw YAML shape of a dependencies: entry (plan 013
// D1). Dependencies are dependency-graph roots and therefore have NO
// depends_on; parseDependencies rejects one explicitly. OnFailure defaults to
// "fail".
type DependencyConfig struct {
	Check     CheckConfig `yaml:"check"`
	Start     string      `yaml:"start"`
	OnFailure string      `yaml:"on_failure"`
}

// TaskConfig is the raw YAML shape of a tasks: entry (plan 013 D1). Env/EnvFile
// mirror process semantics. DependsOn may reference dependencies and other
// tasks (never processes). Timeout is a duration string whose UNSET vs explicit
// "0" distinction is resolved in ToDomain (see resolveTaskTimeout).
type TaskConfig struct {
	Cmd         string            `yaml:"cmd"`
	Env         map[string]string `yaml:"env"`
	EnvFile     string            `yaml:"env_file"`
	DependsOn   []string          `yaml:"depends_on"`
	Timeout     string            `yaml:"timeout"`
	StopTimeout string            `yaml:"stop_timeout"`
	// timeoutPresent records whether the timeout key was literally present in
	// the YAML (plan 013 D1, C1). It distinguishes an ABSENT timeout (unset ->
	// default budget) from an explicit empty one (`timeout:` / `timeout: ""`),
	// which is an error rather than a silent fallback to the default. Populated
	// by parseTasks; false for a directly-built config.
	timeoutPresent bool
}

// Allowed key sets for strict unknown-key rejection (plan 013 D1, C1). Kept as
// package vars so the parsers and their error messages share one source of
// truth.
var (
	// dependencyEntryAllowedKeys includes depends_on so rejectUnknownKeys does
	// not double-report it: a depends_on ON a dependency gets its own dedicated
	// message in parseDependencies (dependencies are dependency-graph roots).
	// Inside a nested check block, by contrast, depends_on is NOT allowed and is
	// caught as a plain unknown field by checkAllowedKeys.
	dependencyEntryAllowedKeys = map[string]struct{}{"check": {}, "start": {}, "on_failure": {}, "depends_on": {}}
	checkAllowedKeys           = map[string]struct{}{"tcp": {}, "url": {}, "cmd": {}, "timeout": {}, "interval": {}}
	taskAllowedKeys            = map[string]struct{}{"cmd": {}, "env": {}, "env_file": {}, "depends_on": {}, "timeout": {}, "stop_timeout": {}}
)

// parseDependencies strictly parses the raw dependencies: block into out,
// returning structural errors (unknown keys, wrong shapes) rather than a fatal
// first-error (plan 013 D1, C1). Values are still extracted via the re-marshal
// path, but unknown keys are caught first by inspecting the raw map -- the
// re-marshal path alone would drop them silently. Entries with structural
// errors are still populated best-effort so later semantic validation has
// something to look at, but Parse returns before that when any structural error
// exists.
func parseDependencies(raw map[string]interface{}, out map[string]DependencyConfig) []string {
	var errs []string
	for _, name := range sortedMapKeys(raw) {
		prefix := fmt.Sprintf("dependencies.%s", name)
		m, ok := raw[name].(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: must be a mapping", prefix))
			continue
		}
		// depends_on on a dependency is a precise error: dependencies are
		// dependency-graph ROOTS and cannot themselves depend on anything. The
		// dedicated message is emitted here; dependencyEntryAllowedKeys lists
		// depends_on so rejectUnknownKeys does not also flag it generically.
		if _, bad := m["depends_on"]; bad {
			errs = append(errs, fmt.Sprintf("%s: depends_on is not allowed on a dependency (dependencies are dependency-graph roots)", prefix))
		}
		errs = append(errs, rejectUnknownKeys(prefix, m, dependencyEntryAllowedKeys)...)
		var checkPresent map[string]bool
		if checkRaw, ok := m["check"]; ok {
			if checkMap, ok := checkRaw.(map[string]interface{}); ok {
				errs = append(errs, rejectUnknownKeys(prefix+".check", checkMap, checkAllowedKeys)...)
				checkPresent = presentKeys(checkMap)
			} else {
				errs = append(errs, fmt.Sprintf("%s.check: must be a mapping", prefix))
			}
		}
		var dep DependencyConfig
		if err := remarshal(m, &dep); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, err))
			continue
		}
		dep.Check.present = checkPresent
		out[name] = dep
	}
	return errs
}

// parseTasks strictly parses the raw tasks: block into out (plan 013 D1, C1).
// Same contract as parseDependencies: unknown keys are rejected against
// taskAllowedKeys, values extracted via re-marshal.
func parseTasks(raw map[string]interface{}, out map[string]TaskConfig) []string {
	var errs []string
	for _, name := range sortedMapKeys(raw) {
		prefix := fmt.Sprintf("tasks.%s", name)
		m, ok := raw[name].(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: must be a mapping", prefix))
			continue
		}
		errs = append(errs, rejectUnknownKeys(prefix, m, taskAllowedKeys)...)
		var task TaskConfig
		if err := remarshal(m, &task); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", prefix, err))
			continue
		}
		_, task.timeoutPresent = m["timeout"]
		out[name] = task
	}
	return errs
}

// rejectUnknownKeys reports one error per key in raw that is not in allowed,
// prefixed with the given context path (e.g. `dependencies.postgres: unknown
// field "timeut"`). Keys are visited in sorted order so multiple unknown keys
// report deterministically. It is purely generic -- any per-key special case
// (e.g. the dedicated depends_on-on-a-dependency message) is handled by the
// caller putting the key in the allowed set and emitting its own error, so a
// key like depends_on stays a plain unknown field inside a nested check block
// where it is genuinely not allowed.
func rejectUnknownKeys(prefix string, raw map[string]interface{}, allowed map[string]struct{}) []string {
	var errs []string
	for _, k := range sortedMapKeys(raw) {
		if _, ok := allowed[k]; ok {
			continue
		}
		errs = append(errs, fmt.Sprintf("%s: unknown field %q", prefix, k))
	}
	return errs
}

// presentKeys returns the set of keys literally present in a raw YAML map, so
// validation can distinguish an absent field from one explicitly set to an
// empty or null value (both coerce to the zero value after remarshal).
func presentKeys(m map[string]interface{}) map[string]bool {
	present := make(map[string]bool, len(m))
	for k := range m {
		present[k] = true
	}
	return present
}

// remarshal round-trips a raw YAML map into a typed struct, reusing the same
// lenient extraction the process/service parsers use. Unknown-key rejection is
// handled separately by the caller BEFORE this runs, so this only coerces
// values.
func remarshal(m map[string]interface{}, out interface{}) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}

// ToDomain converts a parsed check block to its domain form (plan 013 D1),
// applying the timeout/interval defaults. It assumes validation has already
// confirmed exactly one probe kind is set and the durations are in range; a
// bare parse error here is only reachable for a directly-built config that
// skipped Validate.
func (c CheckConfig) ToDomain() (domain.DependencyCheck, error) {
	var kind domain.CheckKind
	var target string
	switch {
	case c.TCP != "":
		kind, target = domain.CheckKindTCP, c.TCP
	case c.URL != "":
		kind, target = domain.CheckKindURL, c.URL
	case c.Cmd != "":
		kind, target = domain.CheckKindCmd, c.Cmd
	}

	timeout, err := parseFieldDuration("timeout", c.Timeout, durationFieldOptions{})
	if err != nil {
		return domain.DependencyCheck{}, err
	}
	if timeout == 0 {
		timeout = constants.DefaultDependencyCheckTimeout
	}
	interval, err := parseFieldDuration("interval", c.Interval, durationFieldOptions{})
	if err != nil {
		return domain.DependencyCheck{}, err
	}
	if interval == 0 {
		interval = constants.DefaultDependencyCheckInterval
	}

	return domain.DependencyCheck{
		Kind:     kind,
		Target:   target,
		Timeout:  timeout,
		Interval: interval,
	}, nil
}

// ToDomain converts a parsed dependencies: entry to its domain form (plan 013
// D1), applying the on_failure default (fail).
func (d DependencyConfig) ToDomain(name string) (domain.DependencyConfig, error) {
	check, err := d.Check.ToDomain()
	if err != nil {
		return domain.DependencyConfig{}, err
	}
	onFailure := domain.FailurePolicyFail
	if d.OnFailure != "" {
		onFailure = domain.FailurePolicy(d.OnFailure)
	}
	return domain.DependencyConfig{
		Name:      name,
		Check:     check,
		Start:     d.Start,
		OnFailure: onFailure,
	}, nil
}

// ToDomain converts a parsed tasks: entry to its domain form (plan 013 D1),
// resolving the timeout unset-vs-zero distinction and parsing stop_timeout.
func (t TaskConfig) ToDomain(name string) (domain.TaskConfig, error) {
	timeout, hasTimeout, err := resolveTaskTimeout(t.Timeout, t.timeoutPresent)
	if err != nil {
		return domain.TaskConfig{}, err
	}
	stopTimeout, err := parseFieldDuration("stop_timeout", t.StopTimeout, stopBudgetOptions)
	if err != nil {
		return domain.TaskConfig{}, err
	}
	return domain.TaskConfig{
		Name:        name,
		Cmd:         t.Cmd,
		Env:         t.Env,
		EnvFile:     t.EnvFile,
		DependsOn:   t.DependsOn,
		Timeout:     timeout,
		HasTimeout:  hasTimeout,
		StopTimeout: stopTimeout,
	}, nil
}

// resolveTaskTimeout maps a raw task timeout string to (duration, hasLimit)
// (plan 013 D1). The cases are distinct:
//   - absent (present=false)      -> (DefaultTaskTimeout, true): default budget.
//   - present but empty ("")      -> error: an explicit blank timeout is a
//     mistake, not a request for the default (that would silently hide a typo).
//   - explicit "0" / "0s"         -> (0, false): no limit, distinct from unset.
//   - explicit "N"                -> (N, true): the configured budget.
//
// A bare time.Duration cannot model unset-vs-no-limit because both would map to
// zero. allowZero lets "0s" parse to zero without tripping a min check.
func resolveTaskTimeout(s string, present bool) (time.Duration, bool, error) {
	if s == "" {
		if present {
			return 0, false, fmt.Errorf("timeout: is present but empty")
		}
		return constants.DefaultTaskTimeout, true, nil
	}
	d, err := parseFieldDuration("timeout", s, durationFieldOptions{allowZero: true})
	if err != nil {
		return 0, false, err
	}
	if d == 0 {
		return 0, false, nil
	}
	return d, true, nil
}
