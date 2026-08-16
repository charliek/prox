package certs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two strings mkcert actually prints when its CA is not in the trust
// stores, read out of the installed binary. The second is a format string whose
// verb mkcert fills with a browser list, which is why the marker matches only
// their shared stem.
const (
	mkcertSystemNote  = "Note: the local CA is not installed in the system trust store! ⚠️"
	mkcertBrowserNote = "Note: the local CA is not installed in the Firefox and/or Chrome/Chromium trust store! ⚠️"
	mkcertCreatedNote = "Created a new certificate valid for the following names 📜"
)

// fakeMkcertOpts configures the stand-in mkcert.
type fakeMkcertOpts struct {
	// Stdout and Stderr are what the fake prints when asked to GENERATE a
	// certificate (the -CAROOT query always answers with the CAROOT path).
	Stdout string
	Stderr string
	// ExitCode is the fake's exit status for a generation.
	ExitCode int
	// NoCA leaves CAROOT without a rootCA.pem, i.e. mkcert has never run on
	// this machine — the state in which prox must never provoke a probe.
	NoCA bool
	// SleepSeconds stalls a generation, for proving the probe is bounded.
	SleepSeconds int
}

// fakeMkcert is a shell script named `mkcert`, on a PATH that contains nothing
// else executable of that name. It is how these tests exercise the real
// capture/marker/probe code without a real mkcert — and, critically, without
// ever touching the developer's actual CAROOT, which a probe against the real
// binary would read and a generation could create.
type fakeMkcert struct {
	dir    string
	caroot string
	calls  string
}

// setStdout rewrites what the fake prints, so a test can model the user going
// away and running `mkcert -install` between two resolves.
func (f *fakeMkcert) setStdout(t *testing.T, out string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "stdout.txt"), []byte(out), 0o600))
}

func newFakeMkcert(t *testing.T, opts fakeMkcertOpts) *fakeMkcert {
	t.Helper()

	dir := t.TempDir()
	caroot := filepath.Join(dir, "caroot")
	require.NoError(t, os.MkdirAll(caroot, 0o755))
	if !opts.NoCA {
		require.NoError(t, os.WriteFile(filepath.Join(caroot, caRootFile), []byte("fake root CA"), 0o600))
	}

	calls := filepath.Join(dir, "calls.log")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stdout.txt"), []byte(opts.Stdout), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(opts.Stderr), 0o600))

	sleep := ""
	if opts.SleepSeconds > 0 {
		// exec, so the sleep REPLACES the shell rather than becoming its child:
		// killing the process on timeout then kills the sleep too. Without this
		// the shell dies and the orphaned sleep lingers for its full duration —
		// and this repo is deliberately intolerant of stranded fixture
		// processes (CodeRabbit review).
		sleep = fmt.Sprintf("exec sleep %d\n", opts.SleepSeconds)
	}

	// Records every invocation, answers -CAROOT, and otherwise writes the cert
	// and key files it was asked for so the caller's own bookkeeping proceeds.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
if [ "$1" = "-CAROOT" ]; then
  printf '%%s\n' %[2]q
  exit 0
fi
%[3]scert=""
key=""
while [ $# -gt 0 ]; do
  case "$1" in
    -cert-file) cert="$2"; shift 2 ;;
    -key-file) key="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$cert" ]; then printf 'fake-cert\n' > "$cert"; fi
if [ -n "$key" ]; then printf 'fake-key\n' > "$key"; fi
cat %[4]q
cat %[5]q >&2
exit %[6]d
`, calls, caroot, sleep, filepath.Join(dir, "stdout.txt"), filepath.Join(dir, "stderr.txt"), opts.ExitCode)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "mkcert"), []byte(script), 0o755))
	// /bin and /usr/bin stay reachable for the script's own `cat`/`sleep`; the
	// real mkcert (typically /opt/homebrew/bin) does not.
	t.Setenv("PATH", dir+":/bin:/usr/bin")

	return &fakeMkcert{dir: dir, caroot: caroot, calls: calls}
}

// callArgs returns the argument line of each invocation, in order.
func (f *fakeMkcert) callArgs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.calls)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// generationCalls returns only the invocations that generated a certificate.
func (f *fakeMkcert) generationCalls(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range f.callArgs(t) {
		if !strings.Contains(c, "-CAROOT") {
			out = append(out, c)
		}
	}
	return out
}

// TestEnsureCerts_CapturesMkcertOutput is the headline test: mkcert's own
// advice must survive generation. Before this commit the output went to
// os.Stdout/os.Stderr, which in the shared daemon are /dev/null — so the note
// telling a user why every HTTPS request fails was discarded, and prox reported
// perfect health (#97).
func TestEnsureCerts_CapturesMkcertOutput(t *testing.T) {
	tests := []struct {
		name        string
		stdout      string
		stderr      string
		wantWarning string // "" means the CA is trusted
	}{
		{
			name:        "marker on stdout",
			stdout:      mkcertCreatedNote + "\n" + mkcertSystemNote + "\n",
			wantWarning: mkcertSystemNote,
		},
		{
			name:        "marker on stderr",
			stdout:      mkcertCreatedNote + "\n",
			stderr:      mkcertSystemNote + "\n",
			wantWarning: mkcertSystemNote,
		},
		{
			name:   "marker absent",
			stdout: mkcertCreatedNote + "\n",
		},
		{
			// mkcert prints one note per trust store it checked. The user has
			// one problem and one fix, so the first line answers for all.
			name:        "multiple matching lines",
			stdout:      mkcertCreatedNote + "\n" + mkcertSystemNote + "\n" + mkcertBrowserNote + "\n",
			wantWarning: mkcertSystemNote,
		},
		{
			// The marker is matched case-insensitively, so a reworded prefix
			// does not silently disable the whole feature.
			name:        "marker in different case",
			stdout:      "NOTE: The Local CA Is Not Installed In the system trust store!\n",
			wantWarning: "NOTE: The Local CA Is Not Installed In the system trust store!",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeMkcert(t, fakeMkcertOpts{Stdout: tc.stdout, Stderr: tc.stderr})
			tr := NewTrustResolver()
			m := NewManagerWithTrust(t.TempDir(), "test.dev", tr)

			paths, lines, err := m.EnsureCerts()
			require.NoError(t, err)
			require.NotNil(t, paths, "the paths must survive the signature change — both callers need them")
			assert.FileExists(t, paths.CertFile)
			assert.FileExists(t, paths.KeyFile)

			// Every non-empty line mkcert wrote, from BOTH streams, is captured.
			for _, want := range notableLines(tc.stdout + tc.stderr) {
				assert.Contains(t, lines, want)
			}

			v := tr.Resolve(context.Background())
			require.True(t, v.Known, "a real generation always answers the question")
			if tc.wantWarning == "" {
				assert.Nil(t, v.Warning)
				assert.Empty(t, fake.generationCalls(t)[1:], "a generation answered, so nothing should probe")
				return
			}
			require.NotNil(t, v.Warning)
			assert.Equal(t, tc.wantWarning, v.Warning.Message, "the user gets mkcert's own sentence, verbatim")
		})
	}
}

// TestEnsureCerts_FailureIncludesOutput pins the strict improvement on the
// error path: whatever mkcert said about WHY it failed used to go to the
// daemon's /dev/null, leaving the user with a bare exit status.
func TestEnsureCerts_FailureIncludesOutput(t *testing.T) {
	newFakeMkcert(t, fakeMkcertOpts{
		Stderr:   "failed to load the CA key: open /nope/rootCA-key.pem: permission denied\n",
		ExitCode: 1,
	})
	tr := NewTrustResolver()
	m := NewManagerWithTrust(t.TempDir(), "test.dev", tr)

	paths, lines, err := m.EnsureCerts()
	require.Error(t, err)
	assert.Nil(t, paths)
	assert.Contains(t, err.Error(), "failed to load the CA key")
	assert.Contains(t, err.Error(), "permission denied")
	assert.Contains(t, lines, "failed to load the CA key: open /nope/rootCA-key.pem: permission denied")

	assert.False(t, tr.verdict().Known,
		"a failed generation proves nothing about the trust stores")
}

// TestEnsureCerts_WarmCertsSkipMkcert pins the case that motivates the probe:
// with the cert files already on disk, mkcert never runs at all, so nothing can
// be learned from its output. This is the NORMAL state of a daemon restarted
// onto a new release, not an edge case.
func TestEnsureCerts_WarmCertsSkipMkcert(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{Stdout: mkcertSystemNote + "\n"})
	tr := NewTrustResolver()
	dir := t.TempDir()
	m := NewManagerWithTrust(dir, "test.dev", tr)

	paths := m.getCertPaths()
	require.NoError(t, os.WriteFile(paths.CertFile, []byte("warm cert"), 0o600))
	require.NoError(t, os.WriteFile(paths.KeyFile, []byte("warm key"), 0o600))

	got, lines, err := m.EnsureCerts()
	require.NoError(t, err)
	assert.Equal(t, paths, got)
	assert.Empty(t, lines, "nothing was generated, so there is no output to report")
	assert.Empty(t, fake.callArgs(t), "mkcert must not run when the certs are already on disk")
	assert.False(t, tr.verdict().Known, "and therefore nothing is known about the CA yet")
}

// TestCAUntrustedWarning_PinnedShape pins the ONE constructor both producers go
// through. Code and Hint live here rather than at each producer precisely so
// the daemon and standalone paths cannot describe the same problem differently.
func TestCAUntrustedWarning_PinnedShape(t *testing.T) {
	w := caUntrustedWarning(mkcertSystemNote)
	assert.Equal(t, domain.WarningCodeMkcertCAUntrusted, w.Code)
	assert.Equal(t, mkcertSystemNote, w.Message, "mkcert's line, verbatim")
	assert.Equal(t, "run 'mkcert -install' and restart prox", w.Hint)
}

// TestTrustResolver_ProbesWhenCertsAreWarm is the test for the step that makes
// the feature work at all: with nothing generated, the resolver asks mkcert
// directly rather than reporting nothing.
func TestTrustResolver_ProbesWhenCertsAreWarm(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{Stdout: mkcertCreatedNote + "\n" + mkcertSystemNote + "\n"})
	tr := NewTrustResolver()

	v := tr.Resolve(context.Background())
	require.True(t, v.Known)
	require.NotNil(t, v.Warning)
	assert.Equal(t, mkcertSystemNote, v.Warning.Message)
	assert.Equal(t, domain.WarningCodeMkcertCAUntrusted, v.Warning.Code)

	gen := fake.generationCalls(t)
	require.Len(t, gen, 1, "exactly one throwaway generation")
	assert.Contains(t, gen[0], probeHostname, "the probe certifies an inert .invalid name, never a real one")

	// The throwaway cert, its key, and their directory are gone.
	for _, field := range []string{"-cert-file", "-key-file"} {
		path := argAfter(t, gen[0], field)
		assert.NoFileExists(t, path, "the probe must not leave %s behind", field)
		assert.NoDirExists(t, filepath.Dir(path), "the probe must delete its temp dir")
	}
}

// TestTrustResolver_SkipsProbeWithoutRootCA is a safety property, not an
// optimisation: mkcert CREATES a local CA when it finds none, and prox must
// never create one as the side effect of a diagnostic — installing nothing and
// explaining nothing.
func TestTrustResolver_SkipsProbeWithoutRootCA(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{NoCA: true, Stdout: mkcertSystemNote + "\n"})
	tr := NewTrustResolver()

	v := tr.Resolve(context.Background())
	assert.False(t, v.Known, "no CA means no verdict — not a guess in either direction")
	assert.Nil(t, v.Warning)
	assert.Empty(t, fake.generationCalls(t), "mkcert must never be asked to generate, which would create a CA")
	assert.NoFileExists(t, filepath.Join(fake.caroot, caRootFile))
}

// TestTrustResolver_GoodGenerationAnswersWithoutProbing pins the free path: a
// real generation said the CA IS installed, which is final for the process, so
// no probe ever runs.
func TestTrustResolver_GoodGenerationAnswersWithoutProbing(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{Stdout: mkcertCreatedNote + "\n"})
	tr := NewTrustResolver()
	m := NewManagerWithTrust(t.TempDir(), "test.dev", tr)

	_, _, err := m.EnsureCerts()
	require.NoError(t, err)

	for range 3 {
		v := tr.Resolve(context.Background())
		require.True(t, v.Known)
		require.Nil(t, v.Warning)
	}

	gen := fake.generationCalls(t)
	require.Len(t, gen, 1, "a settled good verdict must never pay for a probe again")
	assert.NotContains(t, gen[0], probeHostname)
}

// TestTrustResolver_BadVerdictIsReAsked is the anti-lying test, and it is the
// reason the latch is not simply "once per process".
//
// The warning's own hint tells the user to run `mkcert -install` and restart
// prox. In shared mode the DAEMON holds this verdict and outlives that restart,
// so a latched "untrusted" would keep being reported at a machine the user had
// already fixed — prox stating something untrue, which is the same bug as
// stating nothing at all, pointed the other way. So while the answer is bad it
// is re-asked, and the moment mkcert says the CA is installed the warning is
// withdrawn and the probing stops for good.
func TestTrustResolver_BadVerdictIsReAsked(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{
		Stdout: mkcertCreatedNote + "\n" + mkcertSystemNote + "\n",
	})
	tr := NewTrustResolver()

	v := tr.Resolve(context.Background())
	require.NotNil(t, v.Warning, "precondition: the CA is reported untrusted")

	// Still broken: prox keeps asking rather than trusting a stale verdict.
	v = tr.Resolve(context.Background())
	require.NotNil(t, v.Warning)
	require.GreaterOrEqual(t, len(fake.generationCalls(t)), 2,
		"a bad verdict must be re-asked, or the user can never be told they fixed it")

	// The user runs `mkcert -install`: mkcert stops printing the note.
	fake.setStdout(t, mkcertCreatedNote+"\n")

	v = tr.Resolve(context.Background())
	require.True(t, v.Known)
	assert.Nil(t, v.Warning, "the warning must be withdrawn once mkcert says the CA is installed")

	// ...and having settled good, it stops probing.
	before := len(fake.generationCalls(t))
	require.Nil(t, tr.Resolve(context.Background()).Warning)
	assert.Equal(t, before, len(fake.generationCalls(t)))
}

// TestTrustResolver_ProbesAtMostOncePerProcess pins the latch. The probe is a
// subprocess; running it per registration would put mkcert on the hot path of
// every project joining the daemon.
func TestTrustResolver_ProbesAtMostOncePerProcess(t *testing.T) {
	// Exit 1: the probe FAILS, which is the case that most easily degenerates
	// into retrying forever.
	fake := newFakeMkcert(t, fakeMkcertOpts{ExitCode: 1})
	tr := NewTrustResolver()

	for range 5 {
		v := tr.Resolve(context.Background())
		assert.False(t, v.Known, "a failed probe yields no verdict")
		assert.Nil(t, v.Warning)
	}
	assert.Len(t, fake.generationCalls(t), 1, "one attempt, however often it is asked")
}

// TestTrustResolver_ProbeBoundedByContext proves a wedged mkcert cannot stall
// startup: the probe is dropped when its context expires, and dropped means
// unknown — no warning, no error.
func TestTrustResolver_ProbeBoundedByContext(t *testing.T) {
	newFakeMkcert(t, fakeMkcertOpts{SleepSeconds: 30, Stdout: mkcertSystemNote + "\n"})
	tr := NewTrustResolver()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	v := tr.Resolve(ctx)
	elapsed := time.Since(start)

	assert.False(t, v.Known)
	assert.Nil(t, v.Warning)
	// Generous but far below both the fake's 60s stall and the resolver's own
	// 5s cap: the point is that cancellation actually RETURNS. Without
	// probeWaitDelay this took the full 60s, because the stalled child still
	// held the capture pipe after mkcert itself was killed.
	assert.Less(t, elapsed, 2*time.Second, "the probe must not outlive its context")
}

// TestTrustResolver_NoMkcertIsSilent covers the user who has no mkcert at all:
// prox already reports that separately, and a probe has nothing to say.
func TestTrustResolver_NoMkcertIsSilent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tr := NewTrustResolver()

	v := tr.Resolve(context.Background())
	assert.False(t, v.Known)
	assert.Nil(t, v.Warning)
}

// TestTrustResolver_TrustedVerdictWithdrawsWarning pins the anti-lying half:
// once mkcert says the CA IS installed, the resolver stops reporting a warning,
// so consumers can withdraw one they published earlier.
func TestTrustResolver_TrustedVerdictWithdrawsWarning(t *testing.T) {
	tr := NewTrustResolver()
	tr.observe([]string{mkcertCreatedNote, mkcertSystemNote})
	require.NotNil(t, tr.verdict().Warning, "precondition: the CA was untrusted")

	// The user ran `mkcert -install`; the next mkcert run says nothing.
	tr.observe([]string{mkcertCreatedNote})

	v := tr.verdict()
	assert.True(t, v.Known)
	assert.Nil(t, v.Warning, "a fixed problem must stop being reported")
}

// TestSharedTrust_IsProcessWide pins that production callers share one verdict —
// the property that lets one component's real generation spare every other
// component the probe.
func TestSharedTrust_IsProcessWide(t *testing.T) {
	assert.Same(t, SharedTrust(), SharedTrust())
	m := NewManager(t.TempDir(), "test.dev")
	assert.Same(t, SharedTrust(), m.trust, "NewManager reports to the process-wide resolver")
	assert.Same(t, SharedTrust(), NewManagerWithTrust(t.TempDir(), "test.dev", nil).trust,
		"a nil resolver falls back to the shared one rather than panicking")
}

// TestNotableLines pins the line-splitting the captured output goes through.
func TestNotableLines(t *testing.T) {
	assert.Nil(t, notableLines(""))
	assert.Nil(t, notableLines("\n\n  \n"))
	assert.Equal(t, []string{"one", "two"}, notableLines("one\n\n  two  \n\n"))
}

// argAfter returns the token following flag in a recorded invocation line.
func argAfter(t *testing.T, call, flag string) string {
	t.Helper()
	fields := strings.Fields(call)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	t.Fatalf("no %s in %q", flag, call)
	return ""
}
