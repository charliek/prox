package tui

import (
	"sort"
	"strings"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// Filter-menu commands (plan 021 WS8). Dispatched from activateMenuCommand to
// the same filter state the s bar edits; menu touches always canonicalize RawQuery.
const MenuCmdClearFilters MenuCommand = "clear-filters"

const (
	menuCmdToggleProcPrefix   = "toggle-proc:"
	menuCmdToggleLevelPrefix  = "toggle-level:"
	menuCmdToggleMethodPrefix = "toggle-method:"
	menuCmdToggleStatusPrefix = "toggle-status:"
	menuCmdSetInFlightPrefix  = "set-in-flight:"
)

var (
	logsFilterLevels = []struct {
		label string
		level LogLevel
	}{
		{"error", LogLevelError},
		{"warn", LogLevelWarn},
		{"info", LogLevelInfo},
		{"debug", LogLevelDebug},
	}

	requestsStatusClasses = []struct {
		label string
		class int
	}{
		{"2xx", 2},
		{"3xx", 3},
		{"4xx", 4},
		{"5xx", 5},
	}

	inFlightMenuOptions = []struct {
		label string
		value string // any | true | false
	}{
		{"Any", "any"},
		{"In flight", "true"},
		{"Completed", "false"},
	}
)

// filterMenuItems builds Filter-dropdown rows live from the active view (WS8).
func (b *BaseModel) filterMenuItems() []MenuItem {
	if b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail {
		return b.requestsFilterMenuItems()
	}
	return b.logsFilterMenuItems()
}

func (b *BaseModel) logsFilterMenuItems() []MenuItem {
	expr := b.logsFilterExprForMenu()
	var items []MenuItem

	for _, name := range sortedProcessNames(b.processes) {
		checked := logsProcChecked(expr, name)
		items = append(items, MenuItem{
			Label:   name,
			Checked: &checked,
			Cmd:     MenuCommand(menuCmdToggleProcPrefix + name),
		})
	}
	if len(items) > 0 { // no leading separator when there are no processes
		items = append(items, MenuItem{Separator: true})
	}

	for _, lv := range logsFilterLevels {
		checked := logsLevelChecked(expr, lv.level)
		items = append(items, MenuItem{
			Label:   lv.label,
			Checked: &checked,
			Cmd:     MenuCommand(menuCmdToggleLevelPrefix + lv.label),
		})
	}
	items = append(items, MenuItem{Separator: true})
	items = append(items, MenuItem{Label: "Clear all filters", Cmd: MenuCmdClearFilters})
	return items
}

func (b *BaseModel) requestsFilterMenuItems() []MenuItem {
	expr := b.requestsFilterExprForMenu()
	var items []MenuItem

	for _, method := range presentHTTPMethods(b.proxyRequests) {
		checked := requestsMethodChecked(expr, method)
		items = append(items, MenuItem{
			Label:   method,
			Checked: &checked,
			Cmd:     MenuCommand(menuCmdToggleMethodPrefix + method),
		})
	}
	if len(items) > 0 {
		items = append(items, MenuItem{Separator: true})
	}

	for _, sc := range requestsStatusClasses {
		checked := requestsStatusClassChecked(expr, sc.class)
		items = append(items, MenuItem{
			Label:   sc.label,
			Checked: &checked,
			Cmd:     MenuCommand(menuCmdToggleStatusPrefix + sc.label),
		})
	}
	items = append(items, MenuItem{Separator: true})

	sel := requestsInFlightSelection(expr)
	for _, opt := range inFlightMenuOptions {
		selected := sel == opt.value
		items = append(items, MenuItem{
			Label:    opt.label,
			Selected: &selected,
			Cmd:      MenuCommand(menuCmdSetInFlightPrefix + opt.value),
		})
	}
	items = append(items, MenuItem{Separator: true})
	items = append(items, MenuItem{Label: "Clear all filters", Cmd: MenuCmdClearFilters})
	return items
}

// logsFilterExprForMenu returns the expr menu edits apply to. When RawQuery
// does not parse, LastGood is the last sane state (WS8).
func (b *BaseModel) logsFilterExprForMenu() LogsFilterExpr {
	if b.logsFilter.ParseErr != nil {
		return b.logsFilter.LastGood
	}
	expr, err := ParseLogsFilter(b.logsFilter.RawQuery)
	if err != nil {
		return b.logsFilter.LastGood
	}
	return expr
}

func (b *BaseModel) requestsFilterExprForMenu() RequestsFilterExpr {
	if b.requestsFilter.ParseErr != nil {
		return b.requestsFilter.LastGood
	}
	expr, err := ParseRequestsFilter(b.requestsFilter.RawQuery)
	if err != nil {
		return b.requestsFilter.LastGood
	}
	return expr
}

func sortedProcessNames(processes []domain.ProcessInfo) []string {
	names := make([]string, 0, len(processes))
	for _, p := range processes {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func presentHTTPMethods(requests []proxy.RequestRecord) []string {
	seen := make(map[string]string) // upper -> canonical first-seen
	for _, req := range requests {
		up := strings.ToUpper(req.Method)
		if _, ok := seen[up]; !ok {
			seen[up] = req.Method
		}
	}
	methods := make([]string, 0, len(seen))
	for _, m := range seen {
		methods = append(methods, m)
	}
	sort.Slice(methods, func(i, j int) bool {
		return strings.ToUpper(methods[i]) < strings.ToUpper(methods[j])
	})
	return methods
}

func logsProcChecked(expr LogsFilterExpr, proc string) bool {
	if len(expr.procs) > 0 {
		return stringSliceContains(expr.procs, proc)
	}
	return !stringSliceContains(expr.negProcs, proc)
}

func logsLevelChecked(expr LogsFilterExpr, lvl LogLevel) bool {
	return logLevelSliceContains(expr.levels, lvl)
}

func requestsMethodChecked(expr RequestsFilterExpr, method string) bool {
	for _, m := range expr.methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

func requestsStatusClassChecked(expr RequestsFilterExpr, class int) bool {
	for _, s := range expr.statuses {
		if s.class == class {
			return true
		}
	}
	return false
}

func requestsInFlightSelection(expr RequestsFilterExpr) string {
	if expr.inFlight == nil && expr.negInFlight == nil {
		return "any"
	}
	if expr.inFlight != nil && *expr.inFlight {
		return "true"
	}
	if expr.inFlight != nil && !*expr.inFlight {
		return "false"
	}
	return ""
}

// activateFilterMenuCommand handles Filter-menu rows. Returns true when cmd was
// a filter command (including no-op in-flight re-select).
func (b *BaseModel) activateFilterMenuCommand(cmd MenuCommand) bool {
	s := string(cmd)
	switch {
	case cmd == MenuCmdClearFilters:
		b.clearActiveFilter()
		b.updateViewport()
		return true
	case strings.HasPrefix(s, menuCmdToggleProcPrefix):
		proc := strings.TrimPrefix(s, menuCmdToggleProcPrefix)
		expr := b.logsFilterExprForMenu()
		checked := logsProcChecked(expr, proc)
		expr = toggleLogsProc(expr, proc, !checked)
		b.commitLogsFilterExpr(expr)
		return true
	case strings.HasPrefix(s, menuCmdToggleLevelPrefix):
		label := strings.TrimPrefix(s, menuCmdToggleLevelPrefix)
		lvl, ok := logLevelFromMenuLabel(label)
		if !ok {
			return false
		}
		expr := b.logsFilterExprForMenu()
		checked := logsLevelChecked(expr, lvl)
		expr = toggleLogsLevel(expr, lvl, !checked)
		b.commitLogsFilterExpr(expr)
		return true
	case strings.HasPrefix(s, menuCmdToggleMethodPrefix):
		method := strings.TrimPrefix(s, menuCmdToggleMethodPrefix)
		expr := b.requestsFilterExprForMenu()
		checked := requestsMethodChecked(expr, method)
		expr = toggleRequestsMethod(expr, method, !checked)
		b.commitRequestsFilterExpr(expr)
		return true
	case strings.HasPrefix(s, menuCmdToggleStatusPrefix):
		label := strings.TrimPrefix(s, menuCmdToggleStatusPrefix)
		class, ok := statusClassFromMenuLabel(label)
		if !ok {
			return false
		}
		expr := b.requestsFilterExprForMenu()
		checked := requestsStatusClassChecked(expr, class)
		expr = toggleRequestsStatusClass(expr, class, !checked)
		b.commitRequestsFilterExpr(expr)
		return true
	case strings.HasPrefix(s, menuCmdSetInFlightPrefix):
		value := strings.TrimPrefix(s, menuCmdSetInFlightPrefix)
		expr := b.requestsFilterExprForMenu()
		expr = setRequestsInFlight(expr, value)
		b.commitRequestsFilterExpr(expr)
		return true
	default:
		return false
	}
}

func (b *BaseModel) commitLogsFilterExpr(expr LogsFilterExpr) {
	b.setLogsFilterQuery(expr.Serialize())
	b.updateViewport()
}

func (b *BaseModel) commitRequestsFilterExpr(expr RequestsFilterExpr) {
	b.setRequestsFilterQuery(expr.Serialize())
	b.updateViewport()
}

// Toggle semantics (WS8): proc uncheck removes from positive procs if present,
// else appends to negProcs; proc check removes from negProcs only (never adds
// to positives). Level/method/status-class: checked appends to positives,
// unchecked removes. In-flight radio: Any clears both fields; In flight sets
// in_flight:true; Completed sets in_flight:false.
func toggleLogsProc(expr LogsFilterExpr, proc string, checked bool) LogsFilterExpr {
	if checked {
		expr.negProcs = removeString(expr.negProcs, proc)
		return expr
	}
	if stringSliceContains(expr.procs, proc) {
		expr.procs = removeString(expr.procs, proc)
		return expr
	}
	expr.negProcs = appendUniqueString(expr.negProcs, proc)
	return expr
}

func toggleLogsLevel(expr LogsFilterExpr, lvl LogLevel, checked bool) LogsFilterExpr {
	if checked {
		expr.levels = appendUniqueLogLevel(expr.levels, lvl)
	} else {
		expr.levels = removeLogLevel(expr.levels, lvl)
	}
	return expr
}

func toggleRequestsMethod(expr RequestsFilterExpr, method string, checked bool) RequestsFilterExpr {
	if checked {
		expr.methods = appendUniqueStringFold(expr.methods, method)
	} else {
		expr.methods = removeStringFold(expr.methods, method)
	}
	return expr
}

func toggleRequestsStatusClass(expr RequestsFilterExpr, class int, checked bool) RequestsFilterExpr {
	if checked {
		expr.statuses = appendUniqueStatusClass(expr.statuses, class)
	} else {
		expr.statuses = removeStatusClass(expr.statuses, class)
	}
	return expr
}

func setRequestsInFlight(expr RequestsFilterExpr, value string) RequestsFilterExpr {
	switch value {
	case "any":
		expr.inFlight = nil
		expr.negInFlight = nil
	case "true":
		v := true
		expr.inFlight = &v
		expr.negInFlight = nil
	case "false":
		v := false
		expr.inFlight = &v
		expr.negInFlight = nil
	}
	return expr
}

func logLevelFromMenuLabel(label string) (LogLevel, bool) {
	for _, lv := range logsFilterLevels {
		if lv.label == label {
			return lv.level, true
		}
	}
	return 0, false
}

func statusClassFromMenuLabel(label string) (int, bool) {
	for _, sc := range requestsStatusClasses {
		if sc.label == label {
			return sc.class, true
		}
	}
	return 0, false
}

func stringSliceContains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// remove* helpers return FRESH slices: the input expr may share backing
// arrays with LastGood, and in-place filtering (ss[:0]) would corrupt that
// aliased state for any future reader that outlives the commit.
func removeString(ss []string, v string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

func appendUniqueString(ss []string, v string) []string {
	if stringSliceContains(ss, v) {
		return ss
	}
	return append(ss, v)
}

func appendUniqueStringFold(ss []string, v string) []string {
	for _, s := range ss {
		if strings.EqualFold(s, v) {
			return ss
		}
	}
	return append(ss, v)
}

func removeStringFold(ss []string, v string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !strings.EqualFold(s, v) {
			out = append(out, s)
		}
	}
	return out
}

func logLevelSliceContains(ss []LogLevel, lvl LogLevel) bool {
	for _, l := range ss {
		if l == lvl {
			return true
		}
	}
	return false
}

func appendUniqueLogLevel(ss []LogLevel, lvl LogLevel) []LogLevel {
	if logLevelSliceContains(ss, lvl) {
		return ss
	}
	return append(ss, lvl)
}

func removeLogLevel(ss []LogLevel, lvl LogLevel) []LogLevel {
	out := make([]LogLevel, 0, len(ss))
	for _, l := range ss {
		if l != lvl {
			out = append(out, l)
		}
	}
	return out
}

func appendUniqueStatusClass(ss []statusMatch, class int) []statusMatch {
	for _, s := range ss {
		if s.class == class {
			return ss
		}
	}
	return append(ss, statusMatch{class: class})
}

func removeStatusClass(ss []statusMatch, class int) []statusMatch {
	out := make([]statusMatch, 0, len(ss))
	for _, s := range ss {
		if s.class != class {
			out = append(out, s)
		}
	}
	return out
}
