package tui

import (
	"encoding/json"
	"regexp"
	"strings"

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
type logMeta struct {
	level    LogLevel
	hasLevel bool
	isJSON   bool
}

// logfmtLevelRE matches level= / lvl= tokens on the raw line (case-insensitive).
// First match wins. Compiled once at package init.
var logfmtLevelRE = regexp.MustCompile(`(?i)\b(?:level|lvl)=(debug|info|warn|warning|error|trace|fatal)\b`)

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

// classifyLevel detects a log level from the unstyled entry line (appendLogEntry
// receives pre-render text). Returns (level, true) when a level token is found.
func classifyLevel(raw string) (LogLevel, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return LogLevelUnknown, false
	}

	// JSON path: leading object with "level"/"lvl"/"severity" (string, or
	// pino/bunyan numeric). A present-but-unusable key is authoritative:
	// no fall-through to the heuristic paths.
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			for _, key := range []string{"level", "lvl", "severity"} {
				v, ok := obj[key]
				if !ok {
					continue
				}
				switch v := v.(type) {
				case string:
					return normalizeLevel(v)
				case float64:
					return pinoLevel(int(v))
				default:
					return LogLevelUnknown, false
				}
			}
		}
	}

	// logfmt path: scan the raw line (not trimmed) so mid-line tokens match.
	if m := logfmtLevelRE.FindStringSubmatch(raw); len(m) >= 2 {
		return normalizeLevel(m[1])
	}

	// Bare-token path: uppercase level word early in the line, on an
	// ANSI-stripped copy so child-emitted colors (tracing, pino-pretty)
	// don't break the delimiters. Display keeps the raw line.
	scan := ansi.Strip(raw)
	if len(scan) > bareLevelScanLimit {
		scan = scan[:bareLevelScanLimit]
	}
	if m := bareLevelRE.FindStringSubmatch(scan); len(m) >= 2 {
		return normalizeLevel(m[1])
	}

	return LogLevelUnknown, false
}

// isJSONObject reports whether raw is a JSON object (trimmed leading `{` and
// unmarshals as map). C9 reuses this for JSON pretty-print detection.
func isJSONObject(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var obj map[string]any
	return json.Unmarshal([]byte(trimmed), &obj) == nil
}
