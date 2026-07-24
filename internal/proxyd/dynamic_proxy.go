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

// certManager is the cert capability the dynamic proxy and register flow need:
// per-domain cert generation plus SNI cert selection.
type certManager interface {
	EnsureDomain(domain string) error
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

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
	mu        sync.RWMutex
	listeners map[int]*managedListener
	registry  *Registry
	transport *http.Transport
	certMgr   certManager
	// managers holds the per-project request rings (D13, #49). The hot path
	// resolves the matched route's ring via managers.get(route.ProjectDir): a nil
	// result (a request completing after its project deregistered) drops the
	// record safely. The set is shared with the Server so the data plane and the
	// control-plane endpoints see the identical rings.
	managers       *Managers
	captureManager *proxy.CaptureManager
	logger         *slog.Logger
}

// NewDynamicProxy creates a new dynamic proxy. captureManager may be nil, in
// which case no request/response bodies are captured (metadata-only records);
// when non-nil, capture is further gated per route via Route.CaptureEnabled.
//
// certMgr must be either a real *MultiDomainCertManager or the untyped nil
// literal: the HTTPS bind path and the register flow gate on certMgr != nil, so
// a typed nil such as (*MultiDomainCertManager)(nil) would slip past that guard
// (a non-nil interface wrapping a nil pointer) and panic on the first HTTPS bind.
func NewDynamicProxy(registry *Registry, certMgr certManager, managers *Managers, captureManager *proxy.CaptureManager, logger *slog.Logger) *DynamicProxy {
	return &DynamicProxy{
		listeners:      make(map[int]*managedListener),
		registry:       registry,
		certMgr:        certMgr,
		managers:       managers,
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
		// Guard before dereferencing dp.certMgr: a nil interface here would
		// otherwise produce a method-value panic when the TLS stack invokes
		// GetCertificate on the first handshake.
		if dp.certMgr == nil {
			return fmt.Errorf("cannot start https listener on port %d: no certificate manager configured", port)
		}
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
		// FinalizeResponse) are named consistently with the recorded record,
		// AND so the in-flight and completion records share an ID on every
		// route (capture or not).
		requestID := proxy.GenerateRequestID(startTime, r.Method, r.URL.String())

		// Extract subdomain for backward compat (computed before ServeHTTP so
		// the in-flight and completion records share the expression).
		subdomain := ""
		if idx := strings.Index(hostname, "."); idx != -1 {
			subdomain = hostname[:idx]
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
			// Per-project capture cap (D13, #49): the matched route carries the
			// owning project's configured MaxBodySize; 0 means the daemon default.
			reqBody, r.Body, reqHeaders = dp.captureManager.CaptureRequestWithLimit(requestID, r, route.MaxBodySize)
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
			crw = dp.captureManager.WrapResponseWriterWithLimit(w, route.MaxBodySize)
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
			// http.Error writes the 502 through the wrapped writer (latching
			// the status and firing the first-response hook); an explicit
			// WriteHeader first would commit the response before http.Error
			// could set its error headers.
			http.Error(w, "Backend unavailable", http.StatusBadGateway)
		}

		// buildRecord is the single field-parity point for the two-phase
		// recording: the in-flight and completion records for one request differ
		// ONLY in inFlight (which zeroes Duration), the StatusCode source, and
		// Details. Every other field is this one expression.
		buildRecord := func(statusCode int, details *proxy.RequestDetails, inFlight bool) proxy.RequestRecord {
			duration := time.Duration(0)
			if !inFlight {
				duration = time.Since(startTime)
			}
			return proxy.RequestRecord{
				ID:         requestID,
				Timestamp:  startTime,
				Method:     r.Method,
				URL:        r.URL.String(),
				Subdomain:  subdomain,
				Hostname:   hostname,
				ProjectDir: route.ProjectDir,
				StatusCode: statusCode,
				Duration:   duration,
				RemoteAddr: r.RemoteAddr,
				InFlight:   inFlight,
				Details:    details,
			}
		}

		// Phase 1 (in-flight): register a first-response hook that publishes a
		// header-time record via Record (plain append — the fresh ID cannot
		// already exist). buildRecord pins field parity with the completion
		// record below. The hook re-resolves the project's ring at fire time
		// (managers.get): if the project deregistered before the first response,
		// the ring is gone and the in-flight record is dropped safely.
		if dp.managers != nil {
			onFirst := func(statusCode int) {
				if mgr := dp.managers.get(route.ProjectDir); mgr != nil {
					mgr.Record(buildRecord(statusCode, nil, true))
				}
			}
			if crw != nil {
				crw.SetFirstResponseCallback(onFirst)
			} else {
				srw.SetFirstResponseCallback(onFirst)
			}
		}

		// Phase 2 (completion): the finalize-freeze + record build runs in a
		// defer registered BEFORE ServeHTTP with NO recover, so it executes on
		// normal return AND while unwinding ReverseProxy's http.ErrAbortHandler
		// panic (client disconnect / backend death mid-stream). The panic keeps
		// propagating; net/http suppresses ErrAbortHandler. Completion goes
		// through Upsert (same ID as the in-flight record; final beats it).
		defer func() {
			if dp.managers == nil {
				return
			}

			// Finalize capture (if enabled) and derive the status code from the
			// active writer.
			var details *proxy.RequestDetails
			var statusCode int
			switch {
			case crw != nil && crw.Hijacked():
				// After a WebSocket upgrade all traffic bypassed the capture
				// writer; finalizing would record garbage (status 200, empty
				// body). The successful upgrade writes the 101 raw to the
				// hijacked conn, so record the protocol switch and keep
				// metadata only, like a non-capture route.
				statusCode = http.StatusSwitchingProtocols
				// The metadata-only record carries no Details, so eviction
				// would never clean a request-body file spilled to disk before
				// the upgrade; finalize and clean it up here to avoid orphaning
				// it.
				proxy.FinalizeRequestBody(r.Body)
				dp.captureManager.CleanupRequest(requestID)
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
			case srw.Hijacked():
				// Non-capture WebSocket upgrade: record 101 to match the
				// in-flight record and the capture path (the writer's default
				// 200 would otherwise disagree).
				statusCode = http.StatusSwitchingProtocols
			default:
				statusCode = srw.statusCode
			}

			// Re-resolve the project's ring at completion time. A nil result
			// means the project deregistered while this request was in flight
			// (its route was resolved before the deregister, its completion
			// arrives after): drop the completion safely — no record, no panic —
			// and clean any capture files this request spilled to disk, since no
			// PurgeByProject on the removed ring will ever reach them (D13).
			// A nil manager (project deregistered mid-request) and a rejected
			// Upsert (manager Closed between the get and the write) both mean
			// this completion has no ring: clean the request's spilled capture
			// files, since no purge on the destroyed ring will reach them (D13).
			mgr := dp.managers.get(route.ProjectDir)
			if mgr == nil || !mgr.Upsert(buildRecord(statusCode, details, false)) {
				if captureEnabled {
					dp.captureManager.CleanupRequest(requestID)
				}
				return
			}
		}()

		rp.ServeHTTP(rw, r)
	})
}

// statusResponseWriter wraps http.ResponseWriter to capture the status code.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	hijacked    bool

	// onFirstResponse, if set, fires exactly once at the first final response
	// event — see fireFirstResponse. responded latches so it can never re-fire.
	onFirstResponse func(statusCode int)
	responded       bool
}

// SetFirstResponseCallback registers a callback that fires exactly once at the
// first FINAL response event (see fireFirstResponse). Single-goroutine access
// per the http.ResponseWriter contract; set before serving.
func (w *statusResponseWriter) SetFirstResponseCallback(fn func(statusCode int)) {
	w.onFirstResponse = fn
}

// fireFirstResponse invokes the first-response callback at most once. It is
// driven ONLY from WriteHeader (final status) and a successful Hijack — never
// from an implicit bare Write. The reverse proxy always calls WriteHeader
// explicitly for normal responses and hijacks for upgrades.
func (w *statusResponseWriter) fireFirstResponse(code int) {
	if w.responded {
		return
	}
	w.responded = true
	if w.onFirstResponse != nil {
		w.onFirstResponse(code)
	}
}

func (w *statusResponseWriter) WriteHeader(code int) {
	// 1xx provisional responses (e.g. 103 Early Hints) are not the final
	// status: they neither latch the recorded status nor fire the hook.
	if code >= 200 {
		if !w.wroteHeader {
			w.statusCode = code
			w.wroteHeader = true
		}
		// Order: latch status → invoke callback → delegate.
		w.fireFirstResponse(code)
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
		conn, rw, err := h.Hijack()
		if err == nil {
			w.hijacked = true
			// A successful upgrade never calls WriteHeader (the 101 is written
			// raw to the hijacked conn), so fire the hook here with 101.
			w.fireFirstResponse(http.StatusSwitchingProtocols)
		}
		return conn, rw, err
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Hijacked reports whether the connection was taken over (WebSocket upgrade).
func (w *statusResponseWriter) Hijacked() bool {
	return w.hijacked
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
