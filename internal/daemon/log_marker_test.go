package daemon

import (
	"bytes"
	"fmt"
	"os"
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
	// The strict classifier, not just the regex: the timestamp the writer emits
	// must survive time.Parse(RFC3339), or every marker this daemon writes is a
	// damaged one and no run is ever scoped again.
	kind, pid := classifyMarkerLine(strings.TrimRight(line, "\n"))
	if kind != validMarker || pid != 4242 {
		t.Errorf("classifyMarkerLine(%q) = (%v, %d), want (validMarker, 4242)", line, kind, pid)
	}
}

// TestClassifyMarkerLine pins the three-way split the tail scan depends on. The
// damaged cases are the load-bearing ones: each is a line that a
// prefix-and-shape check would wave through as a real marker (or ignore as
// ordinary output), and each would silently re-attribute a run's log to another
// run if it did.
func TestClassifyMarkerLine(t *testing.T) {
	cases := []struct {
		line     string
		wantKind markerKind
		wantPid  int
	}{
		{line: "--- run 2026-08-15T09:00:00Z pid=100 ---", wantKind: validMarker, wantPid: 100},
		{line: "--- run 2026-08-15T09:00:00+02:00 pid=7 ---", wantKind: validMarker, wantPid: 7},
		{line: "starting web", wantKind: notMarker},
		{line: "", wantKind: notMarker},
		{line: "--- run 2026-08-15T09:00:00Z pid=10", wantKind: damagedMarker},
		{line: "--- run ", wantKind: damagedMarker},
		{line: "--- run yesterday pid=100 ---", wantKind: damagedMarker},
		{line: "--- run 2026-13-45T99:00:00Z pid=100 ---", wantKind: damagedMarker},
		{line: "--- run 2026-08-15T09:00:00Z pid=0 ---", wantKind: damagedMarker},
		{line: "--- run 2026-08-15T09:00:00Z pid=99999999999999999999 ---", wantKind: damagedMarker},
	}
	for _, tc := range cases {
		gotKind, gotPid := classifyMarkerLine(tc.line)
		if gotKind != tc.wantKind || gotPid != tc.wantPid {
			t.Errorf("classifyMarkerLine(%q) = (%v, %d), want (%v, %d)",
				tc.line, gotKind, gotPid, tc.wantKind, tc.wantPid)
		}
	}
}

// TestFindRunMarkerTail_RealMarkerParses closes the writer/parser loop against
// a marker written with the CURRENT clock rather than a hand-typed timestamp,
// so a formatting change that time.Parse rejects cannot pass the table above.
func TestFindRunMarkerTail_RealMarkerParses(t *testing.T) {
	var buf bytes.Buffer
	pid := os.Getpid()
	if err := WriteRunMarker(&buf, pid); err != nil {
		t.Fatalf("WriteRunMarker failed: %v", err)
	}
	buf.WriteString("hello\n")

	tail, truncated, ok := FindRunMarkerTail(buf.String(), pid)
	if !ok || truncated || tail != "hello" {
		t.Errorf("FindRunMarkerTail = (%q, %v, %v), want (%q, false, true)", tail, truncated, ok, "hello")
	}
}

// TestFindRunMarkerTail is the table test for the parser (plan 027 C12): the
// same failure #99 exists to kill is a naive "tail from the LAST marker" rule
// finding a DIFFERENT run's marker and presenting its content as current --
// strictly worse than the old unscoped tail. Every case here guards against
// that regression as well as the ordinary happy path.
func TestFindRunMarkerTail(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		pid           int
		wantTail      string
		wantTruncated bool
		wantOK        bool
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
		{
			// The #99 failure mode, reached through a CRASH instead of a typo:
			// the current run got partway through writing its marker and died.
			// The previous run reused this pid, so its marker is the only valid
			// one -- and returning its output would label a dead run's log as
			// this one's. The damaged line ends the segment AND disqualifies it.
			name: "a damaged marker after the match disqualifies the segment",
			content: "" +
				"--- run 2026-08-15T08:00:00Z pid=100 ---\n" +
				"previous run's output, under a reused pid\n" +
				"--- run 2026-08-15T09:00:00Z pid=10",
			pid:      100,
			wantTail: "",
			wantOK:   false,
		},
		{
			name: "a damaged marker before the match is harmless",
			content: "" +
				"--- run 2026-08-15T08:00:00Z pid=1\n" +
				"torn output from a run that never identified itself\n" +
				"--- run 2026-08-15T09:00:00Z pid=100 ---\n" +
				"this run's output\n",
			pid:      100,
			wantTail: "this run's output",
			wantOK:   true,
		},
		{
			// A marker is only a marker if its timestamp is a real RFC3339
			// instant. A loose \S+ accepts anything non-blank, which turns any
			// line of the shape "--- run <word> pid=<n> ---" -- a process
			// echoing its own command line, say -- into a run boundary.
			name: "a marker-shaped line with a non-RFC3339 timestamp is not a marker",
			content: "" +
				"--- run yesterday pid=100 ---\n" +
				"output that belongs to nobody identifiable\n",
			pid:      100,
			wantTail: "",
			wantOK:   false,
		},
		{
			name: "a marker with a pid of zero is not a marker",
			content: "" +
				"--- run 2026-08-15T09:00:00Z pid=0 ---\n" +
				"output\n",
			pid:      0,
			wantTail: "",
			wantOK:   false,
		},
		{
			name:          "a segment longer than the cap keeps the newest lines and says so",
			content:       "--- run 2026-08-15T09:00:00Z pid=100 ---\n" + numberedLines(MaxRunTailLines+50),
			pid:           100,
			wantTail:      strings.Join(splitLines(strings.TrimRight(numberedLines(MaxRunTailLines+50), "\n"))[50:], "\n"),
			wantTruncated: true,
			wantOK:        true,
		},
		{
			name:          "a segment exactly at the cap is not truncated",
			content:       "--- run 2026-08-15T09:00:00Z pid=100 ---\n" + numberedLines(MaxRunTailLines),
			pid:           100,
			wantTail:      strings.TrimRight(numberedLines(MaxRunTailLines), "\n"),
			wantTruncated: false,
			wantOK:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTail, gotTruncated, gotOK := FindRunMarkerTail(tt.content, tt.pid)
			if gotOK != tt.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotTruncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", gotTruncated, tt.wantTruncated)
			}
			if gotTail != tt.wantTail {
				t.Errorf("tail = %q, want %q", gotTail, tt.wantTail)
			}
			if lines := len(splitLines(gotTail)); gotOK && lines > MaxRunTailLines {
				t.Errorf("tail is %d lines, above the %d cap", lines, MaxRunTailLines)
			}
		})
	}
}

// numberedLines builds n distinct log lines (trailing newline included) so a
// cap test can tell WHICH end of the segment survived.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// splitLines splits a joined tail back into lines, treating "" as no lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
