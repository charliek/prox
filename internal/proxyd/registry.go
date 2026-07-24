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
	Hostname   string
	Port       int
	Protocol   string // "http" or "https"
	Target     ServiceTarget
	ProjectDir string
	PID        int
	// StartTime is the owning process's opaque start token, copied from the
	// project's ProjectRegistration (see daemon.ProcessStartTime). It freezes
	// the generation identity onto the route so the dynamic proxy's on-502
	// dead-owner probe (#74) can hand the exact (dir, PID, StartTime) tuple of
	// the failing generation to the identity-guarded removal path, never
	// re-resolving it — a restart that reused the PID reads as a different
	// generation and is protected by DeregisterIfIdentity.
	StartTime    int64
	RegisteredAt time.Time
	// CaptureEnabled is stamped from the owning project's registration so the
	// dynamic proxy can gate body capture per project (a capture-disabled
	// project's traffic is recorded as metadata only).
	CaptureEnabled bool
	// MaxBodySize is the project's per-request/response capture cap in bytes
	// (D13, #49), stamped from the registration like CaptureEnabled. The dynamic
	// proxy passes it as the per-call capture limit; 0 means the daemon default.
	MaxBodySize int64
}

// ProjectRegistration tracks all routes belonging to a project.
type ProjectRegistration struct {
	Dir       string
	PID       int
	Domain    string
	RouteKeys []string // "hostname:port" keys into the routes map
	// StartTime is an opaque process start token (see daemon.ProcessStartTime):
	// a generation discriminator, not a timestamp. 0 means the holder could not
	// read it, so liveness falls back to bare-PID.
	StartTime      int64
	RegisteredAt   time.Time
	CaptureEnabled bool
	// MaxBodySize is the project's per-request/response capture cap in bytes
	// (D13, #49); 0 means the daemon default. Stamped onto each Route.
	MaxBodySize int64
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

// ProjectConflictError is returned by Register when the target project dir is
// already registered. It carries the existing registration's PID, captured
// under the same lock acquisition that detected the conflict, so callers can
// decide (via a liveness check) whether the holder is a running prox up or a
// crashed one whose registration can be replaced. It is matched with
// errors.As; every other Register error stays plain and is never retried.
type ProjectConflictError struct {
	Dir string
	PID int
	// StartTime is the existing holder's opaque process start token (see
	// daemon.ProcessStartTime): a generation discriminator, not a timestamp. It
	// lets the self-heal liveness check distinguish a still-running holder from a
	// crashed one whose PID has been reused. 0 means bare-PID fallback.
	StartTime int64
}

func (e *ProjectConflictError) Error() string {
	return fmt.Sprintf(
		"project %s is already registered by a running prox up (PID %d); "+
			"stop it or run 'prox proxy stop --force'",
		e.Dir, e.PID,
	)
}

// Register adds a project's routes to the registry.
// Returns the registered hostnames, any new ports that need listeners, or an error on conflict.
func (r *Registry) Register(req RegisterRequest) (hostnames []string, newPorts []PortSpec, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if this project is already registered
	if existing, exists := r.projects[req.ProjectDir]; exists {
		return nil, nil, &ProjectConflictError{
			Dir:       req.ProjectDir,
			PID:       existing.PID,
			StartTime: existing.StartTime,
		}
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
			StartTime:      req.StartTime,
			RegisteredAt:   now,
			CaptureEnabled: req.CaptureEnabled,
			MaxBodySize:    req.MaxBodySize,
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
		StartTime:      req.StartTime,
		RegisteredAt:   now,
		CaptureEnabled: req.CaptureEnabled,
		MaxBodySize:    req.MaxBodySize,
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

// DeregisterIfIdentity removes a project's routes only if its CURRENT
// registration matches BOTH pid and startTime. The check and removal happen
// under one lock acquisition, closing the reused-PID teardown race: the
// stale-PID sweep detects a dead generation, the project re-registers with a
// live process that reused the crashed PID, and a PID-only guard would tear
// down that new live registration. Keying on the start token as well as the PID
// makes the reused-PID restart read as a different identity, so it survives.
// Returns removed=false when the project is gone or has re-registered under a
// different pid or start token.
//
// When startTime is 0 (the holder could not read its start token) the guard
// degrades to PID-only, so exact-PID reuse can still reap a live restart — the
// accepted bare-PID fallback (see daemon.IsProcessAlive).
func (r *Registry) DeregisterIfIdentity(projectDir string, pid int, startTime int64) (removed bool, removedHostnames []string, emptyPorts []int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proj, ok := r.projects[projectDir]
	if !ok || proj.PID != pid || proj.StartTime != startTime {
		return false, nil, nil
	}
	removedHostnames, emptyPorts = r.deregisterLocked(projectDir)
	return true, removedHostnames, emptyPorts
}

// routeDescriptor builds a canonical key for a single route's full identity
// (hostname, port, protocol, and backend target) so two route sets can be
// compared as sets regardless of iteration order. Used by registrationMatches.
func routeDescriptor(hostname string, port int, protocol string, target ServiceTarget) string {
	return fmt.Sprintf("%s:%d|%s|%s:%d", hostname, port, protocol, target.Host, target.Port)
}

// registrationMatches reports whether projectDir's CURRENT registration would be
// reproduced byte-for-byte by req: the same route set (hostname + port + protocol
// + backend target for every route) AND the same capture flag. It is the D6a
// no-op-refresh discriminator — server.register's same-identity arm uses it to
// take a true no-op path (no remove+add, no listener churn, no record purge) when
// the re-registering process's config is unchanged, which is the common heal case.
// r.mu is taken for read.
func (r *Registry) registrationMatches(req RegisterRequest) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	proj, ok := r.projects[req.ProjectDir]
	if !ok {
		return false
	}
	if proj.CaptureEnabled != req.CaptureEnabled {
		return false
	}
	// A changed capture cap must NOT take the no-op refresh path — the new cap
	// has to reach the routes so subsequent captures honor it (D13).
	if proj.MaxBodySize != req.MaxBodySize {
		return false
	}

	// Build the descriptor set the request would register (mirrors Register's
	// pending-route construction).
	desired := make(map[string]struct{})
	for svcName, target := range req.Services {
		hostname := fmt.Sprintf("%s.%s", svcName, req.Domain)
		if req.HTTPSPort > 0 {
			desired[routeDescriptor(hostname, req.HTTPSPort, "https", target)] = struct{}{}
		}
		if req.HTTPPort > 0 {
			desired[routeDescriptor(hostname, req.HTTPPort, "http", target)] = struct{}{}
		}
	}

	// The existing route set must be exactly the desired set: same cardinality
	// AND every current route present in desired.
	if len(desired) != len(proj.RouteKeys) {
		return false
	}
	for _, key := range proj.RouteKeys {
		route, ok := r.routes[key]
		if !ok {
			return false
		}
		if _, want := desired[routeDescriptor(route.Hostname, route.Port, route.Protocol, route.Target)]; !want {
			return false
		}
	}
	return true
}

// ProjectHostnames returns the hostnames currently registered for projectDir in
// route-key order, or nil when it isn't registered. server.register's no-op
// idempotent-refresh arm (D6a) echoes this unchanged set instead of running a
// remove+add.
func (r *Registry) ProjectHostnames(projectDir string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	proj, ok := r.projects[projectDir]
	if !ok {
		return nil
	}
	hostnames := make([]string, 0, len(proj.RouteKeys))
	for _, key := range proj.RouteKeys {
		if route, ok := r.routes[key]; ok {
			hostnames = append(hostnames, route.Hostname)
		}
	}
	return hostnames
}

// projectSnapshot captures a project's full registry footprint (its
// ProjectRegistration plus a deep copy of every Route it owns) so
// server.register's same-identity DIFFERENT arm can restore it verbatim when a
// config-changed remove+add fails — a failed re-register must NEVER leave the
// project unregistered (D6a failure-atomicity).
type projectSnapshot struct {
	proj   *ProjectRegistration
	routes []*Route
}

// snapshotProject deep-copies projectDir's registration and routes under RLock.
// ok is false when the project isn't registered.
func (r *Registry) snapshotProject(projectDir string) (projectSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	proj, ok := r.projects[projectDir]
	if !ok {
		return projectSnapshot{}, false
	}
	projCopy := *proj
	projCopy.RouteKeys = append([]string(nil), proj.RouteKeys...)
	routes := make([]*Route, 0, len(proj.RouteKeys))
	for _, key := range proj.RouteKeys {
		if route, ok := r.routes[key]; ok {
			rc := *route
			routes = append(routes, &rc)
		}
	}
	return projectSnapshot{proj: &projCopy, routes: routes}, true
}

// restoreProject re-inserts a snapshot taken by snapshotProject, rebuilding the
// project's route entries and listener refcounts. It returns the ports whose
// listener entry did NOT exist before restore (their physical listener was closed
// by the removal and must be re-opened by the caller). Used only by
// server.register's failure-atomic DIFFERENT arm.
func (r *Registry) restoreProject(snap projectSnapshot) (reopenPorts []PortSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, route := range snap.routes {
		rc := *route
		r.routes[routeKey(rc.Hostname, rc.Port)] = &rc
		if li, ok := r.listeners[rc.Port]; ok {
			li.RouteCount++
		} else {
			r.listeners[rc.Port] = &ListenerInfo{Port: rc.Port, Protocol: rc.Protocol, RouteCount: 1}
			reopenPorts = append(reopenPorts, PortSpec{Port: rc.Port, Protocol: rc.Protocol})
		}
	}
	projCopy := *snap.proj
	projCopy.RouteKeys = append([]string(nil), snap.proj.RouteKeys...)
	r.projects[projCopy.Dir] = &projCopy
	return reopenPorts
}

// sameRegistrationIdentity reports whether a same-dir conflict holder is the
// SAME process generation as the requester, making the register an idempotent
// re-register (D6a) rather than a genuine second `prox up`. It requires an exact
// PID match AND matching NON-ZERO start tokens: a zero token on either side means
// the generation cannot be distinguished from a reused PID, so it is treated as a
// genuine conflict (a hard 409) — deliberately stricter than DeregisterIfIdentity,
// which lets a zero token match for the crash-recovery teardown guard, because
// here we would otherwise silently REPLACE a possibly-different live holder.
func sameRegistrationIdentity(holderPID int, holderToken int64, reqPID int, reqToken int64) bool {
	if holderToken == 0 || reqToken == 0 {
		return false
	}
	return holderPID == reqPID && holderToken == reqToken
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
	// StartTime is the dead generation's opaque process start token (see
	// daemon.ProcessStartTime), carried through to the removal guard so a
	// restart that reused the crashed PID is not torn down. 0 means the holder
	// could not read it, so the guard degrades to bare-PID.
	StartTime int64
}

// StalePIDs returns the registered projects whose owning process generation is
// no longer running. Liveness is keyed on (PID, start token) so a reused PID
// naming a different process reads as dead (see daemon.IsProcessAlive). It only
// detects — removal goes through the consolidated removeStaleProject path
// (identity-guarded via DeregisterIfIdentity) so the crash path purges captured
// records and body files the same way an explicit deregister does, without
// racing a concurrent re-registration.
//
// The candidate set is snapshotted under RLock and the lock is RELEASED before
// any liveness check runs: daemon.IsProcessAlive does OS reads (procfs /
// sysctl) that must not block registry writers.
func (r *Registry) StalePIDs() []StaleProject {
	type candidate struct {
		dir   string
		pid   int
		token int64
	}
	r.mu.RLock()
	candidates := make([]candidate, 0, len(r.projects))
	for dir, proj := range r.projects {
		candidates = append(candidates, candidate{dir: dir, pid: proj.PID, token: proj.StartTime})
	}
	r.mu.RUnlock()

	var stale []StaleProject
	for _, c := range candidates {
		if !daemon.IsProcessAlive(c.pid, c.token) {
			stale = append(stale, StaleProject{Dir: c.dir, PID: c.pid, StartTime: c.token})
		}
	}
	return stale
}
