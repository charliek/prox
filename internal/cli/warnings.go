package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/version"
)

// warningPrefix is the label every user-facing advisory carries. It matches the
// bare `fmt.Fprintf(os.Stderr, "Warning: ...")` convention already used all over
// this package (up.go, daemon_startup.go), so a warning that arrived from the
// shared daemon reads exactly like one raised locally.
const warningPrefix = "Warning: "

// warningHintIndent lines a warning's hint up under its message rather than
// under the label, so the two-line block reads as one item:
//
//	Warning: mkcert's local CA is not installed in your trust stores.
//	         Run 'mkcert -install' and restart prox.
var warningHintIndent = strings.Repeat(" ", len(warningPrefix))

// warningProducerJoinTimeout bounds how long runUp waits for asynchronous
// warning producers before it seals (see warningSink.Wait). A warning must never
// meaningfully delay startup: when the budget expires the session seals with
// whatever has landed, and a producer that reports later still reaches
// GET /status — it just misses the `prox up -d` parent's read.
const warningProducerJoinTimeout = 2 * time.Second

// formatWarning renders one warning as the lines to print, hint included only
// when there is one. Returned as lines (not one embedded-newline string) because
// every destination — the terminal, the TUI's pinned preamble, the system log —
// is line-oriented, and a two-line log entry is not two log entries.
func formatWarning(w domain.Warning) []string {
	lines := []string{warningPrefix + w.Message}
	if w.Hint != "" {
		lines = append(lines, warningHintIndent+w.Hint)
	}
	return lines
}

// writeWarnings writes every warning to out in order. It is the single renderer:
// `prox status`, the `prox up -d` parent and the `prox up` startup path all go
// through it, so the shape cannot drift between the three places a user meets
// the same warning.
func writeWarnings(out io.Writer, ws []domain.Warning) {
	for _, w := range ws {
		for _, line := range formatWarning(w) {
			fmt.Fprintln(out, line)
		}
	}
}

// reportStartupWarnings routes this session's warnings to whichever screen the
// user is actually going to look at. It is the reportTUIWarnings template
// (preamble.go) applied to domain warnings, for the same reasons:
//
//   - the preamble, so a TUI session sees them on the screen bubbletea is about
//     to hide the terminal behind (and, through the preamble's system-log path,
//     so `prox logs` and the daemon log get them too);
//   - stderr, ALWAYS, because the preamble is only ever rendered if the TUI
//     actually opens — and because plain `prox up` has no other channel. In a
//     `-d` child, stderr is .prox/prox.log, which is why the parent reads them
//     back over the API instead (see startDetachedDaemon).
//
// MUST be called from runUp's own goroutine: startupPreamble is explicitly
// single-goroutine and unsynchronized.
func reportStartupWarnings(ws []domain.Warning, pre *startupPreamble, stderr io.Writer) {
	for _, w := range ws {
		for _, line := range formatWarning(w) {
			// note (not printf): printf writes to stdout, and these lines go to
			// stderr below. note is a no-op unless the preamble is enabled (TUI).
			pre.note("%s", line)
		}
	}
	writeWarnings(stderr, ws)
}

// warningTestHookEnvVar injects a synthetic, deliberately SLOW warning producer
// into a real `prox` process. It exists for exactly one thing the suite cannot
// otherwise reach: proving that a warning sealed AFTER the `prox up -d` parent's
// readiness + settle wait still reaches that parent's terminal — the race
// warnings_sealed exists for. Reproducing it needs a producer that finishes at a
// controlled instant well after the API server starts serving, and no real
// producer can be asked to do that.
//
// Value: "<delay>|<code>|<message>[|<hint>]", e.g.
// "1200ms|test_warning|the CA is not installed|run mkcert -install".
// A malformed value is ignored in full — a broken test hook must never break a
// real startup. Unset (the case for every user), nothing runs at all.
const warningTestHookEnvVar = "PROX_TEST_STARTUP_WARNING"

// registerTestWarningProducer wires the warningTestHookEnvVar producer into s,
// if the variable is set and parses. See that constant for why it exists.
//
// It is inert in RELEASE-PIPELINE builds. This is the one mechanism in prox
// that can put a warning in front of a user which nothing actually observed,
// and the whole point of plan 028 is that prox does not tell people things that
// are not true, so the shipped artifact should not carry the capability at all.
// Only .goreleaser.yaml sets the version ldflag, so `version.Version != "dev"`
// is precisely "built by the release pipeline".
//
// Be exact about what that does NOT cover: `make build` and
// `go install ...@latest` both leave Version at "dev", so a source-built prox
// still honours the variable (CodeRabbit review). That residue is accepted
// rather than engineered away — a build tag would mean the integration suite
// tested a different binary from the one users run, which is a worse trade —
// and the blast radius is a user who exports a variable in their own shell to
// print one line to their own terminal.
func registerTestWarningProducer(s *warningSink, spec string) {
	if version.Version != "dev" {
		return
	}
	delay, w, ok := parseTestWarningSpec(spec)
	if !ok {
		return
	}
	s.Go(func() []domain.Warning {
		time.Sleep(delay)
		return []domain.Warning{w}
	})
}

// parseTestWarningSpec parses "<delay>|<code>|<message>[|<hint>]". ok is false
// for an empty or malformed spec.
func parseTestWarningSpec(spec string) (time.Duration, domain.Warning, bool) {
	parts := strings.Split(spec, "|")
	if len(parts) < 3 || len(parts) > 4 {
		return 0, domain.Warning{}, false
	}
	delay, err := time.ParseDuration(parts[0])
	if err != nil || delay < 0 {
		return 0, domain.Warning{}, false
	}
	w := domain.Warning{Code: parts[1], Message: parts[2]}
	if len(parts) == 4 {
		w.Hint = parts[3]
	}
	if w.Code == "" || w.Message == "" {
		return 0, domain.Warning{}, false
	}
	return delay, w, true
}

// warningSink is this session's collection of user-facing advisories
// (domain.Warning) — most of them raised somewhere the person who typed the
// command cannot see: inside the shared proxy daemon, whose stdout/stderr are
// /dev/null, or inside a `prox up -d` child, whose output goes to .prox/prox.log.
//
// Writes happen on runUp's own goroutine during startup, plus the forwarder's
// goroutine on a self-heal re-register (proxyRuntime.heal). The mutex is what
// makes both safe against the READS, which come from HTTP handler goroutines
// serving GET /status.
//
// A nil *warningSink is a usable no-op sink, so a code path that was never
// handed one (a unit-test runtime, a future caller) does not have to nil-check
// before every Add.
type warningSink struct {
	mu       sync.Mutex
	warnings []domain.Warning
	// sealed is the completion latch GET /status publishes as warnings_sealed.
	// See Seal.
	sealed bool

	// pending tracks asynchronous startup producers registered with Go, so Wait
	// can join them before the session seals.
	pending sync.WaitGroup
}

func newWarningSink() *warningSink { return &warningSink{} }

// Add records warnings, dropping any that duplicate one already held, and
// returns the ones that were actually NEW. Callers use that return to log or
// print a warning exactly once even when the producing path runs repeatedly (the
// forwarder's heal loop re-registers on every recovery).
//
// Identity is domain.DedupeWarnings' identity — Code AND Message, hint excluded
// — so the two producers of the same advisory cannot make it appear twice.
func (s *warningSink) Add(ws ...domain.Warning) []domain.Warning {
	if s == nil || len(ws) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	before := len(s.warnings)
	// s.warnings is already deduped, so the surviving prefix is unchanged and
	// everything past `before` is genuinely new.
	s.warnings = domain.DedupeWarnings(append(s.warnings, ws...))
	if len(s.warnings) == before {
		return nil
	}
	added := make([]domain.Warning, len(s.warnings)-before)
	copy(added, s.warnings[before:])
	return added
}

// Warnings returns a COPY of the collected warnings, nil when there are none so
// an omitempty JSON field disappears rather than serializing as []. The copy is
// what lets an HTTP goroutine encode a response while startup is still adding.
func (s *warningSink) Warnings() []domain.Warning {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.warnings) == 0 {
		return nil
	}
	out := make([]domain.Warning, len(s.warnings))
	copy(out, s.warnings)
	return out
}

// Seal latches "every startup warning producer has finished", which GET /status
// publishes as warnings_sealed. It exists for one reader: the `prox up -d`
// parent, which fetches status once its child is ready and its processes have
// settled. Some producers are asynchronous and can still be running at that
// instant, so an unlatched single fetch would race them and silently lose a
// warning; the parent instead polls until this is true (bounded — see
// awaitDaemonWarnings).
//
// Sealing does NOT freeze the sink: a warning raised later in the session (a
// forwarder self-heal re-register) is still recorded and still served. The latch
// says startup is done, not that nothing more can ever be said.
func (s *warningSink) Seal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
}

// WarningsSealed reports the completion latch (see Seal).
func (s *warningSink) WarningsSealed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed
}

// Go runs an asynchronous startup warning producer, recording whatever it
// returns. It is the seam for checks that must not block startup while they run
// but MUST be finished before the session seals: register them here, and Wait
// joins them at the seal point.
func (s *warningSink) Go(produce func() []domain.Warning) {
	if s == nil || produce == nil {
		return
	}
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.Add(produce()...)
	}()
}

// Wait blocks until every producer registered with Go has finished, or until
// timeout expires, reporting whether they all finished. Every Go call happens on
// runUp's goroutine before this one, so there is no Add-after-Wait race on the
// WaitGroup itself.
func (s *warningSink) Wait(timeout time.Duration) bool {
	if s == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
