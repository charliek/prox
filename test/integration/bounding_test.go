package integration

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file proves the invariant plan 027 C6 rests on: a polling helper handed
// a deadline RETURNS BY THAT DEADLINE, even when the thing it is waiting on
// never responds at all.
//
// It is worth stating why the obvious test would be worthless. Asserting on the
// wording of a timeout message ("did contain a deadline") passes just as
// happily against a helper that hangs forever, because the assertion never
// runs -- the test hangs with it. So every case here does three things:
//
//  1. stands up a genuinely stalled dependency (a TCP listener that accepts and
//     never writes a byte; a subprocess that never exits);
//  2. gives the real helper a SHORT deadline and measures the wall clock around
//     the call, failing if it does not come back within a small multiple;
//  3. checks that the elapsed time the helper REPORTS matches the elapsed time
//     that actually passed.
//
// (3) is not pedantry. The helpers used to report a nominal duration -- "did
// not happen within 15s" -- which a deadline cannot even know: `within` hands a
// nested wait only the remainder of its caller's budget, so the sentence was
// routinely false by an order of magnitude and sent readers hunting a stall
// that never happened.

// boundingProbeBudget is the deadline every case here hands its helper. Short,
// because the whole point is that a stalled dependency costs the budget and not
// a second more.
const boundingProbeBudget = 2 * time.Second

// boundingSlack is how far the helper may overshoot its deadline before this
// test calls it unbounded. Generous enough to survive a loaded machine (each
// helper still has to finish an in-flight poll and one sleep), tight enough
// that a genuinely unbounded helper -- which would hang until the watchdog
// fires -- can never pass.
const boundingSlack = 4 * time.Second

// TestBounding_StalledServerDoesNotOutliveTheDeadline points the two
// HTTP-polling helpers at a listener that completes the TCP handshake and then
// says nothing forever.
//
// This is the exact failure http.DefaultClient cannot survive: the connection
// is established, so there is no dial error, and with no Timeout the read
// blocks indefinitely. awaitLogContains in particular used a bare http.Get and
// really did hang here before this commit.
func TestBounding_StalledServerDoesNotOutliveTheDeadline(t *testing.T) {
	startTest(t, defaultTestBudget)

	addr := stalledServer(t)

	t.Run("awaitAPI", func(t *testing.T) {
		assertBounded(t, func(deadline time.Time) error {
			return awaitAPI(addr, deadline)
		})
	})

	t.Run("awaitLogContains", func(t *testing.T) {
		assertBounded(t, func(deadline time.Time) error {
			return awaitLogContains(t, addr, "anything", "never-appears", deadline)
		})
	})
}

// TestBounding_StalledSubprocessDoesNotOutliveTheDeadline points the CLI-driving
// poll at a "prox" that never exits.
//
// pollStatusJSON used to shell out with a bare cmd.Run() and no timeout at all,
// and then -- on the deadline it did not enforce -- RETURN THE LAST PAYLOAD
// instead of failing, so a hung CLI surfaced as a confusing assertion about
// process state one level downstream. Both halves are covered here: the call
// returns on time, and it returns an error saying so.
func TestBounding_StalledSubprocessDoesNotOutliveTheDeadline(t *testing.T) {
	startTest(t, defaultTestBudget)

	dir := t.TempDir()
	stalled := writeStalledBinary(t, dir)

	// The REAL helper, built the way dependencies_test.go builds it -- only the
	// binary it invokes is one that never exits.
	runStatus := runStatusIn(t, stalled, dir, filepath.Join(dir, "prox.yaml"))

	assertBounded(t, func(deadline time.Time) error {
		_, err := awaitStatusJSON(runStatus, deadline, func(statusJSONPayload) bool { return false })
		return err
	})
}

// assertBounded runs wait with a short real deadline and holds it to all three
// claims: it came back, it came back on time, and it told the truth about how
// long it took.
func assertBounded(t *testing.T, wait func(deadline time.Time) error) {
	t.Helper()

	deadline := time.Now().Add(boundingProbeBudget)

	// The call itself runs on another goroutine so that an UNBOUNDED helper
	// fails this assertion instead of hanging the test until the watchdog fires.
	// A hang that reports "unbounded" is a useful failure; a hang that reports
	// nothing is the bug this whole commit is about.
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		err := wait(deadline)
		done <- result{err: err, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(boundingProbeBudget + boundingSlack):
		t.Fatalf("helper did not return %v after its deadline: it is not bounded by the deadline it was given",
			boundingSlack)
	}

	if got.err == nil {
		t.Fatalf("expected a timeout error from a stalled dependency, got nil after %v", got.elapsed)
	}
	if got.elapsed < boundingProbeBudget {
		t.Errorf("helper returned after %v, before its own %v deadline; it is not actually waiting",
			got.elapsed, boundingProbeBudget)
	}

	msg := got.err.Error()

	// The message must carry an ABSOLUTE deadline, not a nominal duration: a
	// nested helper only ever receives the remainder of its caller's budget, so
	// a duration in the text would be a guess about a number nobody kept.
	if !strings.Contains(msg, "deadline "+deadline.Format(clockFormat)) {
		t.Errorf("message does not report the absolute deadline %s: %s", deadline.Format(clockFormat), msg)
	}

	reported, ok := reportedElapsed(msg)
	if !ok {
		t.Fatalf("message does not report an elapsed time: %s", msg)
	}
	if drift := absDuration(reported - got.elapsed); drift > elapsedDrift {
		t.Errorf("reported elapsed %v disagrees with observed %v (drift %v > %v): %s",
			reported, got.elapsed, drift, elapsedDrift, msg)
	}
}

// elapsedDrift is how far the reported elapsed time may differ from the wall
// clock this test measured around the call. Nonzero only because the two clocks
// are started a few statements apart.
const elapsedDrift = 250 * time.Millisecond

// reportedElapsedRE matches the "after 2.001s (deadline ..." prefix waitedFor
// renders.
var reportedElapsedRE = regexp.MustCompile(`after ([0-9smhµn.]+) \(deadline `)

func reportedElapsed(msg string) (time.Duration, bool) {
	m := reportedElapsedRE.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return 0, false
	}
	return d, true
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// stalledServer returns the base URL of a listener that ACCEPTS connections and
// then never writes anything.
//
// Accepting matters: a closed port produces an instant dial error, which every
// helper already survives. The interesting failure -- and the one an unbounded
// client cannot escape -- is a peer that completes the handshake and then goes
// silent, which is what a wedged daemon actually looks like.
func stalledServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	var held []net.Conn
	closed := false

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = c.Close()
				continue
			}
			// Held, never read from and never written to. Dropping the
			// reference would let the connection be closed and hand the client
			// an EOF, i.e. exactly the prompt answer this test must not get.
			held = append(held, c)
			mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range held {
			_ = c.Close()
		}
	})

	return "http://" + ln.Addr().String()
}

// writeStalledBinary writes an executable that ignores its arguments and never
// exits, to stand in for a `prox` that has wedged.
func writeStalledBinary(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "stalled-prox")
	script := "#!/bin/sh\n# stands in for a wedged `prox`: accepts any args, never exits\nsleep 600\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stalled binary: %v", err)
	}
	return path
}

// TestBounding_WaitedForIsHonestAboutARemainderBudget pins the specific lie
// this commit removed from every timeout message.
//
// `within` gives a nested wait only what is LEFT of its caller's budget, so a
// helper nominally documented as a 15s wait can legitimately be handed 200ms.
// A message quoting the nominal 15s would send the reader looking for a stall
// that never happened; waitedFor quotes the clock instead.
func TestBounding_WaitedForIsHonestAboutARemainderBudget(t *testing.T) {
	startTest(t, defaultTestBudget)

	// A caller whose own budget has almost run out.
	callerDeadline := time.Now().Add(200 * time.Millisecond)

	// A nested wait asking for a full role budget gets the remainder instead.
	nested := minTime(time.Now().Add(logAppearTimeout), callerDeadline)
	if !nested.Equal(callerDeadline) {
		t.Fatalf("a nested wait must be clamped to its caller's deadline; got %v", nested)
	}

	start := time.Now()
	time.Sleep(250 * time.Millisecond)
	msg := fmt.Sprintf("something did not happen %s", waitedFor(start, nested))

	if strings.Contains(msg, logAppearTimeout.String()) {
		t.Errorf("message quotes the nominal role budget %v, which this wait never had: %s", logAppearTimeout, msg)
	}
	reported, ok := reportedElapsed(msg)
	if !ok {
		t.Fatalf("message does not report an elapsed time: %s", msg)
	}
	if reported < 250*time.Millisecond {
		t.Errorf("reported elapsed %v is less than the time that actually passed: %s", reported, msg)
	}
	if !strings.Contains(msg, "deadline "+nested.Format(clockFormat)) {
		t.Errorf("message does not report the absolute deadline it was given: %s", msg)
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
