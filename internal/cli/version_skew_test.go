package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastSkewOps returns a skewOps with instant sleeps and real-but-unused clock so
// tests never pay wall-clock waits. Individual fields are overridden per test.
func fastSkewOps() skewOps {
	return skewOps{
		sleep:        func(time.Duration) {},
		now:          time.Now,
		drainTimeout: 5 * time.Second,
		drainPoll:    time.Millisecond,
		retryDelay:   time.Millisecond,
	}
}

func route(dir string) proxyd.RouteInfo { return proxyd.RouteInfo{ProjectDir: dir} }

// TestRecoverFromVersionSkew_BusyLists pins the busy-daemon fatal path: a daemon
// holding projects yields a fatal error (no client) naming both versions, the
// deduped project dirs, and the remediation commands.
func TestRecoverFromVersionSkew_BusyLists(t *testing.T) {
	ops := fastSkewOps()
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		return &proxyd.DaemonStatusResponse{
			ProjectCount: 2,
			// Two routes for /a (deduped to one) plus /b.
			Routes: []proxyd.RouteInfo{route("/projects/a"), route("/projects/a"), route("/projects/b")},
		}, nil
	}
	ops.shutdown = func() error { t.Fatal("must not shut down a busy daemon"); return nil }

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.Error(t, err)
	assert.Nil(t, client)

	msg := err.Error()
	assert.Contains(t, msg, "0.1.2")
	assert.Contains(t, msg, "0.2.0")
	assert.Contains(t, msg, "/projects/a")
	assert.Contains(t, msg, "/projects/b")
	assert.Equal(t, 1, strings.Count(msg, "/projects/a\n"), "duplicate route dir must be listed once")
	assert.Contains(t, msg, "prox proxy stop --force")
	assert.Contains(t, msg, "prox up")
}

// TestRecoverFromVersionSkew_IdleHealSuccess pins the happy heal path: an idle
// daemon is shut down, drains, and a fresh daemon starts — recover returns that
// client with no error.
func TestRecoverFromVersionSkew_IdleHealSuccess(t *testing.T) {
	ops := fastSkewOps()
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		return &proxyd.DaemonStatusResponse{ProjectCount: 0}, nil
	}
	shutdownCalled := false
	ops.shutdown = func() error { shutdownCalled = true; return nil }
	// Socket stops answering immediately after shutdown.
	ops.healthAnswers = func() bool { return false }
	fresh := proxyd.NewClient("/tmp/does-not-matter.sock")
	ensureCalls := 0
	ops.ensureRunning = func() (*proxyd.Client, error) { ensureCalls++; return fresh, nil }

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.NoError(t, err)
	assert.Same(t, fresh, client, "recover must return the freshly started client")
	assert.True(t, shutdownCalled, "idle heal must shut the old daemon down")
	assert.Equal(t, 1, ensureCalls, "one EnsureRunning is enough when the first start succeeds")
}

// TestRecoverFromVersionSkew_RetriesOnNotReady pins the PID-lock-release window:
// the first EnsureRunning reports ErrDaemonNotReady, the heal retries once after
// a delay and succeeds.
func TestRecoverFromVersionSkew_RetriesOnNotReady(t *testing.T) {
	ops := fastSkewOps()
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		return &proxyd.DaemonStatusResponse{ProjectCount: 0}, nil
	}
	ops.shutdown = func() error { return nil }
	ops.healthAnswers = func() bool { return false }
	fresh := proxyd.NewClient("/tmp/x.sock")
	calls := 0
	// The first start loses the PID-lock race (wrapped ErrDaemonNotReady); the
	// heal retries once and succeeds.
	ops.ensureRunning = func() (*proxyd.Client, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("start failed: %w", proxyd.ErrDaemonNotReady)
		}
		return fresh, nil
	}

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.NoError(t, err)
	assert.Same(t, fresh, client)
	assert.Equal(t, 2, calls, "heal must retry EnsureRunning once on ErrDaemonNotReady")
}

// TestRecoverFromVersionSkew_ShutdownRefusedFatal pins the concurrent-register
// case: a project registers in the probe→shutdown window, the daemon refuses the
// graceful shutdown, and recover fails fatally — naming the racing project from a
// best-effort re-probe.
func TestRecoverFromVersionSkew_ShutdownRefusedFatal(t *testing.T) {
	ops := fastSkewOps()
	probeCalls := 0
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		probeCalls++
		if probeCalls == 1 {
			return &proxyd.DaemonStatusResponse{ProjectCount: 0}, nil
		}
		// Re-probe after the refusal sees the racing project.
		return &proxyd.DaemonStatusResponse{ProjectCount: 1, Routes: []proxyd.RouteInfo{route("/projects/racer")}}, nil
	}
	ops.shutdown = func() error { return errors.New("cannot shutdown: active routes (ACTIVE_ROUTES)") }
	ops.ensureRunning = func() (*proxyd.Client, error) {
		t.Fatal("must not start a fresh daemon after refusal")
		return nil, nil
	}

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "/projects/racer", "refused-heal message should name the racing project")
	assert.Equal(t, 2, probeCalls, "refusal re-probes status best-effort")
}

// TestRecoverFromVersionSkew_ProbeTimeoutFatal pins that a status probe
// timeout/failure is fatal with generic remediation (no project list, no
// shutdown attempt).
func TestRecoverFromVersionSkew_ProbeTimeoutFatal(t *testing.T) {
	ops := fastSkewOps()
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		return nil, context.DeadlineExceeded
	}
	ops.shutdown = func() error { t.Fatal("must not shut down when status is unknown"); return nil }

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "0.1.2")
	assert.Contains(t, err.Error(), "prox proxy stop --force")
	assert.NotContains(t, err.Error(), "registered project", "no project list when status is unknown")
}

// TestRecoverFromVersionSkew_SlowDrainFatal pins that a daemon whose socket
// keeps answering past the drain deadline is treated as busy (never force
// killed) → fatal.
func TestRecoverFromVersionSkew_SlowDrainFatal(t *testing.T) {
	ops := fastSkewOps()
	ops.drainTimeout = 0 // deadline already passed → single probe, then give up
	ops.statusProbe = func() (*proxyd.DaemonStatusResponse, error) {
		return &proxyd.DaemonStatusResponse{ProjectCount: 0}, nil
	}
	ops.shutdown = func() error { return nil }
	ops.healthAnswers = func() bool { return true } // never drains
	ops.ensureRunning = func() (*proxyd.Client, error) { t.Fatal("must not start over a draining daemon"); return nil, nil }

	vme := &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
	client, err := recoverFromVersionSkew(vme, ops)
	require.Error(t, err)
	assert.Nil(t, client)
}

// TestStartStandaloneProxy_BindFailureFatal pins D1's last silent path: with a
// proxy configured, a standalone bind failure returns an error (fatal) instead
// of warning and continuing. The error names the port conflict and --no-proxy.
func TestStartStandaloneProxy_BindFailureFatal(t *testing.T) {
	// Occupy a port on all interfaces so the proxy's ":port" bind collides.
	ln, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := &config.Config{
		Proxy: &config.ProxyConfig{
			Enabled:  true,
			Domain:   "local.dev",
			HTTPPort: port,
		},
		Services: map[string]config.ServiceConfig{
			"api": {Host: "127.0.0.1", Port: 3000},
		},
	}

	// handlers is only touched on the success path; the bind fails before that,
	// so nil is safe here (and asserts the failure returns early).
	svc, err := startStandaloneProxy(cfg, t.TempDir(), context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, svc)
	assert.ErrorIs(t, err, proxy.ErrPortInUse, "bind failure must surface a port conflict")
	assert.Contains(t, err.Error(), "--no-proxy", "escape hatch must be named")
}
