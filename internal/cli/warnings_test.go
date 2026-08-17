package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy/certs"
)

// This file covers the session warning sink and its renderer (plan 028 A2): the
// collection every producer writes into, the two-line shape the user reads, and
// the completion latch the `prox up -d` parent polls.

func testWarning(code, message, hint string) domain.Warning {
	return domain.Warning{Code: code, Message: message, Hint: hint}
}

// TestWarningSink_AddDedupesAndReturnsOnlyNewOnes: the same advisory reported
// twice (the shared daemon returns it on the first register AND on every
// self-heal re-register) is ONE warning, and the second Add reports nothing new
// — which is what keeps the heal path from logging it once per heal.
func TestWarningSink_AddDedupesAndReturnsOnlyNewOnes(t *testing.T) {
	s := newWarningSink()

	added := s.Add(testWarning("a", "first", "hint a"), testWarning("b", "second", ""))
	require.Len(t, added, 2)
	assert.Equal(t, "first", added[0].Message)
	assert.Equal(t, "second", added[1].Message)

	// Same Code+Message, different hint: still a duplicate (hint is not part of
	// the identity — see domain.DedupeWarnings), and the FIRST copy is kept.
	assert.Empty(t, s.Add(testWarning("a", "first", "a different hint")))

	added = s.Add(testWarning("c", "third", ""))
	require.Len(t, added, 1)
	assert.Equal(t, "third", added[0].Message)

	got := s.Warnings()
	require.Len(t, got, 3)
	assert.Equal(t, []string{"first", "second", "third"}, []string{got[0].Message, got[1].Message, got[2].Message})
	assert.Equal(t, "hint a", got[0].Hint, "the first-seen copy, hint and all, is the one kept")
}

// TestWarningSink_EmptyAndNilAreNoOps: a nil sink is a usable sink, so no
// producer has to nil-check before every Add, and an empty collection reports
// nil (not []) so the omitempty JSON field disappears.
func TestWarningSink_EmptyAndNilAreNoOps(t *testing.T) {
	var s *warningSink
	assert.NotPanics(t, func() {
		assert.Nil(t, s.Add(testWarning("a", "m", "")))
		assert.Nil(t, s.Warnings())
		assert.False(t, s.WarningsSealed())
		s.Seal()
		s.Go(func() []domain.Warning { return nil })
		assert.True(t, s.Wait(time.Second))
	})

	live := newWarningSink()
	assert.Nil(t, live.Warnings(), "an empty sink reports nil, not an empty slice")
	assert.Nil(t, live.Add(), "adding nothing adds nothing")
}

// TestWarningSink_WarningsReturnsACopy: the reader is an HTTP goroutine
// encoding a response while startup may still be adding, so it must never be
// handed the sink's own backing array.
func TestWarningSink_WarningsReturnsACopy(t *testing.T) {
	s := newWarningSink()
	s.Add(testWarning("a", "original", ""))

	got := s.Warnings()
	got[0].Message = "mutated by the caller"

	assert.Equal(t, "original", s.Warnings()[0].Message)
}

// TestWarningSink_SealLatchesWithoutFreezing: Seal says "startup producers are
// done" (which is all warnings_sealed claims), NOT "nothing more can be said" —
// a forwarder self-heal re-register still records its advisories afterwards.
func TestWarningSink_SealLatchesWithoutFreezing(t *testing.T) {
	s := newWarningSink()
	assert.False(t, s.WarningsSealed(), "a fresh sink is unsealed")

	s.Seal()
	assert.True(t, s.WarningsSealed())
	s.Seal()
	assert.True(t, s.WarningsSealed(), "sealing twice is idempotent")

	require.Len(t, s.Add(testWarning("late", "raised by a heal", "")), 1)
	assert.Len(t, s.Warnings(), 1, "a post-seal warning is still recorded and still served")
	assert.True(t, s.WarningsSealed())
}

// TestWarningSink_ConcurrentReadWhileWriting is the reason the sink has a mutex
// at all: GET /status is served from HTTP goroutines while startup (and later
// the forwarder's heal loop) is still adding. Run under -race.
func TestWarningSink_ConcurrentReadWhileWriting(t *testing.T) {
	s := newWarningSink()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Warnings()
					_ = s.WarningsSealed()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Add(testWarning("code", "message "+string(rune('a'+i%26)), "hint"))
		}
		s.Seal()
		close(stop)
	}()

	wg.Wait()
	assert.Len(t, s.Warnings(), 26, "dedupe held across the concurrent writes")
	assert.True(t, s.WarningsSealed())
}

// TestWarningSink_WaitJoinsProducers: Wait is the seal-time join for
// asynchronous producers — the whole reason a warning can land after the API
// server is already serving.
func TestWarningSink_WaitJoinsProducers(t *testing.T) {
	s := newWarningSink()
	release := make(chan struct{})
	s.Go(func() []domain.Warning {
		<-release
		return []domain.Warning{testWarning("slow", "produced late", "")}
	})

	assert.False(t, s.Wait(20*time.Millisecond), "a producer still running must not report finished")
	assert.Nil(t, s.Warnings(), "and its warning has not landed yet")

	close(release)
	assert.True(t, s.Wait(2*time.Second))
	require.Len(t, s.Warnings(), 1)
	assert.Equal(t, "produced late", s.Warnings()[0].Message)
}

// TestWarningSink_WaitTimeoutStillCollectsLater: blowing the join budget must
// not lose the warning — startup seals without it, and it is served the moment
// it lands.
func TestWarningSink_WaitTimeoutStillCollectsLater(t *testing.T) {
	s := newWarningSink()
	started := make(chan struct{})
	s.Go(func() []domain.Warning {
		close(started)
		time.Sleep(30 * time.Millisecond)
		return []domain.Warning{testWarning("slow", "late", "")}
	})
	<-started

	assert.False(t, s.Wait(time.Millisecond))
	s.Seal()

	require.True(t, s.Wait(2*time.Second))
	assert.Len(t, s.Warnings(), 1, "a warning that missed the seal is still recorded")
}

// TestFormatWarning_ShapeAndHintIndent pins the exact two-line block, hint
// lined up under the message, and the single line when there is no hint.
func TestFormatWarning_ShapeAndHintIndent(t *testing.T) {
	lines := formatWarning(testWarning("c", "mkcert's local CA is not installed.", "Run 'mkcert -install'."))
	require.Len(t, lines, 2)
	assert.Equal(t, "Warning: mkcert's local CA is not installed.", lines[0])
	assert.Equal(t, "         Run 'mkcert -install'.", lines[1])
	assert.Equal(t, len("Warning: "), strings.Index(lines[1], "Run"),
		"the hint lines up under the message, not under the label")

	lines = formatWarning(testWarning("c", "no hint here", ""))
	require.Len(t, lines, 1, "the hint line is omitted entirely when there is no hint")
	assert.Equal(t, "Warning: no hint here", lines[0])
}

// TestWriteWarnings_WritesEveryWarningInOrder covers the shared renderer used by
// `prox status` and the `prox up -d` parent.
func TestWriteWarnings_WritesEveryWarningInOrder(t *testing.T) {
	var buf bytes.Buffer
	writeWarnings(&buf, []domain.Warning{
		testWarning("a", "first", "do this"),
		testWarning("b", "second", ""),
	})
	assert.Equal(t, "Warning: first\n         do this\nWarning: second\n", buf.String())

	buf.Reset()
	writeWarnings(&buf, nil)
	assert.Empty(t, buf.String(), "no warnings, no output")
}

// TestReportStartupWarnings_PlainModeReachesStderr: plain `prox up` has no
// preamble, so stderr is the only channel — and it must carry the full block.
func TestReportStartupWarnings_PlainModeReachesStderr(t *testing.T) {
	pre := newStartupPreamble(false)
	var stderr bytes.Buffer

	reportStartupWarnings([]domain.Warning{testWarning("c", "something is off", "fix it")}, pre, &stderr)

	assert.Equal(t, "Warning: something is off\n         fix it\n", stderr.String())
	assert.Empty(t, pre.Lines(), "a disabled preamble records nothing")
}

// TestReportStartupWarnings_TUIAlsoRecordsInThePreamble: under a TUI, stderr is
// about to be hidden behind the alt screen for the whole session, so the same
// lines have to go into the preamble as well — the reportTUIWarnings contract,
// applied to domain warnings.
func TestReportStartupWarnings_TUIAlsoRecordsInThePreamble(t *testing.T) {
	pre := newStartupPreamble(true)
	var stderr bytes.Buffer

	reportStartupWarnings([]domain.Warning{testWarning("c", "hidden screen", "look here")}, pre, &stderr)

	assert.Equal(t, []string{"Warning: hidden screen", "         look here"}, pre.Lines())
	assert.Contains(t, stderr.String(), "Warning: hidden screen",
		"the stderr copy is kept: a startup that fails before the TUI opens would otherwise lose it")
}

// TestParseTestWarningSpec_RejectsMalformedSpecs: the integration hook must
// never be able to break a real startup, so anything it cannot parse is ignored
// in full.
func TestParseTestWarningSpec_RejectsMalformedSpecs(t *testing.T) {
	for _, spec := range []string{
		"",                     // unset
		"1s|only-two-fields",   // too few
		"1s|c|m|h|extra",       // too many
		"notaduration|c|m",     // bad delay
		"-5ms|c|m",             // negative delay
		"1s||m",                // no code
		"1s|c|",                // no message
		"just some prose here", // not a spec at all
	} {
		_, _, ok := parseTestWarningSpec(spec)
		assert.False(t, ok, "spec %q must be rejected", spec)
	}

	delay, w, ok := parseTestWarningSpec("250ms|test_code|the message|the hint")
	require.True(t, ok)
	assert.Equal(t, 250*time.Millisecond, delay)
	assert.Equal(t, domain.Warning{Code: "test_code", Message: "the message", Hint: "the hint"}, w)

	// The hint is optional.
	_, w, ok = parseTestWarningSpec("0s|test_code|the message")
	require.True(t, ok)
	assert.Empty(t, w.Hint)
}

// TestRegisterTestWarningProducer_IgnoresAnUnsetHook: with the variable unset —
// every real run — nothing is registered and the seal join is instant.
func TestRegisterTestWarningProducer_IgnoresAnUnsetHook(t *testing.T) {
	s := newWarningSink()
	registerTestWarningProducer(s, "")
	assert.True(t, s.Wait(time.Millisecond))
	assert.Nil(t, s.Warnings())

	registerTestWarningProducer(s, "1ms|hooked|hook fired|")
	assert.True(t, s.Wait(2*time.Second))
	require.Len(t, s.Warnings(), 1)
	assert.Equal(t, "hook fired", s.Warnings()[0].Message)
}

// TestRunStatus_WarningsPrintedButNeverChangeTheExitCode is the `prox status`
// half of the contract: a warning is advisory, so it prints — and the command
// still exits 0. A red `prox status` for an untrusted CA would break every
// script that checks the exit code.
func TestRunStatus_WarningsPrintedButNeverChangeTheExitCode(t *testing.T) {
	statusServerWithWarnings(t, []domain.Warning{
		testWarning(domain.WarningCodeMkcertCAUntrusted, "mkcert's local CA is not installed.", "Run 'mkcert -install'."),
	})

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	assert.NoError(t, runErr, "a warning is not a failure")
	assert.Contains(t, stdout, "Warning: mkcert's local CA is not installed.")
	assert.Contains(t, stdout, "         Run 'mkcert -install'.")
}

// TestRunStatus_NoWarningsPrintsNothingExtra: the section is absent, not empty.
func TestRunStatus_NoWarningsPrintsNothingExtra(t *testing.T) {
	statusServerWithWarnings(t, nil)

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runStatus(statusCmd, []string{})
	})

	assert.NoError(t, runErr)
	assert.NotContains(t, stdout, "Warning:")
}

// statusServerWithWarnings starts a fake API server whose status payload
// carries the given warnings, and points apiAddr at it for the test — the
// statusServerWithProxy pattern, for the warnings field.
func statusServerWithWarnings(t *testing.T, warnings []domain.Warning) {
	t.Helper()
	originalApiAddr := apiAddr
	t.Cleanup(func() { apiAddr = originalApiAddr })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status":
			_ = json.NewEncoder(w).Encode(api.StatusResponse{
				Status:         "running",
				UptimeSeconds:  10,
				ConfigFile:     "prox.yaml",
				APIVersion:     "v1",
				Warnings:       warnings,
				WarningsSealed: true,
			})
		case "/api/v1/processes":
			_ = json.NewEncoder(w).Encode(api.ProcessListResponse{Processes: []api.ProcessResponse{
				{Name: "web", Status: "running", PID: 1234, UptimeSeconds: 5, Health: "healthy"},
			}})
		}
	}))
	t.Cleanup(server.Close)
	apiAddr = server.URL
}

// TestRegisterTestWarningProducer_ReleaseBuildsCannotFabricateWarnings pins that
// the synthetic producer is inert in anything but a dev build. It is the only
// mechanism in prox that can show a user a warning nothing observed, and a
// released binary must not carry that capability.
func TestRegisterTestWarningProducer_ReleaseBuildsCannotFabricateWarnings(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	version.Version = "v1.2.3"
	s := newWarningSink()
	registerTestWarningProducer(s, "1ms|hooked|hook fired")
	s.Wait(time.Second)
	require.Empty(t, s.Warnings(), "a release build must ignore the hook entirely")

	version.Version = "dev"
	s = newWarningSink()
	registerTestWarningProducer(s, "1ms|hooked|hook fired")
	s.Wait(time.Second)
	require.Len(t, s.Warnings(), 1, "a dev build still runs it, or the suite loses its only latch test")
}

// TestMkcertTrustWarnings pins the CLI's translation of a verdict into what the
// standalone session reports. Literal verdicts, no resolver: what mkcert's
// output MEANS is tested where it is parsed (internal/proxy/certs), and a CLI
// test must not drive the process-wide resolver to get here.
func TestMkcertTrustWarnings(t *testing.T) {
	untrusted := domain.Warning{
		Code:    domain.WarningCodeMkcertCAUntrusted,
		Message: "Note: the local CA is not installed in the system trust store! ⚠️",
		Hint:    "run 'mkcert -install' and restart prox",
	}

	tests := []struct {
		name    string
		verdict certs.TrustVerdict
		want    []domain.Warning
	}{
		{
			name:    "reports an untrusted CA",
			verdict: certs.TrustVerdict{Known: true, Warning: &untrusted},
			want:    []domain.Warning{untrusted},
		},
		{
			name:    "says nothing for a trusted CA",
			verdict: certs.TrustVerdict{Known: true},
		},
		{
			// Nothing generated a certificate this run, so prox has no evidence:
			// it must not invent a warning, and must not error either.
			name:    "says nothing when the verdict is unknown",
			verdict: certs.TrustVerdict{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mkcertTrustWarnings(tc.verdict))
		})
	}
}
