package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/version"
)

// This file is the PUBLISHER half of remote-hub publishing (plan 031 C5): the
// `prox up --hub` flag precedence, the failure classification that decides what
// a hub problem costs the user, the one goroutine that owns the publisher state
// machine, and the name-collision prompt.
//
// The governing rule is D9: publishing is ADDITIVE. The local registration (or
// the standalone fallback) happens first and is untouched by anything here;
// hub publishing is layered on top of a session that already works. Exactly one
// class of hub problem fails `prox up` — a fatal CLI/config error, resolved
// before anything starts — and every other class leaves the command at exit 0
// with the local proxy behaving exactly as it would with no hub configured
// (§3.1).

// proxHubEnvVar selects a hub for one machine without touching either config
// file (plan 031 D8). It sits between `--hub` and `proxy.hub` in precedence.
const proxHubEnvVar = "PROX_HUB"

// Publisher states (plan 031 D19). The machine is
// disabled → resolving → registering → connecting → connected, with
// `reconnecting` on any retryable failure and three terminal states that stop
// retrying for good. They are the wire values of status.proxy.hub.state.
const (
	hubStateRegistering      = "registering"
	hubStateConnecting       = "connecting"
	hubStateConnected        = "connected"
	hubStateReconnecting     = "reconnecting"
	hubStateDisplaced        = "displaced"
	hubStateProtocolMismatch = "protocol_mismatch"
	hubStateAuthFailed       = "auth_failed"
)

// Warning codes for the advisories this file raises. They are local to the CLI
// (unlike domain.WarningCode*, which the daemon also produces) because every
// one of them is observed here, on the publisher, and never travels a wire.
const (
	warningCodeHubUnreachable      = "hub_unreachable"
	warningCodeHubUnknownAlias     = "hub_unknown_alias"
	warningCodeHubProtocolMismatch = "hub_protocol_mismatch"
	warningCodeHubAuthFailed       = "hub_auth_failed"
	warningCodeHubNameHeld         = "hub_name_held"
	warningCodeHubDisplaced        = "hub_displaced"
	warningCodeHubInlineToken      = "hub_inline_token"
	warningCodeHubNoProxy          = "hub_no_proxy"
)

// --- D8: flag/env/config precedence ---

// hubSelectionInputs is everything resolveHubSelection needs, lifted out of
// cobra and the environment so the precedence rules are a pure table.
type hubSelectionInputs struct {
	// FlagSet reports whether --hub was typed at all, and FlagValue is what it
	// parsed to. Both are needed: `--hub ""` is a mistake worth reporting,
	// while an absent flag simply defers to the next source.
	FlagSet   bool
	FlagValue string
	// NoHub is --no-hub: publish to nothing for this run.
	NoHub bool
	// Env/EnvPresent are PROX_HUB as os.LookupEnv reports it.
	Env        string
	EnvPresent bool
	// ConfigHub is proxy.hub from the project's prox.yaml.
	ConfigHub string
}

// hubSelection is which hub a run selected, and how.
type hubSelection struct {
	// Alias is the selected alias, "" when this run publishes to no hub. It may
	// still be the literal "default", which ResolveHub expands against
	// ~/.prox/hubs.yaml.
	Alias string
	// Explicit is true when the alias was named by the person running the
	// command — on the command line or in their own environment — as opposed to
	// being read out of a committed prox.yaml.
	//
	// It is the whole of §3.1's unknown-alias split: an alias typed here is a
	// mistake the user can fix right now, so an unknown one is fatal, while an
	// alias committed in prox.yaml must never break `prox up` for a teammate
	// who has no such hub, so an unknown one warns and continues. PROX_HUB
	// counts as explicit because it is the user's own per-machine choice, not
	// something that travels in the repository.
	Explicit bool
	// Source is "flag", "env", "config", or "" — used only in messages.
	Source string
}

// resolveHubSelection implements D8's precedence: --hub > PROX_HUB >
// proxy.hub, with --no-hub disabling the whole thing for one run.
//
// It is pure and returns an error for exactly two misuses, both of which fail
// `prox up` under §3.1's fatal row: --hub together with --no-hub (the two
// contradict each other, and silently picking one would surprise whoever typed
// the other), and an empty --hub value.
func resolveHubSelection(in hubSelectionInputs) (hubSelection, error) {
	flagValue := strings.TrimSpace(in.FlagValue)

	if in.NoHub && in.FlagSet {
		return hubSelection{}, fmt.Errorf("--hub and --no-hub cannot be used together")
	}
	if in.NoHub {
		return hubSelection{}, nil
	}
	if in.FlagSet {
		if flagValue == "" {
			return hubSelection{}, fmt.Errorf("--hub requires a hub alias (e.g. --hub llt, or --hub default for the ~/.prox/hubs.yaml default)")
		}
		return hubSelection{Alias: flagValue, Explicit: true, Source: "flag"}, nil
	}
	// An empty PROX_HUB reads as unset rather than as an error: exporting an
	// empty variable is the ordinary way to neutralize one in a shell profile,
	// and turning that into a hard failure would break `prox up` for a reason
	// the user thought they had removed.
	if env := strings.TrimSpace(in.Env); in.EnvPresent && env != "" {
		return hubSelection{Alias: env, Explicit: true, Source: "env"}, nil
	}
	if cfgHub := strings.TrimSpace(in.ConfigHub); cfgHub != "" {
		return hubSelection{Alias: cfgHub, Explicit: false, Source: "config"}, nil
	}
	return hubSelection{}, nil
}

// hubStartToken reads this process's opaque start token for the hub register,
// the same value the local registration sends. A failure yields 0, which the
// daemon reads as "fall back to bare PID" — and which costs a REMOTE
// registration nothing at all, since the hub never probes a publisher's PID
// (D17/P4) and settles same-key conflicts by the key itself.
func hubStartToken() int64 {
	token, _ := daemon.ProcessStartTime(os.Getpid())
	return token
}

// proxyHubAlias reads proxy.hub off a config that may have no proxy block at
// all (a `services:`-less or proxy-less prox.yaml is legal).
func proxyHubAlias(cfg *config.Config) string {
	if cfg == nil || cfg.Proxy == nil {
		return ""
	}
	return cfg.Proxy.Hub
}

// hubResolution is the outcome of the EARLY, pre-startup hub resolution: either
// a usable hub (Enabled, with a client already built and therefore a URL
// already validated), or nothing — possibly with one advisory to report once
// the session's warning sink exists.
//
// Resolving early is what makes §3.1's fatal row say "exits non-zero, before
// anything starts": a typo'd `--hub` alias or a malformed url must not take a
// supervisor, a proxy and a set of child processes down with it.
type hubResolution struct {
	Enabled   bool
	Selection hubSelection
	Hub       config.ResolvedHub
	Client    *proxyd.Client
	// Warning is a non-fatal resolution problem to hand to the warning sink
	// later, when one exists. Deferred rather than printed here because D19
	// routes every hub advisory through the sink and the preamble, which is how
	// a `prox up -d` parent replays it on the terminal the user is looking at.
	Warning *domain.Warning
}

// resolveHubPublishing turns flags, environment and config into a hub
// resolution, returning an error ONLY for §3.1's fatal class.
//
// configPath is the project config's path, used to resolve a relative
// token_file against the file that named it.
func resolveHubPublishing(cfg *config.Config, configPath string, in hubSelectionInputs) (hubResolution, error) {
	sel, err := resolveHubSelection(in)
	if err != nil {
		return hubResolution{}, err
	}
	if sel.Alias == "" {
		return hubResolution{}, nil
	}

	hub, err := config.ResolveHub(cfg, configPath, sel.Alias)
	if err != nil {
		// The ONE split (§3.1). An alias this machine does not define is fatal
		// when the user named it, and advisory when a committed prox.yaml did.
		// Every other resolution failure — two token sources, an unreadable
		// token_file, an unset token_env — is a config error the user must fix
		// either way, so it is fatal regardless of how the alias was chosen.
		if errors.Is(err, config.ErrUnknownHub) && !sel.Explicit {
			return hubResolution{Warning: &domain.Warning{
				Code:    warningCodeHubUnknownAlias,
				Message: fmt.Sprintf("proxy.hub names hub %q, which this machine does not define; continuing with local proxy only", sel.Alias),
				Hint:    "Run 'prox hub add " + sel.Alias + " <url>' to publish through it, or remove proxy.hub from prox.yaml.",
			}}, nil
		}
		return hubResolution{}, err
	}

	// Building the client here is what makes a malformed url fatal before
	// anything starts: NewHubClient validates and normalizes the URL and does
	// no I/O, so this costs nothing and fails at the only moment where failing
	// is free.
	client, err := proxyd.NewHubClient(hub.URL, hub.Token, hub.Origin)
	if err != nil {
		return hubResolution{}, fmt.Errorf("hub %q: %w", hub.Alias, err)
	}

	return hubResolution{Enabled: true, Selection: sel, Hub: hub, Client: client}, nil
}

// --- §3.1: failure classification ---

// hubFailureClass is which row of plan 031's §3.1 table a hub failure lands in.
// Only the classes reachable AFTER resolution appear here — the fatal row is
// resolveHubPublishing's returned error, and the non-explicit config row is its
// deferred warning.
type hubFailureClass string

const (
	// hubFailureRetryable is §3.1's "Retryable" row plus its "Hub-side
	// registration failure" row, which behave identically from here: one
	// warning, retry forever, `Hub: … (reconnecting, down <t>)`.
	hubFailureRetryable hubFailureClass = "retryable"
	// hubFailureProtocol is a PROTOCOL_MISMATCH: terminal, because retrying
	// cannot change either binary's wire version.
	hubFailureProtocol hubFailureClass = "protocol_mismatch"
	// hubFailureAuth is UNAUTHORIZED: terminal, because retrying cannot change
	// a credential that is on disk.
	hubFailureAuth hubFailureClass = "auth_failed"
	// hubFailureNameHeld is HUB_NAME_HELD, the only class with a decision in it
	// (D10): prompt, take over, or continue without hub routes.
	hubFailureNameHeld hubFailureClass = "name_held"
)

// classifyHubFailure maps a register/tunnel error onto §3.1. It matches on the
// daemon's machine-readable Code, never on message text, following the
// DaemonAPIError precedent.
//
// The default is RETRYABLE, deliberately: an unrecognized failure is far more
// likely to be a hub mid-restart, a proxy in the way, or a version of the hub
// that reports something this binary has not heard of than it is to be
// permanent — and the cost of guessing wrong is a background retry loop nobody
// sees, whereas the cost of guessing "terminal" wrong is a hub that never comes
// back without a restart.
func classifyHubFailure(err error) hubFailureClass {
	var apiErr *proxyd.DaemonAPIError
	if !errors.As(err, &apiErr) {
		return hubFailureRetryable
	}
	switch apiErr.Code {
	case "PROTOCOL_MISMATCH":
		return hubFailureProtocol
	case "UNAUTHORIZED":
		return hubFailureAuth
	case "HUB_NAME_HELD":
		return hubFailureNameHeld
	}
	if apiErr.Status == 401 || apiErr.Status == 403 {
		// A hub (or something in front of it) that refuses the credential
		// without the daemon's own code is still an auth failure.
		return hubFailureAuth
	}
	return hubFailureRetryable
}

// hubFailureReason renders a SHORT, single-line reason for the warning line, so
// AC11's `Warning: hub llt unreachable (<reason>); …` stays one line whatever
// the transport said.
func hubFailureReason(err error) string {
	switch {
	case err == nil:
		return "unknown error"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup failed"
	}
	var apiErr *proxyd.DaemonAPIError
	if errors.As(err, &apiErr) {
		if apiErr.Code != "" {
			return fmt.Sprintf("hub returned %d %s", apiErr.Status, apiErr.Code)
		}
		return fmt.Sprintf("hub returned %d", apiErr.Status)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	return oneLine(err.Error())
}

// hubReasonMaxLen bounds a fallback reason so one warning stays one readable
// line; the full error is still available in .prox/prox.log.
const hubReasonMaxLen = 120

// oneLine collapses an error string to a single bounded line. A warning is
// rendered line-by-line by formatWarning, so an embedded newline would silently
// turn one advisory into two and break AC11's "exactly one warning line".
func oneLine(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > hubReasonMaxLen {
		return s[:hubReasonMaxLen-1] + "…"
	}
	return s
}

// --- D19: the publisher state machine ---

// hubPublisher owns one project's hub publishing for the life of a `prox up`.
//
// ONE goroutine drives it after startup: the tunnel loop (proxyd.RunTunnel),
// which reports connect/disconnect through this type as a
// proxyd.ForwarderStatusSink and re-registers through the reregister callback
// when the hub says the registration is gone. There is deliberately no second
// retry loop alongside it — two loops is how duplicate registrations and
// repeated warnings happen (D19).
type hubPublisher struct {
	rt     *proxyRuntime
	client *proxyd.Client
	alias  string
	// key is HubProjectKey(origin, dir): the identity the hub filed this
	// project under, and the single value both the tunnel and the request
	// forwarder address it by.
	key     string
	baseReq proxyd.RegisterRequest
	// deregisterReq is the teardown call, prepared up front so shutdown needs
	// nothing but the publisher.
	deregisterReq proxyd.DeregisterRequest

	mu sync.Mutex
	// warned latches D19's "exactly one warning on the first entry into a
	// non-connected state"; every later transition logs instead.
	warned bool
	// domain/routes are the last successful registration's published facts,
	// carried across reconnects so the `Hub:` line keeps naming them.
	domain string
	routes int
	// registered records that at least one register has succeeded, which is
	// what makes a deregister at shutdown worth sending at all.
	registered bool
	// state/detail/since are the published state machine position. since is the
	// instant the STATE was entered, not the last attempt within it, so
	// "reconnecting, down 12s" measures the outage rather than the gap since
	// the most recent retry.
	state  string
	detail string
	since  time.Time

	cancel context.CancelFunc
	// tunnelCtx is the context the tunnel and forwarder run under; the
	// reregister callback uses it so a teardown cancels an in-flight retry.
	tunnelCtx context.Context

	// run starts the two long-lived goroutines. It is a field, defaulting to
	// runHubTunnel, so the §3.1 classification table can pin WHETHER a given
	// failure retries without standing up real tunnels for every row.
	run tunnelRunner
}

// tunnelRunner starts a publisher's reverse tunnel and request forwarder under
// ctx. See hubPublisher.run.
type tunnelRunner func(ctx context.Context, p *hubPublisher, localRM *proxy.RequestManager)

// runHubTunnel is the production runner: the reverse tunnel (D2) plus the
// request forwarder that bridges hub-side captured records into this project's
// TUI and API, exactly as the local shared-daemon path does.
func runHubTunnel(ctx context.Context, p *hubPublisher, localRM *proxy.RequestManager) {
	go proxyd.RunTunnel(ctx, p.client, p.key, p.baseReq.Services, p.reregister, p)
	if localRM != nil {
		// nil sink and nil heal: the hub forwarder must NOT drive the publisher
		// state machine (the tunnel owns it, D19) and must never re-ensure a
		// LOCAL daemon on a remote hub's behalf.
		go proxyd.ForwardRequestsWithClient(ctx, p.client, p.key, localRM, nil, nil)
	}
}

// setState publishes the state machine's position to the runtime (and
// therefore to `prox status` and GET /status), logging one line per actual
// TRANSITION. Logging every transition is what D19 trades for warning only
// once: the state is always visible in `prox status`, and the history is always
// in .prox/prox.log.
//
// Re-entering the state you are already in — which is what a retry loop does,
// once every few seconds, for as long as a hub stays down — is NOT a transition:
// it refreshes the detail and leaves `since` alone, so the log stays quiet and
// "down 12s" keeps measuring the outage rather than the last attempt.
func (p *hubPublisher) setState(state, detail string) (changedState bool) {
	p.mu.Lock()
	changed := state != p.state
	if changed {
		p.since = time.Now()
	}
	p.state, p.detail = state, detail
	st := &hubRuntimeState{
		Alias:  p.alias,
		State:  p.state,
		Domain: p.domain,
		Routes: p.routes,
		Since:  p.since,
		Detail: p.detail,
	}
	p.mu.Unlock()

	p.rt.SetHubState(st)
	if !changed {
		return false
	}
	if detail != "" {
		log.Printf("prox: hub %s: %s (%s)", p.alias, state, detail)
	} else {
		log.Printf("prox: hub %s: %s", p.alias, state)
	}
	return true
}

// degrade is setState for a non-connected state, plus D19's single warning.
//
// A retry that fails the same way as the last one is not a transition and says
// nothing at all: without that gate a hub that stays down would write the same
// warning to .prox/prox.log every few seconds forever, which is the flood D19's
// "exactly one warning" rule exists to prevent in the first place.
func (p *hubPublisher) degrade(state, detail string, w domain.Warning) {
	if !p.setState(state, detail) {
		return
	}
	p.warnOnce(w)
}

// warnOnce raises w through the session's warning SINK the first time this
// publisher leaves the happy path, and logs it on every later occasion.
//
// The sink, never fmt.Printf (D19): startup renders the sink once, on runUp's
// own goroutine, and publishes it on GET /status, which is the only way a
// `prox up -d` child's advisory reaches the parent's terminal. A warning raised
// AFTER startup has already rendered would otherwise be invisible until
// somebody ran `prox status`, so it is additionally logged — the sink's own
// sealed latch is exactly the "startup has rendered" signal, and the forwarder
// self-heal path (proxyRuntime.heal) resolves the same question the same way.
func (p *hubPublisher) warnOnce(w domain.Warning) {
	p.mu.Lock()
	first := !p.warned
	p.warned = true
	p.mu.Unlock()

	sink := p.rt.WarningSink()
	if !first {
		logWarning(w)
		return
	}
	added := sink.Add(w)
	if len(added) > 0 && sink.WarningsSealed() {
		logWarning(w)
	}
}

// logWarning writes a warning to the stdlib logger (and therefore through the
// stdio sink to a TUI's log pane or plain stderr), line by line, exactly as
// reportStartupWarnings would have rendered it.
func logWarning(w domain.Warning) {
	for _, line := range formatWarning(w) {
		log.Print(line)
	}
}

// recordRegistration remembers a successful registration's published facts.
func (p *hubPublisher) recordRegistration(resp *proxyd.RegisterResponse) {
	p.mu.Lock()
	if resp.Hub != nil {
		p.domain = resp.Hub.Domain
	}
	p.routes = len(resp.Registered)
	p.registered = true
	p.mu.Unlock()
}

// everRegistered reports whether any register has succeeded.
func (p *hubPublisher) everRegistered() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registered
}

// register sends one register to the hub, bounded by ctx.
func (p *hubPublisher) register(ctx context.Context, takeover bool) (*proxyd.RegisterResponse, error) {
	req := p.baseReq
	req.Takeover = takeover
	return p.client.RegisterWithContext(ctx, req)
}

// --- proxyd.ForwarderStatusSink: the tunnel drives the last two states ---

// ForwarderConnected fires when the tunnel session is live. This is the only
// transition INTO connected: a successful register alone leaves the publisher
// `connecting`, because until the tunnel attaches the hub has routes it cannot
// serve.
func (p *hubPublisher) ForwarderConnected() {
	p.setState(hubStateConnected, "")
}

// ForwarderConnectFailed fires on every failed tunnel connect. It is the
// retryable row of §3.1 in its steady state: one warning the first time,
// silence (well — a log line) forever after, and `reconnecting` in
// `prox status` the whole time.
func (p *hubPublisher) ForwarderConnectFailed(err error) {
	p.degrade(hubStateReconnecting, hubFailureReason(err), hubUnreachableWarning(p.alias, err))
}

// ForwarderBackfillFailed is not a publisher-state event: the tunnel never
// backfills. It exists to satisfy the sink interface.
func (p *hubPublisher) ForwarderBackfillFailed() {}

// reregister is RunTunnel's 404 NOT_REGISTERED callback (D19/C4): the hub has
// no registration for this key, so the handshake alone can never succeed. It is
// also the path by which a hub that was DOWN at `prox up` time acquires this
// project's routes with no user action at all (AC11) — the tunnel loop keeps
// dialing, the hub comes up, answers 404, and this puts the registration back.
//
// Its other job is the terminal states. A re-register that comes back
// HUB_NAME_HELD means a later publisher took the name with --hub-takeover
// (D10): the right answer is to stop, not to fight for it, so the tunnel
// context is cancelled and the loop exits.
func (p *hubPublisher) reregister() error {
	ctx, cancel := context.WithTimeout(p.tunnelCtx, constants.HubUnaryTimeout)
	defer cancel()

	resp, err := p.register(ctx, false)
	if err != nil {
		switch classifyHubFailure(err) {
		case hubFailureNameHeld:
			p.degrade(hubStateDisplaced, hubHolderDetail(err), hubDisplacedWarning(p.alias, err))
			p.stop()
		case hubFailureProtocol:
			p.degrade(hubStateProtocolMismatch, hubProtocolDetail(err), hubProtocolWarning(p.alias, err))
			p.stop()
		case hubFailureAuth:
			p.degrade(hubStateAuthFailed, "", hubAuthWarning(p.alias))
			p.stop()
		}
		return err
	}
	p.recordRegistration(resp)
	return nil
}

// stop cancels the tunnel and its request forwarder. Used both by the terminal
// states above and by shutdown.
func (p *hubPublisher) stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// start hands the publisher to its runner on a context derived from the run's
// but cancellable on its own, so performShutdown can stop the tunnel BEFORE
// deregistering (D6c) without disturbing the supervisor.
func (p *hubPublisher) start(ctx context.Context, localRM *proxy.RequestManager) {
	tctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.tunnelCtx = tctx
	p.rt.SetHubCancel(cancel)
	p.run(tctx, p, localRM)
}

// Shutdown stops the tunnel and deregisters from the hub, bounded by timeout.
//
// It follows the same D6c ordering the local path uses and for the same reason:
// cancel the tunnel FIRST, so its reregister callback cannot put the
// registration back a moment after the deregister removed it.
func (p *hubPublisher) Shutdown(timeout time.Duration) {
	p.stop()

	// A publisher that never registered has nothing on the hub to remove, and
	// the hub is usually the reason it never registered — so calling anyway
	// would put a failed-deregister warning on the terminal of every teardown
	// of the AC11 "hub is down" session, for a call that could not have
	// succeeded and would not have mattered if it had.
	if !p.everRegistered() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := p.client.DeregisterWithContext(ctx, p.deregisterReq); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to deregister from hub %s: %v\n", p.alias, err)
	}
}

// --- warnings ---

func hubUnreachableWarning(alias string, err error) domain.Warning {
	// AC11 pins this sentence word for word, including that it is ONE line: no
	// hint, because formatWarning renders a hint as a second line and the
	// acceptance criterion counts lines.
	return domain.Warning{
		Code:    warningCodeHubUnreachable,
		Message: fmt.Sprintf("hub %s unreachable (%s); continuing with local proxy only, will keep retrying", alias, hubFailureReason(err)),
	}
}

func hubProtocolWarning(alias string, err error) domain.Warning {
	return domain.Warning{
		Code:    warningCodeHubProtocolMismatch,
		Message: fmt.Sprintf("hub %s speaks a different hub protocol (%s); continuing with local proxy only", alias, hubProtocolDetail(err)),
		Hint:    "Upgrade the older prox — the hub host and this machine must agree on the hub protocol.",
	}
}

func hubAuthWarning(alias string) domain.Warning {
	return domain.Warning{
		Code:    warningCodeHubAuthFailed,
		Message: fmt.Sprintf("hub %s rejected this machine's token; continuing with local proxy only", alias),
		Hint:    "Check the token in ~/.prox/hubs.yaml against 'prox hub token' on the hub host.",
	}
}

func hubNameHeldWarning(alias string, err error) domain.Warning {
	return domain.Warning{
		Code:    warningCodeHubNameHeld,
		Message: fmt.Sprintf("hub %s already publishes %s; continuing with local proxy only", alias, hubHolderDetail(err)),
		Hint:    "Re-run with --hub-takeover to take the name(s), or stop the other publisher.",
	}
}

// hubNoProxyWarning explains a hub that resolved but has nothing to publish
// through, because this run has no proxy at all (D9: publishing is layered on
// the local path, not an alternative to it).
func hubNoProxyWarning(alias string) domain.Warning {
	return domain.Warning{
		Code:    warningCodeHubNoProxy,
		Message: fmt.Sprintf("hub %s is selected but this run has no proxy (--no-proxy, or proxy.enabled is false); nothing was published", alias),
		Hint:    "Enable the proxy for this project, or drop --hub/--no-proxy.",
	}
}

func hubDisplacedWarning(alias string, err error) domain.Warning {
	return domain.Warning{
		Code:    warningCodeHubDisplaced,
		Message: fmt.Sprintf("hub %s: another publisher took over %s; this project is no longer published there", alias, hubHolderDetail(err)),
	}
}

// hubProtocolDetail renders the "hub 2, this prox 1" tail of the protocol
// mismatch line from the structured field on the error, not from its prose.
func hubProtocolDetail(err error) string {
	var apiErr *proxyd.DaemonAPIError
	if errors.As(err, &apiErr) && apiErr.HubProtocol > 0 {
		return fmt.Sprintf("hub %d, this prox %d", apiErr.HubProtocol, constants.HubProtocolVersion)
	}
	return fmt.Sprintf("this prox %d", constants.HubProtocolVersion)
}

// hubHolderDetail renders the holders of a HUB_NAME_HELD as the one-line tail
// of a warning or a `Hub: … (displaced: …)` line: "auth held by mac:/home/c/app".
func hubHolderDetail(err error) string {
	var apiErr *proxyd.DaemonAPIError
	if !errors.As(err, &apiErr) || len(apiErr.Holders) == 0 {
		return "a service name held by another publisher"
	}
	parts := make([]string, 0, len(apiErr.Holders))
	for _, h := range apiErr.Holders {
		parts = append(parts, fmt.Sprintf("%s held by %s", h.Hostname, hubHolderIdentity(h)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// hubHolderIdentity names one holder. A LOCAL holder carries no origin — it is
// a project on the hub host itself, and D10 says a remote publisher never
// displaces one — so it is labelled rather than shown as ":<dir>".
func hubHolderIdentity(h proxyd.HubHolder) string {
	if h.Origin == "" {
		return "a local project on the hub host (" + h.ProjectDir + ")"
	}
	return h.Origin + ":" + h.ProjectDir
}

// --- D10: the collision prompt ---

// hubCollisionDecision is what to do about a HUB_NAME_HELD.
type hubCollisionDecision string

const (
	// hubCollisionTakeover: --hub-takeover was given, so re-send with
	// takeover:true without asking.
	hubCollisionTakeover hubCollisionDecision = "takeover"
	// hubCollisionAsk: an interactive, attached session lists every holder and
	// asks.
	hubCollisionAsk hubCollisionDecision = "ask"
	// hubCollisionDecline: nobody is there to answer, so continue without hub
	// routes.
	hubCollisionDecline hubCollisionDecision = "decline"
)

// decideHubCollision is D10's prompt rule as a pure function. interactive is
// `isatty(stdin) && isatty(stdout) && !detach` — a detached session has a
// terminal it is about to let go of, so it must never block on a question
// nobody will see.
func decideHubCollision(takeoverFlag, interactive bool) hubCollisionDecision {
	switch {
	case takeoverFlag:
		return hubCollisionTakeover
	case interactive:
		return hubCollisionAsk
	default:
		return hubCollisionDecline
	}
}

// formatHubHolders renders the prompt's holder listing: EVERY conflicting
// holder, one per line (D10 — a project whose two services are held by two
// different publishers needs both names in front of the user, because
// registration is all-or-nothing).
func formatHubHolders(alias string, holders []proxyd.HubHolder) []string {
	if len(holders) == 0 {
		return []string{fmt.Sprintf("Hub %s already publishes a service name this project registers.", alias)}
	}
	lines := []string{fmt.Sprintf("Hub %s already publishes %d service name(s) this project registers:", alias, len(holders))}
	rows := make([]string, 0, len(holders))
	for _, h := range holders {
		state := "disconnected"
		if h.Connected {
			state = "connected"
		}
		rows = append(rows, fmt.Sprintf("  %s — %s (%s)", h.Hostname, hubHolderIdentity(h), state))
	}
	sort.Strings(rows)
	return append(lines, rows...)
}

// askHubTakeover prints the holder listing and reads one line from in. Anything
// but an explicit yes declines: taking a name away from a running publisher is
// not a default.
func askHubTakeover(out io.Writer, in io.Reader, alias string, holders []proxyd.HubHolder) bool {
	for _, line := range formatHubHolders(alias, holders) {
		fmt.Fprintln(out, line)
	}
	fmt.Fprint(out, "Take it over? [y/N]: ")

	reader := bufio.NewReader(in)
	answer, err := reader.ReadString('\n')
	if err != nil && answer == "" {
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// holdersFrom extracts the structured holder list from a HUB_NAME_HELD error.
func holdersFrom(err error) []proxyd.HubHolder {
	var apiErr *proxyd.DaemonAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Holders
	}
	return nil
}

// --- the inline-token advisory ---

// gitWorkTreeChecker reports whether dir is inside a git work tree. Injectable
// so the advisory's gate can be tested without a repository.
type gitWorkTreeChecker func(dir string) bool

// insideGitWorkTree is the production checker: `git rev-parse
// --is-inside-work-tree`, bounded, with every failure (no git, not a repo, a
// slow filesystem) reading as "no" — the advisory is only ever raised when the
// answer is a definite yes.
func insideGitWorkTree(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// warnInlineHubToken raises the advisory for an inline `token:` in a PROJECT
// prox.yaml that sits inside a git work tree (plan 031 C5): that token is about
// to be committed, and the user should hear about it before it is.
//
// It lands here rather than in internal/config because config has no warning
// channel at all — Validate returns errors only, and the sink lives in this
// package — and because the git gate is a runtime fact about where the project
// happens to live, not a property of the document.
//
// The gate is deliberately narrow: the ~/.prox/hubs.yaml copy of the same key
// is not warned about (that file is the user's own, 0600, and is never
// committed), and a project outside a work tree is not warned about either.
func warnInlineHubToken(sink *warningSink, cfg *config.Config, alias, projectDir string, inGit gitWorkTreeChecker) {
	if cfg == nil || alias == "" || inGit == nil {
		return
	}
	entry, ok := cfg.Hubs[alias]
	if !ok || entry.Token == "" {
		return
	}
	if !inGit(projectDir) {
		return
	}
	sink.Add(domain.Warning{
		Code:    warningCodeHubInlineToken,
		Message: fmt.Sprintf("hubs.%s.token in prox.yaml is an inline secret and this project is inside a git work tree", alias),
		Hint:    "Use token_file: or token_env: instead (see 'prox hub add --token-file/--token-env').",
	})
}

// --- the publish entry point ---

// hubPublishOptions is everything startHubPublishing needs. It is a struct
// because the list is long and mostly same-typed, and because a positional
// mistake between (say) two strings would compile and then publish under the
// wrong identity.
type hubPublishOptions struct {
	Res      hubResolution
	Cfg      *config.Config
	Cwd      string
	Runtime  *proxyRuntime
	Preamble *startupPreamble
	// LocalRM is this project's request manager, which hub-side captured
	// records are forwarded into exactly as local ones are.
	LocalRM *proxy.RequestManager
	// StartToken is this process's start token, shared with the local register.
	StartToken int64
	// Takeover is --hub-takeover.
	Takeover bool
	// Interactive is D10's `isatty(stdin) && isatty(stdout) && !detach`.
	Interactive bool
	Stdin       io.Reader
	Stdout      io.Writer
	InGitTree   gitWorkTreeChecker
	// Run overrides the tunnel runner (nil → runHubTunnel). Tests only.
	Run tunnelRunner
}

// startHubPublishing performs the first, bounded registration and starts the
// publisher (plan 031 C5, D9). It returns the publisher so shutdown can
// deregister, or nil when this run publishes to no hub.
//
// It NEVER returns an error: by the time it runs, §3.1's only fatal class has
// already been settled by resolveHubPublishing, and everything reachable from
// here leaves `prox up` at exit 0 with the local proxy untouched.
//
// It must be called on runUp's own goroutine: it prints the preamble line and
// may ask the user a question, and the preamble is unsynchronized by
// construction.
func startHubPublishing(ctx context.Context, opts hubPublishOptions) *hubPublisher {
	res := opts.Res
	if !res.Enabled {
		return nil
	}
	rt := opts.Runtime

	warnInlineHubToken(rt.WarningSink(), opts.Cfg, res.Hub.Alias, opts.Cwd, opts.InGitTree)

	p := newHubPublisher(opts)
	p.setState(hubStateRegistering, "")

	// The FIRST register is bounded by HubConnectTimeout and nothing else
	// (D16/AC11): a hub that accepts the TCP connection and then never answers
	// must not hold `prox up` past this, and after it the tunnel loop owns
	// every further attempt.
	resp, err := p.registerBounded(ctx, false)
	if err == nil {
		p.onRegistered(resp, opts.Preamble)
		p.start(ctx, opts.LocalRM)
		return p
	}

	switch classifyHubFailure(err) {
	case hubFailureProtocol:
		// Terminal: no tunnel, no retry, and a state `prox status` can explain.
		p.degrade(hubStateProtocolMismatch, hubProtocolDetail(err), hubProtocolWarning(p.alias, err))
		return p

	case hubFailureAuth:
		p.degrade(hubStateAuthFailed, "", hubAuthWarning(p.alias))
		return p

	case hubFailureNameHeld:
		return p.resolveCollision(ctx, err, opts)

	default:
		// Retryable (§3.1): warn once, then hand the whole problem to the
		// tunnel loop. Starting the tunnel even though the registration never
		// landed is the point — when the hub comes up it answers the tunnel
		// upgrade with 404 NOT_REGISTERED, the reregister callback fires, and
		// the routes appear with no user action (AC11).
		p.degrade(hubStateReconnecting, hubFailureReason(err), hubUnreachableWarning(p.alias, err))
		p.start(ctx, opts.LocalRM)
		return p
	}
}

// resolveCollision runs D10's decision for a HUB_NAME_HELD on the FIRST
// register: take it over, ask, or continue without hub routes.
func (p *hubPublisher) resolveCollision(ctx context.Context, err error, opts hubPublishOptions) *hubPublisher {
	holders := holdersFrom(err)

	take := false
	switch decideHubCollision(opts.Takeover, opts.Interactive) {
	case hubCollisionTakeover:
		take = true
		// Not a warning: a takeover the user asked for is a success, not an
		// advisory. It is logged because displacing somebody else's publisher
		// should leave a trace.
		log.Printf("prox: hub %s: taking over %s (--hub-takeover)", p.alias, hubHolderDetail(err))
	case hubCollisionAsk:
		take = askHubTakeover(opts.Stdout, opts.Stdin, p.alias, holders)
	case hubCollisionDecline:
	}

	if !take {
		// §3.1's "HUB_NAME_HELD declined" row: one warning, no retry, and NO
		// `Hub:` state — this run simply is not publishing, which is a
		// different thing from publishing badly.
		p.rt.SetHubState(nil)
		p.warnOnce(hubNameHeldWarning(p.alias, err))
		return nil
	}

	resp, rerr := p.registerBounded(ctx, true)
	if rerr == nil {
		p.onRegistered(resp, opts.Preamble)
		p.start(ctx, opts.LocalRM)
		return p
	}

	switch classifyHubFailure(rerr) {
	case hubFailureNameHeld:
		// Still held even with takeover:true — D10's local holder, which a
		// remote publisher never displaces.
		p.rt.SetHubState(nil)
		p.warnOnce(hubNameHeldWarning(p.alias, rerr))
		return nil
	case hubFailureProtocol:
		p.degrade(hubStateProtocolMismatch, hubProtocolDetail(rerr), hubProtocolWarning(p.alias, rerr))
		return p
	case hubFailureAuth:
		p.degrade(hubStateAuthFailed, "", hubAuthWarning(p.alias))
		return p
	default:
		p.degrade(hubStateReconnecting, hubFailureReason(rerr), hubUnreachableWarning(p.alias, rerr))
		p.start(ctx, opts.LocalRM)
		return p
	}
}

// registerBounded is the startup-path register: bounded by HubConnectTimeout so
// a black-holed hub cannot delay `prox up` (AC11/CodeRabbit M1).
func (p *hubPublisher) registerBounded(ctx context.Context, takeover bool) (*proxyd.RegisterResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, constants.HubConnectTimeout)
	defer cancel()
	return p.register(rctx, takeover)
}

// onRegistered records the published facts, prints the preamble line, and
// leaves the publisher in `connecting` — the tunnel promotes it to `connected`.
func (p *hubPublisher) onRegistered(resp *proxyd.RegisterResponse, pre *startupPreamble) {
	p.recordRegistration(resp)
	if line := hubPreambleLine(p.alias, resp.Hub, resp.Registered); line != "" {
		pre.printf("%s", line)
	}
	p.setState(hubStateConnecting, "")
}

// newHubPublisher builds the publisher and the two requests it will ever send.
func newHubPublisher(opts hubPublishOptions) *hubPublisher {
	hub := opts.Res.Hub
	services := make(map[string]proxyd.ServiceTarget, len(opts.Cfg.Services))
	for name, svc := range opts.Cfg.Services {
		services[name] = proxyd.ServiceTarget{Host: svc.Host, Port: svc.Port}
	}

	run := opts.Run
	if run == nil {
		run = runHubTunnel
	}

	return &hubPublisher{
		rt:     opts.Runtime,
		client: opts.Res.Client,
		alias:  hub.Alias,
		run:    run,
		// The hub composes this same key from the origin and dir it is sent
		// (D15); computing it here from the identical two strings is what makes
		// the tunnel, the request stream and the registration provably address
		// one registration.
		key: proxyd.HubProjectKey(hub.Origin, opts.Cwd),
		baseReq: proxyd.RegisterRequest{
			ProjectDir: opts.Cwd,
			PID:        os.Getpid(),
			Version:    version.Version,
			Services:   services,
			// Domain and both ports are deliberately omitted: the hub owns them
			// and overwrites whatever arrives (D4).
			CaptureEnabled:  opts.Cfg.Proxy.CaptureEffectivelyEnabled(),
			MaxBodySize:     captureMaxBodySize(opts.Cfg),
			DiskBudget:      captureDiskBudget(opts.Cfg),
			StartTime:       opts.StartToken,
			Origin:          hub.Origin,
			ProtocolVersion: constants.HubProtocolVersion,
		},
		deregisterReq: proxyd.DeregisterRequest{
			ProjectDir: opts.Cwd,
			PID:        os.Getpid(),
			Origin:     hub.Origin,
		},
	}
}

// hubPreambleLine renders the one extra startup line (plan 031 §4.2):
//
//	Hub (llt): https://*.llt.stridelabs.ai — auth.llt.stridelabs.ai, authapi.llt.stridelabs.ai
//
// Hostnames are sorted so the line is stable across runs (the registry returns
// them in service-map order, which is not).
func hubPreambleLine(alias string, info *proxyd.RegisterHubInfo, registered []string) string {
	names := append([]string(nil), registered...)
	sort.Strings(names)

	var addrs []string
	if info != nil && info.Domain != "" {
		if info.HTTPPort > 0 {
			addrs = append(addrs, hubWildcardURL("http", info.Domain, info.HTTPPort))
		}
		if info.HTTPSPort > 0 {
			addrs = append(addrs, hubWildcardURL("https", info.Domain, info.HTTPSPort))
		}
	}

	switch {
	case len(addrs) > 0 && len(names) > 0:
		return fmt.Sprintf("Hub (%s): %s — %s", alias, strings.Join(addrs, ", "), strings.Join(names, ", "))
	case len(addrs) > 0:
		return fmt.Sprintf("Hub (%s): %s", alias, strings.Join(addrs, ", "))
	case len(names) > 0:
		return fmt.Sprintf("Hub (%s): %s", alias, strings.Join(names, ", "))
	default:
		return ""
	}
}

// hubWildcardURL renders a hub's wildcard address, omitting the port when it is
// the scheme's default so the common case reads as a plain hostname.
func hubWildcardURL(scheme, domain string, port int) string {
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		return fmt.Sprintf("%s://*.%s", scheme, domain)
	}
	return fmt.Sprintf("%s://*.%s:%d", scheme, domain, port)
}

// --- `prox status` rendering (plan 031 §4.2, AC9) ---

// hubStatusLine renders the `Hub:` line for one publishing state, or "" when
// there is no hub to report. now is injected so the "down 12s" tail is
// table-testable.
//
// EVERY state in §4.2 is rendered here, including the two the drafted tests
// skipped (protocol mismatch, auth failed) — an unexplained missing line is
// worse than a line saying something went wrong.
func hubStatusLine(h *api.HubStatusResponse, now time.Time) string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("Hub: %s (%s)", h.Alias, hubStateDescription(h, now))
}

// hubStateDescription renders the parenthesized half of the `Hub:` line.
func hubStateDescription(h *api.HubStatusResponse, now time.Time) string {
	switch h.State {
	case hubStateConnected:
		return fmt.Sprintf("connected, %s", pluralRoutes(h.Routes))
	case hubStateReconnecting:
		return fmt.Sprintf("reconnecting, down %s", hubStateAge(h, now))
	case hubStateDisplaced:
		if h.Detail != "" {
			return "displaced: " + h.Detail
		}
		return "displaced"
	case hubStateProtocolMismatch:
		if h.Detail != "" {
			return "protocol mismatch: " + h.Detail
		}
		return "protocol mismatch"
	case hubStateAuthFailed:
		return "auth failed"
	default:
		// resolving / registering / connecting, and anything a newer daemon
		// might report: show the state verbatim rather than inventing wording
		// for it.
		return strings.ReplaceAll(h.State, "_", " ")
	}
}

// hubStateAge is how long the current state has held, floored at 0 so a clock
// skew between the daemon and the client cannot print a negative duration.
func hubStateAge(h *api.HubStatusResponse, now time.Time) string {
	if h.Since == nil {
		return "0s"
	}
	d := now.Sub(*h.Since)
	if d < 0 {
		d = 0
	}
	return formatDuration(d)
}

func pluralRoutes(n int) string {
	if n == 1 {
		return "1 route"
	}
	return fmt.Sprintf("%d routes", n)
}
