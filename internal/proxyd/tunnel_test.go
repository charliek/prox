package proxyd

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests live in internal/proxyd rather than test/integration on purpose
// (plan 031 P14): fakeCertManager is unexported here, and test/integration's
// startDaemonServer builds no dynamic proxy and no cert manager, so an
// end-to-end "HTTPS listener → tunnel → backend" test simply cannot be written
// over there.

// testClock is the injected clock the hub lease tests drive. It is mutex-guarded
// rather than a bare field because the registry reads it from whatever goroutine
// happens to be registering or sweeping, and a plain assignment would be a data
// race the -race tests would (rightly) fail on.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Now()} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// tunnelHub is a complete hub for testing: a Server with hub mode on a loopback
// ephemeral CONTROL-PLANE port, a real DynamicProxy over a fake cert manager,
// and a real HTTPS DATA-PLANE listener on a genuinely bound port.
//
// The data-plane port is a real port read back from freePort, never 0: on this
// surface 0 already means "no HTTP(S) listener", and overloading it to mean
// "ephemeral" is the confusion P15 calls out.
type tunnelHub struct {
	server    *Server
	proxy     *DynamicProxy
	registry  *Registry
	clock     *testClock
	baseURL   string
	domain    string
	httpsPort int
	token     string
}

func newTunnelHub(t *testing.T) *tunnelHub {
	t.Helper()
	// Isolated HOME: StartHub may write ~/.prox/hub.token, and no test may
	// touch the developer's own daemon files.
	t.Setenv("HOME", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	clock := newTestClock()
	reg := NewRegistry()
	reg.now = clock.now
	ms := NewManagers(100, nil)
	dp := NewDynamicProxy(reg, newFakeCertManager(), ms, nil, logger)

	s := NewServer(ServerConfig{SocketPath: "", Logger: logger, Version: "test"})
	s.SetRegistry(reg)
	s.SetProxy(dp)
	s.SetManagers(ms)
	dp.SetTunnelSessions(s.tunnels)

	h := &tunnelHub{
		server:    s,
		proxy:     dp,
		registry:  reg,
		clock:     clock,
		domain:    "llt.test",
		httpsPort: freePort(t),
		token:     "hub-token",
	}
	require.NoError(t, s.StartHub(HubConfig{
		Domain:    h.domain,
		Listen:    "127.0.0.1:0",
		HTTPSPort: h.httpsPort,
		Auth:      HubAuthToken,
		Token:     h.token,
	}))
	h.baseURL = "http://" + s.HubListenAddr()
	t.Cleanup(func() {
		s.StopHub()
		_ = dp.Shutdown(context.Background())
	})
	return h
}

func (h *tunnelHub) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewHTTPClient(h.baseURL, h.token)
	require.NoError(t, err)
	return c
}

// registerBody builds a well-formed network register request. Domain and ports
// are deliberately "wrong": the hub owns both and overwrites them (D4).
func registerBody(origin, dir string, services map[string]ServiceTarget, takeover bool) RegisterRequest {
	return RegisterRequest{
		ProjectDir:      dir,
		PID:             os.Getpid(),
		Origin:          origin,
		ProtocolVersion: constants.HubProtocolVersion,
		Domain:          "local.publisher.test",
		HTTPSPort:       9999,
		Services:        services,
		Takeover:        takeover,
	}
}

func (h *tunnelHub) mustRegister(t *testing.T, origin, dir string, services map[string]ServiceTarget) {
	t.Helper()
	_, err := h.client(t).Register(registerBody(origin, dir, services, false))
	require.NoError(t, err, "register %s:%s with the hub", origin, dir)
}

// startPublisher registers a project and runs its tunnel client, returning the
// composed key and a stop function that tears the tunnel down and waits for it.
//
// allowed is passed to RunTunnel separately from services so a test can make
// the publisher refuse a target the hub will legitimately ask for (D11).
func (h *tunnelHub) startPublisher(
	t *testing.T,
	origin, dir string,
	services, allowed map[string]ServiceTarget,
) (key string, stop func()) {
	t.Helper()
	h.mustRegister(t, origin, dir, services)
	return h.startTunnelOnly(t, origin, dir, allowed)
}

// startTunnelOnly runs a tunnel client for an ALREADY registered project.
func (h *tunnelHub) startTunnelOnly(t *testing.T, origin, dir string, allowed map[string]ServiceTarget) (key string, stop func()) {
	t.Helper()
	key = HubProjectKey(origin, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunTunnel(ctx, h.client(t), key, allowed, nil, nil)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	h.waitAttached(t, key)
	return key, stop
}

func (h *tunnelHub) waitAttached(t *testing.T, key string) {
	t.Helper()
	require.Eventually(t, func() bool { return h.server.tunnels.get(key) != nil },
		5*time.Second, 2*time.Millisecond, "tunnel for %s never attached", key)
}

func (h *tunnelHub) waitDetached(t *testing.T, key string) {
	t.Helper()
	require.Eventually(t, func() bool { return h.server.tunnels.get(key) == nil },
		5*time.Second, 2*time.Millisecond, "tunnel for %s never detached", key)
}

// httpsClient dials the hub's data-plane port whatever hostname the URL names,
// so a request to https://svc.llt.test:<port>/ carries the right SNI and Host
// without touching DNS or /etc/hosts.
func (h *tunnelHub) httpsClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", h.dataPlaneAddr())
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 15 * time.Second,
	}
}

func (h *tunnelHub) dataPlaneAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(h.httpsPort))
}

func (h *tunnelHub) url(service, path string) string {
	return fmt.Sprintf("https://%s.%s:%d%s", service, h.domain, h.httpsPort, path)
}

// get issues one data-plane request and returns the status and body.
func (h *tunnelHub) get(t *testing.T, client *http.Client, service, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := client.Get(h.url(service, path))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body), resp.Header
}

// tcpPipe returns a connected pair of real TCP conns. A real socket pair rather
// than net.Pipe: net.Pipe is unbuffered and synchronous, which is a poor stand-in
// for the connection yamux actually runs over.
func tcpPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- accepted{c, aerr}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	got := <-ch
	require.NoError(t, got.err)
	t.Cleanup(func() {
		_ = client.Close()
		_ = got.conn.Close()
	})
	return client, got.conn
}

// attachStubTunnel installs a yamux session for key directly into the hub's
// session manager, returning the hub-side session record and the PUBLISHER-side
// yamux session the test drives by hand. It bypasses the HTTP upgrade so a test
// can make the publisher behave in ways a real one never would — including not
// answering at all.
func attachStubTunnel(t *testing.T, h *tunnelHub, key string) (*tunnelSession, *yamux.Session) {
	t.Helper()
	pubConn, hubConn := tcpPipe(t)
	hubSess, err := yamux.Server(hubConn, tunnelYamuxConfig())
	require.NoError(t, err)
	pubSess, err := yamux.Client(pubConn, tunnelYamuxConfig())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pubSess.Close()
		_ = hubSess.Close()
	})
	return h.server.attachTunnel(key, hubSess), pubSess
}

// --- end-to-end ---

// TestTunnel_EndToEndThroughHubHTTPS is the headline path: a real HTTPS request
// to the hub's data-plane port is proxied over the publisher's reverse tunnel to
// a backend on the publisher's own machine, and arrives with the forwarding
// headers the local path sets.
func TestTunnel_EndToEndThroughHubHTTPS(t *testing.T) {
	var gotForwardedHost, gotForwardedProto string
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotForwardedHost = r.Header.Get("X-Forwarded-Host")
		gotForwardedProto = r.Header.Get("X-Forwarded-Proto")
		_, _ = io.WriteString(w, "hello from the publisher")
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	h.startPublisher(t, "shed", "/home/dev/app", services, services)

	status, body, _ := h.get(t, h.httpsClient(), "web", "/")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "hello from the publisher", body)
	assert.Equal(t, fmt.Sprintf("web.%s:%d", h.domain, h.httpsPort), gotForwardedHost,
		"the publisher's backend must see the hub hostname it was reached by")
	assert.Equal(t, "https", gotForwardedProto)
}

// TestTunnel_WebSocketEchoThroughTunnel pins that a hijacked, bidirectional
// upgrade survives the tunnel. The handshake and the echo are hand-rolled:
// golang.org/x/net/websocket is not a dependency of this repo and must not
// become one for a test.
func TestTunnel_WebSocketEchoThroughTunnel(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		// "Frames" are newline-delimited here; the point of the test is the
		// hijacked byte pipe, not RFC 6455 framing.
		scanner := bufio.NewScanner(brw.Reader)
		for scanner.Scan() {
			if _, err := fmt.Fprintf(conn, "echo:%s\n", scanner.Text()); err != nil {
				return
			}
		}
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"ws": {Host: backendHost, Port: backendPort}}
	h.startPublisher(t, "shed", "/home/dev/ws", services, services)

	conn, err := tls.Dial("tcp", h.dataPlaneAddr(), &tls.Config{
		ServerName:         "ws." + h.domain,
		InsecureSkipVerify: true,
	})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	_, err = fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s.%s\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		"ws", h.domain)
	require.NoError(t, err)

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, statusLine, "101", "the upgrade must reach the backend through the tunnel")
	for {
		line, herr := br.ReadString('\n')
		require.NoError(t, herr)
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	for _, msg := range []string{"one", "two", "three"} {
		_, err = fmt.Fprintf(conn, "%s\n", msg)
		require.NoError(t, err)
		echoed, rerr := br.ReadString('\n')
		require.NoError(t, rerr)
		assert.Equal(t, "echo:"+msg+"\n", echoed)
	}
}

// TestTunnel_SSEThroughTunnel pins that a long-lived streaming response is
// delivered incrementally through the tunnel rather than buffered until the
// backend finishes — the failure mode a naive tunnel would have.
func TestTunnel_SSEThroughTunnel(t *testing.T) {
	release := make(chan struct{})
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			_, _ = fmt.Fprintf(w, "data: event-%d\n\n", i)
			flusher.Flush()
		}
		// Hold the response open: if the events only arrived at the end, the
		// reader below would block here and fail the test.
		<-release
	})
	defer close(release)

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"sse": {Host: backendHost, Port: backendPort}}
	h.startPublisher(t, "shed", "/home/dev/sse", services, services)

	client := h.httpsClient()
	resp, err := client.Get(h.url("sse", "/events"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	br := bufio.NewReader(resp.Body)
	for i := 1; i <= 3; i++ {
		line, rerr := br.ReadString('\n')
		require.NoError(t, rerr, "event %d should arrive while the response is still open", i)
		assert.Equal(t, fmt.Sprintf("data: event-%d\n", i), line)
		blank, rerr := br.ReadString('\n')
		require.NoError(t, rerr)
		assert.Equal(t, "\n", blank)
	}
}

// TestTunnel_KeepAliveReusesOneStream pins that the per-session http.Transport
// actually pools tunnel streams: three sequential requests must reach the
// publisher's backend over ONE connection, which can only happen if the hub
// reused one yamux stream rather than paying a CONNECT per request.
func TestTunnel_KeepAliveReusesOneStream(t *testing.T) {
	var newConns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	backendHost, backendPortStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	backendPort, err := strconv.Atoi(backendPortStr)
	require.NoError(t, err)

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}}
	h.startPublisher(t, "shed", "/home/dev/api", services, services)

	client := h.httpsClient()
	for i := 0; i < 3; i++ {
		status, body, _ := h.get(t, client, "api", "/")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "ok", body)
	}
	assert.Equal(t, int64(1), newConns.Load(),
		"three keep-alive requests must reuse one tunnel stream, not open three")
}

// TestTunnel_UnregisteredTargetRefused is D11 end to end: the publisher refuses
// a CONNECT for a target it never registered, and the hub turns that ERR into
// the offline 503 rather than proxying to it.
func TestTunnel_UnregisteredTargetRefused(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should never be reached")
	})

	h := newTunnelHub(t)
	registered := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	// The publisher's allow-list names a DIFFERENT port, so the CONNECT the hub
	// legitimately sends for the registered route is refused.
	allowed := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort + 1}}
	h.startPublisher(t, "shed", "/home/dev/app", registered, allowed)

	status, body, headers := h.get(t, h.httpsClient(), "web", "/")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "5", headers.Get("Retry-After"))
	assert.Contains(t, body, "Publisher offline")
}

// TestTunnel_PreambleHardening is the framing table (plan 031 D2). Both ends
// apply these rules, so one table covers both.
func TestTunnel_PreambleHardening(t *testing.T) {
	t.Run("connect line", func(t *testing.T) {
		tests := []struct {
			name     string
			line     string
			wantHost string
			wantPort int
			wantErr  string
		}{
			{name: "plain host", line: "CONNECT localhost:3000", wantHost: "localhost", wantPort: 3000},
			{name: "ipv4 literal", line: "CONNECT 127.0.0.1:8080", wantHost: "127.0.0.1", wantPort: 8080},
			{name: "bracketed ipv6 literal", line: "CONNECT [::1]:3000", wantHost: "::1", wantPort: 3000},
			{name: "bracketed ipv6 with zone", line: "CONNECT [fe80::1%eth0]:80", wantHost: "fe80::1%eth0", wantPort: 80},
			{name: "oversized", line: "CONNECT " + strings.Repeat("a", tunnelPreambleMaxBytes) + ":80", wantErr: "exceeds"},
			{name: "embedded newline", line: "CONNECT localhost:3000\nCONNECT evil:1", wantErr: "embedded newline"},
			{name: "embedded carriage return", line: "CONNECT localhost:3000\r", wantErr: "embedded newline"},
			{name: "NUL byte", line: "CONNECT local\x00host:3000", wantErr: "NUL byte"},
			{name: "control character", line: "CONNECT local\thost:3000", wantErr: "control character"},
			{name: "wrong verb", line: "GET localhost:3000", wantErr: "malformed tunnel preamble"},
			{name: "no port", line: "CONNECT localhost", wantErr: "malformed tunnel target"},
			{name: "unbracketed ipv6", line: "CONNECT ::1:3000", wantErr: "malformed tunnel target"},
			{name: "empty host", line: "CONNECT :3000", wantErr: "no host"},
			{name: "non-numeric port", line: "CONNECT localhost:http", wantErr: "invalid port"},
			{name: "port out of range", line: "CONNECT localhost:70000", wantErr: "invalid port"},
			{name: "port zero", line: "CONNECT localhost:0", wantErr: "invalid port"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				host, port, err := parseConnectLine(tt.line)
				if tt.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tt.wantErr)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tt.wantHost, host)
				assert.Equal(t, tt.wantPort, port)
				// Round trip: what the hub would write parses back identically.
				rt := strings.TrimSuffix(formatConnectLine(host, port), "\n")
				rtHost, rtPort, rerr := parseConnectLine(rt)
				require.NoError(t, rerr)
				assert.Equal(t, host, rtHost)
				assert.Equal(t, port, rtPort)
			})
		}
	})

	t.Run("reply line", func(t *testing.T) {
		require.NoError(t, parseTunnelReply("OK"))
		require.ErrorContains(t, parseTunnelReply("ERR target not registered"), "target not registered")
		require.ErrorContains(t, parseTunnelReply("ERR"), "no reason given")
		require.ErrorContains(t, parseTunnelReply("okay"), "malformed tunnel reply")
		require.ErrorContains(t, parseTunnelReply("OK\x00"), "NUL byte")
		require.ErrorContains(t, parseTunnelReply(strings.Repeat("O", tunnelPreambleMaxBytes+1)), "exceeds")
	})

	t.Run("reader enforces the byte cap", func(t *testing.T) {
		// A peer that never terminates its line must fail rather than make the
		// reader buffer without bound.
		r := strings.NewReader(strings.Repeat("x", tunnelPreambleMaxBytes*4))
		_, err := readPreambleLine(r, tunnelPreambleMaxBytes)
		require.ErrorContains(t, err, "no line terminator")

		line, err := readPreambleLine(strings.NewReader("CONNECT localhost:3000\nleftover"), tunnelPreambleMaxBytes)
		require.NoError(t, err)
		assert.Equal(t, "CONNECT localhost:3000", line, "the reader must not consume past the terminator")

		_, err = readPreambleLine(strings.NewReader("unterminated"), tunnelPreambleMaxBytes)
		require.ErrorContains(t, err, "without a line terminator")
	})
}

// TestTunnel_CloseThenReconnect walks D3's grace: the tunnel closes, the route
// survives and serves the offline page, and a reattach restores service with no
// re-register and no route churn.
func TestTunnel_CloseThenReconnect(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "alive")
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	key, stop := h.startPublisher(t, "shed", "/home/dev/app", services, services)

	client := h.httpsClient()
	status, body, _ := h.get(t, client, "web", "/")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "alive", body)

	stop()
	h.waitDetached(t, key)

	// Inside the grace the route is still registered and the hub answers with
	// the offline page, not a 404.
	require.True(t, h.registry.HasProject(key), "the registration must survive the disconnect grace")
	status, body, headers := h.get(t, client, "web", "/")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "5", headers.Get("Retry-After"))
	assert.Contains(t, body, "Publisher offline")

	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.False(t, routes[0].Connected, "a disconnected hub route must report connected=false")

	// Reattach with no re-register.
	h.startTunnelOnly(t, "shed", "/home/dev/app", services)
	status, body, _ = h.get(t, client, "web", "/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "alive", body)
	routes = h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.True(t, routes[0].Connected)
}

// TestTunnel_GraceExpiryRemovesRoute drives the lease with the injected clock:
// nothing is removed inside the grace, and the route is gone once it expires.
func TestTunnel_GraceExpiryRemovesRoute(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	key, stop := h.startPublisher(t, "shed", "/home/dev/app", services, services)

	stop()
	h.waitDetached(t, key)
	require.Eventually(t, func() bool {
		lease, ok := h.registry.RemoteLease(key)
		return ok && !lease.DisconnectedAt.IsZero()
	}, 5*time.Second, 2*time.Millisecond, "the registration must be marked disconnected")

	h.clock.advance(constants.HubDisconnectGrace - time.Second)
	assert.Empty(t, h.registry.ExpiredLeases(), "nothing expires inside the grace")
	assert.True(t, h.registry.HasProject(key))

	h.clock.advance(2 * time.Second)
	expired := h.registry.ExpiredLeases()
	require.Len(t, expired, 1)
	assert.Equal(t, key, expired[0].Key)

	removed, hostnames, _ := h.server.removeDisconnectedRemote(expired[0].Key, expired[0].SessionGen)
	require.True(t, removed)
	assert.Equal(t, []string{"web." + h.domain}, hostnames)
	assert.False(t, h.registry.HasProject(key), "an expired lease removes the registration")
	assert.Empty(t, h.registry.AllRoutes())
}

// TestDecideLease is the pure lease decision's table, mirroring decideProbe's
// treatment of the on-502 gate.
func TestDecideLease(t *testing.T) {
	base := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	const grace = 60 * time.Second

	tests := []struct {
		name           string
		now            time.Time
		disconnectedAt time.Time
		reservedUntil  time.Time
		sessionGen     uint64
		currentGen     uint64
		want           bool
	}{
		{
			name:       "connected registration is never removed",
			now:        base.Add(time.Hour),
			sessionGen: 3, currentGen: 3,
		},
		{
			name:           "disconnected but inside the grace",
			now:            base.Add(30 * time.Second),
			disconnectedAt: base,
			sessionGen:     3, currentGen: 3,
		},
		{
			name:           "disconnected exactly at the grace boundary",
			now:            base.Add(grace),
			disconnectedAt: base,
			sessionGen:     3, currentGen: 3,
			want: true,
		},
		{
			name:           "disconnected well past the grace",
			now:            base.Add(10 * time.Minute),
			disconnectedAt: base,
			sessionGen:     3, currentGen: 3,
			want: true,
		},
		{
			name:           "reattached under a newer generation survives (P2)",
			now:            base.Add(10 * time.Minute),
			disconnectedAt: base,
			sessionGen:     3, currentGen: 4,
		},
		{
			name:           "a stale observation of an OLDER generation is refused",
			now:            base.Add(10 * time.Minute),
			disconnectedAt: base,
			sessionGen:     5, currentGen: 4,
		},
		{
			name:           "attach reservation still running blocks removal (P3)",
			now:            base.Add(10 * time.Minute),
			disconnectedAt: base,
			reservedUntil:  base.Add(20 * time.Minute),
			sessionGen:     3, currentGen: 3,
		},
		{
			name:           "expired reservation does not block removal",
			now:            base.Add(10 * time.Minute),
			disconnectedAt: base,
			reservedUntil:  base.Add(time.Minute),
			sessionGen:     3, currentGen: 3,
			want: true,
		},
		{
			name:       "never connected, never disconnected, is not swept here",
			now:        base.Add(time.Hour),
			sessionGen: 0, currentGen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideLease(tt.now, tt.disconnectedAt, tt.reservedUntil, tt.sessionGen, tt.currentGen, grace)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTunnel_SecondSessionReplacesFirst pins D17's replacement rule: the newer
// tunnel wins, the older one is closed, and the registration stays connected
// throughout.
func TestTunnel_SecondSessionReplacesFirst(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "alive")
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	key := HubProjectKey("shed", "/home/dev/app")
	h.mustRegister(t, "shed", "/home/dev/app", services)

	first, _ := attachStubTunnel(t, h, key)
	second, _ := attachStubTunnel(t, h, key)

	assert.Greater(t, second.gen, first.gen, "generations must be monotonic")
	assert.Same(t, second, h.server.tunnels.get(key), "the newer session must be the installed one")
	require.Eventually(t, first.sess.IsClosed, 5*time.Second, 2*time.Millisecond,
		"the replaced session must be closed")

	lease, ok := h.registry.RemoteLease(key)
	require.True(t, ok)
	assert.Equal(t, second.gen, lease.SessionGen)
	assert.True(t, lease.DisconnectedAt.IsZero(), "a replacement is not a disconnect")
}

// --- D10 collisions ---

// TestHubCollision_ActiveHolderRefusesNewcomer covers the core of D10: a name
// held by a connected publisher needs consent, the 409 names every conflicting
// holder, and nothing about the incumbent changes.
func TestHubCollision_ActiveHolderRefusesNewcomer(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	h.startPublisher(t, "alpha", "/home/dev/app", services, services)

	_, err := h.client(t).Register(registerBody("beta", "/other/app", services, false))
	require.Error(t, err)
	apiErr := requireAPIError(t, err)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, "HUB_NAME_HELD", apiErr.Code)

	holders := decodeHolders(t, h, registerBody("beta", "/other/app", services, false))
	require.Len(t, holders, 1)
	assert.Equal(t, "web."+h.domain, holders[0].Hostname)
	assert.Equal(t, "alpha", holders[0].Origin)
	assert.Equal(t, "/home/dev/app", holders[0].ProjectDir, "the holder's OWN dir, not the composed key")
	assert.True(t, holders[0].Connected)

	// The incumbent is untouched and still the route's owner.
	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.Equal(t, HubProjectKey("alpha", "/home/dev/app"), routes[0].ProjectDir)
}

// TestHubCollision_InactiveHolderTakenSilently pins the other half of D10: a
// registration that is neither connected nor reserved does not hold its name.
func TestHubCollision_InactiveHolderTakenSilently(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	h.mustRegister(t, "alpha", "/home/dev/app", services)

	// Alpha never attached a tunnel; let its attach reservation lapse.
	h.clock.advance(constants.HubAttachGrace + time.Second)

	_, err := h.client(t).Register(registerBody("beta", "/other/app", services, false))
	require.NoError(t, err, "an inactive holder must not block a newcomer")

	assert.False(t, h.registry.HasProject(HubProjectKey("alpha", "/home/dev/app")),
		"the losing registration is removed entirely")
	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1)
	assert.Equal(t, HubProjectKey("beta", "/other/app"), routes[0].ProjectDir)
}

// TestHubCollision_TakeoverRemovesWholeRegistration pins that takeover displaces
// the loser ENTIRELY — including the names that did not collide — and closes its
// tunnel, rather than leaving a half-published project behind.
func TestHubCollision_TakeoverRemovesWholeRegistration(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	alphaServices := map[string]ServiceTarget{
		"web":   {Host: backendHost, Port: backendPort},
		"admin": {Host: backendHost, Port: backendPort},
	}
	alphaKey, _ := h.startPublisher(t, "alpha", "/home/dev/app", alphaServices, alphaServices)

	// Beta wants only "web", but taking it removes alpha's "admin" too.
	betaServices := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	_, err := h.client(t).Register(registerBody("beta", "/other/app", betaServices, true))
	require.NoError(t, err, "takeover must displace a connected holder")

	assert.False(t, h.registry.HasProject(alphaKey), "the loser is removed entirely")
	routes := h.registry.AllRoutes()
	require.Len(t, routes, 1, "alpha's non-conflicting admin route must be gone too")
	assert.Equal(t, "web."+h.domain, routes[0].Hostname)

	h.waitDetached(t, alphaKey)
}

// TestHubCollision_LocalHolderNeverDisplaced pins that the hub host's own
// projects win, with or without takeover. The machine running the hub is
// somebody's development machine first.
func TestHubCollision_LocalHolderNeverDisplaced(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}

	// A LOCAL registration on the hub's own domain and data-plane port.
	status, body := h.server.register(RegisterRequest{
		ProjectDir: "/home/hubowner/site",
		PID:        os.Getpid(),
		Version:    "test",
		Domain:     h.domain,
		Services:   services,
		HTTPSPort:  h.httpsPort,
	})
	require.Equal(t, http.StatusOK, status, "local register: %v", body)

	for _, takeover := range []bool{false, true} {
		t.Run(fmt.Sprintf("takeover=%v", takeover), func(t *testing.T) {
			_, err := h.client(t).Register(registerBody("beta", "/other/app", services, takeover))
			require.Error(t, err)
			apiErr := requireAPIError(t, err)
			assert.Equal(t, "HUB_NAME_HELD", apiErr.Code)
			assert.True(t, h.registry.HasProject("/home/hubowner/site"),
				"a local registration is never displaced by a remote one")
		})
	}
}

// TestHubCollision_AllOrNothing pins that a registration whose names are held by
// two different publishers takes both or neither: a partial publish is the one
// outcome that leaves a user with a project that looks up and does not work.
func TestHubCollision_AllOrNothing(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})
	target := ServiceTarget{Host: backendHost, Port: backendPort}

	h := newTunnelHub(t)
	// alpha holds "web" and is CONNECTED; gamma holds "api" and is idle.
	h.startPublisher(t, "alpha", "/home/dev/app", map[string]ServiceTarget{"web": target},
		map[string]ServiceTarget{"web": target})
	h.mustRegister(t, "gamma", "/home/dev/api", map[string]ServiceTarget{"api": target})
	h.clock.advance(constants.HubAttachGrace + time.Second)

	both := map[string]ServiceTarget{"web": target, "api": target}
	_, err := h.client(t).Register(registerBody("beta", "/other/app", both, false))
	require.Error(t, err)
	assert.Equal(t, "HUB_NAME_HELD", requireAPIError(t, err).Code)

	// The refusal removed NOTHING: gamma's idle registration, which would have
	// been taken silently on its own, must survive the refused attempt.
	assert.True(t, h.registry.HasProject(HubProjectKey("gamma", "/home/dev/api")),
		"an all-or-nothing refusal must not remove the holders it could have taken")
	assert.True(t, h.registry.HasProject(HubProjectKey("alpha", "/home/dev/app")))
	assert.False(t, h.registry.HasProject(HubProjectKey("beta", "/other/app")))

	// With takeover it is all-or-nothing in the other direction: both go.
	_, err = h.client(t).Register(registerBody("beta", "/other/app", both, true))
	require.NoError(t, err)
	assert.False(t, h.registry.HasProject(HubProjectKey("alpha", "/home/dev/app")))
	assert.False(t, h.registry.HasProject(HubProjectKey("gamma", "/home/dev/api")))
	assert.Len(t, h.registry.AllRoutes(), 2)
}

// TestTunnel_UpgradeRequiresRegistration pins the 404 NOT_REGISTERED arm that
// RunTunnel's reregister callback exists for.
func TestTunnel_UpgradeRequiresRegistration(t *testing.T) {
	h := newTunnelHub(t)

	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/tunnel", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", tunnelUpgradeProtocol)
	req.Header.Set("X-Prox-Origin", "shed")
	req.Header.Set("X-Prox-Project-Dir", "/never/registered")

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "NOT_REGISTERED", errorCode(t, resp))
}

// TestTunnel_SessionManagerDial exercises the session manager's Dial directly:
// it is the data plane's only entry into the session layer (P5), and the
// absent-session arm is what the offline 503 is built on.
func TestTunnel_SessionManagerDial(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "dialed")
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}

	_, err := h.server.tunnels.Dial(context.Background(), HubProjectKey("shed", "/home/dev/app"), backendHost, backendPort)
	require.ErrorIs(t, err, errNoTunnelSession, "dialing a key with no session must fail, not block")

	key, _ := h.startPublisher(t, "shed", "/home/dev/app", services, services)

	conn, err := h.server.tunnels.Dial(context.Background(), key, backendHost, backendPort)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n",
		net.JoinHostPort(backendHost, strconv.Itoa(backendPort)))
	require.NoError(t, err)
	raw, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "200 OK")
	assert.Contains(t, string(raw), "dialed")
}

// TestTunnel_ReregistersAfterNotRegistered pins the callback RunTunnel's
// signature exists for: on 404 NOT_REGISTERED the publisher re-registers and
// only then retries the handshake. Without it the retry loop would hammer an
// upgrade that can never succeed.
func TestTunnel_ReregistersAfterNotRegistered(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "back")
	})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	client := h.client(t)
	key := HubProjectKey("shed", "/home/dev/app")

	var calls atomic.Int64
	reregister := func() error {
		calls.Add(1)
		_, err := client.Register(registerBody("shed", "/home/dev/app", services, false))
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunTunnel(ctx, client, key, services, reregister, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// The project was never registered, so the FIRST upgrade attempt is a 404.
	h.waitAttached(t, key)
	assert.GreaterOrEqual(t, calls.Load(), int64(1), "the 404 must drive a re-register")

	status, body, _ := h.get(t, h.httpsClient(), "web", "/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "back", body)
}

// TestTunnel_UpgradeRejectsBadHeaders pins the handshake's own validation,
// including the D15 rule that the key is composed from the caller's own headers
// and a pre-qualified one cannot be expressed.
func TestTunnel_UpgradeRejectsBadHeaders(t *testing.T) {
	h := newTunnelHub(t)
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.mustRegister(t, "shed", "/home/dev/app", map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}})

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing upgrade headers",
			headers:    map[string]string{"X-Prox-Origin": "shed", "X-Prox-Project-Dir": "/home/dev/app"},
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "missing origin",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": tunnelUpgradeProtocol,
				"X-Prox-Project-Dir": "/home/dev/app",
			},
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "missing project dir",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": tunnelUpgradeProtocol, "X-Prox-Origin": "shed",
			},
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "an origin carrying the key separator is refused",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": tunnelUpgradeProtocol,
				"X-Prox-Origin": "shed:/home/dev", "X-Prox-Project-Dir": "app",
			},
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "another publisher's dir composes a key that does not exist",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": tunnelUpgradeProtocol,
				"X-Prox-Origin": "intruder", "X-Prox-Project-Dir": "/home/dev/app",
			},
			wantStatus: http.StatusNotFound, wantCode: "NOT_REGISTERED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/tunnel", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+h.token)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantCode, errorCode(t, resp))
		})
	}

	t.Run("without a token the tunnel is not reachable at all", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/tunnel", nil)
		require.NoError(t, err)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", tunnelUpgradeProtocol)
		req.Header.Set("X-Prox-Origin", "shed")
		req.Header.Set("X-Prox-Project-Dir", "/home/dev/app")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// TestTunnel_HubStopClosesTunnels pins that `prox hub stop` takes the publishers
// with it: sessions closed, registrations removed.
func TestTunnel_HubStopClosesTunnels(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {})

	h := newTunnelHub(t)
	services := map[string]ServiceTarget{"web": {Host: backendHost, Port: backendPort}}
	key, _ := h.startPublisher(t, "shed", "/home/dev/app", services, services)

	h.server.StopHub()

	assert.Nil(t, h.server.tunnels.get(key), "hub stop must close every tunnel")
	assert.False(t, h.registry.HasProject(key), "hub stop must drop every remote registration")
	assert.Empty(t, h.registry.AllRoutes())
}

// requireAPIError unwraps a *DaemonAPIError from a client call.
func requireAPIError(t *testing.T, err error) *DaemonAPIError {
	t.Helper()
	var apiErr *DaemonAPIError
	require.True(t, errors.As(err, &apiErr), "expected a *DaemonAPIError, got %T: %v", err, err)
	return apiErr
}

// decodeHolders re-sends a register over the raw wire and returns the holders
// list from the 409 body, which the typed Client does not surface until C5.
func decodeHolders(t *testing.T, h *tunnelHub, body RegisterRequest) []HubHolder {
	t.Helper()
	resp := hubDo(t, http.MethodPost, h.baseURL+"/api/v1/register", h.token, body)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	return er.Holders
}
