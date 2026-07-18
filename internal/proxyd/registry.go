package proxyd

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/charliek/prox/internal/daemon"
)

// PortSpec describes a port that needs a new listener.
type PortSpec struct {
	Port     int
	Protocol string // "http" or "https"
}

// Route represents a single registered proxy route.
type Route struct {
	Hostname     string
	Port         int
	Protocol     string // "http" or "https"
	Target       ServiceTarget
	ProjectDir   string
	PID          int
	RegisteredAt time.Time
	// CaptureEnabled is stamped from the owning project's registration so the
	// dynamic proxy can gate body capture per project (a capture-disabled
	// project's traffic is recorded as metadata only).
	CaptureEnabled bool
}

// ProjectRegistration tracks all routes belonging to a project.
type ProjectRegistration struct {
	Dir            string
	PID            int
	Domain         string
	RouteKeys      []string // "hostname:port" keys into the routes map
	RegisteredAt   time.Time
	CaptureEnabled bool
}

// ListenerInfo tracks the protocol and route count for a port.
type ListenerInfo struct {
	Port       int
	Protocol   string
	RouteCount int
}

// Registry tracks route registrations from multiple projects.
type Registry struct {
	mu        sync.RWMutex
	routes    map[string]*Route               // key: "hostname:port"
	projects  map[string]*ProjectRegistration // key: project dir
	listeners map[int]*ListenerInfo           // key: port
}

// NewRegistry creates a new empty route registry.
func NewRegistry() *Registry {
	return &Registry{
		routes:    make(map[string]*Route),
		projects:  make(map[string]*ProjectRegistration),
		listeners: make(map[int]*ListenerInfo),
	}
}

// routeKey builds the map key for a route.
func routeKey(hostname string, port int) string {
	return fmt.Sprintf("%s:%d", hostname, port)
}

// Register adds a project's routes to the registry.
// Returns the registered hostnames, any new ports that need listeners, or an error on conflict.
func (r *Registry) Register(req RegisterRequest) (hostnames []string, newPorts []PortSpec, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if this project is already registered
	if _, exists := r.projects[req.ProjectDir]; exists {
		return nil, nil, fmt.Errorf("project %s is already registered; deregister first", req.ProjectDir)
	}

	// Reject same port for both HTTP and HTTPS
	if req.HTTPPort > 0 && req.HTTPSPort > 0 && req.HTTPPort == req.HTTPSPort {
		return nil, nil, fmt.Errorf("http_port and https_port cannot be the same (%d)", req.HTTPPort)
	}

	// Build the set of routes this project wants to register.
	type pendingRoute struct {
		hostname string
		port     int
		protocol string
		target   ServiceTarget
	}

	var pending []pendingRoute

	for svcName, target := range req.Services {
		hostname := fmt.Sprintf("%s.%s", svcName, req.Domain)

		if req.HTTPSPort > 0 {
			pending = append(pending, pendingRoute{
				hostname: hostname,
				port:     req.HTTPSPort,
				protocol: "https",
				target:   target,
			})
		}
		if req.HTTPPort > 0 {
			pending = append(pending, pendingRoute{
				hostname: hostname,
				port:     req.HTTPPort,
				protocol: "http",
				target:   target,
			})
		}
	}

	if len(pending) == 0 {
		return nil, nil, fmt.Errorf("no routes to register (no services or no ports configured)")
	}

	// Validate: check for hostname:port conflicts and port protocol mismatches.
	for _, p := range pending {
		key := routeKey(p.hostname, p.port)
		if existing, ok := r.routes[key]; ok {
			return nil, nil, fmt.Errorf(
				"domain %s on port %d is already registered by %s (PID %d)",
				p.hostname, p.port, existing.ProjectDir, existing.PID,
			)
		}

		if li, ok := r.listeners[p.port]; ok {
			if li.Protocol != p.protocol {
				return nil, nil, fmt.Errorf(
					"port %d is already bound as %s, cannot register %s route for %s",
					p.port, li.Protocol, p.protocol, p.hostname,
				)
			}
		}
	}

	// All checks passed — commit the registration.
	now := time.Now()
	var routeKeys []string
	portsNeeded := make(map[int]string) // port -> protocol

	for _, p := range pending {
		key := routeKey(p.hostname, p.port)
		r.routes[key] = &Route{
			Hostname:       p.hostname,
			Port:           p.port,
			Protocol:       p.protocol,
			Target:         p.target,
			ProjectDir:     req.ProjectDir,
			PID:            req.PID,
			RegisteredAt:   now,
			CaptureEnabled: req.CaptureEnabled,
		}
		routeKeys = append(routeKeys, key)
		hostnames = append(hostnames, p.hostname)

		// Track listener info
		if li, ok := r.listeners[p.port]; ok {
			li.RouteCount++
		} else {
			r.listeners[p.port] = &ListenerInfo{
				Port:       p.port,
				Protocol:   p.protocol,
				RouteCount: 1,
			}
			portsNeeded[p.port] = p.protocol
		}
	}

	r.projects[req.ProjectDir] = &ProjectRegistration{
		Dir:            req.ProjectDir,
		PID:            req.PID,
		Domain:         req.Domain,
		RouteKeys:      routeKeys,
		RegisteredAt:   now,
		CaptureEnabled: req.CaptureEnabled,
	}

	for port, proto := range portsNeeded {
		newPorts = append(newPorts, PortSpec{Port: port, Protocol: proto})
	}

	return hostnames, newPorts, nil
}

// Deregister removes all routes for a project.
// Returns the removed hostnames and ports that now have zero routes.
func (r *Registry) Deregister(projectDir string) (removedHostnames []string, emptyPorts []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deregisterLocked(projectDir)
}

// DeregisterIfPID removes a project's routes only if its CURRENT registration
// still carries pid. The check and removal happen under one lock acquisition,
// closing the race where the stale-PID sweep detects a dead generation, the
// project re-registers with a live PID, and the sweep would otherwise tear
// down the new live registration. Returns removed=false when the project is
// gone or has re-registered under a different PID.
func (r *Registry) DeregisterIfPID(projectDir string, pid int) (removed bool, removedHostnames []string, emptyPorts []int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proj, ok := r.projects[projectDir]
	if !ok || proj.PID != pid {
		return false, nil, nil
	}
	removedHostnames, emptyPorts = r.deregisterLocked(projectDir)
	return true, removedHostnames, emptyPorts
}

// deregisterLocked is the shared removal body; r.mu must be held.
func (r *Registry) deregisterLocked(projectDir string) (removedHostnames []string, emptyPorts []int) {
	proj, ok := r.projects[projectDir]
	if !ok {
		return nil, nil
	}

	for _, key := range proj.RouteKeys {
		route, ok := r.routes[key]
		if !ok {
			continue
		}
		removedHostnames = append(removedHostnames, route.Hostname)

		if li, ok := r.listeners[route.Port]; ok {
			li.RouteCount--
			if li.RouteCount <= 0 {
				emptyPorts = append(emptyPorts, route.Port)
				delete(r.listeners, route.Port)
			}
		}

		delete(r.routes, key)
	}

	delete(r.projects, projectDir)
	return removedHostnames, emptyPorts
}

// Lookup finds the route for a given hostname and port.
func (r *Registry) Lookup(hostname string, port int) (*Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.routes[routeKey(hostname, port)]
	return route, ok
}

// AllRoutes returns all registered routes.
func (r *Registry) AllRoutes() []RouteInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	routes := make([]RouteInfo, 0, len(r.routes))
	for _, route := range r.routes {
		routes = append(routes, RouteInfo{
			Hostname:     route.Hostname,
			Port:         route.Port,
			Protocol:     route.Protocol,
			Target:       route.Target,
			ProjectDir:   route.ProjectDir,
			PID:          route.PID,
			RegisteredAt: route.RegisteredAt,
		})
	}
	return routes
}

// ListenerPorts returns all ports with active listeners.
func (r *Registry) ListenerPorts() []int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ports := make([]int, 0, len(r.listeners))
	for port := range r.listeners {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// ProjectCount returns the number of registered projects.
func (r *Registry) ProjectCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.projects)
}

// IsEmpty returns true if no routes are registered.
func (r *Registry) IsEmpty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.routes) == 0
}

// StaleProject identifies a registration whose owning process has died.
type StaleProject struct {
	Dir string
	PID int
}

// StalePIDs returns the registered projects whose PID is no longer running.
// It only detects — removal goes through the consolidated removeProject path
// (PID-guarded via DeregisterIfPID) so the crash path purges captured records
// and body files the same way an explicit deregister does, without racing a
// concurrent re-registration.
func (r *Registry) StalePIDs() []StaleProject {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var stale []StaleProject
	for dir, proj := range r.projects {
		if !daemon.ProcessExists(proj.PID) {
			stale = append(stale, StaleProject{Dir: dir, PID: proj.PID})
		}
	}
	return stale
}
