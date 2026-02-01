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
