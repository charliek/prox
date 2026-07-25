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

// newRedactProxy registers req and returns a DynamicProxy over the shared
// registry/managers/capture manager.
func newRedactProxy(t *testing.T, reg *Registry, ms *Managers, cm *proxy.CaptureManager, req RegisterRequest) *DynamicProxy {
	t.Helper()
	if _, _, err := reg.Register(req); err != nil {
		t.Fatalf("Register(%s): %v", req.ProjectDir, err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewDynamicProxy(reg, nil, ms, cm, logger)
}

func serveRedactHost(dp *DynamicProxy, host, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
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

// TestDynamicProxy_Redaction_EndToEnd asserts that a capture-enabled,
// redaction-on route redacts sensitive headers AND the recorded URL BEFORE
// publication, while the upstream backend still receives the raw Authorization
// header and the original (unredacted) query byte-for-byte (plan 012 D4).
func TestDynamicProxy_Redaction_EndToEnd(t *testing.T) {
	var gotAuth, gotRawQuery string
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
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
	req.Redact = true
	dp := newRedactProxy(t, reg, ms, cm, req)

	rec := serveRedactHost(dp, "api.local.dev", "POST", "http://api.local.dev/echo?code=SECRETCODE&keep=1", "payload",
		map[string]string{"Authorization": "Bearer topsecret", "X-Api-Key": "k", "X-Public": "ok"})
	require.Equal(t, http.StatusFound, rec.Code)

	// Upstream saw the raw credential and the ORIGINAL query, unredacted.
	assert.Equal(t, "Bearer topsecret", gotAuth, "backend must receive the raw Authorization header")
	assert.Equal(t, "code=SECRETCODE&keep=1", gotRawQuery, "backend must receive the original query")

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	require.Len(t, records, 1)
	r0 := records[0]

	// Stored URL redacted.
	assert.Equal(t, "http://api.local.dev/echo?code=REDACTED&keep=1", r0.URL)

	require.NotNil(t, r0.Details)
	d := r0.Details
	assert.Equal(t, []string{"[REDACTED]"}, d.RequestHeaders["Authorization"])
	assert.Equal(t, []string{"[REDACTED]"}, d.RequestHeaders["X-Api-Key"])
	assert.Equal(t, []string{"ok"}, d.RequestHeaders["X-Public"])
	assert.Equal(t, []string{"[REDACTED]"}, d.ResponseHeaders["Set-Cookie"])
	assert.Equal(t, []string{"https://app.example.com/cb?code=REDACTED&state=1"}, d.ResponseHeaders["Location"])
}

// TestDynamicProxy_Redaction_BodylessGET is the panel's critical finding at the
// daemon level: a GET (r.Body nil) still records Details, and its sensitive
// headers must be redacted (plan 012 D4).
func TestDynamicProxy_Redaction_BodylessGET(t *testing.T) {
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	reg := NewRegistry()
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	ms := NewManagers(10, cm.CleanupRequest)
	rm := ms.ensure("/projects/a")

	req := newTestRequest("/projects/a", "local.dev",
		map[string]ServiceTarget{"api": {Host: host, Port: port}}, 80, 0)
	req.CaptureEnabled = true
	req.Redact = true
	dp := newRedactProxy(t, reg, ms, cm, req)

	rec := serveRedactHost(dp, "api.local.dev", "GET", "http://api.local.dev/me", "",
		map[string]string{"Authorization": "Bearer secret", "Cookie": "s=1"})
	require.Equal(t, http.StatusOK, rec.Code)

	records := rm.Recent(proxy.RequestFilter{ProjectDir: "/projects/a"})
	require.Len(t, records, 1)
	require.NotNil(t, records[0].Details)
	assert.Equal(t, []string{"[REDACTED]"}, records[0].Details.RequestHeaders["Authorization"])
	assert.Equal(t, []string{"[REDACTED]"}, records[0].Details.RequestHeaders["Cookie"])
}

// TestDynamicProxy_Redaction_CrossProjectIsolation pins point 6: two projects on
// the ONE shared daemon — one with redaction on, one with redact=false — each
// have their records redacted per their OWN policy, never the other's.
func TestDynamicProxy_Redaction_CrossProjectIsolation(t *testing.T) {
	host, port := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	})

	reg := NewRegistry()
	cm, err := proxy.NewCaptureManagerAt(t.TempDir(), constants.DefaultCaptureMaxBodySize)
	require.NoError(t, err)
	ms := NewManagers(10, cm.CleanupRequest)
	rmOn := ms.ensure("/projects/on")
	rmOff := ms.ensure("/projects/off")

	reqOn := newTestRequest("/projects/on", "on.dev",
		map[string]ServiceTarget{"api": {Host: host, Port: port}}, 80, 0)
	reqOn.CaptureEnabled = true
	reqOn.Redact = true
	if _, _, err := reg.Register(reqOn); err != nil {
		t.Fatalf("Register on: %v", err)
	}

	reqOff := newTestRequest("/projects/off", "off.dev",
		map[string]ServiceTarget{"api": {Host: host, Port: port}}, 80, 0)
	reqOff.CaptureEnabled = true
	reqOff.Redact = false
	if _, _, err := reg.Register(reqOff); err != nil {
		t.Fatalf("Register off: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dp := NewDynamicProxy(reg, nil, ms, cm, logger)

	hdrs := map[string]string{"Authorization": "Bearer secret"}
	require.Equal(t, http.StatusOK, serveRedactHost(dp, "api.on.dev", "POST", "http://api.on.dev/x?token=T", "b", hdrs).Code)
	require.Equal(t, http.StatusOK, serveRedactHost(dp, "api.off.dev", "POST", "http://api.off.dev/x?token=T", "b", hdrs).Code)

	on := rmOn.Recent(proxy.RequestFilter{ProjectDir: "/projects/on"})
	require.Len(t, on, 1)
	assert.Equal(t, "http://api.on.dev/x?token=REDACTED", on[0].URL)
	require.NotNil(t, on[0].Details)
	assert.Equal(t, []string{"[REDACTED]"}, on[0].Details.RequestHeaders["Authorization"])

	off := rmOff.Recent(proxy.RequestFilter{ProjectDir: "/projects/off"})
	require.Len(t, off, 1)
	assert.Equal(t, "http://api.off.dev/x?token=T", off[0].URL, "redact=false project keeps its URL verbatim")
	require.NotNil(t, off[0].Details)
	assert.Equal(t, []string{"Bearer secret"}, off[0].Details.RequestHeaders["Authorization"], "redact=false project keeps its headers verbatim")
}
