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
func NewCaptureManager(cfg *config.CaptureConfig, workDir string) (*CaptureManager, error) {
	cm := &CaptureManager{
		workDir:         workDir,
		maxBodySize:     constants.DefaultCaptureMaxBodySize,
		inlineThreshold: constants.DefaultCaptureInlineThreshold,
	}

	if cfg == nil || !cfg.Enabled {
		cm.enabled = false
		return cm, nil
	}

	cm.enabled = true

	// Parse max body size if configured
	if cfg.MaxBodySize != "" {
		size, err := config.ParseSize(cfg.MaxBodySize)
		if err != nil {
			return nil, err
		}
		if size > 0 {
			cm.maxBodySize = size
		}
	}

	// Set up capture directory
	cm.captureDir = filepath.Join(workDir, constants.CaptureDirectory)

	// Clean up any existing capture files from previous run
	if err := cm.Cleanup(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Create capture directory
	if err := os.MkdirAll(cm.captureDir, constants.DirPermissionPrivate); err != nil {
		return nil, err
	}

	return cm, nil
}

// Enabled returns whether capture is enabled.
func (cm *CaptureManager) Enabled() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.enabled
}

// CaptureRequest captures the request body using a TeeReader.
// Returns the captured body info and a new ReadCloser to use in place of the original body.
// The original body is wrapped so that reading from the returned ReadCloser also captures the data.
func (cm *CaptureManager) CaptureRequest(requestID string, r *http.Request) (*CapturedBody, io.ReadCloser, http.Header) {
	if !cm.enabled || r.Body == nil {
		return nil, r.Body, cloneHeaders(r.Header)
	}

	headers := cloneHeaders(r.Header)
	contentType := r.Header.Get("Content-Type")

	// Create a buffer to capture the body
	captured := &captureBuffer{
		maxSize:   cm.maxBodySize,
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
		ContentType: contentType,
	}

	captured.body = body
	return body, wrappedBody, headers
}

// CaptureResponse captures the response body from a capturingResponseWriter.
// Should be called after the response has been fully written.
func (cm *CaptureManager) CaptureResponse(requestID string, crw *capturingResponseWriter) (*CapturedBody, http.Header) {
	if !cm.enabled {
		return nil, cloneHeaders(crw.Header())
	}

	headers := cloneHeaders(crw.Header())
	contentType := crw.Header().Get("Content-Type")
	data := crw.CapturedBody()

	body := &CapturedBody{
		Size:        int64(len(data)),
		Truncated:   crw.Truncated(),
		ContentType: contentType,
		IsBinary:    isBinaryContent(data, contentType),
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
func (cm *CaptureManager) LoadBody(body *CapturedBody) ([]byte, error) {
	if body == nil {
		return nil, nil
	}

	if body.Data != nil {
		// Return a copy to prevent callers from modifying the original data
		result := make([]byte, len(body.Data))
		copy(result, body.Data)
		return result, nil
	}

	if body.FilePath != "" {
		return os.ReadFile(body.FilePath)
	}

	return nil, nil
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
	requestID string
	suffix    string
	cm        *CaptureManager
	body      *CapturedBody
}

func (cb *captureBuffer) Write(p []byte) (n int, err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.truncated {
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

	if cb.body == nil {
		return nil
	}

	data := cb.buf.Bytes()
	cb.body.Size = int64(len(data))
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

// capturingResponseWriter wraps an http.ResponseWriter to capture the response body.
// It intercepts writes to capture up to maxBodySize bytes while still forwarding
// all data to the underlying ResponseWriter. It also implements http.Flusher,
// http.Hijacker, and http.Pusher for compatibility with streaming and WebSocket
// connections.
type capturingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	body        bytes.Buffer
	maxBodySize int64
	truncated   bool
	wroteHeader bool
}

// newCapturingResponseWriter creates a new capturing response writer.
func newCapturingResponseWriter(w http.ResponseWriter, maxBodySize int64) *capturingResponseWriter {
	return &capturingResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		maxBodySize:    maxBodySize,
	}
}

func (crw *capturingResponseWriter) WriteHeader(code int) {
	if !crw.wroteHeader {
		crw.statusCode = code
		crw.wroteHeader = true
	}
	crw.ResponseWriter.WriteHeader(code)
}

func (crw *capturingResponseWriter) Write(p []byte) (int, error) {
	// Capture up to maxBodySize
	if !crw.truncated {
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
func (crw *capturingResponseWriter) StatusCode() int {
	return crw.statusCode
}

// CapturedBody returns the captured response body.
func (crw *capturingResponseWriter) CapturedBody() []byte {
	return crw.body.Bytes()
}

// Truncated returns whether the body was truncated.
func (crw *capturingResponseWriter) Truncated() bool {
	return crw.truncated
}

// Flush implements http.Flusher for streaming responses (SSE).
func (crw *capturingResponseWriter) Flush() {
	if f, ok := crw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for WebSocket support.
func (crw *capturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := crw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// Push implements http.Pusher for HTTP/2 server push.
func (crw *capturingResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := crw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap returns the underlying ResponseWriter for Go 1.20+ http.ResponseController compatibility.
func (crw *capturingResponseWriter) Unwrap() http.ResponseWriter {
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

// isBinaryContent determines if content appears to be binary based on data and content type.
func isBinaryContent(data []byte, contentType string) bool {
	// Check content type first
	if contentType != "" {
		ct := strings.ToLower(contentType)
		// Text types
		if strings.HasPrefix(ct, "text/") ||
			strings.Contains(ct, "json") ||
			strings.Contains(ct, "xml") ||
			strings.Contains(ct, "javascript") ||
			strings.Contains(ct, "html") {
			return false
		}
		// Known binary types
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

	// Check if the data is valid UTF-8 with no control characters (except common ones)
	if len(data) == 0 {
		return false
	}

	// Sample the first 512 bytes
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}

	if !utf8.Valid(sample) {
		return true
	}

	// Check for binary indicators (non-printable characters)
	for _, b := range sample {
		// Allow common control characters: tab, newline, carriage return
		if b < 32 && b != '\t' && b != '\n' && b != '\r' {
			return true
		}
	}

	return false
}
