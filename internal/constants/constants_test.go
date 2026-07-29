package constants

import "testing"

// TestSSEReadTimeout_ExceedsThreeHeartbeats pins the invariant documented on
// SSEReadTimeout: the CLI's SSE read deadline must outlast at least 3 missed
// server heartbeats, so a single delayed tick (GC pause, scheduling jitter)
// never trips a false "connection dead" verdict.
func TestSSEReadTimeout_ExceedsThreeHeartbeats(t *testing.T) {
	if SSEReadTimeout <= 3*SSEHeartbeatInterval {
		t.Fatalf("SSEReadTimeout (%s) must exceed 3*SSEHeartbeatInterval (%s)", SSEReadTimeout, 3*SSEHeartbeatInterval)
	}
}
