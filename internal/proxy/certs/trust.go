package certs

import (
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// caUntrustedMarker is the case-insensitive STEM shared by both of the strings
// mkcert prints when it issues a certificate while its own local CA is missing
// from the trust stores:
//
//	Note: the local CA is not installed in the system trust store.
//	Note: the local CA is not installed in the %s trust store.
//
// Matching on the stem rather than on either full string is deliberate: the
// second is a format string whose verb is filled with "Firefox and/or
// Chrome/Chromium", so no single literal covers both, and the trailing wording
// is exactly the part mkcert is free to reword.
//
// This is the WHOLE of prox's trust detection, and that is the point. Whether a
// CA is installed is per-OS, per-OS-release, and per-browser; mkcert already
// implements it correctly, and a second implementation living here would be
// wrong in ways nobody notices. prox therefore never inspects a trust store —
// it reads mkcert's own verdict, and carries mkcert's own sentence to the user.
const caUntrustedMarker = "the local ca is not installed in"

// mkcertInstallHint is the next action for an untrusted CA. It is a constant
// (not prose repeated at each producer) because the daemon and the standalone
// CLI both emit this warning, and two copies is how two wordings happen.
const mkcertInstallHint = "run 'mkcert -install' and restart prox"

// mkcertWaitDelay is how long a killed mkcert gets to release its pipes before
// exec abandons them. It is NOT decoration: generation captures into a buffer,
// so exec.Cmd.Wait waits on the copy goroutines, and any grandchild the killed
// mkcert left behind still holds the write end — without this, a generation cut
// short by mkcertGenerateTimeout blocks until that grandchild exits, defeating
// the whole timeout. (A test with a stalling fake mkcert caught exactly that:
// cancelling at 200ms still took 60s to return.)
const mkcertWaitDelay = 500 * time.Millisecond

// mkcertGenerateTimeout bounds a real certificate generation. Generation runs
// inside the daemon's register() under lifecycleMu, so an mkcert that hangs
// would take every other project's registration down with it. The bound is
// deliberately generous: issuing one certificate is sub-second, and this exists
// to break a hang, not to race a slow machine.
const mkcertGenerateTimeout = 60 * time.Second

// caUntrustedWarning is the ONE place a captured mkcert line becomes a
// domain.Warning. Both producers — the shared daemon (internal/proxyd/certs.go)
// and standalone mode (internal/cli/up.go) — reach it through Verdict, so the
// code and the hint cannot drift between the two paths that show a user the
// same problem.
//
// The message is mkcert's own line VERBATIM. prox does not paraphrase it: the
// sentence a user then searches for, or pastes into an issue, is the one the
// tool that detected the problem actually wrote.
func caUntrustedWarning(line string) domain.Warning {
	return domain.Warning{
		Code:    domain.WarningCodeMkcertCAUntrusted,
		Message: line,
		Hint:    mkcertInstallHint,
	}
}

// notableLines splits captured mkcert output into non-empty, trimmed lines.
// Callers hand these to a user or to an error, so trailing blank lines and the
// final newline are noise.
func notableLines(out string) []string {
	var lines []string
	for _, raw := range strings.Split(out, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// javaStoreMarker identifies mkcert's note about the JAVA trust store, which
// this warning deliberately IGNORES.
//
// mkcert checks the system store, the NSS (Firefox/Chrome) stores and the Java
// store independently, and prints a separate note for each one that is missing.
// The Java store has nothing to do with #97: a machine where `mkcert -install`
// ran and a JDK was installed afterwards prints ONLY the Java note, and warning
// there would tell a user their HTTPS is broken when every browser on the
// machine works fine. That is a false alarm, which is the same failure as the
// missing warning this feature exists to fix, pointed the other way.
const javaStoreMarker = "java trust store"

// findCAUntrustedLine returns the first line reporting a trust store that
// actually affects browsing. mkcert can print the note once per store it
// checked; the user has one fix, so the first relevant line answers for all of
// them, and Java-only output is not an answer at all.
func findCAUntrustedLine(lines []string) (string, bool) {
	for _, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, caUntrustedMarker) {
			continue
		}
		if strings.Contains(lower, javaStoreMarker) {
			continue
		}
		return line, true
	}
	return "", false
}

// TrustVerdict is what this process knows about mkcert's local CA.
//
// Known separates "we asked and the CA IS trusted" from "we could not find
// out", which is the distinction that makes the warning withdrawable: only the
// first justifies CLEARING a previously reported warning. Treating unknown as
// trusted would silently retract a true warning; treating it as untrusted would
// invent a false one.
type TrustVerdict struct {
	// Known is true once mkcert has actually told us, one way or the other.
	Known bool
	// Warning is non-nil exactly when the CA is NOT installed in the trust
	// stores. It is built by caUntrustedWarning, so its Code and Hint are the
	// same whichever producer surfaces it.
	Warning *domain.Warning
}

// TrustResolver holds what a real mkcert generation said about mkcert's own
// local CA, for the life of the process.
//
// Process scope, not per-domain scope, because that is the scope of the fact:
// the CA is a property of the machine and the user's trust stores, not of any
// base domain prox happens to serve.
//
// It is a PURE holder: nothing here execs, reads the filesystem, reads a clock,
// or takes a context. mkcert only speaks when it generates something (see
// EnsureCerts), and that free signal is the only input. Plan 028 also ran a
// trust probe — generating a throwaway certificate purely to read mkcert's note
// when the certs were already warm — and plan 029 removed it: measured at
// ~217ms and 2 subprocesses inside the daemon's register() under lifecycleMu,
// where every other project's registration waits on it, and re-run every 30s
// while the verdict stayed bad. #97's headline case (mkcert installed,
// `mkcert -install` never run) has no certs yet, so its very next generation
// answers for free.
//
// Accepted blind spot, deliberately not papered over: a machine with warm certs
// whose CA goes bad LATER (OS reinstall, keychain reset) learns nothing until
// something triggers a real generation. Covering that belongs in a background
// check or `prox doctor` — never in front of a user waiting for processes to
// start.
//
// The zero value is a usable resolver that knows nothing yet.
type TrustResolver struct {
	mu      sync.Mutex
	known   bool
	warning *domain.Warning
}

// NewTrustResolver returns a resolver with no verdict yet. Production code uses
// SharedTrust; this exists so a test can have a resolver whose verdict is its
// own rather than the process-wide one.
func NewTrustResolver() *TrustResolver { return &TrustResolver{} }

// sharedTrust is the process-wide resolver. Every production caller shares it,
// which is what lets a real generation in one component answer for every other
// component that never generates anything.
var sharedTrust = NewTrustResolver()

// SharedTrust returns the process-wide resolver (see TrustResolver).
func SharedTrust() *TrustResolver { return sharedTrust }

// observe records the verdict implied by a real mkcert run's captured output.
// It is called for every genuine generation, and it is the ONLY way this
// resolver ever learns anything.
func (r *TrustResolver) observe(lines []string) {
	line, untrusted := findCAUntrustedLine(lines)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known = true
	if !untrusted {
		r.warning = nil
		return
	}
	w := caUntrustedWarning(line)
	r.warning = &w
}

// Verdict returns what this process knows, and asks nobody: it is a mutex read
// of what a real generation already reported, cheap enough for the daemon to
// call on every registration. It cannot fail, so a missing diagnostic never
// becomes a startup error — an unknown verdict records nothing and clears
// nothing.
//
// The verdict dies with the process, which is what keeps the warning's own hint
// ("run 'mkcert -install' and restart prox") true: a restarted daemon starts
// Unknown and stays silent until the next real generation says otherwise.
func (r *TrustResolver) Verdict() TrustVerdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	return TrustVerdict{Known: r.known, Warning: r.warning}
}
