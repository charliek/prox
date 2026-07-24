package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/daemon"
	"github.com/spf13/cobra"
)

func TestLoadAPIAddrFromConfig(t *testing.T) {
	// Save original configPath and restore after test
	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	t.Run("returns address from config with custom port", func(t *testing.T) {
		// Create temp config file
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
api:
  port: 5552
  host: 127.0.0.1
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "http://127.0.0.1:5552" {
			t.Errorf("expected http://127.0.0.1:5552, got %s", addr)
		}
	})

	t.Run("returns empty when port not specified (dynamic)", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "" {
			t.Errorf("expected empty string for dynamic port, got %s", addr)
		}
	})

	t.Run("returns empty string when config not found", func(t *testing.T) {
		configPath = "/nonexistent/prox.yaml"
		addr := loadAPIAddrFromConfig()

		if addr != "" {
			t.Errorf("expected empty string, got %s", addr)
		}
	})

	t.Run("uses custom host from config", func(t *testing.T) {
		tmpDir := t.TempDir()
		testConfigPath := filepath.Join(tmpDir, "prox.yaml")
		err := os.WriteFile(testConfigPath, []byte(`
api:
  port: 8080
  host: 0.0.0.0
processes:
  test: echo hello
`), 0644)
		if err != nil {
			t.Fatal(err)
		}

		configPath = testConfigPath
		addr := loadAPIAddrFromConfig()

		if addr != "http://0.0.0.0:8080" {
			t.Errorf("expected http://0.0.0.0:8080, got %s", addr)
		}
	})
}

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

// TestDiscoverAPIAddress_NoStateNoConfig pins the D3 breaking change: when
// neither .prox/prox.state nor the config file yields an address, discovery
// must fail loudly (never fall back to the compiled-in :5555 default), and
// the error must name both the missing state file and the --addr escape
// hatch so the operator knows how to recover.
func TestDiscoverAPIAddress_NoStateNoConfig(t *testing.T) {
	dir := withTempCwd(t)

	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()
	configPath = filepath.Join(dir, "prox.yaml") // deliberately does not exist

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

// TestDiscoverAPIAddress_StateFilePresent pins that state-file discovery is
// unchanged by the D3 error-return refactor: a valid .prox/prox.state still
// resolves to the address it records, with no error.
func TestDiscoverAPIAddress_StateFilePresent(t *testing.T) {
	dir := withTempCwd(t)

	state := &daemon.State{
		PID:        os.Getpid(),
		Port:       54321,
		Host:       "127.0.0.1",
		StartedAt:  time.Now(),
		ConfigFile: "prox.yaml",
	}
	if err := state.Write(dir); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	addr, err := discoverAPIAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://127.0.0.1:54321"; addr != want {
		t.Errorf("expected %q, got %q", want, addr)
	}
}

// TestDiscoverAPIAddress_ConfigFallbackWhenNoState pins that the config-file
// fallback (used when no state file exists but prox.yaml pins an explicit
// port) is unchanged by the D3 refactor.
func TestDiscoverAPIAddress_ConfigFallbackWhenNoState(t *testing.T) {
	dir := withTempCwd(t)

	originalConfigPath := configPath
	defer func() { configPath = originalConfigPath }()

	cfgPath := filepath.Join(dir, "prox.yaml")
	err := os.WriteFile(cfgPath, []byte(`
api:
  port: 6001
  host: 127.0.0.1
processes:
  test: echo hello
`), 0644)
	if err != nil {
		t.Fatalf("writing config: %v", err)
	}
	configPath = cfgPath

	addr, err := discoverAPIAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://127.0.0.1:6001"; addr != want {
		t.Errorf("expected %q, got %q", want, addr)
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
