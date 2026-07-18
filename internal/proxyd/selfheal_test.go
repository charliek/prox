package proxyd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProxyServer builds a Server wired to a real DynamicProxy with real TCP
// listeners (HTTP only, no certMgr) — enough to prove the self-heal actually
// closes and rebinds a physical listener, which the registry-only scaffold
// cannot.
func newProxyServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	s := NewServer(ServerConfig{SocketPath: "", Logger: logger, Version: "test"})
	reg := NewRegistry()
	rm := proxy.NewRequestManager(100)
	dp := NewDynamicProxy(reg, nil, rm, nil, logger)
	s.SetRegistry(reg)
	s.SetProxy(dp)
	s.SetRequestManager(rm)
	t.Cleanup(func() { _ = dp.Shutdown(context.Background()) })
	return s
}

// freePort binds an ephemeral port, closes it, and returns the number for a
// registration to rebind. A small race is accepted (tests only).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

// registerOK drives a setup registration to success, tolerating the transient
// PORT_BIND_FAILED that a fresh-port bind can hit when the OS briefly holds a
// just-closed ephemeral port (the small port race the plan sanctions for
// tests). The production self-heal replace path has its own bind retry; this is
// only for test scaffolding that rebinds ports outside that path.
func registerOK(t *testing.T, s *Server, req RegisterRequest) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, body := s.register(req)
		if status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("register never succeeded: status=%d body=%v", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSelfHeal_E2E_DeadPIDRebindsAndServes is the pinned real-listener e2e: a
// dead-PID registration binds a port, a same-dir restart under a live PID
// self-heals (closes and rebinds the listener), and a proxied HTTP request
// round-trips through the new registration.
func TestSelfHeal_E2E_DeadPIDRebindsAndServes(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from backend")
	})

	s := newProxyServer(t)
	port := freePort(t)

	deadReq := RegisterRequest{
		ProjectDir: "/projects/dead", PID: deadPID(t), Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}
	registerOK(t, s, deadReq)

	// Restart: same dir + port, live PID. Self-heal closes and rebinds the port.
	reReq := deadReq
	reReq.PID = os.Getpid()
	status, body := s.register(reReq)
	require.Equal(t, http.StatusOK, status, "self-heal re-register should succeed: %v", body)

	// A proxied request round-trips through the new registration. The rebind has
	// a brief unbound window, so retry briefly.
	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		httpReq, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		httpReq.Host = "api.local.dev"
		resp, err = http.DefaultClient.Do(httpReq)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, err, "proxied request should reach the rebound listener")
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello from backend", string(respBody))
}

// TestSelfHeal_Concurrency_RegisterVsSweep runs registration attempts racing the
// sweep's removeStaleProject on the same dead dir under -race. The lifecycle
// mutex must make every outcome either a self-heal with a routable listener or
// a clean conflict — never PORT_BIND_FAILED, never a purged new generation.
func TestSelfHeal_Concurrency_RegisterVsSweep(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	s := newProxyServer(t)
	port := freePort(t)
	dead := deadPID(t)

	baseReq := RegisterRequest{
		ProjectDir: "/projects/dead", Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: port,
	}

	const iterations = 40
	successes := 0
	for i := 0; i < iterations; i++ {
		// Clean slate, then seed a fresh dead registration binding the port.
		s.removeProject("/projects/dead")
		deadReq := baseReq
		deadReq.PID = dead
		registerOK(t, s, deadReq)

		var wg sync.WaitGroup
		var regStatus int
		wg.Add(2)
		go func() {
			defer wg.Done()
			reReq := baseReq
			reReq.PID = os.Getpid()
			regStatus, _ = s.register(reReq)
		}()
		go func() {
			defer wg.Done()
			s.removeStaleProject("/projects/dead", dead)
		}()
		wg.Wait()

		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, regStatus,
			"iteration %d: only self-heal success or clean conflict is acceptable (got %d)", i, regStatus)

		if regStatus == http.StatusOK {
			successes++
			_, ok := s.registry.Lookup("api.local.dev", port)
			assert.True(t, ok, "iteration %d: successful self-heal must leave a routable listener", i)

			// The route must be backed by a real listening socket, not just
			// registry bookkeeping.
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			require.NoError(t, err, "iteration %d: self-heal listener must accept connections", i)
			_ = conn.Close()

			// A late sweep tick for the dead PID must not touch the live
			// generation's route or records.
			recID := fmt.Sprintf("newgen-%d", i)
			s.requestManager.Record(proxy.RequestRecord{ID: recID, ProjectDir: "/projects/dead", Method: "GET", URL: "/n", Details: &proxy.RequestDetails{}})
			removed, _, _ := s.removeStaleProject("/projects/dead", dead)
			assert.False(t, removed, "iteration %d: late sweep must skip the live generation", i)
			_, ok = s.registry.Lookup("api.local.dev", port)
			assert.True(t, ok, "iteration %d: live route must survive a late sweep", i)
			_, ok = s.requestManager.GetByID(recID)
			assert.True(t, ok, "iteration %d: new generation's record must survive a late sweep", i)
		}
	}
	// The race must not be able to starve the restart entirely — the whole
	// point of #55 is that the re-register wins against a dead holder.
	require.Positive(t, successes, "at least one self-heal must succeed across %d iterations", iterations)
}

// TestSelfHeal_RollbackBindFailSchedulesShutdown pins that a self-heal replace
// whose listener rebind fails rolls back to an empty registry AND schedules the
// graced shutdown check — an emptied registry must never strand an idle daemon.
func TestSelfHeal_RollbackBindFailSchedulesShutdown(t *testing.T) {
	backendHost, backendPort := newTestBackend(t, func(http.ResponseWriter, *http.Request) {})
	s := newProxyServer(t)
	s.shutdownDelay = 20 * time.Millisecond

	// Occupy a port so the self-heal's rebind onto it fails. Bind all interfaces
	// (":0") so the daemon's ":port" bind is guaranteed to collide.
	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	deadReq := RegisterRequest{
		ProjectDir: "/projects/dead", PID: deadPID(t), Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: freePort(t),
	}
	registerOK(t, s, deadReq)

	// Restart onto the occupied port: self-heal replaces the dead registration,
	// then the rebind fails, rolling back to an empty registry.
	reReq := deadReq
	reReq.PID = os.Getpid()
	reReq.HTTPPort = occupiedPort
	status, body := s.register(reReq)
	require.Equal(t, http.StatusInternalServerError, status, "bind failure expected: %v", body)
	require.True(t, s.registry.IsEmpty(), "rollback must empty the registry")

	select {
	case <-s.ShutdownCh():
		// graced shutdown scheduled — correct
	case <-time.After(2 * time.Second):
		t.Fatal("a rollback that emptied the registry must schedule a shutdown check")
	}
}
