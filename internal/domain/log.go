package domain

import "time"

// Stream represents the output stream type
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// String returns the string representation of Stream
func (s Stream) String() string {
	return string(s)
}

// LogEntry represents a single log line from a process
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Process   string    `json:"process"`
	Stream    Stream    `json:"stream"`
	Line      string    `json:"line"`
}

// LogFilter defines criteria for filtering log entries
type LogFilter struct {
	Processes []string // Filter to specific process names
	Pattern   string   // Filter by pattern match
	IsRegex   bool     // If true, Pattern is a regex; otherwise substring match
}

// IsEmpty returns true if no filters are set
func (f LogFilter) IsEmpty() bool {
	return len(f.Processes) == 0 && f.Pattern == ""
}

// MatchesProcess returns true if the process name matches the filter
func (f LogFilter) MatchesProcess(name string) bool {
	if len(f.Processes) == 0 {
		return true
	}
	for _, p := range f.Processes {
		if p == name {
			return true
		}
	}
	return false
}

// LogStats contains statistics about the log buffer
type LogStats struct {
	TotalEntries int
	BufferSize   int
	Subscribers  int
}
