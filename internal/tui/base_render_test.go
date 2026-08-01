package tui

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"

	"github.com/charliek/prox/internal/domain"
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
// short-circuits to the hexdump preview without any pretty-print attempt,
// even when Content-Type claims JSON: the raw bytes are dumped, not the
// pretty-printed JSON they happen to spell out.
func TestRenderBodySection_BinaryWithJSONContentType(t *testing.T) {
	b := &BodyData{
		Size:        15,
		ContentType: "application/json",
		IsBinary:    true,
		Data:        `{"key":"value"}`, // dumped as raw bytes, not pretty-printed
	}
	out := joinBody(t, "Request Body", b)
	assert.NotContains(t, out, `"key": "value"`)
	assert.NotContains(t, out, "[binary data]")
	// First byte '{' = 0x7b, present in both the hex and ASCII gutter.
	assert.Contains(t, out, "7b")
	assert.Contains(t, out, "|{\"key\":\"value\"}|")
}

// TestRenderBodyLines_BinaryEmptyDataFallsBackToPlaceholder pins that a body
// flagged binary but carrying no bytes at all (nothing was actually loaded)
// still falls back to the original "[binary data]" placeholder rather than
// an empty hexdump.
func TestRenderBodyLines_BinaryEmptyDataFallsBackToPlaceholder(t *testing.T) {
	b := &BodyData{
		Size:     4,
		IsBinary: true,
		Data:     "",
	}
	out := strings.Join(renderBodyLines(b), "\n")
	assert.Contains(t, out, "[binary data]")
}

// TestRenderBodyLines_BinaryBase64Data pins the attach-mode path: the API
// base64-encodes binary bytes on the wire (client.go clientBodyToBodyData
// sets DataBase64), and the flag — not a decode-guess — drives the base64
// decode, so the hexdump previews the underlying bytes rather than the
// base64 text.
func TestRenderBodyLines_BinaryBase64Data(t *testing.T) {
	raw := []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}
	b := &BodyData{
		Size:       int64(len(raw)),
		IsBinary:   true,
		Data:       base64.StdEncoding.EncodeToString(raw),
		DataBase64: true,
	}
	out := strings.Join(renderBodyLines(b), "\n")
	assert.Contains(t, out, "47 49 46 38 39 61")
	assert.Contains(t, out, "|GIF89a|")
}

// TestRenderBodyLines_RawBytesThatLookLikeBase64 pins the deterministic-flag
// rule (codex C10 review): LOCAL-mode raw bytes that happen to be valid
// base64 (here the literal bytes "TWFu") must be previewed AS-IS, never
// base64-decoded into different bytes ("Man").
func TestRenderBodyLines_RawBytesThatLookLikeBase64(t *testing.T) {
	b := &BodyData{
		Size:     4,
		IsBinary: true,
		Data:     "TWFu",
	}
	out := strings.Join(renderBodyLines(b), "\n")
	assert.Contains(t, out, "|TWFu|")
	assert.NotContains(t, out, "|Man|")
}

// TestRenderBodyLines_BinaryRawData pins the DataBase64=false path: binary
// bytes stored directly, not base64-encoded, so a failed base64 decode must
// fall back to previewing the raw string bytes rather than dropping the body
// or erroring.
func TestRenderBodyLines_BinaryRawData(t *testing.T) {
	raw := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	b := &BodyData{
		Size:     int64(len(raw)),
		IsBinary: true,
		Data:     string(raw), // not valid base64
	}
	out := strings.Join(renderBodyLines(b), "\n")
	assert.Contains(t, out, "00 01 02 ff fe")
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

// TestRenderBodyLines_ControlCharsSanitized pins that terminal control
// sequences embedded in a (mislabeled non-binary) captured body are neutralized
// before rendering, so a socket-supplied record cannot emit raw ESC/BEL/OSC
// bytes that manipulate the terminal.
func TestRenderBodyLines_ControlCharsSanitized(t *testing.T) {
	b := &BodyData{
		ContentType: "text/plain",
		Data:        "before\x1b]0;evil\x07after",
	}
	joined := strings.Join(renderBodyLines(b), "\n")
	assert.NotContains(t, joined, "\x1b", "raw ESC must not reach the terminal")
	assert.NotContains(t, joined, "\x07", "raw BEL must not reach the terminal")
	assert.Contains(t, joined, "before")
	assert.Contains(t, joined, "after")
	assert.Contains(t, joined, "�", "control chars replaced with U+FFFD")
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

// TestHexPreviewLines_ShortBody is a golden test for a partial line (< 16
// bytes): the hex groups pad with blanks past the present bytes, but the
// ASCII gutter only shows the bytes actually present. Bytes cover a null
// byte, printable ASCII, 0xFF, an ESC control byte, and space/tilde (the
// printable-range boundaries) to pin the '.' vs. literal-char rule.
func TestHexPreviewLines_ShortBody(t *testing.T) {
	data := []byte{0x00, 0x41, 0x42, 0x43, 0xff, 0x1b, 0x20, 0x7e}
	got := hexPreviewLines(data, hexPreviewMaxBytes)
	want := []string{
		"00000000  00 41 42 43 ff 1b 20 7e                           |.ABC.. ~|",
	}
	assert.Equal(t, want, got)
}

// TestHexPreviewLines_Exact256Bytes is a golden test covering every byte
// value 0x00-0xFF (16 full lines, 16 bytes each): no truncation, so no
// "more bytes" trailer. Pins the offset column, the two 8-byte hex groups
// with their extra separating space, and the full printable-ASCII-vs-'.'
// gutter rule across the whole byte range (including the 0x7F DEL boundary
// and the printable 0x20-0x7E range).
func TestHexPreviewLines_Exact256Bytes(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	got := hexPreviewLines(data, hexPreviewMaxBytes)
	want := []string{
		"00000000  00 01 02 03 04 05 06 07  08 09 0a 0b 0c 0d 0e 0f  |................|",
		"00000010  10 11 12 13 14 15 16 17  18 19 1a 1b 1c 1d 1e 1f  |................|",
		"00000020  20 21 22 23 24 25 26 27  28 29 2a 2b 2c 2d 2e 2f  | !\"#$%&'()*+,-./|",
		"00000030  30 31 32 33 34 35 36 37  38 39 3a 3b 3c 3d 3e 3f  |0123456789:;<=>?|",
		"00000040  40 41 42 43 44 45 46 47  48 49 4a 4b 4c 4d 4e 4f  |@ABCDEFGHIJKLMNO|",
		"00000050  50 51 52 53 54 55 56 57  58 59 5a 5b 5c 5d 5e 5f  |PQRSTUVWXYZ[\\]^_|",
		"00000060  60 61 62 63 64 65 66 67  68 69 6a 6b 6c 6d 6e 6f  |`abcdefghijklmno|",
		"00000070  70 71 72 73 74 75 76 77  78 79 7a 7b 7c 7d 7e 7f  |pqrstuvwxyz{|}~.|",
		"00000080  80 81 82 83 84 85 86 87  88 89 8a 8b 8c 8d 8e 8f  |................|",
		"00000090  90 91 92 93 94 95 96 97  98 99 9a 9b 9c 9d 9e 9f  |................|",
		"000000a0  a0 a1 a2 a3 a4 a5 a6 a7  a8 a9 aa ab ac ad ae af  |................|",
		"000000b0  b0 b1 b2 b3 b4 b5 b6 b7  b8 b9 ba bb bc bd be bf  |................|",
		"000000c0  c0 c1 c2 c3 c4 c5 c6 c7  c8 c9 ca cb cc cd ce cf  |................|",
		"000000d0  d0 d1 d2 d3 d4 d5 d6 d7  d8 d9 da db dc dd de df  |................|",
		"000000e0  e0 e1 e2 e3 e4 e5 e6 e7  e8 e9 ea eb ec ed ee ef  |................|",
		"000000f0  f0 f1 f2 f3 f4 f5 f6 f7  f8 f9 fa fb fc fd fe ff  |................|",
	}
	assert.Equal(t, want, got)
	// Exactly 256 bytes must not trigger the "more bytes" trailer.
	for _, line := range got {
		assert.NotContains(t, line, "more bytes")
	}
}

// TestHexPreviewLines_TruncatedOver256 is a golden test for a body longer
// than maxBytes: only the first maxBytes are dumped (16 lines here) and a
// final "(… N more bytes)" line reports the exact remainder.
func TestHexPreviewLines_TruncatedOver256(t *testing.T) {
	data := make([]byte, 260)
	for i := range data {
		data[i] = byte(i)
	}
	got := hexPreviewLines(data, hexPreviewMaxBytes)
	assert.Len(t, got, 17) // 16 hex lines + 1 trailer
	assert.Equal(t, "000000f0  f0 f1 f2 f3 f4 f5 f6 f7  f8 f9 fa fb fc fd fe ff  |................|", got[15])
	assert.Equal(t, "(… 4 more bytes)", got[16])
}

// TestHexPreviewLines_NonASCIIUTF8BytesAsDots is a golden test pinning that
// the ASCII gutter operates on raw bytes, not UTF-8 runes: a multi-byte UTF-8
// sequence (the 'é' in "café", encoded as 0xc3 0xa9) renders as two dots, one
// per byte, never as a decoded/re-encoded character and never as a raw
// non-ASCII byte.
func TestHexPreviewLines_NonASCIIUTF8BytesAsDots(t *testing.T) {
	data := []byte("café") // c3 a9 is the UTF-8 encoding of 'é'
	got := hexPreviewLines(data, hexPreviewMaxBytes)
	want := []string{
		"00000000  63 61 66 c3 a9                                    |caf..|",
	}
	assert.Equal(t, want, got)
}

// TestHexPreviewLines_Empty pins that an empty slice yields no lines (no
// zero-byte offset line, no trailer).
func TestHexPreviewLines_Empty(t *testing.T) {
	got := hexPreviewLines(nil, hexPreviewMaxBytes)
	assert.Empty(t, got)
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
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
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
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, "Request Body (18 bytes, application/json)")
	assert.Contains(t, stripANSI(out), `"key": "value"`)
	assert.Contains(t, out, "Response Body (10 bytes)")
	assert.Contains(t, out, "(body no longer available)")
	assert.Contains(t, out, s.Bold.Render("POST")+s.Base.Render(" ")+s.Base.Render("/api/v1/things"))
}

// TestFormatRequestDetail_InFlight_ShowsDurationNote verifies the Duration
// line reads "(in flight)" instead of a bogus "0ms" for a still-streaming
// request (D10).
func TestFormatRequestDetail_InFlight_ShowsDurationNote(t *testing.T) {
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID:         "req-1",
		Timestamp:  "2026-07-18 00:00:00.000",
		Method:     "GET",
		URL:        "/api/v1/stream",
		StatusCode: 200,
		InFlight:   true,
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, "Duration: (in flight)")
	assert.NotContains(t, out, "Duration: 0ms")
}

// TestFormatRequestDetail_InFlight_NoDetailsNote verifies that a nil-Details
// in-flight record gets an explanatory note rather than silently rendering
// no headers/bodies section (which would otherwise be indistinguishable from
// "capture not enabled").
func TestFormatRequestDetail_InFlight_NoDetailsNote(t *testing.T) {
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID:         "req-1",
		Timestamp:  "2026-07-18 00:00:00.000",
		Method:     "GET",
		URL:        "/api/v1/stream",
		StatusCode: 200,
		InFlight:   true,
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, "(request in flight — details arrive on completion)")
}

// TestFormatRequestDetail_Stale_ShowsStaleDurationAndNote verifies a stale
// in-flight record (D8, #53) renders "stale?" on both the Duration line and
// the in-flight note, distinct from an ordinary (fresh) in-flight record.
func TestFormatRequestDetail_Stale_ShowsStaleDurationAndNote(t *testing.T) {
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID:         "req-1",
		Timestamp:  "2026-07-18 00:00:00.000",
		Method:     "GET",
		URL:        "/api/v1/stream",
		StatusCode: 200,
		InFlight:   true,
		Stale:      true,
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.Contains(t, out, "Duration: (in flight, stale?)")
	assert.Contains(t, out, "stale? — the completion event may have been lost")
	assert.NotContains(t, out, "Duration: (in flight)")
}

// TestFormatRequestDetail_Completed_NoInFlightNote verifies a completed
// record with no captured Details (capture disabled) does NOT get the
// in-flight note.
func TestFormatRequestDetail_Completed_NoInFlightNote(t *testing.T) {
	b := newTestBaseModel()
	b.requestDetail = &RequestDetailData{
		ID:         "req-1",
		Timestamp:  "2026-07-18 00:00:00.000",
		Method:     "GET",
		URL:        "/api/v1/stream",
		StatusCode: 200,
		DurationMs: 42,
	}

	out := strings.Join(b.formatRequestDetail(), "\n")
	assert.NotContains(t, out, "in flight")
	assert.Contains(t, out, "Duration: 42ms")
}

// TestProcessPanel_HealthDot pins the plan 018 D13 process-panel health
// indicator: // rendered name as a separately styled segment, unhealthy gets a red one,
// and unknown/unset render no dot at all — so a process with no healthcheck
// configured is unaffected.
func TestProcessPanel_HealthDot(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(prev)

	t.Run("healthy renders a green dot as a separate segment", func(t *testing.T) {
		b := newTestBaseModel()
		b.viewMode = ViewModeRequests
		b.processes = []domain.ProcessInfo{
			{Name: "web", State: domain.ProcessStateRunning, Health: domain.HealthStatusHealthy},
		}
		out := b.processPanel()
		wantName := processStyle(domain.ProcessStateRunning).Render("web")
		wantDot := s.HealthyDot.Render(" ●")
		assert.Contains(t, out, wantName+wantDot, "the dot follows the name as its own styled segment")
		assert.NotContains(t, out, wantName+" ●", "the dot must be styled, not plain text")
	})

	t.Run("unhealthy renders a red dot as a separate segment", func(t *testing.T) {
		b := newTestBaseModel()
		b.viewMode = ViewModeRequests
		b.processes = []domain.ProcessInfo{
			{Name: "api", State: domain.ProcessStateRunning, Health: domain.HealthStatusUnhealthy},
		}
		out := b.processPanel()
		wantName := processStyle(domain.ProcessStateRunning).Render("api")
		wantDot := s.UnhealthyDot.Render(" ✗")
		assert.Contains(t, out, wantName+wantDot)
	})

	t.Run("unknown renders no dot", func(t *testing.T) {
		b := newTestBaseModel()
		b.viewMode = ViewModeRequests
		b.processes = []domain.ProcessInfo{
			{Name: "worker", State: domain.ProcessStateRunning, Health: domain.HealthStatusUnknown},
		}
		out := b.processPanel()
		assert.NotContains(t, out, "●")
		assert.NotContains(t, out, "✗")
	})

	t.Run("empty/unset health renders no dot", func(t *testing.T) {
		b := newTestBaseModel()
		b.viewMode = ViewModeRequests
		b.processes = []domain.ProcessInfo{
			{Name: "cron", State: domain.ProcessStateRunning},
		}
		out := b.processPanel()
		assert.NotContains(t, out, "●")
		assert.NotContains(t, out, "✗")
	})

	t.Run("no-healthcheck process renders byte-identical to the pre-dot output", func(t *testing.T) {
		b := newTestBaseModel()
		b.viewMode = ViewModeLogs
		b.processes = []domain.ProcessInfo{
			{Name: "web", State: domain.ProcessStateRunning},
			{Name: "worker", State: domain.ProcessStateWaiting, WaitingOn: []string{"postgres"}},
		}
		// joinSep mirrors processPanel's HeaderSep-styled separators under
		// FullFill (empty under legacy) — no dot anywhere.
		want := s.Header.Render(lipgloss.JoinHorizontal(lipgloss.Top, strings.Join([]string{
			processStyle(domain.ProcessStateRunning).Render("1:web"),
			processStyle(domain.ProcessStateWaiting).Render("2:worker (waiting on: postgres)"),
		}, s.HeaderSep.Render("  "))))
		assert.Equal(t, want, b.processPanel())
	})
}
