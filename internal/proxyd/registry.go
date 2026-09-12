package proxyd

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/constants"
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
	// Origin is the publishing machine for a hub-registered route and empty for
	// a local one (plan 031 D5). It is the data plane's "this target lives
	// through a tunnel, not on this host" flag and the reason the on-502
	// dead-owner probe skips the route: a remote PID means nothing here (P4).
	//
	// Origin is the ONLY hub field on Route, deliberately (P5): Registry.Lookup
	// hands the data plane a *Route it reads OUTSIDE the registry lock, so
	// mutable connection state must never live here. It belongs to
	// ProjectRegistration and (from C4) the session manager.
	Origin string
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
	// DiskBudget is the project's configured capture disk budget in bytes (#69);
	// 0 means the daemon default. NOT stamped per-route (the hot path never
	// consults it) — the daemon folds every capture-enabled project's budget into
	// one effective daemon-wide bound via EffectiveCaptureDiskBudget.
	DiskBudget int64
	// Origin is the publishing machine for a hub registration, empty for a local
	// one (plan 031 D5). Its presence is what makes this registration REMOTE,
	// which has two consequences on the hub host: the stale-PID sweep skips it
	// (a publisher's PID is meaningless here — P4) and Dir is the composed key
	// "<origin>:<publisher dir>" rather than a bare directory.
	Origin string

	// --- hub lease state (plan 031 D17), meaningful only when Origin != "" ---
	//
	// This is where a remote registration's connection state LIVES, together
	// with the session manager. It is deliberately NOT on Route: the data plane
	// reads Registry.Lookup's *Route outside the registry lock, so a mutable
	// field there would be a data race (P5).

	// SessionGen is the generation of the tunnel session this registration is
	// currently associated with, 0 before the first attach. It is the identity
	// half of every lease decision: a removal or a disconnect that names an
	// older generation is stale by definition and must be ignored, which is what
	// keeps a sweep from deleting a publisher that reattached between detection
	// and removal (P2).
	SessionGen uint64
	// ConnectedAt is when the current tunnel attached; zero when none ever has.
	ConnectedAt time.Time
	// DisconnectedAt is when the current tunnel closed; zero while connected.
	// A registration is CONNECTED exactly when ConnectedAt is set and this is
	// not — see ProjectRegistration.connected.
	DisconnectedAt time.Time
	// ReservedUntil is register time + HubAttachGrace for a freshly accepted
	// remote registration (D17/P3). Until it expires the registration counts as
	// connected for collision purposes even though no tunnel has attached yet,
	// closing the register→attach window in which a second publisher could
	// otherwise take the name out from under one still completing its handshake.
	ReservedUntil time.Time
}

// connected reports whether this registration currently holds a tunnel.
func (p *ProjectRegistration) connected() bool {
	return !p.ConnectedAt.IsZero() && p.DisconnectedAt.IsZero()
}

// active reports whether this registration holds a name against a newcomer
// (plan 031 D10): a LOCAL registration always does, and a remote one does while
// it is connected or still inside its attach reservation.
func (p *ProjectRegistration) active(now time.Time) bool {
	if p.Origin == "" {
		return true
	}
	return p.connected() || now.Before(p.ReservedUntil)
}

// hubLease is a remote registration's lease state, copied out under the registry
// lock so a caller can reason about it without holding one.
type hubLease struct {
	SessionGen     uint64
	ConnectedAt    time.Time
	DisconnectedAt time.Time
	ReservedUntil  time.Time
}

func leaseOf(p *ProjectRegistration) hubLease {
	return hubLease{
		SessionGen:     p.SessionGen,
		ConnectedAt:    p.ConnectedAt,
		DisconnectedAt: p.DisconnectedAt,
		ReservedUntil:  p.ReservedUntil,
	}
}

func applyLease(p *ProjectRegistration, l hubLease) {
	p.SessionGen = l.SessionGen
	p.ConnectedAt = l.ConnectedAt
	p.DisconnectedAt = l.DisconnectedAt
	p.ReservedUntil = l.ReservedUntil
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
	// now is the clock every hub LEASE decision reads (plan 031 D17): the
	// attach reservation, the disconnect grace, and the collision "is this
	// holder active" test. Injectable so the grace-expiry tests drive it
	// deterministically instead of sleeping, exactly as DynamicProxy's
	// probeClock does for the on-502 probe.
	now func() time.Time
}

// NewRegistry creates a new empty route registry.
func NewRegistry() *Registry {
	return &Registry{
		routes:    make(map[string]*Route),
		projects:  make(map[string]*ProjectRegistration),
		listeners: make(map[int]*ListenerInfo),
		now:       time.Now,
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

// HubNameHeldError is returned when a remote registration collides with service
// names an ACTIVE holder owns (plan 031 D10), analogous to
// ProjectConflictError for the same-dir case. It carries EVERY conflicting
// holder, because registration is all-or-nothing: a publisher whose two
// services are held by two different publishers takes both names or neither,
// and the user deciding whether to force it needs to see both.
//
// It is matched with errors.As; the register handler turns it into a
// 409 HUB_NAME_HELD whose body carries the holder list.
type HubNameHeldError struct {
	Holders []HubHolder
}

func (e *HubNameHeldError) Error() string {
	parts := make([]string, 0, len(e.Holders))
	for _, h := range e.Holders {
		who := h.Origin
		if who == "" {
			who = "this machine"
		}
		state := "disconnected"
		if h.Connected {
			state = "connected"
		}
		parts = append(parts, fmt.Sprintf("%s held by %s:%s (%s)", h.Hostname, who, h.ProjectDir, state))
	}
	return fmt.Sprintf(
		"service name(s) already published on this hub: %s. Retry with --hub-takeover to take them",
		strings.Join(parts, ", "),
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
	now := r.now()
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
			Origin:         req.Origin,
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

	proj := &ProjectRegistration{
		Dir:            req.ProjectDir,
		PID:            req.PID,
		Domain:         req.Domain,
		RouteKeys:      routeKeys,
		StartTime:      req.StartTime,
		RegisteredAt:   now,
		CaptureEnabled: req.CaptureEnabled,
		MaxBodySize:    req.MaxBodySize,
		DiskBudget:     req.DiskBudget,
		Origin:         req.Origin,
	}
	if req.Origin != "" {
		// A freshly accepted remote registration is RESERVED: it counts as
		// connected for collision purposes until its tunnel has had
		// HubAttachGrace to attach (plan 031 D17/P3). Without this, the window
		// between register and attach is one in which nothing is connected, so
		// D10's "inactive is replaceable" rule would let a second publisher take
		// the name from one that is mid-handshake.
		proj.ReservedUntil = now.Add(constants.HubAttachGrace)
	}
	r.projects[req.ProjectDir] = proj

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

// --- hub lease (plan 031 D3/D17) ---

// decideLease is the PURE lease-expiry decision, with every input passed in so
// it can be table-tested against an injected clock — the same shape decideProbe
// takes for the on-502 dead-owner gate.
//
// A remote registration may be removed only when ALL of the following hold:
//
//   - it is disconnected (disconnectedAt is set). A connected publisher is
//     serving traffic; nothing about a sweep should touch it.
//   - its attach reservation has expired. A registration inside HubAttachGrace
//     has not had a fair chance to attach yet (P3).
//   - the grace has elapsed since the disconnect. Below it the hub serves the
//     offline page and a reattach costs no route churn (D3).
//   - the generation the caller OBSERVED is still the current one. This is the
//     P2 guard: a sweep that saw "disconnected past grace", then had the
//     publisher reattach before it got the lock, would otherwise delete a live
//     registration. A reattach bumps currentGen, so the stale decision falls
//     through here instead.
func decideLease(now, disconnectedAt, reservedUntil time.Time, sessionGen, currentGen uint64, grace time.Duration) bool {
	if disconnectedAt.IsZero() {
		return false
	}
	if sessionGen != currentGen {
		return false
	}
	if now.Before(reservedUntil) {
		return false
	}
	return !now.Before(disconnectedAt.Add(grace))
}

// MarkConnected records that a tunnel of generation gen attached for key. It
// reports whether a remote registration was found and updated.
//
// An OLDER generation is refused. Generations are allocated monotonically by
// the one session manager, so a lower number can only come from a session that
// has already been replaced — applying it would roll the registration back onto
// a dead tunnel.
func (r *Registry) MarkConnected(key string, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	proj, ok := r.projects[key]
	if !ok || proj.Origin == "" || gen < proj.SessionGen {
		return false
	}
	proj.SessionGen = gen
	proj.ConnectedAt = r.now()
	proj.DisconnectedAt = time.Time{}
	// The attach reservation has done its job; the tunnel itself now holds the
	// name.
	proj.ReservedUntil = time.Time{}
	return true
}

// MarkDisconnected records that the tunnel of generation gen closed, starting
// the disconnect grace. It is a NO-OP unless gen is still the registration's
// current generation — the second half of the P2 gate, mirroring the session
// manager's own generation check, so a replaced session's late close callback
// can never disconnect its successor.
func (r *Registry) MarkDisconnected(key string, gen uint64, at time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	proj, ok := r.projects[key]
	if !ok || proj.Origin == "" || proj.SessionGen != gen || !proj.DisconnectedAt.IsZero() {
		return false
	}
	proj.DisconnectedAt = at
	return true
}

// RemoteLease returns key's lease state, or ok=false when key is not a remote
// registration. The data plane uses it to render "offline since <t>".
func (r *Registry) RemoteLease(key string) (hubLease, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	proj, ok := r.projects[key]
	if !ok || proj.Origin == "" {
		return hubLease{}, false
	}
	return leaseOf(proj), true
}

// setLease restores lease state onto key's registration. It exists for ONE
// caller: server.register's remote config-changed arm, which removes and
// re-adds the registration and must not silently drop a live tunnel's
// generation on the way through. Without it a publisher that re-registers with
// a changed service map would look disconnected while its tunnel is still
// attached, and that tunnel's eventual close callback — carrying a generation
// the registration no longer knows — would be ignored forever.
func (r *Registry) setLease(key string, l hubLease) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	proj, ok := r.projects[key]
	if !ok || proj.Origin == "" {
		return false
	}
	applyLease(proj, l)
	return true
}

// LeaseCandidate is one remote registration whose disconnect grace has expired,
// paired with the generation that was current when the sweep observed it.
type LeaseCandidate struct {
	Key        string
	SessionGen uint64
}

// ExpiredLeases returns the remote registrations the sweep should remove. The
// answer is advisory by design: it is computed under a read lock and acted on
// later, so DeregisterIfDisconnected re-runs the same decision under the write
// lock before removing anything (P2).
func (r *Registry) ExpiredLeases() []LeaseCandidate {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.now()
	var expired []LeaseCandidate
	for key, proj := range r.projects {
		if proj.Origin == "" {
			continue
		}
		if decideLease(now, proj.DisconnectedAt, proj.ReservedUntil, proj.SessionGen, proj.SessionGen, constants.HubDisconnectGrace) {
			expired = append(expired, LeaseCandidate{Key: key, SessionGen: proj.SessionGen})
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].Key < expired[j].Key })
	return expired
}

// DeregisterIfDisconnected removes a remote registration only if it is STILL
// disconnected past its grace AND its SessionGen is unchanged since the caller
// observed it (plan 031 D17/P2).
//
// This is deliberately NOT removeStaleProject: that path's identity guard is
// (dir, PID, start token), every element of which SURVIVES a tunnel reattach, so
// a sweep that observed an expired lease would happily delete a publisher that
// reconnected in between. The session generation is the only identity that
// changes on reattach, which is why it — and not the PID — is the guard here.
func (r *Registry) DeregisterIfDisconnected(key string, sessionGen uint64) (removed bool, removedHostnames []string, emptyPorts []int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proj, ok := r.projects[key]
	if !ok || proj.Origin == "" {
		return false, nil, nil
	}
	if !decideLease(r.now(), proj.DisconnectedAt, proj.ReservedUntil, sessionGen, proj.SessionGen, constants.HubDisconnectGrace) {
		return false, nil, nil
	}
	removedHostnames, emptyPorts = r.deregisterLocked(key)
	return true, removedHostnames, emptyPorts
}

// HasProject reports whether key names a registered project. The tunnel upgrade
// handler uses it to answer 404 NOT_REGISTERED before hijacking a connection it
// would only have to drop.
func (r *Registry) HasProject(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.projects[key]
	return ok
}

// --- hub name collisions (plan 031 D10) ---

// hubNameCollision is one existing route that stands between a remote
// registration and a service name it wants.
type hubNameCollision struct {
	// HolderKey is the registry key of the project that holds the name, used to
	// remove the whole losing registration when it loses.
	HolderKey string
	// Holder is the wire-facing identity the 409 body reports.
	Holder HubHolder
	// Local marks a socket-registered holder, which is NEVER displaced by a
	// remote registration (D10).
	Local bool
	// Active marks a holder that is connected or still inside its attach
	// reservation — the holders that need consent (takeover) to displace.
	Active bool
}

// HubNameCollisions reports every existing route that blocks req, classified so
// the caller can apply D10 without re-deriving anything under its own lock.
//
// The desired route set mirrors Register's pending-route construction exactly;
// keeping the two expressions adjacent is what makes "the collision check and
// the registration agree about which names are wanted" checkable.
func (r *Registry) HubNameCollisions(req RegisterRequest) []hubNameCollision {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.now()
	seen := make(map[string]struct{})
	var collisions []hubNameCollision

	consider := func(hostname string, port int) {
		route, ok := r.routes[routeKey(hostname, port)]
		if !ok || route.ProjectDir == req.ProjectDir {
			return
		}
		if _, dup := seen[route.ProjectDir+"|"+hostname]; dup {
			return
		}
		seen[route.ProjectDir+"|"+hostname] = struct{}{}

		holder := HubHolder{Hostname: hostname, ProjectDir: route.ProjectDir}
		local := true
		active := true
		if proj, ok := r.projects[route.ProjectDir]; ok {
			local = proj.Origin == ""
			active = proj.active(now)
			holder.Origin = proj.Origin
			holder.Connected = proj.Origin == "" || proj.connected()
			if proj.Origin != "" {
				// Report the publisher's OWN directory, not the composed key:
				// the key is a hub implementation detail, and the person reading
				// the conflict wants a path they recognize.
				holder.ProjectDir = hubKeyProjectDir(proj.Origin, route.ProjectDir)
			}
		}
		collisions = append(collisions, hubNameCollision{
			HolderKey: route.ProjectDir,
			Holder:    holder,
			Local:     local,
			Active:    active,
		})
	}

	for svcName := range req.Services {
		hostname := fmt.Sprintf("%s.%s", svcName, req.Domain)
		if req.HTTPSPort > 0 {
			consider(hostname, req.HTTPSPort)
		}
		if req.HTTPPort > 0 {
			consider(hostname, req.HTTPPort)
		}
	}
	sort.Slice(collisions, func(i, j int) bool {
		if collisions[i].Holder.Hostname != collisions[j].Holder.Hostname {
			return collisions[i].Holder.Hostname < collisions[j].Holder.Hostname
		}
		return collisions[i].HolderKey < collisions[j].HolderKey
	})
	return collisions
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
	// A changed disk budget must force a real re-register so syncCaptureBudget
	// recomputes the effective daemon-wide bound (#69) — a no-op refresh would
	// leave the old budget in force.
	if proj.DiskBudget != req.DiskBudget {
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
	// RouteKeys is the only slice on either struct, so the plain struct copies
	// here and below are complete deep copies once it is explicitly copied.
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
		// A local route is always serveable (the daemon dials the target
		// itself); a hub route needs its publisher's tunnel, whose state the
		// owning registration mirrors from the session manager (plan 031 D17).
		connected := true
		if route.Origin != "" {
			proj, ok := r.projects[route.ProjectDir]
			connected = ok && proj.connected()
		}
		routes = append(routes, RouteInfo{
			Hostname:     route.Hostname,
			Port:         route.Port,
			Protocol:     route.Protocol,
			Target:       route.Target,
			ProjectDir:   route.ProjectDir,
			PID:          route.PID,
			RegisteredAt: route.RegisteredAt,
			Origin:       route.Origin,
			Connected:    connected,
		})
	}
	return routes
}

// RemotePublishers returns one entry per HUB (origin-qualified) registration,
// for the hub operator's `prox hub status` (plan 031 §4.2). Local registrations
// are never listed: this is the publisher roster, not the route table.
func (r *Registry) RemotePublishers() []HubPublisher {
	r.mu.RLock()
	defer r.mu.RUnlock()

	publishers := make([]HubPublisher, 0)
	for key, proj := range r.projects {
		if proj.Origin == "" {
			continue
		}
		publishers = append(publishers, HubPublisher{
			Origin:         proj.Origin,
			ProjectDir:     hubKeyProjectDir(proj.Origin, key),
			Key:            key,
			Connected:      proj.connected(),
			ConnectedAt:    proj.ConnectedAt,
			DisconnectedAt: proj.DisconnectedAt,
			RegisteredAt:   proj.RegisteredAt,
			Routes:         len(proj.RouteKeys),
		})
	}
	sort.Slice(publishers, func(i, j int) bool { return publishers[i].Key < publishers[j].Key })
	return publishers
}

// RemoteProjectKeys returns the registry keys of every hub registration, so
// `prox hub stop` can remove them all when the network control plane closes
// (plan 031 §4.2). Sorted for deterministic teardown order.
func (r *Registry) RemoteProjectKeys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0)
	for key, proj := range r.projects {
		if proj.Origin != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
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

// EffectiveCaptureDiskBudget computes the daemon-wide capture disk budget for the
// one shared capture dir (#69): the MINIMUM over all registered capture-ENABLED
// projects of that project's configured budget if set, else
// DefaultCaptureDiskBudget for a project that left it unset. When no
// capture-ENABLED project is registered, the default applies. Capture-DISABLED
// projects never influence the bound.
//
// There is NO per-value clamp: raising the bound above the default IS allowed
// when EVERY capture-enabled project opts in — including the single-project case
// ({A: 2GB} alone -> 2GB). But an explicit value can never raise ANOTHER
// project's default: an unset capture-enabled project contributes the default to
// the min, so {A: 2GB, B: unset} -> min(2GB, 1GiB) = 1GiB.
func (r *Registry) EffectiveCaptureDiskBudget() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	effective := int64(0)
	seen := false
	for _, proj := range r.projects {
		if !proj.CaptureEnabled {
			continue
		}
		budget := proj.DiskBudget
		if budget <= 0 {
			budget = constants.DefaultCaptureDiskBudget
		}
		if !seen || budget < effective {
			effective = budget
			seen = true
		}
	}
	if !seen {
		return constants.DefaultCaptureDiskBudget
	}
	return effective
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
// no longer running. REMOTE (hub) registrations are skipped entirely: a
// publisher's PID identifies a process on ANOTHER machine, so probing it here
// is meaningless in both directions — it may be dead on the hub host while the
// publisher is perfectly healthy, or (worse) coincidentally alive as some
// unrelated local process, making a genuinely gone publisher look live (plan
// 031 P4/D3). A remote registration's liveness is its tunnel, swept on the
// disconnect grace in C4.
//
// Liveness is keyed on (PID, start token) so a reused PID
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
		if proj.Origin != "" {
			// Remote registration: its PID lives on another machine (P4).
			continue
		}
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
