package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxyd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHeal_SuccessSwapsClient pins D6b/D6c: a heal that re-ensures a fresh daemon
// and re-registers succeeds, swaps in the fresh client (so a later deregister
// uses it), resets the failure counter, and reports heal_state healthy. It stands
// in for the "kill-fake-daemon -> heal re-registers" path with the ensure step
// stubbed and the register exercised end-to-end over a real Unix socket.
func TestHeal_SuccessSwapsClient(t *testing.T) {
	var registerCalls int32
	sock := startFakeRegisterDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&registerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proxyd.RegisterResponse{Registered: []string{"api.local.dev"}})
	})
	fresh := proxyd.NewClient(sock)

	rt := newProxyRuntime()
	rt.SetMode(proxyModeShared)
	rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p", PID: os.Getpid(), Version: "test"})
	// Pre-load some failures so the reset is observable.
	rt.ForwarderConnectFailed(errors.New("down"))
	rt.ForwarderConnectFailed(errors.New("down"))

	ops := healOps{
		ensureRunning: func() (*proxyd.Client, error) { return fresh, nil },
		sleep:         func(time.Duration) {},
		retryDelay:    time.Millisecond,
	}

	ok := rt.heal(ops)
	require.True(t, ok, "heal must succeed against a daemon that accepts register")
	assert.Equal(t, int32(1), atomic.LoadInt32(&registerCalls), "heal must re-register exactly once")
	assert.Same(t, fresh, rt.Client(), "heal must swap in the fresh client for deregister (D6c)")
	assert.Equal(t, int64(0), rt.consecutiveFailures.Load(), "heal must reset the failure counter")
	assert.Equal(t, healStateHealthy, rt.getHealState())
}

// TestHeal_VersionMismatchKeepsWaiting pins D6b: EnsureRunning reporting a busy
// different-version daemon must NOT restart it; heal returns false and sets
// heal_state version_mismatch, leaving the client unchanged. The status block
// surfaces version_mismatch while the daemon is unreachable.
func TestHeal_VersionMismatchKeepsWaiting(t *testing.T) {
	rt := newProxyRuntime()
	rt.SetMode(proxyModeShared)
	rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p", PID: os.Getpid()})

	ops := healOps{
		ensureRunning: func() (*proxyd.Client, error) {
			return nil, &proxyd.VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"}
		},
		sleep:      func(time.Duration) {},
		retryDelay: time.Millisecond,
	}

	ok := rt.heal(ops)
	assert.False(t, ok, "heal must not claim success against a busy version-skewed daemon")
	assert.Equal(t, healStateVersionMismatch, rt.getHealState())
	assert.Nil(t, rt.Client(), "version-mismatch heal must not swap in a client")

	// Status must surface version_mismatch while the daemon is unreachable.
	rt.prober = func() (bool, string) { return false, "" }
	assert.Equal(t, healStateVersionMismatch, rt.ProxyStatus().HealState)
}

// TestHeal_ShutdownBarrierSeesSwappedClient pins FIX 3: a heal that began before
// the shutdown latch (blocked mid-Register) completes fully, and the shutdown's
// client read (clientAfterHealBarrier) blocks on the heal mutex until the swap has
// landed — so the deregister goes through the HEALED client, never the pre-heal one.
func TestHeal_ShutdownBarrierSeesSwappedClient(t *testing.T) {
	release := make(chan struct{})
	var registerCalls int32
	sock := startFakeRegisterDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&registerCalls, 1)
		<-release // block mid-register until the test releases it
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proxyd.RegisterResponse{Registered: []string{"api.local.dev"}})
	})
	fresh := proxyd.NewClient(sock)

	rt := newProxyRuntime()
	rt.SetMode(proxyModeShared)
	rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p", PID: os.Getpid(), Version: "test"})

	ops := healOps{
		ensureRunning: func() (*proxyd.Client, error) { return fresh, nil },
		sleep:         func(time.Duration) {},
		retryDelay:    time.Millisecond,
	}

	healReturned := make(chan bool, 1)
	go func() { healReturned <- rt.heal(ops) }()

	// Wait until the heal is blocked inside Register (it holds healMu here).
	require.Eventually(t, func() bool { return atomic.LoadInt32(&registerCalls) == 1 },
		2*time.Second, time.Millisecond, "heal never reached the blocking Register")

	// Shutdown latches AFTER the heal has begun (heal already passed its
	// IsShuttingDown check), then reads the client through the barrier.
	barrierDone := make(chan *proxyd.Client, 1)
	go func() {
		rt.MarkShuttingDown()
		barrierDone <- rt.clientAfterHealBarrier()
	}()

	// The barrier must not return while the heal is still swapping the client.
	select {
	case <-barrierDone:
		t.Fatal("shutdown barrier returned before the in-flight heal completed its swap")
	case <-time.After(100 * time.Millisecond):
	}

	// Let the register complete: the heal swaps the client and returns.
	close(release)
	assert.True(t, <-healReturned, "a heal begun before the latch must complete and swap the client")
	got := <-barrierDone
	assert.Same(t, fresh, got, "shutdown must deregister through the HEALED client (FIX 3)")
}

// TestHeal_RegisterVersionMismatchKeepsWaiting pins FIX 5: when EnsureRunning
// connects but the daemon's Register returns 409 VERSION_MISMATCH (a busy
// different-version daemon re-checking versions at register time), heal reports
// version_mismatch — like the EnsureRunning VersionMismatchError path — not
// healing, and does not swap in the client.
func TestHeal_RegisterVersionMismatchKeepsWaiting(t *testing.T) {
	sock := startFakeRegisterDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(proxyd.ErrorResponse{
			Error: "version mismatch: daemon is 0.1.2, client is 0.2.0",
			Code:  "VERSION_MISMATCH",
		})
	})
	client := proxyd.NewClient(sock)

	rt := newProxyRuntime()
	rt.SetMode(proxyModeShared)
	rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p", PID: os.Getpid(), Version: "0.2.0"})

	ops := healOps{
		ensureRunning: func() (*proxyd.Client, error) { return client, nil },
		sleep:         func(time.Duration) {},
		retryDelay:    time.Millisecond,
	}

	assert.False(t, rt.heal(ops), "a version-mismatch register must not claim success")
	assert.Equal(t, healStateVersionMismatch, rt.getHealState())
	assert.Nil(t, rt.Client(), "a version-mismatch register must not swap in the client")
}

// TestHeal_SuppressedDuringShutdown pins D6c: once the shutdown latch is set the
// heal callback no-ops without touching the daemon (never re-register a project
// that is tearing down).
func TestHeal_SuppressedDuringShutdown(t *testing.T) {
	rt := newProxyRuntime()
	rt.MarkShuttingDown()

	called := false
	ops := healOps{
		ensureRunning: func() (*proxyd.Client, error) { called = true; return nil, nil },
		sleep:         func(time.Duration) {},
	}

	ok := rt.heal(ops)
	assert.False(t, ok)
	assert.False(t, called, "heal must not contact the daemon once shutdown is latched")
}

// TestHeal_EnsureOrRegisterErrorHealing pins that a transient ensure/register
// failure (not a version mismatch) leaves heal_state "healing" and returns false
// so the forwarder keeps retrying.
func TestHeal_EnsureOrRegisterErrorHealing(t *testing.T) {
	t.Run("ensure_error", func(t *testing.T) {
		rt := newProxyRuntime()
		rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p"})
		ops := healOps{
			ensureRunning: func() (*proxyd.Client, error) { return nil, errors.New("connect refused") },
			sleep:         func(time.Duration) {},
		}
		assert.False(t, rt.heal(ops))
		assert.Equal(t, healStateHealing, rt.getHealState())
	})

	t.Run("register_error", func(t *testing.T) {
		rt := newProxyRuntime()
		rt.SetRegisterRequest(proxyd.RegisterRequest{ProjectDir: "/p"})
		// A client pointed at a dead socket: register fails.
		dead := proxyd.NewClient(filepath.Join(t.TempDir(), "nope.sock"))
		ops := healOps{
			ensureRunning: func() (*proxyd.Client, error) { return dead, nil },
			sleep:         func(time.Duration) {},
		}
		assert.False(t, rt.heal(ops))
		assert.Equal(t, healStateHealing, rt.getHealState())
		assert.Nil(t, rt.Client(), "a failed register must not swap in the client")
	})
}

// TestCancelForwarder_NilSafe pins that CancelForwarder is a no-op when no
// forwarder was launched, and cancels the stored context when one was.
func TestCancelForwarder_NilSafe(t *testing.T) {
	rt := newProxyRuntime()
	rt.CancelForwarder() // no forwarder set: must not panic

	cancelled := false
	rt.SetForwarderCancel(func() { cancelled = true })
	rt.CancelForwarder()
	assert.True(t, cancelled, "CancelForwarder must invoke the stored cancel")
}
