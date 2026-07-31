package proxyd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
)

// ForwarderStatusSink receives connection-state and backfill signals from the
// SSE forwarder so the CLI can surface shared-proxy health in `prox status`
// (D5/D9). It is defined here — not in internal/cli — so proxyd stays free of a
// cli import; the cli's proxyRuntime implements it. All methods must be safe to
// call from the forwarder goroutine and are no-ops when the sink is nil.
type ForwarderStatusSink interface {
	// ForwarderConnected is called each time the bridge establishes an SSE
	// connection (HTTP 200). Implementations reset the consecutive-failure
	// counter and record the connect time.
	ForwarderConnected()
	// ForwarderConnectFailed is called each time a connection attempt fails
	// before reaching a live stream. Implementations increment the
	// consecutive-failure counter.
	ForwarderConnectFailed(err error)
	// ForwarderBackfillFailed is called when a post-connect ring snapshot fetch
	// fails (the stream continues degraded). Implementations increment the
	// backfill-failure counter.
	ForwarderBackfillFailed()
}

// HealFunc is the forwarder's self-heal callback (D6b). It is a plain func passed
// in from internal/cli (keeping proxyd free of a cli import): when the shared
// daemon has been unreachable past the heal threshold, the forwarder invokes it
// INLINE in the reconnect loop (never concurrent with a connect attempt). The
// implementation re-ensures a daemon of this version and re-registers the
// project against it, returning true only when the project is registered on a
// healthy daemon — the forwarder then reconnects eagerly to the fresh daemon on
// the same socket. It no-ops (returns false) once shutdown has been latched.
type HealFunc func() bool

// streamFlapThreshold is the minimum time a connected SSE stream must survive
// to count as a real recovery in the reconnect loop (see the flap guard).
const streamFlapThreshold = time.Second

// ForwardRequests subscribes to the daemon's SSE request stream and forwards
// this project's records into the local RequestManager. This bridges the
// daemon's proxy request data into the project's TUI and API.
//
// projectDir must be the same value sent as RegisterRequest.ProjectDir so the
// daemon-side filter (which scopes records by owning project) matches.
//
// On every (re)connect the bridge also backfills a snapshot of the daemon's
// current ring for this project (see streamRequests / backfillSnapshot), closing
// any gap opened while the subscription was down. The subscription itself stays
// lossy (bounded, non-blocking channels), so the guarantee is bounded gap
// closure, not losslessness.
//
// sink (may be nil) receives connect/disconnect/backfill signals so the CLI can
// report shared-proxy health; it replaces the old silent `_ = err` reconnect
// loop with state that surfaces in `prox status`.
//
// heal (may be nil) is invoked inline after a prolonged outage to re-ensure and
// re-register against a fresh daemon (D6b).
//
// It runs until ctx is cancelled. On disconnect, it reconnects with backoff.
func ForwardRequests(ctx context.Context, socketPath string, projectDir string, localRM *proxy.RequestManager, sink ForwarderStatusSink, heal HealFunc) {
	forwardRequests(ctx, forwarderConfig{
		socketPath:      socketPath,
		projectDir:      projectDir,
		localRM:         localRM,
		sink:            sink,
		heal:            heal,
		now:             time.Now,
		after:           time.After,
		healAfterDown:   constants.ForwarderHealAfterDown,
		healMinInterval: constants.ForwarderHealMinInterval,
		flapThreshold:   streamFlapThreshold,
	})
}

// forwarderConfig bundles the forwarder loop's inputs plus the injectable clock,
// timer, and heal thresholds so C6's heal timing can be unit-tested with no
// wall-clock waits (mirrors the injectable-ops pattern used by the cli's
// skewOps/registerRetryOps). ForwardRequests wires the production values.
type forwarderConfig struct {
	socketPath      string
	projectDir      string
	localRM         *proxy.RequestManager
	sink            ForwarderStatusSink
	heal            HealFunc                             // may be nil
	now             func() time.Time                     // heal-timing clock
	after           func(time.Duration) <-chan time.Time // backoff timer
	healAfterDown   time.Duration
	healMinInterval time.Duration
	// flapThreshold is the minimum lifetime for a connected stream to count
	// as a recovery (streamFlapThreshold in production; tests with instant
	// scripted streams set 0 to opt out, and the flap test pins the guard).
	flapThreshold time.Duration
	// stream is the single connect-and-forward attempt (nil → the production
	// streamRequests). Injectable so heal-timing tests can script a deterministic
	// connect/drop/outage sequence without real sockets (FIX 4).
	stream func(ctx context.Context, socketPath string, snapClient *Client, projectDir string, localRM *proxy.RequestManager, sink ForwarderStatusSink) (connected bool, err error)
}

// shouldHeal reports whether a heal should fire now, given when the current
// outage started (downSince, zero when connected), when the last heal was
// attempted (lastHeal, zero when none yet), and the D6b thresholds: the outage
// must have lasted at least healAfterDown AND at least healMinInterval must have
// elapsed since the last heal attempt (damping a flapping daemon). The first
// heal of an outage is gated only by healAfterDown.
func shouldHeal(now, downSince, lastHeal time.Time, healAfterDown, healMinInterval time.Duration) bool {
	if downSince.IsZero() {
		return false
	}
	if now.Sub(downSince) < healAfterDown {
		return false
	}
	if !lastHeal.IsZero() && now.Sub(lastHeal) < healMinInterval {
		return false
	}
	return true
}

func forwardRequests(ctx context.Context, cfg forwarderConfig) {
	// One snapshot client for the whole forwarder lifetime: its transport pools
	// the idle unix connection across reconnect attempts, rather than leaking a
	// fresh idle conn per attempt (the daemon only reaps idle conns after 60s, so
	// a reconnect storm would otherwise accumulate them).
	snapClient := NewClient(cfg.socketPath)

	stream := cfg.stream
	if stream == nil {
		stream = streamRequests
	}

	backoff := 500 * time.Millisecond
	// Heal-timing state, tracked locally so it is single-threaded with the connect
	// attempts (the heal runs inline in this loop, never concurrently).
	var downSince time.Time // zero while connected; set on the first failed reconnect of an outage
	var lastHeal time.Time  // zero until the first heal attempt of an outage

	for {
		// attemptStart feeds the flap guard below; only sample the clock when
		// the guard is enabled (scripted-clock tests with flapThreshold 0
		// must not consume extra instants).
		var attemptStart time.Time
		if cfg.flapThreshold > 0 {
			attemptStart = cfg.now()
		}
		connected, err := stream(ctx, cfg.socketPath, snapClient, cfg.projectDir, cfg.localRM, cfg.sink)
		if ctx.Err() != nil {
			return // context cancelled, clean shutdown
		}
		// A 200 that dies immediately is a FLAP, not a recovery: a
		// crash-looping daemon that accepts and instantly EOFs would
		// otherwise clear the outage clock on every cycle and permanently
		// suppress healing (CodeRabbit PR #68). Only a stream that lived
		// past the flap threshold counts as connected for reset purposes.
		if connected && cfg.flapThreshold > 0 && cfg.now().Sub(attemptStart) < cfg.flapThreshold {
			connected = false
			if err == nil {
				err = fmt.Errorf("stream ended within %s of connecting (flapping daemon)", cfg.flapThreshold)
			}
		}
		if connected {
			// The previous attempt reached a live stream: the daemon is (or was
			// just) healthy. Clear the outage clock and reconnect eagerly instead
			// of carrying a stale outage-sized backoff into the recovery.
			//
			// lastHeal is deliberately NOT reset here (FIX 4): the ≥healMinInterval
			// heal spacing must hold ACROSS a brief reconnect, so a daemon that
			// flaps (heal, reconnect, drop, new outage) cannot fire a second heal
			// sooner than healMinInterval after the first. Resetting it would let a
			// new outage's healAfterDown alone re-trigger a heal within seconds.
			downSince = time.Time{}
			backoff = 500 * time.Millisecond
		} else {
			// A connect that never reached a live stream is a reconnect failure:
			// the sink counts it (and logs the connected→down transition once). A
			// stream that connected and then dropped is not itself a failure — the
			// NEXT failed reconnect, if any, records the outage.
			if cfg.sink != nil {
				cfg.sink.ForwarderConnectFailed(err)
			}
			now := cfg.now()
			if downSince.IsZero() {
				downSince = now
			}
			// Self-heal (D6b), inline so it cannot race the next connect attempt.
			if cfg.heal != nil && shouldHeal(now, downSince, lastHeal, cfg.healAfterDown, cfg.healMinInterval) {
				lastHeal = now
				if cfg.heal() {
					// Fresh daemon on the same socket: restart the outage clock and
					// reconnect eagerly so a momentary miss doesn't immediately
					// re-heal, and so the next attempt binds the healthy daemon.
					downSince = time.Time{}
					backoff = 500 * time.Millisecond
				}
			}
		}

		// Backoff before reconnecting.
		select {
		case <-ctx.Done():
			return
		case <-cfg.after(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// streamRequests opens an SSE connection to the daemon and processes events.
// snapClient is the shared daemon client used for the backfill snapshot; sink
// (may be nil) receives connect/backfill signals.
//
// It returns connected=true once the SSE connection reached a live stream (HTTP
// 200), regardless of how the stream later ended, so the caller can distinguish
// a failed reconnect (connected=false, count it) from a dropped live stream
// (connected=true, do not count it).
func streamRequests(ctx context.Context, socketPath string, snapClient *Client, projectDir string, localRM *proxy.RequestManager, sink ForwarderStatusSink) (connected bool, err error) {
	// Create HTTP client that dials the Unix socket
	dialer := &net.Dialer{}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	streamURL := fmt.Sprintf("http://proxyd/api/v1/requests/stream?project=%s", url.QueryEscape(projectDir))
	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		return false, fmt.Errorf("creating SSE request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("connecting to daemon SSE: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("daemon SSE returned %d", resp.StatusCode)
	}

	// The subscription is live once the daemon returned 200. Signal the sink so
	// it resets the failure counter and records the connect time (D5); a
	// down→connected transition logs one line there.
	connected = true
	if sink != nil {
		sink.ForwarderConnected()
	}

	// The daemon Subscribes before it writes response headers, so once Do
	// returned 200 the subscription is already live. Backfill a snapshot of the
	// daemon's ring concurrently with the read loop below: launching it in a
	// goroutine (rather than fetching before entering the loop) ensures the
	// forwarder never sits blocked on a slow snapshot while live events pile up
	// — a >100-event burst during the fetch would overflow the daemon-side
	// subscription channel, the exact gap this backfill exists to close.
	//
	// The fetch is scoped to attemptCtx, cancelled when streamRequests returns
	// (defer). This bounds the snapshot to THIS connection attempt: a stalled
	// fetch cannot outlive its stream and replay a stale snapshot after a later
	// attempt has already established fresh state (whose distinct IDs would
	// bypass Upsert's dedupe and cause wrong evictions / duplicate notifies).
	// The goroutine exits when the fetch completes OR attemptCtx is cancelled,
	// so it never leaks; it writes only to localRM, which outlives the stream.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go backfillSnapshot(attemptCtx, snapClient, projectDir, localRM, sink)

	// Read SSE events line by line. bufio.Scanner is NOT used: its token size is
	// capped (and even a raised cap would kill the subscription on the first
	// oversized line). Records can legitimately exceed 64KB — on a capture
	// disk-write failure both bodies fall back to inline storage up to 1MB each,
	// and headers are unbounded — so we use a bufio.Reader with an explicit
	// per-event cap (constants.MaxSSEEventSize). An oversized event is skipped
	// (drained to its newline) with a logged warning and the stream continues,
	// rather than tearing down the bridge.
	reader := bufio.NewReaderSize(resp.Body, constants.ScannerBufferSize)
	dataPrefix := []byte("data: ")
	for {
		line, oversize, readErr := readEventLine(reader, constants.MaxSSEEventSize)

		switch {
		case readErr != nil:
			// A line returned alongside a read error has no terminating
			// newline — the SSE event is incomplete (mid-write disconnect).
			// Discard it rather than recording a possibly-partial event; the
			// reconnect loop will re-sync on fresh events.
		case oversize:
			log.Printf("prox: skipping oversized daemon request event (exceeds %d bytes)", constants.MaxSSEEventSize)
		case len(line) > 0:
			// SSE format: "data: {json}". Trim the trailing "\r\n"/"\n".
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, dataPrefix) {
				jsonData := trimmed[len(dataPrefix):]
				var record proxy.RequestRecord
				if err := json.Unmarshal(jsonData, &record); err == nil {
					// Upsert (not Record): applying live events through the
					// monotonic state machine makes interleaving with the
					// concurrent snapshot replay safe — duplicates and stale
					// in-flight events are no-ops, so nothing is delivered twice.
					localRM.Upsert(record)
				}
				// Malformed events are skipped silently, as before.
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return connected, nil
			}
			return connected, readErr
		}
	}
}

// backfillSnapshot fetches the daemon's current record snapshot for projectDir
// and replays it into localRM, closing any gap opened while the SSE bridge was
// disconnected. It is launched from streamRequests once the subscription is
// live and drains concurrently with the read loop; Upsert's monotonic state
// machine makes any interleaving of {snapshot copy, live copy, completion} safe
// (duplicates and stale in-flight events are no-ops), so records are never
// delivered twice.
//
// The limit is pinned to constants.MaxProxyRequests so a full ring backfills
// completely in one fetch; an omitted limit would silently cap at the daemon's
// default of 100. That works because the daemon ring size
// (constants.DefaultProxyRequestBufferSize) is DEFINED as MaxProxyRequests — do
// not compare either against a literal, and do not raise the ring size without
// raising the clamp. Client.Requests decodes all-or-nothing, so a truncated body
// applies zero records.
//
// Records outside the daemon's captured-body detail window arrive with their
// bodies already marked evicted (D9b); they replay into localRM as-is. A
// record still inside that window arrives WITH its body — the daemon's
// eviction publishes no event, so nothing on this path strips it later; it is
// localRM's own timestamp-ordered inline-body window (see
// proxy.NewReplicaRequestManager) that bounds bodies retained on the live
// path, backfill included.
//
// On any failure — non-200, decode error, timeout, or ctx cancellation — it
// logs one warning and returns, leaving the stream to run in degraded,
// stream-only mode. A failed backfill never tears down the bridge.
//
// ctx is the per-attempt context, so a stream error/return cancels an in-flight
// fetch; client is the forwarder's shared snapshot client (pooled unix conn).
func backfillSnapshot(ctx context.Context, client *Client, projectDir string, localRM *proxy.RequestManager, sink ForwarderStatusSink) {
	records, err := client.Requests(ctx, projectDir, constants.MaxProxyRequests)
	if err != nil {
		if sink != nil {
			sink.ForwarderBackfillFailed()
		}
		log.Printf("prox: request snapshot backfill failed: %v", err)
		return
	}

	// The endpoint returns records newest-first; replay oldest-first so ring
	// order tracks arrival order as closely as the live stream would have.
	// Bail out mid-replay if the attempt ended: a snapshot fetched by a dying
	// attempt should not keep replaying after a newer attempt took over.
	for i := len(records) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
		localRM.Upsert(records[i])
	}
}

// readEventLine reads a single '\n'-terminated line from r, enforcing maxSize.
// It returns:
//   - the line (including its trailing newline) when within the cap;
//   - oversize=true with a nil line when the line exceeds maxSize — the excess is
//     drained up to (and including) the newline so the reader is positioned at
//     the start of the next event and the caller can continue the stream;
//   - a non-nil error (e.g. io.EOF) from the underlying reader, alongside any
//     partial line read so far.
//
// It uses ReadSlice against r's fixed internal buffer, so no single call buffers
// more than that buffer's size; the accumulated line is bounded by maxSize.
func readEventLine(r *bufio.Reader, maxSize int) (line []byte, oversize bool, err error) {
	var buf []byte
	for {
		chunk, rerr := r.ReadSlice('\n')

		if !oversize {
			if len(buf)+len(chunk) > maxSize {
				// Over the cap: stop accumulating and release what we have; keep
				// draining until the newline so the next event is aligned.
				oversize = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}

		switch rerr {
		case nil:
			// Reached the newline: the line is complete.
			if oversize {
				return nil, true, nil
			}
			return buf, false, nil
		case bufio.ErrBufferFull:
			// No newline in this fill; keep reading the same line.
			continue
		default:
			// Underlying error (io.EOF or a transport failure).
			if oversize {
				return nil, true, rerr
			}
			return buf, false, rerr
		}
	}
}
