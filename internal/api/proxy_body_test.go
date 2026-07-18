package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// newBodyTestHandlers builds Handlers with a request manager and an enabled
// capture manager rooted at a fresh temp dir. Returns the handlers, the manager,
// and the capture directory (the allowlist root for FilePath bodies).
func newBodyTestHandlers(t *testing.T) (*Handlers, *proxy.RequestManager, string) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})
	t.Cleanup(logMgr.Close)

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0},
		Processes: map[string]config.ProcessConfig{},
	}
	sup := supervisor.New(cfg, logMgr, nil, supervisor.DefaultSupervisorConfig())
	handlers := NewHandlers(sup, logMgr, "prox.yaml", nil)

	rm := proxy.NewRequestManager(100)
	handlers.SetRequestManager(rm)

	cm, err := proxy.NewCaptureManager(&config.CaptureConfig{Enabled: true}, t.TempDir())
	require.NoError(t, err)
	handlers.SetCaptureManager(cm)

	return handlers, rm, cm.CaptureDir()
}

// getRequestWithBody records a request whose response body is respBody, then
// GETs it with include=body, returning both the parsed response and the raw JSON.
func getRequestWithBody(t *testing.T, handlers *Handlers, rm *proxy.RequestManager, id string, respBody *proxy.CapturedBody) (ProxyRequestDetailResponse, string) {
	t.Helper()
	rm.Record(proxy.RequestRecord{
		ID:         id,
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/api/data",
		Subdomain:  "api",
		StatusCode: 200,
		Duration:   10 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
		Details: &proxy.RequestDetails{
			ResponseHeaders: http.Header{"Content-Type": {respBody.ContentType}},
			ResponseBody:    respBody,
		},
	})

	req := httptest.NewRequest("GET", "/api/v1/proxy/requests/"+id+"?include=body", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handlers.GetProxyRequest(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	raw := w.Body.String()
	var resp ProxyRequestDetailResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	return resp, raw
}

func TestConvertCapturedBody_GzipRoundTrip(t *testing.T) {
	handlers, rm, _ := newBodyTestHandlers(t)
	payload := []byte(`{"message":"hello, gzip","items":[1,2,3]}`)

	t.Run("gzip decodes to readable JSON", func(t *testing.T) {
		resp, _ := getRequestWithBody(t, handlers, rm, "gz00001", &proxy.CapturedBody{
			Size:            int64(len(payload)),
			CapturedSize:    int64(len(payload)),
			ContentType:     "application/json",
			ContentEncoding: "gzip",
			IsBinary:        true, // stored: raw gzip bytes
			Data:            gzipBytes(t, payload),
		})
		require.NotNil(t, resp.Details.ResponseBody)
		body := resp.Details.ResponseBody
		assert.Equal(t, "gzip", body.ContentEncoding)
		assert.False(t, body.IsBinary) // served: decoded JSON is text
		assert.Equal(t, string(payload), body.Data)
	})

	t.Run("x-gzip alias", func(t *testing.T) {
		resp, _ := getRequestWithBody(t, handlers, rm, "gz00002", &proxy.CapturedBody{
			ContentType:     "application/json",
			ContentEncoding: "x-gzip",
			IsBinary:        true,
			Data:            gzipBytes(t, payload),
		})
		assert.False(t, resp.Details.ResponseBody.IsBinary)
		assert.Equal(t, string(payload), resp.Details.ResponseBody.Data)
	})

	t.Run("uppercase GZIP", func(t *testing.T) {
		resp, _ := getRequestWithBody(t, handlers, rm, "gz00003", &proxy.CapturedBody{
			ContentType:     "application/json",
			ContentEncoding: "GZIP",
			IsBinary:        true,
			Data:            gzipBytes(t, payload),
		})
		assert.False(t, resp.Details.ResponseBody.IsBinary)
		assert.Equal(t, string(payload), resp.Details.ResponseBody.Data)
	})
}

func TestConvertCapturedBody_FallsBackToRaw(t *testing.T) {
	handlers, rm, _ := newBodyTestHandlers(t)
	payload := []byte(`{"x":"y"}`)

	t.Run("corrupt gzip stream", func(t *testing.T) {
		raw := gzipBytes(t, payload)
		raw[len(raw)-2] ^= 0xff
		resp, _ := getRequestWithBody(t, handlers, rm, "cor0001", &proxy.CapturedBody{
			ContentType:     "application/json",
			ContentEncoding: "gzip",
			IsBinary:        true,
			Data:            raw,
		})
		body := resp.Details.ResponseBody
		assert.True(t, body.IsBinary)
		assert.Equal(t, "gzip", body.ContentEncoding)
		assert.Equal(t, base64Encode(raw), body.Data)
	})

	t.Run("truncated gzip is not decoded", func(t *testing.T) {
		raw := gzipBytes(t, payload)
		resp, _ := getRequestWithBody(t, handlers, rm, "tru0001", &proxy.CapturedBody{
			ContentType:     "application/json",
			ContentEncoding: "gzip",
			Truncated:       true,
			IsBinary:        true,
			Data:            raw,
		})
		body := resp.Details.ResponseBody
		assert.True(t, body.IsBinary)
		assert.Equal(t, base64Encode(raw), body.Data)
	})

	t.Run("unsupported encodings preserved and served raw", func(t *testing.T) {
		for i, enc := range []string{"br", "gzip, br"} {
			id := "uns000" + string(rune('a'+i))
			resp, _ := getRequestWithBody(t, handlers, rm, id, &proxy.CapturedBody{
				ContentType:     "application/json",
				ContentEncoding: enc,
				IsBinary:        false,
				Data:            payload,
			})
			body := resp.Details.ResponseBody
			assert.True(t, body.IsBinary, enc)
			assert.Equal(t, enc, body.ContentEncoding, enc)
			assert.Equal(t, base64Encode(payload), body.Data, enc)
		}
	})

	t.Run("zip bomb over decode cap served raw", func(t *testing.T) {
		bomb := bytes.Repeat([]byte{'a'}, 11*1024*1024)
		raw := gzipBytes(t, bomb)
		resp, _ := getRequestWithBody(t, handlers, rm, "bomb001", &proxy.CapturedBody{
			ContentType:     "text/plain",
			ContentEncoding: "gzip",
			IsBinary:        true,
			Data:            raw,
		})
		body := resp.Details.ResponseBody
		assert.True(t, body.IsBinary)
		assert.Equal(t, base64Encode(raw), body.Data)
	})
}

// TestConvertCapturedBody_StoredVsServedBinary pins the divergence: the stored
// CapturedBody.IsBinary (raw gzip = true) differs from the served is_binary
// (decoded JSON = false) in the same response cycle.
func TestConvertCapturedBody_StoredVsServedBinary(t *testing.T) {
	handlers, rm, _ := newBodyTestHandlers(t)
	payload := []byte(`{"ok":true}`)
	stored := &proxy.CapturedBody{
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		IsBinary:        true,
		Data:            gzipBytes(t, payload),
	}

	// Without include=body, is_binary reflects the stored (raw) flag.
	rm.Record(proxy.RequestRecord{
		ID:         "div0001",
		Timestamp:  time.Now(),
		Method:     "GET",
		URL:        "/data",
		StatusCode: 200,
		Details:    &proxy.RequestDetails{ResponseBody: stored},
	})
	meta := httptest.NewRequest("GET", "/api/v1/proxy/requests/div0001", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "div0001")
	meta = meta.WithContext(context.WithValue(meta.Context(), chi.RouteCtxKey, rctx))
	mw := httptest.NewRecorder()
	handlers.GetProxyRequest(mw, meta)
	var metaResp ProxyRequestDetailResponse
	require.NoError(t, json.NewDecoder(mw.Body).Decode(&metaResp))
	assert.True(t, metaResp.Details.ResponseBody.IsBinary, "stored raw flag is binary")

	// With include=body, is_binary reflects the served (decoded) bytes.
	req := httptest.NewRequest("GET", "/api/v1/proxy/requests/div0001?include=body", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rw := httptest.NewRecorder()
	handlers.GetProxyRequest(rw, req)
	var servedResp ProxyRequestDetailResponse
	require.NoError(t, json.NewDecoder(rw.Body).Decode(&servedResp))
	assert.False(t, servedResp.Details.ResponseBody.IsBinary, "served decoded flag is text")
	assert.Equal(t, string(payload), servedResp.Details.ResponseBody.Data)
}

func TestConvertCapturedBody_NeverExposesFilePath(t *testing.T) {
	handlers, rm, captureDir := newBodyTestHandlers(t)

	// Disk-backed body: write a file into the allowed capture dir.
	payload := []byte(strings.Repeat("x", 100))
	fp := filepath.Join(captureDir, "big_res.bin")
	require.NoError(t, os.WriteFile(fp, payload, 0600))

	resp, raw := getRequestWithBody(t, handlers, rm, "fp00001", &proxy.CapturedBody{
		Size:         int64(len(payload)),
		CapturedSize: int64(len(payload)),
		ContentType:  "text/plain",
		FilePath:     fp,
	})

	// Disk-backed body loads through the API.
	assert.Equal(t, string(payload), resp.Details.ResponseBody.Data)
	// file_path must never appear in the serialized JSON.
	assert.NotContains(t, raw, "file_path")
	assert.NotContains(t, raw, fp)
}

func TestConvertCapturedBody_EvictedFile(t *testing.T) {
	handlers, rm, captureDir := newBodyTestHandlers(t)

	// FilePath points inside the allowed dir but the file does not exist
	// (evicted / daemon restarted).
	fp := filepath.Join(captureDir, "evicted_res.bin")
	resp, raw := getRequestWithBody(t, handlers, rm, "ev00001", &proxy.CapturedBody{
		Size:         50,
		CapturedSize: 50,
		ContentType:  "application/json",
		FilePath:     fp,
	})

	body := resp.Details.ResponseBody
	assert.Equal(t, "evicted", body.UnavailableReason)
	assert.Empty(t, body.Data)
	assert.NotContains(t, raw, "file_path")
}
