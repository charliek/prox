package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// jsonSummaryMaxPairs caps path=value pairs on a summarized JSON log line
// (plan 021 C9 / WS9 brief). Remainder is noted as ` …(+N more)`.
const jsonSummaryMaxPairs = 8

// jsonSummaryMaxDepth is the maximum path segment count (a.b = depth 2).
const jsonSummaryMaxDepth = 2

// summarizeJSONLog builds a one-line `path=value` summary of a JSON object log
// line (C9). Paths are depth-capped, keys sorted, at most jsonSummaryMaxPairs
// pairs, joined with two spaces. When width > 0 the result is ANSI-truncated
// to that many columns; compact raw JSON is appended only when it fits the
// remaining budget. Returns ("", false) when raw is not a JSON object.
func summarizeJSONLog(raw string, width int) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return "", false
	}

	pairs := flattenJSONPairs(obj, "", 0)
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].path < pairs[j].path })

	var b strings.Builder
	n := len(pairs)
	show := n
	if show > jsonSummaryMaxPairs {
		show = jsonSummaryMaxPairs
	}
	for i := 0; i < show; i++ {
		if i > 0 {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%s=%s", pairs[i].path, pairs[i].value)
	}
	if n > jsonSummaryMaxPairs {
		fmt.Fprintf(&b, "  …(+%d more)", n-jsonSummaryMaxPairs)
	}
	summary := b.String()

	if width <= 0 {
		return summary, true
	}

	// Prefer summary alone when the raw blob won't fit after it.
	sumW := ansi.StringWidth(summary)
	if sumW >= width {
		return ansi.Cut(summary, 0, width), true
	}
	// Compact raw (single-line trimmed) only when the remainder has room.
	remain := width - sumW - 1 // space separator
	compact := strings.Join(strings.Fields(trimmed), " ")
	if remain > 0 && ansi.StringWidth(compact) <= remain {
		return summary + " " + compact, true
	}
	return summary, true
}

type jsonPair struct {
	path, value string
}

// flattenJSONPairs walks a JSON value emitting path=value pairs. Objects and
// arrays deeper than jsonSummaryMaxDepth collapse to {…} / […] placeholders.
func flattenJSONPairs(v any, path string, depth int) []jsonPair {
	switch val := v.(type) {
	case map[string]any:
		if path != "" && depth >= jsonSummaryMaxDepth {
			return []jsonPair{{path, "{…}"}}
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []jsonPair
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out = append(out, flattenJSONPairs(val[k], p, depth+1)...)
		}
		return out
	case []any:
		if path != "" && depth >= jsonSummaryMaxDepth {
			return []jsonPair{{path, "[…]"}}
		}
		var out []jsonPair
		for i, el := range val {
			p := strconv.Itoa(i)
			if path != "" {
				p = path + "." + p
			}
			out = append(out, flattenJSONPairs(el, p, depth+1)...)
		}
		return out
	default:
		if path == "" {
			return nil
		}
		return []jsonPair{{path, formatJSONScalar(val)}}
	}
}

// formatJSONScalar renders a JSON leaf for the summary: strings via
// json.Marshal (quoted), numbers/bools/null in their JSON forms.
func formatJSONScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case float64:
		// encoding/json uses float64 for all numbers.
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case string:
		b, err := json.Marshal(x)
		if err != nil {
			return strconv.Quote(x)
		}
		return string(b)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

// highlightJSONText applies theme JSON syntax colours to pretty-printed JSON
// text (plan 021 C9). Keys = quoted string followed by colon; scalars by
// shape. Operates on the indented output from json.Indent — no new deps.
// Non-JSON-looking text is returned unchanged (caller gates on shouldPrettyPrintJSON).
func highlightJSONText(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 64)
	i := 0
	for i < len(text) {
		// Skip whitespace (incl. newlines) unchanged.
		if c := text[i]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			b.WriteByte(c)
			i++
			continue
		}
		// Structural punctuation.
		if c := text[i]; c == '{' || c == '}' || c == '[' || c == ']' || c == ',' || c == ':' {
			b.WriteByte(c)
			i++
			continue
		}
		// Quoted string: key if followed (after optional space) by ':', else value.
		if text[i] == '"' {
			str, next, ok := scanJSONString(text, i)
			if !ok {
				b.WriteByte(text[i])
				i++
				continue
			}
			j := next
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			if j < len(text) && text[j] == ':' {
				b.WriteString(s.JSONKey.Render(str))
			} else {
				b.WriteString(s.JSONString.Render(str))
			}
			i = next
			continue
		}
		// null / bool / number
		if lit, next, style, ok := scanJSONLiteral(text, i); ok {
			b.WriteString(style.Render(lit))
			i = next
			continue
		}
		// Fallback: copy one rune so we never spin.
		r, size := utf8.DecodeRuneInString(text[i:])
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// scanJSONString returns the full quoted string lexeme starting at i (including
// quotes) and the index just past it.
func scanJSONString(text string, i int) (string, int, bool) {
	if i >= len(text) || text[i] != '"' {
		return "", i, false
	}
	j := i + 1
	for j < len(text) {
		c := text[j]
		if c == '\\' {
			j += 2
			if j > len(text) {
				return "", i, false
			}
			continue
		}
		if c == '"' {
			return text[i : j+1], j + 1, true
		}
		if c == '\n' || c == '\r' {
			return "", i, false
		}
		j++
	}
	return "", i, false
}

// scanJSONLiteral recognises null / true / false / number at i.
func scanJSONLiteral(text string, i int) (string, int, lipgloss.Style, bool) {
	rest := text[i:]
	switch {
	case strings.HasPrefix(rest, "null") && literalEnd(rest, 4):
		return "null", i + 4, s.JSONNull, true
	case strings.HasPrefix(rest, "true") && literalEnd(rest, 4):
		return "true", i + 4, s.JSONBool, true
	case strings.HasPrefix(rest, "false") && literalEnd(rest, 5):
		return "false", i + 5, s.JSONBool, true
	}
	// Number: optional minus, digits, optional frac/exp — JSON subset.
	j := i
	if j < len(text) && text[j] == '-' {
		j++
	}
	if j >= len(text) || !unicode.IsDigit(rune(text[j])) {
		return "", i, lipgloss.Style{}, false
	}
	for j < len(text) && unicode.IsDigit(rune(text[j])) {
		j++
	}
	if j < len(text) && text[j] == '.' {
		j++
		if j >= len(text) || !unicode.IsDigit(rune(text[j])) {
			return "", i, lipgloss.Style{}, false
		}
		for j < len(text) && unicode.IsDigit(rune(text[j])) {
			j++
		}
	}
	if j < len(text) && (text[j] == 'e' || text[j] == 'E') {
		j++
		if j < len(text) && (text[j] == '+' || text[j] == '-') {
			j++
		}
		if j >= len(text) || !unicode.IsDigit(rune(text[j])) {
			return "", i, lipgloss.Style{}, false
		}
		for j < len(text) && unicode.IsDigit(rune(text[j])) {
			j++
		}
	}
	return text[i:j], j, s.JSONNumber, true
}

// literalEnd reports whether rest[n] is a JSON literal boundary (or EOS).
func literalEnd(rest string, n int) bool {
	if len(rest) == n {
		return true
	}
	c := rest[n]
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
		c == ',' || c == '}' || c == ']' || c == ':'
}
