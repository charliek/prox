package certs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	// Stdout and Stderr are what the fake prints when it generates a
	// certificate.
	Stdout string
	Stderr string
	// ExitCode is the fake's exit status for a generation.
	ExitCode int
}

// fakeMkcert is a shell script named `mkcert`, on a PATH that contains nothing
// else executable of that name. It is how these tests exercise the real
// capture/marker code without a real mkcert — and, critically, without ever
// reaching the developer's actual mkcert, whose CAROOT a real generation could
// create.
//
// calls is the per-invocation log: a test can assert not just what prox did
// with mkcert's output but how many times it asked for any.
type fakeMkcert struct {
	calls string
}

func newFakeMkcert(t *testing.T, opts fakeMkcertOpts) *fakeMkcert {
	t.Helper()

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stdout.txt"), []byte(opts.Stdout), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(opts.Stderr), 0o600))

	// Records every invocation and writes the cert and key files it was asked
	// for, so the caller's own bookkeeping proceeds.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
cert=""
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
cat %[2]q
cat %[3]q >&2
exit %[4]d
`, calls, filepath.Join(dir, "stdout.txt"), filepath.Join(dir, "stderr.txt"), opts.ExitCode)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "mkcert"), []byte(script), 0o755))
	// /bin and /usr/bin stay reachable for the script's own `cat`; the real
	// mkcert (typically /opt/homebrew/bin) does not.
	t.Setenv("PATH", dir+":/bin:/usr/bin")

	return &fakeMkcert{calls: calls}
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

			v := tr.Verdict()
			require.True(t, v.Known, "a real generation always answers the question")
			require.Len(t, fake.callArgs(t), 1, "one generation, and nothing else asks mkcert anything")
			if tc.wantWarning == "" {
				assert.Nil(t, v.Warning)
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

	assert.False(t, tr.Verdict().Known,
		"a failed generation proves nothing about the trust stores")
}

// TestEnsureCerts_WarmCertsSkipMkcert pins both halves of the short-circuit:
// with the cert files already on disk, mkcert never runs at all — no subprocess
// on the registration path — and therefore nothing is learned about the CA.
// That silence is the accepted blind spot, not an oversight.
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
	assert.False(t, tr.Verdict().Known, "and therefore nothing is known about the CA yet")
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

// TestTrustResolver_GenerationAnswers pins the only way this process ever
// learns anything: a real generation ran, mkcert spoke, and every later read of
// the verdict returns what it said without asking anyone again.
func TestTrustResolver_GenerationAnswers(t *testing.T) {
	fake := newFakeMkcert(t, fakeMkcertOpts{Stdout: mkcertCreatedNote + "\n" + mkcertSystemNote + "\n"})
	tr := NewTrustResolver()
	m := NewManagerWithTrust(t.TempDir(), "test.dev", tr)

	_, _, err := m.EnsureCerts()
	require.NoError(t, err)

	for range 3 {
		v := tr.Verdict()
		require.True(t, v.Known)
		require.NotNil(t, v.Warning)
		assert.Equal(t, mkcertSystemNote, v.Warning.Message)
		assert.Equal(t, domain.WarningCodeMkcertCAUntrusted, v.Warning.Code)
	}
	assert.Len(t, fake.callArgs(t), 1,
		"the generation is the whole cost: reading the verdict must never add another mkcert call")
}

// TestTrustResolver_TrustedVerdictWithdrawsWarning pins the anti-lying half:
// once mkcert says the CA IS installed, the resolver stops reporting a warning,
// so consumers can withdraw one they published earlier.
func TestTrustResolver_TrustedVerdictWithdrawsWarning(t *testing.T) {
	tr := NewTrustResolver()
	tr.observe([]string{mkcertCreatedNote, mkcertSystemNote})
	require.NotNil(t, tr.Verdict().Warning, "precondition: the CA was untrusted")

	// The user ran `mkcert -install`; the next mkcert run says nothing.
	tr.observe([]string{mkcertCreatedNote})

	v := tr.Verdict()
	assert.True(t, v.Known)
	assert.Nil(t, v.Warning, "a fixed problem must stop being reported")
}

// TestSharedTrust_IsProcessWide pins that production callers share one verdict —
// the property that lets one component's real generation answer for every other
// component, which generates nothing and would otherwise know nothing.
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
