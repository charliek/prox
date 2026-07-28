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

	// Create shared transport for connection pooling.
	// ResponseHeaderTimeout bounds only the time waiting for backend response
	// headers — it does not affect body streaming (SSE, chunked, WebSocket)
	// once headers have been received.
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
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: constants.DefaultProxyReadHeaderTimeout,
		IdleTimeout:       constants.DefaultProxyIdleTimeout,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			return &PortConflictError{Port: s.cfg.HTTPPort, Protocol: "HTTP", Cause: err}
		}
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
		Addr:              addr,
		Handler:           router,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: constants.DefaultProxyReadHeaderTimeout,
		IdleTimeout:       constants.DefaultProxyIdleTimeout,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			return &PortConflictError{Port: s.cfg.HTTPSPort, Protocol: "HTTPS", Cause: err}
		}
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
		requestID := GenerateRequestID(startTime, r.Method, r.URL.String())

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
		// Flush immediately for streaming responses (SSE, chunked transfer)
		proxy.FlushInterval = -1

		// Capture request body and headers if capture is enabled
		var reqBody *CapturedBody
		var reqHeaders http.Header
		if s.captureManager != nil && s.captureManager.Enabled() {
			// A 0 cap keeps the manager's configured max_body_size (effectiveLimit).
			reqBody, r.Body, reqHeaders = s.captureManager.CaptureRequest(requestID, r, 0)
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
		var crw *CaptureResponseWriter
		var brw *responseWriter
		if s.captureManager != nil && s.captureManager.Enabled() {
			crw = s.captureManager.WrapResponseWriter(w, 0)
			rw = crw
		} else {
			brw = &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			rw = brw
		}

		// Custom error handler - log detailed error but return generic message to client
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Error("proxy error",
				"subdomain", subdomain,
				"target", target.String(),
				"error", err,
			)
			// http.Error writes the 502 through the wrapped writer (latching
			// the status and firing the first-response hook); an explicit
			// WriteHeader first would commit the response before http.Error
			// could set its error headers.
			http.Error(w, "Backend unavailable", http.StatusBadGateway)
		}

		// Phase 1 (in-flight): register a first-response hook that publishes a
		// header-time record via Record (plain append — the fresh ID cannot
		// already exist). Field parity with the completion record is pinned:
		// buildRecord computes every field identically; only StatusCode source
		// (the hook arg), InFlight, Duration, and Details differ.
		onFirst := func(statusCode int) {
			s.requestManager.Record(s.buildRecord(r, subdomain, statusCode, startTime, requestID, nil, true))
		}
		if crw != nil {
			crw.SetFirstResponseCallback(onFirst)
		} else {
			brw.SetFirstResponseCallback(onFirst)
		}

		// Phase 2 (completion): the finalize-freeze + record build runs in a
		// defer registered BEFORE ServeHTTP with NO recover, so it executes on
		// normal return AND while unwinding ReverseProxy's http.ErrAbortHandler
		// panic (client disconnect / backend death mid-stream). The panic keeps
		// propagating. Completion goes through Upsert (same ID as the in-flight
		// record; final beats it).
		defer func() {
			// Build request details if capture is enabled
			var details *RequestDetails
			var statusCode int
			switch {
			case crw != nil && crw.Hijacked():
				// A successful reverse-proxy upgrade writes the 101 raw to the
				// hijacked conn, bypassing WriteHeader, so the writer's
				// statusCode is meaningless here. Record the protocol switch and
				// record metadata only rather than a garbage 200/empty-body
				// capture.
				statusCode = http.StatusSwitchingProtocols
				// The metadata-only record carries no Details, so eviction would
				// never clean a request-body file spilled to disk before the
				// upgrade; finalize and clean it up here to avoid orphaning it.
				FinalizeRequestBody(r.Body)
				s.captureManager.CleanupRequest(requestID)
			case crw != nil:
				statusCode = crw.StatusCode()
				// Freeze the request-body capture before the record is published
				// (see FinalizeRequestBody).
				FinalizeRequestBody(r.Body)
				resBody, resHeaders := s.captureManager.FinalizeResponse(requestID, crw)
				details = &RequestDetails{
					RequestHeaders:  reqHeaders,
					ResponseHeaders: resHeaders,
					RequestBody:     reqBody,
					ResponseBody:    resBody,
				}
			case brw.Hijacked():
				// Non-capture WebSocket upgrade: record 101 to match the
				// in-flight record and the capture path (the writer's default
				// 200 would otherwise disagree).
				statusCode = http.StatusSwitchingProtocols
			default:
				statusCode = brw.statusCode
			}

			// Completion recording point (Upsert: final beats the in-flight copy)
			s.requestManager.Upsert(s.buildRecord(r, subdomain, statusCode, startTime, requestID, details, false))
		}()

		// Serve the request
		proxy.ServeHTTP(rw, r)
	})
}

// stripHostPort removes a trailing ":port" from a host header value, if
// present. Shared by extractSubdomain and recordRequest so hostname handling
// stays consistent. Uses net.SplitHostPort so IPv6 literals survive: a bare
// "[::1]" has no port and is returned unchanged rather than mangled at its
// last colon.
func stripHostPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// extractSubdomain extracts the subdomain from the host header.
// For example, "app.local.myapp.dev:6789" with domain "local.myapp.dev" returns "app".
func (s *Service) extractSubdomain(host string) string {
	// Remove port if present
	host = stripHostPort(host)

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

// buildRecord assembles a RequestRecord from a request and derived fields. It is
// the single field-parity point for the two-phase recording: the in-flight and
// completion records for one request differ ONLY in inFlight (which zeroes
// Duration and forces nil-carrying InFlight), the StatusCode source, and
// Details. Every other field is this one expression.
func (s *Service) buildRecord(r *http.Request, subdomain string, statusCode int, startTime time.Time, requestID string, details *RequestDetails, inFlight bool) RequestRecord {
	duration := time.Duration(0)
	if !inFlight {
		duration = time.Since(startTime)
	}
	return RequestRecord{
		ID:        requestID,
		Timestamp: startTime,
		Method:    r.Method,
		// The stored URL is the request URL verbatim; r.URL is never mutated, so
		// the reverse proxy still forwards the byte-identical upstream URL.
		URL:        r.URL.String(),
		Subdomain:  subdomain,
		Hostname:   stripHostPort(r.Host),
		StatusCode: statusCode,
		Duration:   duration,
		RemoteAddr: getClientIP(r),
		InFlight:   inFlight,
		Details:    details,
	}
}

// recordRequest records a final (non-in-flight) request via Record. Used by the
// early-routing 404 paths, which never proxy the request and so have no
// in-flight phase; the two-phase proxy path records completions via Upsert.
func (s *Service) recordRequest(r *http.Request, subdomain string, statusCode int, startTime time.Time, requestID string, details *RequestDetails) {
	s.requestManager.Record(s.buildRecord(r, subdomain, statusCode, startTime, requestID, details, false))
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

// firstResponseHook holds the first-response callback machinery shared by the
// package's response writers (CaptureResponseWriter and responseWriter). It
// latches so the callback fires exactly once at the first FINAL response event
// (see fireFirstResponse). Single-goroutine access per the http.ResponseWriter
// contract; the callback is set before serving.
type firstResponseHook struct {
	onFirstResponse func(statusCode int)
	responded       bool
}

// SetFirstResponseCallback registers a callback that fires exactly once at the
// first FINAL response event (see fireFirstResponse).
func (h *firstResponseHook) SetFirstResponseCallback(fn func(statusCode int)) {
	h.onFirstResponse = fn
}

// fireFirstResponse invokes the first-response callback at most once. It is
// driven ONLY from WriteHeader (final status) and a successful Hijack — never
// from an implicit bare Write. The reverse proxy always calls WriteHeader
// explicitly for normal responses and hijacks for upgrades, so no response
// bytes reach the wire before the hook has run.
func (h *firstResponseHook) fireFirstResponse(code int) {
	if h.responded {
		return
	}
	h.responded = true
	if h.onFirstResponse != nil {
		h.onFirstResponse(code)
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	hijacked   bool
	firstResponseHook
}

func (rw *responseWriter) WriteHeader(code int) {
	// Latch the FIRST final (>=200) status — net/http ignores later
	// WriteHeader calls, so the first one is what the client actually got.
	// 1xx provisional responses (e.g. 103 Early Hints) are delegated but are
	// not the final status: they neither latch nor fire the hook.
	if code >= 200 && !rw.responded {
		rw.statusCode = code
		// Order: latch status → invoke callback → delegate.
		rw.fireFirstResponse(code)
	}
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
		conn, brw, err := h.Hijack()
		if err == nil {
			rw.hijacked = true
			// A successful upgrade never calls WriteHeader (the 101 is written
			// raw to the hijacked conn), so fire the hook here with 101.
			rw.fireFirstResponse(http.StatusSwitchingProtocols)
		}
		return conn, brw, err
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Hijacked reports whether the connection was taken over (WebSocket upgrade).
func (rw *responseWriter) Hijacked() bool {
	return rw.hijacked
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
