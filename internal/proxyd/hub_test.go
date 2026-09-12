package proxyd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyHubListenAddr pins plan 031 D6's listen-address rule as amended
// by F8, with literal addresses, because CI never sees a tailnet and the rule is
// the only thing standing between a typo and a plain-HTTP control plane on the
// public internet.
//
// Three classes, not two. ENCRYPTED (loopback and CGNAT) is the default because
// neither carries the bearer token across a wire a stranger can read. The plain
// private ranges are UNENCRYPTED LAN: still usable, but only with an explicit
// opt-in, since "RFC 1918" describes an address space and not a confidentiality
// boundary — a café LAN is 192.168/16. The two unspecified addresses are the
// cases that matter most: binding 0.0.0.0 or :: would expose the hub on EVERY
// interface, and no flag opts into that.
//
// The CGNAT rows are the review-B5 split: 100.64/10 is Tailscale's range AND a
// carrier-grade-NAT range, so membership alone proves nothing about encryption.
// An address there is encrypted only when it sits on a TUNNEL interface; the
// same address on an ordinary LAN interface is a plaintext wire and needs the
// opt-in like any other. The set of tunnel addresses is injected so these rows
// assert the rule rather than the interfaces of whatever machine runs them.
func TestClassifyHubListenAddr(t *testing.T) {
	// Everything in this table that is NOT the CGNAT split is classified the
	// same either way, so one set covers the whole table: 100.64.0.1 and
	// 100.82.128.123 are "on a tunnel", 100.127.255.254 is not.
	tunnelAddrs := map[string]struct{}{
		"100.64.0.1":     {},
		"100.82.128.123": {},
	}

	tests := []struct {
		name string
		addr string
		want hubListenClass
	}{
		{"loopback v4", "127.0.0.1", hubListenEncrypted},
		{"loopback v4 elsewhere in /8", "127.9.9.9", hubListenEncrypted},
		{"loopback v6", "::1", hubListenEncrypted},
		{"cgnat 100.64/10 low, on a tunnel interface", "100.64.0.1", hubListenEncrypted},
		{"cgnat 100.64/10 tailnet", "100.82.128.123", hubListenEncrypted},
		// The B5 case: a CGNAT address that is NOT on a tunnel device — a
		// carrier-grade-NAT lease, a hotspot, a campus LAN — is a plaintext
		// wire and must not be auto-selected as if it were a tailnet address.
		{"cgnat 100.64/10 high, on a plain interface", "100.127.255.254", hubListenUnencryptedLAN},

		{"rfc1918 10/8", "10.1.2.3", hubListenUnencryptedLAN},
		{"rfc1918 172.16/12 low", "172.16.0.1", hubListenUnencryptedLAN},
		{"rfc1918 172.16/12 high", "172.31.255.254", hubListenUnencryptedLAN},
		{"rfc1918 192.168/16", "192.168.1.5", hubListenUnencryptedLAN},
		{"ipv6 unique-local fc00::/7", "fd12:3456:789a::1", hubListenUnencryptedLAN},
		{"ipv6 unique-local fc range", "fc00::1", hubListenUnencryptedLAN},

		{"unspecified v4", "0.0.0.0", hubListenRefused},
		{"unspecified v6", "::", hubListenRefused},
		{"public v4", "93.184.216.34", hubListenRefused},
		{"public v6", "2606:2800:220:1:248:1893:25c8:1946", hubListenRefused},
		{"just outside 172.16/12", "172.32.0.1", hubListenRefused},
		{"just outside cgnat", "100.128.0.1", hubListenRefused},
		{"ipv4 link-local", "169.254.1.1", hubListenRefused},
		{"ipv6 link-local", "fe80::1", hubListenRefused},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.addr)
			require.NotNil(t, ip, "test address must parse")
			assert.Equal(t, tt.want, classifyHubListenAddrWith(ip, tunnelAddrs))
			assert.Equal(t, tt.want != hubListenRefused, isPrivateListenAddr(ip))
		})
	}
}

// TestIsTunnelInterface pins the device test review B5 rests on: a tailnet
// device is recognized by NAME (tailscale0, utunN, wgN) or by being a
// point-to-point link with no broadcast, and an ordinary Ethernet or Wi-Fi
// interface is neither — which is exactly why a CGNAT address on one of those
// is not evidence of encryption.
func TestIsTunnelInterface(t *testing.T) {
	tests := []struct {
		name  string
		iface net.Interface
		want  bool
	}{
		{"tailscale0 by name", net.Interface{Name: "tailscale0", Flags: net.FlagUp | net.FlagPointToPoint}, true},
		{"macOS utun by name", net.Interface{Name: "utun3", Flags: net.FlagUp}, true},
		{"wireguard by name", net.Interface{Name: "wg0", Flags: net.FlagUp}, true},
		{"unnamed point-to-point tun", net.Interface{Name: "tun9", Flags: net.FlagUp | net.FlagPointToPoint}, true},
		{"ethernet", net.Interface{Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast}, false},
		{"wifi", net.Interface{Name: "wlp3s0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast}, false},
		{"macOS en0", net.Interface{Name: "en0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast}, false},
		{"docker bridge", net.Interface{Name: "docker0", Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTunnelInterface(tt.iface))
		})
	}
}

// TestDefaultHubListenAddr_IgnoresANonTunnelCGNATAddress is the consequence of
// B5 that a user would actually meet: on a machine whose ISP hands out a
// carrier-grade-NAT address, a first `prox hub start` must fall back to loopback
// and say so, NOT silently bind the carrier address and put the bearer token on
// a network the carrier's other customers share.
func TestDefaultHubListenAddr_IgnoresANonTunnelCGNATAddress(t *testing.T) {
	restore := tunnelInterfaceAddrs
	t.Cleanup(func() { tunnelInterfaceAddrs = restore })

	// No tunnel interfaces at all: whatever this machine holds in 100.64/10 is
	// a carrier address as far as the rule is concerned.
	tunnelInterfaceAddrs = func() map[string]struct{} { return nil }
	addr, tailnet := DefaultHubListenAddr()
	assert.False(t, tailnet, "a CGNAT address off a tunnel device is not a tailnet address")
	assert.Equal(t, "127.0.0.1:8443", addr)
}

// TestValidateHubListenAddr covers the wrapper the config path actually calls:
// a bare host gains the default port, a refused address is refused with
// ACTIONABLE text (the rule plus this machine's own addresses), and a hostname
// is rejected because the rule can only be evaluated on a literal IP.
func TestValidateHubListenAddr(t *testing.T) {
	t.Run("adds the default port to a bare host", func(t *testing.T) {
		got, err := validateHubListenAddr("127.0.0.1", false)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:8443", got)
	})

	t.Run("keeps an explicit port, including 0", func(t *testing.T) {
		got, err := validateHubListenAddr("127.0.0.1:0", false)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:0", got)
	})

	// Review B5, through the wrapper the config path actually calls: a CGNAT
	// address that is not on a tunnel device is refused with the LAN advice
	// rather than accepted as a tailnet address, and the opt-in still takes it.
	t.Run("a CGNAT address off a tunnel device needs the LAN opt-in", func(t *testing.T) {
		restore := tunnelInterfaceAddrs
		t.Cleanup(func() { tunnelInterfaceAddrs = restore })
		tunnelInterfaceAddrs = func() map[string]struct{} { return nil }

		_, err := validateHubListenAddr("100.64.1.2:8443", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "plain LAN address")

		got, err := validateHubListenAddr("100.64.1.2:8443", true)
		require.NoError(t, err)
		assert.Equal(t, "100.64.1.2:8443", got)
	})

	// Plan 031: an out-of-range listen port is a CONFIG error, caught here, not
	// a bind failure discovered by net.Listen much later and reported as
	// HUB_BIND_FAILED. 0 stays legal (ephemeral, above); everything else goes
	// through the shared domain.ValidatePort rule.
	t.Run("refuses a listen port outside the range", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:84433", "127.0.0.1:65536", "100.64.1.2:-1"} {
			_, err := validateHubListenAddr(addr, false)
			require.Error(t, err, addr)
			assert.Contains(t, err.Error(), "invalid port", addr)
			var cfgErr *hubConfigError
			assert.ErrorAs(t, err, &cfgErr, "it must be a config error, so the API answers 400")
		}
	})

	t.Run("refuses 0.0.0.0 with advice", func(t *testing.T) {
		_, err := validateHubListenAddr("0.0.0.0:8443", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
		assert.Contains(t, err.Error(), "never 0.0.0.0")
		// The refusal must name a concrete alternative the user can paste.
		assert.Contains(t, err.Error(), "--listen ")
	})

	t.Run("refuses 0.0.0.0 even with the LAN opt-in", func(t *testing.T) {
		// The opt-in is about ENCRYPTION, not about relaxing the rule: an
		// unspecified or public address is refused with or without it.
		for _, addr := range []string{"0.0.0.0:8443", "[::]:8443", "93.184.216.34:8443"} {
			_, err := validateHubListenAddr(addr, true)
			require.Error(t, err, addr)
			assert.Contains(t, err.Error(), "not allowed", addr)
		}
	})

	t.Run("refuses a public address", func(t *testing.T) {
		_, err := validateHubListenAddr("93.184.216.34:8443", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	t.Run("refuses a hostname", func(t *testing.T) {
		_, err := validateHubListenAddr("hub.example.com:8443", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "literal IP")
	})

	// Plan 031 F8: the plain private ranges need the opt-in, and the refusal has
	// to explain WHY (a cleartext bearer token on a shared wire) and HOW to
	// proceed, not just quote the rule back at the operator.
	t.Run("refuses a plain LAN address by default, explaining why", func(t *testing.T) {
		for _, addr := range []string{"10.0.0.5:8443", "172.16.4.4:8443", "192.168.1.10:8443", "[fd00::1]:8443"} {
			_, err := validateHubListenAddr(addr, false)
			require.Error(t, err, addr)
			assert.Contains(t, err.Error(), "plain LAN address", addr)
			assert.Contains(t, err.Error(), "bearer token", addr)
			assert.Contains(t, err.Error(), "--allow-unencrypted-lan", addr)
		}
	})

	t.Run("accepts a plain LAN address with the opt-in", func(t *testing.T) {
		got, err := validateHubListenAddr("192.168.1.10:8443", true)
		require.NoError(t, err)
		assert.Equal(t, "192.168.1.10:8443", got)
	})
}

// TestHubProjectKey_CannotNameALocalProject is the structural half of D15, as
// hardened by F2: a composed key and a local key are different SHAPES, so no
// origin/dir pair a publisher can send composes to a local project's key. This
// is what makes "a publisher cannot deregister a local project" a property
// rather than a check.
//
// The invariant is now the hub: prefix on one side and the socket mount's
// refusal of that prefix on the other (see
// TestSocketRegister_RefusesAHubKeyPrefix), which is why the WINDOWS-shaped
// cases below matter even though Windows is not a build target. The previous
// composition joined with ":" and leaned on "a local key is an absolute path";
// a local key of `C:\work\app` is reproducible as origin "C" + dir `\work\app`
// under that rule, and would have been a real cross-tenant hole on the day
// anyone built for Windows.
func TestHubProjectKey_CannotNameALocalProject(t *testing.T) {
	localKeys := []string{
		"/home/dev/project", "/", "/srv/app-1",
		// Windows-shaped local keys (plan 031 F2). Not reachable on a build
		// target today; the point is that the invariant does not depend on that.
		`C:\work\app`, `C:\`, `D:\src\api`,
	}

	origins := []string{"mac", "popos", "shed-1", "C", "D", "hub"}
	dirs := []string{"/home/dev/project", "/", "/srv/app-1", `\work\app`, `\`, "relative", "hub:/x"}

	for _, origin := range origins {
		for _, dir := range dirs {
			key := HubProjectKey(origin, dir)
			for _, local := range localKeys {
				assert.NotEqual(t, local, key, "composed key must never equal a local project key")
			}
			assert.True(t, isHubProjectKey(key), "every composed key is recognizable as one")
			assert.Equal(t, dir, hubKeyProjectDir(origin, key))

			gotOrigin, gotDir, ok := splitHubProjectKey(key)
			require.True(t, ok, "a composed key must split back")
			assert.Equal(t, origin, gotOrigin)
			assert.Equal(t, dir, gotDir)
		}
	}

	// A local key never splits as a hub key, whatever shape it has.
	for _, local := range localKeys {
		_, _, ok := splitHubProjectKey(local)
		assert.False(t, ok, "a local key %q must not parse as a hub key", local)
	}
}

// TestValidateHubProjectDir pins the slash-absolute rule the key invariant
// rests on (plan 031 F2) and the bounds F9 adds.
func TestValidateHubProjectDir(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		ok   bool
	}{
		{"absolute posix path", "/home/dev/app", true},
		{"root", "/", true},
		{"path containing the key separator", "/home/dev/a|b", true},
		{"empty", "", false},
		{"relative", "app", false},
		{"windows drive path", `C:\work\app`, false},
		{"windows unc path", `\\server\share`, false},
		{"contains NUL", "/home/dev/\x00app", false},
		{"contains newline", "/home/dev/a\nb", false},
		{"too long", "/" + strings.Repeat("a", hubProjectDirMaxBytes), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHubProjectDir(tt.dir)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// TestValidateHubOrigin pins the separators an origin may not contain — the
// reason the shape argument above holds — plus the basic sanity rules.
func TestValidateHubOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		ok     bool
	}{
		{"plain hostname", "popos", true},
		{"fqdn", "popos.tail1234.ts.net", true},
		{"with dash and digits", "shed-1", true},
		{"empty", "", false},
		{"contains colon", "mac:/home/dev", false},
		{"contains slash", "/home/dev", false},
		{"contains the key separator", "mac|other", false},
		{"contains space", "my machine", false},
		{"contains newline", "mac\nother", false},
		{"too long", strings.Repeat("a", 254), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHubOrigin(tt.origin)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// TestHubConfig_SaveLoadRoundTrip checks the file contract: 0600 (it sits next
// to a token and is read by a daemon), and a faithful round trip.
func TestHubConfig_SaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := HubConfig{
		Domain:    "llt.example.com",
		Listen:    "100.82.128.123:8443",
		HTTPSPort: 443,
		HTTPPort:  0,
		Auth:      HubAuthToken,
		Autostart: true,
		// A secret handed to SaveHubConfig must not reach the file: the token
		// lives in hub.token, so hub.yaml stays a config a user can read and
		// share without leaking a credential.
		Token: "s3cret-value-not-in-the-file",
	}
	require.NoError(t, SaveHubConfig(cfg))

	info, err := os.Stat(filepath.Join(home, ".prox", "hub.yaml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "hub.yaml must be 0600 (plan 031 D18)")

	loaded, err := LoadHubConfig()
	require.NoError(t, err)
	wantLoaded := cfg
	wantLoaded.Token = "" // never serialized, so never read back
	assert.Equal(t, wantLoaded, loaded)

	data, err := os.ReadFile(filepath.Join(home, ".prox", "hub.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "s3cret-value-not-in-the-file")
}

// TestLoadHubConfig_MissingFileIsNotExist lets callers tell "no hub configured
// yet" (a first `prox hub start`, a daemon with no autostart) from a broken
// one.
func TestLoadHubConfig_MissingFileIsNotExist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := LoadHubConfig()
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestParseHubConfig_Strict pins the same strict-YAML discipline prox.yaml and
// hubs.yaml get (plan 031 C2): a typo'd key is an error, not a silently ignored
// setting that leaves the hub listening somewhere the user did not intend.
func TestParseHubConfig_Strict(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid",
			yaml: "domain: llt.example.com\nlisten: 127.0.0.1:8443\nhttps_port: 443\nauth: token\n",
		},
		{
			name:    "unknown key",
			yaml:    "domain: llt.example.com\nlisten_addr: 127.0.0.1:8443\n",
			wantErr: "listen_addr",
		},
		{
			name:    "duplicate key",
			yaml:    "domain: a.example.com\ndomain: b.example.com\n",
			wantErr: "already defined",
		},
		{
			name:    "multi-document",
			yaml:    "domain: a.example.com\n---\ndomain: b.example.com\n",
			wantErr: "multi-document",
		},
		{
			name:    "wrong type",
			yaml:    "domain: llt.example.com\nhttps_port: nope\n",
			wantErr: "cannot unmarshal",
		},
		{
			name: "empty file parses to an empty config",
			yaml: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseHubConfig([]byte(tt.yaml), "hub.yaml")
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestHubToken_EnsureAndRotate pins the token file's contract: created on first
// use, stable afterwards, 0600, and a rotation that actually changes the value
// (the in-memory half of rotation is covered in hub_server_test.go).
func TestHubToken_EnsureAndRotate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	first, err := EnsureHubToken()
	require.NoError(t, err)
	assert.NotEmpty(t, first)

	info, err := os.Stat(filepath.Join(home, ".prox", "hub.token"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "hub.token must be 0600 (plan 031 D18)")

	again, err := EnsureHubToken()
	require.NoError(t, err)
	assert.Equal(t, first, again, "EnsureHubToken must not churn an existing token")

	rotated, err := RotateHubToken()
	require.NoError(t, err)
	assert.NotEqual(t, first, rotated)

	read, err := ReadHubToken()
	require.NoError(t, err)
	assert.Equal(t, rotated, read)
}

// TestEnsureHubToken_ReplacesAnEmptyFile: a token truncated to nothing (the
// crash D18's atomic write exists to prevent, or a user's stray `> hub.token`)
// must not leave a hub that rejects every publisher forever.
func TestEnsureHubToken_ReplacesAnEmptyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, EnsureDaemonDir())
	require.NoError(t, os.WriteFile(filepath.Join(home, ".prox", "hub.token"), []byte("  \n"), 0600))

	token, err := EnsureHubToken()
	require.NoError(t, err)
	assert.NotEmpty(t, token)
}

// TestNormalizeHubConfig covers the defaulting and validation every entry point
// funnels through (`prox hub start` before writing the file, StartHub on
// whatever it is handed).
func TestNormalizeHubConfig(t *testing.T) {
	t.Run("defaults auth to token and keeps ports", func(t *testing.T) {
		got, err := NormalizeHubConfig(HubConfig{
			Domain: "llt.example.com", Listen: "127.0.0.1:8443", HTTPSPort: 443,
		})
		require.NoError(t, err)
		assert.Equal(t, HubAuthToken, got.Auth)
		assert.Equal(t, 443, got.HTTPSPort)
	})

	t.Run("requires a domain", func(t *testing.T) {
		_, err := NormalizeHubConfig(HubConfig{Listen: "127.0.0.1:8443", HTTPSPort: 443})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "domain is required")
	})

	t.Run("rejects an unknown auth mode", func(t *testing.T) {
		_, err := NormalizeHubConfig(HubConfig{Domain: "a.test", Listen: "127.0.0.1:0", HTTPSPort: 443, Auth: "basic"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth must be")
	})

	t.Run("requires at least one data-plane port", func(t *testing.T) {
		_, err := NormalizeHubConfig(HubConfig{Domain: "a.test", Listen: "127.0.0.1:0"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one data-plane port")
	})

	t.Run("rejects equal http and https ports", func(t *testing.T) {
		_, err := NormalizeHubConfig(HubConfig{Domain: "a.test", Listen: "127.0.0.1:0", HTTPSPort: 8080, HTTPPort: 8080})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be the same")
	})

	// Plan 031: a data-plane port typo must be HUB_CONFIG_INVALID here, not a
	// listener failure later. 0 keeps meaning "no listener of this kind".
	t.Run("refuses a data-plane port outside the range", func(t *testing.T) {
		for _, cfg := range []HubConfig{
			{Domain: "a.test", Listen: "127.0.0.1:0", HTTPSPort: 65536},
			{Domain: "a.test", Listen: "127.0.0.1:0", HTTPSPort: 4433, HTTPPort: 88888},
			{Domain: "a.test", Listen: "127.0.0.1:0", HTTPSPort: -1},
		} {
			_, err := NormalizeHubConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "between 1 and 65535")
			var cfgErr *hubConfigError
			assert.ErrorAs(t, err, &cfgErr, "it must be a config error, so the API answers 400")
		}
	})

	t.Run("refuses a public listen address", func(t *testing.T) {
		_, err := NormalizeHubConfig(HubConfig{Domain: "a.test", Listen: "93.184.216.34:8443", HTTPSPort: 443})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})

	t.Run("fills an empty listen from the machine default", func(t *testing.T) {
		got, err := NormalizeHubConfig(HubConfig{Domain: "a.test", HTTPSPort: 443})
		require.NoError(t, err)
		assert.NotEmpty(t, got.Listen)
		host, _, err := net.SplitHostPort(got.Listen)
		require.NoError(t, err)
		assert.True(t, isPrivateListenAddr(net.ParseIP(host)), "the default must satisfy the rule it is a default for")
	})
}
