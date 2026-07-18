package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
