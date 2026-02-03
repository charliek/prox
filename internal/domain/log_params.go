package domain

// LogParams holds parameters for log retrieval and streaming.
// This type is shared between the TUI and CLI packages.
//
// Fields:
//   - Process: Filter logs to a specific process name. Empty string means all processes.
//   - Lines: Number of historical log lines to return. 0 means use server default.
//   - Pattern: Text pattern for filtering log lines. Empty string means no filtering.
//   - Regex: If true, Pattern is treated as a regular expression. If false, Pattern
//     is treated as a literal substring match. Has no effect when Pattern is empty.
type LogParams struct {
	Process string
	Lines   int
	Pattern string
	Regex   bool
}

// ProxyRequestParams holds parameters for proxy request retrieval and streaming.
// This type is shared between the TUI and CLI packages.
//
// Fields:
//   - Subdomain: Filter to requests for a specific subdomain. Empty string means all.
//   - Method: Filter to requests with a specific HTTP method. Empty string means all.
//   - MinStatus: Filter to requests with status code >= this value. 0 means no minimum.
//   - MaxStatus: Filter to requests with status code <= this value. 0 means no maximum.
//   - Limit: Maximum number of requests to return. 0 means use server default.
type ProxyRequestParams struct {
	Subdomain string
	Method    string
	MinStatus int
	MaxStatus int
	Limit     int
}
