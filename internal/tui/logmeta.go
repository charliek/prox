package tui

import (
	"encoding/json"
	"regexp"
	"strings"
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

// normalizeLevel maps a level token to LogLevel. warning→warn and fatal→error
// per plan 021 WS7.
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
	case "error", "fatal":
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

	// JSON path: leading object with string "level" or "lvl".
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			for _, key := range []string{"level", "lvl"} {
				v, ok := obj[key]
				if !ok {
					continue
				}
				s, ok := v.(string)
				if !ok {
					// Non-string level (e.g. {"level":123}) → no level.
					return LogLevelUnknown, false
				}
				return normalizeLevel(s)
			}
		}
	}

	// logfmt path: scan the raw line (not trimmed) so mid-line tokens match.
	if m := logfmtLevelRE.FindStringSubmatch(raw); len(m) >= 2 {
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
