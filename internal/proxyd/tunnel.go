package proxyd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/hashicorp/yamux"
)

// --- the tunnel wire protocol (plan 031 D2) ---
//
// A publisher holds ONE outbound connection to the hub, upgraded from
// POST /api/v1/tunnel and then multiplexed with yamux: the hub is the yamux
// SERVER (it opens streams), the publisher is the yamux CLIENT (it accepts
// them). One stream carries one proxied TCP connection, prefixed by a one-line
// preamble:
//
//	hub       -> publisher   "CONNECT host:port\n"
//	publisher -> hub         "OK\n"   or   "ERR <reason>\n"
//
// After "OK" the stream is a plain byte pipe, which is the whole point: the
// hub's httputil.ReverseProxy sees a net.Conn, so WebSocket upgrades, SSE, and
// keep-alive all work without the tunnel knowing anything about HTTP.
const (
	// tunnelUpgradeProtocol is the Upgrade token both ends use, so a stray
	// browser or a WebSocket client cannot accidentally complete the handshake.
	tunnelUpgradeProtocol = "prox-tunnel"

	// tunnelPreambleMaxBytes is the HARD cap on one preamble line, enforced
	// identically on both ends (D2). It bounds what a peer can make the other
	// side buffer before the line is even parsed: without it, a peer that never
	// sends "\n" could stream unbounded bytes into the reader.
	tunnelPreambleMaxBytes = 512

	tunnelConnectPrefix = "CONNECT "
	tunnelReplyOK       = "OK"
	tunnelReplyErr      = "ERR"
)

// tunnelYamuxConfig is the yamux configuration BOTH ends run (plan 031 D16).
//
// The three tuned values matter for different reasons. KeepAliveInterval and
// ConnectionWriteTimeout are what eventually tear a dead peer's session down
// (5s + 10s ≈ 15s, versus ~40s at yamux's defaults) — eventual teardown, not
// the prompt failure path, which is HubDialTimeout. StreamOpenTimeout bounds
// how long a stream whose SYN the peer never ACKs may hold one of the 256
// inflight-SYN slots before yamux closes the session outright.
//
// LogOutput is discarded: yamux logs to os.Stderr by default, and the daemon's
// stderr goes nowhere useful (it is a detached background process).
func tunnelYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = constants.HubTunnelKeepAliveInterval
	cfg.ConnectionWriteTimeout = constants.HubTunnelWriteTimeout
	cfg.StreamOpenTimeout = constants.HubTunnelStreamOpenTimeout
	cfg.LogOutput = io.Discard
	return cfg
}

// tunnelMaxPendingStreamOpens is how many OpenStream attempts may be
// outstanding against ONE session at a time (plan 031, review B4). It is
// yamux's own inflight-SYN budget, read from the config both ends run, because
// that is the number of attempts that can be in flight in the session below us:
// a 257th goroutine would only be a more expensive way of waiting for the same
// slot.
var tunnelMaxPendingStreamOpens = tunnelYamuxConfig().AcceptBacklog

// readPreambleLine reads ONE "\n"-terminated line from r, refusing anything
// longer than max bytes.
//
// It reads a byte at a time ON PURPOSE. A bufio.Reader would be faster but
// would over-read: whatever it buffered past the "\n" would be lost when the
// caller hands the raw stream to an http.Transport (hub side) or to io.Copy
// (publisher side). Byte-at-a-time reads are cheap here — the peer is a yamux
// stream backed by an in-memory buffer, and a preamble is a few dozen bytes.
//
// The caller sets the read DEADLINE on the stream before calling; framing
// strictness is only half the story, and a peer that opens a stream and then
// says nothing is the other half (D2/D16).
func readPreambleLine(r io.Reader, max int) (string, error) {
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return string(buf), nil
			}
			if len(buf) >= max {
				return "", fmt.Errorf("tunnel preamble exceeds %d bytes with no line terminator", max)
			}
			buf = append(buf, one[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return "", errors.New("tunnel preamble ended without a line terminator")
			}
			return "", err
		}
	}
}

// validatePreambleLine is the framing rule applied to every preamble line on
// BOTH ends before it is parsed (plan 031 D2): within the byte cap, and free of
// NUL, embedded newlines, and any other control character.
//
// Rejecting embedded newlines and NUL is not decoration. The preamble is the
// one place a peer's bytes are interpreted as a command, so anything that could
// smuggle a second line — or terminate a C-style string in whatever reads a log
// of it — is refused before parsing rather than after.
func validatePreambleLine(line string) error {
	if len(line) > tunnelPreambleMaxBytes {
		return fmt.Errorf("tunnel preamble exceeds %d bytes", tunnelPreambleMaxBytes)
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == 0:
			return errors.New("tunnel preamble contains a NUL byte")
		case c == '\n' || c == '\r':
			return errors.New("tunnel preamble contains an embedded newline")
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("tunnel preamble contains a control character (0x%02x)", c)
		}
	}
	return nil
}

// parseConnectLine validates and parses the hub's "CONNECT host:port" preamble.
//
// host:port goes through net.SplitHostPort rather than a LastIndex(":") split
// so a bracketed IPv6 literal ("[::1]:3000") parses correctly instead of
// yielding the host "[" and a nonsense port.
func parseConnectLine(line string) (host string, port int, err error) {
	if err := validatePreambleLine(line); err != nil {
		return "", 0, err
	}
	if !strings.HasPrefix(line, tunnelConnectPrefix) {
		return "", 0, fmt.Errorf("malformed tunnel preamble %q: expected %q followed by host:port", line, tunnelConnectPrefix)
	}
	target := strings.TrimPrefix(line, tunnelConnectPrefix)
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, fmt.Errorf("malformed tunnel target %q: %w", target, err)
	}
	if h == "" {
		return "", 0, fmt.Errorf("malformed tunnel target %q: no host", target)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("malformed tunnel target %q: invalid port %q", target, p)
	}
	return h, n, nil
}

// formatConnectLine renders the hub's preamble for a target. JoinHostPort
// brackets an IPv6 literal, which is exactly what parseConnectLine expects.
func formatConnectLine(host string, port int) string {
	return tunnelConnectPrefix + net.JoinHostPort(host, strconv.Itoa(port)) + "\n"
}

// parseTunnelReply validates the publisher's one-line answer. A nil error means
// "OK"; anything else — an explicit ERR, a malformed line, a line that breaks
// the framing rules — fails the dial.
func parseTunnelReply(line string) error {
	if err := validatePreambleLine(line); err != nil {
		return err
	}
	switch {
	case line == tunnelReplyOK:
		return nil
	case strings.HasPrefix(line, tunnelReplyErr):
		reason := strings.TrimSpace(strings.TrimPrefix(line, tunnelReplyErr))
		if reason == "" {
			reason = "no reason given"
		}
		return fmt.Errorf("publisher refused the tunnel target: %s", reason)
	default:
		return fmt.Errorf("malformed tunnel reply %q", line)
	}
}

// --- hub-side session management ---

// tunnelSession is ONE publisher's attached tunnel: its yamux session, the
// generation it was attached under, and the single http.Transport the data
// plane proxies through.
//
// One transport per session is what makes keep-alive work: the transport pools
// idle yamux streams the same way it would pool idle TCP connections, so a
// second request to the same publisher reuses the first request's stream
// instead of paying a fresh CONNECT round trip. When the session goes, so does
// the transport — a pooled stream on a dead session is worthless.
type tunnelSession struct {
	key         string
	gen         uint64
	sess        *yamux.Session
	transport   *http.Transport
	connectedAt time.Time
	dialTimeout time.Duration
	// openSlots bounds how many OpenStream attempts may be outstanding against
	// this session at once (plan 031, review B4). See openStream.
	openSlots chan struct{}
}

// dial opens a stream, performs the CONNECT handshake, and returns the
// established stream as a net.Conn (plan 031 D2/D16).
//
// Every failure path CLOSES the abandoned stream. That matters more than it
// looks: yamux caps inflight (unACKed) SYNs at AcceptBacklog, and a stream left
// dangling against a frozen publisher would sit in that budget until
// StreamOpenTimeout. Closing it releases the slot as soon as the peer
// acknowledges, and bounds the damage to StreamOpenTimeout when it never does.
func (t *tunnelSession) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	// The dial deadline is the whole handshake's bound (D16), and it starts
	// HERE — before the stream exists — because acquiring the stream is itself
	// something that can block (see openStream). A caller context with an
	// earlier deadline wins, so a cancelled request does not wait out the full
	// budget.
	deadline := time.Now().Add(t.dialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	stream, err := t.openStream(ctx, deadline)
	if err != nil {
		return nil, err
	}

	if err := stream.SetDeadline(deadline); err != nil {
		_ = stream.Close()
		return nil, err
	}

	if _, err := io.WriteString(stream, formatConnectLine(host, port)); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("writing the tunnel preamble to %s: %w", t.key, err)
	}
	line, err := readPreambleLine(stream, tunnelPreambleMaxBytes)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("reading the tunnel reply from %s: %w", t.key, err)
	}
	if err := parseTunnelReply(line); err != nil {
		_ = stream.Close()
		return nil, err
	}

	// Handshake done: the stream is an ordinary byte pipe from here, so the
	// handshake deadline must not linger and kill a long-lived WebSocket or SSE
	// response mid-flight.
	if err := stream.SetDeadline(time.Time{}); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return stream, nil
}

// openedStream is one result of an OpenStream attempt, carried on a channel so
// the attempt can be abandoned at a deadline.
type openedStream struct {
	stream net.Conn
	err    error
}

// openStream acquires a yamux stream under the dial deadline (plan 031 F7,
// bounded by review B4).
//
// yamux caps INFLIGHT (unACKed) SYNs at AcceptBacklog — 256 — and OpenStream
// BLOCKS on that budget with no context, no deadline, and no way to give up:
// `select { case s.synCh <- struct{}{}: case <-s.shutdownCh: }`. Against a
// frozen publisher (one whose kernel still ACKs but whose process answers
// nothing) the budget fills, and every caller past the 256th waited inside
// OpenStream until yamux's StreamOpenTimeout (30s) tore the SESSION down —
// tenfold HubDialTimeout, and exactly the "requests hang instead of getting a
// 503" outcome the offline page exists to prevent. Applying the deadline only
// AFTER OpenStream returned could not see any of that.
//
// So the call is raced against the deadline on a goroutine — and the goroutines
// are BOUNDED by openSlots, which is the correction review B4 asked for. The
// first fix made the client-visible symptom right (prompt 503s) and moved the
// cost somewhere invisible: sustained traffic to one frozen route accumulated a
// goroutine per abandoned attempt, forever, because each one stayed parked
// inside OpenStream until the session died. openSlots has exactly as many
// permits as yamux has SYN slots, which is the most attempts that can be
// USEFULLY outstanding: past that point every further attempt is queued behind
// the same budget anyway, so waiting for a permit — cancellably, under the same
// deadline — is strictly better than spawning a goroutine to wait for the
// budget uncancellably. A caller that cannot get a permit before its deadline
// gets the same 503 it would have got at the end of a stream open it was never
// going to win.
//
// The abandoned attempt is not leaked either: the goroutine hands its result to
// whoever is still listening and otherwise closes the stream itself, then
// releases its permit.
func (t *tunnelSession) openStream(ctx context.Context, deadline time.Time) (net.Conn, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	// The permit, taken BEFORE the goroutine exists so that nothing is spawned
	// for an attempt that has already run out of time.
	select {
	case t.openSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("opening a tunnel stream to %s: %w", t.key, ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("timed out waiting for a tunnel stream slot to %s: the publisher is not acknowledging new streams", t.key)
	}

	// ch is UNBUFFERED on purpose: the goroutine's send can only complete while
	// this function is still receiving, so "the caller took it" and "the caller
	// gave up" are mutually exclusive without any shared flag between them.
	ch := make(chan openedStream)
	abandoned := make(chan struct{})
	go func() {
		defer func() { <-t.openSlots }()
		stream, err := t.sess.OpenStream()
		select {
		case ch <- openedStream{stream: stream, err: err}:
		case <-abandoned:
			if err == nil {
				_ = stream.Close()
			}
		}
	}()

	select {
	case got := <-ch:
		if got.err != nil {
			return nil, fmt.Errorf("opening a tunnel stream to %s: %w", t.key, got.err)
		}
		return got.stream, nil
	case <-ctx.Done():
		close(abandoned)
		return nil, fmt.Errorf("opening a tunnel stream to %s: %w", t.key, ctx.Err())
	case <-timer.C:
		close(abandoned)
		return nil, fmt.Errorf("timed out acquiring a tunnel stream to %s: the publisher is not acknowledging new streams", t.key)
	}
}

// close tears the session down and drops any streams the transport pooled.
func (t *tunnelSession) close() {
	t.transport.CloseIdleConnections()
	_ = t.sess.Close()
}

// tunnelSessions is the hub's session manager: at most one attached tunnel per
// project key, each with a monotonically increasing generation (plan 031 D17).
//
// LOCK DISCIPLINE (D17), stated here the way dynamic_proxy.go states probeMu's:
// mu is a LEAF. The daemon's order is lifecycleMu → registry.mu, and mu sits
// below both — it is never taken while either is held, never takes either while
// it is held, and is never held across I/O or a session close. Every method
// here therefore does nothing but read or swap map entries; closing a replaced
// or removed session is the CALLER's job, after the lock is released.
//
// The generation is what makes a replacement safe (P2). A second tunnel for the
// same key replaces the first, and the first's close callback then arrives
// LATE — after a newer session is already installed. Gating every removal on
// "is the stored generation still mine" turns that late callback into a no-op
// instead of a disconnect that would knock the live successor offline.
type tunnelSessions struct {
	mu       sync.Mutex
	sessions map[string]*tunnelSession
	nextGen  uint64
	// dialTimeout is the per-dial handshake bound, injectable so tests can
	// exercise the frozen-publisher path in milliseconds instead of waiting out
	// constants.HubDialTimeout (mirrors DynamicProxy's probe seams).
	dialTimeout time.Duration
}

func newTunnelSessions() *tunnelSessions {
	return &tunnelSessions{
		sessions:    make(map[string]*tunnelSession),
		dialTimeout: constants.HubDialTimeout,
	}
}

// attach installs sess as key's tunnel under a fresh generation and returns the
// new session plus the one it REPLACED (nil when there was none). The caller
// must close the replaced session after releasing every other lock it holds —
// closing it here would violate the leaf rule (never held across a session
// close).
func (ts *tunnelSessions) attach(key string, sess *yamux.Session) (attached, replaced *tunnelSession) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.nextGen++
	t := &tunnelSession{
		key:         key,
		gen:         ts.nextGen,
		sess:        sess,
		connectedAt: time.Now(),
		dialTimeout: ts.dialTimeout,
		// Exactly as many permits as yamux has inflight-SYN slots: one attempt
		// per slot is the most that can make progress, and the rest wait here —
		// cancellably — instead of in a goroutine apiece (review B4).
		openSlots: make(chan struct{}, tunnelMaxPendingStreamOpens),
	}
	t.transport = &http.Transport{
		// Every connection this transport makes goes through the tunnel; the
		// addr it is handed is the publisher's OWN host:port, which is exactly
		// what the CONNECT preamble carries.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("tunnel target %q: %w", addr, err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, fmt.Errorf("tunnel target %q: invalid port %q", addr, portStr)
			}
			return t.dial(ctx, host, port)
		},
		MaxIdleConns:        constants.DefaultProxyMaxIdleConns,
		IdleConnTimeout:     constants.DefaultProxyIdleConnTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	replaced = ts.sessions[key]
	ts.sessions[key] = t
	return t, replaced
}

// setDialTimeout replaces the per-dial handshake bound applied to sessions
// attached from now on. It is the timing seam the frozen-publisher tests use so
// they can exercise the real path in milliseconds rather than waiting out
// constants.HubDialTimeout for every request.
func (ts *tunnelSessions) setDialTimeout(d time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.dialTimeout = d
}

// get returns key's current session, or nil.
func (ts *tunnelSessions) get(key string) *tunnelSession {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.sessions[key]
}

// detach removes key's session only if the stored generation is still gen, and
// reports whether it did. This is the P2 gate: a replaced session's close
// callback carries an older generation and is ignored.
func (ts *tunnelSessions) detach(key string, gen uint64) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.sessions[key]
	if !ok || t.gen != gen {
		return false
	}
	delete(ts.sessions, key)
	return true
}

// remove takes key's session out unconditionally and returns it for the caller
// to close (`prox hub stop`, teardown). nil when key had none.
func (ts *tunnelSessions) remove(key string) *tunnelSession {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.sessions[key]
	if !ok {
		return nil
	}
	delete(ts.sessions, key)
	return t
}

// removeGen takes key's session out only when the installed generation is still
// gen, and returns it for the caller to close (deregister, takeover). nil when
// key has no session or has moved on to a newer one — which is exactly the case
// a blind removal used to get wrong (plan 031, review B1).
func (ts *tunnelSessions) removeGen(key string, gen uint64) *tunnelSession {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.sessions[key]
	if !ok || t.gen != gen {
		return nil
	}
	delete(ts.sessions, key)
	return t
}

// removeAll empties the manager and returns every session for the caller to
// close outside the lock (`prox hub stop`, daemon teardown).
func (ts *tunnelSessions) removeAll() []*tunnelSession {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	all := make([]*tunnelSession, 0, len(ts.sessions))
	for key, t := range ts.sessions {
		all = append(all, t)
		delete(ts.sessions, key)
	}
	return all
}

// TransportFor returns the http.Transport that proxies to key's publisher, and
// ok=false when no tunnel is attached — which is precisely "the publisher is
// offline" and is what the data plane turns into the 503 page.
//
// It is the data plane's ONLY entry point into the session layer, and it hands
// back a transport rather than a dialer on purpose (plan 031, the C3+C4 review's
// reuse gap): there used to be two ways in — a tunnelSessions.Dial that nothing
// but a test called, and the handler reaching through tunnelSessions.get into
// the session's transport field. One abstraction is now the rule: the session is
// resolved HERE, by key (P5 — never read off the *Route the registry hands the
// data plane outside its lock), and the per-session transport is what callers
// get, because a transport is what pools keep-alive streams and a bare dial is
// not.
func (ts *tunnelSessions) TransportFor(key string) (*http.Transport, bool) {
	t := ts.get(key)
	if t == nil {
		return nil, false
	}
	return t.transport, true
}

// --- the hub's upgrade handler ---

// handleHubTunnel is POST /api/v1/tunnel on the NETWORK mount (plan 031 §4.3).
//
// The key is COMPOSED from the caller's own X-Prox-Origin and
// X-Prox-Project-Dir, never read from a pre-qualified header (D15) — the same
// rule the register and deregister handlers follow, with the same two
// strengths: a publisher cannot attach a tunnel to a LOCAL project's key at all
// (no composition produces one), and cannot attach to another publisher's BY
// ACCIDENT. A publisher that deliberately sends another's origin can attach in
// its place, which is D12's accepted trust model (§8) and wants per-origin
// credentials rather than a stricter header rule.
func (s *Server) handleHubTunnel(w http.ResponseWriter, r *http.Request) {
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), tunnelUpgradeProtocol) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("tunnel requires Connection: Upgrade and Upgrade: %s", tunnelUpgradeProtocol),
			Code:  "BAD_REQUEST",
		})
		return
	}

	origin := strings.TrimSpace(r.Header.Get("X-Prox-Origin"))
	if err := validateHubOrigin(origin); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error(), Code: "BAD_REQUEST"})
		return
	}
	dir := strings.TrimSpace(r.Header.Get("X-Prox-Project-Dir"))
	// The SAME rule register, deregister and the capture endpoints apply (plan
	// 031, review B9). This was the one key-composition path that only checked
	// for emptiness, so a header no other endpoint would accept could still be
	// composed into a lookup key here.
	if err := validateHubProjectDir(dir); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("X-Prox-Project-Dir: %v", err),
			Code:  "BAD_REQUEST",
		})
		return
	}
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "daemon is starting up", Code: "NOT_READY"})
		return
	}

	key := HubProjectKey(origin, dir)
	if !s.registry.HasProject(key) {
		// The publisher's registration is gone (swept after a long outage, or
		// displaced by a takeover). Saying so precisely is what lets the
		// publisher re-register instead of retrying a handshake that can never
		// succeed (RunTunnel's reregister callback).
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			Error: fmt.Sprintf("no registration for %s; register before opening a tunnel", key),
			Code:  "NOT_REGISTERED",
		})
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "tunnel upgrade is not supported by this server",
			Code:  "UPGRADE_UNSUPPORTED",
		})
		return
	}
	conn, brw, err := hijacker.Hijack()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: fmt.Sprintf("hijacking the tunnel connection: %v", err),
			Code:  "UPGRADE_FAILED",
		})
		return
	}

	// The hub control-plane server sets ReadTimeout (15s); that deadline is
	// still armed on this conn and would kill the session a quarter-minute in.
	// Clear it: from here the session's own keepalive and the per-dial
	// deadlines are what bound anything.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		s.logger.Error("clearing the tunnel connection deadline", "project", key, "error", err)
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: "+tunnelUpgradeProtocol+"\r\n"+
		"Connection: Upgrade\r\n\r\n"); err != nil {
		_ = conn.Close()
		s.logger.Warn("writing the tunnel upgrade response", "project", key, "error", err)
		return
	}

	// Read through the hijacked bufio.Reader rather than the raw conn: Hijack
	// may hand back bytes the HTTP server already buffered, and dropping them
	// would desynchronize the yamux framing on the very first frame.
	sess, err := yamux.Server(&hijackedConn{Conn: conn, r: brw.Reader}, tunnelYamuxConfig())
	if err != nil {
		_ = conn.Close()
		s.logger.Error("starting the tunnel session", "project", key, "error", err)
		return
	}

	s.attachTunnel(key, sess)
}

// attachTunnel installs a freshly negotiated session, marks the registration
// connected, and arms the close watcher. Split out of handleHubTunnel so tests
// can attach a session without a real HTTP upgrade.
func (s *Server) attachTunnel(key string, sess *yamux.Session) *tunnelSession {
	attached, replaced := s.tunnels.attach(key, sess)

	// A second tunnel for the same key REPLACES the first (D17). The replaced
	// session is closed here, outside the manager's lock, and its own close
	// callback will find a newer generation installed and do nothing (P2).
	if replaced != nil {
		s.logger.Info("replacing an existing tunnel session",
			"project", key, "old_generation", replaced.gen, "new_generation", attached.gen)
		replaced.close()
	}

	// MarkConnected fails only when the registration is gone — deregistered,
	// swept, or displaced between the handler's HasProject check and this
	// attach. A session with nothing to serve is a leak that would sit in the
	// manager until `hub stop`, so drop it and let the publisher re-register.
	if s.registry != nil && !s.registry.MarkConnected(key, attached.gen) {
		if s.tunnels.detach(key, attached.gen) {
			attached.close()
		}
		s.logger.Info("dropped a tunnel whose registration is gone", "project", key, "generation", attached.gen)
		return attached
	}
	s.logger.Info("tunnel attached", "project", key, "generation", attached.gen)

	go s.watchTunnel(attached)
	return attached
}

// watchTunnel marks the registration disconnected when a session ends — but
// only when the session that ended is STILL the installed one (P2). A session
// that was replaced fails the detach gate and its callback is ignored entirely,
// so a slow close cannot disconnect its own successor.
func (s *Server) watchTunnel(t *tunnelSession) {
	<-t.sess.CloseChan()
	t.transport.CloseIdleConnections()
	if !s.tunnels.detach(t.key, t.gen) {
		// Either a newer generation is installed, or the session was already
		// removed deliberately (takeover, hub stop). Nothing to mark.
		return
	}
	if s.registry != nil {
		s.registry.MarkDisconnected(t.key, t.gen, time.Now())
	}
	s.logger.Info("tunnel detached", "project", t.key, "generation", t.gen)
}

// tunnelRef names one session precisely: its key AND the generation it was
// attached under (plan 031, review B1). A key alone is not an identity — a
// publisher that reconnects gets a new generation under the same key — so every
// deliberate close carries both.
type tunnelRef struct {
	key string
	gen uint64
}

// closeTunnelGen removes and closes key's session only when the installed
// session is still the generation gen names, and reports whether it did.
//
// The generation is the whole point (plan 031, review B1). A deregister or a
// takeover decides to close a tunnel while holding lifecycleMu and performs the
// close after releasing it (D17: the session mutex is a leaf and a close is
// I/O), and in that gap the publisher can re-register and reattach. Closing by
// key would then kill the session belonging to the registration that replaced
// the one this call removed. A gen of 0 — a registration that never attached —
// matches no installed session and closes nothing.
//
// Callers must NOT hold lifecycleMu or the registry lock.
func (s *Server) closeTunnelGen(key string, gen uint64) bool {
	t := s.tunnels.removeGen(key, gen)
	if t == nil {
		return false
	}
	t.close()
	return true
}

// closeAllTunnels tears every session down (`prox hub stop`, teardown).
func (s *Server) closeAllTunnels() {
	for _, t := range s.tunnels.removeAll() {
		t.close()
	}
}

// hijackedConn is a net.Conn whose reads come from the bufio.Reader that
// Hijack returned, so bytes the HTTP server had already buffered are consumed
// before the socket is read directly.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// headerHasToken reports whether a comma-separated header value contains token
// (case-insensitively), which is how Connection: keep-alive, Upgrade must be
// read — an exact string compare would reject a perfectly valid client.
func headerHasToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// --- the offline page ---

// tunnelOfflineHTML is the body served for a hub route whose publisher is not
// reachable. It is deliberately tiny and self-contained: it is served by a
// daemon with no template assets, to a browser that just failed to reach
// someone's dev server.
const tunnelOfflineHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>503 — publisher offline</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>Publisher offline</h1>
<p><code>%s</code> is published through a prox hub, but its publisher is not connected.</p>
<p>%s</p>
<p>This page refreshes nothing; retry in a few seconds.</p>
</body></html>
`

// writeTunnelOffline serves the hub's 503 for an unreachable publisher (plan
// 031 C4). Retry-After: 5 tells a well-behaved client (and any load balancer in
// front of it) that this is transient — the publisher is expected back — rather
// than a permanent failure.
func writeTunnelOffline(w http.ResponseWriter, hostname string, since time.Time) {
	detail := "The publisher has not connected its tunnel yet."
	if !since.IsZero() {
		detail = fmt.Sprintf("The publisher went offline at %s (%s ago).",
			since.Format(time.RFC1123), time.Since(since).Truncate(time.Second))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, tunnelOfflineHTML, html.EscapeString(hostname), html.EscapeString(detail))
}
