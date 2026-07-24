package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
)

// TestInFlight_EndToEnd exercises the two-phase (in-flight → completion)
// recording path end-to-end through the daemon topology, observed via the
// PROJECT's API-visible state (its local RequestManager fed by ForwardRequests):
//
//   - A capture-enabled project routes to a backend that writes response headers
//     plus a partial body, then blocks on a channel until the test releases it —
//     deterministic, no sleeps for correctness.
//   - While the backend is still held, the project side already shows the record
//     with in_flight=true and the real header-time status (bodies still
//     streaming, so no Details yet).
//   - After release, the SAME ID becomes final: in_flight=false, Duration>0, and
//     Details populated (headers + body) — and the ring holds exactly one record
//     for that ID (the completion replaced the in-flight row in place, no dup).
func TestInFlight_EndToEnd(t *testing.T) {
	skipShort(t)

	topo := newDaemonTopo(t)

	// Backend: /slow writes headers + a partial body, flushes so the reverse
	// proxy receives the response header (firing the in-flight hook), then blocks
	// on `released` until the test lets it finish. Other paths respond normally
	// so waitBridgeLive's pings succeed.
	released := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	// Always unblock the backend on exit: a failed assertion before the
	// explicit release would otherwise leave backend.Close() hanging.
	t.Cleanup(release)
	const partial = "partial-body-"
	const rest = "rest-of-body"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Path == "/slow" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(partial))
			if f, ok := w.(http.Flusher); ok {
				f.Flush() // push header + partial body to the reverse proxy now
			}
			<-released
			_, _ = w.Write([]byte(rest))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	host, port := splitHostPort(t, backend.URL)

	proxyPort := freePort(t)
	const projectDir = "/projects/inflight"
	const hostname = "app.inflight.local.test"
	if _, err := topo.client.Register(proxyd.RegisterRequest{
		ProjectDir:     projectDir,
		PID:            os.Getpid(),
		Version:        "test",
		Domain:         "inflight.local.test",
		Services:       map[string]proxyd.ServiceTarget{"app": {Host: host, Port: port}},
		HTTPPort:       proxyPort,
		CaptureEnabled: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Project-side bridge (the daemon stream is backfill-plus-live, so start it
	// before driving traffic and confirm it is live).
	localRM := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go proxyd.ForwardRequests(ctx, topo.socketPath, projectDir, localRM, nil, nil)
	waitBridgeLive(t, proxyPort, hostname, localRM)

	// Drive the slow request in a background goroutine — it stays open until we
	// release the backend. (Build the request by hand rather than via driveProxy,
	// whose t.Fatalf must not be called off the test goroutine.)
	type driveResult struct {
		body string
		err  error
	}
	resultCh := make(chan driveResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/slow", proxyPort), nil)
		if err != nil {
			resultCh <- driveResult{err: err}
			return
		}
		req.Host = hostname
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			resultCh <- driveResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		resultCh <- driveResult{body: string(b), err: err}
	}()

	// While the backend is still held, the project side sees the in-flight record
	// with the real header-time status and no Details yet.
	inflight := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.URL == "/slow" && r.InFlight
	})
	if inflight.StatusCode != http.StatusOK {
		t.Errorf("in-flight status = %d, want %d", inflight.StatusCode, http.StatusOK)
	}
	if inflight.Duration != 0 {
		t.Errorf("in-flight Duration = %v, want 0", inflight.Duration)
	}
	if inflight.Details != nil {
		t.Errorf("in-flight record should have nil Details, got %+v", inflight.Details)
	}
	inflightID := inflight.ID

	// Release the backend and let the request complete.
	release()
	res := <-resultCh
	if res.err != nil {
		t.Fatalf("slow request failed: %v", res.err)
	}
	if want := partial + rest; res.body != want {
		t.Errorf("slow response body = %q, want %q", res.body, want)
	}

	// The SAME ID now becomes final: in_flight false, real duration, Details.
	final := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.ID == inflightID && !r.InFlight
	})
	if final.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want %d", final.StatusCode, http.StatusOK)
	}
	if final.Duration <= 0 {
		t.Errorf("final Duration = %v, want > 0", final.Duration)
	}
	if final.Details == nil || final.Details.ResponseBody == nil {
		t.Fatalf("final record missing Details/ResponseBody: %+v", final.Details)
	}
	if len(final.Details.RequestHeaders) == 0 && len(final.Details.ResponseHeaders) == 0 {
		t.Errorf("final record missing captured headers: %+v", final.Details)
	}
	if got := string(final.Details.ResponseBody.Data); got != partial+rest {
		t.Errorf("final response body = %q, want %q", got, partial+rest)
	}

	// Field parity: everything but status-source/duration/details/in_flight is
	// identical between the two events.
	if final.Method != inflight.Method || final.URL != inflight.URL ||
		final.Hostname != inflight.Hostname || final.ProjectDir != inflight.ProjectDir ||
		!final.Timestamp.Equal(inflight.Timestamp) {
		t.Errorf("field parity mismatch:\n in-flight=%+v\n final=%+v", inflight, final)
	}

	// Exactly one row for that ID — the completion replaced the in-flight record
	// in place rather than appending a duplicate.
	if n := countRecordsByID(localRM, inflightID); n != 1 {
		t.Errorf("ring holds %d records for ID %s, want exactly 1", n, inflightID)
	}
}

// TestBackfill_EndToEnd exercises the forwarder snapshot backfill end-to-end:
//
//   - A capture-enabled project is registered and its bridge (ForwardRequests) is
//     brought up; one request is proxied and delivered live (the pre-gap record).
//   - The bridge is severed (its ctx is cancelled and the goroutine is awaited),
//     while the daemon keeps the registration and its ring. Several requests are
//     driven through the daemon during the gap, including one whose response is
//     >64KB and spills to the daemon capture dir.
//   - The bridge is restarted with a fresh ctx and the SAME local RequestManager;
//     the gap records appear locally via backfill — including the disk-backed body
//     loaded through proxy.LoadDecodedBody with the daemon capture dir allowlisted.
//   - Records already present locally before the gap are not duplicated on
//     reconnect (backfill re-delivery of a final record is a no-op).
func TestBackfill_EndToEnd(t *testing.T) {
	skipShort(t)

	topo := newDaemonTopo(t)

	largeBody := bytes.Repeat([]byte("L"), 100*1024) // 100KB > 64KB inline threshold
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Path == "/large" {
			_, _ = w.Write(largeBody)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body) // echo
	}))
	defer backend.Close()
	host, port := splitHostPort(t, backend.URL)

	proxyPort := freePort(t)
	const projectDir = "/projects/gap"
	const hostname = "app.gap.local.test"
	if _, err := topo.client.Register(proxyd.RegisterRequest{
		ProjectDir:     projectDir,
		PID:            os.Getpid(),
		Version:        "test",
		Domain:         "gap.local.test",
		Services:       map[string]proxyd.ServiceTarget{"app": {Host: host, Port: port}},
		HTTPPort:       proxyPort,
		CaptureEnabled: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	localRM := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)

	// --- Bridge up: deliver one pre-gap record live ---
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() {
		proxyd.ForwardRequests(ctx1, topo.socketPath, projectDir, localRM, nil, nil)
		close(done1)
	}()
	waitBridgeLive(t, proxyPort, hostname, localRM)

	if got := driveProxy(t, proxyPort, hostname, http.MethodPost, "/before", []byte("pre-gap")); got != "pre-gap" {
		t.Fatalf("/before response = %q, want %q", got, "pre-gap")
	}
	beforeRec := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.URL == "/before" && !r.InFlight
	})
	beforeID := beforeRec.ID

	// --- Sever the bridge (forwarder down) while the daemon stays registered ---
	// Awaiting done1 guarantees the forwarder goroutine has fully returned, so no
	// subsequent traffic can be delivered live: the gap is real.
	cancel1()
	<-done1

	// --- Drive gap traffic (retained only in the daemon ring) ---
	if got := driveProxy(t, proxyPort, hostname, http.MethodPost, "/gap1", []byte("g1")); got != "g1" {
		t.Fatalf("/gap1 response = %q, want %q", got, "g1")
	}
	if got := driveProxy(t, proxyPort, hostname, http.MethodPost, "/gap2", []byte("g2")); got != "g2" {
		t.Fatalf("/gap2 response = %q, want %q", got, "g2")
	}
	if got := driveProxy(t, proxyPort, hostname, http.MethodGet, "/large", nil); len(got) != len(largeBody) {
		t.Fatalf("/large response len = %d, want %d", len(got), len(largeBody))
	}

	// Sanity: with the bridge down, none of the gap records reached the project
	// side yet — they exist only in the daemon ring.
	for _, u := range []string{"/gap1", "/gap2", "/large"} {
		for _, r := range localRM.Recent(proxy.RequestFilter{}) {
			if r.URL == u {
				t.Fatalf("gap record %q reached the project side while the bridge was down", u)
			}
		}
	}

	// --- Restart the bridge with a fresh ctx and the SAME local manager ---
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go proxyd.ForwardRequests(ctx2, topo.socketPath, projectDir, localRM, nil, nil)

	// Gap records appear locally via backfill.
	gap1 := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.URL == "/gap1" && !r.InFlight
	})
	gap2 := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.URL == "/gap2" && !r.InFlight
	})
	largeRec := waitForRecord(t, localRM, func(r proxy.RequestRecord) bool {
		return r.URL == "/large" && !r.InFlight && r.Details != nil
	})

	// The gap request bodies round-tripped (inline) via the backfilled records.
	if gap1.Details == nil || gap1.Details.RequestBody == nil || string(gap1.Details.RequestBody.Data) != "g1" {
		t.Errorf("/gap1 backfilled request body wrong: %+v", gap1.Details)
	}
	if gap2.Details == nil || gap2.Details.RequestBody == nil || string(gap2.Details.RequestBody.Data) != "g2" {
		t.Errorf("/gap2 backfilled request body wrong: %+v", gap2.Details)
	}

	// The large response is disk-backed under the daemon capture dir and loads
	// through the allowlist — the same body-loading path the existing e2e uses.
	if largeRec.Details.ResponseBody == nil {
		t.Fatalf("/large backfilled record missing response body")
	}
	resBody := largeRec.Details.ResponseBody
	if resBody.FilePath == "" {
		t.Fatalf("/large response body expected disk-backed FilePath, got inline (captured_size=%d)", resBody.CapturedSize)
	}
	if !strings.HasPrefix(resBody.FilePath, topo.daemonCaptureDir+string(filepath.Separator)) {
		t.Errorf("/large FilePath %q not under daemon capture dir %q", resBody.FilePath, topo.daemonCaptureDir)
	}
	decoded, err := proxy.LoadDecodedBody(resBody, []string{topo.daemonCaptureDir})
	if err != nil {
		t.Fatalf("LoadDecodedBody(disk-backed): %v", err)
	}
	if !decoded.Available {
		t.Fatalf("disk-backed body unavailable: %q", decoded.UnavailableReason)
	}
	if !bytes.Equal(decoded.Data, largeBody) {
		t.Errorf("disk-backed body mismatch: got %d bytes, want %d", len(decoded.Data), len(largeBody))
	}

	// No duplicates: the pre-gap record was already present locally, and the
	// backfill re-delivers it as a no-op (final is terminal). Gap records arrive
	// exactly once.
	if n := countRecordsByID(localRM, beforeID); n != 1 {
		t.Errorf("pre-gap record %s duplicated after reconnect: %d rows", beforeID, n)
	}
	if n := countRecordsByID(localRM, gap1.ID); n != 1 {
		t.Errorf("/gap1 record %s duplicated: %d rows", gap1.ID, n)
	}
	if n := countRecordsByID(localRM, gap2.ID); n != 1 {
		t.Errorf("/gap2 record %s duplicated: %d rows", gap2.ID, n)
	}
	if n := countRecordsByID(localRM, largeRec.ID); n != 1 {
		t.Errorf("/large record %s duplicated: %d rows", largeRec.ID, n)
	}
}

// --- helpers (daemon-topology, in-flight/backfill) ---

// daemonTopo bundles the pieces of a running daemon under an ISOLATED HOME that
// the in-flight/backfill e2e tests drive traffic through.
type daemonTopo struct {
	socketPath       string
	daemonCaptureDir string
	client           *proxyd.Client
}

// newDaemonTopo stands up the daemon components (registry, request manager,
// capture manager, dynamic proxy, unix-socket server) wired like RunDaemon but
// with an explicit capture dir and no cert manager (HTTP only), under an
// isolated HOME so ~/.prox/capture is a temp dir. It mirrors the setup block in
// TestDaemonCapture_EndToEnd; registration is left to the caller. Cleanup is
// registered via t.Cleanup in the same order the sibling test defers it.
func newDaemonTopo(t *testing.T) daemonTopo {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	daemonCaptureDir := constants.DaemonCaptureDir(home)

	socketPath := shortSocketPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	registry := proxyd.NewRegistry()

	captureMgr, err := proxy.NewCaptureManagerAt(daemonCaptureDir, constants.DefaultCaptureMaxBodySize)
	if err != nil {
		t.Fatalf("NewCaptureManagerAt: %v", err)
	}
	t.Cleanup(func() { captureMgr.Cleanup() })

	// Per-project rings, wired like RunDaemon: each project's ring is created at
	// register time with the capture eviction callback.
	managers := proxyd.NewManagers(constants.DefaultProxyRequestBufferSize, captureMgr.CleanupRequest)
	dynamicProxy := proxyd.NewDynamicProxy(registry, nil, managers, captureMgr, logger)

	server := proxyd.NewServer(proxyd.ServerConfig{
		SocketPath: socketPath,
		Logger:     logger,
		Version:    "test",
	})
	server.SetRegistry(registry)
	server.SetProxy(dynamicProxy)
	server.SetManagers(managers)

	go func() { _ = server.Start() }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	t.Cleanup(func() { _ = dynamicProxy.Shutdown(context.Background()) })

	client := proxyd.NewClient(socketPath)
	waitDaemonReady(t, client)

	return daemonTopo{
		socketPath:       socketPath,
		daemonCaptureDir: daemonCaptureDir,
		client:           client,
	}
}

// waitBridgeLive pings the given project through the proxy until its local
// manager has observed at least one record, confirming the SSE subscription is
// active (single-project variant of waitBridgesLive).
func waitBridgeLive(t *testing.T, port int, hostname string, localRM *proxy.RequestManager) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if localRM.Count() > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("bridge did not go live (count=%d)", localRM.Count())
		case <-tick.C:
			_ = driveProxy(t, port, hostname, http.MethodGet, "/ping", nil)
		}
	}
}

// countRecordsByID returns how many records in the manager's ring carry the
// given ID — used to assert an in-flight → completion upsert replaces in place
// (exactly one row) rather than appending a duplicate.
func countRecordsByID(rm *proxy.RequestManager, id string) int {
	n := 0
	for _, r := range rm.Recent(proxy.RequestFilter{}) {
		if r.ID == id {
			n++
		}
	}
	return n
}
