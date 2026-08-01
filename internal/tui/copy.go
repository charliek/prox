package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/proxy"
)

// copyFlashClearDelay is shorter than the general status flash (WS10).
const copyFlashClearDelay = 2 * time.Second

// clipboardWriteString is a test seam over atotto/clipboard.
var clipboardWriteString = clipboard.WriteAll

// requestIDCopyPayload returns the full request ID for clipboard (WS10).
func requestIDCopyPayload(id string) string {
	return id
}

// curlCopyHost picks the hostname for curl replay: port-stripped Hostname when
// set, else Subdomain (rows streamed before C10 or odd data — Codex #10).
func curlCopyHost(req proxy.RequestRecord) string {
	if req.Hostname != "" {
		return req.Hostname
	}
	return req.Subdomain
}

// shellSingleQuote wraps s in single quotes with '\” escapes. The curl
// payload is pasted into shells (by users AND agents), and request method/URL
// are attacker-influenceable — an apostrophe in a proxied path must not be
// able to break out of quoting (CodeRabbit PR #102, critical).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// curlCopyPayload builds `curl -X <METHOD> 'https://<host>[:port]<URL>'` per
// plan 021 WS10 / panel B2. ProxyHTTPSPort != 0 includes :port (up --tui);
// 0 omits the port (attach / shared-daemon-on-443). Method and target are
// shellSingleQuote'd.
func curlCopyPayload(req proxy.RequestRecord, proxyHTTPSPort int) string {
	host := curlCopyHost(req)
	url := req.URL
	if !strings.HasPrefix(url, "/") {
		url = "/" + url
	}
	var authority string
	if proxyHTTPSPort != 0 {
		authority = fmt.Sprintf("%s:%d", host, proxyHTTPSPort)
	} else {
		authority = host
	}
	return fmt.Sprintf("curl -X %s %s", shellSingleQuote(req.Method), shellSingleQuote("https://"+authority+url))
}

// detailJSONCopyPayload re-marshals the retained wire response byte-for-byte
// (Codex #10 — RequestDetailData drops hostname, captured_size, …).
func detailJSONCopyPayload(raw *api.ProxyRequestDetailResponse) ([]byte, error) {
	if raw == nil {
		return nil, fmt.Errorf("detail JSON unavailable")
	}
	return json.Marshal(raw)
}

// handleCopyKey routes y/c/Y in ViewModeRequests and ViewModeRequestDetail, and
// logs-view y when a /-search cursor is parked (WS10). Textinput modes never
// reach here (Codex #4 routing).
func (m *ClientModel) handleCopyKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "y":
		return m.handleCopyRequestID()
	case "c":
		return m.handleCopyCurl()
	case "Y":
		return m.handleCopyDetailJSON()
	}
	return false, nil
}

func (m *ClientModel) handleCopyRequestID() (bool, tea.Cmd) {
	switch m.viewMode {
	case ViewModeLogs:
		if m.logCursorIdx < 0 {
			return true, nil
		}
		entries := m.filteredEntries()
		if m.logCursorIdx >= len(entries) {
			return true, nil
		}
		return true, m.writeClipboard(entries[m.logCursorIdx].Line, "")

	case ViewModeRequests:
		req, ok := m.cursorRequestRecord()
		if !ok {
			return true, nil
		}
		return true, m.writeClipboard(
			requestIDCopyPayload(req.ID),
			"copied request id "+req.ID,
		)

	case ViewModeRequestDetail:
		id := m.selectedRequestID
		if id == "" && m.requestDetail != nil {
			id = m.requestDetail.ID
		}
		if id == "" {
			return true, nil
		}
		return true, m.writeClipboard(
			requestIDCopyPayload(id),
			"copied request id "+id,
		)
	}
	return false, nil
}

func (m *ClientModel) handleCopyCurl() (bool, tea.Cmd) {
	var req proxy.RequestRecord
	var ok bool
	switch m.viewMode {
	case ViewModeRequests:
		req, ok = m.cursorRequestRecord()
	case ViewModeRequestDetail:
		req, ok = m.detailRequestForCopy()
	default:
		return false, nil
	}
	if !ok {
		return true, nil
	}
	return true, m.writeClipboard(
		curlCopyPayload(req, m.opts.ProxyHTTPSPort),
		"copied curl",
	)
}

func (m *ClientModel) handleCopyDetailJSON() (bool, tea.Cmd) {
	if m.viewMode != ViewModeRequestDetail {
		// Y is detail-only; no-op in the requests list (WS10).
		return true, nil
	}
	payload, err := detailJSONCopyPayload(m.requestDetailRaw)
	if err != nil {
		return true, m.setStatusFlash(footerError(err.Error()), flashTransient, copyFlashClearDelay)
	}
	return true, m.writeClipboard(string(payload), "copied JSON")
}

func (m *ClientModel) writeClipboard(text, successFlash string) tea.Cmd {
	if err := clipboardWriteString(text); err != nil {
		return m.setStatusFlash(footerError("clipboard unavailable: "+err.Error()), flashTransient, copyFlashClearDelay)
	}
	if successFlash != "" {
		return m.setStatusFlash(footerInfo(successFlash), flashTransient, copyFlashClearDelay)
	}
	return nil
}

func (m ClientModel) cursorRequestRecord() (proxy.RequestRecord, bool) {
	if m.viewMode != ViewModeRequests {
		return proxy.RequestRecord{}, false
	}
	requests := m.filteredProxyRequests()
	if m.cursorIdx < 0 || m.cursorIdx >= len(requests) {
		return proxy.RequestRecord{}, false
	}
	return requests[m.cursorIdx], true
}

func (m ClientModel) detailRequestForCopy() (proxy.RequestRecord, bool) {
	if m.viewMode != ViewModeRequestDetail {
		return proxy.RequestRecord{}, false
	}
	for _, req := range m.proxyRequests {
		if req.ID == m.selectedRequestID {
			return req, true
		}
	}
	if m.requestDetail == nil {
		return proxy.RequestRecord{}, false
	}
	host := ""
	if m.requestDetailRaw != nil {
		host = m.requestDetailRaw.Hostname
	}
	return proxy.RequestRecord{
		ID:        m.requestDetail.ID,
		Method:    m.requestDetail.Method,
		URL:       m.requestDetail.URL,
		Subdomain: m.requestDetail.Subdomain,
		Hostname:  host,
	}, true
}
