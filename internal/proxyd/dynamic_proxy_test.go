package proxyd

import (
	"bufio"
	"bytes"
	"context"
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
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// hijackRecorder is an http.ResponseWriter that also implements http.Hijacker,
// backed by an in-memory net.Pipe, so a reverse-proxy WebSocket upgrade can be
// driven in a unit test (httptest.ResponseRecorder alone is not a Hijacker).
type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rw := bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn))
	return h.conn, rw, nil
}

func TestDynamicProxy_Hijacked_RecordsSwitchingProtocols(t *testing.T) {
	// Backend accepts the upgrade by hijacking and writing a raw 101.
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	})

	dp, rm, _ := newCaptureProxy(t, true, host, port, 10)

	// Subscribe before driving: the ring holds ONE record (in-flight 101 then a
	// final 101 upserted in place), but a subscriber sees TWO events.
	sub := rm.Subscribe(proxy.RequestFilter{ProjectDir: "/projects/a"})
	defer rm.Unsubscribe(sub.ID)

	clientEnd, testEnd := net.Pipe()
	defer clientEnd.Close()
	defer testEnd.Close()
	// Drain whatever the proxy writes to the hijacked client conn (the 101) so
	// its flush does not block on the synchronous pipe.
	go func() { _, _ = io.Copy(io.Discard, testEnd) }()

	hr := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), conn: clientEnd}
	req := httptest.NewRequest("GET", "http://api.local.dev/ws", nil)
	req.Host = "api.local.dev"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	done := make(chan struct{})
	go func() {
		dp.handler(80).ServeHTTP(hr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hijacked request did not complete within 5s")
	}

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec0 := records[0]
	if rec0.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("StatusCode = %d, want %d (101 Switching Protocols)", rec0.StatusCode, http.StatusSwitchingProtocols)
	}
	if rec0.Details != nil {
		t.Error("Details should be nil for a hijacked (metadata-only) record")
	}

	// Two subscriber events: in-flight 101 then final 101 metadata-only.
	ev1 := readProxydEvent(t, sub.Ch)
	ev2 := readProxydEvent(t, sub.Ch)
	assertNoMoreProxydEvents(t, sub.Ch)
	assert.True(t, ev1.InFlight)
	assert.Equal(t, http.StatusSwitchingProtocols, ev1.StatusCode)
	assert.False(t, ev2.InFlight)
	assert.Equal(t, http.StatusSwitchingProtocols, ev2.StatusCode)
	assert.Nil(t, ev2.Details)
	assertProxydFieldParity(t, ev1, ev2)
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

// --- first-response hook + two-phase recording (C4) ---

func readProxydEvent(t *testing.T, ch <-chan proxy.RequestRecord) proxy.RequestRecord {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(3 * time.Second):
		t.Fatal("expected a subscriber event, got none")
		return proxy.RequestRecord{}
	}
}

func assertNoMoreProxydEvents(t *testing.T, ch <-chan proxy.RequestRecord) {
	t.Helper()
	select {
	case rec := <-ch:
		t.Fatalf("unexpected extra subscriber event: %+v", rec)
	case <-time.After(150 * time.Millisecond):
	}
}

// assertProxydFieldParity pins that the in-flight and completion records agree
// on every field except InFlight, Duration, and Details (StatusCode is the same
// value in these tests, so it is asserted equal too).
func assertProxydFieldParity(t *testing.T, inflight, final proxy.RequestRecord) {
	t.Helper()
	assert.Equal(t, inflight.ID, final.ID)
	assert.Equal(t, inflight.Timestamp, final.Timestamp)
	assert.Equal(t, inflight.Method, final.Method)
	assert.Equal(t, inflight.URL, final.URL)
	assert.Equal(t, inflight.Subdomain, final.Subdomain)
	assert.Equal(t, inflight.Hostname, final.Hostname)
	assert.Equal(t, inflight.ProjectDir, final.ProjectDir)
	assert.Equal(t, inflight.RemoteAddr, final.RemoteAddr)
	assert.Equal(t, inflight.StatusCode, final.StatusCode)
}

// newPipeHijackRecorder returns a hijackRecorder whose Hijack() succeeds,
// backed by an in-memory net.Pipe.
func newPipeHijackRecorder(t *testing.T) *hijackRecorder {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	go func() { _, _ = io.Copy(io.Discard, c2) }() // drain what the writer flushes
	return &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), conn: c1}
}

func TestStatusResponseWriter_FirstResponseHook(t *testing.T) {
	newSRW := func(w http.ResponseWriter) *statusResponseWriter {
		return &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	}

	t.Run("repeated WriteHeader fires once with the first >=200 code", func(t *testing.T) {
		srw := newSRW(httptest.NewRecorder())
		var calls []int
		srw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		srw.WriteHeader(http.StatusCreated)
		srw.WriteHeader(http.StatusInternalServerError)

		assert.Equal(t, []int{http.StatusCreated}, calls)
		assert.Equal(t, http.StatusCreated, srw.statusCode, "first-wins latch")
	})

	t.Run("1xx then 200 fires once with 200 and records 200", func(t *testing.T) {
		srw := newSRW(httptest.NewRecorder())
		var calls []int
		srw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		srw.WriteHeader(http.StatusEarlyHints) // 103
		assert.Empty(t, calls, "1xx must not fire the hook")
		assert.Equal(t, http.StatusOK, srw.statusCode, "1xx must not latch the status")

		srw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusOK}, calls)
		assert.Equal(t, http.StatusOK, srw.statusCode)
	})

	t.Run("implicit bare Write does not fire the hook", func(t *testing.T) {
		srw := newSRW(httptest.NewRecorder())
		var calls []int
		srw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _ = srw.Write([]byte("hi"))
		assert.Empty(t, calls)
	})

	t.Run("failed hijack does not fire the hook", func(t *testing.T) {
		srw := newSRW(httptest.NewRecorder()) // ResponseRecorder is not a Hijacker
		var calls []int
		srw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := srw.Hijack()
		require.Error(t, err)
		assert.Empty(t, calls)
		assert.False(t, srw.Hijacked())
	})

	t.Run("successful hijack fires 101 once, no re-fire on late WriteHeader", func(t *testing.T) {
		srw := newSRW(newPipeHijackRecorder(t))
		var calls []int
		srw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := srw.Hijack()
		require.NoError(t, err)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls)
		assert.True(t, srw.Hijacked())

		srw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls)
	})
}

func TestDynamicProxy_TwoPhaseRecording(t *testing.T) {
	for _, capture := range []bool{false, true} {
		capture := capture
		name := "capture-off"
		if capture {
			name = "capture-on"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			doRelease := func() { once.Do(func() { close(release) }) }
			t.Cleanup(doRelease)

			host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusAccepted) // 202
				_, _ = w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write([]byte("-rest"))
			})

			dp, rm, _ := newCaptureProxy(t, capture, host, port, 10)
			filter := proxy.RequestFilter{ProjectDir: "/projects/a"}

			sub := rm.Subscribe(filter)
			defer rm.Unsubscribe(sub.ID)

			req := httptest.NewRequest("GET", "http://api.local.dev/stream", nil)
			req.Host = "api.local.dev"
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				dp.handler(80).ServeHTTP(rec, req)
				close(done)
			}()

			var inflightID string
			require.Eventually(t, func() bool {
				recs := rm.Recent(filter)
				if len(recs) != 1 || !recs[0].InFlight {
					return false
				}
				inflightID = recs[0].ID
				return recs[0].StatusCode == http.StatusAccepted
			}, 3*time.Second, 5*time.Millisecond, "in-flight record should appear mid-stream")

			mid := rm.Recent(filter)
			require.Len(t, mid, 1)
			assert.Equal(t, time.Duration(0), mid[0].Duration)
			assert.Nil(t, mid[0].Details, "in-flight record carries no Details")

			doRelease()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not complete after release")
			}

			finals := rm.Recent(filter)
			require.Len(t, finals, 1, "completion replaces the same ID in place")
			f := finals[0]
			assert.Equal(t, inflightID, f.ID)
			assert.False(t, f.InFlight)
			assert.Equal(t, http.StatusAccepted, f.StatusCode)
			assert.Greater(t, f.Duration, time.Duration(0))
			if capture {
				require.NotNil(t, f.Details)
				require.NotNil(t, f.Details.ResponseBody)
				assert.Equal(t, "partial-rest", string(f.Details.ResponseBody.Data))
			} else {
				assert.Nil(t, f.Details)
			}

			ev1 := readProxydEvent(t, sub.Ch)
			ev2 := readProxydEvent(t, sub.Ch)
			assertNoMoreProxydEvents(t, sub.Ch)
			assert.True(t, ev1.InFlight)
			assert.False(t, ev2.InFlight)
			assertProxydFieldParity(t, ev1, ev2)
		})
	}
}

func TestDynamicProxy_Hijacked_NonCapture_Records101(t *testing.T) {
	// The deliberate D8 behavior change: a non-capture hijacked request now
	// records 101 (previously the writer's default 200).
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	})

	dp, rm, _ := newCaptureProxy(t, false, host, port, 10) // capture OFF
	sub := rm.Subscribe(proxy.RequestFilter{ProjectDir: "/projects/a"})
	defer rm.Unsubscribe(sub.ID)

	hr := newPipeHijackRecorder(t)
	req := httptest.NewRequest("GET", "http://api.local.dev/ws", nil)
	req.Host = "api.local.dev"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	done := make(chan struct{})
	go func() {
		dp.handler(80).ServeHTTP(hr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hijacked request did not complete within 5s")
	}

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	require.Len(t, records, 1)
	assert.Equal(t, http.StatusSwitchingProtocols, records[0].StatusCode,
		"non-capture hijack now records 101, not the writer's default 200")
	assert.Nil(t, records[0].Details)

	ev1 := readProxydEvent(t, sub.Ch)
	ev2 := readProxydEvent(t, sub.Ch)
	assertNoMoreProxydEvents(t, sub.Ch)
	assert.True(t, ev1.InFlight)
	assert.Equal(t, http.StatusSwitchingProtocols, ev1.StatusCode)
	assert.False(t, ev2.InFlight)
	assert.Equal(t, http.StatusSwitchingProtocols, ev2.StatusCode)
}

func TestDynamicProxy_ClientDisconnect_RecordsCompletion(t *testing.T) {
	// Real front server so ReverseProxy panics http.ErrAbortHandler on the
	// mid-stream copy failure; the deferred completion must still record.
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 10000; i++ {
			if _, err := w.Write([]byte("chunk-of-streaming-body-")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-release:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	})

	dp, rm, _ := newCaptureProxy(t, true, host, port, 10)
	front := httptest.NewServer(dp.handler(80))
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", front.URL+"/stream", nil)
	require.NoError(t, err)
	req.Host = "api.local.dev"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	buf := make([]byte, 8)
	_, _ = io.ReadFull(resp.Body, buf)
	cancel()
	_ = resp.Body.Close()

	filter := proxy.RequestFilter{ProjectDir: "/projects/a"}
	require.Eventually(t, func() bool {
		recs := rm.Recent(filter)
		return len(recs) == 1 && !recs[0].InFlight && recs[0].Duration > 0
	}, 3*time.Second, 10*time.Millisecond, "aborted stream must produce a final record")

	f := rm.Recent(filter)[0]
	assert.Equal(t, http.StatusOK, f.StatusCode)
	require.NotNil(t, f.Details, "capture-on aborted stream still records details")
}
