# HTTP API Reference

## Base URL

```
http://{host}:{port}/api/v1
```

By default, `{port}` is a dynamically assigned free localhost port chosen at startup — discover it with `prox status` (which reports the API address), or pin it by setting `api.port` in `prox.yaml`. The examples below assume `api.port: 5555` is set in `prox.yaml`; prox itself does not pin a port by default, so substitute whatever port `prox status` reports unless you've explicitly set `api.port`.

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
| `CURSOR_GONE` | A `before_id` cursor's anchor record is unknown, evicted, or out of scope — restart pagination without a cursor |

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
  "api_version": "v1",
  "proxy": {
    "mode": "shared",
    "daemon_reachable": true,
    "daemon_version": "0.2.0",
    "consecutive_failures": 0,
    "last_connected_at": "2025-01-19T10:32:01.123Z",
    "dropped_events": 0,
    "backfill_failures": 0,
    "heal_state": "healthy"
  },
  "dependencies": [
    {
      "name": "postgres",
      "state": "healthy",
      "check": "tcp localhost:5432",
      "start_invoked": false
    }
  ]
}
```

`proxy` reports this project's shared-proxy health and is present whenever a proxy is configured (`mode` is `"disabled"` and the rest of the block is zero-valued when no proxy is configured):

| Field | Description |
|-------|--------------|
| `mode` | `shared` (registered with the shared daemon), `standalone` (this process runs its own proxy listener), or `disabled` |
| `daemon_reachable` | Result of a live `/health` probe against the shared daemon (500ms timeout, cached 2s); always `false` outside `shared` mode |
| `daemon_version` | The shared daemon's reported version, when reachable |
| `consecutive_failures` | The forwarder's current run of failed reconnects to the shared daemon |
| `last_connected_at` | Last time the forwarder established an SSE stream to the shared daemon |
| `dropped_events` | Request-stream events lost to a full subscriber channel |
| `backfill_failures` | Post-connect ring snapshot fetch failures |
| `heal_state` | `healthy`, `healing`, or `version_mismatch`; empty when not in shared mode |

**When `mode` is `shared` and `daemon_reachable` is `false`, proxied routes are dead** — requests through the proxy will fail even though the project's own processes may be healthy. This is not necessarily an error to act on immediately: the project self-heals automatically (re-registers with a fresh or recovered daemon), worst case within ~45s. An agent polling status should treat a brief `daemon_reachable: false` as transient, retry rather than fail the task outright, and only surface it as a real problem if it persists past that window. `prox proxy status` gives daemon-side detail (routes, version) that this per-project block does not.

`dependencies` reports the resolution state of every configured `dependencies:` entry; omitted entirely (never an empty array) when none are configured:

| Field | Description |
|-------|-------------|
| `name` | The dependency's name |
| `state` | `pending`, `checking`, `starting`, `polling`, `healthy`, `warned` (budget exhausted, `on_failure: warn`), `failed` (budget exhausted, `on_failure: fail`), or `canceled` (torn down by shutdown/reset, not a failure) |
| `check` | One-line `"<kind> <target>"` summary, e.g. `tcp localhost:5432` |
| `last_error` | Most recent probe error; omitted when none |
| `start_invoked` | Whether `start:` was launched this generation |

Only `failed` trips `prox status`'s exit-1 contract; `warned` does not (its dependents still run).

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
      "status": "waiting",
      "pid": 0,
      "uptime_seconds": 0,
      "restarts": 0,
      "health": "unknown",
      "waiting_on": ["postgres", "redis"]
    },
    {
      "name": "migrate",
      "status": "completed",
      "pid": 0,
      "uptime_seconds": 9,
      "restarts": 0,
      "health": "unknown",
      "kind": "task"
    }
  ]
}
```

**Status values:** `running`, `stopped`, `starting`, `stopping`, `crashed`, `waiting`, `blocked`, `completed` (task exited `0`)

**Health values:** `healthy`, `unhealthy`, `unknown` (no healthcheck configured — always `unknown` for a task)

| Field | Description |
|-------|-------------|
| `kind` | `"task"` for a `tasks:` entry; omitted for a plain process |
| `waiting_on` | `depends_on` targets still resolving, in declaration order; present only in the `waiting` state |
| `blocked_on` | `depends_on` targets that failed, in declaration order; present only in the `blocked` state |

`pid` is `0` (CLI table shows `-`) for any state with no live process: `waiting`, `blocked`, `stopped`, `completed`.

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

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `subdomain` | string | all | Filter by subdomain |
| `method` | string | all | Filter by HTTP method (GET, POST, etc.) |
| `min_status` | int | — | Minimum status code |
| `max_status` | int | — | Maximum status code |
| `since` | string | — | Only requests at or after this time (RFC3339 timestamp, e.g. `2025-01-19T10:00:00Z`) |
| `url_contains` | string | — | Filter by URL substring (path+query, case-insensitive) |
| `before_id` | string | — | Cursor: page strictly older than this request ID (see below) |
| `limit` | int | 100 | Max requests to return (max 5000 — the ring size) |

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
  "total_count": 250,
  "next_before_id": "9f8e7d6c5b4a"
}
```

Request IDs are 12 hex characters. A record that's still streaming (recorded at response-header time, before the body finished) carries `"in_flight": true` and `duration_ms: 0` until the completion event replaces it in place; both fields are omitted once the request is complete. A record that has been in-flight for more than 5 minutes also carries `"stale": true` — see [In-flight and stale requests](#in-flight-and-stale-requests) below.

**Cursor pagination (`before_id`):** pass the previous page's `next_before_id` as `before_id` to fetch the next older page. `next_before_id` is the ID of the *oldest scanned* record, not the oldest *returned* one — a page that got filtered down to zero results still advances the cursor instead of stalling. It's omitted when the scan reached the end of the ring (no more history). Ring order is arrival order, not strict timestamp order, so an unknown, evicted, or out-of-scope `before_id` (including a record from a different `?subdomain=`/project scope) returns **`410 Gone`** with code `CURSOR_GONE` — on that response, restart pagination without a cursor rather than retrying the same one.

**Example:**

```bash
# Get all recent requests
curl http://localhost:5555/api/v1/proxy/requests

# Filter by subdomain
curl "http://localhost:5555/api/v1/proxy/requests?subdomain=api"

# Filter for errors (5xx)
curl "http://localhost:5555/api/v1/proxy/requests?min_status=500"

# Page backward through history
curl "http://localhost:5555/api/v1/proxy/requests?before_id=9f8e7d6c5b4a"
```

### GET /proxy/requests/{id}

Retrieve details for one proxied request.

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `include` | string | — | Set to `body` to include captured request and response body data |

Body data is available by default whenever the project's proxy is enabled — capture is on unless the project set `proxy.capture.enabled: false` or ran with `--no-capture` — but is not guaranteed for every record: a body may be absent when capture is unavailable in the daemon (`capture_available: false` in daemon status), when the spilled body file was evicted by the disk budget, or once the record falls outside the newest 1000 requests (bodies are retained only for that window even though the ring keeps 5000 records; metadata and headers remain). All of those carry `unavailable_reason: "evicted"`. Stored `request_headers`/`response_headers` values — including sensitive headers (`Authorization`, `Cookie`, etc.) and sensitive query params (in the URL, or inside `Location`/`Referer` header values) — are captured verbatim, with no values altered or hidden. Bodies are likewise captured verbatim, up to `max_body_size` — see `configuration.md` for the full cleartext posture and limits.

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
      "content_encoding": "",
      "is_binary": false,
      "data": "{\"name\":\"Ada\"}"
    }
  }
}
```

`captured_size` is the bytes actually retained after any truncation (vs `size`, the original body size). `content_encoding` records the stored `Content-Encoding` (e.g. `gzip`); gzip/deflate/zstd/brotli bodies are decoded transparently, so `data`/`is_binary` reflect the decoded (served) bytes when `include=body` is used. If a body can no longer be loaded (its capture file was disk-budget evicted, or the record aged past the newest-1000 captured-body window), the body object carries `"unavailable_reason": "evicted"` instead of `data` — metadata and the record's headers are still there.

**Examples:**

```bash
curl http://localhost:5555/api/v1/proxy/requests/a1b2c3d4e5f6
curl "http://localhost:5555/api/v1/proxy/requests/a1b2c3d4e5f6?include=body"
```

### GET /proxy/requests/stream

Stream proxy requests via Server-Sent Events (SSE).

The stream is long-lived: it is exempt from the 30s request-timeout class and ends only on client disconnect or daemon shutdown.

**Query Parameters:** Same as `GET /proxy/requests` (except `limit`)

**Response:** SSE stream

```
: connected

data: {"id":"a1b2c3d4e5f6","timestamp":"2025-01-19T10:32:01.123Z","method":"GET","url":"/api/users","subdomain":"api","status_code":200,"duration_ms":45,"remote_addr":"127.0.0.1"}
```

**Example:**

```bash
curl -N http://localhost:5555/api/v1/proxy/requests/stream
curl -N "http://localhost:5555/api/v1/proxy/requests/stream?subdomain=api"
```

### In-flight and stale requests

A request appears in `prox requests`/the API as soon as its response headers arrive, before the body finishes streaming, with `in_flight: true`. Normally the completion event replaces it in place once the body finishes. If a request stays `in_flight` for more than 5 minutes, it also gains `stale: true` — this means the completion event may have been lost and the true outcome is unknown, **not** that the request is broken: long-lived streams (SSE, WebSocket-over-HTTP) and large transfers can legitimately sit at `stale: true` for a long time while still live.

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
