# HTTP API Reference

## Base URL

```
http://{host}:{port}/api/v1
```

By default, `{port}` is a dynamically assigned free localhost port chosen at startup — discover it with `prox status` (which reports the API address), or pin it by setting `api.port` in `prox.yaml`. The examples below assume `api.port: 5555` is set in `prox.yaml`.

## Authentication

By default, authentication is enabled automatically unless prox binds to a localhost-only address (`127.0.0.1`, `localhost`, or `::1`); binding to `0.0.0.0` or another non-localhost address turns auth on. When enabled, a bearer token is generated and stored in `~/.prox/token`.

This can be overridden with the `api.auth` field in `prox.yaml`:

- Omitted (default): auto-determine based on `api.host` as described above.
- `true`: force authentication on, even when bound to localhost.
- `false`: force authentication off, even when bound to a non-localhost address. prox prints a startup warning in this case, since any network client can then control the supervisor.

Include the token in requests:

```bash
curl -H "Authorization: Bearer <token>" http://0.0.0.0:5555/api/v1/status
```

## Error Format

All errors return JSON:

```json
{
  "error": "human readable message",
  "code": "MACHINE_READABLE_CODE"
}
```

### Error Codes

| Code | Description |
|------|-------------|
| `PROCESS_NOT_FOUND` | Process name does not exist |
| `PROCESS_ALREADY_RUNNING` | Process is already running |
| `PROCESS_NOT_RUNNING` | Process is not running |
| `INVALID_PATTERN` | Invalid regex pattern |
| `SHUTDOWN_IN_PROGRESS` | Supervisor is shutting down |
| `ENV_RELOAD_FAILED` | A process's `env_file` could not be re-read from disk on start |
| `PROCESS_GROUP_NOT_REAPED` | A process's group survived SIGKILL and could not be confirmed terminated |
| `CONFIG_RELOAD_FAILED` | `prox.yaml` could not be re-read/validated on an API-driven (re)start (missing/unreadable file, YAML syntax, or validation failure); the running process is left untouched |
| `PROCESS_NOT_IN_CONFIG` | The target process is no longer present in `prox.yaml` on a (re)start; run `prox up` to reconcile |
| `PROXY_NOT_ENABLED` | Proxy request inspection is unavailable because the proxy is disabled |
| `STREAMING_NOT_SUPPORTED` | The server cannot provide an SSE stream for this request |
| `REQUEST_NOT_FOUND` | Proxy request ID does not exist in the request buffer |
| `MISSING_REQUEST_ID` | Request detail endpoint was called without an ID |

## Endpoints

### GET /health

Plain liveness check, served at the server root (not under `/api/v1`). No authentication required.

**Response:** `200 OK` with plain-text body `ok` (not JSON).

```bash
curl http://localhost:5555/health
```

### GET /status

Supervisor status.

**Response:**

```json
{
  "status": "running",
  "uptime_seconds": 7200,
  "config_file": "/path/to/prox.yaml",
  "api_version": "v1"
}
```

### GET /processes

List all processes.

**Response:**

```json
{
  "processes": [
    {
      "name": "web",
      "status": "running",
      "pid": 12345,
      "uptime_seconds": 3600,
      "restarts": 0,
      "health": "healthy"
    },
    {
      "name": "api",
      "status": "running",
      "pid": 12346,
      "uptime_seconds": 3600,
      "restarts": 1,
      "health": "unhealthy"
    }
  ]
}
```

**Status values:** `running`, `stopped`, `starting`, `stopping`, `crashed`

**Health values:** `healthy`, `unhealthy`, `unknown` (no healthcheck configured)

### GET /processes/{name}

Get detailed process info.

**Response:**

```json
{
  "name": "api",
  "status": "running",
  "pid": 12345,
  "uptime_seconds": 3600,
  "restarts": 2,
  "health": "healthy",
  "healthcheck": {
    "enabled": true,
    "last_check": "2025-01-19T10:32:01.123Z",
    "last_output": "OK",
    "consecutive_failures": 0
  },
  "cmd": "go run ./cmd/server",
  "env": {
    "PORT": "8080"
  },
  "stop_timeout": "30s"
}
```

`stop_timeout` is the effective SIGTERM→SIGKILL escalation budget in force for this process (its own `stop_timeout`, else the global `shutdown_timeout`, else the `10s` default), as a duration string. It is the budget governing a `POST /processes/{name}/stop` or `POST /processes/{name}/restart`. See [Stop Timeout](configuration.md#stop-timeout).

### POST /processes/{name}/start

Start a stopped process.

`prox.yaml` is re-read first and the process is launched with its current config — see the reload behavior documented under [restart](#post-processesnamerestart) below (it applies identically to `start`).

**Response:**

```json
{
  "success": true
}
```

### POST /processes/{name}/stop

Stop a running process.

**Response:**

```json
{
  "success": true
}
```

### POST /processes/{name}/restart

Restart a process (stop then start).

**Config reload:** before launching, prox re-reads and validates the whole `prox.yaml` and applies the target process's **current** config. Applied on (re)start: `cmd`, `healthcheck`, `stop_timeout` (the new value governs the next stop; the restart's own stop half uses the pre-edit budget), and environment inputs (inline `env`, per-process and global `env_file`, including changed file paths). NOT applied (require `prox up`): renames, added/removed processes, and `services`/`proxy`/port changes.

The reload is **fail-closed**: an invalid file (even an unrelated process or the proxy section), a missing referenced env file, or a removed target aborts the (re)start with an error and leaves the existing process running unchanged:

- `CONFIG_RELOAD_FAILED` (HTTP 422) — the file could not be read/parsed/validated.
- `PROCESS_NOT_IN_CONFIG` (HTTP 409) — the target process is no longer in the file; run `prox up` to reconcile.

**Response:**

```json
{
  "success": true
}
```

### GET /logs

Retrieve logs from buffer.

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `process` | string | all | Comma-separated process names |
| `lines` | int | 100 | Max lines to return |
| `pattern` | string | — | Filter pattern |
| `regex` | bool | false | Treat pattern as regex |

The `lines` parameter is capped at 10000.

**Response:**

```json
{
  "logs": [
    {
      "timestamp": "2025-01-19T10:32:01.123Z",
      "process": "web",
      "stream": "stdout",
      "line": "GET /api/users 200 12ms"
    }
  ],
  "filtered_count": 100,
  "total_count": 4523
}
```

### GET /logs/stream

Stream logs via Server-Sent Events (SSE).

The stream is long-lived: it is exempt from the 30s request-timeout class and ends only on client disconnect or daemon shutdown.

**Query Parameters:** Same as `GET /logs` (except `lines`)

**Response:** SSE stream

```
data: {"timestamp":"2025-01-19T10:32:01.123Z","process":"web","stream":"stdout","line":"GET /api/users 200 12ms"}

data: {"timestamp":"2025-01-19T10:32:01.456Z","process":"api","stream":"stderr","line":"WARN: connection pool low"}
```

**Example:**

```bash
curl -N http://localhost:5555/api/v1/logs/stream
curl -N "http://localhost:5555/api/v1/logs/stream?process=web,api"
curl -N "http://localhost:5555/api/v1/logs/stream?pattern=ERROR"
```

### GET /proxy/requests

Retrieve recent proxy requests (requires proxy to be enabled).

When the project runs under the shared daemon, its request data is bridged from
the daemon over the internal socket. On every (re)connect the bridge backfills a
snapshot of the daemon's current ring for this project before resuming the live
stream, closing gaps opened while the subscription was down (bounded by the
daemon ring and the project's registration lifetime). Reconnects re-deliver no
already-seen record, so this endpoint stays free of duplicates.

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `subdomain` | string | all | Filter by subdomain |
| `method` | string | all | Filter by HTTP method (GET, POST, etc.) |
| `min_status` | int | — | Minimum status code |
| `max_status` | int | — | Maximum status code |
| `since` | string | — | RFC3339 timestamp; only requests recorded at or after this time |
| `url_contains` | string | — | Case-insensitive substring match against the request URL (path+query only — never scheme/host) |
| `limit` | int | 100 | Max requests to return (max 1000) |

**Response:**

```json
{
  "requests": [
    {
      "id": "a1b2c3d4e5f6",
      "timestamp": "2025-01-19T10:32:01.123Z",
      "method": "GET",
      "url": "/api/users",
      "subdomain": "api",
      "hostname": "api.local.dev",
      "status_code": 200,
      "duration_ms": 45,
      "remote_addr": "127.0.0.1"
    }
  ],
  "filtered_count": 50,
  "total_count": 250
}
```

`hostname` is the request's Host header with the port stripped (e.g.
`api.local.dev`); it is omitted when not recorded (older records, or a
daemon-side record that predates this field).

**Example:**

```bash
# Get all recent requests
curl http://localhost:5555/api/v1/proxy/requests

# Filter by subdomain
curl "http://localhost:5555/api/v1/proxy/requests?subdomain=api"

# Filter for errors (5xx)
curl "http://localhost:5555/api/v1/proxy/requests?min_status=500"

# Filter by URL substring (path+query, case-insensitive)
curl "http://localhost:5555/api/v1/proxy/requests?url_contains=/api/users"
```

### GET /proxy/requests/{id}

Retrieve details for one proxied request.

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `include` | string | — | Set to `body` to include captured request and response body data |

Body data is available only when capture was enabled with `prox up --capture` or `proxy.capture.enabled: true`.

**Response:**

```json
{
  "id": "a1b2c3d4e5f6",
  "timestamp": "2025-01-19T10:32:01.123Z",
  "method": "POST",
  "url": "/api/users",
  "subdomain": "api",
  "hostname": "api.local.dev",
  "status_code": 201,
  "duration_ms": 45,
  "remote_addr": "127.0.0.1",
  "details": {
    "request_headers": {
      "Content-Type": ["application/json"]
    },
    "request_body": {
      "size": 27,
      "captured_size": 27,
      "truncated": false,
      "content_type": "application/json",
      "is_binary": false,
      "data": "{\"name\":\"Ada\"}"
    }
  }
}
```

**Captured body fields:**

| Field | Description |
|-------|-------------|
| `size` | Total encoded bytes observed by the capture wrapper before truncation (not Content-Length, not the decoded size) |
| `captured_size` | Encoded bytes actually retained; `truncated` is true when `captured_size < size` |
| `truncated` | Whether the body exceeded the 1MB capture cap and was truncated |
| `content_type` | Captured `Content-Type` header value |
| `content_encoding` | Captured `Content-Encoding` header value (e.g. `gzip`); omitted when unencoded |
| `is_binary` | With `include=body`: whether the **served** (post-decode) bytes are binary. Without it: the stored classification of the **raw wire** bytes (a gzip body reports `true` here even when its decoded form would serve as text) — see decoded-body semantics below |
| `data` | Body content (only with `include=body`): plain text for text bodies, base64 for binary |
| `unavailable_reason` | Set (e.g. `evicted`) when `include=body` was requested but the body could no longer be loaded; `data` is absent |

**Decoded-body semantics:** captured bodies store the raw wire bytes and are decoded at serve time. When `content_encoding` is `gzip` or `x-gzip` and the body was not truncated, the decoded bytes are served and `is_binary` reflects the decoded content (readable JSON/text decodes to `is_binary: false`). Unsupported encodings (`deflate`, `br`, `zstd`, chained values like `gzip, br`), truncated bodies, corrupt streams, and payloads whose decoded size would exceed the 10MB cap are served as the raw bytes, base64-encoded, with `is_binary: true` and `content_encoding` preserved. The stored (raw) binary classification and the served `is_binary` may therefore legitimately differ. The on-disk path of a spilled body is never exposed.

**Examples:**

```bash
curl http://localhost:5555/api/v1/proxy/requests/a1b2c3d4e5f6
curl "http://localhost:5555/api/v1/proxy/requests/a1b2c3d4e5f6?include=body"
```

### GET /proxy/requests/stream

Stream proxy requests via Server-Sent Events (SSE).

The stream is long-lived: it is exempt from the 30s request-timeout class and ends only on client disconnect or daemon shutdown.

**Query Parameters:** Same as `GET /proxy/requests` (except `limit`), including `url_contains`

**Response:** SSE stream

```
: connected

data: {"id":"a1b2c3d4e5f6","timestamp":"2025-01-19T10:32:01.123Z","method":"GET","url":"/api/users","subdomain":"api","hostname":"api.local.dev","status_code":200,"duration_ms":45,"remote_addr":"127.0.0.1"}
```

**Example:**

```bash
curl -N http://localhost:5555/api/v1/proxy/requests/stream
curl -N "http://localhost:5555/api/v1/proxy/requests/stream?subdomain=api"
curl -N "http://localhost:5555/api/v1/proxy/requests/stream?url_contains=/api"
```

### POST /shutdown

Gracefully shut down supervisor and all processes.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `wait` | boolean | When exactly `true`, block until the process-stop verdict lands and return it (see below). Any other or absent value uses the legacy async behavior. |

This route is in the lifecycle timeout class (the same long hang-protection ceiling as `start`/`stop`/`restart`), because the `wait=true` path can block for the full drain.

**Legacy async response** (default, `wait` absent or not `true`):

```json
{
  "success": true
}
```

The daemon acks immediately, then tears down in the background. The connection closes as the supervisor terminates.

**Waited response** (`wait=true`):

```json
{
  "success": true,
  "waited": true,
  "failures": []
}
```

The request blocks until every process has been stopped and its group reaped, then returns the verdict. `success` is `true` only when `failures` is empty. When a process group survives shutdown, the response still uses **HTTP 200** (so the structured body is not discarded) and lists each survivor:

```json
{
  "success": false,
  "waited": true,
  "failures": [
    {
      "process": "web",
      "error": "process group could not be terminated: web",
      "code": "PROCESS_GROUP_NOT_REAPED"
    }
  ]
}
```

The `code` field is a stable machine-readable classifier (`PROCESS_GROUP_NOT_REAPED`). The `waited` field is present only on this path; a daemon that predates the `wait` parameter ignores it and returns the legacy body without `waited`, letting clients detect the older daemon.

The daemon tears down in stages, each with its own deadline: the launch gate is closed, the proxy is deregistered/stopped on a short fixed deadline, then every process is stopped concurrently, each on its own configured stop budget (see [Stop Timeout](configuration.md#stop-timeout)); the verdict is then published to any `wait=true` caller, the logs are flushed and closed, and finally the API server is stopped. Graceful timing therefore derives from the configured `shutdown_timeout` / `stop_timeout` values, not a single fixed window.
