package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractSubdomain(t *testing.T) {
	cfg := &config.ProxyConfig{
		Domain: "local.myapp.dev",
	}
	s := &Service{cfg: cfg}

	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{"simple subdomain", "app.local.myapp.dev", "app"},
		{"subdomain with port", "app.local.myapp.dev:6789", "app"},
		{"nested subdomain", "foo.bar.local.myapp.dev", "foo"},
		{"api subdomain", "api.local.myapp.dev:6789", "api"},
		{"no subdomain", "local.myapp.dev", ""},
		{"no subdomain with port", "local.myapp.dev:6789", ""},
		{"wrong domain", "app.other.dev", ""},
		{"empty host", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := s.extractSubdomain(tt.host)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStripHostPort(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{"host with port", "api.local.myapp.dev:6789", "api.local.myapp.dev"},
		{"host without port", "api.local.myapp.dev", "api.local.myapp.dev"},
		{"bare hostname with port", "localhost:8080", "localhost"},
		{"empty host", "", ""},
		{"ipv6 with port", "[::1]:443", "::1"},
		{"bare ipv6 without port", "[::1]", "[::1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, stripHostPort(tt.host))
		})
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		expected   string
	}{
		{"from RemoteAddr", "192.168.1.1:1234", "", "", "192.168.1.1"},
		{"from X-Forwarded-For", "192.168.1.1:1234", "10.0.0.1", "", "10.0.0.1"},
		{"from X-Forwarded-For multiple", "192.168.1.1:1234", "10.0.0.1, 10.0.0.2", "", "10.0.0.1"},
		{"from X-Real-IP", "192.168.1.1:1234", "", "172.16.0.1", "172.16.0.1"},
		{"X-Forwarded-For takes precedence", "192.168.1.1:1234", "10.0.0.1", "172.16.0.1", "10.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				req.Header.Set("X-Real-IP", tt.xri)
			}

			result := getClientIP(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewService(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()

	t.Run("nil config is allowed", func(t *testing.T) {
		svc, err := NewService(nil, nil, nil, logger, workDir)
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("disabled proxy with no domain is allowed", func(t *testing.T) {
		cfg := &config.ProxyConfig{
			Enabled: false,
		}
		svc, err := NewService(cfg, nil, nil, logger, workDir)
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("enabled proxy without domain fails", func(t *testing.T) {
		cfg := &config.ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
		}
		svc, err := NewService(cfg, nil, nil, logger, workDir)
		require.Error(t, err)
		assert.Nil(t, svc)
		assert.Contains(t, err.Error(), "domain")
	})

	t.Run("enabled proxy with domain succeeds", func(t *testing.T) {
		cfg := &config.ProxyConfig{
			Enabled:   true,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		services := map[string]config.ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		svc, err := NewService(cfg, services, nil, logger, workDir)
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("HTTP only proxy with domain succeeds", func(t *testing.T) {
		cfg := &config.ProxyConfig{
			Enabled:  true,
			HTTPPort: 6788,
			Domain:   "local.myapp.dev",
		}
		services := map[string]config.ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		// No certs needed for HTTP only
		svc, err := NewService(cfg, services, nil, logger, workDir)
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("dual stack proxy with domain succeeds", func(t *testing.T) {
		cfg := &config.ProxyConfig{
			Enabled:   true,
			HTTPPort:  6788,
			HTTPSPort: 6789,
			Domain:    "local.myapp.dev",
		}
		services := map[string]config.ServiceConfig{
			"app": {Port: 3000, Host: "localhost"},
		}
		svc, err := NewService(cfg, services, nil, logger, workDir)
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})
}

func TestRequestManagerSubscriptionID(t *testing.T) {
	rm := NewRequestManager(10)

	t.Run("subscription IDs are formatted correctly", func(t *testing.T) {
		sub1 := rm.Subscribe(RequestFilter{})
		defer rm.Unsubscribe(sub1.ID)

		assert.Equal(t, "sub-1", sub1.ID)

		sub2 := rm.Subscribe(RequestFilter{})
		defer rm.Unsubscribe(sub2.ID)

		assert.Equal(t, "sub-2", sub2.ID)
	})
}

func TestStart_RollbackHTTPOnHTTPSFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()

	hp := findFreePort(t)
	hsp := findFreePort(t)
	if hp == hsp {
		t.Skip("OS returned identical ephemeral ports; skipping to avoid spurious bind error")
	}
	cfg := &config.ProxyConfig{
		Enabled:   true,
		HTTPPort:  hp,
		HTTPSPort: hsp,
		Domain:    "local.myapp.dev",
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: 3000, Host: "localhost"},
	}

	svc, err := NewService(cfg, services, nil, logger, workDir)
	require.NoError(t, err)

	err = svc.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificates not configured")

	require.Eventually(t, func() bool {
		return !isPortListening(cfg.HTTPPort)
	}, time.Second, 20*time.Millisecond, "expected HTTP port to be closed after startup rollback")
}

func findFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func isPortListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestCreateRouter_XForwardedProto(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()

	// Create a backend that captures the X-Forwarded-Proto header
	var receivedProto atomic.Value
	receivedProto.Store("")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedProto.Store(r.Header.Get("X-Forwarded-Proto"))
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Parse backend port
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	cfg := &config.ProxyConfig{
		Enabled:  true,
		HTTPPort: 6788,
		Domain:   "local.myapp.dev",
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: backendPort, Host: "localhost"},
	}

	svc, err := NewService(cfg, services, nil, logger, workDir)
	require.NoError(t, err)

	router := svc.createRouter()

	t.Run("HTTP request sets X-Forwarded-Proto to http", func(t *testing.T) {
		receivedProto.Store("")
		req := httptest.NewRequest("GET", "/test", nil)
		req.Host = "app.local.myapp.dev:6788"
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		assert.Equal(t, "http", receivedProto.Load())
	})

	t.Run("HTTPS request sets X-Forwarded-Proto to https", func(t *testing.T) {
		receivedProto.Store("")
		req := httptest.NewRequest("GET", "/test", nil)
		req.Host = "app.local.myapp.dev:6789"
		req.TLS = &tls.ConnectionState{} // Simulate TLS connection
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		assert.Equal(t, "https", receivedProto.Load())
	})
}

func TestCreateRouter_StampsHostnameStripsPort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	cfg := &config.ProxyConfig{
		Enabled:  true,
		HTTPPort: 6788,
		Domain:   "local.myapp.dev",
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: backendPort, Host: "localhost"},
	}

	svc, err := NewService(cfg, services, nil, logger, workDir)
	require.NoError(t, err)

	router := svc.createRouter()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "app.local.myapp.dev:6788"
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	records := svc.RequestManager().Recent(RequestFilter{})
	require.Len(t, records, 1)
	assert.Equal(t, "app.local.myapp.dev", records[0].Hostname)
}

func TestPortConflictError_ErrorMessage(t *testing.T) {
	tests := []struct {
		port     int
		protocol string
		expected string
	}{
		{443, "HTTPS", "HTTPS proxy port 443 already in use"},
		{80, "HTTP", "HTTP proxy port 80 already in use"},
		{8080, "HTTP", "HTTP proxy port 8080 already in use"},
	}
	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			err := &PortConflictError{Port: tt.port, Protocol: tt.protocol}
			assert.Equal(t, tt.expected, err.Error())
		})
	}
}

func TestPortConflictError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("listen tcp :443: bind: address already in use")
	err := &PortConflictError{Port: 443, Protocol: "HTTPS", Cause: cause}

	// Sentinel is reachable via multi-unwrap
	assert.True(t, errors.Is(err, ErrPortInUse))
	// Original cause is also reachable
	assert.True(t, errors.Is(err, cause))
	assert.False(t, errors.Is(err, errors.New("something else")))
}

func TestPortConflictError_Unwrap_NilCause(t *testing.T) {
	err := &PortConflictError{Port: 80, Protocol: "HTTP"}
	assert.True(t, errors.Is(err, ErrPortInUse))
}

func TestIsAddrInUse(t *testing.T) {
	t.Run("returns true for EADDRINUSE", func(t *testing.T) {
		// Bind a port and try to bind it again
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()

		addr := listener.Addr().String()
		_, err = net.Listen("tcp", addr)
		require.Error(t, err)
		assert.True(t, isAddrInUse(err))
	})

	t.Run("returns false for nil", func(t *testing.T) {
		assert.False(t, isAddrInUse(nil))
	})

	t.Run("returns false for other errors", func(t *testing.T) {
		assert.False(t, isAddrInUse(errors.New("something else")))
	})
}

func TestStart_PortConflict_HTTP(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	workDir := t.TempDir()

	// Hold a port open on all interfaces to match how the proxy binds (":port")
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	cfg := &config.ProxyConfig{
		Enabled:  true,
		HTTPPort: port,
		Domain:   "local.myapp.dev",
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: 3000, Host: "localhost"},
	}

	svc, err := NewService(cfg, services, nil, logger, workDir)
	require.NoError(t, err)

	err = svc.Start(context.Background())
	require.Error(t, err)

	// Verify it's a PortConflictError with correct metadata
	assert.True(t, errors.Is(err, ErrPortInUse))

	var portErr *PortConflictError
	require.True(t, errors.As(err, &portErr))
	assert.Equal(t, port, portErr.Port)
	assert.Equal(t, "HTTP", portErr.Protocol)
	// Original OS error is preserved via multi-unwrap
	assert.NotNil(t, portErr.Cause)
	assert.True(t, errors.Is(err, syscall.EADDRINUSE))
}

func TestResponseWriter_FirstResponseHook(t *testing.T) {
	newRW := func(w http.ResponseWriter) *responseWriter {
		return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	}

	t.Run("hook fires once but status is last-write-wins", func(t *testing.T) {
		rw := newRW(httptest.NewRecorder())
		var calls []int
		rw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		rw.WriteHeader(http.StatusCreated)
		rw.WriteHeader(http.StatusAccepted)

		assert.Equal(t, []int{http.StatusCreated}, calls, "hook fires once on first >=200")
		assert.Equal(t, http.StatusAccepted, rw.statusCode, "status is last-write-wins")
	})

	t.Run("1xx then 200 fires once with 200 and records 200", func(t *testing.T) {
		rw := newRW(httptest.NewRecorder())
		var calls []int
		rw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		rw.WriteHeader(http.StatusEarlyHints) // 103
		assert.Empty(t, calls, "1xx must not fire the hook")
		assert.Equal(t, http.StatusOK, rw.statusCode, "1xx must not overwrite the status")

		rw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusOK}, calls)
		assert.Equal(t, http.StatusOK, rw.statusCode)
	})

	t.Run("implicit bare Write does not fire the hook", func(t *testing.T) {
		rw := newRW(httptest.NewRecorder())
		var calls []int
		rw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _ = rw.Write([]byte("hi"))
		assert.Empty(t, calls)
	})

	t.Run("failed hijack does not fire the hook", func(t *testing.T) {
		rw := newRW(httptest.NewRecorder())
		var calls []int
		rw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := rw.Hijack()
		require.Error(t, err)
		assert.Empty(t, calls)
		assert.False(t, rw.Hijacked())
	})

	t.Run("successful hijack fires 101 once, no re-fire on late WriteHeader", func(t *testing.T) {
		rw := newRW(newHookHijacker(t))
		var calls []int
		rw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := rw.Hijack()
		require.NoError(t, err)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls)
		assert.True(t, rw.Hijacked())

		rw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls)
	})
}

// --- two-phase recording (D6/D7) e2e helpers ---

// newTwoPhaseService builds a Service routing subdomain "app" to backendPort,
// with capture on or off, under a quiet logger.
func newTwoPhaseService(t *testing.T, captureEnabled bool, backendPort int) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.ProxyConfig{
		Enabled:  true,
		HTTPPort: 6788,
		Domain:   "local.myapp.dev",
	}
	if captureEnabled {
		cfg.Capture = &config.CaptureConfig{Enabled: true}
	}
	services := map[string]config.ServiceConfig{
		"app": {Port: backendPort, Host: "localhost"},
	}
	svc, err := NewService(cfg, services, nil, logger, t.TempDir())
	require.NoError(t, err)
	return svc
}

func readRecordEvent(t *testing.T, ch <-chan RequestRecord) RequestRecord {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(3 * time.Second):
		t.Fatal("expected a subscriber event, got none")
		return RequestRecord{}
	}
}

func assertNoMoreEvents(t *testing.T, ch <-chan RequestRecord) {
	t.Helper()
	select {
	case rec := <-ch:
		t.Fatalf("unexpected extra subscriber event: %+v", rec)
	case <-time.After(150 * time.Millisecond):
	}
}

// assertFieldParity pins that the in-flight and completion records agree on
// every field EXCEPT InFlight, Duration, and Details (the StatusCode source is
// the same value in these tests, so it is asserted equal too).
func assertFieldParity(t *testing.T, inflight, final RequestRecord) {
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

func TestCreateRouter_TwoPhaseRecording(t *testing.T) {
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

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusAccepted) // 202 — distinctive non-200 header status
				_, _ = w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				<-release
				_, _ = w.Write([]byte("-rest"))
			}))
			defer backend.Close()
			backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

			svc := newTwoPhaseService(t, capture, backendPort)
			rm := svc.RequestManager()
			router := svc.createRouter()

			sub := rm.Subscribe(RequestFilter{})
			defer rm.Unsubscribe(sub.ID)

			req := httptest.NewRequest("GET", "/stream", nil)
			req.Host = "app.local.myapp.dev:6788"
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				router.ServeHTTP(rec, req)
				close(done)
			}()

			// Phase 1: an in-flight record with the real header status appears
			// while the body is still streaming.
			var inflightID string
			require.Eventually(t, func() bool {
				recs := rm.Recent(RequestFilter{})
				if len(recs) != 1 || !recs[0].InFlight {
					return false
				}
				inflightID = recs[0].ID
				return recs[0].StatusCode == http.StatusAccepted
			}, 3*time.Second, 5*time.Millisecond, "in-flight record should appear mid-stream")

			mid := rm.Recent(RequestFilter{})
			require.Len(t, mid, 1)
			assert.Equal(t, time.Duration(0), mid[0].Duration, "in-flight Duration is 0")
			assert.Nil(t, mid[0].Details, "in-flight record carries no Details")

			// Release and let the response complete.
			doRelease()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not complete after release")
			}

			finals := rm.Recent(RequestFilter{})
			require.Len(t, finals, 1, "completion replaces the same ID in place, no duplicate row")
			f := finals[0]
			assert.Equal(t, inflightID, f.ID, "completion carries the in-flight ID")
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

			// Exactly two subscriber events (in-flight then final), field parity.
			ev1 := readRecordEvent(t, sub.Ch)
			ev2 := readRecordEvent(t, sub.Ch)
			assertNoMoreEvents(t, sub.Ch)
			assert.True(t, ev1.InFlight)
			assert.False(t, ev2.InFlight)
			assertFieldParity(t, ev1, ev2)
		})
	}
}

func TestCreateRouter_EarlyNotFound_SingleFinalEvent(t *testing.T) {
	svc := newTwoPhaseService(t, false, 1) // backend port irrelevant; no proxy happens
	rm := svc.RequestManager()
	router := svc.createRouter()

	sub := rm.Subscribe(RequestFilter{})
	defer rm.Unsubscribe(sub.ID)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Host = "unknown-service.local.myapp.dev:6788" // resolves subdomain, but no such service
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	ev := readRecordEvent(t, sub.Ch)
	assert.False(t, ev.InFlight, "early-404 emits a single FINAL event")
	assert.Equal(t, http.StatusNotFound, ev.StatusCode)
	assertNoMoreEvents(t, sub.Ch)

	recs := rm.Recent(RequestFilter{})
	require.Len(t, recs, 1)
	assert.False(t, recs[0].InFlight)
}

func TestCreateRouter_ErrorHandler_InFlightThenFinal502(t *testing.T) {
	// Route to a port nothing listens on so the reverse proxy's ErrorHandler
	// runs (http.Error → WriteHeader(502) on the wrapper → in-flight hook).
	svc := newTwoPhaseService(t, false, 1)
	rm := svc.RequestManager()
	router := svc.createRouter()

	sub := rm.Subscribe(RequestFilter{})
	defer rm.Unsubscribe(sub.ID)

	req := httptest.NewRequest("GET", "/down", nil)
	req.Host = "app.local.myapp.dev:6788"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)

	ev1 := readRecordEvent(t, sub.Ch)
	ev2 := readRecordEvent(t, sub.Ch)
	assertNoMoreEvents(t, sub.Ch)
	assert.True(t, ev1.InFlight)
	assert.Equal(t, http.StatusBadGateway, ev1.StatusCode)
	assert.False(t, ev2.InFlight)
	assert.Equal(t, http.StatusBadGateway, ev2.StatusCode)
	assert.Equal(t, ev1.ID, ev2.ID)
}

func TestCreateRouter_ClientDisconnect_RecordsCompletion(t *testing.T) {
	// Real front server so ReverseProxy panics http.ErrAbortHandler on the
	// mid-stream copy failure (ResponseRecorder alone never triggers it).
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer backend.Close()
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	svc := newTwoPhaseService(t, true, backendPort)
	rm := svc.RequestManager()

	front := httptest.NewServer(svc.createRouter())
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", front.URL+"/stream", nil)
	require.NoError(t, err)
	req.Host = "app.local.myapp.dev"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	// Read a little of the stream, then abort mid-stream.
	buf := make([]byte, 8)
	_, _ = io.ReadFull(resp.Body, buf)
	cancel()
	_ = resp.Body.Close()

	// The deferred completion must record a final (not stuck in-flight) row.
	require.Eventually(t, func() bool {
		recs := rm.Recent(RequestFilter{})
		return len(recs) == 1 && !recs[0].InFlight && recs[0].Duration > 0
	}, 3*time.Second, 10*time.Millisecond, "aborted stream must produce a final record")

	f := rm.Recent(RequestFilter{})[0]
	assert.Equal(t, http.StatusOK, f.StatusCode)
	require.NotNil(t, f.Details, "capture-on aborted stream still records (truncated) details")
}

func TestCreateRouter_BackendDeath_RecordsCompletion(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial-body-before-death"))
		w.(http.Flusher).Flush()
		// Abruptly kill the connection mid-body: hijack and close.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer backend.Close()
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	svc := newTwoPhaseService(t, true, backendPort)
	rm := svc.RequestManager()

	front := httptest.NewServer(svc.createRouter())
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/stream", nil)
	require.NoError(t, err)
	req.Host = "app.local.myapp.dev"

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body) // drains until the backend death truncates it
	_ = resp.Body.Close()

	require.Eventually(t, func() bool {
		recs := rm.Recent(RequestFilter{})
		return len(recs) == 1 && !recs[0].InFlight && recs[0].Duration > 0
	}, 3*time.Second, 10*time.Millisecond, "backend death mid-body must produce a final record")

	f := rm.Recent(RequestFilter{})[0]
	assert.Equal(t, http.StatusOK, f.StatusCode)
	require.NotNil(t, f.Details)
}
