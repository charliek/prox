package proxyd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"gopkg.in/yaml.v3"
)

// Hub host files and auth modes (plan 031 §4.1, D18).
const (
	// HubConfigFileName is the hub host's config, ~/.prox/hub.yaml.
	HubConfigFileName = "hub.yaml"
	// HubTokenFileName is the hub host's bearer token, ~/.prox/hub.token.
	HubTokenFileName = "hub.token"

	// HubAuthToken requires a bearer token on every network-mount call except
	// /health; HubAuthNone is the user's explicit "this is a private network"
	// choice (D6).
	HubAuthToken = "token"
	HubAuthNone  = "none"
)

// HubConfig is the hub HOST's configuration: the parsed shape of
// ~/.prox/hub.yaml (plan 031 §4.1).
//
// Domain and the two data-plane ports are the hub's, not a publisher's: a
// remote registration's own domain/ports are OVERWRITTEN from here before it
// reaches the registry (D4), so hostnames are always <service>.<hub domain> and
// the hub host alone decides which ports it exposes.
type HubConfig struct {
	Domain    string `yaml:"domain" json:"domain"`
	Listen    string `yaml:"listen" json:"listen"`
	HTTPSPort int    `yaml:"https_port" json:"https_port"`
	HTTPPort  int    `yaml:"http_port" json:"http_port"`
	Auth      string `yaml:"auth" json:"auth"`
	Autostart bool   `yaml:"autostart" json:"autostart"`
	// AllowUnencryptedLAN opts the listen address out of the encrypted-transport
	// default (plan 031 D6 as amended by F8). Without it the hub binds only
	// loopback and tailnet (100.64/10) addresses, whose traffic is encrypted end
	// to end; with it the plain private ranges (10/8, 172.16/12, 192.168/16,
	// fc00::/7) are accepted too, and the operator has taken responsibility for a
	// bearer token crossing a wire other people can read. Public and unspecified
	// addresses are refused either way.
	AllowUnencryptedLAN bool `yaml:"allow_unencrypted_lan" json:"allow_unencrypted_lan"`

	// Token is the bearer credential the network mount checks. It is NEVER
	// serialized into hub.yaml — it lives in its own 0600 file
	// (~/.prox/hub.token) so the config can be read, diffed, and edited without
	// handling a secret. StartHub fills it from EnsureHubToken when auth is
	// "token" and the caller left it empty; tests pass it explicitly.
	Token string `yaml:"-" json:"-"`
}

// HubConfigPath returns ~/.prox/hub.yaml.
func HubConfigPath() string {
	return filepath.Join(DaemonDir(), HubConfigFileName)
}

// HubTokenPath returns ~/.prox/hub.token.
func HubTokenPath() string {
	return filepath.Join(DaemonDir(), HubTokenFileName)
}

// Hub registry-key composition (plan 031 D5/D15, hardened per F2).
const (
	// hubKeyPrefix begins EVERY composed hub key and can begin no local one.
	// That is the whole invariant: the socket register handler refuses a
	// project_dir starting with this prefix (see handleRegister), so the local
	// key space and the hub key space are disjoint by construction rather than
	// by an argument about what a path can look like.
	hubKeyPrefix = "hub:"
	// hubKeySeparator separates the origin from the publisher's directory. An
	// origin may not contain it (validateHubOrigin), so the FIRST occurrence is
	// always the separator and the split is exact even for a directory that
	// contains one.
	hubKeySeparator = "|"
)

// HubProjectKey composes the registry key for a remote registration from the
// caller's OWN origin and project dir (plan 031 D5/D15).
//
// This is the one place the composition exists, used on both the hub side (the
// network handlers) and the publisher side (the request stream's `project`
// param), so the two can never drift. Two properties make it load-bearing:
//
//   - It is DERIVED, never accepted from the wire. A network handler builds the
//     key from the authenticated caller's own identity, so a publisher can only
//     ever address its own registrations BY ACCIDENT-PROOF COMPOSITION. (It is
//     not an authentication boundary: see the note on validateHubOrigin.)
//   - A composed key can never collide with a LOCAL one. Every composed key
//     starts with hubKeyPrefix, and the socket mount refuses any project_dir
//     that starts with it, so no local registration can ever be named by a
//     composition — with any origin and any directory a publisher can send.
//
// The earlier form joined with ":" and leaned on "a local key is an absolute
// path", which a Windows-shaped local key (`C:\work\app` = origin "C" + dir
// `\work\app`) would have defeated. Windows is not a build target here, but the
// prefix plus validateHubProjectDir's slash-absolute rule makes the claim an
// invariant rather than a platform coincidence (plan 031 F2).
func HubProjectKey(origin, dir string) string {
	return hubKeyPrefix + origin + hubKeySeparator + dir
}

// isHubProjectKey reports whether key was produced by HubProjectKey. It is the
// socket mount's guard: a local project_dir carrying this prefix is refused, so
// the two key spaces stay disjoint.
func isHubProjectKey(key string) bool {
	return strings.HasPrefix(key, hubKeyPrefix)
}

// splitHubProjectKey is HubProjectKey's inverse, used by the publisher's tunnel
// client to recover the two halves it must send as X-Prox-Origin and
// X-Prox-Project-Dir (plan 031 §4.3).
//
// Deriving them from the key rather than carrying them separately is what makes
// the §8 "the publisher must use the key consistently" risk structural: the
// tunnel headers and the registered key are the same two strings by
// construction, so the hub's own composition of the key is guaranteed to
// reproduce the one the register call created.
func splitHubProjectKey(key string) (origin, dir string, ok bool) {
	rest, found := strings.CutPrefix(key, hubKeyPrefix)
	if !found {
		return "", "", false
	}
	origin, dir, found = strings.Cut(rest, hubKeySeparator)
	if !found || origin == "" || dir == "" {
		return "", "", false
	}
	return origin, dir, true
}

// hubKeyProjectDir recovers the publisher's OWN directory from a composed key,
// for display (`prox hub status`). It is exact by construction: HubProjectKey
// joins with the single separator that an origin may not itself contain.
func hubKeyProjectDir(origin, key string) string {
	return strings.TrimPrefix(key, hubKeyPrefix+origin+hubKeySeparator)
}

// validateHubOrigin checks a publisher-supplied origin before it is composed
// into a registry key (plan 031 D15). An origin is a machine name: non-empty,
// bounded like a hostname, and free of the separators that would let one
// publisher's composed key take another key's shape — "/" (directories), ":"
// (the key prefix) and the key separator itself.
//
// WHAT THIS IS NOT. The origin is the caller's own CLAIM, and the hub's only
// credential is one shared bearer token (D6/D12), so nothing here proves a
// publisher is the machine it says it is. Composition makes cross-tenant access
// impossible BY ACCIDENT — a publisher naming someone else's directory reaches
// its own key, not theirs — and that is all it makes impossible. A publisher
// that deliberately sends another publisher's origin reaches that publisher's
// registration, which is the accepted residual risk in the plan's §8 (and is
// demonstrated, deliberately, by
// TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation). Closing it
// needs per-origin credentials, not a stricter string rule.
func validateHubOrigin(origin string) error {
	if origin == "" {
		return errors.New("origin is required on the hub control plane")
	}
	if len(origin) > 253 {
		return errors.New("origin is too long (max 253 characters)")
	}
	if strings.ContainsAny(origin, ":/"+hubKeySeparator) {
		return fmt.Errorf(`origin must not contain ":", "/" or %q`, hubKeySeparator)
	}
	for _, r := range origin {
		if r <= ' ' || r == 0x7f {
			return errors.New("origin must not contain whitespace or control characters")
		}
	}
	return nil
}

// hubProjectDirMaxBytes bounds a publisher-supplied project directory. Linux
// caps a path at PATH_MAX (4096) and every other platform is smaller, so this
// rejects nothing real while refusing a megabyte of "directory" as a registry
// key, a ring key, and a log line (plan 031 F9).
const hubProjectDirMaxBytes = 4096

// validateHubProjectDir checks the directory half of a composed key (plan 031
// F2/F9). It must be SLASH-ABSOLUTE: that is what makes "<origin>|<dir>" a
// shape no local key can take on any platform, rather than one that happens to
// differ on the platforms prox is built for today. A Windows-shaped path
// (`C:\work\app`) is refused here, which is the point — not because Windows is
// a target, but because the invariant must not depend on it never becoming one.
func validateHubProjectDir(dir string) error {
	if dir == "" {
		return errors.New("project_dir is required")
	}
	if len(dir) > hubProjectDirMaxBytes {
		return fmt.Errorf("project_dir is too long (max %d bytes)", hubProjectDirMaxBytes)
	}
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("project_dir must be an absolute path beginning with %q, got %q", "/", dir)
	}
	for _, r := range dir {
		if r < ' ' || r == 0x7f {
			return errors.New("project_dir must not contain control characters")
		}
	}
	return nil
}

// LoadHubConfig reads ~/.prox/hub.yaml. A missing file returns an error
// wrapping os.ErrNotExist so callers can tell "no hub configured yet" (a first
// `prox hub start`, or a daemon with no autostart) from a malformed one.
func LoadHubConfig() (HubConfig, error) {
	path := HubConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return HubConfig{}, fmt.Errorf("reading %s: %w", path, os.ErrNotExist)
		}
		return HubConfig{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return parseHubConfig(data, path)
}

// parseHubConfig parses hub.yaml's bytes strictly, named by path for error
// messages (plan 031 C2's "all three YAML schemas get the same strict
// treatment").
//
// The strict-walk helper prox.yaml uses lives unexported in internal/config,
// which proxyd cannot reach, so strictness here is yaml.v3's own:
// KnownFields(true) rejects unknown keys, yaml.v3 rejects duplicate keys in a
// mapping, and a second Decode catches multi-document input. The rejected set
// is the same; only the error text differs from internal/config's batched,
// sorted form.
func parseHubConfig(data []byte, path string) (HubConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg HubConfig
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is an empty config, not a parse error; validation
			// then reports the missing domain.
			return HubConfig{}, nil
		}
		return HubConfig{}, fmt.Errorf("%s: %w", path, err)
	}

	var extra yaml.Node
	err := dec.Decode(&extra)
	switch {
	case err == nil:
		return HubConfig{}, fmt.Errorf("%s: multi-document YAML is not supported", path)
	case errors.Is(err, io.EOF):
		return cfg, nil
	default:
		return HubConfig{}, fmt.Errorf("%s: %w", path, err)
	}
}

// SaveHubConfig writes ~/.prox/hub.yaml 0600 and ATOMICALLY (temp file in the
// same directory → fsync → rename → parent-dir fsync), never an in-place
// truncate (plan 031 D18/P8): a crash mid-write leaves the previous config
// intact rather than a half-written one. The token is not part of the file (see
// HubConfig.Token).
func SaveHubConfig(cfg HubConfig) error {
	if err := EnsureDaemonDir(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling hub config: %w", err)
	}
	path := HubConfigPath()
	if err := hubFileWriter.WriteFile(path, data, constants.FilePermissionPrivate); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// EnsureHubToken returns the hub's bearer token, generating one on first use.
// The file is 0600 and written atomically (D18): a token truncated to nothing
// by a crash would silently turn a token-protected hub into one that rejects
// every publisher.
func EnsureHubToken() (string, error) {
	path := HubTokenPath()
	data, err := os.ReadFile(path)
	if err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
		// An empty or whitespace-only token file is unusable; replace it.
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return RotateHubToken()
}

// RotateHubToken generates a new hub token, writes it, and returns it. The
// caller is responsible for telling a RUNNING hub to accept only the new value
// (POST /api/v1/hub/token does both). There is no grace period: established
// connections authenticated at connect time and survive, but the next call
// carrying the old token gets 401 (D18).
func RotateHubToken() (string, error) {
	if err := EnsureDaemonDir(); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating hub token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	path := HubTokenPath()
	if err := hubFileWriter.WriteFile(path, []byte(token+"\n"), constants.FilePermissionPrivate); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return token, nil
}

// ReadHubToken returns the token currently on disk, or an error wrapping
// os.ErrNotExist when no token has been generated yet.
func ReadHubToken() (string, error) {
	path := HubTokenPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("reading %s: %w", path, os.ErrNotExist)
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// hubFileWriter is the atomic writer ~/.prox/hub.yaml and ~/.prox/hub.token
// are written through (plan 031 D18/P8): temp file in the target's own
// directory, fsync, rename, parent-dir fsync -- never the in-place truncate
// WriteDaemonState uses, so a crash mid-write cannot leave a token truncated to
// nothing. The sequence lives in domain.AtomicWriter, which internal/config and
// internal/tui share; it used to be a deliberate second copy here, and a third
// in the TUI.
var hubFileWriter = domain.AtomicWriter{TempPattern: ".prox-hub-*.tmp"}

// hubListenClass classifies an address the hub control plane might bind (plan
// 031 D6, amended by F8).
//
// The amendment is the important part. D6 originally allowed any RFC 1918
// address, but the control plane speaks PLAIN HTTP with a shared bearer token:
// on an arbitrary LAN — a café, a hotel, a co-working 192.168/16 — a passive
// peer can lift that token off the wire and then exercise every hole the trust
// model already accepts (a forged origin reaches another publisher's
// registration; see validateHubOrigin). So the classes are:
//
//   - ENCRYPTED: loopback (127.0.0.0/8, ::1), and a CGNAT 100.64/10 address
//     THAT SITS ON A TUNNEL INTERFACE. Loopback never leaves the machine, and a
//     tailnet address is reachable only over WireGuard, so the token is never in
//     cleartext on a wire a stranger shares. These are the DEFAULT.
//
//     The interface test is not decoration (plan 031, review B5). 100.64/10 is
//     the CGNAT range, and Tailscale is only one of its users: a phone hotspot,
//     a carrier-grade-NAT ISP, and plenty of campus networks hand out addresses
//     in it on an ORDINARY interface. Treating the range itself as evidence of
//     encryption would auto-select such an address by default and put the
//     bearer token on a plaintext carrier network. What actually distinguishes
//     a tailnet address is the device it lives on — a point-to-point tunnel
//     (tailscale0, utunN, wg0), never a broadcast LAN interface — so that is
//     what is tested, and a CGNAT address anywhere else is treated as plain LAN
//     and needs the same explicit opt-in.
//
//   - UNENCRYPTED LAN: 10/8, 172.16/12, 192.168/16, fc00::/7. Still supported,
//     but only with an explicit opt-in (hub.yaml `allow_unencrypted_lan: true`
//     or `prox hub start --allow-unencrypted-lan`), because "private" is not
//     "confidential".
//
//   - REFUSED: everything else, including the UNSPECIFIED addresses 0.0.0.0 and
//     :: — the dangerous typo, since they bind every interface including a
//     public one — and any public address. No flag opts into those.
type hubListenClass int

const (
	hubListenRefused hubListenClass = iota
	hubListenEncrypted
	hubListenUnencryptedLAN
)

// isCGNATv4 reports whether a 4-byte address is in 100.64.0.0/10, the range
// Tailscale assigns — and the range carrier-grade NAT and some campus networks
// assign too, which is why membership alone decides nothing (review B5).
func isCGNATv4(v4 net.IP) bool {
	return v4[0] == 100 && v4[1]&0xc0 == 64
}

// tunnelInterfaceAddrs returns the addresses this machine holds on a
// point-to-point TUNNEL interface, as a set of IP strings (plan 031, review B5).
//
// Two signals, either of which is enough:
//
//   - The interface NAME: "tailscale0" on Linux/BSD/Windows, "utunN" on macOS
//     (where Tailscale uses a generic utun device), "wgN" for a bare WireGuard
//     interface.
//   - The point-to-point FLAG with no broadcast, which is what a tun device is
//     and what an Ethernet or Wi-Fi interface is not. This is the OS-agnostic
//     half, and the one that keeps the rule honest on a system that names its
//     tailnet device something else.
//
// A failure to enumerate yields an empty set, which is the SAFE direction: a
// CGNAT address then classifies as plain LAN and needs the explicit opt-in.
//
// It is a variable so the classification tests can pin the decision without
// depending on the machine they run on.
var tunnelInterfaceAddrs = func() map[string]struct{} {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make(map[string]struct{})
	for _, iface := range ifaces {
		if !isTunnelInterface(iface) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				out[ipNet.IP.String()] = struct{}{}
			}
		}
	}
	return out
}

// isTunnelInterface applies the name-or-flags test described on
// tunnelInterfaceAddrs.
func isTunnelInterface(iface net.Interface) bool {
	name := strings.ToLower(iface.Name)
	switch {
	case strings.HasPrefix(name, "tailscale"), strings.HasPrefix(name, "utun"), strings.HasPrefix(name, "wg"):
		return true
	}
	return iface.Flags&net.FlagPointToPoint != 0 && iface.Flags&net.FlagBroadcast == 0
}

func classifyHubListenAddr(ip net.IP) hubListenClass {
	return classifyHubListenAddrWith(ip, tunnelInterfaceAddrs())
}

// classifyHubListenAddrWith is classifyHubListenAddr's PURE body: every input is
// passed in, so the rules can be table-tested (including the CGNAT split) rather
// than being a function of whatever interfaces the test machine happens to have.
func classifyHubListenAddrWith(ip net.IP, tunnelAddrs map[string]struct{}) hubListenClass {
	if ip == nil {
		return hubListenRefused
	}
	// 0.0.0.0 and :: first: both would otherwise fall through to the
	// per-family checks, and neither is a place to put this endpoint.
	if ip.IsUnspecified() {
		return hubListenRefused
	}
	if ip.IsLoopback() {
		return hubListenEncrypted
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case isCGNATv4(v4):
			// 100.64.0.0/10. A tailnet address only when it lives on a tunnel
			// device; otherwise it is a carrier or LAN address that merely
			// shares the range, and it needs the LAN opt-in like any other.
			if _, ok := tunnelAddrs[ip.String()]; ok {
				return hubListenEncrypted
			}
			return hubListenUnencryptedLAN
		case v4[0] == 10:
			return hubListenUnencryptedLAN // 10.0.0.0/8
		case v4[0] == 172 && v4[1]&0xf0 == 16:
			return hubListenUnencryptedLAN // 172.16.0.0/12
		case v4[0] == 192 && v4[1] == 168:
			return hubListenUnencryptedLAN // 192.168.0.0/16
		default:
			return hubListenRefused
		}
	}
	if v6 := ip.To16(); v6 != nil && v6[0]&0xfe == 0xfc {
		return hubListenUnencryptedLAN // fc00::/7 (IPv6 unique-local)
	}
	return hubListenRefused
}

// isPrivateListenAddr reports whether ip is bindable at all — under the default
// rules or with the LAN opt-in. Used where only "could this ever be a listen
// address" matters (address discovery and the advice text).
func isPrivateListenAddr(ip net.IP) bool {
	return classifyHubListenAddr(ip) != hubListenRefused
}

// DefaultHubListenAddr returns the address a first `prox hub start` should use
// (plan 031 D6/D14): the machine's first CGNAT (100.64/10) interface address —
// a tailnet address, which is what makes the hub reachable from other machines
// and phones — at HubDefaultPort. When there is no tailnet address it falls
// back to loopback and reports tailnet=false so the caller can warn that only
// this machine will be able to publish.
func DefaultHubListenAddr() (addr string, tailnet bool) {
	port := strconv.Itoa(constants.HubDefaultPort)
	// The candidates are the ENCRYPTED ones, not merely the ones in 100.64/10
	// (plan 031, review B5): a CGNAT address handed out by a carrier or a
	// hotspot is in the same range and is not a tailnet address, and picking it
	// by default would put the bearer token on a plaintext network with no flag
	// and no question asked.
	for _, ip := range localEncryptedAddrs() {
		return net.JoinHostPort(ip.String(), port), true
	}
	return net.JoinHostPort("127.0.0.1", port), false
}

// localPrivateAddrs returns this machine's non-loopback interface addresses
// that the hub is allowed to bind, ENCRYPTED (tailnet) ones first so the refusal
// message and the default both name the most useful address. Interface
// enumeration failures yield an empty list rather than an error: every caller
// treats this as advisory.
func localPrivateAddrs() []net.IP {
	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	tunnelAddrs := tunnelInterfaceAddrs()
	var encrypted, other []net.IP
	for _, a := range ifaceAddrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() {
			continue
		}
		switch classifyHubListenAddrWith(ip, tunnelAddrs) {
		case hubListenEncrypted:
			encrypted = append(encrypted, ip)
		case hubListenUnencryptedLAN:
			other = append(other, ip)
		}
	}
	return append(encrypted, other...)
}

// validateHubListenAddr checks a configured listen address against D6's rule as
// amended by F8 and returns the normalized "host:port" form. A host with no
// port gets HubDefaultPort. allowUnencryptedLAN is the explicit opt-in that
// admits the plain RFC 1918 / ULA ranges.
//
// Both refusals name what to do instead, because "that address is not allowed"
// without an alternative is an error a user cannot act on — and the LAN refusal
// additionally says WHY, since an operator who just typed their own 192.168
// address deserves better than a rule quoted back at them.
func validateHubListenAddr(addr string, allowUnencryptedLAN bool) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port: accept the bare host and supply the default port.
		host, port = addr, strconv.Itoa(constants.HubDefaultPort)
	}
	if host == "" {
		return "", hubConfigErrorf("listen address %q has no host: %s", addr, hubListenAdvice())
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return "", hubConfigErrorf("listen address %q has an invalid port %q", addr, port)
	}
	// Port 0 is the one value the shared rule refuses that IS meaningful here:
	// it means "bind an ephemeral port", which is how the tests get a control
	// plane without racing each other for a fixed number. Everything else goes
	// through domain.ValidatePort, so a typo like 84433 is a 400
	// HUB_CONFIG_INVALID from this one validation point rather than a 500
	// HUB_BIND_FAILED from net.Listen much later (plan 031 D6).
	if portNum != 0 {
		if err := domain.ValidatePort(portNum); err != nil {
			return "", hubConfigErrorf("listen address %q has an invalid port %q: %v", addr, port, err)
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", hubConfigErrorf("listen address %q must be a literal IP address, not a hostname: %s", addr, hubListenAdvice())
	}
	switch classifyHubListenAddr(ip) {
	case hubListenEncrypted:
	case hubListenUnencryptedLAN:
		if !allowUnencryptedLAN {
			return "", hubConfigErrorf("listen address %q is a plain LAN address: %s", addr, hubLANAdvice())
		}
	default:
		return "", hubConfigErrorf("listen address %q is not allowed: %s", addr, hubListenAdvice())
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// hubLANAdvice explains the F8 refusal: why a private LAN address is not
// enough, and the two ways forward.
func hubLANAdvice() string {
	var b strings.Builder
	b.WriteString("the hub control plane speaks plain HTTP and authenticates with one shared bearer token,")
	b.WriteString(" so anyone else on that LAN can read the token off the wire and then publish, deregister,")
	b.WriteString(" or read another publisher's captured traffic as that publisher.")
	b.WriteString(" By default the hub binds only addresses whose transport is encrypted end to end:")
	b.WriteString(" loopback, or a tailnet (100.64/10) address.")
	if addrs := localEncryptedAddrs(); len(addrs) > 0 {
		b.WriteString(" This machine's tailnet address: ")
		b.WriteString(addrs[0].String())
		b.WriteString(" — try --listen ")
		b.WriteString(net.JoinHostPort(addrs[0].String(), strconv.Itoa(constants.HubDefaultPort)))
		b.WriteString(".")
	}
	b.WriteString(" If this LAN really is trusted, opt in explicitly with")
	b.WriteString(" 'prox hub start --allow-unencrypted-lan' (or allow_unencrypted_lan: true in ")
	b.WriteString(HubConfigFileName)
	b.WriteString(").")
	return b.String()
}

// hubListenAdvice is the actionable half of a refused-listen-address error: the
// rule, and the addresses on THIS machine that satisfy it.
func hubListenAdvice() string {
	var b strings.Builder
	b.WriteString("the hub control plane must bind a loopback or tailnet (100.64/10) address")
	b.WriteString(", or — with --allow-unencrypted-lan — a private one (10/8, 172.16/12, 192.168/16, fc00::/7)")
	b.WriteString("; never 0.0.0.0, ::, or a public address")
	addrs := localPrivateAddrs()
	if len(addrs) == 0 {
		b.WriteString(". This machine has no private address besides loopback; try --listen 127.0.0.1:")
		b.WriteString(strconv.Itoa(constants.HubDefaultPort))
		return b.String()
	}
	strs := make([]string, 0, len(addrs))
	for _, ip := range addrs {
		strs = append(strs, ip.String())
	}
	b.WriteString(". This machine's private addresses: ")
	b.WriteString(strings.Join(strs, ", "))
	b.WriteString(" — try --listen ")
	b.WriteString(net.JoinHostPort(addrs[0].String(), strconv.Itoa(constants.HubDefaultPort)))
	return b.String()
}

// localEncryptedAddrs returns this machine's tailnet (CGNAT) addresses — the
// non-loopback ones the hub may bind with no opt-in.
func localEncryptedAddrs() []net.IP {
	tunnelAddrs := tunnelInterfaceAddrs()
	var out []net.IP
	for _, ip := range localPrivateAddrs() {
		if classifyHubListenAddrWith(ip, tunnelAddrs) == hubListenEncrypted {
			out = append(out, ip)
		}
	}
	return out
}

// hubConfigError marks a hub CONFIGURATION defect — a missing domain, a bad
// auth mode, a refused listen address — as opposed to a runtime failure such as
// a port already in use. The socket hub/start handler distinguishes them so a
// user's typo answers 400 HUB_CONFIG_INVALID rather than 500.
type hubConfigError struct{ err error }

func (e *hubConfigError) Error() string { return e.err.Error() }
func (e *hubConfigError) Unwrap() error { return e.err }

func hubConfigErrorf(format string, args ...any) error {
	return &hubConfigError{err: fmt.Errorf(format, args...)}
}

// NormalizeHubConfig fills in defaults and validates a hub configuration,
// returning the config StartHub will actually serve (plan 031 D6/D14). It is
// the single validation point: `prox hub start` runs it before writing
// hub.yaml, and StartHub runs it again on whatever it is handed, so a
// hand-edited file gets the same treatment as a flag.
func NormalizeHubConfig(cfg HubConfig) (HubConfig, error) {
	cfg.Domain = strings.TrimSpace(cfg.Domain)
	if cfg.Domain == "" {
		return HubConfig{}, hubConfigErrorf("hub domain is required (prox hub start --domain <domain>)")
	}
	// The same domain rule internal/config applies to proxy.domain (plan 031
	// F9): every remote hostname is "<service>.<this>", so a domain that is not
	// a DNS name produces routes and certificate names nobody can reach.
	if err := domain.ValidateDomainName(cfg.Domain); err != nil {
		return HubConfig{}, hubConfigErrorf("hub domain: %s", err.Error())
	}

	switch cfg.Auth {
	case "":
		cfg.Auth = HubAuthToken
	case HubAuthToken, HubAuthNone:
	default:
		return HubConfig{}, hubConfigErrorf("hub auth must be %q or %q, got %q", HubAuthToken, HubAuthNone, cfg.Auth)
	}

	if strings.TrimSpace(cfg.Listen) == "" {
		addr, _ := DefaultHubListenAddr()
		cfg.Listen = addr
	}
	listen, err := validateHubListenAddr(strings.TrimSpace(cfg.Listen), cfg.AllowUnencryptedLAN)
	if err != nil {
		return HubConfig{}, err
	}
	cfg.Listen = listen

	// 0 means "no listener of this kind" (P15); every other value is a real
	// port and follows the shared range rule, so an out-of-range number is
	// refused here as HUB_CONFIG_INVALID instead of surviving to the listener.
	for _, p := range []struct {
		name string
		port int
	}{{"https_port", cfg.HTTPSPort}, {"http_port", cfg.HTTPPort}} {
		if p.port == 0 {
			continue
		}
		if err := domain.ValidatePort(p.port); err != nil {
			return HubConfig{}, hubConfigErrorf("hub %s: %v", p.name, err)
		}
	}
	if cfg.HTTPSPort == 0 && cfg.HTTPPort == 0 {
		return HubConfig{}, hubConfigErrorf("hub must expose at least one data-plane port (https_port or http_port)")
	}
	if cfg.HTTPSPort > 0 && cfg.HTTPSPort == cfg.HTTPPort {
		return HubConfig{}, hubConfigErrorf("hub https_port and http_port cannot be the same (%d)", cfg.HTTPSPort)
	}
	return cfg, nil
}
