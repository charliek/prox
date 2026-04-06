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
}

// ErrorResponse is the standard error format for daemon API responses.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
