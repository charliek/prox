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

func TestRegistry_DuplicateProject(t *testing.T) {
	reg := NewRegistry()

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}},
		0, 443,
	)
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Try to register the same project again — a typed conflict carrying the
	// existing registration's dir and PID.
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
