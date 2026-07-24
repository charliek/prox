package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
)

// ShutdownController is the narrow view of the daemon shutdown coordinator the
// shutdown handler needs. The daemon passes its *shutdownCoordinator; a wait=true
// request Trigger()s the sequence, then blocks on Done() to read the latched
// Outcome() (the process-stop verdict). Kept as an interface here so the handler
// package does not depend on the cli package (which owns the coordinator) and so
// tests can drive the handler with a fake (#36, D4).
type ShutdownController interface {
	// Trigger requests shutdown. Idempotent (backed by sync.Once in the daemon).
	Trigger()
	// Done is closed once the shutdown sequence has latched its outcome.
	Done() <-chan struct{}
	// Outcome is the process-stop verdict; valid only after Done() is observed.
	// A nil return means a clean stop (no survivors).
	Outcome() *domain.ProcessStopError
}

// Handlers contains all HTTP handlers
type Handlers struct {
	supervisor     *supervisor.Supervisor
	logManager     *logs.Manager
	requestManager *proxy.RequestManager
	captureManager *proxy.CaptureManager
	configFile     string
	shutdown       ShutdownController
	proxyStatus    ProxyStatusProvider
}

// NewHandlers creates new HTTP handlers. shutdown may be nil in tests that never
// exercise POST /shutdown; the handler guards against it.
func NewHandlers(sup *supervisor.Supervisor, logMgr *logs.Manager, configFile string, shutdown ShutdownController) *Handlers {
	return &Handlers{
		supervisor: sup,
		logManager: logMgr,
		configFile: configFile,
		shutdown:   shutdown,
	}
}

// SetRequestManager sets the proxy request manager for request inspection.
// This uses a setter pattern rather than constructor injection because the
// proxy service is initialized after the API handlers, and the request
// manager comes from the proxy service.
func (h *Handlers) SetRequestManager(rm *proxy.RequestManager) {
	h.requestManager = rm
}

// GetRequestManager returns the proxy request manager, or nil if not set.
func (h *Handlers) GetRequestManager() *proxy.RequestManager {
	return h.requestManager
}

// SetCaptureManager sets the capture manager for loading captured body data.
func (h *Handlers) SetCaptureManager(cm *proxy.CaptureManager) {
	h.captureManager = cm
}

// SetProxyStatusProvider injects the shared-proxy health provider (D5). A setter
// (not a constructor arg) mirrors SetRequestManager: the proxy path resolves
// after the handlers are built, and the provider (the cli's proxyRuntime) is
// wired in then. When unset, GET /status omits the proxy block.
func (h *Handlers) SetProxyStatusProvider(p ProxyStatusProvider) {
	h.proxyStatus = p
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
	if h.proxyStatus != nil {
		resp.Proxy = h.proxyStatus.ProxyStatus()
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

	// No per-handler timeout: the supervisor bounds the operation internally by
	// the process's configured stop budget, and the lifecycle route group caps
	// the request at lifecycleRequestTimeout for hang protection. r.Context()
	// still propagates client disconnect (#35, D2).
	if err := h.supervisor.StartProcess(r.Context(), name); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
}

// StopProcess handles POST /api/v1/processes/{name}/stop
func (h *Handlers) StopProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// No per-handler timeout: the supervisor bounds the stop internally by the
	// process's configured stop budget; the lifecycle route group provides the
	// hang-protection ceiling. r.Context() still propagates client disconnect.
	if err := h.supervisor.StopProcess(r.Context(), name); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
}

// RestartProcess handles POST /api/v1/processes/{name}/restart
func (h *Handlers) RestartProcess(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// No per-handler timeout: the supervisor bounds the stop half internally by
	// the process's configured stop budget; the lifecycle route group provides
	// the hang-protection ceiling. r.Context() still propagates client
	// disconnect.
	if err := h.supervisor.RestartProcess(r.Context(), name); err != nil {
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

// Shutdown handles POST /api/v1/shutdown.
//
// With ?wait=true (exactly the string "true") the handler triggers the shutdown
// sequence and blocks until the daemon latches the process-stop verdict, then
// returns it as a ShutdownResponse. Any other or absent wait value keeps the
// legacy async behavior: ack 200 immediately, then trigger after a short delay so
// the response flushes first. The route lives in the lifecycle timeout group
// (see registerRoutes) because the waited path can block for the whole drain.
func (h *Handlers) Shutdown(w http.ResponseWriter, r *http.Request) {
	if h.shutdown != nil && r.URL.Query().Get("wait") == "true" {
		h.shutdownWait(w, r)
		return
	}

	// Legacy async path: ack immediately, then trigger after a short delay so the
	// response completes first. Trigger is idempotent, so a duplicate POST is
	// safe. h.shutdown may be nil (unit-test handler that never exercises this
	// path); guard before triggering, but the ack itself is unconditional so a
	// nil coordinator still preserves the legacy response shape.
	writeJSON(w, http.StatusOK, SuccessResponse{Success: true})
	if h.shutdown != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // Let response complete
			h.shutdown.Trigger()
		}()
	}
}

// shutdownWait triggers shutdown and blocks until the coordinator latches the
// process-stop verdict, then writes it. It always responds HTTP 200 — even when a
// process group survived — because the CLI discards structured bodies on non-2xx
// responses; the survivor list must ride a 200 to reach the client (#36, D4). If
// the client disconnects first, shutdown proceeds regardless (already triggered)
// and nothing is written to the dead connection.
func (h *Handlers) shutdownWait(w http.ResponseWriter, r *http.Request) {
	h.shutdown.Trigger()

	select {
	case <-h.shutdown.Done():
		outcome := h.shutdown.Outcome()
		resp := ShutdownResponse{
			Waited:   true,
			Failures: shutdownFailures(outcome),
		}
		resp.Success = len(resp.Failures) == 0
		writeJSON(w, http.StatusOK, resp)
	case <-r.Context().Done():
		// Client gone; nothing to write. Shutdown is already in progress.
	}
}

// shutdownFailures flattens a *ProcessStopError into wire failures. Always
// returns a non-nil slice so the JSON body carries "failures": [] on a clean stop.
func shutdownFailures(outcome *domain.ProcessStopError) []ShutdownFailureResponse {
	failures := make([]ShutdownFailureResponse, 0)
	if outcome == nil {
		return failures
	}
	for _, f := range outcome.Failures {
		failures = append(failures, ShutdownFailureResponse{
			Process: f.Name,
			Error:   f.Err.Error(),
			Code:    domain.ErrorCode(f.Err),
		})
	}
	return failures
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
	case errors.Is(err, domain.ErrEnvReloadFailed):
		// The detail (e.g. which env_file failed to load) is the user's own
		// config path, not sensitive, so it's safe to surface directly.
		status = http.StatusInternalServerError
		code = domain.ErrCodeEnvReloadFailed
		message = err.Error()
	case errors.Is(err, domain.ErrProcessGroupNotReaped):
		// Surface the process name/detail so `prox stop`/`restart` report which
		// process's group could not be terminated (systemd/docker-style loud
		// failure). Non-sensitive: it's the user's own process name.
		status = http.StatusInternalServerError
		code = domain.ErrCodeProcessGroupNotReaped
		message = err.Error()
	case errors.Is(err, domain.ErrConfigReloadFailed):
		// The config was re-read on an API-driven (re)start and could not be
		// loaded/validated. The detail is the user's own config path/validation
		// message, not sensitive, so it is surfaced directly (#33, D3).
		status = http.StatusUnprocessableEntity
		code = domain.ErrCodeConfigReloadFailed
		message = err.Error()
	case errors.Is(err, domain.ErrProcessNotInConfig):
		// Target process was removed from the config since `prox up`. The old
		// process keeps running; the user must run `prox up` to reconcile (#33, D3).
		status = http.StatusConflict
		code = domain.ErrCodeProcessNotInConfig
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

	requests, nextBeforeID, anchorFound := h.requestManager.RecentPage(filter)
	if !anchorFound {
		// filter.BeforeID named a record that's unknown, evicted, or out of
		// scope. 410 (not 404): the cursor once pointed at a real ring
		// position that has since aged out — the caller should restart
		// pagination without a cursor rather than retry this one (D12, #50).
		writeJSON(w, http.StatusGone, ErrorResponse{
			Error: "cursor is gone: the before_id record is unknown, evicted, or out of scope",
			Code:  domain.ErrCodeCursorGone,
		})
		return
	}
	total := h.requestManager.Count()

	resp := ProxyRequestsResponse{
		Requests:      make([]ProxyRequestResponse, len(requests)),
		FilteredCount: len(requests),
		TotalCount:    total,
		NextBeforeID:  nextBeforeID,
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

// captureAllowedDirs returns the directories a captured body's FilePath is
// permitted to resolve within: the local capture manager's dir (when present)
// and the shared daemon capture dir under the user's home. A socket-supplied
// path outside these is rejected by the loader.
func (h *Handlers) captureAllowedDirs() []string {
	var primary string
	if h.captureManager != nil {
		primary = h.captureManager.CaptureDir()
	}
	return proxy.CaptureAllowedDirs(primary)
}

// convertCapturedBody converts proxy.CapturedBody to CapturedBodyResponse.
//
// Metadata fields are always populated. When includeData is set, the body is
// loaded and content-decoded via proxy.LoadDecodedBody: is_binary in the
// response reflects the served (decoded) bytes, binary data is base64-encoded,
// and a body that could no longer be loaded reports unavailable_reason with no
// data (HTTP 200 preserved). file_path is never exposed.
func (h *Handlers) convertCapturedBody(body *proxy.CapturedBody, includeData bool) *CapturedBodyResponse {
	if body == nil {
		return nil
	}

	resp := &CapturedBodyResponse{
		Size:            body.Size,
		CapturedSize:    body.CapturedSize,
		Truncated:       body.Truncated,
		ContentType:     body.ContentType,
		ContentEncoding: body.ContentEncoding,
		IsBinary:        body.IsBinary,
	}

	if !includeData {
		return resp
	}

	decoded, err := proxy.LoadDecodedBody(body, h.captureAllowedDirs())
	if err != nil {
		log.Printf("Error loading captured body: %v", err)
		resp.UnavailableReason = decoded.UnavailableReason
		return resp
	}

	if !decoded.Available {
		resp.UnavailableReason = decoded.UnavailableReason
		return resp
	}

	// Report served (decoded) binary semantics.
	resp.IsBinary = decoded.IsBinary

	if len(decoded.Data) == 0 {
		return resp
	}

	switch {
	case decoded.IsBinary:
		resp.Data = base64Encode(decoded.Data)
	case utf8.Valid(decoded.Data):
		resp.Data = string(decoded.Data)
	default:
		// Defense in depth: never JSON-encode invalid UTF-8 as a string even if
		// the classifier considered it text.
		resp.IsBinary = true
		resp.Data = base64Encode(decoded.Data)
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
	filter.URLContains = r.URL.Query().Get("url_contains")
	filter.BeforeID = r.URL.Query().Get("before_id")

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
