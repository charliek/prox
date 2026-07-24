package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveHost drives one request through the proxy handler for a port with an
// explicit Host header (so multi-project shared-port setups can be exercised).
func serveHost(dp *DynamicProxy, port int, method, target, host, body string) *httptest.ResponseRecorder {
	handler := dp.handler(port)
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	req.Host = host
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestPerProjectRing_FloodIsolation pins AC10's first clause: one project
// flooding its ring past capacity cannot evict another project's records,
// because each project owns a full-capacity ring of its own. Two projects share
// one HTTP listener; A is flooded past a small ring capacity while B holds a
// single record, and B's record must survive intact.
func TestPerProjectRing_FloodIsolation(t *testing.T) {
	const ringCap = 5
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	})

	reg := NewRegistry()
	base := RegisterRequest{PID: 1, Version: "dev", HTTPPort: 80,
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}}}
	reqA := base
	reqA.ProjectDir, reqA.Domain = "/projects/a", "a.dev"
	reqB := base
	reqB.ProjectDir, reqB.Domain = "/projects/b", "b.dev"
	_, _, err := reg.Register(reqA)
	require.NoError(t, err)
	_, _, err = reg.Register(reqB)
	require.NoError(t, err)

	ms := NewManagers(ringCap, nil)
	ms.ensure("/projects/a")
	ms.ensure("/projects/b")
	dp := NewDynamicProxy(reg, nil, ms, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// B records exactly one request first.
	recB := serveHost(dp, 80, "GET", "http://api.b.dev/only", "api.b.dev", "")
	require.Equal(t, http.StatusOK, recB.Code)
	bRecords := ms.get("/projects/b").Recent(proxy.RequestFilter{ProjectDir: "/projects/b"})
	require.Len(t, bRecords, 1)
	bID := bRecords[0].ID

	// A floods far past its ring capacity.
	for i := 0; i < ringCap*4; i++ {
		rec := serveHost(dp, 80, "GET", fmt.Sprintf("http://api.a.dev/flood/%d", i), "api.a.dev", "")
		require.Equal(t, http.StatusOK, rec.Code)
	}

	// A's ring is capped; B's ring still holds its one record — no cross-eviction.
	assert.Equal(t, ringCap, ms.get("/projects/a").Count(), "A's ring is bounded by its own capacity")
	assert.Equal(t, 1, ms.get("/projects/b").Count(), "A's flood must not evict B's record")
	_, ok := ms.get("/projects/b").GetByID(bID)
	assert.True(t, ok, "B's exact record must survive A's flood")
}

// TestPerProjectMaxBodySize_EndToEnd pins AC10's max_body_size clause: a project
// registered with a tiny MaxBodySize yields captured_size <= that cap in the
// daemon capture path, while a project registered with 0 (default) captures the
// full body — both through the one shared capture manager, keyed on the route's
// stamped MaxBodySize.
func TestPerProjectMaxBodySize_EndToEnd(t *testing.T) {
	const respLen = 5000
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, strings.Repeat("x", respLen))
	})

	reg := NewRegistry()
	base := RegisterRequest{PID: 1, Version: "dev", HTTPPort: 80, CaptureEnabled: true,
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}}}
	tiny := base
	tiny.ProjectDir, tiny.Domain, tiny.MaxBodySize = "/projects/tiny", "tiny.dev", 100
	big := base
	big.ProjectDir, big.Domain, big.MaxBodySize = "/projects/big", "big.dev", 0 // 0 -> daemon default
	_, _, err := reg.Register(tiny)
	require.NoError(t, err)
	_, _, err = reg.Register(big)
	require.NoError(t, err)

	// Stamped onto the routes.
	tinyRoute, ok := reg.Lookup("api.tiny.dev", 80)
	require.True(t, ok)
	assert.Equal(t, int64(100), tinyRoute.MaxBodySize, "route must carry the project's MaxBodySize")

	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	ms := NewManagers(100, cm.CleanupRequest)
	ms.ensure("/projects/tiny")
	ms.ensure("/projects/big")
	dp := NewDynamicProxy(reg, nil, ms, cm, slog.New(slog.NewTextHandler(io.Discard, nil)))

	recTiny := serveHost(dp, 80, "GET", "http://api.tiny.dev/x", "api.tiny.dev", "")
	require.Equal(t, http.StatusOK, recTiny.Code)
	recBig := serveHost(dp, 80, "GET", "http://api.big.dev/x", "api.big.dev", "")
	require.Equal(t, http.StatusOK, recBig.Code)

	tinyRecords := ms.get("/projects/tiny").Recent(proxy.RequestFilter{ProjectDir: "/projects/tiny"})
	require.Len(t, tinyRecords, 1)
	require.NotNil(t, tinyRecords[0].Details)
	require.NotNil(t, tinyRecords[0].Details.ResponseBody)
	assert.LessOrEqual(t, tinyRecords[0].Details.ResponseBody.CapturedSize, int64(100),
		"tiny project's captured_size must honor its 100-byte cap")
	assert.True(t, tinyRecords[0].Details.ResponseBody.Truncated, "tiny capture must be truncated")

	bigRecords := ms.get("/projects/big").Recent(proxy.RequestFilter{ProjectDir: "/projects/big"})
	require.Len(t, bigRecords, 1)
	require.NotNil(t, bigRecords[0].Details)
	require.NotNil(t, bigRecords[0].Details.ResponseBody)
	assert.Equal(t, int64(respLen), bigRecords[0].Details.ResponseBody.CapturedSize,
		"default-cap project captures the full body")
	assert.False(t, bigRecords[0].Details.ResponseBody.Truncated)
}

// TestDeregister_ClosesDaemonSideSSESubscribers pins AC10's SSE clause: a
// daemon-side stream handler blocked on a project's ring subscription returns
// when that project deregisters (the ring is Closed first, releasing the
// subscription channel).
func TestDeregister_ClosesDaemonSideSSESubscribers(t *testing.T) {
	s := newLifecycleServer()
	s.managers.ensure("/projects/a")

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/requests/stream?project=/projects/a", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		s.handleStreamRequests(rec, req)
		close(done)
	}()

	// Give the handler time to subscribe and reach its blocking select.
	time.Sleep(50 * time.Millisecond)

	// Deregister the project: this Closes the ring FIRST, so the blocked handler
	// observes end-of-stream and returns — it must not hang.
	s.removeProject("/projects/a")

	select {
	case <-done:
		// correct — the handler returned when the ring closed
	case <-time.After(2 * time.Second):
		t.Fatal("deregister must release the blocked daemon-side SSE handler")
	}
}

// TestInFlightCompletionAfterDeregister_DropsSafely pins AC10's race clause: a
// request whose route resolved before its project deregistered, and whose
// completion arrives after the ring was destroyed, drops safely — no panic, no
// record, and its capture files are cleaned. Run under -race, the concurrent
// hot-path ring lookup and the control-plane ring removal must not race.
func TestInFlightCompletionAfterDeregister_DropsSafely(t *testing.T) {
	release := make(chan struct{})
	hit := make(chan struct{}, 1)
	backendHost, backendPort := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release // hold the response open until the test deregisters
		_, _ = io.WriteString(w, "late")
	})

	s := newProxyServer(t)
	req := RegisterRequest{
		ProjectDir: "/projects/inflight", PID: 1, Version: "test", Domain: "local.dev",
		Services: map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort: freePort(t), CaptureEnabled: true,
	}
	registerOK(t, s, req)

	served := make(chan struct{})
	go func() {
		defer close(served)
		serveHost(s.proxy, req.HTTPPort, "GET", "http://api.local.dev/slow", "api.local.dev", "body")
	}()

	// Wait until the request is in flight at the backend.
	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the backend")
	}

	// Deregister mid-request: destroys the ring while the completion is pending.
	s.removeProject("/projects/inflight")

	// Let the response complete: the completion defer resolves a nil ring and
	// drops the record safely.
	close(release)

	select {
	case <-served:
		// correct — no panic on the completion path
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	assert.Nil(t, s.managers.get("/projects/inflight"),
		"the deregistered project's ring must be gone; the late completion left no ring")
}

// TestMissingProjectSnapshot_ReturnsEmpty200 pins the D13 endpoint contract: a
// requests snapshot for a project with no ring returns 200 with an empty list
// (a stable forwarder backfill contract during heal/deregister windows).
func TestMissingProjectSnapshot_ReturnsEmpty200(t *testing.T) {
	_, client, _ := startTestServer(t)

	resp, err := client.httpClient.Get("http://proxyd/api/v1/requests?project=/projects/missing")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Requests []proxy.RequestRecord `json:"requests"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotNil(t, body.Requests, "missing project must return a non-nil empty list")
	assert.Empty(t, body.Requests)
}

// TestMissingProjectStream_EndsCleanly pins the D13 endpoint contract: a stream
// for a project with no ring returns 200 and ends cleanly right after the
// headers (the forwarder treats a clean end as a reconnect signal).
func TestMissingProjectStream_EndsCleanly(t *testing.T) {
	_, client, _ := startTestServer(t)

	resp, err := client.httpClient.Get("http://proxyd/api/v1/requests/stream?project=/projects/missing")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The body ends cleanly (EOF) after the connected comment rather than
	// blocking forever.
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(resp.Body)
		done <- data
	}()
	select {
	case data := <-done:
		assert.Contains(t, string(data), ": connected", "stream must emit the connected comment then end")
	case <-time.After(2 * time.Second):
		t.Fatal("missing-project stream must end cleanly, not block")
	}
}

// TestDaemonStatus_PerProjectCountsAndSummedDrops pins the D13 status surface:
// per-project record counts and a daemon-wide dropped-events total summed across
// every project's ring.
func TestDaemonStatus_PerProjectCountsAndSummedDrops(t *testing.T) {
	s := newLifecycleServer()
	aRing := s.managers.ensure("/projects/a")
	bRing := s.managers.ensure("/projects/b")

	for i := 0; i < 3; i++ {
		aRing.Record(proxy.RequestRecord{ID: fmt.Sprintf("a%d", i), ProjectDir: "/projects/a", Method: "GET", URL: "/a"})
	}
	bRing.Record(proxy.RequestRecord{ID: "b0", ProjectDir: "/projects/b", Method: "GET", URL: "/b"})

	assert.Equal(t, map[string]int{"/projects/a": 3, "/projects/b": 1}, s.managers.recordCounts())

	// Force drops on A's ring: a subscriber that never reads, flooded past its
	// 100-slot channel. The daemon-wide total sums across rings.
	sub := aRing.Subscribe(proxy.RequestFilter{ProjectDir: "/projects/a"})
	defer aRing.Unsubscribe(sub.ID)
	for i := 0; i < 250; i++ {
		aRing.Record(proxy.RequestRecord{ID: fmt.Sprintf("flood%d", i), ProjectDir: "/projects/a", Method: "GET", URL: "/f"})
	}
	assert.Positive(t, s.managers.droppedTotal(), "a slow subscriber must register dropped events in the daemon-wide total")

	// The status handler surfaces both.
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var status DaemonStatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
	assert.Positive(t, status.DroppedEvents, "status must report the summed dropped events")
	assert.Equal(t, s.managers.recordCounts()["/projects/b"], status.RecordCounts["/projects/b"])
}

// TestRegisterCreatesRing_DeregisterDestroys pins the per-project ring lifecycle
// at the register/deregister boundary: a fresh register creates the ring, and a
// genuine deregister destroys it.
func TestRegisterCreatesRing_DeregisterDestroys(t *testing.T) {
	s := newLifecycleServer()
	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: "localhost", Port: 3000}}, 0, 443)
	req.PID = 1
	req.StartTime = 12345

	status, body := s.register(req)
	require.Equal(t, http.StatusOK, status, "register failed: %v", body)
	require.NotNil(t, projectRing(s, "/projects/a"), "register must create the project's ring")

	s.removeProject("/projects/a")
	assert.Nil(t, projectRing(s, "/projects/a"), "deregister must destroy the project's ring")
}
