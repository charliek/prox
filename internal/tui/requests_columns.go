package tui

import (
	"fmt"
	"strings"

	"github.com/charliek/prox/internal/proxy"
)

// requestsColumnKey identifies a column in the requests table.
type requestsColumnKey int

const (
	reqColTime requestsColumnKey = iota
	reqColHost
	reqColMethod
	reqColStatus
	reqColDuration
	reqColID
	reqColURL
)

// requestsColumnDef is one entry in the shared requests-table column spec.
// Header and data formatters both read widths from here so they cannot diverge.
type requestsColumnDef struct {
	key       requestsColumnKey
	label     string
	width     int // 0 = variable (URL)
	sepBefore int // spaces before this column when it is not the first visible one
}

// requestsColumnSpec is the single source of truth for requests-view column
// labels, widths, and inter-column spacing (plan 024 F2).
var requestsColumnSpec = []requestsColumnDef{
	{key: reqColTime, label: "Time", width: 8, sepBefore: 2},
	{key: reqColHost, label: "Host", width: 10, sepBefore: 2},
	{key: reqColMethod, label: "Method", width: 7, sepBefore: 2},
	{key: reqColStatus, label: "Status", width: 6, sepBefore: 1},
	{key: reqColDuration, label: "Duration", width: 8, sepBefore: 1},
	{key: reqColID, label: "ID", width: 8, sepBefore: 2},
	{key: reqColURL, label: "URL", width: 0, sepBefore: 2},
}

// requestsColGap is the unstyled inter-column gap; only sepBefore 1 and 2 appear
// in the spec. Styled at render time so theme/FullFill BG stays correct.
var requestsColGap = [...]string{"", " ", "  "}

func (c RequestsColumns) columnVisible(key requestsColumnKey) bool {
	switch key {
	case reqColTime:
		return c.Time
	case reqColHost:
		return c.Host
	case reqColMethod:
		return c.Method
	case reqColStatus:
		return c.Status
	case reqColDuration:
		return c.Duration
	case reqColID:
		return c.ID
	case reqColURL:
		return true
	default:
		return false
	}
}

func formatRequestsHeaderCell(def requestsColumnDef) string {
	if def.width == 0 {
		return def.label
	}
	return fmt.Sprintf("%-*s", def.width, def.label)
}

func (b *BaseModel) formatRequestsDataCell(def requestsColumnDef, req proxy.RequestRecord) string {
	switch def.key {
	case reqColTime:
		return styles.Dim.Render(req.Timestamp.Format("15:04:05"))
	case reqColHost:
		return styles.Dim.Render(truncatePadDisplay(req.Subdomain, def.width))
	case reqColMethod:
		plain := fmt.Sprintf("%-*s", def.width, req.Method)
		return httpMethodStyle(req.Method).Render(plain)
	case reqColStatus:
		plain := fmt.Sprintf("%*d", def.width, req.StatusCode)
		switch {
		case req.InFlight || req.StatusCode < 200:
			return styles.Dim.Render(plain)
		case req.StatusCode >= 500:
			return styles.Status5xx.Render(plain)
		case req.StatusCode >= 400:
			return styles.Status4xx.Render(plain)
		case req.StatusCode >= 300:
			return styles.Base.Render(plain)
		case req.StatusCode >= 200:
			return styles.Status2xx.Render(plain)
		}
		return styles.Dim.Render(plain)
	case reqColDuration:
		stale := b.requestIsStale(req)
		plain := padLeftDisplay(requestDurationPlain(req, stale), def.width)
		if stale || req.InFlight {
			return styles.Dim.Render(plain)
		}
		durationMs := req.Duration.Milliseconds()
		switch {
		case durationMs >= 2000:
			return styles.HTTPError.Render(plain)
		case durationMs >= 500:
			return styles.Warn.Render(plain)
		case durationMs >= 100:
			return styles.Base.Render(plain)
		default:
			return styles.HTTPSuccess.Render(plain)
		}
	case reqColID:
		return styles.Dim.Render(shortRequestID(req.ID))
	case reqColURL:
		return styles.Base.Render(req.URL)
	default:
		return ""
	}
}

// requestDurationPlain is the fixed-width duration glyph before column padding.
func requestDurationPlain(req proxy.RequestRecord, stale bool) string {
	durationMs := req.Duration.Milliseconds()
	switch {
	case stale:
		return "stale?"
	case req.InFlight:
		return "  ...ms"
	case durationMs > 9999:
		return "9999+ms"
	default:
		return fmt.Sprintf("%5dms", durationMs)
	}
}

func renderRequestsRow(b *BaseModel, cols RequestsColumns, req proxy.RequestRecord, header bool) string {
	var sb strings.Builder
	first := true
	for _, def := range requestsColumnSpec {
		if !cols.columnVisible(def.key) {
			continue
		}
		if !first {
			sb.WriteString(styles.Base.Render(requestsColGap[def.sepBefore]))
		}
		if header {
			sb.WriteString(styles.Dim.Render(formatRequestsHeaderCell(def)))
		} else {
			sb.WriteString(b.formatRequestsDataCell(def, req))
		}
		first = false
	}
	return sb.String()
}
