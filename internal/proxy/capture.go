// Package proxy provides an HTTPS reverse proxy with subdomain-based routing.
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
)

// CaptureManager handles request/response body capture with hybrid memory/disk storage.
type CaptureManager struct {
	mu              sync.RWMutex
	enabled         bool
	maxBodySize     int64
	inlineThreshold int64
	captureDir      string
	workDir         string
}

// NewCaptureManager creates a new capture manager.
// If cfg is nil or capture is not enabled, returns a manager that does nothing.
//
// This constructor treats workDir as a WORK directory: the capture directory is
// derived as workDir/.prox/capture. Callers that already hold an exact capture
// directory (e.g. the shared daemon, whose capture dir is ~/.prox/capture) must
// use NewCaptureManagerAt instead to avoid a doubled ".prox/capture" suffix.
func NewCaptureManager(cfg *config.CaptureConfig, workDir string) (*CaptureManager, error) {
	if cfg == nil || !cfg.Enabled {
		return &CaptureManager{
			workDir:         workDir,
			enabled:         false,
			maxBodySize:     constants.DefaultCaptureMaxBodySize,
			inlineThreshold: constants.DefaultCaptureInlineThreshold,
		}, nil
	}

	maxBodySize := int64(constants.DefaultCaptureMaxBodySize)

	// Parse max body size if configured
	if cfg.MaxBodySize != "" {
		size, err := config.ParseSize(cfg.MaxBodySize)
		if err != nil {
			return nil, err
		}
		if size > 0 {
			maxBodySize = size
		}
	}

	captureDir := filepath.Join(workDir, constants.CaptureDirectory)
	cm, err := NewCaptureManagerAt(captureDir, maxBodySize)
	if err != nil {
		return nil, err
	}
	cm.workDir = workDir
	return cm, nil
}

// NewCaptureManagerAt creates an enabled capture manager rooted at an EXACT
// capture directory (no ".prox/capture" suffix is appended). It is the shared
// setup that NewCaptureManager delegates to once it has resolved the capture
// directory and body-size limit. Any existing files under captureDir are removed
// (previous-run cleanup) and the directory is created.
func NewCaptureManagerAt(captureDir string, maxBodySize int64) (*CaptureManager, error) {
	if maxBodySize <= 0 {
		maxBodySize = constants.DefaultCaptureMaxBodySize
	}

	cm := &CaptureManager{
		enabled:         true,
		maxBodySize:     maxBodySize,
		inlineThreshold: constants.DefaultCaptureInlineThreshold,
		captureDir:      captureDir,
	}

	// Clean up any existing capture files from a previous run.
	if err := cm.Cleanup(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Create capture directory.
	if err := os.MkdirAll(cm.captureDir, constants.DirPermissionPrivate); err != nil {
		return nil, err
	}

	return cm, nil
}

// CaptureDir returns the directory where captured body files are stored, or the
// empty string when capture is disabled. Used by consumers building the
// LoadCapturedBody allowlist.
func (cm *CaptureManager) CaptureDir() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.captureDir
}

// Enabled returns whether capture is enabled.
func (cm *CaptureManager) Enabled() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.enabled
}

// effectiveLimit resolves a per-call capture cap: a positive limit is used
// verbatim, and 0 (or negative) falls back to the manager's configured
// maxBodySize (which itself defaults to DefaultCaptureMaxBodySize). The daemon
// passes each route's own cap here (D13, #49); the standalone in-process proxy
// passes 0, keeping the project's configured cap. maxBodySize is set once at
// construction and never mutated, so no lock is needed.
func (cm *CaptureManager) effectiveLimit(limit int64) int64 {
	if limit > 0 {
		return limit
	}
	return cm.maxBodySize
}

// CaptureRequest captures the request body using the manager's configured cap.
// It is CaptureRequestWithLimit with a 0 (manager-default) limit — the signature
// the standalone in-process proxy uses.
func (cm *CaptureManager) CaptureRequest(requestID string, r *http.Request) (*CapturedBody, io.ReadCloser, http.Header) {
	return cm.CaptureRequestWithLimit(requestID, r, 0)
}

// CaptureRequestWithLimit captures the request body using a TeeReader bounded by
// a per-call cap (D13, #49): the daemon passes the matched route's MaxBodySize so
// each project honors its own quota through the one shared capture dir. A limit
// of 0 falls back to the manager's configured cap (see effectiveLimit).
// Returns the captured body info and a new ReadCloser to use in place of the original body.
// The original body is wrapped so that reading from the returned ReadCloser also captures the data.
func (cm *CaptureManager) CaptureRequestWithLimit(requestID string, r *http.Request, limit int64) (*CapturedBody, io.ReadCloser, http.Header) {
	if !cm.enabled || r.Body == nil {
		return nil, r.Body, cloneHeaders(r.Header)
	}

	headers := cloneHeaders(r.Header)
	contentType := r.Header.Get("Content-Type")

	// Create a buffer to capture the body
	captured := &captureBuffer{
		maxSize:   cm.effectiveLimit(limit),
		requestID: requestID,
		suffix:    "_req",
		cm:        cm,
	}

	// Wrap the body with a TeeReader
	teeReader := io.TeeReader(r.Body, captured)
	wrappedBody := &captureReadCloser{
		Reader:   teeReader,
		Closer:   r.Body,
		captured: captured,
	}

	// We return a placeholder body info; the actual data will be filled after reading completes
	body := &CapturedBody{
		ContentType:     contentType,
		ContentEncoding: r.Header.Get("Content-Encoding"),
	}

	captured.body = body
	return body, wrappedBody, headers
}

// WrapResponseWriter wraps w in a CaptureResponseWriter bounded by the manager's
// configured max body size. It is WrapResponseWriterWithLimit with a 0
// (manager-default) limit — the signature the standalone in-process proxy uses.
func (cm *CaptureManager) WrapResponseWriter(w http.ResponseWriter) *CaptureResponseWriter {
	return cm.WrapResponseWriterWithLimit(w, 0)
}

// WrapResponseWriterWithLimit wraps w in a CaptureResponseWriter that records up
// to a per-call cap (D13, #49) while forwarding all writes downstream: the daemon
// passes the matched route's MaxBodySize so each project honors its own quota. A
// limit of 0 falls back to the manager's configured cap (see effectiveLimit).
// The returned writer preserves http.Flusher/Hijacker/Pusher/Unwrap behavior.
func (cm *CaptureManager) WrapResponseWriterWithLimit(w http.ResponseWriter, limit int64) *CaptureResponseWriter {
	return newCaptureResponseWriter(w, cm.effectiveLimit(limit))
}

// FinalizeResponse captures the response body from a CaptureResponseWriter.
// Should be called after the response has been fully written.
func (cm *CaptureManager) FinalizeResponse(requestID string, crw *CaptureResponseWriter) (*CapturedBody, http.Header) {
	if !cm.enabled {
		return nil, cloneHeaders(crw.Header())
	}

	headers := cloneHeaders(crw.Header())
	contentType := crw.Header().Get("Content-Type")
	data := crw.CapturedBody()

	body := &CapturedBody{
		Size:            crw.TotalSeen(),
		CapturedSize:    int64(len(data)),
		Truncated:       crw.Truncated(),
		ContentType:     contentType,
		ContentEncoding: crw.Header().Get("Content-Encoding"),
		IsBinary:        isBinaryContent(data, contentType),
	}

	// Determine if we should store inline or on disk
	if int64(len(data)) <= cm.inlineThreshold {
		body.Data = data
	} else {
		// Store on disk
		filePath := filepath.Join(cm.captureDir, requestID+"_res.bin")
		if err := os.WriteFile(filePath, data, constants.FilePermissionPrivate); err == nil {
			body.FilePath = filePath
		} else {
			// Fall back to inline if disk write fails
			body.Data = data
		}
	}

	return body, headers
}

// LoadBody loads a captured body's data, reading from disk if necessary.
// Returns a copy of the data to prevent callers from modifying the original.
// FilePath bodies are constrained to the manager's own capture directory via
// LoadCapturedBody's allowlist.
func (cm *CaptureManager) LoadBody(body *CapturedBody) ([]byte, error) {
	return LoadCapturedBody(body, []string{cm.CaptureDir()})
}

// CleanupRequest removes disk files associated with a specific request.
func (cm *CaptureManager) CleanupRequest(requestID string) {
	if !cm.enabled || cm.captureDir == "" {
		return
	}

	// Remove both request and response body files
	_ = os.Remove(filepath.Join(cm.captureDir, requestID+"_req.bin"))
	_ = os.Remove(filepath.Join(cm.captureDir, requestID+"_res.bin"))
}

// Cleanup removes the entire capture directory.
func (cm *CaptureManager) Cleanup() error {
	if cm.captureDir == "" {
		return nil
	}
	return os.RemoveAll(cm.captureDir)
}

// captureBuffer is a write buffer that captures up to maxSize bytes.
// It is safe for concurrent use via the embedded mutex.
type captureBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	maxSize   int64
	truncated bool
	finalized bool
	totalSeen int64 // total bytes observed across all writes, counting past truncation
	requestID string
	suffix    string
	cm        *CaptureManager
	body      *CapturedBody
}

func (cb *captureBuffer) Write(p []byte) (n int, err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Once finalized, the CapturedBody snapshot is frozen: late writes from a
	// transport goroutine still draining a canceled request must not mutate
	// state that a recorded (and possibly already-serialized) body points at.
	if cb.finalized {
		return len(p), nil
	}

	// Count every byte observed, including data discarded after truncation.
	cb.totalSeen += int64(len(p))

	if cb.truncated || len(p) == 0 {
		return len(p), nil // Discard but pretend we wrote it
	}

	remaining := cb.maxSize - int64(cb.buf.Len())
	if remaining <= 0 {
		cb.truncated = true
		return len(p), nil
	}

	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
		cb.truncated = true
	}

	n, err = cb.buf.Write(toWrite)
	if err != nil {
		return n, err
	}

	// Return full length even if we truncated
	return len(p), nil
}

func (cb *captureBuffer) finalize() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.body == nil || cb.finalized {
		return nil
	}
	cb.finalized = true

	data := cb.buf.Bytes()
	cb.body.Size = cb.totalSeen
	cb.body.CapturedSize = int64(len(data))
	cb.body.Truncated = cb.truncated
	cb.body.IsBinary = isBinaryContent(data, cb.body.ContentType)

	// Determine storage location
	if int64(len(data)) <= cb.cm.inlineThreshold {
		cb.body.Data = data
		return nil
	}

	if cb.cm.captureDir != "" {
		// Store on disk
		filePath := filepath.Join(cb.cm.captureDir, cb.requestID+cb.suffix+".bin")
		if err := os.WriteFile(filePath, data, constants.FilePermissionPrivate); err != nil {
			// Fall back to inline if disk write fails, but return error for caller awareness
			cb.body.Data = data
			return fmt.Errorf("failed to write capture file %s: %w", filePath, err)
		}
		cb.body.FilePath = filePath
		return nil
	}

	cb.body.Data = data
	return nil
}

// captureReadCloser wraps a reader to finalize capture when closed.
// It combines a TeeReader with the original body's Closer, ensuring that
// captured data is finalized (written to disk or stored inline) when the
// request body is closed.
type captureReadCloser struct {
	io.Reader
	io.Closer
	captured *captureBuffer
}

func (crc *captureReadCloser) Close() error {
	// Finalize the capture
	if crc.captured != nil {
		if err := crc.captured.finalize(); err != nil {
			// Log the error but don't fail the close - the data is still captured inline
			log.Printf("Warning: capture finalize failed: %v", err)
		}
	}
	return crc.Closer.Close()
}

// FinalizeRequestBody forces finalization of a request body previously wrapped
// by CaptureRequest. Idempotent; a non-wrapped body is a no-op. Proxy handlers
// call this after the reverse proxy returns and BEFORE recording, so the
// CapturedBody snapshot is complete when the record is published (SSE
// subscribers serialize records at notify time). Without it, a canceled
// request's transport goroutine may still be draining the body, and its later
// Close-triggered finalize would race the serialization; after this call that
// finalize is a no-op and late writes are discarded.
func FinalizeRequestBody(rc io.ReadCloser) {
	if crc, ok := rc.(*captureReadCloser); ok && crc.captured != nil {
		if err := crc.captured.finalize(); err != nil {
			log.Printf("Warning: capture finalize failed: %v", err)
		}
	}
}

// CaptureResponseWriter wraps an http.ResponseWriter to capture the response body.
// It intercepts writes to capture up to maxBodySize bytes while still forwarding
// all data to the underlying ResponseWriter. It also implements http.Flusher,
// http.Hijacker, and http.Pusher for compatibility with streaming and WebSocket
// connections.
type CaptureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	body        bytes.Buffer
	maxBodySize int64
	truncated   bool
	wroteHeader bool
	hijacked    bool
	totalSeen   int64 // total bytes observed across all writes, counting past truncation

	// firstResponseHook fires the registered callback once at the first final
	// response event (see fireFirstResponse).
	firstResponseHook
}

// newCaptureResponseWriter creates a new capturing response writer.
func newCaptureResponseWriter(w http.ResponseWriter, maxBodySize int64) *CaptureResponseWriter {
	return &CaptureResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		maxBodySize:    maxBodySize,
	}
}

func (crw *CaptureResponseWriter) WriteHeader(code int) {
	// 1xx provisional responses (e.g. 103 Early Hints) are not the final
	// status: they neither latch the recorded status nor fire the hook.
	// ReverseProxy may forward them via WriteHeader before the real status.
	if code >= 200 {
		if !crw.wroteHeader {
			crw.statusCode = code
			crw.wroteHeader = true
		}
		// Order: latch status → invoke callback → delegate, so the in-flight
		// record exists before any response bytes hit the wire.
		crw.fireFirstResponse(code)
	}
	crw.ResponseWriter.WriteHeader(code)
}

func (crw *CaptureResponseWriter) Write(p []byte) (int, error) {
	// Count every byte observed, including data not retained after truncation.
	crw.totalSeen += int64(len(p))

	// Capture up to maxBodySize
	if !crw.truncated && len(p) > 0 {
		remaining := crw.maxBodySize - int64(crw.body.Len())
		if remaining > 0 {
			toCapture := p
			if int64(len(p)) > remaining {
				toCapture = p[:remaining]
				crw.truncated = true
			}
			crw.body.Write(toCapture)
		} else {
			crw.truncated = true
		}
	}

	return crw.ResponseWriter.Write(p)
}

// StatusCode returns the captured status code.
func (crw *CaptureResponseWriter) StatusCode() int {
	return crw.statusCode
}

// CapturedBody returns the captured response body.
func (crw *CaptureResponseWriter) CapturedBody() []byte {
	return crw.body.Bytes()
}

// Truncated returns whether the body was truncated.
func (crw *CaptureResponseWriter) Truncated() bool {
	return crw.truncated
}

// TotalSeen returns the total number of bytes observed by Write, counting
// bytes that were not retained after truncation.
func (crw *CaptureResponseWriter) TotalSeen() int64 {
	return crw.totalSeen
}

// Flush implements http.Flusher for streaming responses (SSE).
func (crw *CaptureResponseWriter) Flush() {
	if f, ok := crw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (crw *CaptureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := crw.ResponseWriter.(http.Hijacker); ok {
		conn, rw, err := h.Hijack()
		if err == nil {
			crw.hijacked = true
			// A successful upgrade never calls WriteHeader (the 101 is written
			// raw to the hijacked conn), so fire the hook here with 101.
			crw.fireFirstResponse(http.StatusSwitchingProtocols)
		}
		return conn, rw, err
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Hijacked reports whether the connection was taken over (WebSocket upgrade).
// After a hijack all traffic bypasses this writer, so the captured status/body
// do not describe the response — callers should record metadata only rather
// than finalize garbage Details. Single-goroutine access per the
// http.ResponseWriter contract.
func (crw *CaptureResponseWriter) Hijacked() bool {
	return crw.hijacked
}

// Push implements http.Pusher for HTTP/2 server push.
func (crw *CaptureResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := crw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter for Go 1.20+ http.ResponseController compatibility.
func (crw *CaptureResponseWriter) Unwrap() http.ResponseWriter {
	return crw.ResponseWriter
}

// cloneHeaders creates a shallow copy of HTTP headers.
func cloneHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	clone := make(http.Header, len(h))
	for k, v := range h {
		clone[k] = v
	}
	return clone
}

// isBinaryContent determines if content appears to be binary based on data and
// content type.
//
// Integrity-first rule (D9): content is never classified as text unless the
// COMPLETE retained data is valid UTF-8. Known-binary content types are always
// binary; a text-y Content-Type never short-circuits to text — data validity
// decides. The full-buffer scan (no 512-byte sampling) is bounded by the 1MB
// capture cap.
func isBinaryContent(data []byte, contentType string) bool {
	// Known-binary content types are binary regardless of the data.
	if contentType != "" {
		ct := strings.ToLower(contentType)
		if strings.HasPrefix(ct, "image/") ||
			strings.HasPrefix(ct, "audio/") ||
			strings.HasPrefix(ct, "video/") ||
			strings.Contains(ct, "octet-stream") ||
			strings.Contains(ct, "zip") ||
			strings.Contains(ct, "gzip") ||
			strings.Contains(ct, "tar") ||
			strings.Contains(ct, "pdf") {
			return true
		}
	}

	// Empty data is not binary.
	if len(data) == 0 {
		return false
	}

	// The entire retained buffer must be valid UTF-8 to be considered text.
	if !utf8.Valid(data) {
		return true
	}

	// Scan the entire buffer for non-printable control characters.
	// Allow common control characters: tab, newline, carriage return.
	for _, b := range data {
		if b < 32 && b != '\t' && b != '\n' && b != '\r' {
			return true
		}
	}

	return false
}
