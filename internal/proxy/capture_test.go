package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
)

func TestNewCaptureManager(t *testing.T) {
	t.Run("nil config returns disabled manager", func(t *testing.T) {
		cm, err := NewCaptureManager(nil, t.TempDir())
		require.NoError(t, err)
		assert.False(t, cm.Enabled())
	})

	t.Run("disabled config returns disabled manager", func(t *testing.T) {
		cfg := &config.CaptureConfig{Enabled: false}
		cm, err := NewCaptureManager(cfg, t.TempDir())
		require.NoError(t, err)
		assert.False(t, cm.Enabled())
	})

	t.Run("enabled config creates capture directory", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)
		assert.True(t, cm.Enabled())

		captureDir := filepath.Join(workDir, constants.CaptureDirectory)
		info, err := os.Stat(captureDir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("custom max body size is applied", func(t *testing.T) {
		cfg := &config.CaptureConfig{Enabled: true, MaxBodySize: "512KB"}
		cm, err := NewCaptureManager(cfg, t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, int64(512*1024), cm.maxBodySize)
	})

	t.Run("invalid max body size returns error", func(t *testing.T) {
		cfg := &config.CaptureConfig{Enabled: true, MaxBodySize: "not-a-size"}
		_, err := NewCaptureManager(cfg, t.TempDir())
		require.Error(t, err)
	})

	t.Run("defaults applied when max body size is empty", func(t *testing.T) {
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, int64(constants.DefaultCaptureMaxBodySize), cm.maxBodySize)
		assert.Equal(t, int64(constants.DefaultCaptureInlineThreshold), cm.inlineThreshold)
	})
}

func newEnabledCaptureManager(t *testing.T) *CaptureManager {
	t.Helper()
	cfg := &config.CaptureConfig{Enabled: true}
	cm, err := NewCaptureManager(cfg, t.TempDir())
	require.NoError(t, err)
	return cm
}

func TestCaptureRequest(t *testing.T) {
	t.Run("disabled manager returns original body unchanged", func(t *testing.T) {
		cm, err := NewCaptureManager(nil, t.TempDir())
		require.NoError(t, err)

		body := io.NopCloser(strings.NewReader("hello"))
		req := httptest.NewRequest("POST", "/test", body)
		req.Header.Set("Content-Type", "text/plain")

		capturedBody, wrappedBody, headers := cm.CaptureRequest("req1", req)
		assert.Nil(t, capturedBody)
		assert.Equal(t, body, wrappedBody) // same object
		assert.Equal(t, "text/plain", headers.Get("Content-Type"))
	})

	t.Run("nil body returns nil captured body", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		req := httptest.NewRequest("GET", "/test", nil)
		// httptest.NewRequest sets body to http.NoBody, so explicitly nil it
		req.Body = nil
		capturedBody, wrappedBody, _ := cm.CaptureRequest("req1", req)
		assert.Nil(t, capturedBody)
		assert.Nil(t, wrappedBody)
	})

	t.Run("small body is captured inline after read and close", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		payload := "small request body"
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")

		capturedBody, wrappedBody, headers := cm.CaptureRequest("req1", req)
		require.NotNil(t, capturedBody)

		// Read the wrapped body fully (this drives the TeeReader)
		data, err := io.ReadAll(wrappedBody)
		require.NoError(t, err)
		assert.Equal(t, payload, string(data))

		// Close triggers finalize
		err = wrappedBody.Close()
		require.NoError(t, err)

		// Now the captured body should be populated
		assert.Equal(t, int64(len(payload)), capturedBody.Size)
		assert.False(t, capturedBody.Truncated)
		assert.Equal(t, "application/json", capturedBody.ContentType)
		assert.Equal(t, payload, string(capturedBody.Data))
		assert.Empty(t, capturedBody.FilePath)

		// Headers should be cloned
		assert.Equal(t, "application/json", headers.Get("Content-Type"))
	})

	t.Run("large body is written to disk", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)

		// Set inline threshold low to force disk storage
		cm.inlineThreshold = 10

		payload := strings.Repeat("x", 100)
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))
		req.Header.Set("Content-Type", "text/plain")

		capturedBody, wrappedBody, _ := cm.CaptureRequest("req-large", req)

		_, err = io.ReadAll(wrappedBody)
		require.NoError(t, err)
		err = wrappedBody.Close()
		require.NoError(t, err)

		assert.Equal(t, int64(100), capturedBody.Size)
		assert.Nil(t, capturedBody.Data)
		assert.NotEmpty(t, capturedBody.FilePath)

		// Verify file exists and has the right content
		fileData, err := os.ReadFile(capturedBody.FilePath)
		require.NoError(t, err)
		assert.Equal(t, payload, string(fileData))
	})

	t.Run("body exceeding max size is truncated", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true, MaxBodySize: "50"}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)

		payload := strings.Repeat("a", 100)
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))

		capturedBody, wrappedBody, _ := cm.CaptureRequest("req-trunc", req)

		// Read and close to finalize
		data, err := io.ReadAll(wrappedBody)
		require.NoError(t, err)
		assert.Equal(t, payload, string(data)) // full data still passed through
		err = wrappedBody.Close()
		require.NoError(t, err)

		assert.Equal(t, int64(100), capturedBody.Size)        // total bytes observed
		assert.Equal(t, int64(50), capturedBody.CapturedSize) // bytes retained
		assert.True(t, capturedBody.Truncated)
	})

	t.Run("headers are cloned independently", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		req := httptest.NewRequest("POST", "/test", strings.NewReader("body"))
		req.Header.Set("X-Custom", "original")

		_, wrappedBody, headers := cm.CaptureRequest("req1", req)

		// Modify the clone
		headers.Set("X-Custom", "modified")

		// Original should be unchanged
		assert.Equal(t, "original", req.Header.Get("X-Custom"))

		_, _ = io.ReadAll(wrappedBody)
		_ = wrappedBody.Close()
	})
}

func TestCaptureResponse(t *testing.T) {
	t.Run("disabled manager returns nil body", func(t *testing.T) {
		cm, err := NewCaptureManager(nil, t.TempDir())
		require.NoError(t, err)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		crw.Header().Set("Content-Type", "text/html")
		crw.Write([]byte("hello"))

		body, headers := cm.FinalizeResponse("req1", crw)
		assert.Nil(t, body)
		assert.Equal(t, "text/html", headers.Get("Content-Type"))
	})

	t.Run("small response stored inline", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, constants.DefaultCaptureMaxBodySize)
		crw.Header().Set("Content-Type", "application/json")
		crw.Write([]byte(`{"ok":true}`))

		body, headers := cm.FinalizeResponse("req1", crw)
		require.NotNil(t, body)
		assert.Equal(t, int64(11), body.Size)
		assert.False(t, body.Truncated)
		assert.Equal(t, "application/json", body.ContentType)
		assert.False(t, body.IsBinary)
		assert.Equal(t, `{"ok":true}`, string(body.Data))
		assert.Empty(t, body.FilePath)
		assert.Equal(t, "application/json", headers.Get("Content-Type"))
	})

	t.Run("large response stored on disk", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)
		cm.inlineThreshold = 10

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, constants.DefaultCaptureMaxBodySize)
		crw.Header().Set("Content-Type", "text/plain")

		payload := strings.Repeat("y", 100)
		crw.Write([]byte(payload))

		body, _ := cm.FinalizeResponse("req-disk", crw)
		require.NotNil(t, body)
		assert.Equal(t, int64(100), body.Size)
		assert.Nil(t, body.Data)
		assert.NotEmpty(t, body.FilePath)

		fileData, err := os.ReadFile(body.FilePath)
		require.NoError(t, err)
		assert.Equal(t, payload, string(fileData))
	})

	t.Run("truncated response is flagged", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 20)
		crw.Header().Set("Content-Type", "text/plain")

		payload := strings.Repeat("z", 50)
		crw.Write([]byte(payload))

		body, _ := cm.FinalizeResponse("req-trunc", crw)
		require.NotNil(t, body)
		assert.True(t, body.Truncated)
		assert.Equal(t, int64(50), body.Size)         // total bytes observed
		assert.Equal(t, int64(20), body.CapturedSize) // bytes retained
	})
}

func TestLoadBody(t *testing.T) {
	t.Run("nil body returns nil", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)
		data, err := cm.LoadBody(nil)
		require.NoError(t, err)
		assert.Nil(t, data)
	})

	t.Run("inline body returns a copy", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)
		original := []byte("hello world")
		body := &CapturedBody{Data: original}

		loaded, err := cm.LoadBody(body)
		require.NoError(t, err)
		assert.Equal(t, original, loaded)

		// Mutating the returned copy should not affect the original
		loaded[0] = 'X'
		assert.Equal(t, byte('h'), original[0])
	})

	t.Run("file-backed body reads from disk", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		// Write a test file inside the manager's capture dir (the allowlist root).
		tmpFile := filepath.Join(cm.CaptureDir(), "test_body.bin")
		err := os.WriteFile(tmpFile, []byte("file content"), 0600)
		require.NoError(t, err)

		body := &CapturedBody{FilePath: tmpFile}
		loaded, err := cm.LoadBody(body)
		require.NoError(t, err)
		assert.Equal(t, "file content", string(loaded))
	})

	t.Run("missing file returns error", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)
		body := &CapturedBody{FilePath: "/nonexistent/path/body.bin"}
		_, err := cm.LoadBody(body)
		require.Error(t, err)
	})

	t.Run("empty body returns nil", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)
		body := &CapturedBody{} // no Data, no FilePath
		loaded, err := cm.LoadBody(body)
		require.NoError(t, err)
		assert.Nil(t, loaded)
	})
}

func TestCleanupRequest(t *testing.T) {
	t.Run("removes request and response files", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)

		// Create fake capture files
		reqFile := filepath.Join(cm.captureDir, "abc1234_req.bin")
		resFile := filepath.Join(cm.captureDir, "abc1234_res.bin")
		require.NoError(t, os.WriteFile(reqFile, []byte("req"), 0600))
		require.NoError(t, os.WriteFile(resFile, []byte("res"), 0600))

		cm.CleanupRequest("abc1234")

		_, err = os.Stat(reqFile)
		assert.True(t, os.IsNotExist(err))
		_, err = os.Stat(resFile)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("noop when disabled", func(t *testing.T) {
		cm, err := NewCaptureManager(nil, t.TempDir())
		require.NoError(t, err)
		// Should not panic
		cm.CleanupRequest("anything")
	})
}

func TestCleanup(t *testing.T) {
	t.Run("removes entire capture directory", func(t *testing.T) {
		workDir := t.TempDir()
		cfg := &config.CaptureConfig{Enabled: true}
		cm, err := NewCaptureManager(cfg, workDir)
		require.NoError(t, err)

		// Create a file inside capture dir
		require.NoError(t, os.WriteFile(filepath.Join(cm.captureDir, "test.bin"), []byte("data"), 0600))

		err = cm.Cleanup()
		require.NoError(t, err)

		_, err = os.Stat(cm.captureDir)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("noop when capture dir is empty string", func(t *testing.T) {
		cm := &CaptureManager{}
		err := cm.Cleanup()
		assert.NoError(t, err)
	})
}

// --- captureBuffer tests ---

func TestCaptureBuffer_Write(t *testing.T) {
	t.Run("captures data up to max size", func(t *testing.T) {
		cb := &captureBuffer{
			maxSize: 10,
			body:    &CapturedBody{},
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		n, err := cb.Write([]byte("hello"))
		require.NoError(t, err)
		assert.Equal(t, 5, n)
		assert.Equal(t, "hello", cb.buf.String())
		assert.False(t, cb.truncated)
	})

	t.Run("truncates and reports full length", func(t *testing.T) {
		cb := &captureBuffer{
			maxSize: 5,
			body:    &CapturedBody{},
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		n, err := cb.Write([]byte("hello world"))
		require.NoError(t, err)
		assert.Equal(t, 11, n) // reports full length
		assert.Equal(t, "hello", cb.buf.String())
		assert.True(t, cb.truncated)
	})

	t.Run("discards further writes once truncated", func(t *testing.T) {
		cb := &captureBuffer{
			maxSize: 5,
			body:    &CapturedBody{},
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		cb.Write([]byte("hello world"))
		n, err := cb.Write([]byte("more data"))
		require.NoError(t, err)
		assert.Equal(t, 9, n) // reports full length
		assert.Equal(t, "hello", cb.buf.String())
	})

	t.Run("exact boundary triggers truncation on next write", func(t *testing.T) {
		cb := &captureBuffer{
			maxSize: 5,
			body:    &CapturedBody{},
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		cb.Write([]byte("hello"))
		assert.False(t, cb.truncated)

		n, err := cb.Write([]byte("!"))
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.True(t, cb.truncated)
		assert.Equal(t, "hello", cb.buf.String())
	})
}

func TestCaptureBuffer_Finalize(t *testing.T) {
	t.Run("populates body fields for inline storage", func(t *testing.T) {
		body := &CapturedBody{ContentType: "text/plain"}
		cb := &captureBuffer{
			maxSize: 1024,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}
		cb.Write([]byte("test data"))

		err := cb.finalize()
		require.NoError(t, err)

		assert.Equal(t, int64(9), body.Size)
		assert.False(t, body.Truncated)
		assert.False(t, body.IsBinary)
		assert.Equal(t, "test data", string(body.Data))
	})

	t.Run("stores on disk when over threshold", func(t *testing.T) {
		workDir := t.TempDir()
		captureDir := filepath.Join(workDir, "capture")
		require.NoError(t, os.MkdirAll(captureDir, 0700))

		body := &CapturedBody{ContentType: "text/plain"}
		cb := &captureBuffer{
			maxSize:   1024,
			requestID: "req123",
			suffix:    "_req",
			body:      body,
			cm: &CaptureManager{
				inlineThreshold: 5, // very low
				captureDir:      captureDir,
				acct:            newDiskAccountant(captureDir, constants.DefaultCaptureDiskBudget),
			},
		}
		cb.Write([]byte("this is longer than threshold"))

		err := cb.finalize()
		require.NoError(t, err)

		assert.NotEmpty(t, body.FilePath)
		assert.Nil(t, body.Data)

		fileData, err := os.ReadFile(body.FilePath)
		require.NoError(t, err)
		assert.Equal(t, "this is longer than threshold", string(fileData))
	})

	t.Run("falls back to inline when disk write fails", func(t *testing.T) {
		body := &CapturedBody{ContentType: "text/plain"}
		cb := &captureBuffer{
			maxSize:   1024,
			requestID: "req123",
			suffix:    "_req",
			body:      body,
			cm: &CaptureManager{
				inlineThreshold: 5,
				captureDir:      "/nonexistent/directory",
				acct:            newDiskAccountant("/nonexistent/directory", constants.DefaultCaptureDiskBudget),
			},
		}
		cb.Write([]byte("some data here"))

		err := cb.finalize()
		require.Error(t, err) // returns error for caller awareness
		assert.Equal(t, "some data here", string(body.Data))
		assert.Empty(t, body.FilePath)
	})

	t.Run("nil body is a noop", func(t *testing.T) {
		cb := &captureBuffer{body: nil}
		err := cb.finalize()
		assert.NoError(t, err)
	})

	t.Run("marks truncated when buffer was truncated", func(t *testing.T) {
		body := &CapturedBody{ContentType: "text/plain"}
		cb := &captureBuffer{
			maxSize: 5,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}
		cb.Write([]byte("hello world"))
		cb.finalize()

		assert.True(t, body.Truncated)
		assert.Equal(t, int64(11), body.Size)        // total bytes observed
		assert.Equal(t, int64(5), body.CapturedSize) // bytes retained
	})
}

// --- captureReadCloser tests ---

func TestCaptureReadCloser(t *testing.T) {
	t.Run("close finalizes capture and closes underlying body", func(t *testing.T) {
		body := &CapturedBody{ContentType: "text/plain"}
		cb := &captureBuffer{
			maxSize: 1024,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}
		cb.Write([]byte("captured"))

		underlying := io.NopCloser(strings.NewReader(""))
		crc := &captureReadCloser{
			Reader:   strings.NewReader("read data"),
			Closer:   underlying,
			captured: cb,
		}

		err := crc.Close()
		require.NoError(t, err)

		// Verify finalize ran
		assert.Equal(t, int64(8), body.Size)
		assert.Equal(t, "captured", string(body.Data))
	})
}

// --- CaptureResponseWriter tests ---

func TestCapturingResponseWriter(t *testing.T) {
	t.Run("captures status code on first WriteHeader", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)

		crw.WriteHeader(http.StatusCreated)
		assert.Equal(t, http.StatusCreated, crw.StatusCode())

		// Second call should not change it
		crw.WriteHeader(http.StatusInternalServerError)
		assert.Equal(t, http.StatusCreated, crw.StatusCode())
	})

	t.Run("defaults to 200 if WriteHeader not called", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		assert.Equal(t, http.StatusOK, crw.StatusCode())
	})

	t.Run("captures body while forwarding to underlying writer", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)

		n, err := crw.Write([]byte("hello "))
		require.NoError(t, err)
		assert.Equal(t, 6, n)

		n, err = crw.Write([]byte("world"))
		require.NoError(t, err)
		assert.Equal(t, 5, n)

		assert.Equal(t, "hello world", string(crw.CapturedBody()))
		assert.Equal(t, "hello world", w.Body.String()) // forwarded
	})

	t.Run("truncates capture at max size but forwards full data", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 5)

		crw.Write([]byte("hello world"))

		assert.Equal(t, "hello", string(crw.CapturedBody()))
		assert.True(t, crw.Truncated())
		assert.Equal(t, "hello world", w.Body.String()) // full data forwarded
	})

	t.Run("Flush delegates to underlying flusher", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		// Should not panic even if underlying doesn't implement Flusher
		crw.Flush()
	})

	t.Run("Hijack returns error when unsupported", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		_, _, err := crw.Hijack()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "hijacking not supported")
	})

	t.Run("Push returns ErrNotSupported when unsupported", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		err := crw.Push("/resource", nil)
		assert.ErrorIs(t, err, http.ErrNotSupported)
	})

	t.Run("Unwrap returns underlying writer", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 1024)
		assert.Equal(t, w, crw.Unwrap())
	})

	t.Run("multiple writes with truncation mid-write", func(t *testing.T) {
		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 8)

		crw.Write([]byte("abcd"))
		assert.False(t, crw.Truncated())

		crw.Write([]byte("efghij"))
		assert.True(t, crw.Truncated())
		assert.Equal(t, "abcdefgh", string(crw.CapturedBody()))

		// Further writes are discarded from capture
		crw.Write([]byte("more"))
		assert.Equal(t, "abcdefgh", string(crw.CapturedBody()))
		assert.Equal(t, "abcdefghijmore", w.Body.String()) // all forwarded
	})
}

// --- isBinaryContent tests ---

func TestIsBinaryContent(t *testing.T) {
	t.Run("text content types return false", func(t *testing.T) {
		assert.False(t, isBinaryContent([]byte("data"), "text/html"))
		assert.False(t, isBinaryContent([]byte("data"), "text/plain"))
		assert.False(t, isBinaryContent([]byte("data"), "application/json"))
		assert.False(t, isBinaryContent([]byte("data"), "application/xml"))
		assert.False(t, isBinaryContent([]byte("data"), "application/javascript"))
		assert.False(t, isBinaryContent([]byte("data"), "text/html; charset=utf-8"))
	})

	t.Run("binary content types return true", func(t *testing.T) {
		assert.True(t, isBinaryContent([]byte("data"), "image/png"))
		assert.True(t, isBinaryContent([]byte("data"), "image/jpeg"))
		assert.True(t, isBinaryContent([]byte("data"), "audio/mpeg"))
		assert.True(t, isBinaryContent([]byte("data"), "video/mp4"))
		assert.True(t, isBinaryContent([]byte("data"), "application/octet-stream"))
		assert.True(t, isBinaryContent([]byte("data"), "application/zip"))
		assert.True(t, isBinaryContent([]byte("data"), "application/gzip"))
		assert.True(t, isBinaryContent([]byte("data"), "application/pdf"))
	})

	t.Run("empty data with no content type returns false", func(t *testing.T) {
		assert.False(t, isBinaryContent([]byte{}, ""))
	})

	t.Run("invalid UTF-8 data returns true", func(t *testing.T) {
		invalidUTF8 := []byte{0xff, 0xfe, 0x80, 0x81}
		assert.True(t, isBinaryContent(invalidUTF8, ""))
	})

	t.Run("non-printable control characters return true", func(t *testing.T) {
		// ASCII control char 0x01 (SOH)
		withControl := []byte("hello\x01world")
		assert.True(t, isBinaryContent(withControl, ""))
	})

	t.Run("allowed control characters return false", func(t *testing.T) {
		withAllowed := []byte("hello\tworld\nfoo\rbar")
		assert.False(t, isBinaryContent(withAllowed, ""))
	})

	t.Run("scans the entire buffer, not just first 512 bytes", func(t *testing.T) {
		// Regression for the 512-byte sampling bug (multipart-style):
		// >512 bytes of valid ASCII followed by invalid-UTF-8 bytes.
		data := make([]byte, 600)
		for i := 0; i < 512; i++ {
			data[i] = 'a'
		}
		for i := 512; i < 600; i++ {
			data[i] = 0xff // invalid UTF-8 past the old sampling window
		}
		// Even with a text-y content type, invalid UTF-8 makes this binary.
		assert.True(t, isBinaryContent(data, "multipart/form-data"))
		assert.True(t, isBinaryContent(data, ""))
	})

	t.Run("text-y content type does not short-circuit invalid UTF-8", func(t *testing.T) {
		// Invalid UTF-8 must classify binary regardless of a text-y CT.
		binaryData := []byte{0xff, 0xfe, 0x00}
		assert.True(t, isBinaryContent(binaryData, "application/json"))
	})

	t.Run("gzip-compressed JSON with json content type is binary", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, err := gz.Write([]byte(`{"hello":"world","n":42}`))
		require.NoError(t, err)
		require.NoError(t, gz.Close())

		// Compressed bytes are (almost surely) not valid UTF-8 → binary,
		// even though the declared content type is application/json.
		assert.True(t, isBinaryContent(buf.Bytes(), "application/json"))
	})

	t.Run("known-binary content type is binary even with empty data", func(t *testing.T) {
		assert.True(t, isBinaryContent([]byte{}, "image/png"))
	})
}

// --- cloneHeaders tests ---

func TestCloneHeaders(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		assert.Nil(t, cloneHeaders(nil))
	})

	t.Run("clone is independent of original", func(t *testing.T) {
		original := http.Header{
			"Content-Type": {"application/json"},
			"X-Custom":     {"value1", "value2"},
		}

		clone := cloneHeaders(original)
		assert.Equal(t, original, clone)

		// Mutating the clone should not affect the original
		clone.Set("Content-Type", "text/plain")
		assert.Equal(t, "application/json", original.Get("Content-Type"))
	})
}

// --- Eviction callback integration ---

func TestRequestManager_EvictionCallbackCleanup(t *testing.T) {
	workDir := t.TempDir()
	cfg := &config.CaptureConfig{Enabled: true}
	cm, err := NewCaptureManager(cfg, workDir)
	require.NoError(t, err)

	rm := NewRequestManager(2) // small buffer to trigger eviction quickly
	rm.SetEvictionCallback(func(id string) {
		cm.CleanupRequest(id)
	})

	// Create capture files for first request
	reqFile := filepath.Join(cm.captureDir, "req001_req.bin")
	resFile := filepath.Join(cm.captureDir, "req001_res.bin")
	require.NoError(t, os.WriteFile(reqFile, []byte("req"), 0600))
	require.NoError(t, os.WriteFile(resFile, []byte("res"), 0600))

	// Record requests to fill and overflow the buffer
	rm.Record(RequestRecord{
		ID:      "req001",
		Details: &RequestDetails{},
	})
	rm.Record(RequestRecord{
		ID:      "req002",
		Details: &RequestDetails{},
	})
	// This third record should evict req001
	rm.Record(RequestRecord{
		ID:      "req003",
		Details: &RequestDetails{},
	})

	// Files for req001 should be cleaned up
	_, err = os.Stat(reqFile)
	assert.True(t, os.IsNotExist(err), "request file should be cleaned up on eviction")
	_, err = os.Stat(resFile)
	assert.True(t, os.IsNotExist(err), "response file should be cleaned up on eviction")
}

// --- Size semantics (D11): total observed vs retained ---

func TestCaptureBuffer_SizeSemantics(t *testing.T) {
	t.Run("counts bytes past the truncation cap", func(t *testing.T) {
		body := &CapturedBody{}
		cb := &captureBuffer{
			maxSize: 10,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		// Three writes of 6 bytes each = 18 total, cap 10.
		cb.Write([]byte("aaaaaa"))
		cb.Write([]byte("bbbbbb"))
		cb.Write([]byte("cccccc"))

		require.NoError(t, cb.finalize())
		assert.Equal(t, int64(18), body.Size)         // total observed, counting past the cap
		assert.Equal(t, int64(10), body.CapturedSize) // bytes retained
		assert.True(t, body.Truncated)
	})

	t.Run("exact-cap boundary is not truncated", func(t *testing.T) {
		body := &CapturedBody{}
		cb := &captureBuffer{
			maxSize: 10,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		// Writes summing exactly to the cap.
		cb.Write([]byte("aaaaa"))
		cb.Write([]byte("bbbbb"))

		require.NoError(t, cb.finalize())
		assert.Equal(t, int64(10), body.Size)
		assert.Equal(t, int64(10), body.CapturedSize)
		assert.Equal(t, body.Size, body.CapturedSize)
		assert.False(t, body.Truncated)
	})

	t.Run("zero-length write at exact cap does not mark truncated", func(t *testing.T) {
		body := &CapturedBody{}
		cb := &captureBuffer{
			maxSize: 10,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		cb.Write([]byte("aaaaaaaaaa"))
		cb.Write([]byte{})

		require.NoError(t, cb.finalize())
		assert.Equal(t, int64(10), body.Size)
		assert.Equal(t, int64(10), body.CapturedSize)
		assert.False(t, body.Truncated)
	})

	t.Run("writes after finalize are discarded and finalize is idempotent", func(t *testing.T) {
		// A canceled request's transport goroutine can keep draining the body
		// after the handler finalized and recorded; those late writes must not
		// mutate the frozen CapturedBody snapshot.
		body := &CapturedBody{}
		cb := &captureBuffer{
			maxSize: 100,
			body:    body,
			cm:      &CaptureManager{inlineThreshold: 1024},
		}

		cb.Write([]byte("early"))
		require.NoError(t, cb.finalize())
		assert.Equal(t, int64(5), body.Size)
		assert.Equal(t, "early", string(body.Data))

		cb.Write([]byte("late-bytes"))
		require.NoError(t, cb.finalize()) // second finalize: no-op
		assert.Equal(t, int64(5), body.Size, "late writes must not change the snapshot")
		assert.Equal(t, "early", string(body.Data))
	})
}

func TestFinalizeRequestBody(t *testing.T) {
	t.Run("forces finalize on a wrapped body before Close", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)
		req := httptest.NewRequest("POST", "/test", strings.NewReader("payload"))
		capturedBody, wrappedBody, _ := cm.CaptureRequest("req-force", req)

		_, err := io.ReadAll(wrappedBody)
		require.NoError(t, err)

		FinalizeRequestBody(wrappedBody)
		assert.Equal(t, int64(7), capturedBody.Size, "snapshot complete without Close")

		// The transport's later Close must not re-finalize or error.
		require.NoError(t, wrappedBody.Close())
		assert.Equal(t, int64(7), capturedBody.Size)
	})

	t.Run("non-wrapped body is a no-op", func(t *testing.T) {
		require.NotPanics(t, func() {
			FinalizeRequestBody(io.NopCloser(strings.NewReader("x")))
			FinalizeRequestBody(nil)
		})
	})
}

func TestCaptureResponseWriter_Hijacked(t *testing.T) {
	t.Run("not hijacked by default", func(t *testing.T) {
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		assert.False(t, crw.Hijacked())
	})

	t.Run("failed hijack does not mark hijacked", func(t *testing.T) {
		// httptest.ResponseRecorder does not implement http.Hijacker.
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		_, _, err := crw.Hijack()
		require.Error(t, err)
		assert.False(t, crw.Hijacked())
	})
}

func TestCapturingResponseWriter_SizeSemantics(t *testing.T) {
	t.Run("counts bytes past the truncation cap via CaptureResponse", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 10)
		crw.Header().Set("Content-Type", "text/plain")

		crw.Write([]byte("aaaaaa"))
		crw.Write([]byte("bbbbbb"))
		crw.Write([]byte("cccccc"))

		body, _ := cm.FinalizeResponse("req-total", crw)
		require.NotNil(t, body)
		assert.Equal(t, int64(18), body.Size)
		assert.Equal(t, int64(10), body.CapturedSize)
		assert.True(t, body.Truncated)
	})

	t.Run("exact-cap boundary is not truncated via CaptureResponse", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 10)
		crw.Header().Set("Content-Type", "text/plain")

		crw.Write([]byte("aaaaa"))
		crw.Write([]byte("bbbbb"))

		body, _ := cm.FinalizeResponse("req-exact", crw)
		require.NotNil(t, body)
		assert.Equal(t, int64(10), body.Size)
		assert.Equal(t, int64(10), body.CapturedSize)
		assert.Equal(t, body.Size, body.CapturedSize)
		assert.False(t, body.Truncated)
	})

	t.Run("zero-length write at exact cap does not mark truncated", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, 10)
		crw.Header().Set("Content-Type", "text/plain")

		crw.Write([]byte("aaaaaaaaaa"))
		crw.Write([]byte{})

		body, _ := cm.FinalizeResponse("req-zero", crw)
		require.NotNil(t, body)
		assert.Equal(t, int64(10), body.Size)
		assert.Equal(t, int64(10), body.CapturedSize)
		assert.False(t, body.Truncated)
	})
}

// --- ContentEncoding capture (data model only; no decode) ---

func TestContentEncodingCaptured(t *testing.T) {
	t.Run("request body records Content-Encoding", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		req := httptest.NewRequest("POST", "/test", strings.NewReader("payload"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")

		capturedBody, wrappedBody, _ := cm.CaptureRequest("req-enc", req)
		require.NotNil(t, capturedBody)

		_, err := io.ReadAll(wrappedBody)
		require.NoError(t, err)
		require.NoError(t, wrappedBody.Close())

		assert.Equal(t, "gzip", capturedBody.ContentEncoding)
	})

	t.Run("response body records Content-Encoding", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		w := httptest.NewRecorder()
		crw := newCaptureResponseWriter(w, constants.DefaultCaptureMaxBodySize)
		crw.Header().Set("Content-Type", "application/json")
		crw.Header().Set("Content-Encoding", "gzip")
		crw.Write([]byte("compressed-ish"))

		body, _ := cm.FinalizeResponse("res-enc", crw)
		require.NotNil(t, body)
		assert.Equal(t, "gzip", body.ContentEncoding)
	})

	t.Run("absent Content-Encoding stays empty", func(t *testing.T) {
		cm := newEnabledCaptureManager(t)

		req := httptest.NewRequest("POST", "/test", strings.NewReader("payload"))
		capturedBody, wrappedBody, _ := cm.CaptureRequest("req-noenc", req)
		require.NotNil(t, capturedBody)
		_, err := io.ReadAll(wrappedBody)
		require.NoError(t, err)
		require.NoError(t, wrappedBody.Close())

		assert.Empty(t, capturedBody.ContentEncoding)
	})
}

// hookHijacker is an http.ResponseWriter that also implements http.Hijacker,
// backed by an in-memory net.Pipe, so a SUCCESSFUL Hijack() can be exercised in
// a unit test (httptest.ResponseRecorder is not a Hijacker so it fails hijack).
type hookHijacker struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hookHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func newHookHijacker(t *testing.T) *hookHijacker {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return &hookHijacker{ResponseRecorder: httptest.NewRecorder(), conn: c1}
}

func TestCaptureResponseWriter_FirstResponseHook(t *testing.T) {
	t.Run("repeated WriteHeader fires once with the first >=200 code", func(t *testing.T) {
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		var calls []int
		crw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		crw.WriteHeader(http.StatusCreated)
		crw.WriteHeader(http.StatusInternalServerError)

		assert.Equal(t, []int{http.StatusCreated}, calls)
		assert.Equal(t, http.StatusCreated, crw.StatusCode(), "first-wins latch")
	})

	t.Run("1xx then 200 fires once with 200 and records 200", func(t *testing.T) {
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		var calls []int
		crw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		crw.WriteHeader(http.StatusEarlyHints) // 103
		assert.Empty(t, calls, "1xx must not fire the hook")
		assert.Equal(t, http.StatusOK, crw.StatusCode(), "1xx must not latch the status")

		crw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusOK}, calls)
		assert.Equal(t, http.StatusOK, crw.StatusCode())
	})

	t.Run("implicit bare Write does not fire the hook", func(t *testing.T) {
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		var calls []int
		crw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _ = crw.Write([]byte("hi"))
		assert.Empty(t, calls)
	})

	t.Run("failed hijack does not fire the hook", func(t *testing.T) {
		// httptest.ResponseRecorder does not implement http.Hijacker.
		crw := newCaptureResponseWriter(httptest.NewRecorder(), 1024)
		var calls []int
		crw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := crw.Hijack()
		require.Error(t, err)
		assert.Empty(t, calls)
	})

	t.Run("successful hijack fires 101 once, no re-fire on late WriteHeader", func(t *testing.T) {
		crw := newCaptureResponseWriter(newHookHijacker(t), 1024)
		var calls []int
		crw.SetFirstResponseCallback(func(code int) { calls = append(calls, code) })

		_, _, err := crw.Hijack()
		require.NoError(t, err)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls)

		crw.WriteHeader(http.StatusOK)
		assert.Equal(t, []int{http.StatusSwitchingProtocols}, calls, "hook must not re-fire after hijack")
	})
}

// TestPerCallCaptureLimits pins the D13 per-call caps: CaptureRequestWithLimit
// and WrapResponseWriterWithLimit honor a positive per-call byte limit, while a
// 0 limit falls back to the manager's configured cap (which itself defaults to
// DefaultCaptureMaxBodySize). This is what lets the daemon apply each project's
// own MaxBodySize through the one shared capture manager, and what keeps the
// standalone in-process proxy on its project's configured cap.
func TestPerCallCaptureLimits(t *testing.T) {
	// A manager whose configured cap is 50 bytes; positive per-call limits must
	// override it in both directions.
	cfg := &config.CaptureConfig{Enabled: true, MaxBodySize: "50"}
	cm, err := NewCaptureManager(cfg, t.TempDir())
	require.NoError(t, err)

	payload := strings.Repeat("a", 200)

	t.Run("request tighter per-call limit wins", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))
		captured, wrapped, _ := cm.CaptureRequestWithLimit("req-tight", req, 10)
		_, _ = io.ReadAll(wrapped)
		require.NoError(t, wrapped.Close())
		assert.Equal(t, int64(200), captured.Size, "total observed unchanged")
		assert.Equal(t, int64(10), captured.CapturedSize, "per-call limit 10 caps retained bytes")
		assert.True(t, captured.Truncated)
	})

	t.Run("request looser per-call limit wins", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))
		captured, wrapped, _ := cm.CaptureRequestWithLimit("req-loose", req, 150)
		_, _ = io.ReadAll(wrapped)
		require.NoError(t, wrapped.Close())
		assert.Equal(t, int64(150), captured.CapturedSize, "per-call limit 150 overrides the 50-byte manager cap")
		assert.True(t, captured.Truncated)
	})

	t.Run("request zero per-call limit falls back to manager cap", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/test", strings.NewReader(payload))
		captured, wrapped, _ := cm.CaptureRequestWithLimit("req-zero", req, 0)
		_, _ = io.ReadAll(wrapped)
		require.NoError(t, wrapped.Close())
		assert.Equal(t, int64(50), captured.CapturedSize, "0 limit uses the manager's configured 50-byte cap")
		assert.True(t, captured.Truncated)
	})

	t.Run("response per-call limit and zero fallback", func(t *testing.T) {
		body := []byte(payload)

		tight := cm.WrapResponseWriterWithLimit(httptest.NewRecorder(), 10)
		tight.WriteHeader(http.StatusOK)
		_, _ = tight.Write(body)
		assert.Len(t, tight.CapturedBody(), 10, "per-call limit 10 caps response capture")

		fallback := cm.WrapResponseWriterWithLimit(httptest.NewRecorder(), 0)
		fallback.WriteHeader(http.StatusOK)
		_, _ = fallback.Write(body)
		assert.Len(t, fallback.CapturedBody(), 50, "0 limit uses the manager's configured 50-byte cap")

		// The no-limit convenience wrapper is exactly the 0-limit path.
		def := cm.WrapResponseWriter(httptest.NewRecorder())
		def.WriteHeader(http.StatusOK)
		_, _ = def.Write(body)
		assert.Len(t, def.CapturedBody(), 50, "WrapResponseWriter delegates to the manager cap")
	})
}
