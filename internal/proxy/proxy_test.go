package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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
