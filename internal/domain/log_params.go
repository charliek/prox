package domain

import "time"

// LogParams holds parameters for log retrieval and streaming.
// This type is shared between the TUI and CLI packages.
//
// Fields:
//   - Process: Filter logs to a specific process name. Empty string means all processes.
//   - Lines: Number of historical log lines to return. 0 means use server default.
//   - Pattern: Text pattern for filtering log lines. Empty string means no filtering.
//   - Regex: If true, Pattern is treated as a regular expression. If false, Pattern
//     is treated as a literal substring match. Has no effect when Pattern is empty.
//   - SinceSeq: Resume cursor for GET /logs — return only entries whose server
//     ingest sequence (LogEntry.Seq) is greater than this (plan 017 C8). 0 means
//     "no cursor": the request falls back to the last-Lines path. A caller that
//     genuinely holds cursor 0 (it has consumed nothing) wants exactly that
//     fallback, so 0 is never sent on the wire — see buildLogQueryParams.
type LogParams struct {
	Process  string
	Lines    int
	Pattern  string
	Regex    bool
	SinceSeq uint64
}

// ProxyRequestParams holds parameters for proxy request retrieval and streaming.
// This type is shared between the TUI and CLI packages.
//
// Fields:
//   - Subdomain: Filter to requests for a specific subdomain. Empty string means all.
//   - Method: Filter to requests with a specific HTTP method. Empty string means all.
//   - MinStatus: Filter to requests with status code >= this value. 0 means no minimum.
//   - MaxStatus: Filter to requests with status code <= this value. 0 means no maximum.
//   - Since: Filter to requests recorded at or after this time. Zero value means no lower bound.
//   - URLContains: Filter to requests whose URL (path+query) contains this substring,
//     case-insensitive. Empty string means no filtering.
//   - Limit: Maximum number of requests to return. 0 means use server default.
type ProxyRequestParams struct {
	Subdomain   string
	Method      string
	MinStatus   int
	MaxStatus   int
	Since       time.Time
	URLContains string
	Limit       int
}
