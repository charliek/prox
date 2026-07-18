package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// joinBody renders a body section and joins the lines into one string so
// tests can use assert.Contains and sidestep the ANSI styling that wraps
// header lines.
func joinBody(t *testing.T, title string, b *BodyData) string {
	t.Helper()
	return strings.Join(renderBodySection(title, b), "\n")
}

// TestRenderBodySection_JSONContentType pins that a body whose Content-Type
// declares JSON is pretty-printed 2-space indented via json.Indent.
func TestRenderBodySection_JSONContentType(t *testing.T) {
	b := &BodyData{
		Size:        18,
		ContentType: "application/json",
		Data:        `{"key":"value"}`,
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, `"key": "value"`)
	assert.Contains(t, out, "\n  {\n")
}

// TestRenderBodySection_ValidJSONWithoutContentType pins the json.Valid
// fallback path: no Content-Type declares JSON, but the raw text parses as
// JSON on its own, so it is still pretty-printed.
func TestRenderBodySection_ValidJSONWithoutContentType(t *testing.T) {
	b := &BodyData{
		Size: 18,
		Data: `{"key":"value"}`,
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, `"key": "value"`)
}

// TestRenderBodySection_InvalidJSONWithJSONContentType pins that a
// json.Indent failure falls back to the raw text unchanged, rather than
// dropping or mangling the body.
func TestRenderBodySection_InvalidJSONWithJSONContentType(t *testing.T) {
	b := &BodyData{
		Size:        13,
		ContentType: "application/json",
		Data:        `{not valid json`,
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, "  {not valid json")
	assert.NotContains(t, out, "  {\n")
}

// TestRenderBodySection_BinaryWithJSONContentType pins that a binary body
// short-circuits to the "[binary data]" marker without any pretty-print
// attempt, even when Content-Type claims JSON.
func TestRenderBodySection_BinaryWithJSONContentType(t *testing.T) {
	b := &BodyData{
		Size:        4,
		ContentType: "application/json",
		IsBinary:    true,
		Data:        `{"key":"value"}`, // must be ignored entirely
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, "[binary data]")
	assert.NotContains(t, out, `"key": "value"`)
}

// TestRenderBodySection_NonJSONTextUnchanged pins that ordinary text renders
// byte-for-byte, line for line, with no pretty-print interference.
func TestRenderBodySection_NonJSONTextUnchanged(t *testing.T) {
	b := &BodyData{
		Size:        16,
		ContentType: "text/plain",
		Data:        "plain text\nline2",
	}
	lines := renderBodySection("Request Body", b)
	assert.Contains(t, lines, "  plain text")
	assert.Contains(t, lines, "  line2")
}

// TestRenderBodySection_EmptyContentTypeTitle pins that an absent
// Content-Type/Content-Encoding leaves no dangling comma or empty-segment
// artifact in the section title.
func TestRenderBodySection_EmptyContentTypeTitle(t *testing.T) {
	b := &BodyData{
		Size: 10,
		Data: "plain text",
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, "Request Body (10 bytes)")
	assert.NotContains(t, out, ",  bytes")
	assert.NotContains(t, out, ", )")
	assert.NotContains(t, out, "(,")
}

// TestRenderBodySection_ContentTypeAndEncodingInTitle pins that both fields
// appear in the section title when present, alongside the truncated flag.
func TestRenderBodySection_ContentTypeAndEncodingInTitle(t *testing.T) {
	b := &BodyData{
		Size:            2048,
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		Truncated:       true,
		Data:            `{"a":1}`,
	}
	out := joinBody(t, "Request Body", b)
	assert.Contains(t, out, "Request Body (2048 bytes, application/json, gzip, truncated)")
}

// TestRenderBodySection_Unavailable pins the existing (body no longer
// available) message for evicted/unreadable bodies, unchanged by this
// refactor.
func TestRenderBodySection_Unavailable(t *testing.T) {
	b := &BodyData{
		Size:              10,
		Unavailable:       true,
		UnavailableReason: "evicted",
	}
	out := joinBody(t, "Response Body", b)
	assert.Contains(t, out, "(body no longer available)")
}

// TestFormatRequestDetail_BodySections is an integration-level check that
// formatRequestDetail (shared by Model and ClientModel via BaseModel) wires
// renderBodySection in for both request and response bodies.
func TestFormatRequestDetail_BodySections(t *testing.T) {
	b := &BaseModel{
		requestDetail: &RequestDetailData{
			ID:         "req-1",
			Timestamp:  "2026-07-18 00:00:00.000",
			Method:     "POST",
			URL:        "/api/v1/things",
			StatusCode: 200,
			RequestBody: &BodyData{
				Size:        18,
				ContentType: "application/json",
				Data:        `{"key":"value"}`,
			},
			ResponseBody: &BodyData{
				Size:              10,
				Unavailable:       true,
				UnavailableReason: "evicted",
			},
		},
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, "Request Body (18 bytes, application/json)")
	assert.Contains(t, out, `"key": "value"`)
	assert.Contains(t, out, "Response Body (10 bytes)")
	assert.Contains(t, out, "(body no longer available)")
}
