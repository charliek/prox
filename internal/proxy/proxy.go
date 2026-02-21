// Package proxy provides HTTP/HTTPS reverse proxy with subdomain-based routing.
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

// Service manages the HTTP/HTTPS reverse proxy servers.
type Service struct {
	cfg      *config.ProxyConfig
	services map[string]config.ServiceConfig
	certs    *certs.Manager
	logger   *slog.Logger

	httpServer  *http.Server
	httpsServer *http.Server
	transport   *http.Transport
	mu          sync.RWMutex

	// Request tracking
	requestManager *RequestManager

	// Request/response capture
	captureManager *CaptureManager
}

// NewService creates a new proxy service.
// Returns an error if cfg is nil when proxy is expected to be enabled.
// workDir is used for storing captured request/response bodies on disk.
func NewService(cfg *config.ProxyConfig, services map[string]config.ServiceConfig, certsCfg *config.CertsConfig, logger *slog.Logger, workDir string) (*Service, error) {
	// Allow nil cfg only if proxy won't be started
	if cfg != nil && cfg.Enabled && cfg.Domain == "" {
		return nil, fmt.Errorf("proxy config requires domain when enabled")
	}

	var certsMgr *certs.Manager
	// Only create cert manager if HTTPS is enabled and certs are configured
	if certsCfg != nil && cfg != nil && cfg.HTTPSPort > 0 {
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

	// Create capture manager if capture is configured
	var captureCfg *config.CaptureConfig
	if cfg != nil {
		captureCfg = cfg.Capture
	}
	captureMgr, err := NewCaptureManager(captureCfg, workDir)
	if err != nil {
		return nil, fmt.Errorf("creating capture manager: %w", err)
	}

	requestMgr := NewRequestManager(constants.DefaultProxyRequestBufferSize)

	// Set up eviction callback to clean up captured body files
	if captureMgr.Enabled() {
		requestMgr.SetEvictionCallback(captureMgr.CleanupRequest)
	}

	return &Service{
		cfg:            cfg,
		services:       services,
		certs:          certsMgr,
		logger:         logger,
		transport:      transport,
		requestManager: requestMgr,
		captureManager: captureMgr,
	}, nil
}

// Start starts the HTTP and/or HTTPS reverse proxy servers.
func (s *Service) Start(ctx context.Context) error {
	if s.cfg == nil || !s.cfg.Enabled {
		return nil
	}

	router := s.createRouter()
	httpStarted := false

	// Start HTTP server if configured
	if s.cfg.HTTPPort > 0 {
		if err := s.startHTTP(router); err != nil {
			return err
		}
		httpStarted = true
	}

	// Start HTTPS server if configured
	if s.cfg.HTTPSPort > 0 {
		if err := s.startHTTPS(router); err != nil {
			// Roll back HTTP start if HTTPS fails so startup is atomic.
			if httpStarted {
				rollbackCtx, cancel := context.WithTimeout(context.Background(), constants.DefaultShutdownTimeout)
				defer cancel()
				for _, shutdownErr := range s.stopServers(rollbackCtx) {
					s.logger.Error("failed to rollback proxy startup", "error", shutdownErr)
				}
			}
			return err
		}
	}

	return nil
}

// startHTTP starts the HTTP proxy server.
func (s *Service) startHTTP(router http.Handler) error {
	addr := fmt.Sprintf(":%d", s.cfg.HTTPPort)
	server := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  constants.DefaultProxyReadTimeout,
		WriteTimeout: constants.DefaultProxyWriteTimeout,
		IdleTimeout:  constants.DefaultProxyIdleTimeout,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	s.mu.Lock()
	s.httpServer = server
	s.mu.Unlock()

	s.logger.Info("HTTP proxy server started",
		"addr", addr,
		"domain", s.cfg.Domain,
		"services", len(s.services),
	)

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP proxy server error", "error", err)
		}
	}()

	return nil
}

// startHTTPS starts the HTTPS proxy server.
func (s *Service) startHTTPS(router http.Handler) error {
	// Check that certs manager is configured
	if s.certs == nil {
		return fmt.Errorf("certificates not configured for HTTPS proxy")
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

	addr := fmt.Sprintf(":%d", s.cfg.HTTPSPort)
	server := &http.Server{
		Addr:         addr,
		Handler:      router,
		TLSConfig:    tlsConfig,
		ReadTimeout:  constants.DefaultProxyReadTimeout,
		WriteTimeout: constants.DefaultProxyWriteTimeout,
		IdleTimeout:  constants.DefaultProxyIdleTimeout,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	s.mu.Lock()
	s.httpsServer = server
	s.mu.Unlock()

	tlsListener := tls.NewListener(listener, tlsConfig)

	s.logger.Info("HTTPS proxy server started",
		"addr", addr,
		"domain", s.cfg.Domain,
		"services", len(s.services),
	)

	go func() {
		if err := server.Serve(tlsListener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTPS proxy server error", "error", err)
		}
	}()

	return nil
}

// Shutdown gracefully stops the proxy servers.
func (s *Service) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down proxy servers")

	shutdownErrs := s.stopServers(ctx)

	// Close the request manager to clean up subscriptions
	s.requestManager.Close()

	// Clean up captured body files
	if s.captureManager != nil {
		if err := s.captureManager.Cleanup(); err != nil {
			s.logger.Error("failed to cleanup capture files", "error", err)
		}
	}

	if len(shutdownErrs) > 0 {
		return errors.Join(shutdownErrs...)
	}

	return nil
}

func (s *Service) stopServers(ctx context.Context) []error {
	s.mu.Lock()
	httpServer := s.httpServer
	httpsServer := s.httpsServer
	s.httpServer = nil
	s.httpsServer = nil
	s.mu.Unlock()

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		shutdownErrs []error
	)
	shutdownOne := func(srv *http.Server, name string) {
		defer wg.Done()
		if err := srv.Shutdown(ctx); err != nil {
			mu.Lock()
			shutdownErrs = append(shutdownErrs, fmt.Errorf("%s server shutdown: %w", name, err))
			mu.Unlock()
		}
	}
	if httpServer != nil {
		wg.Add(1)
		go shutdownOne(httpServer, "HTTP")
	}
	if httpsServer != nil {
		wg.Add(1)
		go shutdownOne(httpsServer, "HTTPS")
	}
	wg.Wait()
	return shutdownErrs
}

// RequestManager returns the request manager for tracking proxy requests.
func (s *Service) RequestManager() *RequestManager {
	return s.requestManager
}

// CaptureManager returns the capture manager for loading captured bodies.
func (s *Service) CaptureManager() *CaptureManager {
	return s.captureManager
}

// createRouter creates the HTTP handler that routes requests based on subdomain.
func (s *Service) createRouter() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()

		// Generate request ID early for capture
		requestID := generateRequestID(startTime, r.Method, r.URL.String())

		// Extract subdomain from host
		subdomain := s.extractSubdomain(r.Host)
		if subdomain == "" {
			s.recordRequest(r, subdomain, http.StatusNotFound, startTime, requestID, nil)
			http.Error(w, "No subdomain specified", http.StatusNotFound)
			return
		}

		// Look up service
		svc, ok := s.services[subdomain]
		if !ok {
			s.recordRequest(r, subdomain, http.StatusNotFound, startTime, requestID, nil)
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

		// Capture request body and headers if capture is enabled
		var reqBody *CapturedBody
		var reqHeaders http.Header
		if s.captureManager != nil && s.captureManager.Enabled() {
			reqBody, r.Body, reqHeaders = s.captureManager.CaptureRequest(requestID, r)
		} else {
			reqHeaders = cloneHeaders(r.Header)
		}

		// Determine if request came via HTTPS
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}

		// Customize the director to preserve the original request info
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			// Preserve the original host header for applications that need it
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", proto)
			req.Header.Set("X-Real-IP", getClientIP(r))
		}

		// Choose response writer based on capture mode
		var rw http.ResponseWriter
		var crw *capturingResponseWriter
		if s.captureManager != nil && s.captureManager.Enabled() {
			crw = newCapturingResponseWriter(w, s.captureManager.maxBodySize)
			rw = crw
		} else {
			rw = &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		}

		// Custom error handler - log detailed error but return generic message to client
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Error("proxy error",
				"subdomain", subdomain,
				"target", target.String(),
				"error", err,
			)
			if crw != nil {
				crw.WriteHeader(http.StatusBadGateway)
			} else if basicRw, ok := rw.(*responseWriter); ok {
				basicRw.statusCode = http.StatusBadGateway
			}
			http.Error(w, "Backend unavailable", http.StatusBadGateway)
		}

		// Serve the request
		proxy.ServeHTTP(rw, r)

		// Build request details if capture is enabled
		var details *RequestDetails
		var statusCode int
		if crw != nil {
			statusCode = crw.StatusCode()
			resBody, resHeaders := s.captureManager.CaptureResponse(requestID, crw)
			details = &RequestDetails{
				RequestHeaders:  reqHeaders,
				ResponseHeaders: resHeaders,
				RequestBody:     reqBody,
				ResponseBody:    resBody,
			}
		} else if basicRw, ok := rw.(*responseWriter); ok {
			statusCode = basicRw.statusCode
		} else {
			statusCode = http.StatusOK
		}

		// Record the request (single recording point for all cases)
		s.recordRequest(r, subdomain, statusCode, startTime, requestID, details)
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
func (s *Service) recordRequest(r *http.Request, subdomain string, statusCode int, startTime time.Time, requestID string, details *RequestDetails) {
	record := RequestRecord{
		ID:         requestID,
		Timestamp:  startTime,
		Method:     r.Method,
		URL:        r.URL.String(),
		Subdomain:  subdomain,
		StatusCode: statusCode,
		Duration:   time.Since(startTime),
		RemoteAddr: getClientIP(r),
		Details:    details,
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
