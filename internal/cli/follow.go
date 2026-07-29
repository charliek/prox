package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/stream"
	"github.com/spf13/cobra"
)

// This file implements the CLI's --follow reconnect behavior (plan 017 C13,
// W2+W7), shared by `prox logs --follow` and `prox requests --follow`.
//
// Shape, chosen deliberately for the smallest change against today's
// behavior: the FIRST connect is made exactly as before this landed — the
// channel form (Client.StreamLogsChannel / StreamProxyRequestsChannel),
// synchronously, with its error returned straight to the caller (PINNED:
// fail-fast + today's error text + non-zero exit, UNCHANGED). Only once that
// channel CLOSES — meaning at least one connect already succeeded and the
// stream then ended for any reason — does control hand off to a
// stream.Loop, built from the consume* attempt forms, for every subsequent
// attempt. There is no backfill across the handoff or across any later
// reconnect: the stderr notices mark the gap instead, and duplicates on
// stdout are avoided by never re-fetching history.
//
// Ctrl-C is wired via signal.NotifyContext around the whole follow session
// (both the first connect and the reconnect loop), so an interactive quit
// cancels ctx and unwinds cleanly to exit 0, exactly like today's Ctrl-C.

// followStreamDisconnected / followStreamReconnected are the stderr notices
// for a --follow reconnect. They print on TRANSITIONS only (first drop of an
// outage, and the OK that ends one) — never on every retry attempt — mirrored
// on the TUI's streamDropped latch (internal/tui/stream_health.go). stdout
// carries only rendered events, so `prox logs --follow --json | jq` etc. never
// sees these.
const (
	followStreamDisconnected = "prox: stream disconnected, reconnecting..."
	followStreamReconnected  = "prox: stream reconnected"
)

// runFollowLoop drains an already-connected --follow channel (first) to
// completion, printing each event via printEvent exactly as the pre-reconnect
// channel loop did, then — once first closes — keeps the stream going across
// drops with a stream.Loop built from attempt/classify until ctx is cancelled
// or an attempt error is classified terminal.
//
// It returns nil when ctx was cancelled (Ctrl-C: exit 0, matching today), and
// the terminal error otherwise (non-zero exit via cobra's RunE contract).
// Status transitions print to stderr per followStatusPrinter; stdout is left
// to printEvent/attempt alone so it stays machine-clean under --json.
func runFollowLoop[T any](ctx context.Context, first <-chan T, printEvent func(T), attempt func(context.Context, func()) error, classify func(error) stream.Classification) error {
	for entry := range first {
		printEvent(entry)
	}
	// The channel closes either because ctx was cancelled (Ctrl-C) or because
	// the stream ended out from under us. Only the latter engages the
	// reconnect loop; a cancelled ctx must return immediately with no further
	// output.
	if ctx.Err() != nil {
		return nil
	}

	// Reaching here IS the disconnect: the first connection, established
	// outside any loop, just ended. Announce it once, unconditionally, before
	// the reconnect loop takes over — the loop's own first attempt reports
	// StateConnecting (it has no memory of the connection that preceded it),
	// so nothing downstream would ever print this notice otherwise. Passing
	// dropped=true into followStatusPrinter latches it immediately, so the
	// loop's first OK (whether on its first attempt or after some retries)
	// prints exactly one matching followStreamReconnected.
	fmt.Fprintln(os.Stderr, followStreamDisconnected)

	var termErr error
	loop := stream.NewLoop(stream.Config{
		Attempt:  attempt,
		Classify: classify,
		OnStatus: followStatusPrinter(&termErr, true),
	})
	loop.Run(ctx)

	if ctx.Err() != nil {
		return nil
	}
	return termErr
}

// followStatusPrinter builds the OnStatus callback for a --follow
// stream.Loop. dropped starts pre-latched to true (runFollowLoop already
// printed followStreamDisconnected for the handoff itself), and from there it
// tracks exactly like the TUI's streamDropped latch
// (internal/tui/stream_health.go): a LATER StateReconnecting prints the
// disconnected notice again only if a reconnected notice cleared the latch in
// between, and the OK that ends any drop prints followStreamReconnected
// exactly once. A terminal StateClosed (a classified error, not a cancelled
// ctx) prints the error and stashes it in *termErr for runFollowLoop to
// return.
func followStatusPrinter(termErr *error, dropped bool) func(stream.Status) {
	return func(s stream.Status) {
		switch s.State {
		case stream.StateReconnecting:
			if !dropped {
				dropped = true
				fmt.Fprintln(os.Stderr, followStreamDisconnected)
			}
		case stream.StateOK:
			if dropped {
				dropped = false
				fmt.Fprintln(os.Stderr, followStreamReconnected)
			}
		case stream.StateClosed:
			// StateClosed also fires on context cancellation with Err ==
			// ctx.Err() — that is a clean Ctrl-C, not a failure, and must not
			// write "prox: context canceled" to the terminal (codex C13
			// finding). Only a real terminal error is recorded and printed.
			if s.Err != nil && !errors.Is(s.Err, context.Canceled) {
				*termErr = s.Err
				fmt.Fprintf(os.Stderr, "prox: %v\n", s.Err)
			}
		}
	}
}

// followSignalContext wraps ctx with signal.NotifyContext(SIGINT, SIGTERM) for
// a --follow session, so Ctrl-C (or a TERM) cancels the connect/reconnect loop
// cleanly instead of the process dying mid-stream. Callers must defer the
// returned stop func.
func followSignalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// classifyLogsFollowError is the CLI --follow reconnect policy for the logs
// stream: an authentication/authorization failure will not fix itself by
// retrying, so it ends the loop (exit non-zero); everything else (dial
// failures, hangups, 5xx) is transient and worth reconnecting.
func classifyLogsFollowError(err error) stream.Classification {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return stream.ClassTerminal
		}
	}
	return stream.ClassTransient
}

// followLogs implements `prox logs --follow` (plan 017 C13). The first
// connect is the plain channel form on a signal-wired ctx, so a connect
// failure fails fast with today's error and non-zero exit (PINNED). Once that
// channel closes, runFollowLoop hands off to a reconnect loop built from
// ConsumeLogs; onHandshake is nil because --follow has no cursor/backfill
// protocol to resume (no backfill on reconnect is the deliberate contract —
// see the file comment). markSynced rides ConsumeLogs' onConnect, matching
// the attach TUI's pure-consumer streams (internal/tui/app.go
// consumeProcesses).
func followLogs(cmd *cobra.Command, client *Client, params domain.LogParams, jsonOutput bool, printer *LogPrinter) error {
	ctx, stop := followSignalContext(commandContext(cmd))
	defer stop()

	ch, err := client.StreamLogsChannel(ctx, params)
	if err != nil {
		// Ctrl-C during the initial dial cancels the request and surfaces as
		// a connect error — that is a clean exit, not a failure (codex C13
		// finding). Genuine connect failures keep today's fail-fast error.
		if ctx.Err() != nil {
			return nil
		}
		return clientError(err, "Is prox running? Try 'prox up' first.")
	}

	printEvent := func(entry api.LogEntryResponse) {
		printLogEntry(entry, jsonOutput, printer)
	}
	attempt := func(attemptCtx context.Context, markSynced func()) error {
		return client.ConsumeLogs(attemptCtx, params, markSynced, nil, printEvent)
	}

	return runFollowLoop(ctx, ch, printEvent, attempt, classifyLogsFollowError)
}

// printLogEntry renders one streamed log entry exactly as the pre-C13 channel
// loop did — JSON-encoded to stdout under --json, or through the LogPrinter's
// colorized/plain rendering otherwise. Factored out so the first connection's
// drain loop and every reconnect attempt share identical rendering.
func printLogEntry(entry api.LogEntryResponse, jsonOutput bool, printer *LogPrinter) {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(entry); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode log entry: %v\n", err)
		}
		return
	}
	printer.PrintAPIEntry(entry)
}

// followRequests implements `prox requests --follow` (plan 017 C13). The
// first connect is the plain channel form on a signal-wired ctx, so a connect
// failure fails fast with today's error and non-zero exit (PINNED). Once that
// channel closes, runFollowLoop hands off to a reconnect loop built from
// ConsumeProxyRequests. markSynced rides onConnect, exactly as consumeProcesses
// does for the attach TUI's processes stream (internal/tui/app.go): the
// requests stream has no sync protocol to reconcile, so connect IS sync.
func followRequests(cmd *cobra.Command, client *Client, params domain.ProxyRequestParams, jsonOutput bool) error {
	ctx, stop := followSignalContext(commandContext(cmd))
	defer stop()

	ch, err := client.StreamProxyRequestsChannel(ctx, params)
	if err != nil {
		// Same clean-exit rule as followLogs: a signal-cancelled initial dial
		// is not a failure.
		if ctx.Err() != nil {
			return nil
		}
		return clientError(err, "Is prox running with proxy enabled? Try 'prox up' first.")
	}

	printEvent := func(req api.ProxyRequestResponse) {
		printProxyRequestEvent(req, jsonOutput)
	}
	attempt := func(attemptCtx context.Context, markSynced func()) error {
		return client.ConsumeProxyRequests(attemptCtx, params, markSynced, printEvent)
	}

	return runFollowLoop(ctx, ch, printEvent, attempt, classifyRequestsFollowError)
}

// printProxyRequestEvent renders one streamed proxy request exactly as the
// pre-C13 channel loop did — JSON-encoded to stdout under --json, or through
// printProxyRequest's line format otherwise. Factored out for the same reason
// as printLogEntry.
func printProxyRequestEvent(req api.ProxyRequestResponse, jsonOutput bool) {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(req); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to encode request: %v\n", err)
		}
		return
	}
	printProxyRequest(req)
}

// classifyRequestsFollowError extends classifyLogsFollowError with the one
// condition unique to the requests stream: the daemon runs fine with the
// proxy disabled, and a 503 PROXY_NOT_ENABLED means "no such feed" rather than
// a transient outage. Unlike the TUI's attach-mode policy (which parks the
// loop passively, StateUnavailable, behind a status bar), a --follow CLI
// command has no passive display to fall back to, so this is ALSO terminal
// here: print the error and exit non-zero rather than retry forever against a
// feed that will never come back without a config/restart change.
func classifyRequestsFollowError(err error) stream.Classification {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusUnauthorized, apiErr.Status == http.StatusForbidden:
			return stream.ClassTerminal
		case apiErr.Status == http.StatusServiceUnavailable && apiErr.Code == domain.ErrCodeProxyNotEnabled:
			return stream.ClassTerminal
		}
	}
	return stream.ClassTransient
}
