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
	Domain    string `yaml:"domain"`
	Listen    string `yaml:"listen"`
	HTTPSPort int    `yaml:"https_port"`
	HTTPPort  int    `yaml:"http_port"`
	Auth      string `yaml:"auth"`
	Autostart bool   `yaml:"autostart"`

	// Token is the bearer credential the network mount checks. It is NEVER
	// serialized into hub.yaml — it lives in its own 0600 file
	// (~/.prox/hub.token) so the config can be read, diffed, and edited without
	// handling a secret. StartHub fills it from EnsureHubToken when auth is
	// "token" and the caller left it empty; tests pass it explicitly.
	Token string `yaml:"-"`
}

// HubConfigPath returns ~/.prox/hub.yaml.
func HubConfigPath() string {
	return filepath.Join(DaemonDir(), HubConfigFileName)
}

// HubTokenPath returns ~/.prox/hub.token.
func HubTokenPath() string {
	return filepath.Join(DaemonDir(), HubTokenFileName)
}

// HubProjectKey composes the registry key for a remote registration from the
// caller's OWN origin and project dir (plan 031 D5/D15).
//
// This is the one place the composition exists, used on both the hub side (the
// network handlers) and the publisher side (the request stream's `project`
// param), so the two can never drift. Two properties make it load-bearing:
//
//   - It is DERIVED, never accepted from the wire. A network handler builds the
//     key from the authenticated caller's own identity, so a publisher can only
//     ever address its own registrations.
//   - A composed key can never collide with a LOCAL one. validateHubOrigin
//     rejects an origin containing "/" or ":", so "<origin>:<dir>" always
//     begins with a non-"/" label followed by ":" — a shape no absolute project
//     directory (the local key form) can take. That is why "a publisher cannot
//     deregister a local project" is structural rather than a check someone can
//     forget to write.
func HubProjectKey(origin, dir string) string {
	return origin + ":" + dir
}

// hubKeyProjectDir recovers the publisher's OWN directory from a composed key,
// for display (`prox hub status`). It is exact by construction: HubProjectKey
// joins with the single ":" that an origin may not itself contain.
func hubKeyProjectDir(origin, key string) string {
	return strings.TrimPrefix(key, origin+":")
}

// validateHubOrigin checks a publisher-supplied origin before it is composed
// into a registry key (plan 031 D15). An origin is a machine name: non-empty,
// bounded like a hostname, and free of the separators that would let one
// publisher's composed key impersonate another key shape — "/" (absolute dirs,
// i.e. local keys) and ":" (the composition separator itself).
func validateHubOrigin(origin string) error {
	if origin == "" {
		return errors.New("origin is required on the hub control plane")
	}
	if len(origin) > 253 {
		return errors.New("origin is too long (max 253 characters)")
	}
	if strings.ContainsAny(origin, ":/") {
		return errors.New(`origin must not contain ":" or "/"`)
	}
	for _, r := range origin {
		if r <= ' ' || r == 0x7f {
			return errors.New("origin must not contain whitespace or control characters")
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

// isPrivateListenAddr reports whether ip is an address the hub control plane
// may bind (plan 031 D6): loopback (127.0.0.0/8, ::1), RFC 1918 private IPv4
// (10/8, 172.16/12, 192.168/16), CGNAT 100.64/10 (the range Tailscale assigns),
// or IPv6 unique-local fc00::/7.
//
// Everything else is refused, and the two refusals that matter most are the
// UNSPECIFIED addresses 0.0.0.0 and :: — the dangerous typo, since they bind
// every interface including a public one. A public address is refused outright:
// the control plane speaks plain HTTP in v1 and is protected by a shared bearer
// token, so exposing it beyond a private network would be a real vulnerability
// rather than an inconvenience.
func isPrivateListenAddr(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// 0.0.0.0 and :: first: both would otherwise fall through to the
	// per-family checks, and neither is a place to put this endpoint.
	if ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 10:
			return true // 10.0.0.0/8
		case v4[0] == 172 && v4[1]&0xf0 == 16:
			return true // 172.16.0.0/12
		case v4[0] == 192 && v4[1] == 168:
			return true // 192.168.0.0/16
		case v4[0] == 100 && v4[1]&0xc0 == 64:
			return true // 100.64.0.0/10 (CGNAT / tailnet)
		default:
			return false
		}
	}
	if v6 := ip.To16(); v6 != nil && v6[0]&0xfe == 0xfc {
		return true // fc00::/7 (IPv6 unique-local)
	}
	return false
}

// DefaultHubListenAddr returns the address a first `prox hub start` should use
// (plan 031 D6/D14): the machine's first CGNAT (100.64/10) interface address —
// a tailnet address, which is what makes the hub reachable from other machines
// and phones — at HubDefaultPort. When there is no tailnet address it falls
// back to loopback and reports tailnet=false so the caller can warn that only
// this machine will be able to publish.
func DefaultHubListenAddr() (addr string, tailnet bool) {
	port := strconv.Itoa(constants.HubDefaultPort)
	for _, ip := range localPrivateAddrs() {
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
			return net.JoinHostPort(ip.String(), port), true
		}
	}
	return net.JoinHostPort("127.0.0.1", port), false
}

// localPrivateAddrs returns this machine's non-loopback interface addresses
// that the hub is allowed to bind, CGNAT (tailnet) ones first so the refusal
// message and the default both name the most useful address. Interface
// enumeration failures yield an empty list rather than an error: every caller
// treats this as advisory.
func localPrivateAddrs() []net.IP {
	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var cgnat, other []net.IP
	for _, a := range ifaceAddrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || !isPrivateListenAddr(ip) {
			continue
		}
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
			cgnat = append(cgnat, ip)
			continue
		}
		other = append(other, ip)
	}
	return append(cgnat, other...)
}

// validateHubListenAddr checks a configured listen address against D6's rule
// and returns the normalized "host:port" form. A host with no port gets
// HubDefaultPort. The refusal names this machine's OWN private/tailnet
// addresses, because "that address is not allowed" without saying which one to
// use instead is an error a user cannot act on.
func validateHubListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port: accept the bare host and supply the default port.
		host, port = addr, strconv.Itoa(constants.HubDefaultPort)
	}
	if host == "" {
		return "", hubConfigErrorf("listen address %q has no host: %s", addr, hubListenAdvice())
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", hubConfigErrorf("listen address %q has an invalid port %q", addr, port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", hubConfigErrorf("listen address %q must be a literal IP address, not a hostname: %s", addr, hubListenAdvice())
	}
	if !isPrivateListenAddr(ip) {
		return "", hubConfigErrorf("listen address %q is not allowed: %s", addr, hubListenAdvice())
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// hubListenAdvice is the actionable half of a refused-listen-address error: the
// rule, and the addresses on THIS machine that satisfy it.
func hubListenAdvice() string {
	var b strings.Builder
	b.WriteString("the hub control plane must bind a loopback, private (10/8, 172.16/12, 192.168/16), tailnet (100.64/10), or IPv6 unique-local (fc00::/7) address")
	b.WriteString(" — never 0.0.0.0, ::, or a public address")
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
	listen, err := validateHubListenAddr(strings.TrimSpace(cfg.Listen))
	if err != nil {
		return HubConfig{}, err
	}
	cfg.Listen = listen

	if cfg.HTTPSPort < 0 || cfg.HTTPPort < 0 {
		return HubConfig{}, hubConfigErrorf("hub https_port and http_port must not be negative")
	}
	if cfg.HTTPSPort == 0 && cfg.HTTPPort == 0 {
		return HubConfig{}, hubConfigErrorf("hub must expose at least one data-plane port (https_port or http_port)")
	}
	if cfg.HTTPSPort > 0 && cfg.HTTPSPort == cfg.HTTPPort {
		return HubConfig{}, hubConfigErrorf("hub https_port and http_port cannot be the same (%d)", cfg.HTTPSPort)
	}
	return cfg, nil
}
