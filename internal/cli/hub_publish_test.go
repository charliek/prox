package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- D8: precedence ---

func TestResolveHubSelection(t *testing.T) {
	tests := []struct {
		name     string
		in       hubSelectionInputs
		want     hubSelection
		wantErr  string
		wantNone bool
	}{
		{
			name: "nothing configured",
			in:   hubSelectionInputs{},
			want: hubSelection{},
		},
		{
			name: "flag only",
			in:   hubSelectionInputs{FlagSet: true, FlagValue: "llt"},
			want: hubSelection{Alias: "llt", Explicit: true, Source: "flag"},
		},
		{
			name: "flag wins over env and config",
			in: hubSelectionInputs{
				FlagSet: true, FlagValue: "flagged",
				Env: "envhub", EnvPresent: true,
				ConfigHub: "cfghub",
			},
			want: hubSelection{Alias: "flagged", Explicit: true, Source: "flag"},
		},
		{
			name: "env wins over config",
			in:   hubSelectionInputs{Env: "envhub", EnvPresent: true, ConfigHub: "cfghub"},
			want: hubSelection{Alias: "envhub", Explicit: true, Source: "env"},
		},
		{
			name: "config only is not explicit",
			in:   hubSelectionInputs{ConfigHub: "cfghub"},
			want: hubSelection{Alias: "cfghub", Explicit: false, Source: "config"},
		},
		{
			name: "empty env falls through to config",
			in:   hubSelectionInputs{Env: "  ", EnvPresent: true, ConfigHub: "cfghub"},
			want: hubSelection{Alias: "cfghub", Explicit: false, Source: "config"},
		},
		{
			name: "no-hub disables a configured hub",
			in:   hubSelectionInputs{NoHub: true, ConfigHub: "cfghub", Env: "envhub", EnvPresent: true},
			want: hubSelection{},
		},
		{
			name: "the literal default alias is passed through for expansion",
			in:   hubSelectionInputs{FlagSet: true, FlagValue: "default"},
			want: hubSelection{Alias: "default", Explicit: true, Source: "flag"},
		},
		{
			name:    "flag and no-hub together",
			in:      hubSelectionInputs{FlagSet: true, FlagValue: "llt", NoHub: true},
			wantErr: "--hub and --no-hub cannot be used together",
		},
		{
			name:    "flag with an empty value",
			in:      hubSelectionInputs{FlagSet: true, FlagValue: "   "},
			wantErr: "--hub requires a hub alias",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHubSelection(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveHubPublishing_UnknownAliasSplit pins the one split §3.1 makes: an
// alias TYPED by the user is fatal, the same alias COMMITTED in prox.yaml warns
// and continues.
func TestResolveHubPublishing_UnknownAliasSplit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &config.Config{Proxy: &config.ProxyConfig{Enabled: true, Hub: "ghost"}}

	t.Run("explicit flag is fatal", func(t *testing.T) {
		_, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{FlagSet: true, FlagValue: "ghost"})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrUnknownHub)
	})

	t.Run("explicit env is fatal", func(t *testing.T) {
		_, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{Env: "ghost", EnvPresent: true})
		require.Error(t, err)
		assert.ErrorIs(t, err, config.ErrUnknownHub)
	})

	t.Run("proxy.hub warns and continues", func(t *testing.T) {
		res, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{ConfigHub: "ghost"})
		require.NoError(t, err)
		assert.False(t, res.Enabled)
		require.NotNil(t, res.Warning)
		assert.Equal(t, warningCodeHubUnknownAlias, res.Warning.Code)
	})

	t.Run("proxy.hub: default names the RESOLVED alias in its remediation", func(t *testing.T) {
		// `default:` points at an alias this machine does not define. The
		// warning has to send the user after "llt" — "prox hub add default"
		// would name a reserved alias that is never consulted (plan 031 D8).
		require.NoError(t, config.SaveUserHubs(config.UserHubs{
			Hubs:    map[string]config.HubConfig{"home": {URL: "http://b.example:8443"}},
			Default: "llt",
		}))
		t.Cleanup(func() { require.NoError(t, config.SaveUserHubs(config.UserHubs{})) })

		res, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{ConfigHub: "default"})
		require.NoError(t, err)
		assert.False(t, res.Enabled)
		require.NotNil(t, res.Warning)
		assert.Contains(t, res.Warning.Hint, "prox hub add llt <url>")
		assert.NotContains(t, res.Warning.Hint, "prox hub add default")
		assert.Contains(t, res.Warning.Message, `"llt"`)
	})

	t.Run("no hub at all resolves to nothing", func(t *testing.T) {
		res, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{NoHub: true, ConfigHub: "ghost"})
		require.NoError(t, err)
		assert.False(t, res.Enabled)
		assert.Nil(t, res.Warning)
	})
}

// TestResolveHubPublishing_FatalConfigErrors pins the rest of §3.1's fatal row:
// problems with the ENTRY itself are fatal however the alias was selected,
// because they are mistakes the user must fix either way.
func TestResolveHubPublishing_FatalConfigErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Run("malformed url", func(t *testing.T) {
		cfg := &config.Config{
			Proxy: &config.ProxyConfig{Enabled: true, Hub: "llt"},
			Hubs:  map[string]config.HubConfig{"llt": {URL: "ftp://nope"}},
		}
		_, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{ConfigHub: "llt"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "http")
	})

	t.Run("two token sources", func(t *testing.T) {
		cfg := &config.Config{
			Proxy: &config.ProxyConfig{Enabled: true, Hub: "llt"},
			Hubs: map[string]config.HubConfig{"llt": {
				URL: "http://hub.invalid:8443", Token: "a", TokenFile: "b",
			}},
		}
		_, err := resolveHubPublishing(cfg, "prox.yaml", hubSelectionInputs{ConfigHub: "llt"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most one of token")
	})
}

// --- §3.1: the failure classification table ---

// fakeHubRegister scripts one hub register response and returns the base URL.
type fakeHubRegister struct {
	status int
	body   any
	calls  chan proxyd.RegisterRequest
}

func startFakeHub(t *testing.T, h *fakeHubRegister) string {
	t.Helper()
	h.calls = make(chan proxyd.RegisterRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/register" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req proxyd.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		select {
		case h.calls <- req:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		_ = json.NewEncoder(w).Encode(h.body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startBlackHoleHub returns the URL of a listener that ACCEPTS connections and
// then never answers — the case AC11 adds to plain connection-refused, because
// a black hole is what makes an unbounded first register stall startup.
func startBlackHoleHub(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		// The goroutine owns what it accepted and closes it on its own way
		// out. t.Cleanup would be the wrong owner twice over: closing the
		// listener unblocks Accept but leaves an already-accepted conn open,
		// and a Cleanup appended from here can land after the cleanup runner
		// has already drained its list.
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, conn) // hold it open, say nothing
		}
	}()
	return "http://" + ln.Addr().String()
}

// refusedHubURL returns a URL whose port is bound and immediately released, so
// a connect there is refused rather than routed anywhere.
func refusedHubURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return "http://" + addr
}

// fakeTunnelRunner stands in for runHubTunnel.
//
// It is a real runner in the three ways the assertions depend on (plan 031,
// review A11). The previous stand-in only incremented a counter and returned,
// so `wantRetries` proved that a function had been called once — not that
// anything retries, not that exactly one owner exists, and not that cancelling
// joins anything. This one:
//
//   - counts STARTS, so "one goroutine owns the publisher state machine" (D19)
//     is an assertion rather than an assumption;
//   - actually LOOPS, counting attempts, so "retries forever" is observed
//     happening rather than inferred from a single call;
//   - returns only when its context is cancelled, and says so on finished, so a
//     test can join it exactly as hubPublisher.Shutdown does.
type fakeTunnelRunner struct {
	mu       sync.Mutex
	starts   int
	attempts int
	once     sync.Once
	finished chan struct{}
}

func newFakeTunnelRunner() *fakeTunnelRunner {
	return &fakeTunnelRunner{finished: make(chan struct{})}
}

func (f *fakeTunnelRunner) run(ctx context.Context, _ *hubPublisher, _ *proxy.RequestManager) {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	defer f.once.Do(func() { close(f.finished) })

	for {
		f.mu.Lock()
		f.attempts++
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *fakeTunnelRunner) counts() (starts, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.attempts
}

// captureLogOutput redirects the stdlib logger — the channel every hub advisory
// that is NOT going through the startup render uses — into a buffer, so a test
// can count the lines a user would actually see.
//
// It exists because "exactly one warning" is a claim about OUTPUT, and the
// tests that asserted it were reading the sink instead (plan 031, review A5).
func captureLogOutput(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return buf
}

// syncBuffer is a bytes.Buffer safe for the logger's goroutine and the test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hubPublishHarness wires a runtime, a warning sink and a recording tunnel
// runner around startHubPublishing.
type hubPublishHarness struct {
	rt     *proxyRuntime
	sink   *warningSink
	runner *fakeTunnelRunner
	pre    *startupPreamble
}

func newHubPublishHarness() *hubPublishHarness {
	rt := newProxyRuntime()
	sink := newWarningSink()
	rt.SetWarningSink(sink)
	return &hubPublishHarness{rt: rt, sink: sink, runner: newFakeTunnelRunner(), pre: newStartupPreamble(false)}
}

// starts is how many times the tunnel runner was launched.
func (h *hubPublishHarness) starts() int {
	starts, _ := h.runner.counts()
	return starts
}

func (h *hubPublishHarness) publish(t *testing.T, ctx context.Context, hubURL string, opts ...func(*hubPublishOptions)) *hubPublisher {
	t.Helper()
	client, err := proxyd.NewHubClient(hubURL, "tok", "publisher")
	require.NoError(t, err)

	o := hubPublishOptions{
		Res: hubResolution{
			Enabled: true,
			Hub:     config.ResolvedHub{Alias: "llt", URL: hubURL, Token: "tok", Origin: "publisher"},
			Client:  client,
		},
		Cfg: &config.Config{
			Proxy:    &config.ProxyConfig{Enabled: true, Domain: "local.test", HTTPSPort: 443},
			Services: map[string]config.ServiceConfig{"auth": {Host: "localhost", Port: 3000}},
		},
		Cwd:      "/tmp/project",
		Runtime:  h.rt,
		Preamble: h.pre,
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		Run:      h.runner.run,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return startHubPublishing(ctx, o)
}

// TestHubFailureClassification_Section31 is plan 031 §3.1 as a table: every
// non-fatal failure class mapped to what it costs the user — exit code (always
// 0 here, because startHubPublishing cannot fail the command at all), how many
// warnings are printed, whether the publisher keeps retrying, and which `Hub:`
// state `prox status` shows.
//
// AC11's warning wording is pinned here, verbatim.
func TestHubFailureClassification_Section31(t *testing.T) {
	heldBody := proxyd.ErrorResponse{
		Error: "held", Code: "HUB_NAME_HELD",
		Holders: []proxyd.HubHolder{{Hostname: "auth.llt.test", Origin: "mac", ProjectDir: "/home/c/slauth", Connected: true}},
	}

	tests := []struct {
		name string
		// hubURL is built per-case.
		url func(t *testing.T) string
		// wantWarnings is the exact set of rendered warning LINES.
		wantWarnings []string
		wantRetries  bool
		// wantState is the expected `Hub:` state; "" means NO hub state at all.
		wantState string
		// wantDetail, when set, is the exact detail the state carries.
		wantDetail string
		wantNil    bool
	}{
		{
			name: "retryable: connection refused",
			url:  refusedHubURL,
			wantWarnings: []string{
				"Warning: hub llt unreachable (connection refused); continuing with local proxy only, will keep retrying",
			},
			wantRetries: true,
			wantState:   hubStateReconnecting,
		},
		{
			name: "retryable: black hole",
			url:  startBlackHoleHub,
			wantWarnings: []string{
				"Warning: hub llt unreachable (timed out); continuing with local proxy only, will keep retrying",
			},
			wantRetries: true,
			wantState:   hubStateReconnecting,
		},
		{
			name: "retryable: hub-side 500",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{
					status: http.StatusInternalServerError,
					body:   proxyd.ErrorResponse{Error: "cert generation failed", Code: "INTERNAL"},
				})
			},
			wantWarnings: []string{
				"Warning: hub llt unreachable (hub returned 500 INTERNAL); continuing with local proxy only, will keep retrying",
			},
			wantRetries: true,
			wantState:   hubStateReconnecting,
		},
		{
			name: "terminal: protocol mismatch",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{
					status: http.StatusConflict,
					body:   proxyd.ErrorResponse{Error: "nope", Code: "PROTOCOL_MISMATCH", HubProtocol: 2},
				})
			},
			wantWarnings: []string{
				fmt.Sprintf("Warning: hub llt speaks a different hub protocol (hub 2, this prox %d); continuing with local proxy only", constants.HubProtocolVersion),
				"         Upgrade the older prox — the hub host and this machine must agree on the hub protocol.",
			},
			wantRetries: false,
			wantState:   hubStateProtocolMismatch,
		},
		{
			name: "terminal: auth failed",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{
					status: http.StatusUnauthorized,
					body:   proxyd.ErrorResponse{Error: "nope", Code: "UNAUTHORIZED"},
				})
			},
			wantWarnings: []string{
				"Warning: hub llt rejected this machine's token; continuing with local proxy only",
				"         Check the token in ~/.prox/hubs.yaml against 'prox hub token' on the hub host.",
			},
			wantRetries: false,
			wantState:   hubStateAuthFailed,
		},
		{
			// Review A7: a declined collision keeps a TERMINAL, visible state.
			// It used to erase the hub from `prox status` entirely, which says
			// "no hub was configured" — the one thing that is not true here.
			name: "terminal: name held, declined (non-interactive)",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{status: http.StatusConflict, body: heldBody})
			},
			wantWarnings: []string{
				"Warning: hub llt already publishes auth.llt.test held by mac:/home/c/slauth; continuing with local proxy only",
				"         Re-run with --hub-takeover to take the name(s), or stop the other publisher.",
			},
			wantRetries: false,
			wantState:   hubStateNameHeld,
			wantDetail:  "auth.llt.test held by mac:/home/c/slauth",
		},
		{
			name: "success",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{
					status: http.StatusOK,
					body: proxyd.RegisterResponse{
						Registered: []string{"auth.llt.test"},
						Hub:        &proxyd.RegisterHubInfo{Domain: "llt.test", HTTPSPort: 443},
					},
				})
			},
			wantWarnings: nil,
			wantRetries:  true,
			wantState:    hubStateConnecting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			h := newHubPublishHarness()
			// Every user-visible line this session produces, captured: AC11
			// counts WARNING LINES the user sees, and a sink entry is not one
			// (review A5). logWarning writes through the stdlib logger, which
			// is the terminal in plain mode and the TUI's log pane otherwise.
			logged := captureLogOutput(t)

			start := time.Now()
			p := h.publish(t, ctx, tc.url(t))
			elapsed := time.Since(start)

			// Exit code: startHubPublishing returns no error at all, so there
			// is nothing here that can fail `prox up` (§3.1).
			if tc.wantNil {
				assert.Nil(t, p, "this row publishes nothing")
			} else {
				require.NotNil(t, p)
			}

			var lines []string
			for _, w := range h.sink.Warnings() {
				lines = append(lines, formatWarning(w)...)
			}
			assert.Equal(t, tc.wantWarnings, lines, "the warnings the startup render will print")

			// And the same count as USER-VISIBLE lines: the sink is rendered
			// once by runUp, so nothing may have been printed on top of it
			// while the session was still unsealed.
			assert.Equal(t, 0, strings.Count(logged.String(), warningPrefix),
				"a warning raised before the startup render must reach the user through the sink, not twice")

			if tc.wantRetries {
				// A real retry loop: one owner, and attempts that keep coming.
				require.Eventually(t, func() bool {
					starts, attempts := h.runner.counts()
					return starts == 1 && attempts >= 3
				}, 5*time.Second, 2*time.Millisecond,
					"the tunnel loop must own the retries and keep retrying")
				// Cancelling joins it, which is what shutdown depends on.
				cancel()
				select {
				case <-h.runner.finished:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelling the run context did not stop the tunnel loop")
				}
				starts, _ := h.runner.counts()
				assert.Equal(t, 1, starts, "exactly one tunnel loop, ever")
			} else {
				// A terminal failure starts nothing at all. Give it a moment:
				// "did not happen" needs a window to not happen in.
				time.Sleep(50 * time.Millisecond)
				assert.Equal(t, 0, h.starts(), "a terminal failure must not retry")
			}

			state := h.rt.HubState()
			if tc.wantState == "" {
				assert.Nil(t, state, "no hub state at all")
			} else {
				require.NotNil(t, state)
				assert.Equal(t, tc.wantState, state.State)
				if tc.wantDetail != "" {
					assert.Equal(t, tc.wantDetail, state.Detail)
				}
			}

			// AC11: `prox up` must not be delayed beyond HubConnectTimeout
			// waiting for a hub. Generous slack for a loaded CI box; the point
			// is that it is bounded at all.
			assert.Less(t, elapsed, constants.HubConnectTimeout+2*time.Second,
				"the first hub register must be bounded by HubConnectTimeout")
		})
	}
}

// TestHubPublish_WarnsOnlyOnce pins D19's "exactly one warning on the FIRST
// entry into a non-connected state": every later transition logs instead.
//
// It asserts on the OUTPUT, not on the sink (plan 031, review A5). AC11 promises
// the user one warning line, and the sink is only one of the two places a line
// can come from — the other is logWarning, which writes a fully rendered
// `Warning:` line straight to the terminal (or the TUI's log pane). Counting
// sink entries passed happily while later transitions printed more of them.
func TestHubPublish_WarnsOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHubPublishHarness()
	logged := captureLogOutput(t)
	p := h.publish(t, ctx, refusedHubURL(t))
	require.NotNil(t, p)
	require.Len(t, h.sink.Warnings(), 1)

	// visibleWarnings is what the user ends up seeing: the sink, rendered once
	// by runUp at the end of startup, plus anything written to the log since.
	visibleWarnings := func() int {
		return len(h.sink.Warnings()) + strings.Count(logged.String(), warningPrefix)
	}
	require.Equal(t, 1, visibleWarnings(), "the first outage is the one warning")

	outageStart := h.rt.HubState().Since

	// Three more retry failures, exactly as the tunnel loop would report them.
	for i := 0; i < 3; i++ {
		p.ForwarderConnectFailed(fmt.Errorf("still down"))
	}
	assert.Len(t, h.sink.Warnings(), 1, "the retry loop is silent after the first warning")
	assert.Equal(t, 1, visibleWarnings(), "and prints nothing on top of it")
	assert.Equal(t, hubStateReconnecting, h.rt.HubState().State)
	// "down <t>" must measure the OUTAGE, so a retry that fails the same way
	// does not restart the clock.
	assert.Equal(t, outageStart, h.rt.HubState().Since)

	// And a recovery flips the state without adding anything.
	p.ForwarderConnected()
	assert.Equal(t, hubStateConnected, h.rt.HubState().State)
	assert.Equal(t, 1, visibleWarnings())

	// A LATER outage is a real transition — it logs and restarts the outage
	// clock — but it still adds no second warning: the sink is what a
	// `prox up -d` parent replays at startup, and this session has already had
	// its one advisory.
	p.ForwarderConnectFailed(fmt.Errorf("down again"))
	assert.Equal(t, hubStateReconnecting, h.rt.HubState().State)
	assert.Equal(t, 1, visibleWarnings())
	assert.True(t, h.rt.HubState().Since.After(outageStart))

	// A DIFFERENT advisory entirely — a later transition into another
	// non-connected state, which is where the second `Warning:` line used to
	// come from. It is recorded in the log, without the label.
	p.degrade(hubStateDisplaced, "auth.llt.test held by mac:/a",
		hubDisplacedWarning("llt", &proxyd.DaemonAPIError{Code: "HUB_NAME_HELD", Status: 409}))
	assert.Equal(t, hubStateDisplaced, h.rt.HubState().State)
	assert.Equal(t, 1, visibleWarnings(),
		"AC11 counts WARNING LINES the user sees: a later state change is not a second warning")
	assert.Contains(t, logged.String(), "another publisher took over",
		"but it is still recorded, which is what .prox/prox.log is for")
}

// recordingHub is a hub that records every call it receives, in order, and can
// be scripted to fail registers. It is how the lifecycle findings (A1, A2, A3)
// are asserted: each of them is a claim about WHICH calls this publisher makes,
// with what, and in what order.
type recordingHub struct {
	mu sync.Mutex
	// calls is "register(takeover=…)" / "deregister", in arrival order.
	calls []string
	// registerStatus is the status registers answer with; 200 means success.
	registerStatus int
	url            string
}

func startRecordingHub(t *testing.T, registerStatus int) *recordingHub {
	t.Helper()
	h := &recordingHub{registerStatus: registerStatus}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/register":
			var req proxyd.RegisterRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.record(fmt.Sprintf("register(takeover=%t)", req.Takeover))
			status := h.status()
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(proxyd.RegisterResponse{
					Registered: []string{"auth.llt.test"},
					Hub:        &proxyd.RegisterHubInfo{Domain: "llt.test", HTTPSPort: 443},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(proxyd.ErrorResponse{Error: "nope", Code: "INTERNAL"})
		case "/api/v1/deregister":
			h.record("deregister")
			_, _ = w.Write([]byte(`{"removed":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *recordingHub) record(call string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call)
}

func (h *recordingHub) status() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registerStatus
}

func (h *recordingHub) setStatus(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registerStatus = status
}

func (h *recordingHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// TestHubPublish_TakeoverSurvivesToTheRetryPath is plan 031 review A1.
//
// The case the flag exists for is a hub that is DOWN when `prox up` runs: the
// first register never reaches anybody, the tunnel loop takes over, and the
// register that finally meets the held name is the RE-register. reregister()
// hard-coded takeover:false, so `--hub-takeover` was silently dropped exactly
// when the user needed it and the publisher went `displaced` instead.
//
// The second half is just as deliberate: once a registration has succeeded, the
// flag is spent. A publisher that kept re-asserting takeover would fight every
// later displacement, and two such publishers take the name from each other
// forever.
func TestHubPublish_TakeoverSurvivesToTheRetryPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The hub is down for the FIRST register (500 → retryable), which is the
	// state a publisher that starts before its hub is in.
	hub := startRecordingHub(t, http.StatusInternalServerError)

	h := newHubPublishHarness()
	p := h.publish(t, ctx, hub.url, func(o *hubPublishOptions) { o.Takeover = true })
	require.NotNil(t, p)
	require.Equal(t, []string{"register(takeover=false)"}, hub.seen(),
		"the first register deliberately asks without takeover, so a collision can be reported")

	// The hub comes up. This is RunTunnel's 404 NOT_REGISTERED callback.
	hub.setStatus(http.StatusOK)
	require.NoError(t, p.reregister())
	assert.Equal(t, []string{"register(takeover=false)", "register(takeover=true)"}, hub.seen(),
		"--hub-takeover must reach the register that actually lands")

	// And it is spent: a later re-register does not fight for the name again.
	require.NoError(t, p.reregister())
	assert.Equal(t, []string{
		"register(takeover=false)",
		"register(takeover=true)",
		"register(takeover=false)",
	}, hub.seen(), "a takeover is honored once, not asserted forever")
}

// TestHubPublish_ShutdownJoinsWorkersBeforeDeregistering is plan 031 review A2.
//
// D6c's ordering — cancel the tunnel, THEN deregister — was documented and not
// enforced: both workers were launched with a bare `go`, so a re-register
// already in flight when the cancel landed could reach the hub AFTER the
// deregister and leave the registration behind for its whole lease.
func TestHubPublish_ShutdownJoinsWorkersBeforeDeregistering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := startRecordingHub(t, http.StatusOK)

	h := newHubPublishHarness()
	// A runner that re-registers on its way OUT, slowly — the in-flight
	// re-register the ordering exists to defeat.
	stopped := make(chan struct{})
	p := h.publish(t, ctx, hub.url, func(o *hubPublishOptions) {
		o.Run = func(ctx context.Context, p *hubPublisher, _ *proxy.RequestManager) {
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond)
			_ = p.reregister()
			close(stopped)
		}
	})
	require.NotNil(t, p)

	p.Shutdown(5 * time.Second)

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the runner had not finished when Shutdown returned, so it was never joined")
	}

	calls := hub.seen()
	require.NotEmpty(t, calls)
	assert.Equal(t, "deregister", calls[len(calls)-1],
		"the deregister must be the LAST word: a re-register landing after it would resurrect the registration")
}

// TestHubPublish_TunnelAttachProvesRegistration is plan 031 review A3.
//
// `registered` was set only by a fully decoded 200, so a register that timed out
// or came back malformed AFTER the hub committed left a registration nothing
// would ever clean up — it sat until the lease expired. A tunnel that attaches
// is proof the registration exists: the hub answers the upgrade 404
// NOT_REGISTERED otherwise.
func TestHubPublish_TunnelAttachProvesRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Registers fail, so nothing is ever confirmed by the register path.
	hub := startRecordingHub(t, http.StatusInternalServerError)

	h := newHubPublishHarness()
	p := h.publish(t, ctx, hub.url)
	require.NotNil(t, p)
	require.False(t, p.everRegistered())

	// The tunnel attaches anyway: the registration DID land, this process just
	// never got to read the answer.
	p.ForwarderConnected()
	assert.True(t, p.everRegistered(), "a tunnel only attaches to a registration that exists")

	p.Shutdown(5 * time.Second)
	assert.Contains(t, hub.seen(), "deregister",
		"a registration proven by the tunnel must be removed at shutdown")
}

// TestHubPublish_AmbiguousRegisterIsCleanedUp is A3's second half: a register
// whose outcome is UNKNOWN — a timeout, a connection cut mid-response — may have
// been committed, so shutdown tries to remove it. A register that was REFUSED
// (connection refused, or a complete error response) was not committed and is
// left alone, which is what keeps the AC11 "hub is down" teardown quiet.
func TestHubPublish_AmbiguousRegisterIsCleanedUp(t *testing.T) {
	t.Run("a complete error response is not ambiguous", func(t *testing.T) {
		assert.False(t, registerMayHaveLanded(&proxyd.DaemonAPIError{Status: 500, Code: "INTERNAL"}))
	})
	t.Run("connection refused is not ambiguous", func(t *testing.T) {
		assert.False(t, registerMayHaveLanded(fmt.Errorf("dial: %w", syscall.ECONNREFUSED)))
	})
	t.Run("a DNS failure is not ambiguous", func(t *testing.T) {
		assert.False(t, registerMayHaveLanded(fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host"})))
	})
	t.Run("a timeout IS ambiguous", func(t *testing.T) {
		assert.True(t, registerMayHaveLanded(context.DeadlineExceeded))
	})

	// End to end: a hub that accepts the connection and never answers leaves the
	// publisher unsure, and shutdown cleans up rather than leaving a
	// registration to sit out its lease.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHubPublishHarness()
	p := h.publish(t, ctx, startBlackHoleHub(t))
	require.NotNil(t, p)
	assert.False(t, p.everRegistered())
	assert.True(t, p.mayBeRegistered(), "a register that timed out may well have been committed")

	// And a REFUSED hub leaves nothing to clean up, so teardown stays silent.
	h2 := newHubPublishHarness()
	p2 := h2.publish(t, ctx, refusedHubURL(t))
	require.NotNil(t, p2)
	assert.False(t, p2.mayBeRegistered())
}

// --- D10: the collision prompt ---

func TestDecideHubCollision(t *testing.T) {
	tests := []struct {
		name        string
		takeover    bool
		interactive bool
		want        hubCollisionDecision
	}{
		{"tty and attached asks", false, true, hubCollisionAsk},
		{"detached or piped declines", false, false, hubCollisionDecline},
		{"--hub-takeover takes without asking", true, false, hubCollisionTakeover},
		{"--hub-takeover beats the prompt", true, true, hubCollisionTakeover},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decideHubCollision(tc.takeover, tc.interactive))
		})
	}
}

// TestFormatHubHolders_ListsEveryHolder pins D10's multi-holder rule: a project
// whose two services are held by two different publishers must see BOTH, since
// registration is all-or-nothing.
func TestFormatHubHolders_ListsEveryHolder(t *testing.T) {
	lines := formatHubHolders("llt", []proxyd.HubHolder{
		{Hostname: "auth.llt.test", Origin: "mac", ProjectDir: "/home/c/slauth", Connected: true},
		{Hostname: "api.llt.test", Origin: "vm2", ProjectDir: "/srv/app", Connected: false},
	})
	require.Len(t, lines, 3)
	assert.Equal(t, "Hub llt already publishes 2 service name(s) this project registers:", lines[0])
	assert.Equal(t, "  api.llt.test — vm2:/srv/app (disconnected)", lines[1])
	assert.Equal(t, "  auth.llt.test — mac:/home/c/slauth (connected)", lines[2])

	// A LOCAL holder is named as such — D10 never lets a remote publisher
	// displace one, so "take it over?" must not imply otherwise.
	local := formatHubHolders("llt", []proxyd.HubHolder{
		{Hostname: "auth.llt.test", ProjectDir: "/srv/hubhost", Connected: true},
	})
	assert.Contains(t, local[1], "a local project on the hub host (/srv/hubhost)")
}

func TestAskHubTakeover(t *testing.T) {
	holders := []proxyd.HubHolder{{Hostname: "auth.llt.test", Origin: "mac", ProjectDir: "/a", Connected: true}}
	tests := []struct {
		answer string
		want   bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"maybe\n", false},
	}
	for _, tc := range tests {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			var out strings.Builder
			got := askHubTakeover(context.Background(), &out, strings.NewReader(tc.answer), "llt", holders)
			assert.Equal(t, tc.want, got)
			assert.Contains(t, out.String(), "Take it over? [y/N]")
			assert.Contains(t, out.String(), "auth.llt.test")
		})
	}

	// Review A8: `prox up` has already called signal.Notify by the time this
	// prompt appears, so SIGINT no longer terminates the process — the prompt
	// has to honor the run context itself or Ctrl-C leaves startup wedged in
	// ReadString until somebody presses enter. A reader that never produces a
	// line stands in for a terminal nobody is typing at.
	t.Run("a cancelled context declines without a keystroke", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var out strings.Builder
		// The reader is released at cleanup rather than blocking forever: a
		// test helper that parks a goroutine for the life of the binary is the
		// leak this branch keeps re-learning about.
		stdin := neverReader{release: make(chan struct{})}
		t.Cleanup(func() { close(stdin.release) })
		done := make(chan bool, 1)
		go func() { done <- askHubTakeover(ctx, &out, stdin, "llt", holders) }()

		select {
		case <-done:
			t.Fatal("the prompt returned before the context was cancelled")
		case <-time.After(50 * time.Millisecond):
		}

		cancel()
		select {
		case got := <-done:
			assert.False(t, got, "a cancelled prompt must decline")
		case <-time.After(5 * time.Second):
			t.Fatal("cancelling the run context did not release the takeover prompt")
		}
	})
}

// neverReader blocks until released, like a terminal with nobody at it.
type neverReader struct{ release chan struct{} }

func (r neverReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// TestHubPublish_TakeoverResendsWithTakeover pins that answering yes (or
// passing --hub-takeover) re-sends the SAME registration with takeover: true.
func TestHubPublish_TakeoverResendsWithTakeover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// seen is written by the server's handler goroutine and read by the test,
	// so it needs a mutex: the HTTP response lifecycle orders the append before
	// publish returns, but ordering is not synchronization and -race says so.
	var seenMu sync.Mutex
	var seen []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proxyd.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenMu.Lock()
		seen = append(seen, req.Takeover)
		seenMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !req.Takeover {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(proxyd.ErrorResponse{
				Error: "held", Code: "HUB_NAME_HELD",
				Holders: []proxyd.HubHolder{{Hostname: "auth.llt.test", Origin: "mac", ProjectDir: "/a", Connected: true}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(proxyd.RegisterResponse{
			Registered: []string{"auth.llt.test"},
			Hub:        &proxyd.RegisterHubInfo{Domain: "llt.test", HTTPSPort: 443},
		})
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		opt  func(*hubPublishOptions)
	}{
		{"--hub-takeover", func(o *hubPublishOptions) { o.Takeover = true }},
		{"interactive yes", func(o *hubPublishOptions) {
			o.Interactive = true
			o.Stdin = strings.NewReader("y\n")
			o.Stdout = io.Discard
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seenMu.Lock()
			seen = nil
			seenMu.Unlock()

			h := newHubPublishHarness()
			p := h.publish(t, ctx, srv.URL, tc.opt)
			require.NotNil(t, p)

			seenMu.Lock()
			got := append([]bool(nil), seen...)
			seenMu.Unlock()
			assert.Equal(t, []bool{false, true}, got, "first without takeover, then with")
			assert.Empty(t, h.sink.Warnings(), "a takeover the user asked for is not an advisory")
			require.NotNil(t, h.rt.HubState())
			assert.Equal(t, hubStateConnecting, h.rt.HubState().State)
			require.Eventually(t, func() bool { return h.starts() == 1 }, 5*time.Second, 2*time.Millisecond,
				"exactly one tunnel loop owns the publisher")
		})
	}
}

// --- AC9: the `Hub:` line in every state of §4.2 ---

func TestHubStatusLine_EveryState(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	twelveAgo := now.Add(-12 * time.Second)

	tests := []struct {
		name string
		hub  *api.HubStatusResponse
		want string
	}{
		{"no hub configured", nil, ""},
		{
			"connected",
			&api.HubStatusResponse{Alias: "llt", State: hubStateConnected, Routes: 2},
			"Hub: llt (connected, 2 routes)",
		},
		{
			"connected with one route",
			&api.HubStatusResponse{Alias: "llt", State: hubStateConnected, Routes: 1},
			"Hub: llt (connected, 1 route)",
		},
		{
			"reconnecting",
			&api.HubStatusResponse{Alias: "llt", State: hubStateReconnecting, Since: &twelveAgo},
			"Hub: llt (reconnecting, down 12s)",
		},
		{
			"reconnecting with no since",
			&api.HubStatusResponse{Alias: "llt", State: hubStateReconnecting},
			"Hub: llt (reconnecting, down 0s)",
		},
		{
			"displaced",
			&api.HubStatusResponse{Alias: "llt", State: hubStateDisplaced, Detail: "auth held by mac:/home/c/slauth"},
			"Hub: llt (displaced: auth held by mac:/home/c/slauth)",
		},
		{
			"protocol mismatch",
			&api.HubStatusResponse{Alias: "llt", State: hubStateProtocolMismatch, Detail: "hub 2, this prox 1"},
			"Hub: llt (protocol mismatch: hub 2, this prox 1)",
		},
		{
			"auth failed",
			&api.HubStatusResponse{Alias: "llt", State: hubStateAuthFailed},
			"Hub: llt (auth failed)",
		},
		{
			// Review A7: a declined collision is a visible terminal state, not
			// an erased hub. The detail names who holds the name.
			"name held, declined",
			&api.HubStatusResponse{Alias: "llt", State: hubStateNameHeld, Detail: "auth.llt.test held by mac:/home/c/slauth"},
			"Hub: llt (name held: auth.llt.test held by mac:/home/c/slauth)",
		},
		{
			"name held with no holders",
			&api.HubStatusResponse{Alias: "llt", State: hubStateNameHeld},
			"Hub: llt (name held)",
		},
		{
			"a transient state renders verbatim",
			&api.HubStatusResponse{Alias: "llt", State: hubStateConnecting},
			"Hub: llt (connecting)",
		},
		{
			"an unknown state from a newer prox",
			&api.HubStatusResponse{Alias: "llt", State: "something_new"},
			"Hub: llt (something new)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hubStatusLine(tc.hub, now))
		})
	}
}

// TestHubStatus_NeverChangesExitCode is AC12: a degraded hub is advisory in
// EVERY state, so `prox status` keeps exiting 0 while the local proxy is
// healthy, and the existing non-zero exits are untouched.
func TestHubStatus_NeverChangesExitCode(t *testing.T) {
	for _, state := range []string{
		hubStateConnected, hubStateConnecting, hubStateReconnecting,
		hubStateDisplaced, hubStateProtocolMismatch, hubStateAuthFailed,
		hubStateNameHeld,
	} {
		t.Run(state, func(t *testing.T) {
			p := &api.ProxyStatusResponse{
				Mode:            proxyModeShared,
				DaemonReachable: true,
				Hub:             &api.HubStatusResponse{Alias: "llt", State: state},
			}
			assert.False(t, sharedProxyDown(p), "the hub must never make the LOCAL proxy look down")
			assert.NoError(t, statusExitError(sharedProxyDown(p), nil, nil, nil))
		})
	}

	// And the existing non-zero exits still fire, with a hub present.
	down := &api.ProxyStatusResponse{
		Mode:            proxyModeShared,
		DaemonReachable: false,
		Hub:             &api.HubStatusResponse{Alias: "llt", State: hubStateConnected},
	}
	assert.True(t, sharedProxyDown(down))
	assert.Error(t, statusExitError(sharedProxyDown(down), nil, nil, nil))
	assert.Error(t, statusExitError(false, []string{"web"}, nil, nil))
}

// TestProxyRuntime_HubBlockAbsentWithoutAHub is AC1's negative space, asserted
// BYTE FOR BYTE (plan 031, review A12).
//
// "Marshal it and check the string does not contain \"hub\"" is a weaker claim
// than AC1 makes: it would pass for a payload that had gained some other key,
// or lost one, or renamed one — any of which breaks a client that predates hub
// mode just as thoroughly. The golden document below is the whole payload, so
// ANY drift in a hub-less run fails here, and the hub key's absence is one
// consequence of that rather than the only thing checked.
func TestProxyRuntime_HubBlockAbsentWithoutAHub(t *testing.T) {
	rt := newProxyRuntime()
	rt.SetMode(proxyModeStandalone)

	got, err := json.Marshal(rt.ProxyStatus())
	require.NoError(t, err)

	// Every key a hub-less status carries, in struct order. Update this only
	// when the payload is INTENTIONALLY changed for everyone.
	want := `{"mode":"standalone","daemon_reachable":false,` +
		`"consecutive_failures":0,"dropped_events":0,"backfill_failures":0,"capture_enabled":false}`
	assert.Equal(t, want, string(got),
		"a run with no hub must serialize exactly as it did before hub mode existed")

	// And the hub key appears only once a hub state exists.
	rt.SetHubState(&hubRuntimeState{Alias: "llt", State: hubStateConnected, Routes: 2})
	withHub, err := json.Marshal(rt.ProxyStatus())
	require.NoError(t, err)
	assert.Contains(t, string(withHub), `"hub":{`)

	hub := rt.ProxyStatus().Hub
	require.NotNil(t, hub)
	assert.Equal(t, "llt", hub.Alias)
	assert.Equal(t, 2, hub.Routes)
}

// --- classification and rendering helpers ---

func TestClassifyHubFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want hubFailureClass
	}{
		{"dial error", fmt.Errorf("dial tcp: connection refused"), hubFailureRetryable},
		{"protocol", &proxyd.DaemonAPIError{Code: "PROTOCOL_MISMATCH", Status: 409}, hubFailureProtocol},
		{"unauthorized", &proxyd.DaemonAPIError{Code: "UNAUTHORIZED", Status: 401}, hubFailureAuth},
		{"bare 401", &proxyd.DaemonAPIError{Status: 401, Message: "no"}, hubFailureAuth},
		{"name held", &proxyd.DaemonAPIError{Code: "HUB_NAME_HELD", Status: 409}, hubFailureNameHeld},
		{"hub disabled", &proxyd.DaemonAPIError{Code: "HUB_DISABLED", Status: 503}, hubFailureRetryable},
		{"unknown code", &proxyd.DaemonAPIError{Code: "SOMETHING_NEW", Status: 500}, hubFailureRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyHubFailure(tc.err))
		})
	}
}

func TestHubFailureReason_IsOneShortLine(t *testing.T) {
	assert.Equal(t, "timed out", hubFailureReason(context.DeadlineExceeded))
	assert.Equal(t, "hub returned 503 HUB_DISABLED", hubFailureReason(&proxyd.DaemonAPIError{Code: "HUB_DISABLED", Status: 503}))
	assert.Equal(t, "unknown error", hubFailureReason(nil))

	long := hubFailureReason(fmt.Errorf("a\nb\n%s", strings.Repeat("x", 400)))
	assert.NotContains(t, long, "\n", "a warning is rendered line by line; an embedded newline would become a second line")
	assert.LessOrEqual(t, len(long), hubReasonMaxLen+3)
}

func TestHubPreambleLine(t *testing.T) {
	tests := []struct {
		name       string
		info       *proxyd.RegisterHubInfo
		registered []string
		want       string
	}{
		{
			name:       "https on the default port omits it",
			info:       &proxyd.RegisterHubInfo{Domain: "llt.stridelabs.ai", HTTPSPort: 443},
			registered: []string{"authapi.llt.stridelabs.ai", "auth.llt.stridelabs.ai"},
			want:       "Hub (llt): https://*.llt.stridelabs.ai — auth.llt.stridelabs.ai, authapi.llt.stridelabs.ai",
		},
		{
			name:       "a non-default port is shown",
			info:       &proxyd.RegisterHubInfo{Domain: "hub.test", HTTPSPort: 18443},
			registered: []string{"web.hub.test"},
			want:       "Hub (llt): https://*.hub.test:18443 — web.hub.test",
		},
		{
			name:       "http and https together",
			info:       &proxyd.RegisterHubInfo{Domain: "hub.test", HTTPPort: 80, HTTPSPort: 443},
			registered: []string{"web.hub.test"},
			want:       "Hub (llt): http://*.hub.test, https://*.hub.test — web.hub.test",
		},
		{
			name:       "an older hub that reports no facts still names the hostnames",
			info:       nil,
			registered: []string{"web.hub.test"},
			want:       "Hub (llt): web.hub.test",
		},
		{
			name:       "nothing at all",
			info:       nil,
			registered: nil,
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hubPreambleLine("llt", tc.info, tc.registered))
		})
	}
}

// --- the inline-token advisory ---

func TestWarnInlineHubToken(t *testing.T) {
	inGit := func(bool) gitWorkTreeChecker { return func(string) bool { return true } }
	notGit := gitWorkTreeChecker(func(string) bool { return false })

	cfgWithInline := &config.Config{Hubs: map[string]config.HubConfig{
		"llt": {URL: "http://hub:8443", Token: "s3cr3t"},
	}}
	cfgWithFile := &config.Config{Hubs: map[string]config.HubConfig{
		"llt": {URL: "http://hub:8443", TokenFile: "/x/llt.token"},
	}}

	t.Run("inline token inside a work tree warns", func(t *testing.T) {
		s := newWarningSink()
		warnInlineHubToken(s, cfgWithInline, "llt", "/proj", inGit(true))
		require.Len(t, s.Warnings(), 1)
		assert.Equal(t, warningCodeHubInlineToken, s.Warnings()[0].Code)
	})

	t.Run("inline token outside a work tree is silent", func(t *testing.T) {
		s := newWarningSink()
		warnInlineHubToken(s, cfgWithInline, "llt", "/proj", notGit)
		assert.Empty(t, s.Warnings())
	})

	t.Run("token_file is never warned about", func(t *testing.T) {
		s := newWarningSink()
		warnInlineHubToken(s, cfgWithFile, "llt", "/proj", inGit(true))
		assert.Empty(t, s.Warnings())
	})

	t.Run("an alias that lives only in the user file is never warned about", func(t *testing.T) {
		s := newWarningSink()
		warnInlineHubToken(s, cfgWithInline, "other", "/proj", inGit(true))
		assert.Empty(t, s.Warnings())
	})
}

// TestInsideGitWorkTree exercises the real gate both ways, so the production
// checker is not merely assumed to work.
func TestInsideGitWorkTree(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, insideGitWorkTree(dir), "a bare temp dir is not a work tree")

	// The repository this test runs in is one.
	wd, err := os.Getwd()
	require.NoError(t, err)
	if _, err := os.Stat(filepath.Join(wd, "..", "..", ".git")); err == nil {
		assert.True(t, insideGitWorkTree(wd))
	}
}

// TestHubHolderDetail renders the holder tail used by both the warning and the
// `Hub: … (displaced: …)` line.
func TestHubHolderDetail(t *testing.T) {
	err := &proxyd.DaemonAPIError{Code: "HUB_NAME_HELD", Holders: []proxyd.HubHolder{
		{Hostname: "auth.llt.test", Origin: "mac", ProjectDir: "/home/c/slauth"},
		{Hostname: "api.llt.test", Origin: "vm2", ProjectDir: "/srv/app"},
	}}
	assert.Equal(t, "api.llt.test held by vm2:/srv/app, auth.llt.test held by mac:/home/c/slauth", hubHolderDetail(err))
	assert.Equal(t, "a service name held by another publisher", hubHolderDetail(fmt.Errorf("boom")))
}
