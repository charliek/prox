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

func TestFormatLogEntry_UndetectedByteIdentical(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")

	m := newTestModel()
	m.settings.Timestamps = true
	m.settings.Wrap = false
	entry := domain.LogEntry{
		Timestamp:  time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC),
		Process:    "api",
		Line:       "hello plain world",
		DisplaySeq: 1,
	}
	got := m.formatLogEntry(entry)
	proc := getProcessStyle("api", m.processes).Render("api       ")
	ts := s.Dim.Render("15:04:05")
	want := ts + " " + proc + " " + "hello plain world"
	assert.Equal(t, want, got)
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
		{1, "level=error boom", s.LogError},
		{2, "level=warn careful", s.LogWarn},
		{3, "level=debug noise", s.LogDebug},
		{4, "level=info ok", s.LogInfo},
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
	assert.Contains(t, got, s.Err.Render(" ERR "))
	assert.Contains(t, got, s.LogError.Render("level=error boom"))
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

	assert.Contains(t, got, s.SearchHighlight.Render("boom"), "highlighted span keeps search colour")
	assert.Contains(t, got, s.LogError.Render("level=error "), "prefix keeps level tint")
	assert.Contains(t, got, s.LogError.Render(" now"), "suffix keeps level tint")
	assert.NotContains(t, got, s.LogError.Render("boom"))
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
	m.logMeta = map[int64]logMeta{1: {level: LogLevelError, hasLevel: true, isJSON: true}}
	entry := domain.LogEntry{Process: "api", Line: raw, DisplaySeq: 1}
	got := m.formatLogEntry(entry)
	plain := stripANSI(got)
	assert.Contains(t, plain, `code=500`)
	assert.Contains(t, plain, `level="error"`)
	assert.Contains(t, plain, `msg="fail"`)
	assert.NotContains(t, plain, `{"level"`, "raw blob dropped when it does not fit")
	assert.Contains(t, got, s.LogError.Render(`code=500  level="error"  msg="fail"`))
}

func TestSummarizeJSONLog_AppendsRawWhenFits(t *testing.T) {
	raw := `{"a":1}`
	sum, ok := summarizeJSONLog(raw, 80)
	require.True(t, ok)
	assert.Contains(t, sum, `a=1`)
	assert.Contains(t, sum, `{"a":1}`, "compact raw included when width allows")
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
	assert.True(t, strings.HasSuffix(got, line))
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
		{"GET", 200, s.HTTPGet},
		{"POST", 404, s.HTTPPost},
		{"PUT", 500, s.HTTPPut},
		{"DELETE", 301, s.HTTPDelete},
		{"PATCH", 0, s.HTTPPatch},
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
			assert.Contains(t, got, s.Dim.Render(tok))
		case c.status >= 500:
			assert.Contains(t, got, s.Status5xx.Render(tok))
		case c.status >= 400:
			assert.Contains(t, got, s.Status4xx.Render(tok))
		case c.status >= 300:
			assert.Contains(t, got, tok)
			assert.NotContains(t, got, s.Status2xx.Render(tok))
		default:
			assert.Contains(t, got, s.Status2xx.Render(tok))
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
	assert.Contains(t, mk(50), s.HTTPSuccess.Render("   50ms"))
	assert.Contains(t, mk(300), "  300ms")
	assert.NotContains(t, mk(300), s.Warn.Render("  300ms"))
	assert.Contains(t, mk(800), s.Warn.Render("  800ms"))
	assert.Contains(t, mk(3000), s.HTTPError.Render(" 3000ms"))
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
	assert.Contains(t, got, s.Dim.Render("  ...ms"))
	assert.Contains(t, got, s.Dim.Render("200"))
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
	assert.Contains(t, out, s.Bold.Render("POST")+" /api/v1/things")
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
	assert.Contains(t, out, s.JSONKey.Render(`"s"`))
	assert.Contains(t, out, s.JSONString.Render(`"hi"`))
	assert.Contains(t, out, s.JSONNumber.Render("1"))
	assert.Contains(t, out, s.JSONBool.Render("true"))
	assert.Contains(t, out, s.JSONNull.Render("null"))
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
	assert.Contains(t, out, "  "+plain)
	assert.NotContains(t, out, s.JSONKey.Render(`"hello"`))
}

func TestHighlightJSONText_RoundTripPlain(t *testing.T) {
	pinANSIProfile(t)
	withTestTheme(t, "tokyo-night")
	in := "{\n  \"a\": 1\n}"
	out := highlightJSONText(in)
	assert.Equal(t, `{"a":1}`, strings.Join(strings.Fields(stripANSI(out)), ""))
}
