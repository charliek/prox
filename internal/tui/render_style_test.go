package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

func pinANSIProfile(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func TestFormatLogEntry_UndetectedPlain(t *testing.T) {
	pinANSIProfile(t)

	const line = "hello plain world"
	entry := domain.LogEntry{
		Timestamp:  time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC),
		Process:    "api",
		Line:       line,
		DisplaySeq: 1,
	}
	for _, tc := range []struct {
		theme string
		raw   bool // legacy: Base is a no-op — content stays raw (byte-identical to C8)
	}{
		{"tokyo-night", false},
		{"legacy", true},
	} {
		t.Run(tc.theme, func(t *testing.T) {
			withTestTheme(t, tc.theme)
			m := newTestModel()
			m.settings.Timestamps = true
			m.settings.Wrap = false
			got := m.formatLogEntry(entry)
			proc := getProcessStyle("api", m.processes).Render("api       ")
			ts := styles.Dim.Render("15:04:05")
			sep := styles.Base.Render(" ")
			content := styles.Base.Render(line)
			if tc.raw {
				content = line
			}
			want := ts + sep + proc + sep + content
			assert.Equal(t, want, got)
		})
	}
}

func TestFormatLogEntry_LevelColors(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = false
	m.logMeta = map[int64]logMeta{
		1: {level: LogLevelError, hasLevel: true},
		2: {level: LogLevelWarn, hasLevel: true},
		3: {level: LogLevelDebug, hasLevel: true},
		4: {level: LogLevelInfo, hasLevel: true},
	}

	cases := []struct {
		seq   int64
		line  string
		style lipgloss.Style
	}{
		{1, "level=error boom", styles.LogError},
		{2, "level=warn careful", styles.LogWarn},
		{3, "level=debug noise", styles.LogDebug},
		{4, "level=info ok", styles.LogInfo},
	}
	for _, tc := range cases {
		entry := domain.LogEntry{Process: "api", Line: tc.line, DisplaySeq: tc.seq}
		got := m.formatLogEntry(entry)
		assert.Contains(t, got, tc.style.Render(tc.line), "level tint on content")
	}
}

func TestFormatLogEntry_StderrBadgeIntactWithLevel(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = false
	m.logMeta = map[int64]logMeta{1: {level: LogLevelError, hasLevel: true}}
	entry := domain.LogEntry{
		Process: "api", Line: "level=error boom", Stream: domain.StreamStderr, DisplaySeq: 1,
	}
	got := m.formatLogEntry(entry)
	assert.Contains(t, got, styles.Err.Render(" ERR "))
	assert.Contains(t, got, styles.LogError.Render("level=error boom"))
}

func TestFormatLogEntry_HighlightComposesWithLevel(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = false
	m.logSearchQuery = "boom"
	m.logMeta = map[int64]logMeta{1: {level: LogLevelError, hasLevel: true}}
	entry := domain.LogEntry{Process: "api", Line: "level=error boom now", DisplaySeq: 1}
	got := m.formatLogEntry(entry)

	assert.Contains(t, got, styles.SearchHighlight.Render("boom"), "highlighted span keeps search colour")
	assert.Contains(t, got, styles.LogError.Render("level=error "), "prefix keeps level tint")
	assert.Contains(t, got, styles.LogError.Render(" now"), "suffix keeps level tint")
	assert.NotContains(t, got, styles.LogError.Render("boom"))
}

func TestSummarizeJSONLog_SortedDepthCapRemainder(t *testing.T) {
	raw := `{"z":1,"a":{"b":2,"c":{"d":3}},"m":true,"n":null,"s":"hi","arr":[9]}`
	sum, ok := summarizeJSONLog(raw, 0)
	require.True(t, ok)
	assert.Contains(t, sum, `a.b=2`)
	assert.Contains(t, sum, `a.c={…}`)
	assert.Contains(t, sum, `arr.0=9`)
	assert.Contains(t, sum, `m=true`)
	assert.Contains(t, sum, `n=null`)
	assert.Contains(t, sum, `s="hi"`)
	assert.Contains(t, sum, `z=1`)
	assert.True(t, strings.Index(sum, "a.b=") < strings.Index(sum, "z="))
}

func TestSummarizeJSONLog_EightPairCap(t *testing.T) {
	obj := map[string]any{}
	for i := 0; i < 12; i++ {
		obj[string(rune('a'+i))] = i
	}
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	sum, ok := summarizeJSONLog(string(b), 0)
	require.True(t, ok)
	assert.Contains(t, sum, "…(+4 more)")
	assert.Equal(t, 8, strings.Count(sum, "="))
}

func TestSummarizeJSONLog_WidthTruncation(t *testing.T) {
	raw := `{"message":"a fairly long message value for truncation","level":"info","extra":1}`
	sum, ok := summarizeJSONLog(raw, 40)
	require.True(t, ok)
	assert.LessOrEqual(t, ansi.StringWidth(sum), 40)
	assert.NotContains(t, sum, "{", "raw JSON dropped when it does not fit")
}

func TestSummarizeJSONLog_NonJSON(t *testing.T) {
	_, ok := summarizeJSONLog("not json", 0)
	assert.False(t, ok)
	_, ok = summarizeJSONLog("[1,2]", 0)
	assert.False(t, ok)
}

func TestFormatLogEntry_JSONSummary(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = false
	m.settings.Wrap = false
	// Narrow enough that the compact raw blob cannot fit after the summary
	// (C9: raw is appended only when the remainder budget allows).
	m.viewport.Width = 60
	raw := `{"level":"error","msg":"fail","code":500}`
	m.appendLogEntry(domain.LogEntry{Process: "api", Line: raw})
	entry := m.logEntries[0]
	got := m.formatLogEntry(entry)
	plain := stripANSI(got)
	assert.Contains(t, plain, `code=500`)
	assert.Contains(t, plain, `level="error"`)
	assert.Contains(t, plain, `msg="fail"`)
	assert.NotContains(t, plain, `{"level"`, "raw blob dropped when it does not fit")
	assert.Contains(t, got, styles.LogError.Render(`code=500  level="error"  msg="fail"`))
}

func TestFormatLogEntry_JSONSummary_WrapOnOffConsistent(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	raw := `{"a":1,"b":"hi"}`
	// Wide viewport so wrap-off includes compact raw.
	mk := func(wrap bool) string {
		m := newTestModel()
		m.settings.Timestamps = false
		m.settings.Wrap = wrap
		m.viewport.Width = 120
		m.appendLogEntry(domain.LogEntry{Process: "api", Line: raw})
		return stripANSI(m.formatLogContent(m.logEntries[0]))
	}
	off := mk(false)
	on := mk(true)
	assert.Equal(t, off, on, "wrap-on keeps summary+raw consistent with wrap-off")
	assert.Contains(t, off, `a=1`)
	assert.Contains(t, off, `{"a":1,"b":"hi"}`)
}

func TestSummarizeJSONLog_AppendsRawWhenFits(t *testing.T) {
	raw := `{"a":1}`
	sum, ok := summarizeJSONLog(raw, 80)
	require.True(t, ok)
	assert.Contains(t, sum, `a=1`)
	assert.Contains(t, sum, `{"a":1}`, "compact raw included when width allows")
}

func TestSummarizeJSONLog_WidthZeroKeepsRaw(t *testing.T) {
	raw := `{"a":1}`
	sum, ok := summarizeJSONLog(raw, 0)
	require.True(t, ok)
	assert.Contains(t, sum, `a=1`)
	assert.Contains(t, sum, `{"a":1}`, "width<=0 keeps compact raw (wrap-on path)")
}

func TestFormatLogEntry_NonJSONUntouched(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = false
	line := "plain text not json"
	m.logMeta = map[int64]logMeta{1: {isJSON: false}}
	entry := domain.LogEntry{Process: "api", Line: line, DisplaySeq: 1}
	got := m.formatLogEntry(entry)
	// Content is Base-wrapped under FullFill (plan 024 F1); not JSON-summarized.
	assert.True(t, strings.HasSuffix(got, styles.Base.Render(line)))
	assert.NotContains(t, ansi.Strip(got), `=`, "non-JSON must not become path=value summary")
}

func TestFormatProxyRequest_MethodAndStatusColors(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	m := newTestModel()

	cases := []struct {
		method string
		status int
		msty   lipgloss.Style
	}{
		{"GET", 200, styles.HTTPGet},
		{"POST", 404, styles.HTTPPost},
		{"PUT", 500, styles.HTTPPut},
		{"DELETE", 301, styles.HTTPDelete},
		{"PATCH", 0, styles.HTTPPatch},
	}
	for _, c := range cases {
		req := proxy.RequestRecord{
			Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			Subdomain: "api", Method: c.method, URL: "/x",
			StatusCode: c.status, Duration: 50 * time.Millisecond,
		}
		got := m.formatProxyRequest(req)
		assert.Contains(t, got, c.msty.Render(fmt.Sprintf("%-7s", c.method)), c.method)
		tok := fmt.Sprintf("%3d", c.status)
		switch {
		case c.status < 200:
			assert.Contains(t, got, styles.Dim.Render(tok))
		case c.status >= 500:
			assert.Contains(t, got, styles.Status5xx.Render(tok))
		case c.status >= 400:
			assert.Contains(t, got, styles.Status4xx.Render(tok))
		case c.status >= 300:
			assert.Contains(t, got, tok)
			assert.NotContains(t, got, styles.Status2xx.Render(tok))
		default:
			assert.Contains(t, got, styles.Status2xx.Render(tok))
		}
	}
}

func TestFormatProxyRequest_DurationScale(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	m := newTestModel()
	mk := func(ms int) string {
		return m.formatProxyRequest(proxy.RequestRecord{
			Timestamp: time.Now(), Subdomain: "api", Method: "GET", URL: "/",
			StatusCode: 200, Duration: time.Duration(ms) * time.Millisecond,
		})
	}
	assert.Contains(t, mk(50), styles.HTTPSuccess.Render("   50ms"))
	assert.Contains(t, mk(300), styles.Base.Render("  300ms"))
	assert.NotContains(t, mk(300), styles.Warn.Render("  300ms"))
	assert.Contains(t, mk(800), styles.Warn.Render("  800ms"))
	assert.Contains(t, mk(3000), styles.HTTPError.Render(" 3000ms"))
}

func TestFormatProxyRequest_InFlightDim(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	m := newTestModel()
	req := proxy.RequestRecord{
		Timestamp: time.Now(), Subdomain: "api", Method: "GET", URL: "/s",
		StatusCode: 200, InFlight: true,
	}
	got := m.formatProxyRequest(req)
	assert.Contains(t, got, styles.Dim.Render("  ...ms"))
	assert.Contains(t, got, styles.Dim.Render("200"))
}

func TestFormatRequestDetail_BoldMethodURL(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID: "req-1", Timestamp: "2026-07-18 00:00:00.000",
		Method: "POST", URL: "/api/v1/things", StatusCode: 200, DurationMs: 12,
	}
	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, styles.Bold.Render("POST")+styles.Base.Render(" ")+styles.Base.Render("/api/v1/things"))
	assert.NotContains(t, out, "Method:   POST")
}

func TestFormatRequestDetail_JSONBodySyntaxColor(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID: "req-1", Timestamp: "t", Method: "POST", URL: "/x", StatusCode: 200,
		RequestBody: &BodyData{
			Size: 40, ContentType: "application/json",
			Data: `{"s":"hi","n":1,"b":true,"z":null}`,
		},
	}
	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, styles.JSONKey.Render(`"s"`))
	assert.Contains(t, out, styles.JSONString.Render(`"hi"`))
	assert.Contains(t, out, styles.JSONNumber.Render("1"))
	assert.Contains(t, out, styles.JSONBool.Render("true"))
	assert.Contains(t, out, styles.JSONNull.Render("null"))
}

func TestFormatRequestDetail_NonJSONBodyUnchanged(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	plain := "hello: not-json"
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID: "req-1", Timestamp: "t", Method: "POST", URL: "/x", StatusCode: 200,
		RequestBody: &BodyData{
			Size: int64(len(plain)), ContentType: "text/plain", Data: plain,
		},
	}
	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, styles.Base.Render("  ")+plain)
	assert.NotContains(t, out, styles.JSONKey.Render(`"hello"`))
}

func TestHighlightJSONText_RoundTripPlain(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	in := "{\n  \"a\": 1\n}"
	out := highlightJSONText(in)
	assert.Equal(t, `{"a":1}`, strings.Join(strings.Fields(stripANSI(out)), ""))
}

// summarizeJSONLog is the test-only convenience wrapper for the ingest+render
// pair: one ingest parse, then format from the cached pairs (production render
// does exactly this via logMeta.pairs).
func summarizeJSONLog(raw string, width int) (string, bool) {
	meta := ingestLogMeta(raw)
	if !meta.isJSON {
		return "", false
	}
	return formatJSONSummary(meta.pairs, strings.TrimSpace(raw), width), true
}
