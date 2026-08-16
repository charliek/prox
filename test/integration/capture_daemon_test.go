package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/proxyd"
)

// TestDaemonCapture_EndToEnd exercises the full daemon-mode capture path under
// an ISOLATED HOME:
//
//   - A capture-enabled project (A) and a capture-disabled project (B) share one
//     HTTP listener but own distinct hostnames, each routed to its own backend.
//   - Traffic is driven through the dynamic proxy: a small (inline) POST to A, a
//     large (>64KB, disk-backed) response from A, and a request to B.
//   - Records are read back two ways — the daemon's /api/v1/requests?project=
//     endpoint and a ForwardRequests bridge into a project-side RequestManager —
//     and asserted: A carries request/response bodies, B is metadata-only, and
//     neither project sees the other's records.
//   - The disk-backed body file lives under the isolated HOME's .prox/capture and
//     loads through proxy.LoadDecodedBody with that directory as the allowlist.
func TestDaemonCapture_EndToEnd(t *testing.T) {
	skipShort(t)

	// Isolated HOME so the daemon capture dir (~/.prox/capture) is a temp dir and
	// never touches the user's real state.
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemonCaptureDir := constants.DaemonCaptureDir(home)

	// Backends. A echoes the request body on any path except /large, which
	// returns a >64KB body (forces disk-backed capture). B returns a fixed body.
	largeBody := bytes.Repeat([]byte("L"), 100*1024) // 100KB > 64KB inline threshold
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Path == "/large" {
			_, _ = w.Write(largeBody)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body) // echo
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("b-response"))
	}))
	defer backendB.Close()

	hostA, portA := splitHostPort(t, backendA.URL)
	hostB, portB := splitHostPort(t, backendB.URL)

	// Daemon components, wired like RunDaemon but with an explicit capture dir and
	// no cert manager (HTTP only).
	socketPath := shortSocketPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	registry := proxyd.NewRegistry()

	captureMgr, err := proxy.NewCaptureManagerAt(daemonCaptureDir, constants.DefaultCaptureMaxBodySize)
	if err != nil {
		t.Fatalf("NewCaptureManagerAt: %v", err)
	}
	defer captureMgr.Cleanup()

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
	defer server.Shutdown(context.Background())
	defer dynamicProxy.Shutdown(context.Background())

	client := proxyd.NewClient(socketPath)
	waitDaemonReady(t, client)

	// One HTTP listener; A and B share the port with distinct domains. A's
	// registration is the one that binds it, so it is the one that retries.
	proxyPort := registerOnFreePort(t, func(port int) error {
		_, err := client.Register(proxyd.RegisterRequest{
			ProjectDir:     "/projects/a",
			PID:            os.Getpid(),
			Version:        "test",
			Domain:         "a.local.test",
			Services:       map[string]proxyd.ServiceTarget{"app": {Host: hostA, Port: portA}},
			HTTPPort:       port,
			CaptureEnabled: true,
		})
		return err
	})
	if _, err := client.Register(proxyd.RegisterRequest{
		ProjectDir:     "/projects/b",
		PID:            os.Getpid(),
		Version:        "test",
		Domain:         "b.local.test",
		Services:       map[string]proxyd.ServiceTarget{"app": {Host: hostB, Port: portB}},
		HTTPPort:       proxyPort,
		CaptureEnabled: false,
	}); err != nil {
		t.Fatalf("Register B: %v", err)
	}

	const hostnameA = "app.a.local.test"
	const hostnameB = "app.b.local.test"

	// Project-side bridges (the daemon stream is backfill-free, so start them
	// before driving traffic).
	localRMA := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)
	localRMB := proxy.NewRequestManager(constants.DefaultProxyRequestBufferSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go proxyd.ForwardRequests(ctx, socketPath, "/projects/a", localRMA, nil, nil)
	go proxyd.ForwardRequests(ctx, socketPath, "/projects/b", localRMB, nil, nil)

	// Confirm both bridges are live by pinging until each side observes a record.
	// (Guards against subscribing after the real traffic was already published.)
	waitBridgesLive(t, proxyPort, hostnameA, hostnameB, localRMA, localRMB)

	// Drive the scenario traffic once each.
	if got := driveProxy(t, proxyPort, hostnameA, http.MethodPost, "/echo", []byte("hello small")); got != "hello small" {
		t.Fatalf("A /echo response = %q, want %q", got, "hello small")
	}
	if got := driveProxy(t, proxyPort, hostnameA, http.MethodGet, "/large", nil); len(got) != len(largeBody) {
		t.Fatalf("A /large response len = %d, want %d", len(got), len(largeBody))
	}
	if got := driveProxy(t, proxyPort, hostnameB, http.MethodGet, "/", nil); got != "b-response" {
		t.Fatalf("B response = %q, want %q", got, "b-response")
	}

	// --- Assert via the ForwardRequests bridge (project-side view) ---
	echoRec := waitForRecord(t, localRMA, func(r proxy.RequestRecord) bool { return r.URL == "/echo" })
	largeRec := waitForRecord(t, localRMA, func(r proxy.RequestRecord) bool { return r.URL == "/large" })
	bRec := waitForRecord(t, localRMB, func(r proxy.RequestRecord) bool { return r.URL == "/" })

	// A: small POST carries inline request + response bodies that round-trip.
	if echoRec.Details == nil || echoRec.Details.RequestBody == nil || echoRec.Details.ResponseBody == nil {
		t.Fatalf("A /echo record missing captured bodies: details=%+v", echoRec.Details)
	}
	if got := string(echoRec.Details.RequestBody.Data); got != "hello small" {
		t.Errorf("A /echo request body = %q, want %q", got, "hello small")
	}
	if got := string(echoRec.Details.ResponseBody.Data); got != "hello small" {
		t.Errorf("A /echo response body = %q, want %q", got, "hello small")
	}

	// A: large response is disk-backed — FilePath under the isolated HOME's
	// capture dir, no inline Data, and it loads through the allowlist.
	if largeRec.Details == nil || largeRec.Details.ResponseBody == nil {
		t.Fatalf("A /large record missing response body")
	}
	resBody := largeRec.Details.ResponseBody
	if resBody.FilePath == "" {
		t.Fatalf("A /large response body expected disk-backed FilePath, got inline (captured_size=%d)", resBody.CapturedSize)
	}
	if !strings.HasPrefix(resBody.FilePath, daemonCaptureDir+string(filepath.Separator)) {
		t.Errorf("A /large FilePath %q not under daemon capture dir %q", resBody.FilePath, daemonCaptureDir)
	}
	if _, err := os.Stat(resBody.FilePath); err != nil {
		t.Errorf("A /large capture file not on disk: %v", err)
	}
	decoded, err := proxy.LoadDecodedBody(resBody, []string{daemonCaptureDir})
	if err != nil {
		t.Fatalf("LoadDecodedBody(disk-backed): %v", err)
	}
	if !decoded.Available {
		t.Fatalf("disk-backed body unavailable: %q", decoded.UnavailableReason)
	}
	if !bytes.Equal(decoded.Data, largeBody) {
		t.Errorf("disk-backed body mismatch: got %d bytes, want %d", len(decoded.Data), len(largeBody))
	}

	// B: capture disabled → metadata only, no Details.
	if bRec.Details != nil {
		t.Errorf("B record should be metadata-only, got Details=%+v", bRec.Details)
	}

	// No cross-project delivery on either bridge.
	for _, r := range localRMA.Recent(proxy.RequestFilter{}) {
		if r.ProjectDir != "/projects/a" {
			t.Errorf("localRMA received foreign record: project=%q url=%q", r.ProjectDir, r.URL)
		}
	}
	for _, r := range localRMB.Recent(proxy.RequestFilter{}) {
		if r.ProjectDir != "/projects/b" {
			t.Errorf("localRMB received foreign record: project=%q url=%q", r.ProjectDir, r.URL)
		}
	}

	// --- Assert via the daemon /api/v1/requests?project= endpoint ---
	aRecs := daemonGetRequests(t, socketPath, "/projects/a")
	if findRecord(aRecs, "/echo") == nil {
		t.Errorf("daemon /requests for A missing /echo record")
	}
	if r := findRecord(aRecs, "/echo"); r != nil {
		if r.Details == nil || r.Details.RequestBody == nil || string(r.Details.RequestBody.Data) != "hello small" {
			t.Errorf("daemon /requests A /echo body not captured: %+v", r.Details)
		}
	}
	if r := findRecord(aRecs, "/large"); r == nil || r.Details == nil || r.Details.ResponseBody == nil || r.Details.ResponseBody.FilePath == "" {
		t.Errorf("daemon /requests A /large not disk-backed as expected")
	}
	for _, r := range aRecs {
		if r.ProjectDir != "/projects/a" {
			t.Errorf("daemon /requests?project=/projects/a returned foreign record: %q", r.ProjectDir)
		}
	}

	bRecs := daemonGetRequests(t, socketPath, "/projects/b")
	if r := findRecord(bRecs, "/"); r == nil {
		t.Errorf("daemon /requests for B missing record")
	} else if r.Details != nil {
		t.Errorf("daemon /requests B record should be metadata-only, got Details")
	}
	for _, r := range bRecs {
		if r.ProjectDir != "/projects/b" {
			t.Errorf("daemon /requests?project=/projects/b returned foreign record: %q", r.ProjectDir)
		}
	}
}

// --- helpers ---

func splitHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host:port %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}

// shortSocketPath returns a socket path under /tmp to stay within the macOS
// 104-byte Unix socket path limit (t.TempDir() can be too long).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "prox-cap-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// freePort reserves an ephemeral port and returns it with the reservation
// STILL OPEN. The caller closes the listener immediately before handing the
// port to whatever will bind it.
//
// The previous version closed the listener itself and returned a bare number,
// which is a window, not a reservation: anything on the machine could take the
// port in between, and something did -- TestInFlight_EndToEnd failed with
// "bind: address already in use" under -race. Holding the listener does not
// close that window (the port must be free when the real listener binds), but
// it narrows it to a single statement and makes the handoff point explicit.
// Callers that can retry should go through registerOnFreePort.
func freePort(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	return l.Addr().(*net.TCPAddr).Port, l
}

func waitDaemonReady(t *testing.T, client *proxyd.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Health(); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon socket did not become ready")
}

// driveProxy sends one request through the proxy listener with the given Host
// header and returns the response body as a string.
func driveProxy(t *testing.T, port int, hostname, method, path string, body []byte) string {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = hostname
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("drive %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return string(got)
}

// waitBridgesLive pings A and B through the proxy until each project-side manager
// has observed at least one record, confirming both SSE subscriptions are active.
func waitBridgesLive(t *testing.T, port int, hostnameA, hostnameB string, localRMA, localRMB *proxy.RequestManager) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if localRMA.Count() > 0 && localRMB.Count() > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("bridges did not go live (A=%d, B=%d)", localRMA.Count(), localRMB.Count())
		case <-tick.C:
			_ = driveProxy(t, port, hostnameA, http.MethodGet, "/ping", nil)
			_ = driveProxy(t, port, hostnameB, http.MethodGet, "/ping", nil)
		}
	}
}

func waitForRecord(t *testing.T, rm *proxy.RequestManager, match func(proxy.RequestRecord) bool) proxy.RequestRecord {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, r := range rm.Recent(proxy.RequestFilter{}) {
			if match(r) {
				return r
			}
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for matching record")
		case <-tick.C:
		}
	}
}

func findRecord(recs []proxy.RequestRecord, urlPath string) *proxy.RequestRecord {
	for i := range recs {
		if recs[i].URL == urlPath {
			return &recs[i]
		}
	}
	return nil
}

// daemonGetRequests fetches records for a project from the daemon's unix-socket
// /api/v1/requests endpoint.
func daemonGetRequests(t *testing.T, socketPath, project string) []proxy.RequestRecord {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	reqURL := fmt.Sprintf("http://proxyd/api/v1/requests?project=%s", url.QueryEscape(project))
	resp, err := client.Get(reqURL)
	if err != nil {
		t.Fatalf("daemon GET /requests: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("daemon GET /requests status %d", resp.StatusCode)
	}
	var out struct {
		Requests []proxy.RequestRecord `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /requests: %v", err)
	}
	return out.Requests
}
