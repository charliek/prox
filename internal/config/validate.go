package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// domainRegex validates domain format (basic DNS name validation)
var domainRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Validate checks the configuration for errors
func Validate(config *Config) error {
	var errs []string

	// Validate API config
	if config.API.Port < 0 || config.API.Port > 65535 {
		errs = append(errs, fmt.Sprintf("api.port: must be between 0 and 65535, got %d", config.API.Port))
	}

	// Validate the default shutdown_timeout (#35, D1): empty is fine (falls
	// back to constants.DefaultShutdownTimeout); anything else must parse and
	// fall in (KillGrace, MaxStopTimeout].
	if _, err := parseFieldDuration("shutdown_timeout", config.ShutdownTimeout, stopBudgetOptions); err != nil {
		errs = append(errs, err.Error())
	}

	// Validate processes
	if len(config.Processes) == 0 {
		errs = append(errs, "processes: at least one process must be defined")
	}

	for name, proc := range config.Processes {
		if proc.Cmd == "" {
			errs = append(errs, fmt.Sprintf("processes.%s.cmd: command is required", name))
		}

		// Validate stop_timeout under the same policy as shutdown_timeout
		// (#35, D1); prefixed like the healthcheck field errors below.
		if _, err := parseFieldDuration("stop_timeout", proc.StopTimeout, stopBudgetOptions); err != nil {
			errs = append(errs, fmt.Sprintf("processes.%s.%s", name, err))
		}

		// Validate healthcheck if present
		if proc.Healthcheck != nil {
			if proc.Healthcheck.Cmd == "" {
				errs = append(errs, fmt.Sprintf("processes.%s.healthcheck.cmd: command is required", name))
			}
			if proc.Healthcheck.Retries < 0 {
				errs = append(errs, fmt.Sprintf("processes.%s.healthcheck.retries: must be non-negative", name))
			}
			for _, d := range []struct{ field, value string }{
				{"interval", proc.Healthcheck.Interval},
				{"timeout", proc.Healthcheck.Timeout},
				{"start_period", proc.Healthcheck.StartPeriod},
			} {
				if _, err := parseHealthDuration(d.field, d.value); err != nil {
					errs = append(errs, fmt.Sprintf("processes.%s.healthcheck.%s", name, err))
				}
			}
		}
	}

	// Validate proxy config if present
	if config.Proxy != nil {
		// Validate HTTP port if set
		if config.Proxy.HTTPPort < 0 || config.Proxy.HTTPPort > 65535 {
			errs = append(errs, fmt.Sprintf("proxy.http_port: must be between 0 and 65535, got %d", config.Proxy.HTTPPort))
		}

		// Validate HTTPS port if set
		if config.Proxy.HTTPSPort < 0 || config.Proxy.HTTPSPort > 65535 {
			errs = append(errs, fmt.Sprintf("proxy.https_port: must be between 0 and 65535, got %d", config.Proxy.HTTPSPort))
		}

		// Require at least one port when proxy is enabled
		if config.Proxy.Enabled && config.Proxy.HTTPPort == 0 && config.Proxy.HTTPSPort == 0 {
			errs = append(errs, "proxy: at least one of http_port or https_port must be set when proxy is enabled")
		}

		if config.Proxy.Enabled && config.Proxy.Domain == "" {
			errs = append(errs, "proxy.domain: required when proxy is enabled")
		}
		if config.Proxy.Domain != "" && !domainRegex.MatchString(config.Proxy.Domain) {
			errs = append(errs, fmt.Sprintf("proxy.domain: invalid domain format %q", config.Proxy.Domain))
		}

		// Validate capture disk budget if set (#69). Empty means "use the default"
		// (constants.DefaultCaptureDiskBudget); anything else must parse and be
		// positive. A budget smaller than max_body_size is intentionally allowed
		// (no warning infra exists): the accountant evicts an oversized single
		// spill as the oldest-and-only group, so a tiny budget still converges.
		if config.Proxy.Capture != nil && config.Proxy.Capture.DiskBudget != "" {
			n, err := ParseSize(config.Proxy.Capture.DiskBudget)
			if err != nil {
				errs = append(errs, fmt.Sprintf("proxy.capture.disk_budget: %s", err.Error()))
			} else if n <= 0 {
				errs = append(errs, fmt.Sprintf("proxy.capture.disk_budget: must be positive, got %q", config.Proxy.Capture.DiskBudget))
			}
		}

		// Validate the redaction extension lists (plan 012 D4). Both extend the
		// built-in sets, so entries must be non-empty; redact_headers entries must
		// additionally be usable HTTP field names (no spaces/colons/control chars)
		// so canonicalization and header matching behave.
		if config.Proxy.Capture != nil {
			for _, name := range config.Proxy.Capture.RedactHeaders {
				if err := validateRedactHeaderName(name); err != nil {
					errs = append(errs, fmt.Sprintf("proxy.capture.redact_headers: %s", err.Error()))
				}
			}
			for _, name := range config.Proxy.Capture.RedactQueryParams {
				if name == "" {
					errs = append(errs, "proxy.capture.redact_query_params: entry cannot be empty")
				}
			}
		}
	}

	// Validate certs config if present
	if config.Certs != nil {
		if config.Certs.Dir == "" {
			errs = append(errs, "certs.dir: directory path is required")
		}
	}

	// Validate that HTTPS requires certs when enabled
	if config.Proxy != nil && config.Proxy.Enabled && config.Proxy.HTTPSPort > 0 {
		if config.Certs == nil {
			errs = append(errs, "certs: certificate configuration required when HTTPS proxy is enabled")
		}
	}

	// Validate services config if present
	for name, svc := range config.Services {
		if svc.Port <= 0 || svc.Port > 65535 {
			errs = append(errs, fmt.Sprintf("services.%s.port: must be between 1 and 65535, got %d", name, svc.Port))
		}
		if err := validateServiceName(name); err != nil {
			errs = append(errs, fmt.Sprintf("services.%s: %s", name, err.Error()))
		}
		if err := validateHost(svc.Host); err != nil {
			errs = append(errs, fmt.Sprintf("services.%s.host: %s", name, err.Error()))
		}
	}

	// Validate that services require proxy to be enabled
	if len(config.Services) > 0 && (config.Proxy == nil || !config.Proxy.Enabled) {
		errs = append(errs, "services: proxy must be enabled when services are defined")
	}

	// Validate dependencies, tasks, and the depends_on graph (plan 013 D1, C1).
	// Emitted in deterministic order by the helper so map iteration never
	// perturbs the report.
	errs = append(errs, validateDependenciesAndTasks(config)...)

	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrInvalidConfig, strings.Join(errs, "; "))
	}

	return nil
}

// validateRedactHeaderName checks a redact_headers entry is a usable HTTP field
// name (plan 012 D4): non-empty and composed entirely of RFC 7230 "tchar"
// token characters (ALPHA / DIGIT / "!" / "#" / "$" / "%" / "&" / "'" / "*" /
// "+" / "-" / "." / "^" / "_" / "`" / "|" / "~"). Anything else — commas,
// slashes, non-ASCII bytes, spaces, colons, control characters — is rejected:
// those bytes can slip past a naive check yet still make
// http.CanonicalHeaderKey refuse to canonicalize the value, so the configured
// header would silently never match at redaction time.
func validateRedactHeaderName(name string) error {
	if name == "" {
		return fmt.Errorf("entry cannot be empty")
	}
	for _, r := range name {
		if !isTokenChar(r) {
			return fmt.Errorf("invalid header name %q (must be a valid HTTP token: letters, digits, or !#$%%&'*+-.^_`|~)", name)
		}
	}
	return nil
}

// isTokenChar reports whether r is an RFC 7230 "tchar" (the character class
// allowed in HTTP token productions such as header field names).
func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		return true
	default:
		return false
	}
}

// validateServiceName checks if a service name is valid as a subdomain
func validateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	// Service names become subdomains, so they must be valid DNS labels
	// - Only lowercase alphanumeric and hyphens
	// - Cannot start or end with hyphen
	// - Max 63 characters
	if len(name) > 63 {
		return fmt.Errorf("service name too long (max 63 characters)")
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("service name cannot start or end with hyphen")
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("service name can only contain lowercase letters, numbers, and hyphens")
		}
	}
	return nil
}

// ValidateProcessName checks if a process name is valid
func ValidateProcessName(name string) error {
	if name == "" {
		return &ValidationError{Field: "name", Message: "process name cannot be empty"}
	}
	if strings.ContainsAny(name, " \t\n/\\") {
		return &ValidationError{Field: "name", Message: "process name cannot contain whitespace or path separators"}
	}
	return nil
}

// validateDependenciesAndTasks runs every semantic check over the dependencies:
// and tasks: blocks and the depends_on graph (plan 013 D1, C1). Structural
// problems (unknown keys, malformed shapes) are already caught in Parse before
// this runs, so here every dependency/task is well-formed. All output is
// produced in deterministic order -- names are visited sorted and any
// cross-name reporting (collisions, cycles) is sorted before being appended --
// so the same invalid config always yields byte-identical error strings.
func validateDependenciesAndTasks(config *Config) []string {
	var errs []string

	procNames := keySet(config.Processes)
	depNames := keySet(config.Dependencies)
	taskNames := keySet(config.Tasks)

	// Name validity: dependency and task names follow the same rules as process
	// names (reuse ValidateProcessName).
	for _, name := range sortedMapKeys(config.Dependencies) {
		if err := ValidateProcessName(name); err != nil {
			errs = append(errs, fmt.Sprintf("dependencies.%s: %s", name, validationMessage(err)))
		}
	}
	for _, name := range sortedMapKeys(config.Tasks) {
		if err := ValidateProcessName(name); err != nil {
			errs = append(errs, fmt.Sprintf("tasks.%s: %s", name, validationMessage(err)))
		}
	}

	// Cross-namespace uniqueness: a name may live in at most one of processes,
	// tasks, dependencies (case-sensitive). Report each collision once, naming
	// every namespace it appears in.
	errs = append(errs, collisionErrors(procNames, taskNames, depNames)...)

	// Per-dependency check validation.
	for _, name := range sortedMapKeys(config.Dependencies) {
		errs = append(errs, validateDependency(name, config.Dependencies[name])...)
	}

	// Per-task validation (cmd required, timeout/stop_timeout well-formed,
	// depends_on targets resolvable).
	for _, name := range sortedMapKeys(config.Tasks) {
		task := config.Tasks[name]
		prefix := fmt.Sprintf("tasks.%s", name)
		if task.Cmd == "" {
			errs = append(errs, fmt.Sprintf("%s.cmd: command is required", prefix))
		}
		if _, _, err := resolveTaskTimeout(task.Timeout, task.timeoutPresent); err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s", prefix, err))
		}
		if _, err := parseFieldDuration("stop_timeout", task.StopTimeout, stopBudgetOptions); err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s", prefix, err))
		}
		errs = append(errs, validateDependsOnList(prefix, task.DependsOn, depNames, taskNames, procNames)...)
	}

	// Per-process depends_on validation (processes reference deps/tasks only).
	for _, name := range sortedMapKeys(config.Processes) {
		proc := config.Processes[name]
		if len(proc.DependsOn) == 0 {
			continue
		}
		prefix := fmt.Sprintf("processes.%s", name)
		errs = append(errs, validateDependsOnList(prefix, proc.DependsOn, depNames, taskNames, procNames)...)
	}

	// Cycle detection over the task graph (dependencies are roots and processes
	// are leaves, so only task->task edges can form a cycle).
	errs = append(errs, detectTaskCycles(config.Tasks, taskNames)...)

	return errs
}

// validateDependency checks one dependencies: entry (plan 013 D1): exactly one
// check kind, a well-formed target, sane interval/timeout, and a known
// on_failure policy.
func validateDependency(name string, dep DependencyConfig) []string {
	var errs []string
	prefix := fmt.Sprintf("dependencies.%s", name)
	check := dep.Check

	// A present-but-empty kind field is its own precise error: it is a mistake
	// distinct from omitting the field, and would otherwise be invisible (the
	// coerced value is "" either way). Reported per kind field that is present
	// yet blank.
	for _, kf := range []struct{ key, val string }{
		{"tcp", check.TCP},
		{"url", check.URL},
		{"cmd", check.Cmd},
	} {
		if check.present[kf.key] && kf.val == "" {
			errs = append(errs, fmt.Sprintf("%s.check.%s: is present but empty", prefix, kf.key))
		}
	}

	// Exactly one of tcp/url/cmd, counted by PRESENCE (not non-empty value): a
	// present-but-empty field still counts as a chosen kind, so `{url: x, cmd: ""}`
	// is two kinds, not one. present is nil when the check block is absent, so
	// this correctly reports "none set".
	kinds := 0
	for _, k := range []string{"tcp", "url", "cmd"} {
		if check.present[k] {
			kinds++
		}
	}
	switch {
	case kinds == 0:
		errs = append(errs, fmt.Sprintf("%s.check: must specify exactly one of tcp, url, or cmd (none set)", prefix))
	case kinds > 1:
		errs = append(errs, fmt.Sprintf("%s.check: must specify exactly one of tcp, url, or cmd (%d set)", prefix, kinds))
	}

	// Target well-formedness (only meaningful for the single kind that is set).
	if check.URL != "" {
		u, err := url.Parse(check.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Sprintf("%s.check.url: must be a valid http/https URL: %q", prefix, check.URL))
		} else if u.Scheme != "http" && u.Scheme != "https" {
			errs = append(errs, fmt.Sprintf("%s.check.url: scheme must be http or https, got %q", prefix, u.Scheme))
		}
	}
	if check.TCP != "" {
		host, port, err := net.SplitHostPort(check.TCP)
		if err != nil || host == "" || port == "" {
			errs = append(errs, fmt.Sprintf("%s.check.tcp: must be host:port with both parts set: %q", prefix, check.TCP))
		}
	}

	// interval > 0 and timeout >= interval, both defaulted when unset.
	interval, ierr := parseFieldDuration("interval", check.Interval, durationFieldOptions{})
	if ierr != nil {
		errs = append(errs, fmt.Sprintf("%s.check.%s", prefix, ierr))
	}
	timeout, terr := parseFieldDuration("timeout", check.Timeout, durationFieldOptions{})
	if terr != nil {
		errs = append(errs, fmt.Sprintf("%s.check.%s", prefix, terr))
	}
	if ierr == nil && terr == nil {
		effInterval := interval
		if check.Interval == "" {
			effInterval = constants.DefaultDependencyCheckInterval
		}
		effTimeout := timeout
		if check.Timeout == "" {
			effTimeout = constants.DefaultDependencyCheckTimeout
		}
		if effInterval <= 0 {
			errs = append(errs, fmt.Sprintf("%s.check.interval: must be greater than 0", prefix))
		} else if effTimeout < effInterval {
			// Only compare once interval is sane; an explicit zero timeout falls
			// here too (0 is not special for dependencies -- plan 013 D1).
			errs = append(errs, fmt.Sprintf("%s.check.timeout: must be greater than or equal to interval (%s)", prefix, trimZeroDuration(effInterval)))
		}
	}

	// on_failure policy.
	switch dep.OnFailure {
	case "", string(domain.FailurePolicyFail), string(domain.FailurePolicyWarn):
	default:
		errs = append(errs, fmt.Sprintf("%s.on_failure: must be %q or %q, got %q", prefix, domain.FailurePolicyFail, domain.FailurePolicyWarn, dep.OnFailure))
	}

	return errs
}

// validateDependsOnList checks one depends_on list (plan 013 D1): entries must
// resolve to a dependency or task, duplicates are rejected, and a process name
// is a distinct precise error (processes are never dependency targets).
func validateDependsOnList(prefix string, list []string, depNames, taskNames, procNames map[string]struct{}) []string {
	var errs []string
	seen := make(map[string]struct{}, len(list))
	for _, entry := range list {
		if _, dup := seen[entry]; dup {
			errs = append(errs, fmt.Sprintf("%s.depends_on: duplicate entry %q", prefix, entry))
			continue
		}
		seen[entry] = struct{}{}

		if _, ok := depNames[entry]; ok {
			continue
		}
		if _, ok := taskNames[entry]; ok {
			continue
		}
		if _, ok := procNames[entry]; ok {
			errs = append(errs, fmt.Sprintf("%s.depends_on: %q is a process; processes cannot be dependency targets", prefix, entry))
			continue
		}
		errs = append(errs, fmt.Sprintf("%s.depends_on: unknown target %q (must be a dependency or task)", prefix, entry))
	}
	return errs
}

// collisionErrors reports every name shared across more than one of the three
// namespaces (plan 013 D1). Deterministic: names visited sorted, namespaces per
// name reported in a fixed order.
func collisionErrors(procNames, taskNames, depNames map[string]struct{}) []string {
	occ := map[string][]string{}
	// Fixed namespace visitation order so a name's namespace list is stable.
	for _, ns := range []struct {
		label string
		set   map[string]struct{}
	}{
		{"dependencies", depNames},
		{"processes", procNames},
		{"tasks", taskNames},
	} {
		for name := range ns.set {
			occ[name] = append(occ[name], ns.label)
		}
	}

	names := make([]string, 0, len(occ))
	for name, spaces := range occ {
		if len(spaces) > 1 {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var errs []string
	for _, name := range names {
		spaces := occ[name]
		sort.Strings(spaces)
		errs = append(errs, fmt.Sprintf("name %q is defined in multiple namespaces (%s); names must be unique across processes, tasks, and dependencies", name, strings.Join(spaces, ", ")))
	}
	return errs
}

// detectTaskCycles finds every cycle in the task dependency graph via Tarjan's
// strongly-connected-components (plan 013 D1). Only task->task depends_on edges
// participate: dependencies have no outgoing edges and processes are never
// targets, so no cycle can leave the task subgraph. Each SCC with more than one
// member, plus any single task that depends on itself, is a cycle. Output is
// sorted (members within a cycle, and the cycles among themselves) so multiple
// disjoint cycles report deterministically.
func detectTaskCycles(tasks map[string]TaskConfig, taskNames map[string]struct{}) []string {
	// Build the task->task adjacency, keeping only edges whose target is a task.
	edges := make(map[string]map[string]struct{}, len(tasks))
	for name := range tasks {
		edges[name] = map[string]struct{}{}
	}
	for name, task := range tasks {
		for _, dep := range task.DependsOn {
			if _, ok := taskNames[dep]; ok {
				edges[name][dep] = struct{}{}
			}
		}
	}

	var (
		index    int
		indices  = map[string]int{}
		lowlink  = map[string]int{}
		onStack  = map[string]bool{}
		stack    []string
		cycleMsg []string
	)

	var strongconnect func(v string)
	strongconnect = func(v string) {
		indices[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true

		neighbors := make([]string, 0, len(edges[v]))
		for w := range edges[v] {
			neighbors = append(neighbors, w)
		}
		sort.Strings(neighbors)
		for _, w := range neighbors {
			if _, seen := indices[w]; !seen {
				strongconnect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if indices[w] < lowlink[v] {
					lowlink[v] = indices[w]
				}
			}
		}

		if lowlink[v] == indices[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			isCycle := len(comp) > 1
			if !isCycle {
				if _, self := edges[comp[0]][comp[0]]; self {
					isCycle = true
				}
			}
			if isCycle {
				sort.Strings(comp)
				cycleMsg = append(cycleMsg, fmt.Sprintf("tasks: dependency cycle detected: %s", strings.Join(comp, " -> ")))
			}
		}
	}

	// Visit tasks in sorted order so SCC discovery is deterministic.
	names := make([]string, 0, len(tasks))
	for name := range tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, v := range names {
		if _, seen := indices[v]; !seen {
			strongconnect(v)
		}
	}

	sort.Strings(cycleMsg)
	return cycleMsg
}

// validationMessage extracts the human message from a ValidateProcessName
// error, stripping its "name: " field prefix so the dependency/task caller can
// re-prefix with the right namespace path.
func validationMessage(err error) string {
	if ve, ok := err.(*ValidationError); ok {
		return ve.Message
	}
	return err.Error()
}

// keySet returns the key set of a map as a set for membership tests.
func keySet[V any](m map[string]V) map[string]struct{} {
	set := make(map[string]struct{}, len(m))
	for k := range m {
		set[k] = struct{}{}
	}
	return set
}

// sortedMapKeys returns a map's keys in sorted order for deterministic
// iteration in validation.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hostnameRegex validates hostname format (excluding IP addresses)
var hostnameRegex = regexp.MustCompile(`^(localhost|[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*)$`)

// validateHost checks if a host is a valid hostname or IP address
func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("host cannot be empty")
	}
	// First check if it's a valid IP address (handles both IPv4 and IPv6)
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	// Otherwise validate as hostname
	if !hostnameRegex.MatchString(host) {
		return fmt.Errorf("invalid host format %q", host)
	}
	return nil
}
