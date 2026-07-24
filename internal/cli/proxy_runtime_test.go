package cli

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
)

// TestProxyRuntime_ProxyStatus_ProbeUpDown pins D5: the proxy block reports
// mode, reachability, version, and heal_state driven by the injectable prober.
func TestProxyRuntime_ProxyStatus_ProbeUpDown(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		rt := newProxyRuntime()
		rt.SetMode(proxyModeShared)
		rt.prober = func() (bool, string) { return true, "9.9.9" }

		s := rt.ProxyStatus()
		if s.Mode != proxyModeShared {
			t.Errorf("Mode = %q, want %q", s.Mode, proxyModeShared)
		}
		if !s.DaemonReachable {
			t.Error("DaemonReachable = false, want true")
		}
		if s.DaemonVersion != "9.9.9" {
			t.Errorf("DaemonVersion = %q, want 9.9.9", s.DaemonVersion)
		}
		if s.HealState != "healthy" {
			t.Errorf("HealState = %q, want healthy", s.HealState)
		}
	})

	t.Run("down", func(t *testing.T) {
		rt := newProxyRuntime()
		rt.SetMode(proxyModeShared)
		rt.prober = func() (bool, string) { return false, "" }

		s := rt.ProxyStatus()
		if s.DaemonReachable {
			t.Error("DaemonReachable = true, want false")
		}
		if s.DaemonVersion != "" {
			t.Errorf("DaemonVersion = %q, want empty", s.DaemonVersion)
		}
		if s.HealState != "" {
			t.Errorf("HealState = %q, want empty when unreachable", s.HealState)
		}
	})
}

// TestProxyRuntime_ProxyStatus_StandaloneNoProbe pins that standalone/disabled
// modes report mode only and never invoke the probe (no daemon to reach).
func TestProxyRuntime_ProxyStatus_StandaloneNoProbe(t *testing.T) {
	for _, mode := range []string{proxyModeStandalone, proxyModeDisabled} {
		t.Run(mode, func(t *testing.T) {
			rt := newProxyRuntime()
			rt.SetMode(mode)
			probed := false
			rt.prober = func() (bool, string) { probed = true; return true, "x" }

			s := rt.ProxyStatus()
			if s.Mode != mode {
				t.Errorf("Mode = %q, want %q", s.Mode, mode)
			}
			if probed {
				t.Error("prober invoked in non-shared mode; want no probe")
			}
			if s.DaemonReachable {
				t.Error("DaemonReachable = true in non-shared mode")
			}
		})
	}
}

// TestProxyRuntime_ProbeCacheTTL pins D5: a second status within the TTL reuses
// the cached probe result rather than re-probing; past the TTL it re-probes.
func TestProxyRuntime_ProbeCacheTTL(t *testing.T) {
	rt := newProxyRuntime()
	rt.SetMode(proxyModeShared)

	calls := 0
	rt.prober = func() (bool, string) { calls++; return true, "v" }

	base := time.Now()
	cur := base
	rt.now = func() time.Time { return cur }

	rt.ProxyStatus() // first call probes
	rt.ProxyStatus() // within TTL: cached
	if calls != 1 {
		t.Fatalf("prober calls = %d after two calls within TTL, want 1 (cache hit)", calls)
	}

	cur = base.Add(rt.probeTTL + time.Millisecond) // advance past TTL
	rt.ProxyStatus()
	if calls != 2 {
		t.Fatalf("prober calls = %d after advancing past TTL, want 2 (re-probe)", calls)
	}
}

// TestProxyRuntime_ForwarderStateTransitions pins D5: as the forwarder status
// sink, N failures raise consecutive_failures to N with no connect time, and a
// subsequent connect resets the counter and stamps last_connected_at.
func TestProxyRuntime_ForwarderStateTransitions(t *testing.T) {
	rt := newProxyRuntime()

	const n = 3
	for i := 0; i < n; i++ {
		rt.ForwarderConnectFailed(errors.New("boom"))
	}
	if got := rt.consecutiveFailures.Load(); got != n {
		t.Errorf("consecutiveFailures = %d, want %d", got, n)
	}
	if got := rt.lastConnectedAt.Load(); got != 0 {
		t.Errorf("lastConnectedAt = %d, want 0 before any connect", got)
	}

	rt.ForwarderConnected()
	if got := rt.consecutiveFailures.Load(); got != 0 {
		t.Errorf("consecutiveFailures = %d after connect, want 0 (reset)", got)
	}
	if got := rt.lastConnectedAt.Load(); got == 0 {
		t.Error("lastConnectedAt = 0 after connect, want set")
	}

	// Backfill failures accumulate independently.
	rt.ForwarderBackfillFailed()
	rt.ForwarderBackfillFailed()
	if got := rt.backfillFailures.Load(); got != 2 {
		t.Errorf("backfillFailures = %d, want 2", got)
	}
}

// TestProxyRuntime_DroppedEventsFromLocalManager pins that the proxy block's
// dropped_events reflects the project's local forwarding request manager (D9).
func TestProxyRuntime_DroppedEventsFromLocalManager(t *testing.T) {
	rt := newProxyRuntime()
	rt.SetMode(proxyModeStandalone) // avoid the shared-mode probe

	rm := proxy.NewRequestManager(1000)
	sub := rm.Subscribe(proxy.RequestFilter{}) // unread channel
	_ = sub
	for i := 0; i < 150; i++ {
		rm.Record(proxy.RequestRecord{ID: fmt.Sprintf("r%03d", i), Method: "GET", URL: "/x"})
	}
	rt.SetLocalRequestManager(rm)

	s := rt.ProxyStatus()
	if s.DroppedEvents <= 0 {
		t.Errorf("DroppedEvents = %d, want > 0 (mirrors local manager)", s.DroppedEvents)
	}
	if s.DroppedEvents != rm.DroppedEvents() {
		t.Errorf("DroppedEvents = %d, want %d (local manager total)", s.DroppedEvents, rm.DroppedEvents())
	}
}
