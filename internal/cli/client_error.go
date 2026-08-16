package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/charliek/prox/internal/domain"
)

// This file owns the two rules that keep a failed client command's message
// TRUE (plan 027 C11, #95):
//
//  1. the "Is prox running?" hint is attached only when the error POSITIVELY
//     says nothing is listening (hintFits below), and
//  2. a PROCESS_NOT_FOUND from the daemon is enriched, client-side, with the
//     name that was asked for and the names that exist (enrichProcessNotFound).
//
// Both exist because the previous behavior printed advice the code could
// already prove wrong: `prox stop boom` against a LIVE daemon printed the
// daemon's own answer followed by "Is prox running? Try 'prox up' first."

// clientError wraps err with hint, but ONLY when the error is consistent with
// "nothing is listening". Every hint threaded through here is a variation on
// "Is prox running? Try 'prox up' first", i.e. an assertion about the daemon's
// liveness — so attaching it unconditionally (which is what this function used
// to do, never once inspecting err) printed that assertion on top of errors
// that could only have come from a daemon that had just answered.
//
// A nil hint, or an error that does not qualify, returns err untouched.
func clientError(err error, hint string) error {
	if hint == "" || !hintFits(err) {
		return err
	}
	return fmt.Errorf("%w\n%s", err, hint)
}

// hintFits reports whether err is positive evidence that no daemon is
// listening — the only case in which telling the user to run `prox up` names an
// action that actually works.
//
// The classification is POSITIVE, not a negation of the daemon-answered cases,
// and it fails CLOSED: an error this function does not recognize gets no hint,
// because an unrecognized error must never be turned into an assertion that the
// daemon is down. The order mirrors classifyOwnershipProbeFailure (root.go),
// which faced the same question for the ownership probe, and reuses its
// predicates so the two cannot drift:
//
//  1. decode failure — the daemon ANSWERED, just not with something parseable
//     (client.go's "decoding response: ..."). Checked first because a truncated
//     or empty body surfaces as io.EOF, which the transport check below would
//     otherwise read as "nothing is listening".
//  2. *APIError — the daemon answered with a status. `prox stop boom` returning
//     PROCESS_NOT_RUNNING is the canonical case: prox just replied.
//  3. context cancellation — Ctrl-C, or a caller's own deadline. A live daemon
//     produces this as readily as a dead one, and it arrives wrapped in
//     *url.Error, so it must be excluded BEFORE the transport check.
//  4. timeout — something accepted the connection and never answered (or the
//     client's own timeout fired). A wedged listener IS a listener; `prox up`
//     is not the fix, so no hint.
//  5. positively unreachable — dial refused, reset, DNS/route failure. THIS is
//     the hint's case.
//  6. anything else — no hint.
func hintFits(err error) bool {
	if err == nil {
		return false
	}
	if isDecodeFailure(err) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if isProbeTimeout(err) {
		return false
	}
	// ECONNREFUSED/ECONNRESET are named explicitly so a raw dial error that
	// never passed through http.Client.Do (and is therefore not wrapped in
	// *url.Error) still classifies as unreachable.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	return isUnreachable(err)
}

// processClientError is the error path for the single-process lifecycle
// commands (`prox start|stop|restart <name>`). It is the seam clientError
// cannot be: enriching PROCESS_NOT_FOUND needs both the name that was asked for
// and the client to ask the daemon what names exist, neither of which an
// (err, hint) pair carries.
func processClientError(client *Client, name string, err error, hint string) error {
	return clientError(enrichProcessNotFound(client, name, err), hint)
}

// enrichProcessNotFound turns the daemon's bare "PROCESS_NOT_FOUND: process not
// found" into a message that names the process that was asked for and either
// the name that was probably meant or the names that exist.
//
// The valid names come from the DAEMON (GetProcesses), never from a client-side
// config.Load: the running daemon's own view is the only one that answers the
// question being asked, and a client-side load diverges the moment `-c` names a
// different file than the daemon loaded (the trap plan 020 C3 removed with
// getProcessNames).
//
// Enrichment is best-effort: any failure of that lookup returns the ORIGINAL
// error unchanged, so a slow or wedged daemon never hides the real failure
// behind an enrichment error of our own making.
func enrichProcessNotFound(client *Client, name string, err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != domain.ErrCodeProcessNotFound {
		return err
	}
	names, ok := knownProcessNames(client)
	if !ok {
		return err
	}
	return &processNotFoundError{err: err, name: name, detail: processNameHelp(name, names)}
}

// processNotFoundError renders the daemon's own error, the name that was
// rejected, and the client-side help on its own line. It unwraps to the
// daemon's error so errors.As/Is still reach the *APIError (and its
// PROCESS_NOT_FOUND code) underneath.
type processNotFoundError struct {
	err    error
	name   string
	detail string
}

func (e *processNotFoundError) Error() string {
	return fmt.Sprintf("%s: %q\n%s", e.err, e.name, e.detail)
}

func (e *processNotFoundError) Unwrap() error { return e.err }

// unknownProcessError is the local (never daemon-issued) form, for a name the
// CLI rejects before it sends a request at all — see validateLogProcesses.
func unknownProcessError(name string, names []string) error {
	return fmt.Errorf("unknown process %q\n%s", name, processNameHelp(name, names))
}

// knownProcessNames returns the daemon's current process names. The bool
// reports whether the daemon actually answered: an empty list from a daemon
// with nothing configured is a real answer ("there are no processes"), which is
// a different thing from not having been able to ask.
func knownProcessNames(client *Client) ([]string, bool) {
	resp, err := client.GetProcesses()
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(resp.Processes))
	for _, p := range resp.Processes {
		names = append(names, p.Name)
	}
	return names, true
}

// processNameHelp is the one-line "what should I have typed" advice shared by
// the daemon-side and client-side unknown-name messages. The matching rule is
// pinned: a case-insensitive exact match wins outright, then a SINGLE closest
// name within Levenshtein distance 2, and otherwise the full list — a tie
// between two equally close names is not a suggestion, it is a list.
func processNameHelp(name string, names []string) string {
	if len(names) == 0 {
		return "This prox has no processes."
	}
	if suggestion, ok := suggestProcessName(name, names); ok {
		return fmt.Sprintf("Did you mean %q?", suggestion)
	}
	return "Known processes: " + strings.Join(names, ", ")
}

// suggestProcessName picks the single name a mistyped one probably meant.
func suggestProcessName(name string, names []string) (string, bool) {
	// A case-insensitive exact match is unambiguous: the user typed the right
	// name in the wrong case, and process names are matched case-sensitively
	// everywhere in prox.
	for _, candidate := range names {
		if candidate != name && strings.EqualFold(candidate, name) {
			return candidate, true
		}
	}

	const maxDistance = 2
	best, bestDistance, ties := "", maxDistance+1, 0
	for _, candidate := range names {
		if candidate == name {
			continue
		}
		d := levenshtein(strings.ToLower(name), strings.ToLower(candidate))
		switch {
		case d < bestDistance:
			best, bestDistance, ties = candidate, d, 1
		case d == bestDistance:
			ties++
		}
	}
	if bestDistance > maxDistance || ties != 1 {
		return "", false
	}
	return best, true
}

// levenshtein is the standard edit distance, over runes so a multi-byte name
// costs one edit per character rather than per byte. Two rows, since only the
// distance is wanted.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}

	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// validateLogProcesses rejects a `prox logs` process filter that names a
// process the daemon does not have.
//
// Without this the name is only ever a ring-buffer FILTER (api/handlers.go
// parseLogParams): a typo matches no entry, so `prox logs wbe` printed nothing
// and exited 0 — silence that reads exactly like "this process has logged
// nothing". A name that DOES match keeps today's behavior byte for byte,
// including legitimately empty output; only a wrong NAME is an error now.
//
// It FAILS OPEN. If the daemon cannot be asked for its process list, the
// command proceeds exactly as it did before: `prox logs` against a merely slow
// daemon must not start failing over a check that is only there to improve a
// message.
//
// filter is the raw comma-separated value the request will carry (the
// --process flag, or the positional argument folded into it), and is split
// exactly as the daemon splits it so validation and filtering cannot disagree.
func validateLogProcesses(client *Client, filter string) error {
	if filter == "" {
		return nil
	}

	wanted := make([]string, 0, 1)
	for _, part := range strings.Split(filter, ",") {
		// An empty segment (a trailing comma) is harmless to the daemon's
		// filter and is not worth an error; a filter made ONLY of empty
		// segments names nothing and would silently match nothing, which is the
		// very failure this check exists to remove.
		if part != "" {
			wanted = append(wanted, part)
		}
	}
	if len(wanted) == 0 {
		return fmt.Errorf("--process %q names no process", filter)
	}

	names, ok := knownProcessNames(client)
	if !ok {
		return nil
	}

	known := make(map[string]struct{}, len(names))
	for _, n := range names {
		known[n] = struct{}{}
	}
	for _, w := range wanted {
		if _, found := known[w]; !found {
			return unknownProcessError(w, names)
		}
	}
	return nil
}
