package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
)

// followTestCmd returns a bare *cobra.Command carrying ctx, standing in for
// the real rootCmd wiring so followLogs/followRequests's commandContext(cmd)
// call resolves to a context the test can cancel (simulating Ctrl-C via
// followSignalContext's parent-cancellation path).
func followTestCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	return cmd
}

// resetLogsFollowFlags and resetRequestsFollowFlags restore the package-level
// flag vars these tests mutate, matching the save/restore pattern the rest of
// commands_test.go uses for the same vars.
func resetLogsFollowFlags(t *testing.T) {
	t.Helper()
	origAddr, origFollow, origJSON := apiAddr, logsFollow, logsJSON
	origProcess, origPattern, origRegex, origLines := logsProcess, logsPattern, logsRegex, logsLines
	t.Cleanup(func() {
		apiAddr, logsFollow, logsJSON = origAddr, origFollow, origJSON
		logsProcess, logsPattern, logsRegex, logsLines = origProcess, origPattern, origRegex, origLines
	})
	logsProcess, logsPattern, logsRegex, logsLines = "", "", false, 100
}

func resetRequestsFollowFlags(t *testing.T) {
	t.Helper()
	origAddr, origFollow, origJSON := apiAddr, requestsFollow, requestsJSON
	t.Cleanup(func() {
		apiAddr, requestsFollow, requestsJSON = origAddr, origFollow, origJSON
	})
}

// sseLogFrame renders one logs-stream SSE data frame for a raw httptest
// handler (below the Client, which is what's under test in this file).
func sseLogFrame(line string) string {
	entry := api.LogEntryResponse{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Process:   "web",
		Stream:    "stdout",
		Line:      line,
	}
	data, _ := json.Marshal(entry)
	return fmt.Sprintf("data: %s\n\n", data)
}

// sseRequestFrame renders one requests-stream SSE data frame.
func sseRequestFrame(id string) string {
	req := api.ProxyRequestResponse{
		ID:        id,
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Method:    "GET",
		URL:       "/" + id,
	}
	data, _ := json.Marshal(req)
	return fmt.Sprintf("data: %s\n\n", data)
}

// TestRunLogs_FollowReconnectsAfterMidStreamDrop proves the C13 shape: the
// first connect succeeds and delivers entry1, the server then ends that
// stream (a mid-stream drop), and the reconnect loop transparently opens a
// second connection that delivers entry2 — with both entries reaching stdout
// and the disconnected/reconnected notices reaching stderr, transitions only.
func TestRunLogs_FollowReconnectsAfterMidStreamDrop(t *testing.T) {
	resetLogsFollowFlags(t)

	var attempts int32
	attempt3 := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		switch n {
		case 1:
			// One entry, then the handler returns: the connection closes and
			// the first (channel-form) attempt ends cleanly — the mid-stream
			// drop.
			w.Write([]byte(sseLogFrame("entry1")))
			flusher.Flush()
		case 2:
			// The reconnect loop's attempt: one more entry, then close again.
			// onEvent prints synchronously inside the read loop, so attempt 3
			// ARRIVING server-side proves entry2 was already printed — the
			// deterministic sync point the old fixed sleep lacked (codex C13
			// finding).
			w.Write([]byte(sseLogFrame("entry2")))
			flusher.Flush()
		default:
			close(attempt3)
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	apiAddr = server.URL
	logsFollow = true

	ctx, cancel := context.WithCancel(context.Background())
	cmd := followTestCmd(ctx)

	go func() {
		<-attempt3
		cancel()
	}()

	stdout, stderr := captureOutput(t, func() {
		if err := runLogs(cmd, []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "entry1") {
		t.Errorf("expected entry1 on stdout, got %q", stdout)
	}
	if !strings.Contains(stdout, "entry2") {
		t.Errorf("expected entry2 on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, followStreamDisconnected) {
		t.Errorf("expected disconnect notice on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, followStreamReconnected) {
		t.Errorf("expected reconnect notice on stderr, got %q", stderr)
	}
	// Two outages (after attempt 1 and after attempt 2), so exactly two
	// disconnect notices — one per outage, never one per retry. The second
	// reconnect notice races the cancel, so only the first is guaranteed.
	if n := strings.Count(stderr, followStreamDisconnected); n != 2 {
		t.Errorf("expected exactly 2 disconnect notices, got %d in %q", n, stderr)
	}
	if n := strings.Count(stderr, followStreamReconnected); n < 1 {
		t.Errorf("expected at least 1 reconnect notice, got %d in %q", n, stderr)
	}
	// A clean Ctrl-C teardown must never surface as an error (codex C13).
	if strings.Contains(stderr, "context canceled") {
		t.Errorf("cancellation leaked onto stderr: %q", stderr)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected exactly 3 connect attempts, got %d", got)
	}
}

// TestRunLogs_FollowInitialConnectFailureFailsFast pins the UNCHANGED
// behavior for a failed FIRST connect (plan 017 C13 requirement 1): no
// reconnect loop ever engages, and the error text/hint are byte-for-byte what
// `prox logs --follow` returned before this landed.
func TestRunLogs_FollowInitialConnectFailureFailsFast(t *testing.T) {
	resetLogsFollowFlags(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	apiAddr = server.URL
	logsFollow = true

	var err error
	_, _ = captureOutput(t, func() {
		err = runLogs(logsCmd, []string{})
	})

	if err == nil {
		t.Fatal("expected an error from a failed initial connect")
	}
	const wantText = "server error: the prox daemon encountered an internal error\nIs prox running? Try 'prox up' first."
	if err.Error() != wantText {
		t.Errorf("expected fail-fast error %q, got %q", wantText, err.Error())
	}
}

// TestRunLogs_FollowTerminalErrorExitsNonZero proves a 401 raised by a
// RECONNECT attempt (never by the first connect, which already succeeded) is
// classified terminal: the loop ends, the error reaches stderr, and runLogs
// returns a non-zero error rather than retrying forever.
func TestRunLogs_FollowTerminalErrorExitsNonZero(t *testing.T) {
	resetLogsFollowFlags(t)

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(sseLogFrame("entry1")))
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(api.ErrorResponse{Error: "invalid token", Code: "UNAUTHORIZED"})
	}))
	defer server.Close()

	apiAddr = server.URL
	logsFollow = true

	cmd := followTestCmd(context.Background())

	var err error
	stdout, stderr := captureOutput(t, func() {
		err = runLogs(cmd, []string{})
	})

	if err == nil {
		t.Fatal("expected a terminal error")
	}
	const wantErr = "UNAUTHORIZED: invalid token"
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("expected error to contain %q, got %q", wantErr, err.Error())
	}
	if !strings.Contains(stderr, wantErr) {
		t.Errorf("expected stderr to carry the terminal error, got %q", stderr)
	}
	if !strings.Contains(stdout, "entry1") {
		t.Errorf("expected entry1 printed before the terminal failure, got %q", stdout)
	}
}

// TestRunRequests_FollowReconnectsAfterMidStreamDrop is the requests-command
// counterpart of TestRunLogs_FollowReconnectsAfterMidStreamDrop, proving the
// shared runFollowLoop wiring works identically for `prox requests --follow`
// with its own event type and rendering.
func TestRunRequests_FollowReconnectsAfterMidStreamDrop(t *testing.T) {
	resetRequestsFollowFlags(t)

	var attempts int32
	attempt3 := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		switch n {
		case 1:
			w.Write([]byte(sseRequestFrame("req1")))
			flusher.Flush()
		case 2:
			// Close after req2; attempt 3 arriving proves req2 was printed
			// (see the logs counterpart for the full reasoning).
			w.Write([]byte(sseRequestFrame("req2")))
			flusher.Flush()
		default:
			close(attempt3)
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	apiAddr = server.URL
	requestsFollow = true

	ctx, cancel := context.WithCancel(context.Background())
	cmd := followTestCmd(ctx)

	go func() {
		<-attempt3
		cancel()
	}()

	stdout, stderr := captureOutput(t, func() {
		if err := runRequests(cmd, []string{}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(stdout, "req1") || !strings.Contains(stdout, "req2") {
		t.Errorf("expected both requests rendered on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, followStreamDisconnected) || !strings.Contains(stderr, followStreamReconnected) {
		t.Errorf("expected both notices on stderr, got %q", stderr)
	}
	if strings.Contains(stderr, "context canceled") {
		t.Errorf("cancellation leaked onto stderr: %q", stderr)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected exactly 3 connect attempts, got %d", got)
	}
}

// TestRunLogs_FollowSignalDuringInitialDialExitsClean pins the clean-Ctrl-C
// contract for the FIRST connect (codex C13 finding): a signal that cancels
// the initial dial is a clean exit 0, not the fail-fast connect error.
func TestRunLogs_FollowSignalDuringInitialDialExitsClean(t *testing.T) {
	resetLogsFollowFlags(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer: only cancellation ends the dial
	}))
	defer server.Close()

	apiAddr = server.URL
	logsFollow = true

	// A pre-cancelled parent stands in for the signal firing mid-dial: the
	// NotifyContext child is born cancelled, exactly as after a Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := followTestCmd(ctx)

	_, stderr := captureOutput(t, func() {
		if err := runLogs(cmd, []string{}); err != nil {
			t.Errorf("a signal-cancelled initial dial must exit clean, got: %v", err)
		}
	})
	if strings.Contains(stderr, "Is prox running") {
		t.Errorf("fail-fast error leaked for a cancelled dial: %q", stderr)
	}
}

// TestRunRequests_FollowTerminalProxyDisabledExitsNonZero proves the
// requests-specific reconnect policy: a 503 PROXY_NOT_ENABLED raised by a
// RECONNECT attempt is terminal for the CLI (unlike the TUI's passive
// StateUnavailable park — a --follow command has no status bar to fall back
// to), so it exits non-zero with the error on stderr instead of retrying
// forever against a feed that will never come back without a config change.
func TestRunRequests_FollowTerminalProxyDisabledExitsNonZero(t *testing.T) {
	resetRequestsFollowFlags(t)

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(sseRequestFrame("req1")))
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(api.ErrorResponse{
			Error: "proxy is not enabled",
			Code:  domain.ErrCodeProxyNotEnabled,
		})
	}))
	defer server.Close()

	apiAddr = server.URL
	requestsFollow = true

	cmd := followTestCmd(context.Background())

	var err error
	_, stderr := captureOutput(t, func() {
		err = runRequests(cmd, []string{})
	})

	if err == nil {
		t.Fatal("expected a terminal error")
	}
	if !strings.Contains(err.Error(), domain.ErrCodeProxyNotEnabled) {
		t.Errorf("expected error to name %s, got %q", domain.ErrCodeProxyNotEnabled, err.Error())
	}
	if !strings.Contains(stderr, domain.ErrCodeProxyNotEnabled) {
		t.Errorf("expected stderr to carry the terminal error, got %q", stderr)
	}
}
