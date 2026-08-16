package certs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// trustProbeTimeout bounds the probe. mkcert issuing one certificate is fast
// (well under a second); the budget exists so a wedged binary cannot stall a
// registration or a startup, not because the work is slow.
const trustProbeTimeout = 5 * time.Second

// probeWaitDelay is how long a killed probe gets to release its pipes before
// exec abandons them. It is NOT decoration: the probe captures into a buffer,
// so exec.Cmd.Wait waits on the copy goroutines, and any grandchild the killed
// mkcert left behind still holds the write end — without this, a cancelled
// probe blocks until that grandchild exits, defeating the whole timeout. (A
// test with a stalling fake mkcert caught exactly that: cancelling at 200ms
// still took 60s to return.)
const probeWaitDelay = 500 * time.Millisecond

// mkcertGenerateTimeout bounds a real certificate generation. Generation runs
// inside the daemon's register() under lifecycleMu, so an mkcert that hangs
// would take every other project's registration down with it. Far more
// generous than trustProbeTimeout because this one does real work a user is
// waiting on, rather than answering a diagnostic question.
const mkcertGenerateTimeout = 60 * time.Second

// probeHostname is the name the probe asks mkcert to certify. It is under the
// reserved .invalid TLD (RFC 2606), so the throwaway certificate can never be
// valid for anything real, and the name cannot resolve.
const probeHostname = "prox-trust-probe.invalid"

// caRootFile is the file whose existence means mkcert already HAS a local CA.
const caRootFile = "rootCA.pem"

// caUntrustedWarning is the ONE place a captured mkcert line becomes a
// domain.Warning. Both producers — the shared daemon (internal/proxyd/certs.go)
// and standalone mode (internal/cli/up.go) — reach it through Resolve, so the
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

// TrustResolver answers "is mkcert's local CA installed in the trust stores?"
// ONCE PER PROCESS.
//
// Process scope, not per-domain scope, because that is the scope of the fact:
// the CA is a property of the machine and the user's trust stores, not of any
// base domain prox happens to serve.
//
// Once, because of how the answer is obtained. mkcert only speaks when it
// generates something, and generation only happens when the cert FILES are
// absent (see EnsureCerts). A daemon restarted onto a new release — which
// RELEASING.md requires after every release — starts with warm certs on disk
// and would never run mkcert again, so without a deliberate probe the whole
// feature would do nothing for most users, most of the time. The probe closes
// that hole; running it at most once keeps it off the per-registration path.
//
// The zero value is a usable resolver that has not asked anything yet.
type TrustResolver struct {
	mu      sync.Mutex
	known   bool
	warning *domain.Warning

	// probeMu serializes the probe itself. It is separate from mu so the state
	// lock is never held across an exec, and probed (guarded by it) is what
	// makes the probe at-most-once even if the first attempt fails.
	probeMu   sync.Mutex
	probed    bool
	lastProbe time.Time

	// now is time.Now, swappable in tests. Nothing else in this file reads the
	// clock, so a test can drive the re-probe interval without sleeping.
	now func() time.Time
}

// reProbeInterval is the floor between re-probes while the verdict is BAD.
//
// Re-asking is what lets a fixed machine stop being warned at (see Resolve),
// but the probe runs inside the daemon's register() under lifecycleMu, so an
// unthrottled one would make every registration on a broken machine pay for a
// subprocess while every other project waits (CodeRabbit, PR #110). A user who
// runs `mkcert -install` and re-runs `prox up` is not doing it twice within
// this window, so the floor costs them nothing.
const reProbeInterval = 30 * time.Second

// NewTrustResolver returns a resolver with no verdict yet. Production code uses
// SharedTrust; this exists so a test can have a resolver whose once-per-process
// latch is its own.
func NewTrustResolver() *TrustResolver { return &TrustResolver{now: time.Now} }

// clock reads the resolver's time source, defaulting to time.Now so a
// zero-value TrustResolver stays usable.
func (r *TrustResolver) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// sharedTrust is the process-wide resolver. Every production caller shares it,
// which is what lets a real generation in one component answer for a probe that
// another component would otherwise have to run.
var sharedTrust = NewTrustResolver()

// SharedTrust returns the process-wide resolver (see TrustResolver).
func SharedTrust() *TrustResolver { return sharedTrust }

// observe records the verdict implied by a real mkcert run's captured output.
// It is called for every genuine generation, so the free answer is always
// preferred to the probe.
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

// verdict returns the currently held answer.
func (r *TrustResolver) verdict() TrustVerdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	return TrustVerdict{Known: r.known, Warning: r.warning}
}

// Resolve returns this process's verdict, running the probe at most once if
// nothing has answered yet.
//
// It never fails and never blocks longer than trustProbeTimeout: every probe
// failure — no mkcert, no CA, a timeout, a non-zero exit — yields an unknown
// verdict, which records nothing and clears nothing. A diagnostic that cannot
// be produced must not become a startup error.
func (r *TrustResolver) Resolve(ctx context.Context) TrustVerdict {
	// A settled GOOD verdict is final for the life of the process: a CA that is
	// installed does not spontaneously stop being installed, so re-asking would
	// pay for a subprocess on every registration forever to learn nothing.
	if v := r.verdict(); v.Known && v.Warning == nil {
		return v
	}

	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if v := r.verdict(); v.Known && v.Warning == nil {
		return v
	}

	// A settled BAD verdict is re-asked, and that is what makes the warning's
	// own hint true. The hint tells the user to run `mkcert -install` and
	// restart prox — but in shared mode the DAEMON holds this verdict and
	// outlives their restart, so a latched "untrusted" would keep being
	// reported at a machine the user had already fixed. Re-probing while the
	// answer is bad costs one short subprocess per registration, and only ever
	// while something is actually wrong; once mkcert says the CA is installed,
	// the branch above makes it free again forever.
	//
	// The at-most-once latch therefore applies only to the UNKNOWN case, where
	// a probe that could not answer (no mkcert, no CA, a failure) must not be
	// retried on every registration for the life of the process.
	if !r.verdict().Known && r.probed {
		return TrustVerdict{}
	}
	// Re-asking a BAD verdict is throttled: see reProbeInterval. Until the
	// window elapses the held verdict stands, so the warning keeps being
	// reported -- it just is not re-derived on every single registration.
	if v := r.verdict(); v.Known && v.Warning != nil {
		if !r.lastProbe.IsZero() && r.clock().Sub(r.lastProbe) < reProbeInterval {
			return v
		}
	}
	r.probed = true
	r.lastProbe = r.clock()

	lines, ok := probeCATrust(ctx)
	if !ok {
		// Return whatever is currently held rather than a bare zero value: a
		// real generation may have recorded a verdict while this probe ran, and
		// discarding it would report "unknown" over a known answer.
		return r.verdict()
	}
	r.observe(lines)
	return r.verdict()
}

// probeCATrust asks mkcert about the CA without prox having to know anything
// about trust stores: it issues a throwaway certificate for an inert name into
// a temp directory and reads the note mkcert prints. The temp directory (and
// therefore the certificate and its key) is deleted before returning.
//
// It runs ONLY when a CA already exists. mkcert CREATES a local CA when it
// finds none, and prox generating a CA as the side effect of a diagnostic —
// installing nothing, explaining nothing — would be a far worse behaviour than
// the missing warning it was trying to produce. Checking for rootCA.pem is not
// a reimplementation of trust-store detection: it is only "has mkcert been run
// at all"; the trust verdict itself still comes from mkcert's own output.
//
// ok is false for every failure, and the caller ignores it entirely.
func probeCATrust(ctx context.Context) ([]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, trustProbeTimeout)
	defer cancel()

	if _, err := exec.LookPath("mkcert"); err != nil {
		return nil, false
	}
	caroot, ok := mkcertCAROOT(ctx)
	if !ok {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(caroot, caRootFile)); err != nil {
		return nil, false // no CA yet: never provoke mkcert into creating one
	}

	dir, err := os.MkdirTemp("", "prox-catrust-")
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "mkcert",
		"-cert-file", filepath.Join(dir, "probe.pem"),
		"-key-file", filepath.Join(dir, "probe-key.pem"),
		probeHostname,
	)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.WaitDelay = probeWaitDelay
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	return notableLines(buf.String()), true
}

// mkcertCAROOT reads the directory mkcert keeps its CA in. Asking mkcert beats
// reconstructing the path from CAROOT/XDG_DATA_HOME/%LOCALAPPDATA% rules that
// differ per platform — the same argument as the rest of this file.
func mkcertCAROOT(ctx context.Context) (string, bool) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "mkcert", "-CAROOT")
	cmd.Stdout = &out
	cmd.WaitDelay = probeWaitDelay
	if err := cmd.Run(); err != nil {
		return "", false
	}
	caroot := strings.TrimSpace(out.String())
	if caroot == "" {
		return "", false
	}
	return caroot, true
}
