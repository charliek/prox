# Phase 3: HTTP API

## Status: COMPLETE

## Objective

Build the HTTP API with all endpoints specified, including SSE streaming for logs.

## Tasks

### Server Setup (`internal/api/server.go`)

- [ ] Server struct with chi router
- [ ] Middleware (logging, recovery, CORS if needed)
- [ ] Graceful shutdown support
- [ ] Configuration (host, port)

### Routes (`internal/api/routes.go`)

- [ ] Route registration
- [ ] API versioning (/api/v1/...)

### Handlers (`internal/api/handlers.go`)

- [ ] GET /status - Supervisor status
- [ ] GET /processes - List all processes
- [ ] GET /processes/{name} - Process details
- [ ] POST /processes/{name}/start
- [ ] POST /processes/{name}/stop
- [ ] POST /processes/{name}/restart
- [ ] GET /logs - Retrieve logs with filtering
- [ ] POST /shutdown - Graceful shutdown

### SSE Streaming (`internal/api/sse.go`)

- [ ] GET /logs/stream - SSE endpoint
- [ ] Filter support (process, pattern, regex)
- [ ] Proper SSE format (data: JSON\n\n)
- [ ] Connection cleanup on client disconnect

### Response Types (`internal/api/responses.go`)

- [ ] StatusResponse
- [ ] ProcessListResponse
- [ ] ProcessDetailResponse
- [ ] LogsResponse
- [ ] ErrorResponse with codes

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | /api/v1/status | Supervisor status |
| GET | /api/v1/processes | List all processes |
| GET | /api/v1/processes/{name} | Process details |
| POST | /api/v1/processes/{name}/start | Start process |
| POST | /api/v1/processes/{name}/stop | Stop process |
| POST | /api/v1/processes/{name}/restart | Restart process |
| GET | /api/v1/logs | Get logs (query params: process, lines, pattern, regex) |
| GET | /api/v1/logs/stream | SSE log stream |
| POST | /api/v1/shutdown | Shutdown supervisor |

## Error Codes

```go
const (
    ErrCodeProcessNotFound      = "PROCESS_NOT_FOUND"
    ErrCodeProcessAlreadyRunning = "PROCESS_ALREADY_RUNNING"
    ErrCodeProcessNotRunning    = "PROCESS_NOT_RUNNING"
    ErrCodeInvalidPattern       = "INVALID_PATTERN"
    ErrCodeShutdownInProgress   = "SHUTDOWN_IN_PROGRESS"
)
```

## Response Examples

### GET /status
```json
{
  "status": "running",
  "uptime_seconds": 7200,
  "config_file": "/path/to/prox.yaml",
  "api_version": "v1"
}
```

### GET /processes
```json
{
  "processes": [
    {
      "name": "web",
      "status": "running",
      "pid": 12345,
      "uptime_seconds": 3600,
      "restarts": 0,
      "health": "unknown"
    }
  ]
}
```

### SSE Format
```
data: {"timestamp":"2025-01-19T10:32:01.123Z","process":"web","stream":"stdout","line":"GET /api/users 200 12ms"}

```

## Verification

```bash
go test ./internal/api/... -v

# Manual verification
go run ./cmd/prox up &
curl http://localhost:5555/api/v1/status
curl http://localhost:5555/api/v1/processes
curl http://localhost:5555/api/v1/logs?lines=10
curl -N http://localhost:5555/api/v1/logs/stream  # SSE stream
curl -X POST http://localhost:5555/api/v1/processes/web/restart
curl -X POST http://localhost:5555/api/v1/shutdown
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [ ] Chi router set up with middleware
- [ ] All endpoints implemented
- [ ] Error responses use proper codes
- [ ] SSE streaming works
- [ ] Log filtering works (process, pattern, regex)
- [ ] Graceful shutdown via API
- [ ] Tests for all handlers
- [ ] Tests pass with race detector
