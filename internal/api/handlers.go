package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
)

// Handlers contains all HTTP handlers
type Handlers struct {
	supervisor     *supervisor.Supervisor
	logManager     *logs.Manager
	requestManager *proxy.RequestManager
	captureManager *proxy.CaptureManager
	configFile     string
	shutdownFn     func()
}

// NewHandlers creates new HTTP handlers
func NewHandlers(sup *supervisor.Supervisor, logMgr *logs.Manager, configFile string, shutdownFn func()) *Handlers {
	return &Handlers{
		supervisor: sup,
		logManager: logMgr,
		configFile: configFile,
		shutdownFn: shutdownFn,
	}
}

// SetRequestManager sets the proxy request manager for request inspection.
// This uses a setter pattern rather than constructor injection because the
// proxy service is initialized after the API handlers, and the request
// manager comes from the proxy service.
func (h *Handlers) SetRequestManager(rm *proxy.RequestManager) {
	h.requestManager = rm
}

// SetCaptureManager sets the capture manager for loading captured body data.
func (h *Handlers) SetCaptureManager(cm *proxy.CaptureManager) {
	h.captureManager = cm
}

// GetStatus handles GET /api/v1/status
func (h *Handlers) GetStatus(w http.ResponseWriter, r *http.Request) {
	status := h.supervisor.Status()

	resp := StatusResponse{
		Status:        status.State,
		UptimeSeconds: status.UptimeSeconds(),
		ConfigFile:    h.configFile,
		APIVersion:    "v1",
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetProcesses handles GET /api/v1/processes
func (h *Handlers) GetProcesses(w http.ResponseWriter, r *http.Request) {
	processes := h.supervisor.Processes()

	resp := ProcessListResponse{
		Processes: make([]ProcessResponse, len(processes)),
	}

	for i, p := range processes {
		resp.Processes[i] = ToProcessResponse(p)
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetProcess handles GET /api/v1/processes/{name}
func (h *Handlers) GetProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	info, err := h.supervisor.Process(name)
	if err != nil {
		writeError(w, err)
		return
	}

	resp := ToProcessDetailResponse(info)
	writeJSON(w, http.StatusOK, resp)
}

// StartProcess handles POST /api/v1/processes/{name}/start
func (h *Handlers) StartProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := h.supervisor.StartProcess(ctx, name); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
}

// StopProcess handles POST /api/v1/processes/{name}/stop
func (h *Handlers) StopProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := h.supervisor.StopProcess(ctx, name); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
}

// RestartProcess handles POST /api/v1/processes/{name}/restart
func (h *Handlers) RestartProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := h.supervisor.RestartProcess(ctx, name); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
}

// GetLogs handles GET /api/v1/logs
func (h *Handlers) GetLogs(w http.ResponseWriter, r *http.Request) {
	filter, limit, err := parseLogParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: err.Error(),
			Code:  domain.ErrCodeInvalidPattern,
		})
		return
	}

	entries, total, err := h.logManager.QueryLast(filter, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	resp := LogsResponse{
		Logs:          make([]LogEntryResponse, len(entries)),
		FilteredCount: len(entries),
		TotalCount:    total,
	}

	for i, e := range entries {
		resp.Logs[i] = ToLogEntryResponse(e)
	}

	writeJSON(w, http.StatusOK, resp)
}

// Shutdown handles POST /api/v1/shutdown
func (h *Handlers) Shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})

	// Trigger shutdown asynchronously
	go func() {
		time.Sleep(100 * time.Millisecond) // Let response complete
		if h.shutdownFn != nil {
			h.shutdownFn()
		}
	}()
}

// parseLogParams extracts log filter parameters from request
func parseLogParams(r *http.Request) (domain.LogFilter, int, error) {
	filter := domain.LogFilter{}

	// Process filter
	if processes := r.URL.Query().Get("process"); processes != "" {
		filter.Processes = strings.Split(processes, ",")
	}

	// Pattern filter
	filter.Pattern = r.URL.Query().Get("pattern")

	// Regex flag
	if r.URL.Query().Get("regex") == "true" {
		filter.IsRegex = true
	}

	// Lines limit (default 100, max 10000 to prevent DoS)
	limit := constants.DefaultLogLimit
	if linesStr := r.URL.Query().Get("lines"); linesStr != "" {
		if l, err := strconv.Atoi(linesStr); err == nil && l > 0 {
			if l > constants.MaxLogLines {
				limit = constants.MaxLogLines
			} else {
				limit = l
			}
		}
	}

	return filter, limit, nil
}

// writeJSON writes a JSON response
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// writeError writes an error response
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "INTERNAL_ERROR"
	message := "an internal error occurred"

	switch {
	case errors.Is(err, domain.ErrProcessNotFound):
		status = http.StatusNotFound
		code = domain.ErrCodeProcessNotFound
		message = err.Error()
	case errors.Is(err, domain.ErrProcessAlreadyRunning):
		status = http.StatusConflict
		code = domain.ErrCodeProcessAlreadyRunning
		message = err.Error()
	case errors.Is(err, domain.ErrProcessNotRunning):
		status = http.StatusConflict
		code = domain.ErrCodeProcessNotRunning
		message = err.Error()
	case errors.Is(err, domain.ErrInvalidPattern):
		status = http.StatusBadRequest
		code = domain.ErrCodeInvalidPattern
		message = err.Error()
	case errors.Is(err, domain.ErrShutdownInProgress):
		status = http.StatusServiceUnavailable
		code = domain.ErrCodeShutdownInProgress
		message = err.Error()
	default:
		// For unknown errors, log the actual error but return a sanitized message
		// to avoid leaking internal paths or sensitive information
		log.Printf("Internal error: %v", err)
	}

	writeJSON(w, status, ErrorResponse{
		Error: message,
		Code:  code,
	})
}

// GetProxyRequests handles GET /api/v1/proxy/requests
func (h *Handlers) GetProxyRequests(w http.ResponseWriter, r *http.Request) {
	if h.requestManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "proxy not enabled",
			Code:  domain.ErrCodeProxyNotEnabled,
		})
		return
	}

	filter := parseProxyRequestParams(r)

	requests := h.requestManager.Recent(filter)
	total := h.requestManager.Count()

	resp := ProxyRequestsResponse{
		Requests:      make([]ProxyRequestResponse, len(requests)),
		FilteredCount: len(requests),
		TotalCount:    total,
	}

	for i, req := range requests {
		resp.Requests[i] = ToProxyRequestResponse(req)
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetProxyRequest handles GET /api/v1/proxy/requests/{id}
func (h *Handlers) GetProxyRequest(w http.ResponseWriter, r *http.Request) {
	if h.requestManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "proxy not enabled",
			Code:  domain.ErrCodeProxyNotEnabled,
		})
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: "missing request id",
			Code:  domain.ErrCodeMissingRequestID,
		})
		return
	}

	record, found := h.requestManager.GetByID(id)
	if !found {
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			Error: "request not found",
			Code:  domain.ErrCodeRequestNotFound,
		})
		return
	}

	// Check if body content should be included
	includeBody := r.URL.Query().Get("include") == "body"

	resp := ProxyRequestDetailResponse{
		ProxyRequestResponse: ToProxyRequestResponse(record),
	}

	// Include details if available
	if record.Details != nil {
		resp.Details = h.convertRequestDetails(record.Details, includeBody)
	}

	writeJSON(w, http.StatusOK, resp)
}

// convertRequestDetails converts proxy.RequestDetails to RequestDetailsResponse
func (h *Handlers) convertRequestDetails(details *proxy.RequestDetails, includeBody bool) *RequestDetailsResponse {
	if details == nil {
		return nil
	}

	resp := &RequestDetailsResponse{
		RequestHeaders:  details.RequestHeaders,
		ResponseHeaders: details.ResponseHeaders,
	}

	if details.RequestBody != nil {
		resp.RequestBody = h.convertCapturedBody(details.RequestBody, includeBody)
	}

	if details.ResponseBody != nil {
		resp.ResponseBody = h.convertCapturedBody(details.ResponseBody, includeBody)
	}

	return resp
}

// convertCapturedBody converts proxy.CapturedBody to CapturedBodyResponse
func (h *Handlers) convertCapturedBody(body *proxy.CapturedBody, includeData bool) *CapturedBodyResponse {
	if body == nil {
		return nil
	}

	resp := &CapturedBodyResponse{
		Size:        body.Size,
		Truncated:   body.Truncated,
		ContentType: body.ContentType,
		IsBinary:    body.IsBinary,
	}

	if includeData {
		// Load body data (may be from disk)
		var data []byte
		var err error

		if h.captureManager != nil {
			data, err = h.captureManager.LoadBody(body)
		} else if body.Data != nil {
			data = body.Data
		}

		if err != nil {
			log.Printf("Error loading captured body: %v", err)
		} else if data != nil {
			if body.IsBinary {
				// Encode binary data as base64
				resp.Data = base64Encode(data)
			} else {
				resp.Data = string(data)
			}
		}
	}

	return resp
}

// StreamProxyRequests handles GET /api/v1/proxy/requests/stream (SSE)
func (h *Handlers) StreamProxyRequests(w http.ResponseWriter, r *http.Request) {
	if h.requestManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error: "proxy not enabled",
			Code:  domain.ErrCodeProxyNotEnabled,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error: "streaming not supported",
			Code:  domain.ErrCodeStreamingNotSupported,
		})
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	filter := parseProxyRequestParams(r)
	sub := h.requestManager.Subscribe(filter)
	defer h.requestManager.Unsubscribe(sub.ID)

	// Send initial comment to establish connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case req, ok := <-sub.Ch:
			if !ok {
				return
			}

			resp := ToProxyRequestResponse(req)

			data, err := json.Marshal(resp)
			if err != nil {
				continue
			}

			if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseProxyRequestParams extracts proxy request filter parameters
func parseProxyRequestParams(r *http.Request) proxy.RequestFilter {
	filter := proxy.RequestFilter{}

	filter.Subdomain = r.URL.Query().Get("subdomain")
	filter.Method = r.URL.Query().Get("method")

	if minStatus := r.URL.Query().Get("min_status"); minStatus != "" {
		if v, err := strconv.Atoi(minStatus); err == nil {
			filter.MinStatus = v
		}
	}

	if maxStatus := r.URL.Query().Get("max_status"); maxStatus != "" {
		if v, err := strconv.Atoi(maxStatus); err == nil {
			filter.MaxStatus = v
		}
	}

	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339Nano, sinceStr); err == nil {
			filter.Since = t
		}
	}

	limit := constants.DefaultProxyRequestLimit
	if linesStr := r.URL.Query().Get("limit"); linesStr != "" {
		if l, err := strconv.Atoi(linesStr); err == nil && l > 0 && l <= constants.MaxProxyRequests {
			limit = l
		}
	}
	filter.Limit = limit

	return filter
}

// base64Encode encodes data to base64 string
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
