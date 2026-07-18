package proxyd

import (
	"context"
	"testing"
	"time"

	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
)

// TestForwardRequests_FiltersByProject pins that the forwarder subscribes by
// project dir (via the ?project= stream param) and the daemon delivers only the
// owning project's records — even when a second project shares the hostname.
func TestForwardRequests_FiltersByProject(t *testing.T) {
	server, _, socketPath := startTestServer(t)

	daemonRM := proxy.NewRequestManager(100)
	server.SetRequestManager(daemonRM)

	localRM := proxy.NewRequestManager(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ForwardRequests(ctx, socketPath, "/projects/a", localRM)

	// The bridge is stream-only (no backfill), so records must be produced after
	// the forwarder subscribes. Record both projects on a tick until the local
	// side observes A's record.
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()

	i := 0
loop:
	for {
		select {
		case <-deadline:
			t.Fatal("forwarder never received project A's record")
		case <-tick.C:
			i++
			ts := time.Now()
			daemonRM.Record(proxy.RequestRecord{Timestamp: ts, Method: "GET", URL: "/a", Hostname: "api.local.dev", ProjectDir: "/projects/a"})
			daemonRM.Record(proxy.RequestRecord{Timestamp: ts.Add(time.Duration(i)), Method: "GET", URL: "/b", Hostname: "api.local.dev", ProjectDir: "/projects/b"})
			if localRM.Count() > 0 {
				break loop
			}
		}
	}

	// Let any (incorrectly) delivered B records land, then assert none did.
	time.Sleep(100 * time.Millisecond)
	for _, rec := range localRM.Recent(proxy.RequestFilter{}) {
		assert.Equal(t, "/projects/a", rec.ProjectDir, "forwarder must only receive project A's records")
	}
}
