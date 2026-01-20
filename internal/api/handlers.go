package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// Handlers contains all HTTP handlers
type Handlers struct {
	supervisor *supervisor.Supervisor
	logManager *logs.Manager
	configFile string
	shutdownFn func()
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
