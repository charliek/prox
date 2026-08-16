package daemon

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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
var runMarkerPattern = regexp.MustCompile(`^--- run (\S+) pid=(\d+) ---$`)

// WriteRunMarker writes a run marker line identifying this run by pid and
// start time to w. Callers must write it before anything else so a later scan
// for "this run's output" has an unambiguous starting line.
func WriteRunMarker(w io.Writer, pid int) error {
	_, err := fmt.Fprintf(w, "--- run %s pid=%d ---\n", time.Now().UTC().Format(time.RFC3339), pid)
	return err
}

// FindRunMarkerTail scans log content for the run marker matching pid and
// returns the content between it and the next marker line (of any pid), or
// end of content if there is none. When pid appears in more than one marker
// (a pid can be reused across runs over a long-lived log), the LAST such
// marker is used, since that is the most recent run to have owned it.
//
// ok is false when no marker for pid is found at all -- a legacy log written
// before this feature existed, the pre-SetupLogging window (checkEarlyDeath
// can fire before the child ever reaches SetupLogging), or a log that only
// carries markers for OTHER pids. Callers must fall back to a different
// diagnostic in that case rather than print another run's content as if it
// were the current one -- that regression is exactly what this function
// exists to prevent.
func FindRunMarkerTail(content string, pid int) (tail string, ok bool) {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	matchLine := -1
	for i, line := range lines {
		m := runMarkerPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		linePid, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		if linePid == pid {
			matchLine = i
		}
	}
	if matchLine == -1 {
		return "", false
	}

	end := len(lines)
	for i := matchLine + 1; i < len(lines); i++ {
		if runMarkerPattern.MatchString(lines[i]) {
			end = i
			break
		}
	}

	return strings.Join(lines[matchLine+1:end], "\n"), true
}
