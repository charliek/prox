package proxyd

import (
	"bytes"
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

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
)

// newTestBackend starts an httptest server running h and returns its host, port,
// and a cleanup func registered on t.
func newTestBackend(t *testing.T, h http.HandlerFunc) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	hostStr, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return hostStr, p
}

// newCaptureProxy wires a DynamicProxy against a backend, registering a single
// http route on port 80 (hostname api.local.dev) with the given capture setting.
func newCaptureProxy(t *testing.T, captureEnabled bool, backendHost string, backendPort, bufferCap int) (*DynamicProxy, *proxy.RequestManager, *proxy.CaptureManager) {
	t.Helper()

	reg := NewRegistry()
	req := RegisterRequest{
		ProjectDir:     "/projects/a",
		PID:            1,
		Version:        "dev",
		Domain:         "local.dev",
		Services:       map[string]ServiceTarget{"api": {Host: backendHost, Port: backendPort}},
		HTTPPort:       80,
		CaptureEnabled: captureEnabled,
	}
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	if err != nil {
		t.Fatalf("NewCaptureManagerAt: %v", err)
	}
	rm := proxy.NewRequestManager(bufferCap)
	rm.SetEvictionCallback(cm.CleanupRequest)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dp := NewDynamicProxy(reg, nil, rm, cm, logger)
	return dp, rm, cm
}

// serve drives one request through the proxy handler for port 80.
func serve(dp *DynamicProxy, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	handler := dp.handler(80)
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	req.Host = "api.local.dev"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestDynamicProxy_CaptureEnabled_RecordsDetailsInline(t *testing.T) {
	const respBody = `{"ok":true}`
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		if string(reqBody) != "ping" {
			t.Errorf("backend saw request body %q, want %q", reqBody, "ping")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Backend", "yes")
		_, _ = w.Write([]byte(respBody))
	})

	dp, rm, _ := newCaptureProxy(t, true, host, port, 10)

	rec := serve(dp, "POST", "http://api.local.dev/echo", "ping", map[string]string{"X-Custom": "abc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec0 := records[0]

	if rec0.ID == "" {
		t.Error("record ID is empty; expected a pre-proxy generated ID")
	}
	if rec0.ProjectDir != "/projects/a" {
		t.Errorf("ProjectDir = %q, want /projects/a", rec0.ProjectDir)
	}
	if rec0.Details == nil {
		t.Fatal("Details is nil; expected captured details for a capture-enabled route")
	}

	d := rec0.Details
	if d.RequestBody == nil || string(d.RequestBody.Data) != "ping" {
		t.Errorf("captured request body = %v, want \"ping\"", d.RequestBody)
	}
	if d.ResponseBody == nil || string(d.ResponseBody.Data) != respBody {
		t.Errorf("captured response body = %v, want %q", d.ResponseBody, respBody)
	}
	if got := d.RequestHeaders["X-Custom"]; len(got) != 1 || got[0] != "abc" {
		t.Errorf("captured request header X-Custom = %v, want [abc]", got)
	}
	if got := d.ResponseHeaders["X-Backend"]; len(got) != 1 || got[0] != "yes" {
		t.Errorf("captured response header X-Backend = %v, want [yes]", got)
	}
}

func TestDynamicProxy_CaptureDisabled_MetadataOnly(t *testing.T) {
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	dp, rm, _ := newCaptureProxy(t, false, host, port, 10)

	rec := serve(dp, "POST", "http://api.local.dev/echo", "ping", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec0 := records[0]

	if rec0.Details != nil {
		t.Error("Details is non-nil; capture-disabled route must record metadata only")
	}
	if rec0.ID == "" {
		t.Error("record ID is empty; Record should generate one")
	}
	if rec0.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", rec0.StatusCode)
	}
}

func TestDynamicProxy_CaptureDisk_EvictionDeletesFiles(t *testing.T) {
	// Response larger than the 64KB inline threshold forces a disk spill.
	big := strings.Repeat("A", constants.DefaultCaptureInlineThreshold+4096)
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(big))
	})

	// Buffer capacity 1 so the second request evicts the first.
	dp, rm, cm := newCaptureProxy(t, true, host, port, 1)

	serve(dp, "POST", "http://api.local.dev/one", "a", nil)

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	id1 := records[0].ID
	if records[0].Details == nil || records[0].Details.ResponseBody == nil {
		t.Fatal("expected captured response body")
	}
	if records[0].Details.ResponseBody.FilePath == "" {
		t.Fatal("expected disk-backed response body (FilePath set)")
	}

	resFile := filepath.Join(cm.CaptureDir(), id1+"_res.bin")
	if _, err := os.Stat(resFile); err != nil {
		t.Fatalf("expected capture file %s to exist: %v", resFile, err)
	}

	// Second request evicts the first, firing CleanupRequest for id1.
	serve(dp, "POST", "http://api.local.dev/two", "b", nil)

	if _, err := os.Stat(resFile); !os.IsNotExist(err) {
		t.Errorf("capture file %s should have been deleted on eviction, stat err = %v", resFile, err)
	}
}

func TestDynamicProxy_ErrorHandler_CaptureRecordsBadGateway(t *testing.T) {
	// Register a route to a backend port that nothing is listening on.
	reg := NewRegistry()
	req := RegisterRequest{
		ProjectDir:     "/projects/a",
		PID:            1,
		Version:        "dev",
		Domain:         "local.dev",
		Services:       map[string]ServiceTarget{"api": {Host: "127.0.0.1", Port: 1}},
		HTTPPort:       80,
		CaptureEnabled: true,
	}
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	if err != nil {
		t.Fatalf("NewCaptureManagerAt: %v", err)
	}
	rm := proxy.NewRequestManager(10)
	rm.SetEvictionCallback(cm.CleanupRequest)
	dp := NewDynamicProxy(reg, nil, rm, cm, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := serve(dp, "GET", "http://api.local.dev/down", "", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].StatusCode != http.StatusBadGateway {
		t.Errorf("recorded StatusCode = %d, want 502", records[0].StatusCode)
	}
}

// TestDaemonCaptureDir_IsolatedHome pins the daemon capture directory shape
// under an isolated HOME without standing up the full daemon.
func TestDaemonCaptureDir_IsolatedHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	captureDir := constants.DaemonCaptureDir(homeDir)

	want := filepath.Join(tmp, ".prox", "capture")
	if captureDir != want {
		t.Fatalf("DaemonCaptureDir = %q, want %q", captureDir, want)
	}

	cm, err := proxy.NewCaptureManagerAt(captureDir, constants.DefaultCaptureMaxBodySize)
	if err != nil {
		t.Fatalf("NewCaptureManagerAt: %v", err)
	}
	if cm.CaptureDir() != want {
		t.Errorf("CaptureDir() = %q, want %q", cm.CaptureDir(), want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("capture dir %s not created (err=%v)", want, err)
	}
}
