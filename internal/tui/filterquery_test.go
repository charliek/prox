package tui

import (
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

func TestParseLogsFilter(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
		errPos  int
		check   func(t *testing.T, e LogsFilterExpr)
	}{
		{
			name:  "empty",
			query: "",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.True(t, e.IsEmpty())
			},
		},
		{
			name:  "whitespace only",
			query: "   \t  ",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.True(t, e.IsEmpty())
			},
		},
		{
			// The quoted-field colon lookahead must stop at the token
			// boundary — searching the whole remainder glued the bare word,
			// the field, and its value into one bare term (CodeRabbit #102).
			name:  "bare word before quoted field",
			query: `foo proc:"my app"`,
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"my app"}, e.procs)
				assert.Equal(t, []string{"foo"}, e.terms)
			},
		},
		{
			name:  "proc single",
			query: "proc:api",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"api"}, e.procs)
			},
		},
		{
			name:  "proc repeatable OR",
			query: "proc:api proc:web",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"api", "web"}, e.procs)
			},
		},
		{
			name:  "level each known",
			query: "level:trace level:debug level:info level:warn level:error",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []LogLevel{
					LogLevelTrace, LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError,
				}, e.levels)
			},
		},
		{
			name:  "level aliases warning fatal",
			query: "level:warning level:fatal",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []LogLevel{LogLevelWarn, LogLevelError}, e.levels)
			},
		},
		{
			name:  "negation proc and bare",
			query: "-proc:api -health",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"api"}, e.negProcs)
				assert.Equal(t, []string{"health"}, e.negTerms)
			},
		},
		{
			name:  "mixed bare AND",
			query: "foo bar",
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"foo", "bar"}, e.terms)
			},
		},
		{
			name:  "quoted value",
			query: `proc:"my app" level:error`,
			check: func(t *testing.T, e LogsFilterExpr) {
				assert.Equal(t, []string{"my app"}, e.procs)
				assert.Equal(t, []LogLevel{LogLevelError}, e.levels)
			},
		},
		{
			name:  "re single",
			query: "re:timeout",
			check: func(t *testing.T, e LogsFilterExpr) {
				require.Len(t, e.res, 1)
				assert.Equal(t, "timeout", e.res[0].String())
			},
		},
		{
			name:  "re quoted whitespace",
			query: `re:"foo bar"`,
			check: func(t *testing.T, e LogsFilterExpr) {
				require.Len(t, e.res, 1)
				assert.Equal(t, "foo bar", e.res[0].String())
			},
		},
		{
			name:  "re multiple AND + negation",
			query: `re:err.* re:timeout -re:health`,
			check: func(t *testing.T, e LogsFilterExpr) {
				require.Len(t, e.res, 2)
				assert.Equal(t, "err.*", e.res[0].String())
				assert.Equal(t, "timeout", e.res[1].String())
				require.Len(t, e.negRes, 1)
				assert.Equal(t, "health", e.negRes[0].String())
			},
		},
		{
			name:  "re case-sensitive flag",
			query: `re:(?i)Error`,
			check: func(t *testing.T, e LogsFilterExpr) {
				require.Len(t, e.res, 1)
				assert.Equal(t, "(?i)Error", e.res[0].String())
			},
		},
		{
			name:    "unknown field",
			query:   "method:GET",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad level",
			query:   "level:chatty",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad level mid query",
			query:   "proc:api level:chatty",
			wantErr: true,
			errPos:  9,
		},
		{
			name:    "unknown field mid",
			query:   "foo status:200",
			wantErr: true,
			errPos:  4,
		},
		{
			name:    "empty re",
			query:   "re:",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad re compile",
			query:   "re:(unclosed",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "re too long",
			query:   "re:" + strings.Repeat("a", 257),
			wantErr: true,
			errPos:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := ParseLogsFilter(tt.query)
			if tt.wantErr {
				require.Error(t, err)
				var fq *FilterQueryError
				require.ErrorAs(t, err, &fq)
				assert.Equal(t, tt.errPos, fq.Position())
				return
			}
			require.NoError(t, err)
			tt.check(t, e)
		})
	}
}

func TestParseRequestsFilter(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
		errPos  int
		check   func(t *testing.T, e RequestsFilterExpr)
	}{
		{
			name:  "empty",
			query: "",
			check: func(t *testing.T, e RequestsFilterExpr) {
				assert.True(t, e.IsEmpty())
			},
		},
		{
			name:  "method repeatable",
			query: "method:GET method:post",
			check: func(t *testing.T, e RequestsFilterExpr) {
				assert.Equal(t, []string{"GET", "post"}, e.methods)
			},
		},
		{
			name:  "status exact",
			query: "status:200",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 1)
				assert.Equal(t, 200, e.statuses[0].exact)
			},
		},
		{
			name:  "status class",
			query: "status:4xx status:5XX",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 2)
				assert.Equal(t, 4, e.statuses[0].class)
				assert.Equal(t, 5, e.statuses[1].class)
			},
		},
		{
			name:  "status gte",
			query: "status:>=400",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 1)
				assert.Equal(t, 400, e.statuses[0].min)
				assert.Equal(t, 0, e.statuses[0].max)
				assert.Equal(t, 0, e.statuses[0].exact)
			},
		},
		{
			name:  "status lte",
			query: "status:<=299",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 1)
				assert.Equal(t, 299, e.statuses[0].max)
				assert.Equal(t, 0, e.statuses[0].min)
			},
		},
		{
			name:  "status inclusive range",
			query: "status:200-399",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 1)
				assert.Equal(t, 200, e.statuses[0].min)
				assert.Equal(t, 399, e.statuses[0].max)
			},
		},
		{
			name:  "status range + neg gte",
			query: "status:200-299 -status:>=500",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.Len(t, e.statuses, 1)
				assert.Equal(t, 200, e.statuses[0].min)
				assert.Equal(t, 299, e.statuses[0].max)
				require.Len(t, e.negStatuses, 1)
				assert.Equal(t, 500, e.negStatuses[0].min)
			},
		},
		{
			name:  "host url",
			query: "host:api.local url:/orders",
			check: func(t *testing.T, e RequestsFilterExpr) {
				assert.Equal(t, []string{"api.local"}, e.hosts)
				assert.Equal(t, []string{"/orders"}, e.urls)
			},
		},
		{
			name:  "in_flight true",
			query: "in_flight:true",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.NotNil(t, e.inFlight)
				assert.True(t, *e.inFlight)
			},
		},
		{
			name:  "in_flight false",
			query: "in_flight:false",
			check: func(t *testing.T, e RequestsFilterExpr) {
				require.NotNil(t, e.inFlight)
				assert.False(t, *e.inFlight)
			},
		},
		{
			name:  "negation and bare",
			query: "-method:GET -health foo",
			check: func(t *testing.T, e RequestsFilterExpr) {
				assert.Equal(t, []string{"GET"}, e.negMethods)
				assert.Equal(t, []string{"health"}, e.negTerms)
				assert.Equal(t, []string{"foo"}, e.terms)
			},
		},
		{
			name:    "unknown field",
			query:   "proc:api",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "sub field removed",
			query:   "sub:web",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad status",
			query:   "status:abc",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad in_flight",
			query:   "in_flight:maybe",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "bad status class digit",
			query:   "status:9xx",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status reversed range",
			query:   "status:399-200",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status range endpoint low",
			query:   "status:99-200",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status range endpoint high",
			query:   "status:200-600",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status gte out of range",
			query:   "status:>=99",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status lte out of range",
			query:   "status:<=600",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status malformed operator gt",
			query:   "status:>400",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status malformed operator lt",
			query:   "status:<500",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status partial gte",
			query:   "status:>=",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status partial range hi",
			query:   "status:200-",
			wantErr: true,
			errPos:  0,
		},
		{
			name:    "status partial number in gte",
			query:   "status:>=4xx",
			wantErr: true,
			errPos:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := ParseRequestsFilter(tt.query)
			if tt.wantErr {
				require.Error(t, err)
				var fq *FilterQueryError
				require.ErrorAs(t, err, &fq)
				assert.Equal(t, tt.errPos, fq.Position())
				return
			}
			require.NoError(t, err)
			tt.check(t, e)
		})
	}
}

func TestLogsFilterExpr_Match(t *testing.T) {
	mk := func(proc, line string) domain.LogEntry {
		return domain.LogEntry{Process: proc, Line: line, DisplaySeq: 1}
	}
	meta := func(lvl LogLevel, has bool) logMeta {
		return logMeta{level: lvl, hasLevel: has}
	}

	expr, err := ParseLogsFilter("proc:api proc:web level:error -health")
	require.NoError(t, err)

	assert.True(t, expr.Match(mk("api", "boom error"), meta(LogLevelError, true)))
	assert.True(t, expr.Match(mk("web", "fatal"), meta(LogLevelError, true)))
	assert.False(t, expr.Match(mk("db", "boom error"), meta(LogLevelError, true)), "proc OR miss")
	assert.False(t, expr.Match(mk("api", "ok"), meta(LogLevelInfo, true)), "level miss")
	assert.False(t, expr.Match(mk("api", "no level"), meta(0, false)), "unknown level excluded")
	assert.False(t, expr.Match(mk("api", "health check error"), meta(LogLevelError, true)), "neg bare")

	// level OR
	expr2, err := ParseLogsFilter("level:warn level:error")
	require.NoError(t, err)
	assert.True(t, expr2.Match(mk("x", "w"), meta(LogLevelWarn, true)))
	assert.True(t, expr2.Match(mk("x", "e"), meta(LogLevelError, true)))
	assert.False(t, expr2.Match(mk("x", "i"), meta(LogLevelInfo, true)))

	// bare AND
	expr3, err := ParseLogsFilter("foo bar")
	require.NoError(t, err)
	assert.True(t, expr3.Match(mk("x", "foo and bar"), meta(0, false)))
	assert.False(t, expr3.Match(mk("x", "foo only"), meta(0, false)))

	// -level
	expr4, err := ParseLogsFilter("-level:error")
	require.NoError(t, err)
	assert.True(t, expr4.Match(mk("x", "info"), meta(LogLevelInfo, true)))
	assert.True(t, expr4.Match(mk("x", "unknown"), meta(0, false)), "unknown passes -level")
	assert.False(t, expr4.Match(mk("x", "err"), meta(LogLevelError, true)))

	// re: AND + -re: against raw line (case-sensitive by default)
	expr5, err := ParseLogsFilter(`re:err.* re:timeout -re:health`)
	require.NoError(t, err)
	assert.True(t, expr5.Match(mk("x", "error: timeout"), meta(0, false)))
	assert.False(t, expr5.Match(mk("x", "error only"), meta(0, false)), "missing second re")
	assert.False(t, expr5.Match(mk("x", "Error: timeout"), meta(0, false)), "case-sensitive")
	assert.False(t, expr5.Match(mk("x", "error: timeout health"), meta(0, false)), "neg re")

	expr6, err := ParseLogsFilter(`re:(?i)Error`)
	require.NoError(t, err)
	assert.True(t, expr6.Match(mk("x", "error"), meta(0, false)))
	assert.True(t, expr6.Match(mk("x", "ERROR"), meta(0, false)))
}

func TestRequestsFilterExpr_Match(t *testing.T) {
	mk := func(method, host, sub, url string, status int, inFlight bool) proxy.RequestRecord {
		return proxy.RequestRecord{
			Method: method, Hostname: host, Subdomain: sub, URL: url,
			StatusCode: status, InFlight: inFlight,
		}
	}

	expr, err := ParseRequestsFilter("method:POST status:5xx url:/orders")
	require.NoError(t, err)
	assert.True(t, expr.Match(mk("POST", "", "api", "/orders/1", 500, false)))
	assert.True(t, expr.Match(mk("post", "", "api", "/orders", 503, false)), "method case-insensitive")
	assert.False(t, expr.Match(mk("GET", "", "api", "/orders", 500, false)))
	assert.False(t, expr.Match(mk("POST", "", "api", "/orders", 404, false)))
	assert.False(t, expr.Match(mk("POST", "", "api", "/users", 500, false)))

	// status exact
	expr2, err := ParseRequestsFilter("status:200")
	require.NoError(t, err)
	assert.True(t, expr2.Match(mk("GET", "", "", "/", 200, false)))
	assert.False(t, expr2.Match(mk("GET", "", "", "/", 201, false)))

	// host
	expr3, err := ParseRequestsFilter("host:api.local")
	require.NoError(t, err)
	assert.True(t, expr3.Match(mk("GET", "api.local.dev", "", "/", 200, false)))
	assert.False(t, expr3.Match(mk("GET", "web.local.dev", "", "/", 200, false)))

	// in_flight
	expr4, err := ParseRequestsFilter("in_flight:true")
	require.NoError(t, err)
	assert.True(t, expr4.Match(mk("GET", "", "", "/", 0, true)))
	assert.False(t, expr4.Match(mk("GET", "", "", "/", 200, false)))

	expr5, err := ParseRequestsFilter("in_flight:false")
	require.NoError(t, err)
	assert.True(t, expr5.Match(mk("GET", "", "", "/", 200, false)))
	assert.False(t, expr5.Match(mk("GET", "", "", "/", 0, true)))

	// bare across method/host/url/subdomain (today's OR semantics per term)
	expr6, err := ParseRequestsFilter("api")
	require.NoError(t, err)
	assert.True(t, expr6.Match(mk("GET", "", "api", "/x", 200, false)), "subdomain bare")
	assert.True(t, expr6.Match(mk("GET", "api.host", "", "/x", 200, false)), "hostname bare")
	assert.True(t, expr6.Match(mk("GET", "", "", "/api/x", 200, false)), "url bare")
	assert.False(t, expr6.Match(mk("GET", "", "web", "/users", 200, false)))

	// negation
	expr7, err := ParseRequestsFilter("-method:GET")
	require.NoError(t, err)
	assert.False(t, expr7.Match(mk("GET", "", "", "/", 200, false)))
	assert.True(t, expr7.Match(mk("POST", "", "", "/", 200, false)))

	// status >= / <= / range
	expr8, err := ParseRequestsFilter("status:>=500")
	require.NoError(t, err)
	assert.True(t, expr8.Match(mk("GET", "", "", "/", 500, false)))
	assert.True(t, expr8.Match(mk("GET", "", "", "/", 599, false)))
	assert.False(t, expr8.Match(mk("GET", "", "", "/", 499, false)))

	expr9, err := ParseRequestsFilter("status:<=299")
	require.NoError(t, err)
	assert.True(t, expr9.Match(mk("GET", "", "", "/", 200, false)))
	assert.True(t, expr9.Match(mk("GET", "", "", "/", 299, false)))
	assert.False(t, expr9.Match(mk("GET", "", "", "/", 300, false)))

	expr10, err := ParseRequestsFilter("status:200-399")
	require.NoError(t, err)
	assert.True(t, expr10.Match(mk("GET", "", "", "/", 200, false)))
	assert.True(t, expr10.Match(mk("GET", "", "", "/", 399, false)))
	assert.False(t, expr10.Match(mk("GET", "", "", "/", 199, false)))
	assert.False(t, expr10.Match(mk("GET", "", "", "/", 400, false)))
}

func TestFilterSerializeRoundTrip(t *testing.T) {
	logsQueries := []string{
		"",
		"proc:api",
		"proc:api proc:web level:error -health",
		"level:warn -proc:db foo",
		`proc:"my app" level:info`,
		`re:timeout`,
		`re:"foo bar" -re:health`,
		`re:err.* re:timeout`,
	}
	for _, q := range logsQueries {
		t.Run("logs/"+q, func(t *testing.T) {
			e1, err := ParseLogsFilter(q)
			require.NoError(t, err)
			s1 := e1.Serialize()
			e2, err := ParseLogsFilter(s1)
			require.NoError(t, err)
			assert.True(t, equalLogsFilterExpr(e1, e2), "parse→serialize→parse must be expr-equivalent\n  in=%q\n  ser=%q\n  e1=%+v\n  e2=%+v", q, s1, e1, e2)
			assert.Equal(t, s1, e2.Serialize(), "serialize must be stable")
			if q == "" {
				assert.Empty(t, s1)
			}
		})
	}

	reqQueries := []string{
		"",
		"method:POST status:5xx url:/orders",
		"status:200 host:api in_flight:true -health",
		"method:GET method:POST status:4xx",
		"status:>=400",
		"status:<=299",
		"status:200-399",
		"status:>=500 -status:<=199",
	}
	for _, q := range reqQueries {
		t.Run("requests/"+q, func(t *testing.T) {
			e1, err := ParseRequestsFilter(q)
			require.NoError(t, err)
			s1 := e1.Serialize()
			e2, err := ParseRequestsFilter(s1)
			require.NoError(t, err)
			assert.True(t, equalRequestsFilterExpr(e1, e2), "parse→serialize→parse must be expr-equivalent\n  in=%q\n  ser=%q", q, s1)
			assert.Equal(t, s1, e2.Serialize())
		})
	}

	// Menu-edit-friendly ordering: fields before bare, positives before negs in group.
	e, err := ParseLogsFilter("-health level:error proc:web proc:api re:boom")
	require.NoError(t, err)
	assert.Equal(t, "proc:web proc:api level:error re:boom -health", e.Serialize())

	// re: with whitespace uses quoted field form.
	eRe, err := ParseLogsFilter(`re:"foo bar"`)
	require.NoError(t, err)
	assert.Equal(t, `re:"foo bar"`, eRe.Serialize())
}

// Confirmed B4 failing shapes (plan 023 §2): non-field-shaped colon+quoted
// tokens become bare terms that must serialize verbatim.
func TestSerializeBareTermVerbatim(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "non-field colon+quote", query: `a.b:"x y"`, want: `a.b:"x y"`},
		{name: "negated non-field colon+quote", query: `-a.b:"x y"`, want: `-a.b:"x y"`},
		{name: "colon-leading quoted", query: `:"a b:"`, want: `:"a b:"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e1, err := ParseLogsFilter(tt.query)
			require.NoError(t, err)
			got := e1.Serialize()
			assert.Equal(t, tt.want, got)
			e2, err := ParseLogsFilter(got)
			require.NoError(t, err)
			assert.True(t, equalLogsFilterExpr(e1, e2), "round-trip expr mismatch\n  ser=%q\n  e1=%+v\n  e2=%+v", got, e1, e2)

			// Same shapes are legal bare terms on the requests grammar too.
			r1, err := ParseRequestsFilter(tt.query)
			require.NoError(t, err)
			rs := r1.Serialize()
			assert.Equal(t, tt.want, rs)
			r2, err := ParseRequestsFilter(rs)
			require.NoError(t, err)
			assert.True(t, equalRequestsFilterExpr(r1, r2))
		})
	}
}

func TestSerializeDropsUnknownLevels(t *testing.T) {
	e := LogsFilterExpr{
		levels:    []LogLevel{LogLevelError, LogLevelUnknown, LogLevelInfo},
		negLevels: []LogLevel{LogLevelUnknown, LogLevelWarn},
		terms:     []string{"keep"},
	}
	assert.Equal(t, "level:error level:info -level:warn keep", e.Serialize())

	// Equivalence ignores Unknown on both sides.
	withUnknown := LogsFilterExpr{levels: []LogLevel{LogLevelInfo, LogLevelUnknown}}
	without := LogsFilterExpr{levels: []LogLevel{LogLevelInfo}}
	assert.True(t, equalLogsFilterExpr(withUnknown, without))
}

func TestQuoteIfNeededUsesFilterSpace(t *testing.T) {
	// Tab / newline / CR are isFilterSpace — field values must quote them.
	assert.Equal(t, `"a`+"\t"+`b"`, quoteIfNeeded("a\tb"))
	assert.Equal(t, `"a`+"\n"+`b"`, quoteIfNeeded("a\nb"))
	assert.Equal(t, `"a b"`, quoteIfNeeded("a b"))
	assert.Equal(t, "plain", quoteIfNeeded("plain"))
}

// Values with both whitespace and `"` are unrepresentable (no escapes). The
// tokenizer ends the quoted field at the first closing quote; trailing junk
// is a clear parse error (plan 023 A5).
func TestParseRejectsQuoteEscapeInRE(t *testing.T) {
	_, err := ParseLogsFilter(`re:"a \"b\""`)
	require.Error(t, err)
	var fq *FilterQueryError
	require.ErrorAs(t, err, &fq)
	assert.Contains(t, fq.Msg, "junk after closing quote")
}

func TestParseStatusMatchDistinctErrors(t *testing.T) {
	tests := []struct {
		query   string
		msgPart string
	}{
		{"status:399-200", "reversed status range"},
		{"status:99-200", "out of range"},
		{"status:>=600", "out of range"},
		{"status:>400", "malformed status operator"},
		{"status:>=", "partial status number"},
		{"status:200-", "partial status number"},
		{"status:>=4xx", "partial status number"},
		{"status:abc", "bad status"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			_, err := ParseRequestsFilter(tt.query)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.msgPart)
		})
	}
}

// Property: every parseable query survives parse → serialize → parse with
// expr-equivalence. Seeded with the confirmed B4 shapes; then random mixes.
func TestSerializeRoundTripProperty(t *testing.T) {
	const iterations = 500
	rng := rand.New(rand.NewSource(23)) // deterministic corpus

	logsSeeds := []string{
		`a.b:"x y"`,
		`-a.b:"x y"`,
		`:"a b:"`,
		"",
		"proc:api",
		`proc:"my app" level:error -health`,
		"level:warn -proc:db foo bar",
		`15:04 a.b:1`,
		`re:timeout`,
		`re:"foo bar" -re:health`,
		`re:(?i)err.*`,
	}
	for _, q := range logsSeeds {
		assertLogsRoundTrip(t, q)
	}
	for i := 0; i < iterations; i++ {
		assertLogsRoundTrip(t, genLogsFilterQuery(rng))
	}

	reqSeeds := []string{
		`a.b:"x y"`,
		`-a.b:"x y"`,
		`:"a b:"`,
		"",
		"method:POST status:5xx url:/orders",
		"status:200 host:api in_flight:true -health",
		`method:GET host:"my host" url:"/a b"`,
		"status:>=400",
		"status:<=299",
		"status:200-399",
	}
	for _, q := range reqSeeds {
		assertRequestsRoundTrip(t, q)
	}
	for i := 0; i < iterations; i++ {
		assertRequestsRoundTrip(t, genRequestsFilterQuery(rng))
	}
}

func assertLogsRoundTrip(t *testing.T, q string) {
	t.Helper()
	e1, err := ParseLogsFilter(q)
	require.NoError(t, err, "seed/gen must be parseable: %q", q)
	s := e1.Serialize()
	e2, err := ParseLogsFilter(s)
	require.NoError(t, err, "serialize must reparse: in=%q ser=%q", q, s)
	if !equalLogsFilterExpr(e1, e2) {
		t.Fatalf("expr inequivalent after round-trip\n  in=%q\n  ser=%q\n  e1=%+v\n  e2=%+v", q, s, e1, e2)
	}
}

func assertRequestsRoundTrip(t *testing.T, q string) {
	t.Helper()
	e1, err := ParseRequestsFilter(q)
	require.NoError(t, err, "seed/gen must be parseable: %q", q)
	s := e1.Serialize()
	e2, err := ParseRequestsFilter(s)
	require.NoError(t, err, "serialize must reparse: in=%q ser=%q", q, s)
	if !equalRequestsFilterExpr(e1, e2) {
		t.Fatalf("expr inequivalent after round-trip\n  in=%q\n  ser=%q\n  e1=%+v\n  e2=%+v", q, s, e1, e2)
	}
}

func genLogsFilterQuery(rng *rand.Rand) string {
	var parts []string
	n := rng.Intn(5) // 0..4 atoms
	for i := 0; i < n; i++ {
		neg := ""
		if rng.Intn(4) == 0 {
			neg = "-"
		}
		switch rng.Intn(7) {
		case 0:
			parts = append(parts, neg+"proc:"+genFieldValue(rng, []string{"api", "web", "db", "my app", "worker-1"}))
		case 1:
			lvls := []string{"trace", "debug", "info", "warn", "error", "warning", "fatal"}
			parts = append(parts, neg+"level:"+lvls[rng.Intn(len(lvls))])
		case 2:
			// Confirmed-shape non-field colon+quote bare term.
			parts = append(parts, neg+genColonQuoteTerm(rng))
		case 3:
			parts = append(parts, neg+"re:"+genREPattern(rng))
		default:
			parts = append(parts, neg+genBareWord(rng))
		}
	}
	return strings.Join(parts, " ")
}

func genRequestsFilterQuery(rng *rand.Rand) string {
	var parts []string
	n := rng.Intn(5)
	inFlightSet := false
	for i := 0; i < n; i++ {
		neg := ""
		if rng.Intn(4) == 0 {
			neg = "-"
		}
		switch rng.Intn(9) {
		case 0:
			methods := []string{"GET", "POST", "PUT", "DELETE", "patch"}
			parts = append(parts, neg+"method:"+methods[rng.Intn(len(methods))])
		case 1:
			statuses := []string{"200", "404", "500", "2xx", "4xx", "5XX", ">=400", "<=299", "200-399", "100-599"}
			parts = append(parts, neg+"status:"+statuses[rng.Intn(len(statuses))])
		case 2:
			parts = append(parts, neg+"host:"+genFieldValue(rng, []string{"api", "web.local", "my host"}))
		case 3:
			parts = append(parts, neg+"url:"+genFieldValue(rng, []string{"/orders", "/a b", "/health"}))
		case 4:
			if inFlightSet {
				continue
			}
			inFlightSet = true
			parts = append(parts, neg+"in_flight:"+[]string{"true", "false"}[rng.Intn(2)])
		case 5:
			parts = append(parts, neg+genColonQuoteTerm(rng))
		default:
			parts = append(parts, neg+genBareWord(rng))
		}
	}
	return strings.Join(parts, " ")
}

func genFieldValue(rng *rand.Rand, choices []string) string {
	v := choices[rng.Intn(len(choices))]
	if containsFilterSpace(v) {
		return `"` + v + `"`
	}
	return v
}

func genREPattern(rng *rand.Rand) string {
	// Always-valid RE2 patterns; quote when whitespace is present.
	patterns := []string{"timeout", "err.*", "(?i)error", "foo bar", "[0-9]+", "a+b"}
	return genFieldValue(rng, patterns)
}

func genBareWord(rng *rand.Rand) string {
	words := []string{"foo", "bar", "health", "15:04", "a.b:1", "err", "timeout"}
	return words[rng.Intn(len(words))]
}

func genColonQuoteTerm(rng *rand.Rand) string {
	// Pre-colon part is NOT field-shaped (dot / digit / empty) and must not
	// itself contain ':', so the tokenizer's first-colon+quote scan fires.
	prefixes := []string{"a.b", "x.y.z", "1", "Foo", ""}
	inners := []string{"x y", "a b:", "hello world", "p q"}
	return prefixes[rng.Intn(len(prefixes))] + `:"` + inners[rng.Intn(len(inners))] + `"`
}

func TestStringFilter_BareSubstringParity(t *testing.T) {
	// `s foo` bare substring behaves exactly as today's substring filter.
	m := newTestModel()
	m.logEntries = []domain.LogEntry{
		{Process: "web", Line: "web log 1"},
		{Process: "api", Line: "api log 1"},
		{Process: "web", Line: "web log 2"},
	}
	m.setLogsFilterQuery("log 1")
	got := m.filteredEntries()
	require.Len(t, got, 2)
	for _, e := range got {
		assert.Contains(t, e.Line, "log 1")
	}

	m2 := newTestModel()
	m2.proxyRequests = []proxy.RequestRecord{
		{Subdomain: "api", Method: "GET", URL: "/users"},
		{Subdomain: "web", Method: "POST", URL: "/login"},
	}
	m2.setRequestsFilterQuery("users")
	require.Len(t, m2.filteredProxyRequests(), 1)
}

func TestStringFilter_InvalidKeepsLastGood(t *testing.T) {
	m := newTestModel()
	m.ready = true
	m.width = 160
	m.height = 30
	m.viewMode = ViewModeLogs
	m.logEntries = []domain.LogEntry{
		{Process: "api", Line: "hello world"},
		{Process: "api", Line: "goodbye"},
	}
	m.setLogsFilterQuery("hello")
	require.Len(t, m.filteredEntries(), 1)

	// Mid-typing an invalid field keeps LastGood ("hello") evaluating.
	m.applyActiveFilterQuery("hello level:chatty")
	require.Error(t, m.logsFilter.ParseErr)
	assert.Equal(t, "hello", m.logsFilter.LastGood.Serialize())
	assert.Len(t, m.filteredEntries(), 1, "LastGood keeps filtering")

	bar := m.statusBar("")
	assert.Contains(t, bar, "Filter: hello level:chatty")
	assert.Contains(t, bar, "invalid filter")

	// Same hint while the s-bar is open.
	m.mode = ModeStringFilter
	m.textInput.SetValue(m.logsFilter.RawQuery)
	bar = m.statusBar("")
	assert.Contains(t, bar, "invalid filter")
}

func TestStringFilter_EnterKeepsEscInBarClears(t *testing.T) {
	m := newTestModel()
	m.ready = true
	m.width = 120
	m.height = 30
	m.viewMode = ViewModeLogs
	m.logEntries = []domain.LogEntry{{Process: "api", Line: "alpha"}, {Process: "api", Line: "beta"}}

	m = clientUpdate(m, keyRune('s'))
	m.textInput.SetValue("alpha")
	m.applyActiveFilterQuery("alpha")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ModeNormal, m.mode)
	assert.Equal(t, "alpha", m.logsFilter.RawQuery)
	assert.Len(t, m.filteredEntries(), 1)

	// Esc inside the bar clears the active view only.
	m.setRequestsFilterQuery("status:500")
	m = clientUpdate(m, keyRune('s'))
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logsFilter.RawQuery)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
	assert.Equal(t, "status:500", m.requestsFilter.RawQuery, "other view untouched")
}

func TestStringFilter_NormalEscClearsBoth(t *testing.T) {
	m := newTestModel()
	m.setLogsFilterQuery("foo")
	m.setRequestsFilterQuery("bar")
	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.Empty(t, m.logsFilter.RawQuery)
	assert.Empty(t, m.requestsFilter.RawQuery)
	assert.True(t, m.logsFilter.LastGood.IsEmpty())
	assert.True(t, m.requestsFilter.LastGood.IsEmpty())
}

func TestStringFilter_SurvivesTab(t *testing.T) {
	m := newTestModel()
	m.ready = true
	m.width = 120
	m.height = 30
	m.setLogsFilterQuery("keep")
	m.setRequestsFilterQuery("api")
	require.Equal(t, ViewModeLogs, m.viewMode)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, ViewModeRequests, m.viewMode)
	assert.Equal(t, "keep", m.logsFilter.RawQuery)
	assert.Equal(t, "api", m.requestsFilter.RawQuery)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, ViewModeLogs, m.viewMode)
	assert.Equal(t, "keep", m.logsFilter.RawQuery)
	assert.Equal(t, "api", m.requestsFilter.RawQuery)
}

func TestStringFilter_StatusShowsRawBothViews(t *testing.T) {
	m := newTestModel()
	m.ready = true
	m.width = 160
	m.height = 30

	m.setLogsFilterQuery("hello")
	bar := m.statusBar("")
	assert.Contains(t, bar, "Filter: hello")

	// With / search active, filter is still shown (Codex #3 unification).
	m.logSearchQuery = "needle"
	m.logEntries = []domain.LogEntry{{Line: "needle hello", DisplaySeq: 1}}
	m.logMeta = map[int64]logMeta{1: {}}
	m.logCursorIdx = 0
	m.logCursorSeq = 1
	bar = m.statusBar("")
	assert.Contains(t, bar, "/needle")
	assert.Contains(t, bar, "filter: hello")

	m.viewMode = ViewModeRequests
	m.setRequestsFilterQuery("status:4xx")
	m.requestSearchQuery = ""
	bar = m.statusBar("")
	assert.Contains(t, bar, "Filter: status:4xx")
}

func TestStringFilter_LiveReparseViaKeys(t *testing.T) {
	m := newTestModel()
	m.ready = true
	m.width = 120
	m.height = 30
	m.logEntries = []domain.LogEntry{
		{Process: "api", Line: "alpha line"},
		{Process: "api", Line: "beta line"},
	}

	m = clientUpdate(m, keyRune('s'))
	require.Equal(t, ModeStringFilter, m.mode)

	// Type "alpha" via live key updates through handleStringFilterKey.
	for _, r := range "alpha" {
		m = clientUpdate(m, keyRune(r))
	}
	assert.Equal(t, "alpha", m.logsFilter.RawQuery)
	assert.NoError(t, m.logsFilter.ParseErr)
	assert.Len(t, m.filteredEntries(), 1)
	assert.Equal(t, "alpha line", m.filteredEntries()[0].Line)
}

// equalLogsFilterExpr reports whether a and b are equivalent: same constraints
// in the same order per slot. Order is identity because Serialize preserves
// parse order. Unknown levels are ignored (Serialize drops them). re: patterns
// compare by regexp source string (pointers differ across compiles).
func equalLogsFilterExpr(a, b LogsFilterExpr) bool {
	return slices.Equal(a.procs, b.procs) &&
		slices.Equal(a.negProcs, b.negProcs) &&
		equalLevelsDropUnknown(a.levels, b.levels) &&
		equalLevelsDropUnknown(a.negLevels, b.negLevels) &&
		equalRegexpSlices(a.res, b.res) &&
		equalRegexpSlices(a.negRes, b.negRes) &&
		slices.Equal(a.terms, b.terms) &&
		slices.Equal(a.negTerms, b.negTerms)
}

// equalRequestsFilterExpr is the requests counterpart of equalLogsFilterExpr.
// statusMatch values (exact/class/gte/lte/range) compare by struct equality.
func equalRequestsFilterExpr(a, b RequestsFilterExpr) bool {
	return slices.Equal(a.methods, b.methods) &&
		slices.Equal(a.negMethods, b.negMethods) &&
		slices.Equal(a.statuses, b.statuses) &&
		slices.Equal(a.negStatuses, b.negStatuses) &&
		slices.Equal(a.hosts, b.hosts) &&
		slices.Equal(a.negHosts, b.negHosts) &&
		slices.Equal(a.urls, b.urls) &&
		slices.Equal(a.negURLs, b.negURLs) &&
		equalBoolPtr(a.inFlight, b.inFlight) &&
		equalBoolPtr(a.negInFlight, b.negInFlight) &&
		slices.Equal(a.terms, b.terms) &&
		slices.Equal(a.negTerms, b.negTerms)
}

func equalRegexpSlices(a, b []*regexp.Regexp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if a[i] != b[i] {
				return false
			}
			continue
		}
		if a[i].String() != b[i].String() {
			return false
		}
	}
	return true
}

func equalLevelsDropUnknown(a, b []LogLevel) bool {
	return slices.Equal(dropUnknownLevels(a), dropUnknownLevels(b))
}

func dropUnknownLevels(in []LogLevel) []LogLevel {
	if len(in) == 0 {
		return nil
	}
	var out []LogLevel
	for _, l := range in {
		if l != LogLevelUnknown {
			out = append(out, l)
		}
	}
	return out
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
