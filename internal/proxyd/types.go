// Package proxyd implements the shared proxy daemon that allows multiple
// prox instances to register routes through a single set of proxy ports.
// Communication happens over a Unix socket HTTP API at ~/.prox/proxy.sock.
package proxyd

import (
	"time"
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
	// StartTime is an opaque process start token (see daemon.ProcessStartTime):
	// a generation discriminator, not a timestamp. 0 means the client could not
	// read it, so the daemon falls back to bare-PID liveness for this holder.
	StartTime int64 `json:"start_time,omitempty"`
}

// RegisterResponse is returned after a successful registration.
type RegisterResponse struct {
	Registered []string `json:"registered"` // fully-qualified hostnames registered
}

// DeregisterRequest is sent by prox down to remove a project's routes.
type DeregisterRequest struct {
	ProjectDir string `json:"project_dir"`
	PID        int    `json:"pid"`
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
}

// ErrorResponse is the standard error format for daemon API responses.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
