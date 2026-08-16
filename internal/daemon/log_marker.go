package daemon

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runMarkerPrefix is what a run marker line STARTS with. It is matched
// separately from runMarkerPattern so a line that begins a marker but does not
// finish one (a crash mid-write, a torn tail) is recognized as a DAMAGED marker
// rather than mistaken for ordinary log output -- see classifyMarkerLine.
const runMarkerPrefix = "--- run "

// runMarkerPattern matches a run marker line written by WriteRunMarker:
//
//	--- run <RFC3339> pid=<childpid> ---
//
// SetupLogging writes one of these immediately after opening .prox/prox.log
// and before redirecting os.Stdout/os.Stderr, so printDaemonFailure
// (internal/cli/daemon_startup.go) can tail only the CURRENT run's output
// instead of the whole accumulated history of a log that is never truncated
// (issue #99). The parser lives here, next to the writer, so the two cannot
// drift apart.
//
// The timestamp group is loose on purpose -- the shape is checked here, the
// VALUE is parsed as real RFC3339 in classifyMarkerLine. A regex that merely
// looks timestamp-ish would accept `--- run yesterday pid=100 ---`, and every
// line this parser wrongly accepts as a marker is a line that can relabel one
// run's output as another's.
var runMarkerPattern = regexp.MustCompile(`^--- run (\S+) pid=(\d+) ---$`)

// MaxRunTailLines caps how many lines of a run's segment FindRunMarkerTail
// returns. .prox/prox.log is never truncated and a single run can be arbitrarily
// chatty (a process in a crash loop, a debug-logging framework), so an uncapped
// segment means an uncapped print into the user's terminal on a startup
// failure. The newest lines are kept -- a failure's cause is at the END of what
// it managed to log -- and the caller is told when older ones were dropped.
const MaxRunTailLines = 200

// WriteRunMarker writes a run marker line identifying this run by pid and
// start time to w. Callers must write it before anything else so a later scan
// for "this run's output" has an unambiguous starting line.
func WriteRunMarker(w io.Writer, pid int) error {
	_, err := fmt.Fprintf(w, "--- run %s pid=%d ---\n", time.Now().UTC().Format(time.RFC3339), pid)
	return err
}

// markerKind classifies one log line's relationship to the run-marker format.
type markerKind int

const (
	// notMarker is ordinary log output.
	notMarker markerKind = iota
	// damagedMarker is a line that begins like a marker but does not fully
	// parse as one: a marker whose write was cut short by a crash or a torn
	// disk write, or -- indistinguishably -- a log line that happens to open
	// with the marker prefix. Either way it is evidence that the segment it
	// sits in cannot be trusted to belong to a single, identified run.
	damagedMarker
	// validMarker is a complete, fully parsed marker.
	validMarker
)

// classifyMarkerLine reports what line is, and (for a validMarker) the pid it
// names. Validity is strict: the prefix, an RFC3339 timestamp that actually
// PARSES, a positive decimal pid, and the trailing delimiter. Anything that
// starts like a marker and fails any of that is damaged, never ordinary output.
func classifyMarkerLine(line string) (markerKind, int) {
	if !strings.HasPrefix(line, runMarkerPrefix) {
		return notMarker, 0
	}
	m := runMarkerPattern.FindStringSubmatch(line)
	if m == nil {
		return damagedMarker, 0
	}
	if _, err := time.Parse(time.RFC3339, m[1]); err != nil {
		return damagedMarker, 0
	}
	pid, err := strconv.Atoi(m[2])
	if err != nil || pid <= 0 {
		return damagedMarker, 0
	}
	return validMarker, pid
}

// FindRunMarkerTail scans log content for the run marker matching pid and
// returns the content between it and the next marker line (of any pid), or
// end of content if there is none. When pid appears in more than one marker
// (a pid can be reused across runs over a long-lived log), the LAST such
// marker is used, since that is the most recent run to have owned it. At most
// MaxRunTailLines lines are returned; truncated reports whether older lines of
// the segment were dropped to honor that cap, so the caller can say so rather
// than present a partial segment as the whole of it.
//
// ok is false when no marker for pid is found at all -- a legacy log written
// before this feature existed, the pre-SetupLogging window (checkEarlyDeath
// can fire before the child ever reaches SetupLogging), or a log that only
// carries markers for OTHER pids. Callers must fall back to a different
// diagnostic in that case rather than print another run's content as if it
// were the current one -- that regression is exactly what this function
// exists to prevent.
//
// ok is ALSO false when a damagedMarker is the first marker-ish line after the
// match. Such a line means some later run began writing a marker that never
// landed, so the segment before it cannot be shown as "the run that owns pid":
// under pid reuse it is a PREVIOUS run's output, and presenting it as current
// is precisely the misinformation #99 exists to kill. Refusing costs the caller
// a scoped tail and buys back a fallback that claims nothing about which run it
// came from -- show less, honestly, over showing the wrong run.
func FindRunMarkerTail(content string, pid int) (tail string, truncated bool, ok bool) {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	matchLine := -1
	for i, line := range lines {
		if kind, linePid := classifyMarkerLine(line); kind == validMarker && linePid == pid {
			matchLine = i
		}
	}
	if matchLine == -1 {
		return "", false, false
	}

	end := len(lines)
	for i := matchLine + 1; i < len(lines); i++ {
		kind, _ := classifyMarkerLine(lines[i])
		if kind == damagedMarker {
			return "", false, false
		}
		if kind == validMarker {
			end = i
			break
		}
	}

	segment := lines[matchLine+1 : end]
	if len(segment) > MaxRunTailLines {
		segment = segment[len(segment)-MaxRunTailLines:]
		truncated = true
	}
	return strings.Join(segment, "\n"), truncated, true
}
