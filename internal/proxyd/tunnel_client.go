package proxyd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/hashicorp/yamux"
)

// RunTunnel is the PUBLISHER half of the reverse tunnel (plan 031 D2/D11). It
// holds one outbound connection to the hub and serves whatever the hub asks for
// over it, reconnecting forever until ctx is cancelled.
//
// key is the composed registry key (HubProjectKey(origin, dir)) the project
// registered under; the two halves are recovered from it for the upgrade
// headers, so the tunnel can never address a different registration than the
// register call did — the §8 "publisher must use the key consistently" risk is
// closed by construction rather than by discipline.
//
// allowed is the project's OWN service map and is the ENTIRE set of targets
// this publisher will dial (D11). Anything else the hub asks for is refused
// with ERR. This is what stops a hub — compromised, misconfigured, or simply
// buggy — from becoming a general port-forwarder into the publisher's machine
// or its private network.
//
// reregister (may be nil) is called when the hub answers 404 NOT_REGISTERED:
// the registration is gone (swept after a long outage, or displaced), so
// retrying the handshake alone would loop forever. It is the callback that
// makes that rule implementable at all.
//
// sink (may be nil) receives the same connect/failure signals the SSE forwarder
// reports, so `prox status` can show the tunnel's state.
func RunTunnel(
	ctx context.Context,
	client *Client,
	key string,
	allowed map[string]ServiceTarget,
	reregister func() error,
	sink ForwarderStatusSink,
) {
	runTunnel(ctx, tunnelClientConfig{
		client:          client,
		key:             key,
		allowed:         allowed,
		reregister:      reregister,
		sink:            sink,
		after:           time.After,
		dialTarget:      dialTunnelTarget,
		baseBackoff:     constants.StreamReconnectBaseBackoff,
		maxBackoff:      constants.StreamReconnectMaxBackoff,
		preambleTimeout: constants.HubDialTimeout,
	})
}

// tunnelClientConfig bundles RunTunnel's inputs plus the injectable timer,
// dialer, and backoff bounds, mirroring forwarderConfig so the reconnect and
// refusal paths can be tested without real wall-clock waits.
type tunnelClientConfig struct {
	client     *Client
	key        string
	allowed    map[string]ServiceTarget
	reregister func() error
	sink       ForwarderStatusSink

	after      func(time.Duration) <-chan time.Time
	dialTarget func(ctx context.Context, address string) (net.Conn, error)
	// preambleTimeout bounds how long the publisher waits for the hub's
	// CONNECT line on a freshly accepted stream. A stream the hub opens and
	// then says nothing on must not pin a goroutine forever.
	preambleTimeout         time.Duration
	baseBackoff, maxBackoff time.Duration
}

// dialTunnelTarget is the production backend dialer: a plain TCP dial to the
// publisher's own service, bounded like every other proxy dial.
func dialTunnelTarget(ctx context.Context, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: constants.DefaultProxyDialTimeout, KeepAlive: constants.DefaultProxyKeepAlive}
	return d.DialContext(ctx, "tcp", address)
}

// allowedTargets renders the D11 allow-list as the exact "host:port" strings a
// CONNECT preamble must match. The comparison is deliberately literal: the hub
// received these targets verbatim in the register request and echoes them back,
// so a mismatch means the hub asked for something this project never published.
func (cfg tunnelClientConfig) allowedTargets() map[string]struct{} {
	set := make(map[string]struct{}, len(cfg.allowed))
	for _, t := range cfg.allowed {
		set[net.JoinHostPort(t.Host, strconv.Itoa(t.Port))] = struct{}{}
	}
	return set
}

// runTunnel is RunTunnel's loop: connect, serve until the session ends, back
// off, repeat. It is single-goroutine by construction — the publisher's state
// machine (D19) owns it — so there is no locking here at all.
func runTunnel(ctx context.Context, cfg tunnelClientConfig) {
	allowed := cfg.allowedTargets()
	backoff := cfg.baseBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		connected, err := cfg.runSession(ctx, allowed)
		if ctx.Err() != nil {
			return
		}

		switch {
		case connected:
			// The session was live and has now ended; reconnect eagerly rather
			// than carrying a stale outage-sized backoff into the recovery.
			backoff = cfg.baseBackoff
		default:
			if cfg.sink != nil {
				cfg.sink.ForwarderConnectFailed(err)
			}
			// 404 NOT_REGISTERED means the hub has no registration for this
			// key at all: retrying the upgrade can never succeed, so re-register
			// first and then reconnect eagerly (D19).
			if isNotRegistered(err) && cfg.reregister != nil {
				if rerr := cfg.reregister(); rerr == nil {
					backoff = cfg.baseBackoff
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-cfg.after(backoff):
		}
		if backoff < cfg.maxBackoff {
			backoff *= 2
			if backoff > cfg.maxBackoff {
				backoff = cfg.maxBackoff
			}
		}
	}
}

// isNotRegistered reports whether err is the hub's 404 NOT_REGISTERED.
func isNotRegistered(err error) bool {
	var apiErr *DaemonAPIError
	return errors.As(err, &apiErr) && apiErr.Code == "NOT_REGISTERED"
}

// runSession performs one upgrade and serves streams until the session ends. It
// reports connected=true once the 101 was received, so the caller can tell a
// failed connect (count it, back off) from a session that lived and then
// dropped (reconnect eagerly) — the same distinction the SSE forwarder makes.
func (cfg tunnelClientConfig) runSession(ctx context.Context, allowed map[string]struct{}) (connected bool, err error) {
	conn, err := cfg.dialHub(ctx)
	if err != nil {
		return false, err
	}

	sess, err := yamux.Client(conn, tunnelYamuxConfig())
	if err != nil {
		_ = conn.Close()
		return false, fmt.Errorf("starting the tunnel session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	if cfg.sink != nil {
		cfg.sink.ForwarderConnected()
	}

	// Cancelling ctx must tear the session down, not merely stop new streams:
	// AcceptStream blocks on the session, so closing it is the only way out.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	// Every in-flight stream must finish before this function returns, so a
	// cancelled run does not leave copies writing into a closed session.
	defer wg.Wait()

	for {
		stream, aerr := sess.AcceptStream()
		if aerr != nil {
			return true, aerr
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg.serveStream(ctx, stream, allowed)
		}()
	}
}

// dialHub performs the HTTP/1.1 upgrade handshake and returns the raw
// connection underneath it (plan 031 §4.3).
//
// The upgrade goes through the Client's UNBOUNDED http.Client: its bounded twin
// carries a 30s whole-request timeout that covers reading the body, which for a
// 101 response IS the tunnel — routing the upgrade through it would sever every
// tunnel after 30 seconds (P1/D16).
func (cfg tunnelClientConfig) dialHub(ctx context.Context) (io.ReadWriteCloser, error) {
	origin, dir, ok := splitHubProjectKey(cfg.key)
	if !ok {
		return nil, fmt.Errorf("malformed hub project key %q", cfg.key)
	}

	headers := http.Header{}
	headers.Set("Connection", "Upgrade")
	headers.Set("Upgrade", tunnelUpgradeProtocol)
	headers.Set("X-Prox-Origin", origin)
	headers.Set("X-Prox-Project-Dir", dir)

	resp, err := cfg.client.streamUpgrade(ctx, http.MethodPost, "/api/v1/tunnel", headers)
	if err != nil {
		return nil, fmt.Errorf("opening the hub tunnel: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, cfg.client.readError(resp)
	}

	// net/http hands back the raw connection as the body of a 101 response,
	// which is precisely the contract this handshake needs (and the same one
	// every WebSocket client over net/http relies on).
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errors.New("hub accepted the upgrade but the connection is not writable")
	}
	return conn, nil
}

// serveStream handles one proxied connection: read and validate the preamble,
// enforce the D11 allow-list, dial, answer, and pipe.
func (cfg tunnelClientConfig) serveStream(ctx context.Context, stream net.Conn, allowed map[string]struct{}) {
	closeStream := true
	defer func() {
		if closeStream {
			_ = stream.Close()
		}
	}()

	// Framing is strict on BOTH ends (D2): an explicit deadline so a stream the
	// hub never writes on cannot pin this goroutine, plus the shared byte cap
	// and character rules.
	if err := stream.SetReadDeadline(time.Now().Add(cfg.preambleTimeout)); err != nil {
		return
	}
	line, err := readPreambleLine(stream, tunnelPreambleMaxBytes)
	if err != nil {
		// No usable preamble means no usable reply channel either: the hub's
		// own dial deadline is what fails this attempt.
		return
	}
	host, port, err := parseConnectLine(line)
	if err != nil {
		cfg.replyErr(stream, "malformed preamble")
		return
	}

	address := net.JoinHostPort(host, strconv.Itoa(port))
	if _, ok := allowed[address]; !ok {
		// D11. The hub asked for something this project never registered;
		// refusing is the difference between a reverse proxy and an open port
		// forwarder into the publisher's network.
		cfg.replyErr(stream, "target not registered by this publisher")
		return
	}

	backend, err := cfg.dialTarget(ctx, address)
	if err != nil {
		cfg.replyErr(stream, "backend unavailable")
		return
	}

	if err := stream.SetDeadline(time.Time{}); err != nil {
		_ = backend.Close()
		return
	}
	if _, err := io.WriteString(stream, tunnelReplyOK+"\n"); err != nil {
		_ = backend.Close()
		return
	}

	closeStream = false // pipe owns both ends from here
	pipeConns(stream, backend)
}

// replyErr sends the one-line refusal. The reason is always a fixed, framing-safe
// string chosen here — never anything derived from the peer's bytes, which is
// what keeps a hostile preamble from smuggling a newline back across the wire.
func (cfg tunnelClientConfig) replyErr(stream net.Conn, reason string) {
	_ = stream.SetWriteDeadline(time.Now().Add(cfg.preambleTimeout))
	_, _ = io.WriteString(stream, tunnelReplyErr+" "+reason+"\n")
}

// pipeConns copies bytes both ways until both directions finish, then closes
// both ends.
//
// The asymmetry is deliberate. When the hub's half of the stream ends (request
// body complete, or the hub's transport dropped the connection), the BACKEND is
// half-closed with CloseWrite so it sees a clean end-of-request and can still
// write its response — tearing the whole pair down at the first EOF is the
// classic broken-proxy truncation bug. When the backend's response ends, the
// stream is closed outright, because a yamux stream cannot be half-closed for
// writing only: its Close returns EOF to local reads once the buffer drains. By
// then the response is fully copied, so there is nothing left to truncate.
func pipeConns(stream, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, stream)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, backend)
		_ = stream.Close()
	}()
	wg.Wait()
	_ = stream.Close()
	_ = backend.Close()
}

// closeWrite half-closes c's write side when it can, so the peer sees a clean
// EOF while the other direction keeps flowing. Falls back to a full close for a
// conn that cannot half-close.
func closeWrite(c net.Conn) {
	type writeCloser interface{ CloseWrite() error }
	if wc, ok := c.(writeCloser); ok {
		_ = wc.CloseWrite()
		return
	}
	_ = c.Close()
}
