package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold it open, say nothing. Closed by the listener's own close.
			t.Cleanup(func() { _ = conn.Close() })
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

// hubPublishHarness wires a runtime, a warning sink and a recording tunnel
// runner around startHubPublishing.
type hubPublishHarness struct {
	rt      *proxyRuntime
	sink    *warningSink
	started int
	pre     *startupPreamble
}

func newHubPublishHarness() *hubPublishHarness {
	rt := newProxyRuntime()
	sink := newWarningSink()
	rt.SetWarningSink(sink)
	return &hubPublishHarness{rt: rt, sink: sink, pre: newStartupPreamble(false)}
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
		Run: func(context.Context, *hubPublisher, *proxy.RequestManager) {
			h.started++
		},
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
		wantNil   bool
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
			name: "terminal: name held, declined (non-interactive)",
			url: func(t *testing.T) string {
				return startFakeHub(t, &fakeHubRegister{status: http.StatusConflict, body: heldBody})
			},
			wantWarnings: []string{
				"Warning: hub llt already publishes auth.llt.test held by mac:/home/c/slauth; continuing with local proxy only",
				"         Re-run with --hub-takeover to take the name(s), or stop the other publisher.",
			},
			wantRetries: false,
			wantState:   "",
			wantNil:     true,
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
			start := time.Now()
			p := h.publish(t, ctx, tc.url(t))
			elapsed := time.Since(start)

			// Exit code: startHubPublishing returns no error at all, so there
			// is nothing here that can fail `prox up` (§3.1).
			if tc.wantNil {
				assert.Nil(t, p, "a declined collision publishes nothing")
			} else {
				require.NotNil(t, p)
			}

			var lines []string
			for _, w := range h.sink.Warnings() {
				lines = append(lines, formatWarning(w)...)
			}
			assert.Equal(t, tc.wantWarnings, lines)

			if tc.wantRetries {
				assert.Equal(t, 1, h.started, "the tunnel loop must own the retries")
			} else {
				assert.Equal(t, 0, h.started, "a terminal failure must not retry")
			}

			state := h.rt.HubState()
			if tc.wantState == "" {
				assert.Nil(t, state, "no hub state at all")
			} else {
				require.NotNil(t, state)
				assert.Equal(t, tc.wantState, state.State)
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
func TestHubPublish_WarnsOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHubPublishHarness()
	p := h.publish(t, ctx, refusedHubURL(t))
	require.NotNil(t, p)
	require.Len(t, h.sink.Warnings(), 1)

	outageStart := h.rt.HubState().Since

	// Three more retry failures, exactly as the tunnel loop would report them.
	for i := 0; i < 3; i++ {
		p.ForwarderConnectFailed(fmt.Errorf("still down"))
	}
	assert.Len(t, h.sink.Warnings(), 1, "the retry loop is silent after the first warning")
	assert.Equal(t, hubStateReconnecting, h.rt.HubState().State)
	// "down <t>" must measure the OUTAGE, so a retry that fails the same way
	// does not restart the clock.
	assert.Equal(t, outageStart, h.rt.HubState().Since)

	// And a recovery flips the state without adding anything.
	p.ForwarderConnected()
	assert.Equal(t, hubStateConnected, h.rt.HubState().State)
	assert.Len(t, h.sink.Warnings(), 1)

	// A LATER outage is a real transition — it logs and restarts the outage
	// clock — but it still adds no second warning: the sink is what a
	// `prox up -d` parent replays at startup, and this session has already had
	// its one advisory.
	p.ForwarderConnectFailed(fmt.Errorf("down again"))
	assert.Equal(t, hubStateReconnecting, h.rt.HubState().State)
	assert.Len(t, h.sink.Warnings(), 1)
	assert.True(t, h.rt.HubState().Since.After(outageStart))
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
			got := askHubTakeover(&out, strings.NewReader(tc.answer), "llt", holders)
			assert.Equal(t, tc.want, got)
			assert.Contains(t, out.String(), "Take it over? [y/N]")
			assert.Contains(t, out.String(), "auth.llt.test")
		})
	}
}

// TestHubPublish_TakeoverResendsWithTakeover pins that answering yes (or
// passing --hub-takeover) re-sends the SAME registration with takeover: true.
func TestHubPublish_TakeoverResendsWithTakeover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proxyd.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req.Takeover)
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
			seen = nil
			h := newHubPublishHarness()
			p := h.publish(t, ctx, srv.URL, tc.opt)
			require.NotNil(t, p)
			assert.Equal(t, []bool{false, true}, seen, "first without takeover, then with")
			assert.Empty(t, h.sink.Warnings(), "a takeover the user asked for is not an advisory")
			require.NotNil(t, h.rt.HubState())
			assert.Equal(t, hubStateConnecting, h.rt.HubState().State)
			assert.Equal(t, 1, h.started)
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

// TestProxyRuntime_HubBlockAbsentWithoutAHub is AC1's negative space: a run
// with no hub emits no `hub` key under status.proxy at all.
func TestProxyRuntime_HubBlockAbsentWithoutAHub(t *testing.T) {
	rt := newProxyRuntime()
	rt.SetMode(proxyModeStandalone)
	assert.Nil(t, rt.ProxyStatus().Hub)

	encoded, err := json.Marshal(rt.ProxyStatus())
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "\"hub\"")

	rt.SetHubState(&hubRuntimeState{Alias: "llt", State: hubStateConnected, Routes: 2})
	got := rt.ProxyStatus().Hub
	require.NotNil(t, got)
	assert.Equal(t, "llt", got.Alias)
	assert.Equal(t, 2, got.Routes)
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
