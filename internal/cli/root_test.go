package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/daemon"
	"github.com/spf13/cobra"
)

func TestGetProcessNames(t *testing.T) {
	// Save original configPath and restore after test
	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	t.Run("returns process names from config", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
processes:
  web: npm run dev
  api: go run ./cmd/api
  worker: python worker.py
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		names := getProcessNames()

		if len(names) != 3 {
			t.Errorf("expected 3 process names, got %d", len(names))
		}

		// Check that all expected names are present
		nameSet := make(map[string]bool)
		for _, name := range names {
			nameSet[name] = true
		}

		expected := []string{"web", "api", "worker"}
		for _, exp := range expected {
			if !nameSet[exp] {
				t.Errorf("expected process name %q not found", exp)
			}
		}
	})

	t.Run("returns nil when config not found", func(t *testing.T) {
		configPath = "/nonexistent/prox.yaml"
		names := getProcessNames()

		if names != nil {
			t.Errorf("expected nil, got %v", names)
		}
	})

	// Tasks are start/stop/restart targets too (plan 013 D5), so their names must
	// complete alongside process names.
	t.Run("includes task names", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
processes:
  web: npm run dev
tasks:
  migrate:
    cmd: ./migrate.sh
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		names := getProcessNames()

		nameSet := make(map[string]bool)
		for _, name := range names {
			nameSet[name] = true
		}
		if !nameSet["web"] {
			t.Errorf("expected process name %q in completion, got %v", "web", names)
		}
		if !nameSet["migrate"] {
			t.Errorf("expected task name %q in completion, got %v", "migrate", names)
		}
	})
}

// TestClientCommandsDiscoveryAllowlist pins the discovery allowlist contract:
// every command whose handler reaches the daemon API through the shared apiAddr
// global (the NewClient(apiAddr) call sites in this package, plus attach, which
// falls back to apiAddr when --addr is set) must be in clientCommands, or it
// silently talks to the :5555 default and breaks against dynamic-port daemons.
// 'start' (#41) and 'requests' (#43) both shipped with exactly that gap; when
// adding a new client command, add it to clientCommands AND to this list.
//
// D3 changed *what* happens for allowlisted commands when discovery fails
// (they now return an error instead of falling back to :5555 silently), but
// not *which* commands are allowlisted, so the allowlist itself is unchanged
// here. The PersistentPreRunE assertion below pins that the error-returning
// hook is actually wired up, since that's the mechanism the D3 error path
// depends on.
func TestClientCommandsDiscoveryAllowlist(t *testing.T) {
	if rootCmd.PersistentPreRunE == nil {
		t.Fatal("rootCmd.PersistentPreRunE must be set so discovery errors (D3) can propagate; a plain PersistentPreRun cannot return an error")
	}
	// The enumerated apiAddr-consuming commands. downCmd reuses runStop, and
	// attachCmd honors apiAddr when explicitly set, so both belong here.
	apiAddrCommands := []string{
		"status",   // runStatus
		"logs",     // runLogs
		"stop",     // runStop
		"down",     // RunE: runStop
		"start",    // runStartProcess
		"restart",  // runRestart
		"attach",   // runAttach (apiAddr fallback + state discovery)
		"requests", // runRequests
	}

	for _, name := range apiAddrCommands {
		if !clientCommands[name] {
			t.Errorf("command %q performs client calls via apiAddr but is missing from the clientCommands discovery allowlist", name)
		}
	}

	// Every allowlist entry must be a real registered subcommand, so a renamed
	// or removed command can't leave a stale entry silently matching nothing.
	registered := make(map[string]bool)
	for _, cmd := range rootCmd.Commands() {
		registered[cmd.Name()] = true
	}
	for name := range clientCommands {
		if !registered[name] {
			t.Errorf("clientCommands allowlist entry %q is not a registered command", name)
		}
	}

	if len(clientCommands) != len(apiAddrCommands) {
		t.Errorf("clientCommands has %d entries but %d apiAddr-consuming commands are enumerated here; keep the allowlist and this test in sync",
			len(clientCommands), len(apiAddrCommands))
	}
}

// withTempCwd chdirs into a fresh temp directory for the duration of the
// test (so os.Getwd()-based discovery sees no .prox/prox.state) and restores
// the original working directory on cleanup.
func withTempCwd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restoring cwd: %v", err)
		}
	})
	return dir
}

// withCwd chdirs into dir for the duration of the test AND pins $PWD to the
// same string, so os.Getwd() returns dir verbatim rather than its physical
// (symlink-resolved) form. That distinction is the whole point of the
// symlink/`/tmp` ownership cases below: a user's shell sets $PWD to the
// symlinked path, while every exec.Cmd{Dir: ...} (and so every coding agent)
// produces the resolved one.
func withCwd(t *testing.T, dir string) {
	t.Helper()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Setenv("PWD", dir)
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restoring cwd: %v", err)
		}
	})
}

// startFakeDaemon runs h on loopback and returns its host and port, ready to be
// written into a state file. Using a real server (rather than a probe seam)
// keeps the transport-level branches -- timeouts, refused dials, non-JSON
// bodies -- honest.
func startFakeDaemon(t *testing.T, h http.Handler) (string, int) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing %q: %v", srv.URL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}

// statusHandler answers GET /api/v1/status with body (marshaled as JSON) and
// 404s everything else. body is a map so tests can OMIT fields entirely --
// which is exactly what an older daemon does with project_dir.
func statusHandler(body map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// writeStateFile writes a .prox/prox.state into dir pointing at host:port.
func writeStateFile(t *testing.T, dir, host string, port int, configFile string) {
	t.Helper()
	state := &daemon.State{
		PID:        os.Getpid(),
		Port:       port,
		Host:       host,
		StartedAt:  time.Now(),
		ConfigFile: configFile,
	}
	if err := state.Write(dir); err != nil {
		t.Fatalf("writing state: %v", err)
	}
}

// TestDiscoverAPIAddress_NoState pins the D3 breaking change: with no
// .prox/prox.state, discovery must fail loudly (never fall back to the
// compiled-in :5555 default), naming both the missing state file and the
// --addr escape hatch.
func TestDiscoverAPIAddress_NoState(t *testing.T) {
	withTempCwd(t)

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("expected discovery error, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), ".prox/prox.state") {
		t.Errorf("error should name .prox/prox.state, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--addr") {
		t.Errorf("error should mention --addr as the fix, got: %v", err)
	}
}

// TestDiscoverAPIAddress_ConfigFileIsNotADiscoverySource pins plan 020 C3
// part A: prox.yaml's api.port is NO LONGER a discovery source. `prox up`
// writes the state file (fatally, on failure) strictly before it binds the API
// listener, so a listening daemon always has one; the fallback's only residual
// effect was to let a CLIENT's own --config resolution pick an address for a
// daemon it had never verified -- which is how two directories pinning the
// same api.port ended up able to `prox down` each other.
func TestDiscoverAPIAddress_ConfigFileIsNotADiscoverySource(t *testing.T) {
	dir := withTempCwd(t)

	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	cfgPath := filepath.Join(dir, "prox.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
api:
  port: 6001
  host: 127.0.0.1
processes:
  test: echo hello
`), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	configPath = cfgPath

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("a pinned api.port must not be a discovery source, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), ".prox/prox.state") {
		t.Errorf("error should name .prox/prox.state, got: %v", err)
	}
}

// TestDiscoverAPIAddress_OwnProjectAllowed is the baseline false-refusal
// guard: a daemon reporting THIS directory resolves normally. Note the cwd the
// test runs in is the physical path (t.TempDir() may hand back a symlinked
// one), so this already exercises os.SameFile rather than string equality on
// Darwin.
func TestDiscoverAPIAddress_OwnProjectAllowed(t *testing.T) {
	dir := withTempCwd(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": cwd,
		"config_file": filepath.Join(cwd, "prox.yaml"),
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err != nil {
		t.Fatalf("unexpected refusal for our own daemon: %v", err)
	}
	if want := "http://" + net.JoinHostPort(host, strconv.Itoa(port)); addr != want {
		t.Errorf("expected %q, got %q", want, addr)
	}
}

// TestDiscoverAPIAddress_SymlinkedProjectRootAllowed is the headline
// false-refusal case: the daemon recorded the RESOLVED project path (what
// os.Getwd returns under exec.Cmd{Dir: ...}) while the client stands in the
// SYMLINKED one (what a shell's $PWD gives). A string comparison refuses here;
// os.SameFile must not.
func TestDiscoverAPIAddress_SymlinkedProjectRootAllowed(t *testing.T) {
	real := t.TempDir()
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(realResolved, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": realResolved,
	}))
	writeStateFile(t, realResolved, host, port, filepath.Join(realResolved, "prox.yaml"))

	withCwd(t, link)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if cwd != link {
		t.Fatalf("test setup: expected cwd to stay the symlinked path %q, got %q", link, cwd)
	}

	if _, err := discoverAPIAddress(); err != nil {
		t.Fatalf("a symlinked project root must not be refused: %v", err)
	}
}

// TestDiscoverAPIAddress_TmpVsPrivateTmpAllowed covers the Darwin
// /tmp -> /private/tmp case specifically, with $PWD UNSET so os.Getwd returns
// the physical path while the daemon recorded the /tmp form. This is the shape
// every exec.Cmd{Dir: ...} and coding-agent invocation produces.
func TestDiscoverAPIAddress_TmpVsPrivateTmpAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("/tmp is a symlink to /private/tmp only on Darwin")
	}
	dir, err := os.MkdirTemp("/tmp", "prox-c3-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": dir, // the /tmp/... form, as a shell would report it
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	// No $PWD: os.Getwd falls through to syscall.Getwd and returns
	// /private/tmp/..., diverging in string form from what the daemon recorded.
	t.Setenv("PWD", "")
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if cwd == dir {
		t.Skipf("test setup: expected cwd %q to resolve away from %q", cwd, dir)
	}

	if _, err := discoverAPIAddress(); err != nil {
		t.Fatalf("/tmp vs /private/tmp must not be refused: %v", err)
	}
}

// TestDiscoverAPIAddress_ForeignProjectRefused is the destructive case: a
// stale state file (or two projects pinning one api.port) points at a port
// another project's prox now owns. Discovery must refuse AND name the owner
// plus a copy-pasteable --addr, so the user is not left believing their own
// prox is simply down.
func TestDiscoverAPIAddress_ForeignProjectRefused(t *testing.T) {
	dir := withTempCwd(t)
	owner := t.TempDir()

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": owner,
		"config_file": filepath.Join(owner, "prox.yaml"),
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("expected refusal, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), owner) {
		t.Errorf("error must name the owning project %q, got: %v", owner, err)
	}
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	if !strings.Contains(err.Error(), "--addr http://"+hostPort) {
		t.Errorf("error must offer a copy-pasteable --addr escape hatch, got: %v", err)
	}
}

// TestDiscoverAPIAddress_VanishedProjectDirAllowed pins the rename/move
// escape valve (codex review finding). Rename a project directory while its
// daemon runs and .prox/prox.state travels with it -- so it is still found
// here -- while the daemon keeps reporting the path it started in. That old
// path no longer Stats, os.SameFile cannot run, and a plain string comparison
// would REFUSE, locking the owner out of their own live daemon (including out
// of `prox down`, leaving no way to stop it but --addr).
//
// A reported directory that does not exist is absent evidence, not evidence of
// a foreign owner: a genuinely foreign project is a LIVE project, and a live
// project's directory exists -- which samePath already decides. So allow, and
// warn.
func TestDiscoverAPIAddress_VanishedProjectDirAllowed(t *testing.T) {
	dir := withTempCwd(t)

	// A path that is well-formed but absent: the pre-rename location.
	vanished := filepath.Join(t.TempDir(), "renamed-away")

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": vanished,
		"config_file": filepath.Join(vanished, "prox.yaml"),
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err != nil {
		t.Fatalf("a daemon whose recorded project dir no longer exists must NOT lock the user out, got: %v", err)
	}
	if want := "http://" + net.JoinHostPort(host, strconv.Itoa(port)); addr != want {
		t.Errorf("addr = %q, want %q", addr, want)
	}
}

// TestDiscoverAPIAddress_SharedConfigTwoRootsRefused is the false-ALLOW guard
// that fixes the identity basis. Two project roots sharing ONE config file
// (`prox up -c ../shared/prox.yaml`) have identical config_file values, so a
// config-path comparison would declare each the owner of the other. The
// project directory is authoritative and must still refuse.
func TestDiscoverAPIAddress_SharedConfigTwoRootsRefused(t *testing.T) {
	dir := withTempCwd(t)
	owner := t.TempDir()
	shared := filepath.Join(t.TempDir(), "prox.yaml")
	if err := os.WriteFile(shared, []byte("processes:\n  a: echo hi\n"), 0644); err != nil {
		t.Fatalf("writing shared config: %v", err)
	}

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": owner,
		"config_file": shared, // identical to the state file's ConfigFile below
	}))
	writeStateFile(t, dir, host, port, shared)

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("two roots sharing one config must not control each other, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), owner) {
		t.Errorf("error must name the owning project %q, got: %v", owner, err)
	}
}

// TestDiscoverAPIAddress_LegacyDaemonConfigMatchAllowed covers a daemon too
// old to report project_dir: the fallback compares the responder's config_file
// against the config recorded in THIS directory's state file. Both were
// written by daemons, so their form cannot diverge.
func TestDiscoverAPIAddress_LegacyDaemonConfigMatchAllowed(t *testing.T) {
	dir := withTempCwd(t)
	cfg := filepath.Join(dir, "prox.yaml")

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"config_file": cfg,
	}))
	writeStateFile(t, dir, host, port, cfg)

	if _, err := discoverAPIAddress(); err != nil {
		t.Fatalf("legacy daemon with a matching config must be allowed: %v", err)
	}
}

// TestDiscoverAPIAddress_LegacyDaemonConfigMismatchRefused is the same
// fallback refusing.
func TestDiscoverAPIAddress_LegacyDaemonConfigMismatchRefused(t *testing.T) {
	dir := withTempCwd(t)
	otherCfg := filepath.Join(t.TempDir(), "prox.yaml")

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"config_file": otherCfg,
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("expected refusal, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), otherCfg) {
		t.Errorf("error must name the owning config %q, got: %v", otherCfg, err)
	}
}

// TestDiscoverAPIAddress_NoIdentityFieldsAllowed pins the mid-upgrade policy: a
// daemon reporting NEITHER identity field is allowed through. Refusing would
// lock a user out of a daemon they could then no longer stop.
func TestDiscoverAPIAddress_NoIdentityFieldsAllowed(t *testing.T) {
	dir := withTempCwd(t)

	host, port := startFakeDaemon(t, statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
	}))
	writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

	if _, err := discoverAPIAddress(); err != nil {
		t.Fatalf("a daemon with no identity fields must be allowed: %v", err)
	}
}

// TestDiscoverAPIAddress_AuthErrorPassesThrough pins the 401/403 policy. The
// probe and the real request share one Client and one token, so a 401 here
// guarantees a 401 there: refusing adds zero safety and would replace a precise
// UNAUTHORIZED with a vague "could not verify owner".
func TestDiscoverAPIAddress_AuthErrorPassesThrough(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			dir := withTempCwd(t)
			host, port := startFakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "nope", "code": "UNAUTHORIZED"})
			}))
			writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

			addr, err := discoverAPIAddress()
			if err != nil {
				t.Fatalf("an auth failure must pass through to the real request, got: %v", err)
			}
			if want := "http://" + net.JoinHostPort(host, strconv.Itoa(port)); addr != want {
				t.Errorf("expected %q, got %q", want, addr)
			}
		})
	}
}

// TestDiscoverAPIAddress_NotAProx covers every "something answered, but it is
// not a prox" shape. Each must say so explicitly -- degrading to "no running
// prox instance" would send the user off debugging the wrong thing.
func TestDiscoverAPIAddress_NotAProx(t *testing.T) {
	cases := []struct {
		name    string
		handler http.Handler
	}{
		{
			// Decodes cleanly into an all-zero StatusResponse: this is why a
			// POSITIVE api_version marker is required.
			name:    "empty json object",
			handler: statusHandler(map[string]any{}),
		},
		{
			name: "non-json body",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html>hello from some other service</html>"))
			}),
		},
		{
			name: "empty body",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
		{
			name: "404 from an unrelated server",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			}),
		},
		{
			name: "500 from an unrelated server",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := withTempCwd(t)
			host, port := startFakeDaemon(t, tc.handler)
			writeStateFile(t, dir, host, port, filepath.Join(dir, "prox.yaml"))

			addr, err := discoverAPIAddress()
			if err == nil {
				t.Fatalf("expected refusal, got addr %q", addr)
			}
			if !strings.Contains(err.Error(), "not a prox") {
				t.Errorf("error must say the responder is not a prox, got: %v", err)
			}
			if strings.Contains(err.Error(), "no running prox instance") {
				t.Errorf("must not degrade to 'no running prox instance', got: %v", err)
			}
		})
	}
}

// TestDiscoverAPIAddress_NothingListening pins part C: a stale state file whose
// port nobody took reports the ordinary "not running" outcome (not a raw dial
// error), while still naming the address it tried.
func TestDiscoverAPIAddress_NothingListening(t *testing.T) {
	dir := withTempCwd(t)

	// Bind and immediately release a port so it is almost certainly free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	writeStateFile(t, dir, "127.0.0.1", port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err == nil {
		t.Fatalf("expected refusal, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), "no running prox instance") {
		t.Errorf("a stale state file with nothing listening should read as 'not running', got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("error should name the address it tried, got: %v", err)
	}
}

// TestDiscoverAPIAddress_WedgedListenerFailsClosedFast pins the probe's own
// short timeout. A listener that accepts and then never answers must fail
// CLOSED, and must not make the CLI sit out the 30s httpClient timeout (let
// alone the 11m lifecycle ceiling).
func TestDiscoverAPIAddress_WedgedListenerFailsClosedFast(t *testing.T) {
	dir := withTempCwd(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open, answering nothing.
			go func() { <-done; _ = conn.Close() }()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	writeStateFile(t, dir, "127.0.0.1", port, filepath.Join(dir, "prox.yaml"))

	start := time.Now()
	addr, err := discoverAPIAddress()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a wedged listener must fail closed, got addr %q", addr)
	}
	if !strings.Contains(err.Error(), "could not verify") || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should report an unverifiable owner and a timeout, got: %v", err)
	}
	// Bound relative to the constant rather than a hardcoded number, so raising
	// the timeout for slow-daemon headroom cannot silently un-pin this test.
	if maxElapsed := constants.OwnershipProbeTimeout + 2*time.Second; elapsed >= maxElapsed {
		t.Errorf("probe took %s (bound %s); it must stay well under the 30s client timeout", elapsed, maxElapsed)
	}
}

// TestDiscoverAPIAddress_IPv6HostIsBracketed pins the URL construction: an
// IPv6 api.host must be bracketed (net.JoinHostPort), or the address becomes
// the unparseable "http://::1:5552".
func TestDiscoverAPIAddress_IPv6HostIsBracketed(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	dir := withTempCwd(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	srv := &http.Server{Handler: statusHandler(map[string]any{
		"status":      "running",
		"api_version": "v1",
		"project_dir": cwd,
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	writeStateFile(t, dir, "::1", port, filepath.Join(dir, "prox.yaml"))

	addr, err := discoverAPIAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://[::1]:" + strconv.Itoa(port); addr != want {
		t.Errorf("expected %q, got %q", want, addr)
	}
}

// TestSamePath covers the identity primitive directly, including the
// Stat-failure fallback: a config file deleted while its prox still runs must
// NOT make that healthy daemon uncontrollable.
func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !samePath(resolved, link) {
		t.Errorf("a symlinked path must compare equal to its target")
	}
	if !samePath(resolved, resolved+"/./") {
		t.Errorf("path-form differences must compare equal")
	}
	if samePath(resolved, t.TempDir()) {
		t.Errorf("distinct directories must not compare equal")
	}

	// Both missing: fall back to cleaned absolute string comparison.
	gone := filepath.Join(dir, "deleted", "prox.yaml")
	if !samePath(gone, filepath.Join(dir, "deleted", ".", "prox.yaml")) {
		t.Errorf("missing paths must fall back to a cleaned-absolute comparison")
	}
	if samePath(gone, filepath.Join(dir, "other", "prox.yaml")) {
		t.Errorf("distinct missing paths must not compare equal")
	}
}

// resetAPIAddrGlobals saves the apiAddr/apiAddrExplicitlySet/configPath
// package globals and returns a restore func, so rootPersistentPreRunE tests
// (which mutate them, mirroring production flag-parsing + hook behavior)
// don't leak state into other tests in this package.
func resetAPIAddrGlobals(t *testing.T) {
	t.Helper()
	originalAddr := apiAddr
	originalExplicit := apiAddrExplicitlySet
	originalConfigPath := configPath
	apiAddrExplicitlySet = false
	t.Cleanup(func() {
		apiAddr = originalAddr
		apiAddrExplicitlySet = originalExplicit
		configPath = originalConfigPath
	})
}

// TestRootPersistentPreRunE_ClientCommandMissingStateReturnsError pins that
// the PersistentPreRunE hook (not just discoverAPIAddress in isolation)
// surfaces the D3 discovery error for allowlisted client commands.
func TestRootPersistentPreRunE_ClientCommandMissingStateReturnsError(t *testing.T) {
	withTempCwd(t)
	resetAPIAddrGlobals(t)
	configPath = "/nonexistent/prox.yaml"

	root := &cobra.Command{Use: "prox"}
	cmd := &cobra.Command{Use: "status"}
	root.AddCommand(cmd)
	var addr string
	cmd.Flags().StringVar(&addr, "addr", "", "unused in this test")

	err := rootPersistentPreRunE(cmd, nil)
	if err == nil {
		t.Fatal("expected discovery error for a client command with no state, no config, and no --addr")
	}
	if !strings.Contains(err.Error(), ".prox/prox.state") || !strings.Contains(err.Error(), "--addr") {
		t.Errorf("error should name .prox/prox.state and --addr, got: %v", err)
	}
}

// TestRootPersistentPreRunE_ExplicitAddrBypassesDiscoveryError pins that an
// explicitly-passed --addr always wins, even with no state and no config: no
// discovery is attempted and no error is returned.
func TestRootPersistentPreRunE_ExplicitAddrBypassesDiscoveryError(t *testing.T) {
	withTempCwd(t)
	resetAPIAddrGlobals(t)
	configPath = "/nonexistent/prox.yaml"

	root := &cobra.Command{Use: "prox"}
	cmd := &cobra.Command{Use: "status"}
	root.AddCommand(cmd)
	// Bind to the real apiAddr global, mirroring how
	// rootCmd.PersistentFlags().StringVar(&apiAddr, "addr", ...) wires the
	// production flag: pflag's Set (which cobra's real flag parsing calls)
	// writes straight through to apiAddr before the hook runs.
	cmd.Flags().StringVar(&apiAddr, "addr", "", "unused in this test")
	if err := cmd.Flags().Set("addr", "http://127.0.0.1:9999"); err != nil {
		t.Fatalf("setting --addr: %v", err)
	}

	if err := rootPersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("explicit --addr must bypass discovery entirely, got error: %v", err)
	}
	if !apiAddrExplicitlySet {
		t.Error("expected apiAddrExplicitlySet to be true after --addr was Changed")
	}
	if want := "http://127.0.0.1:9999"; apiAddr != want {
		t.Errorf("expected apiAddr to remain the explicitly-set value %q, got %q", want, apiAddr)
	}
}

// TestRootPersistentPreRunE_NonClientCommandUnaffectedByMissingState pins
// that commands outside the clientCommands allowlist (version, up, proxy,
// completion, __complete, help) never attempt discovery and so are
// unaffected by missing .prox/prox.state.
func TestRootPersistentPreRunE_NonClientCommandUnaffectedByMissingState(t *testing.T) {
	withTempCwd(t)
	resetAPIAddrGlobals(t)
	configPath = "/nonexistent/prox.yaml"

	cmd := &cobra.Command{Use: "version"}
	var addr string
	cmd.Flags().StringVar(&addr, "addr", "", "unused in this test")

	if err := rootPersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("non-client command must not be affected by missing state, got error: %v", err)
	}
}

// TestNeedsAPIDiscovery_RealCommandTree pins the allowlist match against the
// REAL rootCmd tree (codex C4 review): top-level client commands need
// discovery; nested proxy subcommands that share a name with them
// (`prox proxy status`, `prox proxy stop`) talk to the proxy daemon's Unix
// socket and must NOT — bare cmd.Name() matching would wrongly demand (and,
// post-D3, fail) discovery for them outside a project directory.
func TestNeedsAPIDiscovery_RealCommandTree(t *testing.T) {
	cases := []struct {
		path []string
		want bool
	}{
		{[]string{"status"}, true},
		{[]string{"stop"}, true},
		{[]string{"requests"}, true},
		{[]string{"proxy", "status"}, false},
		{[]string{"proxy", "stop"}, false},
		{[]string{"proxy", "routes"}, false},
		{[]string{"version"}, false},
		{[]string{"up"}, false},
	}
	for _, tc := range cases {
		cmd, _, err := rootCmd.Find(tc.path)
		if err != nil {
			t.Fatalf("Find(%v): %v", tc.path, err)
		}
		if got := needsAPIDiscovery(cmd); got != tc.want {
			t.Errorf("needsAPIDiscovery(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
