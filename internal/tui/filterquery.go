package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// FilterQueryError is a parse failure with a character offset into the raw
// query (plan 021 WS6). The s-bar uses Pos to surface an "invalid filter" hint
// while LastGood keeps evaluating.
type FilterQueryError struct {
	Pos int
	Msg string
}

func (e *FilterQueryError) Error() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Position returns the byte offset of the offending token.
func (e *FilterQueryError) Position() int {
	if e == nil {
		return 0
	}
	return e.Pos
}

// logsFilterState is the logs-view s-bar state (Codex #3): RawQuery is what the
// bar edits; LastGood is the last successfully parsed expression (kept on
// ParseErr so mid-typing invalid queries don't blank the list); ParseErr is
// non-nil while RawQuery does not parse.
type logsFilterState struct {
	RawQuery string
	LastGood LogsFilterExpr
	ParseErr error
}

// requestsFilterState is the requests-view counterpart of logsFilterState.
type requestsFilterState struct {
	RawQuery string
	LastGood RequestsFilterExpr
	ParseErr error
}

// LogsFilterExpr is the evaluated form of a logs-view filter query (plan 021
// WS6). Within-field positives are OR'd; everything else is AND'd. Empty expr
// matches every entry.
type LogsFilterExpr struct {
	procs     []string   // positive proc: values (OR)
	negProcs  []string   // -proc: values (each AND-excluded)
	levels    []LogLevel // positive level: values (OR)
	negLevels []LogLevel // -level: values (each AND-excluded)
	terms     []string   // bare words (AND, case-insensitive line substrings)
	negTerms  []string   // -bare words (AND-excluded)
}

// RequestsFilterExpr is the evaluated form of a requests-view filter query.
// Empty expr matches every request.
type RequestsFilterExpr struct {
	methods     []string      // positive method: (OR, case-insensitive)
	negMethods  []string      // -method:
	statuses    []statusMatch // positive status: (OR)
	negStatuses []statusMatch // -status:
	hosts       []string      // positive host: (OR, case-insensitive hostname substring)
	negHosts    []string      // -host:
	urls        []string      // positive url: (OR, case-insensitive path+query substring)
	negURLs     []string      // -url:
	inFlight    *bool         // nil = unconstrained; set by in_flight:true|false
	negInFlight *bool         // -in_flight:true|false (match when InFlight != value)
	terms       []string      // bare words AND'd across method+host+url+subdomain
	negTerms    []string      // -bare
}

// statusMatch is either an exact status code or an Nxx class (e.g. 4xx → 400–499).
type statusMatch struct {
	exact int // >0 when exact
	class int // 1–5 when Nxx (hundreds digit); 0 when exact
}

func (s statusMatch) matches(code int) bool {
	if s.exact > 0 {
		return code == s.exact
	}
	if s.class > 0 {
		lo := s.class * 100
		return code >= lo && code < lo+100
	}
	return false
}

func (s statusMatch) String() string {
	if s.exact > 0 {
		return strconv.Itoa(s.exact)
	}
	if s.class > 0 {
		return fmt.Sprintf("%dxx", s.class)
	}
	return ""
}

// token is one space-separated query atom with its start offset.
type filterToken struct {
	raw  string
	pos  int // byte offset in the original query
	neg  bool
	body string // raw without leading '-'
}

// isFilterSpace reports ASCII whitespace only. Bytes are decoded one at a
// time during the scan; a UTF-8 continuation byte (0x80-0xBF) cast to rune
// must never be treated as a separator (CodeRabbit PR #102).
func isFilterSpace(b byte) bool {
	return b < utf8.RuneSelf && unicode.IsSpace(rune(b))
}

// tokenizeFilter splits query into tokens, honouring field:"quoted value".
// Empty/whitespace-only input yields a nil token slice (valid empty query).
func tokenizeFilter(query string) ([]filterToken, error) {
	var toks []filterToken
	i := 0
	n := len(query)
	for i < n {
		for i < n && isFilterSpace(query[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		neg := false
		if query[i] == '-' {
			neg = true
			i++
			if i >= n || isFilterSpace(query[i]) {
				return nil, &FilterQueryError{Pos: start, Msg: "lonely negation"}
			}
		}
		bodyStart := i
		// field:"quoted" — scan to closing quote after the first colon+quote.
		// The colon lookahead is bounded to the CURRENT token: searching the
		// whole remainder would glue `foo bar:"x y"` into one token and hide
		// the bar field (CodeRabbit PR #102).
		if colon := indexByteWithinToken(query, i, ':'); colon >= 0 {
			afterColon := colon + 1
			if afterColon < n && query[afterColon] == '"' {
				j := afterColon + 1
				for j < n && query[j] != '"' {
					j++
				}
				if j >= n {
					return nil, &FilterQueryError{Pos: afterColon, Msg: "unclosed quote"}
				}
				i = j + 1
				body := query[bodyStart:i]
				toks = append(toks, filterToken{raw: query[start:i], pos: start, neg: neg, body: body})
				continue
			}
		}
		// Unquoted token: run to next whitespace.
		for i < n && !isFilterSpace(query[i]) {
			i++
		}
		body := query[bodyStart:i]
		toks = append(toks, filterToken{raw: query[start:i], pos: start, neg: neg, body: body})
	}
	return toks, nil
}

// indexByteWithinToken finds c starting at i, stopping at the end of the
// current token (next ASCII whitespace). Returns the absolute index or -1.
func indexByteWithinToken(query string, i int, c byte) int {
	for j := i; j < len(query); j++ {
		if isFilterSpace(query[j]) {
			return -1
		}
		if query[j] == c {
			return j
		}
	}
	return -1
}

// splitFieldValue splits "field:value" / `field:"quoted"`. ok is false when
// there is no colon (bare term) — or when the pre-colon part is not
// field-shaped. Fields are lowercase alpha/underscore only (all known fields
// match), so colon-bearing terms users substring-search today (`15:04`,
// `a.b:1`) stay bare terms instead of erroring — back-compat with the
// pre-query `s` substring. A field-shaped unknown (`levl:error`) still
// errors per plan.

func splitFieldValue(body string) (field, value string, ok bool) {
	colon := strings.IndexByte(body, ':')
	if colon <= 0 {
		return "", "", false
	}
	field = body[:colon]
	for _, r := range field {
		if (r < 'a' || r > 'z') && r != '_' {
			return "", "", false
		}
	}
	value = body[colon+1:]
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return field, value, true
}

// ParseLogsFilter parses a logs-view filter query (plan 021 WS6). Empty query
// yields a zero LogsFilterExpr that matches everything.
func ParseLogsFilter(query string) (LogsFilterExpr, error) {
	var expr LogsFilterExpr
	toks, err := tokenizeFilter(query)
	if err != nil {
		return LogsFilterExpr{}, err
	}
	for _, tok := range toks {
		field, value, isField := splitFieldValue(tok.body)
		if !isField {
			if tok.body == "" {
				return LogsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty term"}
			}
			if tok.neg {
				expr.negTerms = append(expr.negTerms, tok.body)
			} else {
				expr.terms = append(expr.terms, tok.body)
			}
			continue
		}
		switch strings.ToLower(field) {
		case "proc":
			if value == "" {
				return LogsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty proc value"}
			}
			if tok.neg {
				expr.negProcs = append(expr.negProcs, value)
			} else {
				expr.procs = append(expr.procs, value)
			}
		case "level":
			lvl, ok := normalizeLevel(value)
			if !ok {
				return LogsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: fmt.Sprintf("bad level %q", value)}
			}
			if tok.neg {
				expr.negLevels = append(expr.negLevels, lvl)
			} else {
				expr.levels = append(expr.levels, lvl)
			}
		default:
			return LogsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: fmt.Sprintf("unknown field %q", field)}
		}
	}
	return expr, nil
}

// ParseRequestsFilter parses a requests-view filter query (plan 021 WS6).
func ParseRequestsFilter(query string) (RequestsFilterExpr, error) {
	var expr RequestsFilterExpr
	toks, err := tokenizeFilter(query)
	if err != nil {
		return RequestsFilterExpr{}, err
	}
	for _, tok := range toks {
		field, value, isField := splitFieldValue(tok.body)
		if !isField {
			if tok.body == "" {
				return RequestsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty term"}
			}
			if tok.neg {
				expr.negTerms = append(expr.negTerms, tok.body)
			} else {
				expr.terms = append(expr.terms, tok.body)
			}
			continue
		}
		switch strings.ToLower(field) {
		case "method":
			if value == "" {
				return RequestsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty method value"}
			}
			if tok.neg {
				expr.negMethods = append(expr.negMethods, value)
			} else {
				expr.methods = append(expr.methods, value)
			}
		case "status":
			sm, err := parseStatusMatch(value, tok.pos)
			if err != nil {
				return RequestsFilterExpr{}, err
			}
			if tok.neg {
				expr.negStatuses = append(expr.negStatuses, sm)
			} else {
				expr.statuses = append(expr.statuses, sm)
			}
		case "host":
			if value == "" {
				return RequestsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty host value"}
			}
			if tok.neg {
				expr.negHosts = append(expr.negHosts, value)
			} else {
				expr.hosts = append(expr.hosts, value)
			}
		case "url":
			if value == "" {
				return RequestsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: "empty url value"}
			}
			if tok.neg {
				expr.negURLs = append(expr.negURLs, value)
			} else {
				expr.urls = append(expr.urls, value)
			}
		case "in_flight":
			v, err := parseInFlight(value, tok.pos)
			if err != nil {
				return RequestsFilterExpr{}, err
			}
			// Single-valued: last write wins (still one constraint).
			if tok.neg {
				expr.negInFlight = &v
				expr.inFlight = nil
			} else {
				expr.inFlight = &v
				expr.negInFlight = nil
			}
		default:
			return RequestsFilterExpr{}, &FilterQueryError{Pos: tok.pos, Msg: fmt.Sprintf("unknown field %q", field)}
		}
	}
	return expr, nil
}

func parseStatusMatch(value string, pos int) (statusMatch, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if len(v) == 3 && v[1] == 'x' && v[2] == 'x' && v[0] >= '1' && v[0] <= '5' {
		return statusMatch{class: int(v[0] - '0')}, nil
	}
	code, err := strconv.Atoi(v)
	if err != nil || code < 100 || code > 599 {
		return statusMatch{}, &FilterQueryError{Pos: pos, Msg: fmt.Sprintf("bad status %q", value)}
	}
	return statusMatch{exact: code}, nil
}

func parseInFlight(value string, pos int) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &FilterQueryError{Pos: pos, Msg: fmt.Sprintf("bad in_flight %q", value)}
	}
}

// Match reports whether entry passes the logs filter. meta supplies the C6
// logMeta cache entry (hasLevel/level); entries with no detected level match
// NO positive level: clause.
func (e LogsFilterExpr) Match(entry domain.LogEntry, meta logMeta) bool {
	if len(e.procs) > 0 {
		ok := false
		for _, p := range e.procs {
			if entry.Process == p {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, p := range e.negProcs {
		if entry.Process == p {
			return false
		}
	}

	if len(e.levels) > 0 {
		if !meta.hasLevel {
			return false
		}
		ok := false
		for _, l := range e.levels {
			if meta.level == l {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, l := range e.negLevels {
		if meta.hasLevel && meta.level == l {
			return false
		}
	}

	for _, t := range e.terms {
		if !containsIgnoreCase(entry.Line, t) {
			return false
		}
	}
	for _, t := range e.negTerms {
		if containsIgnoreCase(entry.Line, t) {
			return false
		}
	}
	return true
}

// Match reports whether req passes the requests filter.
func (e RequestsFilterExpr) Match(req proxy.RequestRecord) bool {
	if len(e.methods) > 0 {
		ok := false
		for _, m := range e.methods {
			if strings.EqualFold(req.Method, m) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, m := range e.negMethods {
		if strings.EqualFold(req.Method, m) {
			return false
		}
	}

	if len(e.statuses) > 0 {
		ok := false
		for _, s := range e.statuses {
			if s.matches(req.StatusCode) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, s := range e.negStatuses {
		if s.matches(req.StatusCode) {
			return false
		}
	}

	if len(e.hosts) > 0 {
		ok := false
		for _, h := range e.hosts {
			if containsIgnoreCase(req.Hostname, h) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, h := range e.negHosts {
		if containsIgnoreCase(req.Hostname, h) {
			return false
		}
	}

	if len(e.urls) > 0 {
		ok := false
		for _, u := range e.urls {
			if containsIgnoreCase(req.URL, u) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, u := range e.negURLs {
		if containsIgnoreCase(req.URL, u) {
			return false
		}
	}

	if e.inFlight != nil && req.InFlight != *e.inFlight {
		return false
	}
	if e.negInFlight != nil && req.InFlight == *e.negInFlight {
		return false
	}

	// Bare terms: AND'd; each matches if it appears in method OR host OR url
	// OR subdomain (today's s-bar OR across fields, plus Hostname for host: parity).
	for _, t := range e.terms {
		if !requestBareMatch(req, t) {
			return false
		}
	}
	for _, t := range e.negTerms {
		if requestBareMatch(req, t) {
			return false
		}
	}
	return true
}

func requestBareMatch(req proxy.RequestRecord, term string) bool {
	return containsIgnoreCase(req.URL, term) ||
		containsIgnoreCase(req.Method, term) ||
		containsIgnoreCase(req.Subdomain, term) ||
		containsIgnoreCase(req.Hostname, term)
}

// Serialize returns the canonical string form of a logs filter (C8 menu edits
// regenerate the bar from this). Field order: proc, level, then bare terms;
// positives before negations within each group. Round-trips stably through
// ParseLogsFilter.
func (e LogsFilterExpr) Serialize() string {
	var parts []string
	for _, p := range e.procs {
		parts = append(parts, "proc:"+quoteIfNeeded(p))
	}
	for _, p := range e.negProcs {
		parts = append(parts, "-proc:"+quoteIfNeeded(p))
	}
	for _, l := range e.levels {
		parts = append(parts, "level:"+levelToken(l))
	}
	for _, l := range e.negLevels {
		parts = append(parts, "-level:"+levelToken(l))
	}
	for _, t := range e.terms {
		parts = append(parts, quoteIfNeeded(t))
	}
	for _, t := range e.negTerms {
		parts = append(parts, "-"+quoteIfNeeded(t))
	}
	return strings.Join(parts, " ")
}

// Serialize returns the canonical string form of a requests filter.
// Order: method, status, host, url, in_flight, then bare terms.
func (e RequestsFilterExpr) Serialize() string {
	var parts []string
	for _, m := range e.methods {
		parts = append(parts, "method:"+quoteIfNeeded(m))
	}
	for _, m := range e.negMethods {
		parts = append(parts, "-method:"+quoteIfNeeded(m))
	}
	for _, s := range e.statuses {
		parts = append(parts, "status:"+s.String())
	}
	for _, s := range e.negStatuses {
		parts = append(parts, "-status:"+s.String())
	}
	for _, h := range e.hosts {
		parts = append(parts, "host:"+quoteIfNeeded(h))
	}
	for _, h := range e.negHosts {
		parts = append(parts, "-host:"+quoteIfNeeded(h))
	}
	for _, u := range e.urls {
		parts = append(parts, "url:"+quoteIfNeeded(u))
	}
	for _, u := range e.negURLs {
		parts = append(parts, "-url:"+quoteIfNeeded(u))
	}
	if e.inFlight != nil {
		parts = append(parts, "in_flight:"+strconv.FormatBool(*e.inFlight))
	}
	if e.negInFlight != nil {
		parts = append(parts, "-in_flight:"+strconv.FormatBool(*e.negInFlight))
	}
	for _, t := range e.terms {
		parts = append(parts, quoteIfNeeded(t))
	}
	for _, t := range e.negTerms {
		parts = append(parts, "-"+quoteIfNeeded(t))
	}
	return strings.Join(parts, " ")
}

// quoteIfNeeded quotes only when whitespace (or emptiness) demands it. A
// value containing `"` but no whitespace is left bare — the unquoted
// tokenizer round-trips it intact, whereas quoting would force a strip.
func quoteIfNeeded(v string) string {
	if v == "" || strings.ContainsAny(v, " \t") {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return v
}

func levelToken(l LogLevel) string {
	switch l {
	case LogLevelTrace:
		return "trace"
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	default:
		return "unknown"
	}
}

// IsEmpty reports whether the expr imposes no constraints.
func (e LogsFilterExpr) IsEmpty() bool {
	return len(e.procs) == 0 && len(e.negProcs) == 0 &&
		len(e.levels) == 0 && len(e.negLevels) == 0 &&
		len(e.terms) == 0 && len(e.negTerms) == 0
}

// IsEmpty reports whether the expr imposes no constraints.
func (e RequestsFilterExpr) IsEmpty() bool {
	return len(e.methods) == 0 && len(e.negMethods) == 0 &&
		len(e.statuses) == 0 && len(e.negStatuses) == 0 &&
		len(e.hosts) == 0 && len(e.negHosts) == 0 &&
		len(e.urls) == 0 && len(e.negURLs) == 0 &&
		e.inFlight == nil && e.negInFlight == nil &&
		len(e.terms) == 0 && len(e.negTerms) == 0
}
