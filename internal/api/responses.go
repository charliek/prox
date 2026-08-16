package api

import (
	"strings"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// sensitiveEnvPatterns contains patterns that indicate sensitive environment variables
var sensitiveEnvPatterns = []string{
	"PASSWORD",
	"SECRET",
	"KEY",
	"TOKEN",
	"CREDENTIAL",
	"PRIVATE",
	"AUTH",
	"API_KEY",
	"APIKEY",
	"ACCESS_KEY",
	"ACCESSKEY",
}

// StatusResponse represents the response for GET /status
type StatusResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ConfigFile    string `json:"config_file,omitempty"`
	// ProjectDir is the daemon's project directory: the directory it was
	// started in, and the one whose .prox/prox.state it wrote (plan 020 C3).
	// It is the IDENTITY a client checks before acting on an address it
	// discovered from a state file — "does the prox answering on this port
	// belong to the project I am standing in?". The project directory, not the
	// config file, is the right basis: two project roots can legitimately share
	// one config (`prox up -c ../shared/prox.yaml`), and a config-path
	// comparison would then let each control the other.
	//
	// Additive and omitempty, so an older daemon simply omits it and clients
	// fall back to comparing config paths.
	ProjectDir string `json:"project_dir,omitempty"`
	APIVersion string `json:"api_version"`
	// Proxy reports shared-proxy health (D5). Present whenever a
	// ProxyStatusProvider was injected (the normal `prox up` path); omitted only
	// where none is set (e.g. unit-test handlers), so those consumers see no
	// change. The CLI omits the rendered line when Mode is "disabled".
	Proxy *ProxyStatusResponse `json:"proxy,omitempty"`
	// Dependencies reports the resolution state of each configured dependency
	// (plan 013 D5), extending this existing status payload rather than adding a
	// second endpoint so `prox status` reads a consistent snapshot in one fetch.
	// Additive: omitted (nil) when no dependencies are configured.
	Dependencies []DependencyStatusResponse `json:"dependencies,omitempty"`
	// Warnings are this session's user-facing advisories (plan 028 A2), most of
	// them raised where the person who typed the command cannot see them: inside
	// the shared proxy daemon (stdout/stderr are /dev/null) or inside a
	// `prox up -d` child (output goes to .prox/prox.log). Publishing them here is
	// what lets the detached parent print them on the terminal the user is
	// actually looking at, and `prox status` re-print them later.
	Warnings []domain.Warning `json:"warnings,omitempty"`
	// WarningsSealed reports whether every startup warning producer has
	// finished. Some are asynchronous and can still be running when the
	// `prox up -d` parent finishes its readiness + settle wait, so a single
	// status fetch at that moment would race them and silently lose a warning;
	// the parent polls this flag instead (bounded).
	//
	// NOT omitempty: false is the meaningful value the parent polls on, and a
	// key that vanishes exactly when it is false is a key a client cannot
	// distinguish from a daemon that never had the field.
	WarningsSealed bool `json:"warnings_sealed"`
}

// DependencyStatusResponse reports one configured dependency's resolution state
// for GET /status (plan 013 D5). State is the resolver's DepState string
// (pending/checking/starting/polling/healthy/warned/failed/canceled), reported
// as "pending" for a dependency the resolver has not yet begun resolving this
// generation. Check is a one-line summary of the readiness probe
// ("tcp host:port" / "url ..." / "cmd ...").
type DependencyStatusResponse struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	Check        string `json:"check"`
	LastError    string `json:"last_error,omitempty"`
	StartInvoked bool   `json:"start_invoked"`
}

// ProxyStatusResponse reports the health of this project's proxy path (D5),
// surfaced under StatusResponse.proxy. It is the single source of truth `prox
// status` renders for shared-proxy reachability and request-stream health.
type ProxyStatusResponse struct {
	// Mode is "shared", "standalone", or "disabled".
	Mode string `json:"mode"`
	// DaemonReachable is the result of a live /health probe against the shared
	// daemon (shared mode only; always false otherwise).
	DaemonReachable bool `json:"daemon_reachable"`
	// DaemonVersion is the shared daemon's reported version when reachable.
	DaemonVersion string `json:"daemon_version,omitempty"`
	// ConsecutiveFailures is the forwarder's current run of failed reconnects.
	ConsecutiveFailures int64 `json:"consecutive_failures"`
	// LastConnectedAt is the last time the forwarder established an SSE stream.
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
	// DroppedEvents is the number of request-stream events lost to a full
	// subscriber channel (D9), read from the project's local request manager.
	DroppedEvents int64 `json:"dropped_events"`
	// BackfillFailures counts post-connect ring snapshot fetch failures.
	BackfillFailures int64 `json:"backfill_failures"`
	// HealState is "healthy" when the shared daemon is reachable, "" otherwise
	// (C5). C6 refines it to "healing"/"version_mismatch".
	HealState string `json:"heal_state,omitempty"`
	// CaptureEnabled reports whether request/response capture is effectively on
	// for this project (proxy enabled AND capture.enabled), so a client can say
	// WHY a request list or a request detail is empty instead of promising
	// traffic that will never be recorded.
	//
	// It is a POINTER on purpose. The project API carries no version gate --
	// unlike the shared daemon, which requires an exact version match with its
	// clients -- so `prox attach` can legitimately talk to an older daemon that
	// predates this field. A plain bool would decode that daemon's absent field
	// as false and assert "capture is off" about a daemon that never had an
	// opinion. nil means UNKNOWN and callers must fall back to their
	// capture-agnostic wording rather than treating it as disabled.
	CaptureEnabled *bool `json:"capture_enabled,omitempty"`
}

// ProxyStatusProvider supplies the proxy block for GET /status. The daemon
// injects an implementation (the cli's proxyRuntime); it is defined here as a
// narrow interface — following the ShutdownController precedent — so the api
// package does not depend on internal/cli or internal/proxyd and tests can
// drive GetStatus with a fake.
type ProxyStatusProvider interface {
	ProxyStatus() *ProxyStatusResponse
}

// WarningProvider supplies this session's user-facing warnings, and the
// completion latch that says whether every startup producer has finished, for
// GET /status (plan 028 A2). The daemon injects an implementation (the cli's
// warningSink); like ProxyStatusProvider it is defined here as a narrow
// interface so the api package does not depend on internal/cli and tests can
// drive GetStatus with a fake.
//
// Two methods rather than one returning both, so each maps to exactly one
// response field and neither can be silently swapped for the other.
type WarningProvider interface {
	Warnings() []domain.Warning
	WarningsSealed() bool
}

// ProcessListResponse represents the response for GET /processes
type ProcessListResponse struct {
	Processes []ProcessResponse `json:"processes"`
}

// ProcessResponse represents a single process in responses
type ProcessResponse struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Restarts      int    `json:"restarts"`
	Health        string `json:"health"`
	// Kind is the child's run mode ("process" or "task"), plumbed from
	// domain.ProcessInfo.Kind (plan 013 D3). Rendering lands in C5; here it is
	// carried mechanically so clients can distinguish tasks.
	Kind string `json:"kind,omitempty"`
	// WaitingOn lists the depends_on targets a waiting process is gated on, in
	// declaration order (plan 013 D5); empty in every other state. Additive:
	// omitted (nil) unless the process is waiting.
	WaitingOn []string `json:"waiting_on,omitempty"`
	// BlockedOn lists the depends_on targets that failed and left a process
	// blocked, in declaration order (plan 013 D5); empty in every other state.
	BlockedOn []string `json:"blocked_on,omitempty"`
}

// ProcessDetailResponse represents the response for GET /processes/{name}
type ProcessDetailResponse struct {
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	PID           int               `json:"pid"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Restarts      int               `json:"restarts"`
	Health        string            `json:"health"`
	Healthcheck   *HealthcheckInfo  `json:"healthcheck,omitempty"`
	Cmd           string            `json:"cmd"`
	Env           map[string]string `json:"env,omitempty"`
	// StopTimeout is the effective SIGTERM->SIGKILL escalation budget as a
	// duration string (e.g. "10s"), so users can see the budget governing a
	// stop/restart. Omitted only when unset (process built outside the
	// supervisor's normal resolution path).
	StopTimeout string `json:"stop_timeout,omitempty"`
}

// HealthcheckInfo represents health check details
type HealthcheckInfo struct {
	Enabled             bool   `json:"enabled"`
	LastCheck           string `json:"last_check,omitempty"`
	LastOutput          string `json:"last_output,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// LogsResponse represents the response for GET /logs
type LogsResponse struct {
	Logs          []LogEntryResponse `json:"logs"`
	FilteredCount int                `json:"filtered_count"`
	TotalCount    int                `json:"total_count"`
	// StreamID identifies the logs.Manager lifetime Logs and the two bounds
	// below came from (plan 017 C8). A client comparing this against a
	// StreamID it saw earlier learns whether the daemon restarted underneath
	// it, in which case every Seq it holds belongs to a dead epoch and it must
	// re-sync from scratch rather than ask for "everything after seq N".
	StreamID string `json:"stream_id"`
	// OldestSeq and LatestSeq describe the CURRENT BUFFER as a whole --
	// ignoring both the filter and any since_seq cursor -- at the moment Logs
	// was read, taken from the SAME buffer snapshot as Logs (see
	// logs.Manager.QueryFromSeq). Both are 0 when the buffer is empty. They
	// let a caller tell "caught up" apart from "the buffer rolled past me":
	// OldestSeq > since_seq+1 means entries in between were evicted.
	OldestSeq uint64 `json:"oldest_seq"`
	LatestSeq uint64 `json:"latest_seq"`
}

// LogEntryResponse represents a single log entry
type LogEntryResponse struct {
	Timestamp string `json:"timestamp"`
	Process   string `json:"process"`
	Stream    string `json:"stream"`
	Line      string `json:"line"`
	// Seq is the server ingest sequence (domain.LogEntry.Seq), copied through
	// unchanged so a client can resume a query or an SSE stream with
	// since_seq=Seq (plan 017 C8). Zero on an entry that never passed through
	// logs.Manager.Write.
	Seq uint64 `json:"seq"`
}

// HandshakeResponse is the body of the "handshake" SSE event StreamLogs sends
// immediately after the ": connected" comment, before any log data (plan 017
// C8). A reconnecting client must learn the CURRENT stream epoch before it can
// decide how to backfill: if StreamID differs from the one it last resumed
// against, every Seq it holds belongs to a dead logs.Manager lifetime and it
// must re-sync from scratch instead of asking for "everything after seq N".
//
// This rides a named "event: handshake" frame rather than a bare "data:"
// line specifically so it is NOT mistaken for a log entry: the generic SSE
// reader (internal/cli/client.go readSSE) ignores "event:" lines and would
// otherwise hand this payload's "data:" line straight to parseSSELogEntry,
// which unmarshals into LogEntryResponse leniently (unknown fields ignored)
// and would produce a phantom empty log row. parseSSELogEntry guards against
// exactly that by rejecting any parse whose Process, Line, and Seq are all
// zero-valued -- a real log entry never has all three empty.
type HandshakeResponse struct {
	StreamID string `json:"stream_id"`
}

// SuccessResponse represents a simple success response
type SuccessResponse struct {
	Success bool `json:"success"`
}

// ShutdownFailureResponse describes one process whose group survived the
// full-stop shutdown. Code is the stable machine-readable classifier
// (e.g. PROCESS_GROUP_NOT_REAPED) so tooling can branch without string matching.
type ShutdownFailureResponse struct {
	Process string `json:"process"`
	Error   string `json:"error"`
	Code    string `json:"code"`
}

// ShutdownResponse is the body of POST /api/v1/shutdown?wait=true once the
// process-stop verdict has landed. Success is true only when Failures is empty.
// Waited is always true on this path; its presence (vs its absence on the legacy
// async 200, which sends a bare SuccessResponse) is how an older CLI detects that
// it reached a waited-capable daemon. The endpoint responds HTTP 200 even when
// Failures is non-empty, because the CLI discards structured bodies on non-2xx
// responses — the survivor list must ride a 200 to be read (#36, D4).
type ShutdownResponse struct {
	Success  bool                      `json:"success"`
	Waited   bool                      `json:"waited"`
	Failures []ShutdownFailureResponse `json:"failures"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// ToProcessResponse converts domain.ProcessInfo to ProcessResponse
func ToProcessResponse(info domain.ProcessInfo) ProcessResponse {
	return ProcessResponse{
		Name:          info.Name,
		Status:        string(info.State),
		PID:           info.PID,
		UptimeSeconds: info.UptimeSeconds(),
		Restarts:      info.RestartCount,
		Health:        string(info.Health),
		Kind:          string(info.Kind),
		WaitingOn:     info.WaitingOn,
		BlockedOn:     info.BlockedOn,
	}
}

// ToProcessDetailResponse converts domain.ProcessInfo to ProcessDetailResponse
func ToProcessDetailResponse(info domain.ProcessInfo) ProcessDetailResponse {
	resp := ProcessDetailResponse{
		Name:          info.Name,
		Status:        string(info.State),
		PID:           info.PID,
		UptimeSeconds: info.UptimeSeconds(),
		Restarts:      info.RestartCount,
		Health:        string(info.Health),
		Cmd:           info.Cmd,
		Env:           filterSensitiveEnv(info.Env),
	}

	if info.StopTimeout > 0 {
		resp.StopTimeout = info.StopTimeout.String()
	}

	if info.HealthDetails != nil {
		resp.Healthcheck = &HealthcheckInfo{
			Enabled:             info.HealthDetails.Enabled,
			LastOutput:          info.HealthDetails.LastOutput,
			ConsecutiveFailures: info.HealthDetails.ConsecutiveFailures,
		}
		if !info.HealthDetails.LastCheck.IsZero() {
			resp.Healthcheck.LastCheck = info.HealthDetails.LastCheck.Format(time.RFC3339)
		}
	}

	return resp
}

// filterSensitiveEnv filters out sensitive environment variables
// Variables matching sensitive patterns have their values replaced with "[REDACTED]"
func filterSensitiveEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}

	filtered := make(map[string]string, len(env))
	for key, value := range env {
		if isSensitiveEnvVar(key) {
			filtered[key] = "[REDACTED]"
		} else {
			filtered[key] = value
		}
	}
	return filtered
}

// isSensitiveEnvVar checks if an environment variable name matches sensitive patterns
func isSensitiveEnvVar(name string) bool {
	upperName := strings.ToUpper(name)
	for _, pattern := range sensitiveEnvPatterns {
		if strings.Contains(upperName, pattern) {
			return true
		}
	}
	return false
}

// ToLogEntryResponse converts domain.LogEntry to LogEntryResponse
func ToLogEntryResponse(entry domain.LogEntry) LogEntryResponse {
	return LogEntryResponse{
		Timestamp: entry.Timestamp.Format(time.RFC3339Nano),
		Process:   entry.Process,
		Stream:    string(entry.Stream),
		Line:      entry.Line,
		Seq:       entry.Seq,
	}
}

// ProxyRequestResponse represents a single proxy request
type ProxyRequestResponse struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	Subdomain  string `json:"subdomain"`
	Hostname   string `json:"hostname,omitempty"` // full hostname, port stripped (e.g. api.local.dev)
	StatusCode int    `json:"status_code"`
	DurationMs int64  `json:"duration_ms"`
	RemoteAddr string `json:"remote_addr"`
	// InFlight marks a record published at response-header time, before the
	// body finished streaming; DurationMs stays 0 until the completion event
	// replaces this record (same ID). Omitted for completed records so their
	// JSON stays byte-identical to the pre-in-flight wire format.
	InFlight bool `json:"in_flight,omitempty"`
	// Stale marks an in-flight record that has been in-flight for longer than
	// constants.InFlightStaleAfter as of conversion time (D8, #53). It means
	// "completion unknown" (the completion event may have been lost), not
	// "broken" — long-lived streams and large transfers can legitimately stay
	// in-flight this long. Always false (and omitted) for completed records.
	Stale bool `json:"stale,omitempty"`
}

// ProxyRequestsResponse represents the response for GET /proxy/requests
type ProxyRequestsResponse struct {
	Requests      []ProxyRequestResponse `json:"requests"`
	FilteredCount int                    `json:"filtered_count"`
	TotalCount    int                    `json:"total_count"`
	// NextBeforeID is the before_id to pass for the next older page
	// (ring-position cursor pagination, D12/#50). Populated whenever the
	// scan didn't reach the ring's oldest record — including on a plain
	// first page (no before_id given), so pollers can start cursoring
	// immediately. Omitted on the last page (scan reached the ring tail).
	NextBeforeID string `json:"next_before_id,omitempty"`
}

// ToProxyRequestResponse converts proxy.RequestRecord to ProxyRequestResponse.
// Staleness (D8) is computed here, at serve/conversion time, against the
// current wall clock — there is no background reaper.
func ToProxyRequestResponse(req proxy.RequestRecord) ProxyRequestResponse {
	return ProxyRequestResponse{
		ID:         req.ID,
		Timestamp:  req.Timestamp.Format(time.RFC3339Nano),
		Method:     req.Method,
		URL:        req.URL,
		Subdomain:  req.Subdomain,
		Hostname:   req.Hostname,
		StatusCode: req.StatusCode,
		DurationMs: req.Duration.Milliseconds(),
		RemoteAddr: req.RemoteAddr,
		InFlight:   req.InFlight,
		Stale:      req.StaleAt(time.Now()),
	}
}

// CapturedBodyResponse represents a captured request or response body in API responses.
// It never exposes the on-disk file_path of a spilled body.
type CapturedBodyResponse struct {
	Size            int64  `json:"size"`
	CapturedSize    int64  `json:"captured_size"` // Bytes retained after truncation
	Truncated       bool   `json:"truncated,omitempty"`
	ContentType     string `json:"content_type,omitempty"`
	ContentEncoding string `json:"content_encoding,omitempty"` // Stored Content-Encoding (e.g. "gzip")
	IsBinary        bool   `json:"is_binary,omitempty"`        // With include=body: the SERVED (post-decode) bytes; otherwise the stored raw-bytes classification
	Data            string `json:"data,omitempty"`             // base64 for binary, plain text otherwise
	// UnavailableReason is set (e.g. "evicted") when include=body was requested
	// but the body could no longer be loaded; Data is empty in that case.
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// RequestDetailsResponse represents captured request/response details in API responses
type RequestDetailsResponse struct {
	RequestHeaders  map[string][]string   `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string   `json:"response_headers,omitempty"`
	RequestBody     *CapturedBodyResponse `json:"request_body,omitempty"`
	ResponseBody    *CapturedBodyResponse `json:"response_body,omitempty"`
}

// ProxyRequestDetailResponse extends ProxyRequestResponse with captured details
type ProxyRequestDetailResponse struct {
	ProxyRequestResponse
	Details *RequestDetailsResponse `json:"details,omitempty"`
}
