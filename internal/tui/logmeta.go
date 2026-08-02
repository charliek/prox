package tui

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// LogLevel is a content-derived severity parsed from a log line (plan 021 WS7).
// Unknown means no level was detected; rendering and filtering treat it as
// uncolored / excluded from level: queries (C7/C9).
type LogLevel int

const (
	LogLevelUnknown LogLevel = iota
	LogLevelTrace
	LogLevelDebug
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

// logMeta caches ingest-time classification keyed by DisplaySeq (plan 021 WS7).
// Eviction prunes keys in lockstep with the log ring (Codex #11).
// pairs is a width-independent JSON path=value list filled at ingest from the
// same Unmarshal that feeds level detection (plan 023 C14); render formats
// from pairs without re-unmarshalling.
type logMeta struct {
	level    LogLevel
	hasLevel bool
	isJSON   bool
	pairs    []jsonPair
}

// logfmtLevelRE matches level= / lvl= tokens on the raw line (case-insensitive).
// The key must be at line start or preceded by whitespace so tokens embedded in
// words (xlevel=, mylevel=) do not match. First match wins.
var logfmtLevelRE = regexp.MustCompile(`(?i)(?:^|\s)(?:level|lvl)=(debug|info|warn|warning|error|trace|fatal)(?:\s|$|[\]\)\}:,])`)

// bareLevelRE matches a standalone UPPERCASE level token early in a line —
// the shape of python's %(levelname)s (stridelabs-python dev), tracing's
// text fmt layer (stridelabs-rust), pino-pretty (callbell), and uvicorn's
// "INFO:     ..." access lines. Case-sensitive on purpose: prose mentions
// ("error handling…") stay Unknown. The trailing delimiter is consumed by
// the match (Go regexp has no lookahead); longer alternatives precede
// shorter ones so WARNING wins over WARN.
var bareLevelRE = regexp.MustCompile(`(?:^|[\s\[\(\{])(TRACE|DEBUG|INFO|WARNING|WARN|ERROR|CRITICAL|FATAL)(?:[\s\]\)\}:]|$)`)

// bareLevelScanLimit caps the bare-token scan: dev formats place the level
// within ~35 columns (right after a timestamp); later occurrences are prose.
const bareLevelScanLimit = 80

// normalizeLevel maps a level token to LogLevel. warning→warn and
// fatal/critical→error per plan 021 WS7 (+ python CRITICAL).
func normalizeLevel(raw string) (LogLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "trace":
		return LogLevelTrace, true
	case "debug":
		return LogLevelDebug, true
	case "info":
		return LogLevelInfo, true
	case "warn", "warning":
		return LogLevelWarn, true
	case "error", "fatal", "critical":
		return LogLevelError, true
	default:
		return LogLevelUnknown, false
	}
}

// pinoLevel maps pino/bunyan numeric JSON levels (10=trace … 60=fatal).
func pinoLevel(n int) (LogLevel, bool) {
	switch n {
	case 10:
		return LogLevelTrace, true
	case 20:
		return LogLevelDebug, true
	case 30:
		return LogLevelInfo, true
	case 40:
		return LogLevelWarn, true
	case 50, 60:
		return LogLevelError, true
	default:
		return LogLevelUnknown, false
	}
}

// classifyJSONObjectLevel reads level/lvl/severity from a parsed JSON object.
// ok is true when a usable level was found. authoritative is true when any of
// those keys is present (even if unusable) — callers must not fall through to
// heuristics in that case.
func classifyJSONObjectLevel(obj map[string]any) (level LogLevel, ok bool, authoritative bool) {
	for _, key := range []string{"level", "lvl", "severity"} {
		v, present := obj[key]
		if !present {
			continue
		}
		switch v := v.(type) {
		case string:
			level, ok = normalizeLevel(v)
			return level, ok, true
		case float64:
			level, ok = pinoLevel(int(v))
			return level, ok, true
		default:
			return LogLevelUnknown, false, true
		}
	}
	return LogLevelUnknown, false, false
}

// classifyLevelHeuristics applies logfmt then bare-token detection (non-JSON).
func classifyLevelHeuristics(raw string) (LogLevel, bool) {
	if m := logfmtLevelRE.FindStringSubmatch(raw); len(m) >= 2 {
		return normalizeLevel(m[1])
	}
	scan := ansi.Strip(raw)
	if len(scan) > bareLevelScanLimit {
		scan = truncateUTF8Bytes(scan, bareLevelScanLimit)
	}
	if m := bareLevelRE.FindStringSubmatch(scan); len(m) >= 2 {
		return normalizeLevel(m[1])
	}
	return LogLevelUnknown, false
}

// truncateUTF8Bytes returns the prefix of s with byte length ≤ max that does
// not split a UTF-8 code point.
func truncateUTF8Bytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// ingestLogMeta classifies a line at append time. JSON objects pay one
// Unmarshal that feeds both level detection and a cached path=value pair list
// for render (plan 023 C14). Eviction of the containing logMeta map entry is
// unchanged (keyed by DisplaySeq, pruned with the ring).
func ingestLogMeta(raw string) logMeta {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return logMeta{}
	}

	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			pairs := flattenJSONPairs(obj, "", 0)
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].path < pairs[j].path })
			meta := logMeta{isJSON: true, pairs: pairs}
			if level, ok, auth := classifyJSONObjectLevel(obj); auth {
				meta.level = level
				meta.hasLevel = ok
				return meta
			}
			// Valid JSON object without a level key: still cache pairs; fall
			// through so logfmt/bare can tint the line.
			level, has := classifyLevelHeuristics(raw)
			meta.level = level
			meta.hasLevel = has
			return meta
		}
	}

	level, has := classifyLevelHeuristics(raw)
	return logMeta{level: level, hasLevel: has}
}
