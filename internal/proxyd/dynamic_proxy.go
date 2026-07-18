package proxyd

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

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
)

// managedListener tracks a dynamically created port listener.
type managedListener struct {
	port     int
	protocol string
	listener net.Listener
	server   *http.Server
}

// DynamicProxy manages multiple HTTP/HTTPS listeners that can be added and
// removed at runtime as projects register and deregister routes.
type DynamicProxy struct {
	mu             sync.RWMutex
	listeners      map[int]*managedListener
	registry       *Registry
	transport      *http.Transport
	certMgr        *MultiDomainCertManager
	requestManager *proxy.RequestManager
	captureManager *proxy.CaptureManager
	logger         *slog.Logger
}

// NewDynamicProxy creates a new dynamic proxy. captureManager may be nil, in
// which case no request/response bodies are captured (metadata-only records);
// when non-nil, capture is further gated per route via Route.CaptureEnabled.
func NewDynamicProxy(registry *Registry, certMgr *MultiDomainCertManager, requestManager *proxy.RequestManager, captureManager *proxy.CaptureManager, logger *slog.Logger) *DynamicProxy {
	return &DynamicProxy{
		listeners:      make(map[int]*managedListener),
		registry:       registry,
		certMgr:        certMgr,
		requestManager: requestManager,
		captureManager: captureManager,
		logger:         logger,
		transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   constants.DefaultProxyDialTimeout,
				KeepAlive: constants.DefaultProxyKeepAlive,
			}).DialContext,
			MaxIdleConns:        constants.DefaultProxyMaxIdleConns,
			IdleConnTimeout:     constants.DefaultProxyIdleConnTimeout,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// AddListener binds a new port and starts serving proxy requests on it.
func (dp *DynamicProxy) AddListener(port int, protocol string) error {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	if _, exists := dp.listeners[port]; exists {
		return fmt.Errorf("listener already exists on port %d", port)
	}

	addr := fmt.Sprintf(":%d", port)
	handler := dp.handler(port)

	var ln net.Listener
	var err error

	if protocol == "https" {
		tlsCfg := &tls.Config{
			GetCertificate: dp.certMgr.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
		tcpLn, err2 := net.Listen("tcp", addr)
		if err2 != nil {
			return fmt.Errorf("binding port %d: %w", port, err2)
		}
		ln = tls.NewListener(tcpLn, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("binding port %d: %w", port, err)
		}
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: constants.DefaultProxyReadHeaderTimeout,
		IdleTimeout:       constants.DefaultProxyIdleTimeout,
	}

	ml := &managedListener{
		port:     port,
		protocol: protocol,
		listener: ln,
		server:   server,
	}

	dp.listeners[port] = ml
	dp.logger.Info("started listener", "port", port, "protocol", protocol)

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			dp.logger.Error("listener error", "port", port, "error", err)
		}
	}()

	return nil
}

// RemoveListener gracefully shuts down the listener on the given port.
func (dp *DynamicProxy) RemoveListener(port int) error {
	dp.mu.Lock()
	ml, ok := dp.listeners[port]
	if !ok {
		dp.mu.Unlock()
		return fmt.Errorf("no listener on port %d", port)
	}
	delete(dp.listeners, port)
	dp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dp.logger.Info("stopping listener", "port", port)
	return ml.server.Shutdown(ctx)
}

// Shutdown stops all listeners.
func (dp *DynamicProxy) Shutdown(ctx context.Context) error {
	dp.mu.Lock()
	all := make([]*managedListener, 0, len(dp.listeners))
	for _, ml := range dp.listeners {
		all = append(all, ml)
	}
	dp.listeners = make(map[int]*managedListener)
	dp.mu.Unlock()

	var lastErr error
	for _, ml := range all {
		if err := ml.server.Shutdown(ctx); err != nil {
			lastErr = err
			dp.logger.Error("error shutting down listener", "port", ml.port, "error", err)
		}
	}
	return lastErr
}

// handler returns an http.Handler that routes requests by full hostname lookup.
func (dp *DynamicProxy) handler(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		hostname := extractHostname(r.Host)

		route, ok := dp.registry.Lookup(hostname, port)
		if !ok {
			http.Error(w, fmt.Sprintf("no route for %s on port %d", hostname, port), http.StatusNotFound)
			return
		}

		// Capture is gated per project: only when the matched route opted in and
		// a capture manager is available and enabled.
		captureEnabled := route.CaptureEnabled && dp.captureManager != nil && dp.captureManager.Enabled()

		// Generate the request ID BEFORE proxying so capture files (written by
		// FinalizeResponse) are named consistently with the recorded record.
		requestID := ""
		if captureEnabled {
			requestID = proxy.GenerateRequestID(startTime, r.Method, r.URL.String())
		}

		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", route.Target.Host, route.Target.Port),
		}

		rp := httputil.NewSingleHostReverseProxy(target)
		rp.Transport = dp.transport
		// Flush immediately for streaming responses (SSE, chunked transfer)
		rp.FlushInterval = -1

		// Determine protocol from the listener
		proto := route.Protocol

		// Capture the request body and headers before the Director mutates them.
		// CaptureRequest clones the headers, so reqHeaders is unaffected by the
		// Director's later header sets (mirrors the in-process proxy path).
		var reqBody *proxy.CapturedBody
		var reqHeaders http.Header
		if captureEnabled {
			reqBody, r.Body, reqHeaders = dp.captureManager.CaptureRequest(requestID, r)
		}

		originalDirector := rp.Director
		rp.Director = func(req *http.Request) {
			originalDirector(req)
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", proto)
			req.Header.Set("X-Real-IP", getClientIP(r))
		}

		// Choose the response writer: a capturing writer when enabled, otherwise
		// the lightweight status-only writer used for metadata-only records.
		var rw http.ResponseWriter
		var crw *proxy.CaptureResponseWriter
		var srw *statusResponseWriter
		if captureEnabled {
			crw = dp.captureManager.WrapResponseWriter(w)
			rw = crw
		} else {
			srw = &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			rw = srw
		}

		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			dp.logger.Error("proxy error",
				"hostname", hostname,
				"target", target.String(),
				"error", err,
			)
			if crw != nil {
				crw.WriteHeader(http.StatusBadGateway)
			} else {
				srw.statusCode = http.StatusBadGateway
			}
			http.Error(w, "Backend unavailable", http.StatusBadGateway)
		}

		rp.ServeHTTP(rw, r)

		// Record the request
		if dp.requestManager != nil {
			// Extract subdomain for backward compat
			subdomain := ""
			if idx := strings.Index(hostname, "."); idx != -1 {
				subdomain = hostname[:idx]
			}

			// Finalize capture (if enabled) and derive the status code from the
			// active writer.
			var details *proxy.RequestDetails
			var statusCode int
			switch {
			case crw != nil && crw.Hijacked():
				// After a WebSocket upgrade all traffic bypassed the capture
				// writer; finalizing would record garbage (status 200, empty
				// body). Record metadata only, like a non-capture route.
				statusCode = crw.StatusCode()
			case crw != nil:
				statusCode = crw.StatusCode()
				// Freeze the request-body capture before publishing the record:
				// a canceled request's transport goroutine may still be draining
				// the wrapped body, and SSE subscribers serialize the record at
				// notify time.
				proxy.FinalizeRequestBody(r.Body)
				resBody, resHeaders := dp.captureManager.FinalizeResponse(requestID, crw)
				details = &proxy.RequestDetails{
					RequestHeaders:  reqHeaders,
					ResponseHeaders: resHeaders,
					RequestBody:     reqBody,
					ResponseBody:    resBody,
				}
			default:
				statusCode = srw.statusCode
			}

			dp.requestManager.Record(proxy.RequestRecord{
				ID:         requestID, // empty for non-capture routes; Record generates one
				Timestamp:  startTime,
				Method:     r.Method,
				URL:        r.URL.String(),
				Subdomain:  subdomain,
				Hostname:   hostname,
				ProjectDir: route.ProjectDir,
				StatusCode: statusCode,
				Duration:   time.Since(startTime),
				RemoteAddr: r.RemoteAddr,
				Details:    details,
			})
		}
	})
}

// statusResponseWriter wraps http.ResponseWriter to capture the status code.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher for streaming responses (SSE).
func (w *statusResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (w *statusResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController compatibility.
func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// extractHostname strips the port from a Host header value.
func extractHostname(host string) string {
	// Handle [IPv6]:port
	if i := strings.LastIndex(host, "]:"); i != -1 {
		return host[:i+1]
	}
	// Handle host:port
	if i := strings.LastIndex(host, ":"); i != -1 {
		return host[:i]
	}
	return host
}

// getClientIP extracts the client IP from the request.
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i != -1 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
