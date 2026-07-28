package proxyd

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCleartextProxy registers req and returns a DynamicProxy over the shared
// registry/managers/capture manager.
func newCleartextProxy(t *testing.T, reg *Registry, ms *Managers, cm *proxy.CaptureManager, req RegisterRequest) *DynamicProxy {
	t.Helper()
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register(%s): %v", req.ProjectDir, err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewDynamicProxy(reg, nil, ms, cm, logger)
}

func serveCleartextHost(dp *DynamicProxy, host, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	handler := dp.handler(80)
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestDynamicProxy_CleartextCapture_EndToEnd pins the visible-by-default capture
// posture at the daemon level: a capture-enabled route stores the recorded URL
// (sensitive query param included), the sensitive request headers
// (Authorization, Cookie), and the sensitive response headers (Set-Cookie, plus
// a Location whose own query carries an OAuth code) VERBATIM — while the
// upstream backend receives the request untampered.
func TestDynamicProxy_CleartextCapture_EndToEnd(t *testing.T) {
	var gotAuth, gotCookie, gotRawQuery string
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotRawQuery = r.URL.RawQuery
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Set-Cookie", "sid=SECRET")
		w.Header().Set("Location", "https://app.example.com/cb?code=OAUTHLEAK&state=1")
		w.WriteHeader(http.StatusFound)
	})

	reg := NewRegistry()
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	ms := NewManagers(10, cm.CleanupRequest)
	rm := ms.ensure("/projects/a")

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: host, Port: port}}, 80, 0)
	req.CaptureEnabled = true
	dp := newCleartextProxy(t, reg, ms, cm, req)

	rec := serveCleartextHost(dp, "api.local.dev", "POST", "http://api.local.dev/echo?code=SECRETCODE&keep=1", "payload",
		map[string]string{
			"Authorization": "Bearer topsecret",
			"Cookie":        "session=SECRETSESSION",
			"X-Api-Key":     "k",
			"X-Public":      "ok",
		})
	require.Equal(t, http.StatusFound, rec.Code)

	// Upstream saw the raw credentials and the ORIGINAL query, untampered.
	assert.Equal(t, "Bearer topsecret", gotAuth, "backend must receive the raw Authorization header")
	assert.Equal(t, "session=SECRETSESSION", gotCookie, "backend must receive the raw Cookie header")
	assert.Equal(t, "code=SECRETCODE&keep=1", gotRawQuery, "backend must receive the original query")

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	require.Len(t, records, 1)
	r0 := records[0]

	// Stored URL keeps the sensitive query param verbatim.
	assert.Equal(t, "http://api.local.dev/echo?code=SECRETCODE&keep=1", r0.URL)

	require.NotNil(t, r0.Details)
	d := r0.Details
	assert.Equal(t, []string{"Bearer topsecret"}, d.RequestHeaders["Authorization"])
	assert.Equal(t, []string{"session=SECRETSESSION"}, d.RequestHeaders["Cookie"])
	assert.Equal(t, []string{"k"}, d.RequestHeaders["X-Api-Key"])
	assert.Equal(t, []string{"ok"}, d.RequestHeaders["X-Public"])
	assert.Equal(t, []string{"sid=SECRET"}, d.ResponseHeaders["Set-Cookie"])
	assert.Equal(t, []string{"https://app.example.com/cb?code=OAUTHLEAK&state=1"}, d.ResponseHeaders["Location"])
}
