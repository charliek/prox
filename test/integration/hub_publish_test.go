package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/charliek/prox/internal/version"
)

// This file is plan 031 C5's CLI-level integration surface: the REAL `prox`
// binary publishing to a hub, or failing to.
//
// Two harness rules govern everything here.
//
// ISOLATED HOME (the capture_daemon_test.go precedent, P14). Every `prox up`
// resolves its shared proxy daemon through $HOME/.prox, so a test that did not
// move HOME would connect to — and register with — the DEVELOPER's own daemon.
// Each test therefore gets a private HOME with a private socket.
//
// NO FORKED DAEMON. Rather than let `prox up` fork a real shared daemon into
// that HOME, the harness starts one IN-PROCESS on the socket path prox will
// look for, reporting prox's own version so EnsureRunning adopts it. That same
// server carries hub mode on its network mount, which is exactly D1's premise
// (one daemon, one route table) and gives the test direct, synchronous access
// to both sides of what it is asserting.
//
// The end-to-end DATA path through a tunnel is deliberately not tested here: it
// needs TLS and a dynamic proxy with a cert manager, which is why plan 031 P14
// moved those tests into internal/proxyd. What is tested here is what the CLI
// owns — exit codes, warnings, the `Hub:` line, collisions, and teardown.

// hubPublishEnv is one test's isolated publisher environment: a private HOME
// with an in-process shared daemon on the socket prox will find, a real HTTP
// data-plane port, and (optionally) hub mode on a known loopback address.
type hubPublishEnv struct {
	t          *testing.T
	home       string
	socketPath string
	server     *proxyd.Server
	registry   *proxyd.Registry
	client     *proxyd.Client
	// proxyPort is the shared daemon's HTTP data-plane port, which every
	// fixture below registers, so a local route can actually be driven.
	proxyPort int
	// hubAddr is the hub control plane's address once StartHub has run.
	hubAddr string
	// backend is the upstream every fixture's `app` service points at, and
	// backendPort is the port a rendered fixture names for it.
	backend     *httptest.Server
	backendPort int
}

// newHubPublishEnv builds the environment WITHOUT hub mode; call StartHub to
// turn it on, before or after a publisher starts.
func newHubPublishEnv(t *testing.T) *hubPublishEnv {
	t.Helper()

	// A short /tmp path, not t.TempDir(): the Unix socket under it has a
	// ~104-byte limit on macOS and t.TempDir() bakes the test name into the
	// path.
	home, err := os.MkdirTemp("/tmp", "prox-hubhome-")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".prox"), 0o700); err != nil {
		t.Fatalf("create .prox: %v", err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("backend-ok"))
	}))
	t.Cleanup(backend.Close)
	_, backendPort := splitHostPort(t, backend.URL)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	registry := proxyd.NewRegistry()
	managers := proxyd.NewManagers(constants.DefaultProxyRequestBufferSize, nil)
	dynamicProxy := proxyd.NewDynamicProxy(registry, nil, managers, nil, logger)

	socketPath := filepath.Join(home, ".prox", "proxy.sock")
	server := proxyd.NewServer(proxyd.ServerConfig{
		SocketPath: socketPath,
		Logger:     logger,
		// prox's OWN version: EnsureRunning requires an exact match, and a
		// mismatch would send the CLI down the version-skew recovery path
		// instead of registering.
		Version: version.Version,
	})
	server.SetRegistry(registry)
	server.SetProxy(dynamicProxy)
	server.SetManagers(managers)

	go func() { _ = server.Start() }()
	t.Cleanup(func() {
		server.StopHub()
		_ = server.Shutdown(context.Background())
		_ = dynamicProxy.Shutdown(context.Background())
	})

	client := proxyd.NewClient(socketPath)
	waitDaemonReady(t, client, within(t, apiReadyTimeout))

	port, reservation := freePort(t)
	if err := reservation.Close(); err != nil {
		t.Fatalf("release reserved proxy port: %v", err)
	}

	return &hubPublishEnv{
		t: t, home: home, socketPath: socketPath, server: server,
		registry: registry, client: client, proxyPort: port,
		backend: backend, backendPort: backendPort,
	}
}

// StartHub turns hub mode on, either on a caller-chosen address (so a publisher
// can be configured for a hub that does not exist yet) or on a loopback
// ephemeral port, and returns the address actually bound.
func (e *hubPublishEnv) StartHub(listen string) string {
	e.t.Helper()
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if err := e.server.StartHub(proxyd.HubConfig{
		Domain:    "llt.test",
		Listen:    listen,
		HTTPSPort: 0,
		HTTPPort:  e.proxyPort,
		Auth:      proxyd.HubAuthNone,
	}); err != nil {
		e.t.Fatalf("StartHub: %v", err)
	}
	e.hubAddr = e.server.HubListenAddr()
	return e.hubAddr
}

// reservedHubAddr hands back a loopback address nothing is listening on, for
// the AC11 cases where the hub is not running when `prox up` starts: the
// publisher must survive it, and (in the recovery test) the hub must be able to
// come up on exactly that address later.
func reservedHubAddr(t *testing.T) string {
	t.Helper()
	port, reservation := freePort(t)
	if err := reservation.Close(); err != nil {
		t.Fatalf("release reserved hub port: %v", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// fixtureFor renders a publisher project: one long-running process, a local
// HTTP proxy route on the shared daemon's port, and a hubs: entry naming hubURL
// under the alias "llt".
//
// domain differs per publisher so two publishers can register the SAME service
// name without colliding LOCALLY — which is the only way to make them collide
// on the HUB, where D4 gives every publisher the hub's one domain.
func (e *hubPublishEnv) fixtureFor(t *testing.T, domain, hubURL string, hubKey string) *proxFixture {
	t.Helper()
	doc := fmt.Sprintf(`
processes:
  web:
    cmd: ./testdata/scripts/long_running.sh

proxy:
  domain: %s
  http_port: %d
  hub: %s

hubs:
  llt:
    url: %s

services:
  app: %d
`, domain, e.proxyPort, hubKey, hubURL, e.backendPort)
	return newInlineFixture(t, doc)
}

// ac11WarningRe is AC11's warning line, pinned: exactly this sentence, with
// only the reason varying.
var ac11WarningRe = regexp.MustCompile(`Warning: hub llt unreachable \([^)]+\); continuing with local proxy only, will keep retrying`)

// countMatches counts non-overlapping matches of re in s.
func countMatches(re *regexp.Regexp, s string) int {
	return len(re.FindAllString(s, -1))
}

// TestHubPublish_UnreachableHubIsNeverFatal is AC11, for both of the cases the
// panel added: a REFUSED port and a BLACK HOLE (a listener that accepts and
// never answers). Each asserts exit 0, exactly one warning line with the pinned
// wording, an elapsed time bounded by HubConnectTimeout, working local routes,
// and `Hub: llt (reconnecting …)` in a `prox status` that itself exits 0
// (AC12).
func TestHubPublish_UnreachableHubIsNeverFatal(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)
	binary := buildBinary(t)

	env := newHubPublishEnv(t)

	for _, tc := range []struct {
		name   string
		hubURL func(t *testing.T) string
	}{
		{
			name:   "connection refused",
			hubURL: func(t *testing.T) string { return "http://" + reservedHubAddr(t) },
		},
		{
			name: "black hole",
			hubURL: func(t *testing.T) string {
				return "http://" + startBlackHoleListener(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := strings.ReplaceAll(tc.name, " ", "-") + ".test"
			f := env.fixtureFor(t, domain, tc.hubURL(t), "llt")

			start := time.Now()
			run := f.StartDetached(t, binary, "up", "-d")
			elapsed := time.Since(start)

			// `prox up -d` exits 0: a configured but unreachable hub is never
			// fatal.
			if code := run.ExitCode(t); code != 0 {
				t.Fatalf("prox up -d exit code = %d, want 0\noutput:\n%s", code, run.Output())
			}

			// Exactly one warning line, with AC11's exact wording. The parent
			// replays it because the publisher raised it through the
			// warning sink (D19), not fmt.Printf inside a detached child.
			out := run.Output()
			if n := countMatches(ac11WarningRe, out); n != 1 {
				t.Fatalf("got %d AC11 warning lines, want exactly 1\noutput:\n%s", n, out)
			}

			// Bounded: a black-holed hub must not hold startup past
			// HubConnectTimeout. The launcher also waits for readiness and a
			// process settle window, so the bound is generous — what it rules
			// out is an UNBOUNDED wait on the hub.
			if max := constants.HubConnectTimeout + 25*time.Second; elapsed > max {
				t.Fatalf("prox up -d took %s, want under %s", elapsed, max)
			}

			// The local route works exactly as it would with no hub at all.
			if got := driveProxy(t, env.proxyPort, "app."+domain, http.MethodGet, "/", nil); got != "backend-ok" {
				t.Fatalf("local route response = %q, want backend-ok", got)
			}

			// `prox status` shows the reconnecting hub AND exits 0 (AC12).
			statusOut, code := f.Run(t, binary, "status")
			if code != 0 {
				t.Fatalf("prox status exit code = %d, want 0 (a degraded hub never changes the exit code)\n%s", code, statusOut)
			}
			if !strings.Contains(statusOut, "Hub: llt (reconnecting") {
				t.Fatalf("prox status missing the reconnecting Hub line:\n%s", statusOut)
			}

			run.Shutdown(t)
		})
	}
}

// startBlackHoleListener accepts connections and never answers on them — the
// case a plain refused port does not cover, because a black hole is what turns
// an unbounded first register into a startup stall.
func startBlackHoleListener(t *testing.T) string {
	t.Helper()
	_, ln := freePort(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
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
			held = append(held, conn)
		}
	}()
	return ln.Addr().String()
}

// TestHubPublish_HubComesUpLaterAndDownDeregisters is the second half of AC11
// plus the teardown contract: a publisher that started while the hub was down
// acquires its routes with NO user action once the hub appears, `prox status`
// flips to connected, and `prox down` removes the registration from the hub.
func TestHubPublish_HubComesUpLaterAndDownDeregisters(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)
	binary := buildBinary(t)

	env := newHubPublishEnv(t)
	hubAddr := reservedHubAddr(t)
	f := env.fixtureFor(t, "late.test", "http://"+hubAddr, "llt")

	run := f.StartDetached(t, binary, "up", "-d")
	if code := run.ExitCode(t); code != 0 {
		t.Fatalf("prox up -d exit code = %d, want 0\n%s", code, run.Output())
	}
	if n := countMatches(ac11WarningRe, run.Output()); n != 1 {
		t.Fatalf("got %d AC11 warning lines, want exactly 1\n%s", n, run.Output())
	}

	// The hub appears. Nothing is done to the publisher.
	env.StartHub(hubAddr)

	deadline := within(t, 30*time.Second)
	waitForHubRoute(t, env.client, "app.llt.test", deadline)

	statusOut := waitForStatusContains(t, f, binary, "Hub: llt (connected", deadline)
	if !strings.Contains(statusOut, "Hub: llt (connected, 1 route)") {
		t.Fatalf("want the connected Hub line with its route count:\n%s", statusOut)
	}

	// `prox down` deregisters from the hub as well as the local daemon.
	if out, code := f.Run(t, binary, "down"); code != 0 {
		t.Fatalf("prox down exit code = %d\n%s", code, out)
	}
	waitForNoHubRoute(t, env.client, "app.llt.test", within(t, 20*time.Second))
}

// TestHubPublish_TwoPublishersCollide is AC6 at the CLI level: a second
// publisher of the same service name warns and starts WITHOUT hub routes, and
// with --hub-takeover it takes them — after which the first publisher's own
// re-register comes back HUB_NAME_HELD and it reports `displaced`.
func TestHubPublish_TwoPublishersCollide(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)
	binary := buildBinary(t)

	env := newHubPublishEnv(t)
	hubAddr := env.StartHub("")
	hubURL := "http://" + hubAddr

	// Two projects, two LOCAL domains, one shared service name — which is one
	// hostname on the hub, because the hub owns the domain (D4).
	first := env.fixtureFor(t, "one.test", hubURL, "llt")
	second := env.fixtureFor(t, "two.test", hubURL, "llt")

	runA := first.StartDetached(t, binary, "up", "-d")
	if code := runA.ExitCode(t); code != 0 {
		t.Fatalf("publisher A exit code = %d\n%s", code, runA.Output())
	}
	waitForHubRoute(t, env.client, "app.llt.test", within(t, 20*time.Second))
	waitForStatusContains(t, first, binary, "Hub: llt (connected", within(t, 20*time.Second))

	// The §4.2 preamble line, from the real binary. A detached child's preamble
	// goes to .prox/prox.log rather than the launcher's terminal, so that is
	// where it is pinned.
	wantLine := fmt.Sprintf("Hub (llt): http://*.llt.test:%d — app.llt.test", env.proxyPort)
	if log := readProxLog(t, runA); !strings.Contains(log, wantLine) {
		t.Fatalf("publisher A's log is missing %q:\n%s", wantLine, log)
	}

	// B collides. Non-interactive, so it warns and continues (D10).
	runB := second.StartDetached(t, binary, "up", "-d")
	if code := runB.ExitCode(t); code != 0 {
		t.Fatalf("publisher B exit code = %d, want 0 — a collision is never fatal\n%s", code, runB.Output())
	}
	if !strings.Contains(runB.Output(), "already publishes app.llt.test") {
		t.Fatalf("publisher B did not warn about the held name:\n%s", runB.Output())
	}
	if log := readProxLog(t, runB); strings.Contains(log, "Hub (llt):") {
		t.Fatalf("publisher B must not claim hub routes it did not get:\n%s", log)
	}
	// B's own local route still works — publishing is additive.
	if got := driveProxy(t, env.proxyPort, "app.two.test", http.MethodGet, "/", nil); got != "backend-ok" {
		t.Fatalf("publisher B local route = %q, want backend-ok", got)
	}
	// And B reports no hub at all, rather than a degraded one.
	statusB, code := second.Run(t, binary, "status")
	if code != 0 {
		t.Fatalf("publisher B status exit code = %d, want 0\n%s", code, statusB)
	}
	if strings.Contains(statusB, "Hub:") {
		t.Fatalf("a declined collision leaves NO hub state:\n%s", statusB)
	}
	shutdownAndAwait(t, runB)

	// B again, this time taking the name.
	runB2 := second.StartDetached(t, binary, "up", "-d", "--hub-takeover")
	if code := runB2.ExitCode(t); code != 0 {
		t.Fatalf("publisher B (takeover) exit code = %d\n%s", code, runB2.Output())
	}
	waitForStatusContains(t, second, binary, "Hub: llt (connected", within(t, 20*time.Second))

	// The hub now serves B's backend for that hostname, and A — whose tunnel
	// was closed and whose registration was removed entirely (D10) — discovers
	// it on its next register and reports `displaced`.
	statusA := waitForStatusContains(t, first, binary, "Hub: llt (displaced", within(t, 30*time.Second))
	if !strings.Contains(statusA, "app.llt.test held by") {
		t.Fatalf("the displaced line should name the holder:\n%s", statusA)
	}
	// A stays otherwise healthy and still exits 0 (AC12).
	if _, code := first.Run(t, binary, "status"); code != 0 {
		t.Fatalf("publisher A status exit code = %d, want 0 while displaced", code)
	}

	runB2.Shutdown(t)
	runA.Shutdown(t)
}

// TestHubPublish_FlagPrecedenceAtTheCLI covers AC4's CLI half: an unknown alias
// named with --hub is fatal, the same alias in prox.yaml warns and starts, and
// --no-hub suppresses a configured hub entirely.
func TestHubPublish_FlagPrecedenceAtTheCLI(t *testing.T) {
	startTest(t, defaultTestBudget)
	skipShort(t)
	binary := buildBinary(t)

	env := newHubPublishEnv(t)
	hubAddr := env.StartHub("")
	f := env.fixtureFor(t, "flags.test", "http://"+hubAddr, "llt")

	t.Run("unknown --hub alias is fatal", func(t *testing.T) {
		out, code := f.Run(t, binary, "up", "--hub", "ghost")
		if code == 0 {
			t.Fatalf("prox up --hub ghost exited 0, want non-zero\n%s", out)
		}
		if !strings.Contains(out, "unknown hub alias") {
			t.Fatalf("want an unknown-alias error, got:\n%s", out)
		}
	})

	t.Run("--hub and --no-hub conflict", func(t *testing.T) {
		out, code := f.Run(t, binary, "up", "--hub", "llt", "--no-hub")
		if code == 0 {
			t.Fatalf("conflicting flags exited 0, want non-zero\n%s", out)
		}
		if !strings.Contains(out, "--hub and --no-hub cannot be used together") {
			t.Fatalf("want the flag-conflict error, got:\n%s", out)
		}
	})

	t.Run("--no-hub suppresses a configured hub", func(t *testing.T) {
		run := f.StartDetached(t, binary, "up", "-d", "--no-hub")
		defer run.Shutdown(t)
		if code := run.ExitCode(t); code != 0 {
			t.Fatalf("exit code = %d\n%s", code, run.Output())
		}
		if strings.Contains(run.Output(), "Hub (llt):") {
			t.Fatalf("--no-hub still published:\n%s", run.Output())
		}
		out, code := f.Run(t, binary, "status")
		if code != 0 {
			t.Fatalf("status exit code = %d\n%s", code, out)
		}
		if strings.Contains(out, "Hub:") {
			t.Fatalf("--no-hub must leave no hub state:\n%s", out)
		}
		routes, err := env.client.Routes()
		if err != nil {
			t.Fatalf("Routes: %v", err)
		}
		for _, r := range routes {
			if r.Origin != "" {
				t.Fatalf("--no-hub registered a hub route: %+v", r)
			}
		}
	})

	t.Run("PROX_HUB names an unknown alias explicitly and is fatal", func(t *testing.T) {
		// The CLI inherits this process's environment (fixture launches leave
		// Cmd.Env nil), so setting it here is how the child sees it.
		t.Setenv("PROX_HUB", "ghost")
		out, code := f.Run(t, binary, "up")
		if code == 0 {
			t.Fatalf("PROX_HUB=ghost exited 0, want non-zero\n%s", out)
		}
		if !strings.Contains(out, "unknown hub alias") {
			t.Fatalf("want an unknown-alias error, got:\n%s", out)
		}
	})
}

// shutdownAndAwait stops a detached daemon and waits for the PROCESS to be
// gone, not merely for the waited shutdown to answer. The difference matters
// when the same fixture directory is started again immediately: the PID file
// lock outlives the shutdown response by a moment, and `prox up` in that window
// fails with "prox is already running".
func shutdownAndAwait(t *testing.T, r *proxRun) {
	t.Helper()
	id := r.DaemonIdentity()
	r.Shutdown(t)
	if !awaitIdentityGone(id, daemonExitBudget) {
		t.Fatalf("daemon %s was still alive %s after a waited shutdown", id, daemonExitBudget)
	}
}

// readProxLog returns a detached run's .prox/prox.log, which is where a `-d`
// child's own stdout — preamble included — lands.
func readProxLog(t *testing.T, r *proxRun) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.StateDir(), "prox.log"))
	if err != nil {
		t.Fatalf("read prox.log: %v", err)
	}
	return string(data)
}

// --- waiters ---

func waitForHubRoute(t *testing.T, client *proxyd.Client, hostname string, deadline time.Time) {
	t.Helper()
	start := time.Now()
	for time.Now().Before(deadline) {
		routes, err := client.Routes()
		if err == nil {
			for _, r := range routes {
				if r.Hostname == hostname && r.Origin != "" {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("hub route %s did not appear %s", hostname, waitedFor(start, deadline))
}

func waitForNoHubRoute(t *testing.T, client *proxyd.Client, hostname string, deadline time.Time) {
	t.Helper()
	start := time.Now()
	for time.Now().Before(deadline) {
		routes, err := client.Routes()
		if err == nil {
			found := false
			for _, r := range routes {
				if r.Hostname == hostname && r.Origin != "" {
					found = true
				}
			}
			if !found {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("hub route %s was still registered %s", hostname, waitedFor(start, deadline))
}

// waitForStatusContains polls `prox status` until it contains want, returning
// the matching output. The exit code is asserted to stay 0 on every poll, which
// is AC12 checked repeatedly rather than once.
func waitForStatusContains(t *testing.T, f *proxFixture, binary, want string, deadline time.Time) string {
	t.Helper()
	start := time.Now()
	var last string
	for time.Now().Before(deadline) {
		out, code := f.Run(t, binary, "status")
		last = out
		if code != 0 {
			t.Fatalf("prox status exit code = %d, want 0 (a degraded hub never changes it)\n%s", code, out)
		}
		if strings.Contains(out, want) {
			return out
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("prox status never contained %q %s\nlast output:\n%s", want, waitedFor(start, deadline), last)
	return last
}
