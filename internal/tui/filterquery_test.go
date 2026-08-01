package tui

import (
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
}

func TestFilterSerializeRoundTrip(t *testing.T) {
	logsQueries := []string{
		"",
		"proc:api",
		"proc:api proc:web level:error -health",
		"level:warn -proc:db foo",
		`proc:"my app" level:info`,
	}
	for _, q := range logsQueries {
		t.Run("logs/"+q, func(t *testing.T) {
			e1, err := ParseLogsFilter(q)
			require.NoError(t, err)
			s1 := e1.Serialize()
			e2, err := ParseLogsFilter(s1)
			require.NoError(t, err)
			s2 := e2.Serialize()
			assert.Equal(t, s1, s2, "serialize(parse(x)) must be stable")
			// Empty stays empty.
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
	}
	for _, q := range reqQueries {
		t.Run("requests/"+q, func(t *testing.T) {
			e1, err := ParseRequestsFilter(q)
			require.NoError(t, err)
			s1 := e1.Serialize()
			e2, err := ParseRequestsFilter(s1)
			require.NoError(t, err)
			s2 := e2.Serialize()
			assert.Equal(t, s1, s2)
		})
	}

	// Menu-edit-friendly ordering: fields before bare, positives before negs in group.
	e, err := ParseLogsFilter("-health level:error proc:web proc:api")
	require.NoError(t, err)
	assert.Equal(t, "proc:web proc:api level:error -health", e.Serialize())
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
