package tui

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

func TestCurlCopyPayload_WithPort(t *testing.T) {
	req := proxy.RequestRecord{
		Method:   "POST",
		Hostname: "api.local.dev",
		URL:      "/orders?id=1",
	}
	assert.Equal(t,
		"curl -X 'POST' 'https://api.local.dev:6789/orders?id=1'",
		curlCopyPayload(req, 6789),
	)
}

func TestCurlCopyPayload_PortlessAttach(t *testing.T) {
	req := proxy.RequestRecord{
		Method:   "get",
		Hostname: "api.local.dev",
		URL:      "/health",
	}
	assert.Equal(t,
		"curl -X 'get' 'https://api.local.dev/health'",
		curlCopyPayload(req, 0),
	)
}

func TestCurlCopyPayload_SubdomainFallback(t *testing.T) {
	req := proxy.RequestRecord{
		Method:    "PUT",
		Subdomain: "api",
		URL:       "/v1/x",
	}
	assert.Equal(t,
		"curl -X 'PUT' 'https://api:8443/v1/x'",
		curlCopyPayload(req, 8443),
	)
}

func TestCurlCopyPayload_PrefersHostnameOverSubdomain(t *testing.T) {
	req := proxy.RequestRecord{
		Method:    "DELETE",
		Hostname:  "full.host.dev",
		Subdomain: "api",
		URL:       "/z",
	}
	assert.Equal(t,
		"curl -X 'DELETE' 'https://full.host.dev/z'",
		curlCopyPayload(req, 0),
	)
}

func TestRequestIDCopyPayload(t *testing.T) {
	assert.Equal(t, "abc123def456", requestIDCopyPayload("abc123def456"))
}

// The payload is pasted into shells by users AND agents; method/URL are
// attacker-influenceable, so apostrophes must not break quoting (CodeRabbit
// PR #102, critical).
func TestCurlCopyPayload_ShellEscapes(t *testing.T) {
	req := proxy.RequestRecord{
		Method:    "GET",
		Subdomain: "api",
		URL:       "/search?q=';rm -rf ~;'",
	}
	got := curlCopyPayload(req, 0)
	assert.Equal(t,
		`curl -X 'GET' 'https://api/search?q='\'';rm -rf ~;'\'''`,
		got)

	req.Method = "G'ET"
	got = curlCopyPayload(req, 0)
	assert.Contains(t, got, `'G'\''ET'`)
}

func TestDetailJSONCopyPayload_ByteExact(t *testing.T) {
	raw := &api.ProxyRequestDetailResponse{
		ProxyRequestResponse: api.ProxyRequestResponse{
			ID:         "req-abc123def4",
			Timestamp:  "2026-01-02T03:04:05.123456789Z",
			Method:     "GET",
			URL:        "/path",
			Subdomain:  "api",
			Hostname:   "api.local.dev",
			StatusCode: 200,
			DurationMs: 12,
			RemoteAddr: "127.0.0.1:12345",
		},
		Details: &api.RequestDetailsResponse{
			RequestHeaders: map[string][]string{"X-Test": {"1"}},
		},
	}
	want, err := json.Marshal(raw)
	require.NoError(t, err)

	got, err := detailJSONCopyPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDetailJSONCopyPayload_Unavailable(t *testing.T) {
	_, err := detailJSONCopyPayload(nil)
	require.Error(t, err)
}

func TestStreamedProxyRequest_MapsHostname(t *testing.T) {
	rec := streamedProxyRequest(nil, api.ProxyRequestResponse{
		ID:        "req-1",
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Method:    "GET",
		URL:       "/x",
		Subdomain: "api",
		Hostname:  "api.local.dev",
	})
	assert.Equal(t, "api.local.dev", rec.Hostname)
}

func TestCopyKeys_RequestList_CurlAndID(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newClientRequestsModel(&stubTUIClient{}, 3, 10)
	m.proxyRequests[2] = proxy.RequestRecord{
		ID: "req-002", Timestamp: time.Unix(2, 0), Method: "POST",
		URL: "/orders", Hostname: "shop.local.dev", StatusCode: 201,
	}
	m.opts.ProxyHTTPSPort = 6789
	// follow pins cursor to newest row (req-002 at index 2)

	m = clientUpdate(m, keyRune('y'))
	assert.Equal(t, "req-002", clipboard)
	assert.Equal(t, "copied request id req-002", m.statusFlash)

	m = clientUpdate(m, keyRune('c'))
	assert.Equal(t, "curl -X 'POST' 'https://shop.local.dev:6789/orders'", clipboard)
}

func TestCopyKeys_DetailView(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	raw := &api.ProxyRequestDetailResponse{
		ProxyRequestResponse: api.ProxyRequestResponse{
			ID: "req-detail", Method: "GET", URL: "/z",
			Hostname: "h.local.dev", StatusCode: 200,
		},
	}
	stub := &stubTUIClient{detailResp: raw}
	m := newClientRequestsModel(stub, 1, 10)
	m.proxyRequests[0] = proxy.RequestRecord{
		ID: "req-detail", Timestamp: time.Unix(0, 0), Method: "GET",
		URL: "/z", Hostname: "h.local.dev", StatusCode: 200,
	}
	m.opts.ProxyHTTPSPort = 443
	requests := m.filteredProxyRequests()
	m.setRequestCursor(requests, 0)

	m = clientUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, ViewModeRequestDetail, m.viewMode)
	m = clientUpdate(m, RequestDetailMsg{
		ID: "req-detail", Seq: m.detailFetchSeq, Details: &RequestDetailData{
			ID: "req-detail", Method: "GET", URL: "/z",
		},
		Raw: raw,
	})

	m = clientUpdate(m, keyRune('y'))
	assert.Equal(t, "req-detail", clipboard)

	m = clientUpdate(m, keyRune('c'))
	assert.Equal(t, "curl -X 'GET' 'https://h.local.dev:443/z'", clipboard)

	wantJSON, err := json.Marshal(raw)
	require.NoError(t, err)
	m = clientUpdate(m, keyRune('Y'))
	assert.Equal(t, string(wantJSON), clipboard)
	assert.Equal(t, "copied JSON", m.statusFlash)
}

func TestCopyKeys_YNoOpInRequestList(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newClientRequestsModel(&stubTUIClient{}, 2, 10)
	clientUpdate(m, keyRune('Y'))
	assert.Empty(t, clipboard)
}

func TestCopyKeys_LogsYParkedCursor(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newTestModel()
	m.viewMode = ViewModeLogs
	m.logEntries = []domain.LogEntry{{Line: "alpha"}, {Line: "beta NEEDLE"}}
	m.logSearchQuery = "NEEDLE"
	m.seekLogSearchMatch(0)
	require.GreaterOrEqual(t, m.logCursorIdx, 0)

	m = clientUpdate(m, keyRune('y'))
	assert.Equal(t, "beta NEEDLE", clipboard)
}

func TestCopyKeys_LogsYNoOpWithoutCursor(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newTestModel()
	m.viewMode = ViewModeLogs
	m.logEntries = []domain.LogEntry{{Line: "alpha"}}
	m.logCursorIdx = -1

	m = clientUpdate(m, keyRune('y'))
	assert.Empty(t, clipboard)
}

func TestCopyKeys_ClipboardFailure(t *testing.T) {
	prev := clipboardWriteString
	clipboardWriteString = func(string) error {
		return errors.New("no display")
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newClientRequestsModel(&stubTUIClient{}, 1, 10)
	m = clientUpdate(m, keyRune('y'))
	assert.Equal(t, "clipboard unavailable: no display", m.statusFlash)
}

func TestCopyKeys_NoOpInTextinputModes(t *testing.T) {
	var clipboard string
	prev := clipboardWriteString
	clipboardWriteString = func(s string) error {
		clipboard = s
		return nil
	}
	t.Cleanup(func() { clipboardWriteString = prev })

	m := newClientRequestsModel(&stubTUIClient{}, 2, 10)
	m.mode = ModeSearch
	m.textInput.Focus()
	m = clientUpdate(m, keyRune('y'))
	m = clientUpdate(m, keyRune('c'))
	m = clientUpdate(m, keyRune('Y'))
	assert.Empty(t, clipboard)

	m.mode = ModeStringFilter
	m.textInput.Focus()
	m = clientUpdate(m, keyRune('y'))
	assert.Empty(t, clipboard)
}

func TestNewClientModel_ProxyPortsFromOptions(t *testing.T) {
	m := NewClientModel(&stubTUIClient{}, ClientOptions{
		ProxyHTTPSPort: 6789,
		ProxyHTTPPort:  6788,
	})
	assert.Equal(t, 6789, m.opts.ProxyHTTPSPort)
	assert.Equal(t, 6788, m.opts.ProxyHTTPPort)
}
