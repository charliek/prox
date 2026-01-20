package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStream_String(t *testing.T) {
	assert.Equal(t, "stdout", StreamStdout.String())
	assert.Equal(t, "stderr", StreamStderr.String())
}

func TestLogFilter_IsEmpty(t *testing.T) {
	tests := []struct {
		name   string
		filter LogFilter
		want   bool
	}{
		{
			name:   "empty filter",
			filter: LogFilter{},
			want:   true,
		},
		{
			name:   "with processes",
			filter: LogFilter{Processes: []string{"web"}},
			want:   false,
		},
		{
			name:   "with pattern",
			filter: LogFilter{Pattern: "error"},
			want:   false,
		},
		{
			name:   "with both",
			filter: LogFilter{Processes: []string{"web"}, Pattern: "error"},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.filter.IsEmpty())
		})
	}
}

func TestLogFilter_MatchesProcess(t *testing.T) {
	tests := []struct {
		name    string
		filter  LogFilter
		process string
		want    bool
	}{
		{
			name:    "empty filter matches all",
			filter:  LogFilter{},
			process: "web",
			want:    true,
		},
		{
			name:    "matches included process",
			filter:  LogFilter{Processes: []string{"web", "api"}},
			process: "web",
			want:    true,
		},
		{
			name:    "does not match excluded process",
			filter:  LogFilter{Processes: []string{"web", "api"}},
			process: "worker",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.filter.MatchesProcess(tt.process))
		})
	}
}
