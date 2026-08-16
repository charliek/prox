package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
)

// This file implements the post-start settle check (plan 027 C13, #94).
//
// `prox up -d`, `prox start` and `prox restart` used to report that the REQUEST
// succeeded, never the resulting STATE: the daemon acked as soon as exec.Start
// had returned, so `prox up -d` exited 0 while `prox status` said `crashed` a
// second later. Every one of those commands now watches the processes it just
// asked for across a short window and refuses to claim success while one of
// them is in a terminal-failed state.
//
// WHAT THIS GUARANTEES, EXACTLY: "no terminal failure was observed within
// processSettleTimeout". It is NOT "the resulting state is verified". A process
// that crashes at 501ms still exits 0 here, and nothing about this check makes
// that impossible — it is a floor on observation, deliberately a heuristic. Do
// not widen the window to chase certainty; certainty is not available at any
// window size, and the cost is paid by every successful command (see below).
//
// It costs ~processSettleTimeout on the SUCCESS path of `start`/`restart`,
// because proving the ABSENCE of a failure means running the window out. That
// is accepted, not an oversight.

const (
	// processSettleTimeout is how long a start is watched before it is called a
	// success. Pinned at 500ms: long enough to catch the overwhelmingly common
	// failure this exists for (a process that execs and dies immediately —
	// missing binary, bad flag, port already bound), short enough that it does
	// not become a tax on interactive use. See the honesty note above before
	// changing it.
	processSettleTimeout = 500 * time.Millisecond

	// processSettlePollInterval is the gap between observations inside that
	// window.
	processSettlePollInterval = 50 * time.Millisecond
)

// settleProcess is the minimal projection of a process the settle check needs.
// It exists so the list endpoint (`up -d`) and the single-process endpoint
// (`start`/`restart`) feed ONE evaluator and ONE formatter, which is the only
// way the two paths cannot drift apart.
type settleProcess struct {
	Name string
	// Status is the raw wire status string, compared against the
	// domain.ProcessState enum. It is deliberately not typed as
	// domain.ProcessState: an unknown value from a newer daemon must fall
	// through as "not a terminal failure", not be forced into the enum.
	Status string
	// BlockedOn lists the failed depends_on targets for a blocked process, when
	// the endpoint reports them. GET /processes/{name} does not, so this is
	// empty on the start/restart path and the formatter degrades to the bare
	// name.
	BlockedOn []string
}

// settleProcessesFromList projects GET /processes.
func settleProcessesFromList(procs []api.ProcessResponse) []settleProcess {
	out := make([]settleProcess, 0, len(procs))
	for _, p := range procs {
		out = append(out, settleProcess{Name: p.Name, Status: p.Status, BlockedOn: p.BlockedOn})
	}
	return out
}

// settleProcessFromDetail projects GET /processes/{name}.
func settleProcessFromDetail(p *api.ProcessDetailResponse) []settleProcess {
	return []settleProcess{{Name: p.Name, Status: p.Status}}
}

// isTerminalFailureState reports whether a reported process status means the
// start has definitively FAILED.
//
// The truth lives in domain.ProcessState.IsTerminalFailure (crashed or
// blocked; widening it is a behavior change to three commands' exit codes).
// This wrapper stays STRING-typed rather than taking a domain.ProcessState:
// status comes off the wire, and an unknown value from a newer daemon must
// fall through as "not a terminal failure" rather than being forced into the
// enum (a bad ProcessState conversion has no zero value that means "I don't
// know"). TestIsTerminalFailureState_CoversEveryState pins the whole enum
// against exactly this.
func isTerminalFailureState(status string) bool {
	return domain.ProcessState(status).IsTerminalFailure()
}

// settleVerdict is what one observation of the processes concluded.
type settleVerdict struct {
	// crashed holds the names of crashed processes, in the order the daemon
	// reported them (no sort — the daemon's order is the config's order).
	crashed []string
	// blocked holds the blocked processes, in the same reported order.
	blocked []settleProcess
}

// failed reports whether the observation saw any terminal failure.
func (v settleVerdict) failed() bool { return len(v.crashed) > 0 || len(v.blocked) > 0 }

// err renders the verdict as the error the command returns, reusing the SAME
// sentinels `prox status` returns so one failure never has two vocabularies.
// Crashed outranks blocked, matching statusExitError's precedence.
func (v settleVerdict) err() error {
	switch {
	case len(v.crashed) > 0:
		return errProcessesCrashed(len(v.crashed))
	case len(v.blocked) > 0:
		return errProcessesBlocked(len(v.blocked))
	default:
		return nil
	}
}

// writeTo prints the human-readable half of the verdict — the same Crashed: and
// Blocked: lines `prox status` prints, built by the same helpers — followed by
// hint when one is supplied. Both lines print when both apply, so neither
// signal is hidden.
func (v settleVerdict) writeTo(w io.Writer, hint string) {
	if !v.failed() {
		// A clean verdict has nothing to say — including the hint, which is
		// remediation for a failure that did not happen.
		return
	}
	if len(v.crashed) > 0 {
		fmt.Fprintln(w, crashedLine(v.crashed))
	}
	if len(v.blocked) > 0 {
		summaries := make([]string, 0, len(v.blocked))
		for _, p := range v.blocked {
			summaries = append(summaries, blockedSummary(p.Name, p.BlockedOn))
		}
		fmt.Fprintln(w, blockedLine(summaries))
	}
	if hint != "" {
		fmt.Fprintln(w, hint)
	}
}

// evaluateProcessSettle classifies one observation. It is pure: no I/O, no
// clock, so the enum coverage test can drive it directly.
func evaluateProcessSettle(procs []settleProcess) settleVerdict {
	var v settleVerdict
	for _, p := range procs {
		// One gate, so the terminal-failure SET is defined in exactly one place
		// and the split below only decides which line the failure prints on.
		if !isTerminalFailureState(p.Status) {
			continue
		}
		switch p.Status {
		case string(domain.ProcessStateBlocked):
			v.blocked = append(v.blocked, p)
		default: // crashed — the only other member of the set
			v.crashed = append(v.crashed, p.Name)
		}
	}
	return v
}

// settleFetch observes the processes under scrutiny once, bounded by ctx.
type settleFetch func(ctx context.Context) ([]settleProcess, error)

// awaitProcessSettle watches fetch for processSettleTimeout and reports what it
// saw.
//
// It returns as soon as it observes a terminal failure (crashed is stable —
// there is no restart policy — so a later poll could only confirm it), and
// otherwise polls until the window is exhausted: concluding the ABSENCE of a
// failure requires running the window out, which is exactly why a clean
// start/restart now costs ~500ms.
//
// The second return value is the VERIFICATION's own failure, kept strictly
// separate from the processes' failure. A transport error, a malformed body, a
// 404, or a daemon that shut down mid-poll must not invent a process failure:
// the caller keeps its pre-existing exit code and warns that state could not be
// confirmed. It is returned only when NO observation succeeded at all — one
// good observation is enough to answer the question, and a later error (the
// daemon going away, say) does not retract it.
func awaitProcessSettle(ctx context.Context, fetch settleFetch, window, interval time.Duration) (settleVerdict, error) {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	deadline := time.Now().Add(window)
	var lastErr error
	observed := false

poll:
	for {
		procs, err := fetch(ctx)
		if err != nil {
			lastErr = err
		} else {
			observed = true
			if v := evaluateProcessSettle(procs); v.failed() {
				return v, nil
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := interval
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			// The window elapsed mid-request, or the caller was cancelled
			// (Ctrl-C). Either way: stop polling, report on what we have.
			break poll
		case <-time.After(sleep):
		}
	}

	if !observed {
		if lastErr == nil {
			// Unreachable in practice (the loop always attempts at least one
			// fetch), but a nil error alongside "nothing observed" would make
			// the caller claim a clean settle it never saw.
			lastErr = fmt.Errorf("no process state was observed within %s", window)
		}
		return settleVerdict{}, lastErr
	}
	return settleVerdict{}, nil
}

// settleAllProcesses observes every process the daemon manages — the `up -d`
// path, where the question is "did anything fail to come up".
func settleAllProcesses(client *Client) settleFetch {
	return func(ctx context.Context) ([]settleProcess, error) {
		resp, err := client.GetProcessesWithContext(ctx)
		if err != nil {
			return nil, err
		}
		return settleProcessesFromList(resp.Processes), nil
	}
}

// settleOneProcess observes a single named process — the `start`/`restart`
// path, where another project's unrelated crashed process must not turn this
// command red.
func settleOneProcess(client *Client, name string) settleFetch {
	return func(ctx context.Context) ([]settleProcess, error) {
		resp, err := client.GetProcessWithContext(ctx, name)
		if err != nil {
			return nil, err
		}
		return settleProcessFromDetail(resp), nil
	}
}

// reportProcessLifecycle is the shared tail of `prox start` and `prox restart`:
// the daemon has already acked, so this decides what the command PRINTS and
// what it EXITS with, based on what the process actually did next.
//
// headline is the success line ("Started process: web"), printed only after the
// window closes without a terminal failure — printing it first and then exiting
// non-zero would be the same lie in a new place.
func reportProcessLifecycle(client *Client, name, headline string) error {
	verdict, err := awaitProcessSettle(
		context.Background(), settleOneProcess(client, name), processSettleTimeout, processSettlePollInterval)
	if err != nil {
		// Verification failed, not the process. The daemon accepted the
		// request, so the pre-existing exit code stands and the doubt is
		// reported once, on stderr.
		fmt.Println(headline)
		fmt.Fprintf(os.Stderr, "Warning: could not confirm the state of %s after the request: %v\n", name, err)
		return nil
	}
	if verdict.failed() {
		verdict.writeTo(os.Stderr, "")
		return verdict.err()
	}
	fmt.Println(headline)
	return nil
}
