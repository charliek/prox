// Package proxy provides an HTTPS reverse proxy with subdomain-based routing.
// It allows mapping subdomains to local ports (e.g., app.local.dev:6789 → localhost:3000).
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy/certs"
)

// Service manages the HTTPS reverse proxy server.
type Service struct {
	cfg      *config.ProxyConfig
	services map[string]config.ServiceConfig
	certs    *certs.Manager
	logger   *slog.Logger

	server    *http.Server
	transport *http.Transport
	mu        sync.RWMutex

	// Request tracking
	requestManager *RequestManager
}

// NewService creates a new proxy service.
// Returns an error if cfg is nil when proxy is expected to be enabled.
func NewService(cfg *config.ProxyConfig, services map[string]config.ServiceConfig, certsCfg *config.CertsConfig, logger *slog.Logger) (*Service, error) {
	// Allow nil cfg only if proxy won't be started
	if cfg != nil && cfg.Enabled && cfg.Domain == "" {
		return nil, fmt.Errorf("proxy config requires domain when enabled")
	}

	var certsMgr *certs.Manager
	if certsCfg != nil && cfg != nil {
		certsMgr = certs.NewManager(certsCfg.Dir, cfg.Domain)
	}

	// Create shared transport for connection pooling
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   constants.DefaultProxyDialTimeout,
			KeepAlive: constants.DefaultProxyKeepAlive,
		}).DialContext,
		ResponseHeaderTimeout: constants.DefaultProxyBackendTimeout,
		MaxIdleConns:          constants.DefaultProxyMaxIdleConns,
		IdleConnTimeout:       constants.DefaultProxyIdleConnTimeout,
	}

	return &Service{
		cfg:            cfg,
		services:       services,
		certs:          certsMgr,
		logger:         logger,
		transport:      transport,
		requestManager: NewRequestManager(constants.DefaultProxyRequestBufferSize),
	}, nil
}

// Start starts the HTTPS reverse proxy server.
func (s *Service) Start(ctx context.Context) error {
	if s.cfg == nil || !s.cfg.Enabled {
		return nil
	}

	// Check that certs manager is configured
	if s.certs == nil {
		return fmt.Errorf("certificates not configured for proxy")
	}

	// Ensure certificates exist
	certPaths, err := s.certs.EnsureCerts()
	if err != nil {
		return fmt.Errorf("ensuring certificates: %w", err)
	}

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(certPaths.CertFile, certPaths.KeyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate: %w", err)
	}

	// Create TLS config
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// Create the router
	router := s.createRouter()

	// Create HTTP server
	addr := fmt.Sprintf(":%d", s.cfg.HTTPSPort)
	server := &http.Server{
		Addr:         addr,
		Handler:      router,
		TLSConfig:    tlsConfig,
		ReadTimeout:  constants.DefaultProxyReadTimeout,
		WriteTimeout: constants.DefaultProxyWriteTimeout,
		IdleTimeout:  constants.DefaultProxyIdleTimeout,
	}

	// Assign server with lock to avoid race condition with Shutdown
	s.mu.Lock()
	s.server = server
	s.mu.Unlock()

	// Start listening
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	tlsListener := tls.NewListener(listener, tlsConfig)

	s.logger.Info("proxy server started",
		"addr", addr,
		"domain", s.cfg.Domain,
		"services", len(s.services),
	)

	// Start server in goroutine
	go func() {
		if err := s.server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("proxy server error", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully stops the proxy server.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	s.mu.Unlock()

	if server == nil {
		return nil
	}

	s.logger.Info("shutting down proxy server")

	// Close the request manager to clean up subscriptions
	s.requestManager.Close()

	return server.Shutdown(ctx)
}

// RequestManager returns the request manager for tracking proxy requests.
func (s *Service) RequestManager() *RequestManager {
	return s.requestManager
}

// createRouter creates the HTTP handler that routes requests based on subdomain.
func (s *Service) createRouter() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()

		// Extract subdomain from host
		subdomain := s.extractSubdomain(r.Host)
		if subdomain == "" {
			s.recordRequest(r, subdomain, http.StatusNotFound, startTime)
			http.Error(w, "No subdomain specified", http.StatusNotFound)
			return
		}

		// Look up service
		svc, ok := s.services[subdomain]
		if !ok {
			s.recordRequest(r, subdomain, http.StatusNotFound, startTime)
			http.Error(w, fmt.Sprintf("Unknown service: %s", subdomain), http.StatusNotFound)
			return
		}

		// Create reverse proxy
		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", svc.Host, svc.Port),
		}

		proxy := httputil.NewSingleHostReverseProxy(target)

		// Use shared transport for connection pooling
		proxy.Transport = s.transport

		// Customize the director to preserve the original request info
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			// Preserve the original host header for applications that need it
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Real-IP", getClientIP(r))
		}

		// Wrap response writer to capture status code (before ErrorHandler so it can set status)
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Custom error handler - log detailed error but return generic message to client
		// Note: We set rw.statusCode here instead of calling recordRequest to avoid double-recording
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Error("proxy error",
				"subdomain", subdomain,
				"target", target.String(),
				"error", err,
			)
			rw.statusCode = http.StatusBadGateway
			http.Error(w, "Backend unavailable", http.StatusBadGateway)
		}

		// Serve the request
		proxy.ServeHTTP(rw, r)

		// Record the request (single recording point for all cases)
		s.recordRequest(r, subdomain, rw.statusCode, startTime)
	})
}

// extractSubdomain extracts the subdomain from the host header.
// For example, "app.local.myapp.dev:6789" with domain "local.myapp.dev" returns "app".
func (s *Service) extractSubdomain(host string) string {
	// Remove port if present
	if colonIdx := strings.LastIndex(host, ":"); colonIdx != -1 {
		host = host[:colonIdx]
	}

	// Check if host ends with our domain with a proper label boundary (dot)
	// This prevents "evilocal.myapp.dev" from matching domain "local.myapp.dev"
	suffix := "." + s.cfg.Domain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}

	// Remove the domain and the dot before it
	subdomain := strings.TrimSuffix(host, suffix)

	// Handle nested subdomains - take only the first part
	if dotIdx := strings.Index(subdomain, "."); dotIdx != -1 {
		subdomain = subdomain[:dotIdx]
	}

	return subdomain
}

// recordRequest records a request in the request manager.
func (s *Service) recordRequest(r *http.Request, subdomain string, statusCode int, startTime time.Time) {
	record := RequestRecord{
		Timestamp:  startTime,
		Method:     r.Method,
		URL:        r.URL.String(),
		Subdomain:  subdomain,
		StatusCode: statusCode,
		Duration:   time.Since(startTime),
		RemoteAddr: getClientIP(r),
	}
	s.requestManager.Record(record)
}

// getClientIP extracts the client IP from the request.
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher for streaming responses (SSE).
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Push implements http.Pusher for HTTP/2 server push.
func (rw *responseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := rw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter for Go 1.20+ http.ResponseController compatibility.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
