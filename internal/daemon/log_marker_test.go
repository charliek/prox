package daemon

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteRunMarker pins the exact marker line format: WriteRunMarker's
// output is what FindRunMarkerTail's pattern must parse, and what a human
// tailing the raw file with `tail -f` sees between runs.
func TestWriteRunMarker(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRunMarker(&buf, 4242); err != nil {
		t.Fatalf("WriteRunMarker failed: %v", err)
	}

	line := buf.String()
	if !strings.HasPrefix(line, "--- run ") {
		t.Errorf("expected marker to start with %q, got %q", "--- run ", line)
	}
	if !strings.Contains(line, "pid=4242") {
		t.Errorf("expected marker to contain pid=4242, got %q", line)
	}
	if !strings.HasSuffix(line, " ---\n") {
		t.Errorf("expected marker to end with %q, got %q", " ---\n", line)
	}
	if !runMarkerPattern.MatchString(strings.TrimRight(line, "\n")) {
		t.Errorf("marker line %q does not match runMarkerPattern -- writer and parser have drifted", line)
	}
}

// TestFindRunMarkerTail is the table test for the parser (plan 027 C12): the
// same failure #99 exists to kill is a naive "tail from the LAST marker" rule
// finding a DIFFERENT run's marker and presenting its content as current --
// strictly worse than the old unscoped tail. Every case here guards against
// that regression as well as the ordinary happy path.
func TestFindRunMarkerTail(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		pid      int
		wantTail string
		wantOK   bool
	}{
		{
			name: "matching-pid marker",
			content: "" +
				"--- run 2026-08-15T10:00:00Z pid=100 ---\n" +
				"starting up\n" +
				"config loaded\n",
			pid:      100,
			wantTail: "starting up\nconfig loaded",
			wantOK:   true,
		},
		{
			name: "marker for a different pid present must fall back",
			content: "" +
				"--- run 2026-08-15T09:00:00Z pid=999 ---\n" +
				"a previous, unrelated run's output\n" +
				"more of that old run\n",
			pid:      100,
			wantTail: "",
			wantOK:   false,
		},
		{
			name: "multiple markers selects the one matching pid, not the last overall",
			content: "" +
				"--- run 2026-08-15T09:00:00Z pid=100 ---\n" +
				"first run's output\n" +
				"--- run 2026-08-15T09:05:00Z pid=200 ---\n" +
				"second run's output (different pid)\n" +
				"more second run output\n",
			pid:      100,
			wantTail: "first run's output",
			wantOK:   true,
		},
		{
			name: "multiple markers selects the later of two markers matching the same pid",
			content: "" +
				"--- run 2026-08-15T08:00:00Z pid=100 ---\n" +
				"stale run reusing this pid\n" +
				"--- run 2026-08-15T09:00:00Z pid=100 ---\n" +
				"current run reusing this pid\n",
			pid:      100,
			wantTail: "current run reusing this pid",
			wantOK:   true,
		},
		{
			name:     "no marker at all is a legacy log",
			content:  "plain old log line 1\nplain old log line 2\n",
			pid:      100,
			wantTail: "",
			wantOK:   false,
		},
		{
			name: "marker with no content after it",
			content: "" +
				"--- run 2026-08-15T09:00:00Z pid=100 ---\n" +
				"--- run 2026-08-15T09:00:05Z pid=200 ---\n",
			pid:      100,
			wantTail: "",
			wantOK:   true,
		},
		{
			name: "marker with no content after it and no later marker",
			content: "" +
				"--- run 2026-08-15T09:00:00Z pid=100 ---\n",
			pid:      100,
			wantTail: "",
			wantOK:   true,
		},
		{
			name:     "empty content",
			content:  "",
			pid:      100,
			wantTail: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTail, gotOK := FindRunMarkerTail(tt.content, tt.pid)
			if gotOK != tt.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotTail != tt.wantTail {
				t.Errorf("tail = %q, want %q", gotTail, tt.wantTail)
			}
		})
	}
}
