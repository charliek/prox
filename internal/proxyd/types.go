// Package proxyd implements the shared proxy daemon that allows multiple
// prox instances to register routes through a single set of proxy ports.
// Communication happens over a Unix socket HTTP API at ~/.prox/proxy.sock.
package proxyd

import (
	"time"

	"github.com/charliek/prox/internal/domain"
)

// ServiceTarget represents a backend service to proxy to.
type ServiceTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// RegisterRequest is sent by prox up to register a project's routes.
type RegisterRequest struct {
	ProjectDir     string                   `json:"project_dir"`
	PID            int                      `json:"pid"`
	Version        string                   `json:"version"`
	Domain         string                   `json:"domain"`
	Services       map[string]ServiceTarget `json:"services"`
	HTTPPort       int                      `json:"http_port,omitempty"`
	HTTPSPort      int                      `json:"https_port,omitempty"`
	CaptureEnabled bool                     `json:"capture_enabled,omitempty"`
	// MaxBodySize is the project's configured per-request/response capture cap in
	// BYTES, populated from cfg.Proxy.Capture.MaxBodySize (D13, #49). The daemon
	// stamps it onto the project's routes and passes it as the per-call capture
	// limit on the hot path; 0 means "use the daemon default"
	// (DefaultCaptureMaxBodySize). Wire-compatible in both directions: an older
	// daemon ignores the unknown field, and an omitted field decodes to 0.
	MaxBodySize int64 `json:"max_body_size,omitempty"`
	// DiskBudget is the project's configured capture disk budget in BYTES (#69),
	// populated from cfg.Proxy.Capture.DiskBudget; 0 means "use the daemon
	// default" (DefaultCaptureDiskBudget). The daemon folds every registered
	// capture-enabled project's budget into a single effective daemon-wide bound
	// for the one shared capture dir: the min of each project's budget-or-default
	// (an unset project contributes the default, so one project can never raise
	// another's bound; raising above the default takes every enabled project
	// opting in — see EffectiveCaptureDiskBudget). Wire-compatible in both
	// directions: an older daemon ignores the unknown field, and an omitted field
	// decodes to 0.
	DiskBudget int64 `json:"disk_budget,omitempty"`
	// StartTime is an opaque process start token (see daemon.ProcessStartTime):
	// a generation discriminator, not a timestamp. 0 means the client could not
	// read it, so the daemon falls back to bare-PID liveness for this holder.
	StartTime int64 `json:"start_time,omitempty"`

	// --- network (hub) mount only (plan 031 §4.3) ---
	// These three fields are read ONLY by the hub's network control plane. The
	// Unix socket mount clears Origin and Takeover on arrival and ignores
	// ProtocolVersion, so a socket registration can never claim an origin (and
	// therefore never composes an origin-qualified registry key).

	// Origin is the publisher machine's name. The hub composes the registry key
	// itself as HubProjectKey(origin, project_dir) (D5/D15) — it NEVER accepts a
	// pre-qualified key from the wire — so this is the caller's own identity,
	// not a target selector.
	Origin string `json:"origin,omitempty"`
	// ProtocolVersion is the publisher's hub wire version, checked against
	// constants.HubProtocolVersion on the network mount only (D7).
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// Takeover asks the hub to displace a CONNECTED holder of a colliding
	// service name (D10). Connection state arrives with the tunnel in C4, so
	// this commit carries the field on the wire without acting on it: with no
	// sessions yet, nothing is ever connected and the cross-publisher
	// collision path is still the registry's plain route conflict.
	Takeover bool `json:"takeover,omitempty"`
}

// RegisterResponse is returned after a successful registration.
type RegisterResponse struct {
	Registered []string `json:"registered"` // fully-qualified hostnames registered
	// Warnings carries user-facing advisories the daemon observed on the
	// project's behalf — things the invoking CLI could never have seen itself,
	// because in shared mode they happen inside the daemon process, whose
	// stdout/stderr are /dev/null (daemon.go). The register response is the one
	// point where the daemon can hand them to the person who typed the command.
	// It is populated on BOTH success arms of register(), including the
	// idempotent no-op refresh the self-heal path takes, so a reconnecting
	// client still learns about them.
	//
	// Compatibility: this is ordinary additive JSON — an older daemon simply
	// omits the field and it decodes to nil; an older client ignores the unknown
	// key. That, not the register version gate, is what makes it safe: local
	// `make build` binaries all report version "dev"
	// (internal/version/version.go), so the exact-version-match requirement does
	// NOT separate two different development builds, and a dev client can very
	// well talk to a dev daemon built before this field existed.
	Warnings []domain.Warning `json:"warnings,omitempty"`
}

// DeregisterRequest is sent by prox down to remove a project's routes.
type DeregisterRequest struct {
	ProjectDir string `json:"project_dir"`
	PID        int    `json:"pid"`
	// Origin is the caller's own machine name on the NETWORK mount (plan 031
	// D15). The hub composes HubProjectKey(origin, project_dir) itself, so a
	// publisher can only ever deregister its own registration — never another
	// publisher's, and never a LOCAL project (whose key is a bare dir that no
	// origin composition can produce). Ignored on the socket mount.
	Origin string `json:"origin,omitempty"`
}

// RouteInfo describes a single registered route.
type RouteInfo struct {
	Hostname     string        `json:"hostname"`
	Port         int           `json:"port"`
	Protocol     string        `json:"protocol"` // "http" or "https"
	Target       ServiceTarget `json:"target"`
	ProjectDir   string        `json:"project_dir"`
	PID          int           `json:"pid"`
	RegisteredAt time.Time     `json:"registered_at"`
	// Origin is the publishing machine for a hub-registered route, empty for a
	// LOCAL one (plan 031 D5). It is what `prox proxy routes` renders as the
	// SOURCE column, and what tells the data plane a route's target lives
	// through a tunnel rather than on this host. ProjectDir for such a route is
	// the composed key "<origin>:<dir>".
	Origin string `json:"origin,omitempty"`
	// Connected reports whether the route can currently be served. It is always
	// true for a local route (the daemon dials the target directly). For a hub
	// route it reflects the publisher's tunnel, which arrives in C4 — until
	// then a remote route reports false, because there is no session to serve
	// it through.
	Connected bool `json:"connected"`
}

// DaemonStatusResponse is returned by the status endpoint.
type DaemonStatusResponse struct {
	Version       string      `json:"version"`
	PID           int         `json:"pid"`
	Uptime        string      `json:"uptime"`
	StartedAt     time.Time   `json:"started_at"`
	Routes        []RouteInfo `json:"routes"`
	ListenerPorts []int       `json:"listener_ports"`
	ProjectCount  int         `json:"project_count"`
	RouteCount    int         `json:"route_count"`
	// DroppedEvents is the daemon-wide count of SSE-subscriber notifications
	// dropped because a subscriber's channel was full (D9). It is summed across
	// every project's ring (D13 per-project managers). It surfaces the
	// request-stream degradation the forwarder would otherwise absorb silently.
	DroppedEvents int64 `json:"dropped_events"`
	// RecordCounts is the per-project count of records currently held in memory,
	// keyed by project dir (D13). It makes the N×ring memory trade-off of the
	// per-project rings diagnosable. Empty when no project is registered.
	RecordCounts map[string]int `json:"record_counts,omitempty"`
	// CaptureDiskUsed is the total logical bytes of spilled capture body files on
	// disk across ALL projects (the daemon's flat capture dir), and
	// CaptureDiskBudget is the effective daemon-wide ceiling enforced against it
	// (#69). Both are 0 when the daemon has no capture manager.
	CaptureDiskUsed   int64 `json:"capture_disk_used"`
	CaptureDiskBudget int64 `json:"capture_disk_budget"`
	// CaptureAvailable reports whether the daemon initialized a capture manager
	// at startup (plan 012 D1, C4). false here means capture cannot work for ANY
	// project on this daemon regardless of their own proxy.capture.enabled --
	// distinct from a project simply choosing capture off. CaptureError carries
	// the init failure reason (e.g. home directory unresolved, capture dir
	// uncreatable) when CaptureAvailable is false; empty when capture is
	// available or the daemon predates this field.
	CaptureAvailable bool   `json:"capture_available"`
	CaptureError     string `json:"capture_error,omitempty"`
	// Hub is the hub HOST's view of hub mode (plan 031 D20), present only while
	// hub mode is ON — a daemon with no hub emits no `hub` key at all, so
	// hub-less status output is unchanged. This is NOT the publisher-side hub
	// object: that one lives on the project API's status.proxy.hub and
	// describes a project's own publishing state (C5).
	Hub *HubStatus `json:"hub,omitempty"`
}

// HubStatus is the hub HOST's view of hub mode (plan 031 D20), returned by the
// socket endpoints GET /api/v1/hub/status and GET /api/v1/status. Enabled is
// false with every other field zero when hub mode is off.
type HubStatus struct {
	Enabled bool   `json:"enabled"`
	Domain  string `json:"domain"`
	// Listen is the address the control plane is ACTUALLY bound to, not the
	// configured one: `--listen host:0` binds an ephemeral port and this is
	// where the operator (and the tests) read it back (plan 031 D14/P15).
	Listen    string    `json:"listen"`
	HTTPSPort int       `json:"https_port"`
	HTTPPort  int       `json:"http_port"`
	Auth      string    `json:"auth"` // "token" | "none"
	StartedAt time.Time `json:"started_at,omitempty"`
	// Publishers lists every remote registration currently held, newest-key
	// order not guaranteed — the CLI sorts.
	Publishers []HubPublisher `json:"publishers"`
}

// HubPublisher is one remote registration as the hub operator sees it.
type HubPublisher struct {
	// Origin is the publishing machine, ProjectDir the publisher's OWN
	// directory, and Key the composed registry key "<origin>:<dir>" the hub
	// actually stores it under (plan 031 D5).
	Origin     string `json:"origin"`
	ProjectDir string `json:"project_dir"`
	Key        string `json:"key"`
	// Connected reflects the publisher's tunnel; sessions arrive in C4, so this
	// commit always reports false for a registration that has no session layer
	// to consult.
	Connected    bool      `json:"connected"`
	RegisteredAt time.Time `json:"registered_at"`
	Routes       int       `json:"routes"`
}

// HubTokenResponse is the socket POST /api/v1/hub/token (rotate) reply: the
// freshly written token and the file it was written to (plan 031 D18).
type HubTokenResponse struct {
	Token string `json:"token"`
	Path  string `json:"path"`
}

// ErrorResponse is the standard error format for daemon API responses.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
