package proxyd

import (
	"errors"
	"strings"
	"testing"
)

func newTestRequest(projectDir, domain string, services map[string]ServiceTarget, httpPort, httpsPort int) RegisterRequest {
	return RegisterRequest{
		ProjectDir: projectDir,
		PID:        12345,
		Version:    "dev",
		Domain:     domain,
		Services:   services,
		HTTPPort:   httpPort,
		HTTPSPort:  httpsPort,
	}
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)

	hostnames, newPorts, err := reg.Register(req)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if len(hostnames) != 1 || hostnames[0] != "api.local.dev" {
		t.Errorf("hostnames = %v, want [api.local.dev]", hostnames)
	}
	if len(newPorts) != 1 || newPorts[0].Port != 443 || newPorts[0].Protocol != "https" {
		t.Errorf("newPorts = %v, want [{443 https}]", newPorts)
	}

	route, ok := reg.Lookup("api.local.dev", 443)
	if !ok {
		t.Fatal("Lookup returned false for registered route")
	}
	if route.Target.Port != 3000 {
		t.Errorf("route target port = %d, want 3000", route.Target.Port)
	}
	if route.Protocol != "https" {
		t.Errorf("route protocol = %q, want https", route.Protocol)
	}

	// Verify counts
	if reg.ProjectCount() != 1 {
		t.Errorf("ProjectCount = %d, want 1", reg.ProjectCount())
	}
	if reg.IsEmpty() {
		t.Error("IsEmpty should be false")
	}
}

func TestRegistry_MultipleServices(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
			"web": {Host: "localhost", Port: 3001},
		},
		80, 443,
	)

	hostnames, newPorts, err := reg.Register(req)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 2 services * 2 ports = 4 routes
	if len(hostnames) != 4 {
		t.Errorf("got %d hostnames, want 4", len(hostnames))
	}
	if len(newPorts) != 2 {
		t.Errorf("got %d new ports, want 2", len(newPorts))
	}

	// Verify all routes are lookupable
	for _, hn := range []string{"api.local.dev", "web.local.dev"} {
		for _, port := range []int{80, 443} {
			if _, ok := reg.Lookup(hn, port); !ok {
				t.Errorf("Lookup(%s, %d) returned false", hn, port)
			}
		}
	}
}

func TestRegistry_DomainConflict(t *testing.T) {
	reg := NewRegistry()

	reqA := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Same domain+port, different project
	reqB := newTestRequest("/projects/b", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}},
		0, 443,
	)
	_, _, err := reg.Register(reqB)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestRegistry_DifferentDomainsSharePort(t *testing.T) {
	reg := NewRegistry()

	reqA := newTestRequest("/projects/a", "local.alpha.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Different domain, same port — should work
	reqB := newTestRequest("/projects/b", "local.beta.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}},
		0, 443,
	)
	hostnames, newPorts, err := reg.Register(reqB)
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	if len(hostnames) != 1 || hostnames[0] != "api.local.beta.dev" {
		t.Errorf("hostnames = %v, want [api.local.beta.dev]", hostnames)
	}
	// Port 443 already has a listener, so no new ports needed
	if len(newPorts) != 0 {
		t.Errorf("newPorts = %v, want empty (port already bound)", newPorts)
	}
}

func TestRegistry_ProtocolMismatch(t *testing.T) {
	reg := NewRegistry()

	// Register HTTPS on port 443
	reqA := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Try HTTP on same port 443
	reqB := newTestRequest("/projects/b", "other.dev",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}},
		443, 0,
	)
	_, _, err := reg.Register(reqB)
	if err == nil {
		t.Fatal("expected protocol mismatch error, got nil")
	}
	t.Logf("got expected error: %v", err)
}

func TestRegistry_Deregister(t *testing.T) {
	reg := NewRegistry()

	reqA := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	removed, emptyPorts := reg.Deregister("/projects/a")
	if len(removed) != 1 || removed[0] != "api.local.dev" {
		t.Errorf("removed = %v, want [api.local.dev]", removed)
	}
	if len(emptyPorts) != 1 || emptyPorts[0] != 443 {
		t.Errorf("emptyPorts = %v, want [443]", emptyPorts)
	}
	if !reg.IsEmpty() {
		t.Error("registry should be empty after deregister")
	}

	// Lookup should fail now
	if _, ok := reg.Lookup("api.local.dev", 443); ok {
		t.Error("Lookup should return false after deregister")
	}
}

func TestRegistry_DeregisterLeavesOtherProjects(t *testing.T) {
	reg := NewRegistry()

	reqA := newTestRequest("/projects/a", "local.alpha.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	reqB := newTestRequest("/projects/b", "local.beta.dev",
		map[string]ServiceTarget{"web": {Host: "localhost", Port: 4000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqB); err != nil {
		t.Fatalf("Register B: %v", err)
	}

	// Deregister A — port 443 should not become empty
	_, emptyPorts := reg.Deregister("/projects/a")
	if len(emptyPorts) != 0 {
		t.Errorf("emptyPorts = %v, want empty (B still uses 443)", emptyPorts)
	}

	// B's route should still work
	if _, ok := reg.Lookup("web.local.beta.dev", 443); !ok {
		t.Error("B's route should still be reachable")
	}

	// A's route should be gone
	if _, ok := reg.Lookup("api.local.alpha.dev", 443); ok {
		t.Error("A's route should be gone")
	}
}

// TestRegistry_DeregisterIfIdentity pins the reused-PID teardown guard (#61):
// removal happens only when the CURRENT registration matches BOTH the pid and
// the start token, so a restart that reused a crashed PID under a new token is
// never torn down.
func TestRegistry_DeregisterIfIdentity(t *testing.T) {
	t.Run("pid and token match removes", func(t *testing.T) {
		reg := NewRegistry()
		req := newTestRequest("/projects/a", "local.dev",
			map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
		req.PID = 111
		req.StartTime = 424242
		if _, _, err := reg.Register(req); err != nil {
			t.Fatalf("Register: %v", err)
		}

		removed, hostnames, emptyPorts := reg.DeregisterIfIdentity("/projects/a", 111, 424242)
		if !removed {
			t.Fatal("removed = false, want true when pid and token match")
		}
		if len(hostnames) != 1 || hostnames[0] != "api.local.dev" {
			t.Errorf("hostnames = %v, want [api.local.dev]", hostnames)
		}
		if len(emptyPorts) != 1 || emptyPorts[0] != 443 {
			t.Errorf("emptyPorts = %v, want [443]", emptyPorts)
		}
		if !reg.IsEmpty() {
			t.Error("registry should be empty after a matching identity removal")
		}
	})

	t.Run("token mismatch leaves registration intact", func(t *testing.T) {
		reg := NewRegistry()
		req := newTestRequest("/projects/a", "local.dev",
			map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
		req.PID = 111
		req.StartTime = 424242
		if _, _, err := reg.Register(req); err != nil {
			t.Fatalf("Register: %v", err)
		}

		// Same PID reused by a restart under a different token — must not remove.
		removed, hostnames, emptyPorts := reg.DeregisterIfIdentity("/projects/a", 111, 424243)
		if removed {
			t.Fatal("removed = true, want false when the start token differs")
		}
		if hostnames != nil || emptyPorts != nil {
			t.Errorf("expected nil results on skip, got hostnames=%v emptyPorts=%v", hostnames, emptyPorts)
		}
		if _, ok := reg.Lookup("api.local.dev", 443); !ok {
			t.Error("registration must survive a token mismatch")
		}
	})

	t.Run("missing project is a no-op", func(t *testing.T) {
		reg := NewRegistry()
		removed, hostnames, emptyPorts := reg.DeregisterIfIdentity("/projects/missing", 111, 424242)
		if removed {
			t.Fatal("removed = true, want false for a missing project")
		}
		if hostnames != nil || emptyPorts != nil {
			t.Errorf("expected nil results, got hostnames=%v emptyPorts=%v", hostnames, emptyPorts)
		}
	})
}

func TestRegistry_DuplicateProject(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	req.StartTime = 987654321 // opaque start token; the conflict must echo it back
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Try to register the same project again — a typed conflict carrying the
	// existing registration's dir, PID, and start token.
	_, _, err := reg.Register(req)
	if err == nil {
		t.Fatal("expected duplicate project error, got nil")
	}
	var conflict *ProjectConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ProjectConflictError", err)
	}
	if conflict.Dir != "/projects/a" {
		t.Errorf("conflict.Dir = %q, want /projects/a", conflict.Dir)
	}
	if conflict.PID != req.PID {
		t.Errorf("conflict.PID = %d, want %d", conflict.PID, req.PID)
	}
	if conflict.StartTime != req.StartTime {
		t.Errorf("conflict.StartTime = %d, want %d", conflict.StartTime, req.StartTime)
	}
	if !strings.Contains(err.Error(), "already registered by a running prox up") ||
		!strings.Contains(err.Error(), "prox proxy stop --force") {
		t.Errorf("error message = %q, want the upgraded holder-naming message", err.Error())
	}
}

// TestRegistry_ConflictCarriesNewPIDAfterReRegister pins that the typed
// conflict reflects the CURRENT registration: after a project deregisters and
// re-registers under a different PID, the conflict names the new PID, not the
// original one.
func TestRegistry_ConflictCarriesNewPIDAfterReRegister(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	req.PID = 111
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reg.Deregister("/projects/a")
	req.PID = 222
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("re-Register: %v", err)
	}

	_, _, err := reg.Register(req)
	var conflict *ProjectConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ProjectConflictError", err)
	}
	if conflict.PID != 222 {
		t.Errorf("conflict.PID = %d, want 222 (the current registration's PID)", conflict.PID)
	}
}

// TestRegistry_RouteConflictIsUntyped pins that a non-same-dir conflict (another
// project owning the hostname:port) stays a plain error — it must never be
// mistaken for a replaceable stale registration.
func TestRegistry_RouteConflictIsUntyped(t *testing.T) {
	reg := NewRegistry()

	reqA := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	reqB := newTestRequest("/projects/b", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}},
		0, 443,
	)
	_, _, err := reg.Register(reqB)
	if err == nil {
		t.Fatal("expected route conflict error, got nil")
	}
	var conflict *ProjectConflictError
	if errors.As(err, &conflict) {
		t.Errorf("route conflict should be untyped, got *ProjectConflictError: %v", err)
	}
}

func TestRegistry_AllRoutes(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{
			"api": {Host: "localhost", Port: 3000},
			"web": {Host: "localhost", Port: 3001},
		},
		0, 443,
	)
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	routes := reg.AllRoutes()
	if len(routes) != 2 {
		t.Errorf("got %d routes, want 2", len(routes))
	}
}

func TestRegistry_ListenerPorts(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		80, 443,
	)
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ports := reg.ListenerPorts()
	if len(ports) != 2 {
		t.Errorf("got %d ports, want 2", len(ports))
	}
	// Should be sorted
	if ports[0] != 80 || ports[1] != 443 {
		t.Errorf("ports = %v, want [80, 443]", ports)
	}
}

func TestRegistry_StampsCaptureEnabled(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		80, 443,
	)
	req.CaptureEnabled = true
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Every route the project registered carries the capture flag.
	for _, port := range []int{80, 443} {
		route, ok := reg.Lookup("api.local.dev", port)
		if !ok {
			t.Fatalf("Lookup(api.local.dev, %d) returned false", port)
		}
		if !route.CaptureEnabled {
			t.Errorf("route on port %d: CaptureEnabled = false, want true", port)
		}
	}
}

func TestRegistry_CaptureDisabledByDefault(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	// CaptureEnabled left false.
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	route, ok := reg.Lookup("api.local.dev", 443)
	if !ok {
		t.Fatal("Lookup returned false")
	}
	if route.CaptureEnabled {
		t.Error("route CaptureEnabled = true, want false")
	}
}

func TestRegistry_DifferentPorts(t *testing.T) {
	reg := NewRegistry()

	// Project A on port 443
	reqA := newTestRequest("/projects/a", "local.alpha.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(reqA); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Project B on port 8888
	reqB := newTestRequest("/projects/b", "local.beta.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 4000}},
		0, 8888,
	)
	_, newPorts, err := reg.Register(reqB)
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	if len(newPorts) != 1 || newPorts[0].Port != 8888 {
		t.Errorf("newPorts = %v, want [{8888 https}]", newPorts)
	}

	ports := reg.ListenerPorts()
	if len(ports) != 2 {
		t.Errorf("got %d ports, want 2", len(ports))
	}
}

// TestRegistrationMatches_Capture pins the no-op-refresh discriminator for the
// capture fields: an identical capture config is a no-op refresh, while a
// changed capture_enabled, max_body_size (D13), or disk_budget (#69) each force
// a real re-register so the new value reaches the routes / the daemon-wide
// budget.
func TestRegistrationMatches_Capture(t *testing.T) {
	reg := NewRegistry()
	base := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	base.CaptureEnabled = true
	base.MaxBodySize = 1024
	base.DiskBudget = 4096
	if _, _, err := reg.Register(base); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RegisterRequest)
		want   bool
	}{
		{
			name:   "identical capture config is a no-op refresh",
			mutate: func(*RegisterRequest) {},
			want:   true,
		},
		{
			name:   "changed capture_enabled forces re-register",
			mutate: func(r *RegisterRequest) { r.CaptureEnabled = false },
			want:   false,
		},
		{
			name:   "changed max_body_size forces re-register",
			mutate: func(r *RegisterRequest) { r.MaxBodySize = 2048 },
			want:   false,
		},
		{
			name:   "changed disk_budget forces re-register",
			mutate: func(r *RegisterRequest) { r.DiskBudget = 8192 },
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if got := reg.registrationMatches(req); got != tt.want {
				t.Errorf("registrationMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}
