package cli

import (
	"fmt"
	"io"
)

// reportTUIWarnings routes the mode resolver's warnings (an unrecognized
// PROX_TUI value) to whichever screen the user is actually going to look at.
//
// Under a TUI the warning goes into the preamble, because stderr at this point
// in startup is the primary screen — which bubbletea is about to hide behind the
// alt screen for the entire session, so a warning written only there is a
// warning nobody sees.
//
// It ALSO goes to stderr, in both modes. The preamble is only ever rendered if
// the TUI actually opens, and plenty of startup can still fail first: a config
// load, an API bind, a proxy start, a supervisor start. In those runs `prox up`
// exits without a TUI and the in-memory log manager is discarded, so a
// preamble-only warning is lost outright — silently ignoring a setting the user
// believes is in force, which is the exact failure the warning exists to prevent
// (codex review finding). The duplicate under a TUI is harmless: the stderr copy
// sits on the primary screen, hidden during the session and visible again after
// it, while the preamble copy is the one seen during it.
func reportTUIWarnings(warnings []string, pre *startupPreamble, tuiEnabled bool, stderr io.Writer) {
	for _, w := range warnings {
		if tuiEnabled {
			pre.note("%s", w)
		}
		fmt.Fprintln(stderr, w)
	}
}

// startupPreamble collects the session-info lines `prox up` emits while it is
// starting — the config path, the API address, the auth-token path, the proxy
// URLs and registered domains, the process list, and any warning the TUI-mode
// resolver returned.
//
// Those lines are the most useful thing prox prints (the proxy URL is *the*
// answer to "where is my app?"), and under a TUI session the terminal they were
// printed on is hidden behind the alt screen for the whole run. So a TUI session
// needs them delivered through the TUI itself, by two paths that are NOT
// redundant:
//
//  1. tui.ClientOptions.Preamble — the GUARANTEED path. The model renders these
//     on construction, pinned, so nothing can push them out.
//  2. the supervisor's SystemLog — so the same lines reach `prox logs`, the
//     daemon log file, and every other subscriber.
//
// Path 2 alone is not enough: the TUI backfills at most 1000 entries from a ring
// that holds 1000 entries SHARED with process output, so a chatty startup (a
// webpack build, a migration) can evict the entire preamble before the TUI has
// even connected. Path 1 alone is not enough either — it reaches one screen and
// nothing else.
//
// Terminal printing is unchanged either way: printf still writes to stdout
// exactly as the bare fmt.Printf calls it replaced did, so plain `prox up`, a
// `-d` child and CI see byte-identical output. Collection and system logging are
// gated on enabled (true only when this run resolved to a TUI), which is also
// what keeps plain mode free of duplicate terminal lines — its log subscriber
// prints every system entry to the very same terminal.
//
// Single-goroutine by construction: every call site is on runUp's own goroutine
// during startup, before the TUI or any concurrent producer exists.
type startupPreamble struct {
	enabled bool
	lines   []string
	// logged is how many of lines have already been handed to logf. Lines added
	// before the supervisor exists (the resolver's warnings) sit here until
	// logTo attaches the sink and flushes them.
	logged int
	logf   func(string, ...interface{})
}

// newStartupPreamble returns an accumulator. When enabled is false (plain
// streaming) it records nothing and logs nothing — printf degrades to the
// fmt.Printf it replaced.
func newStartupPreamble(enabled bool) *startupPreamble {
	return &startupPreamble{enabled: enabled}
}

// printf prints one preamble line to the terminal — exactly as before — and
// records it for the TUI.
func (p *startupPreamble) printf(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line)
	p.record(line)
}

// note records a line WITHOUT printing it. It exists for the mode resolver's
// warnings, which a TUI session must not write to a screen it is about to hide;
// the plain-mode caller prints those to stderr itself and never reaches here.
func (p *startupPreamble) note(format string, args ...interface{}) {
	p.record(fmt.Sprintf(format, args...))
}

// record appends an already-formatted line and, if the system-log sink is
// attached, emits it there too.
func (p *startupPreamble) record(line string) {
	if !p.enabled {
		return
	}
	p.lines = append(p.lines, line)
	p.flush()
}

// logTo attaches the supervisor's SystemLog (path 2) and flushes everything
// recorded before the supervisor existed. Safe to call before sup.Start.
func (p *startupPreamble) logTo(logf func(string, ...interface{})) {
	if !p.enabled {
		return
	}
	p.logf = logf
	p.flush()
}

// flush emits every not-yet-logged line through logf. The lines are already
// formatted, so they go through a "%s" verb: a config path or a URL containing
// a percent sign must not be re-interpreted as a format string.
func (p *startupPreamble) flush() {
	if p.logf == nil {
		return
	}
	for _, line := range p.lines[p.logged:] {
		p.logf("%s", line)
	}
	p.logged = len(p.lines)
}

// Lines returns the collected preamble for tui.ClientOptions (path 1). Empty in
// plain mode.
func (p *startupPreamble) Lines() []string {
	return p.lines
}
