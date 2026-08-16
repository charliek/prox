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
| `CURSOR_GONE` | A `before_id` cursor's anchor record is unknown, evicted, or out of scope for the current filters; restart pagination without a cursor |

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
    "heal_state": "healthy",
    "capture_enabled": true
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

`proxy` reports this project's shared-proxy health and is present whenever a proxy is configured (`mode` is `disabled` and the rest of the block is zero-valued when no proxy is configured):

| Field | Description |
|-------|-------------|
| `mode` | `shared` (registered with the shared proxy daemon), `standalone` (this process runs its own in-process proxy listener), or `disabled` |
| `daemon_reachable` | Result of a live `/health` probe against the shared daemon (500ms timeout, cached 2s); always `false` outside `shared` mode |
| `daemon_version` | The shared daemon's reported version, when reachable |
| `consecutive_failures` | The forwarder's current run of failed reconnects to the shared daemon |
| `last_connected_at` | Last time the forwarder established an SSE stream to the shared daemon |
| `dropped_events` | Request-stream events lost to a full subscriber channel |
| `backfill_failures` | Post-connect ring snapshot fetch failures |
| `heal_state` | `healthy`, `healing`, or `version_mismatch`; empty when not in shared mode |
| `capture_enabled` | Whether request/response capture is effectively on (a proxy is running for this session **and** `proxy.capture.enabled` is true). Daemons predating this field omit it entirely; an absent `capture_enabled` means "unknown", **not** `false` |

`prox status` (the CLI command) renders this block as a `Proxy:` line and, when `mode` is `shared` and `daemon_reachable` is `false`, prints `Proxy: DOWN — shared proxy daemon unreachable (proxied routes are dead). Check 'prox proxy status'.` and **exits with status 1** even though the project's own processes may be healthy. The project self-heals automatically (re-registers with a fresh or recovered daemon), worst case within ~45s — treat a brief `daemon_reachable: false` as transient rather than a hard failure. See [`prox status`](cli.md#status).

`dependencies` reports the resolution state of every configured `dependencies:` entry; the field is omitted entirely when no dependencies are configured (it is never an empty array):

| Field | Description |
|-------|-------------|
| `name` | The dependency's name |
| `state` | `pending` (no resolution started yet this generation), `checking`, `starting`, `polling`, `healthy`, `warned` (budget exhausted, `on_failure: warn`), `failed` (budget exhausted, `on_failure: fail`), or `canceled` (a resolution torn down by shutdown/reset, not a readiness failure) |
| `check` | A one-line `"<kind> <target>"` summary of the readiness probe, e.g. `tcp localhost:5432`, `url http://localhost:9070/health`, or `cmd ...` (truncated with `…` past 60 characters) |
| `last_error` | The most recent probe error as a string; omitted when there is none |
| `start_invoked` | Whether the `start:` command was launched this generation (always `false` when the initial check already passed) |

Only `failed` trips the exit-1 contract (see [`prox status`](cli.md#status) for the full precedence); `warned`/`pending`/`checking`/`polling`/`healthy`/`canceled` do not.

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
      "health": "none",
      "kind": "task"
    }
  ]
}
```

**Status values:** `running`, `stopped`, `starting`, `stopping`, `crashed`, `waiting` (a gated process/task whose `depends_on` targets are still resolving), `blocked` (a gated process/task left terminal when a required target failed), `completed` (a task that exited `0`)

**Health values:** `healthy`, `unhealthy`, `unknown`, `none`. `none` means **no `healthcheck:` is configured**, so prox never had a check to run — the `prox status` table renders it as `-`. `unknown` means a check **is** configured but has not reported a verdict yet: the process is stopped, crashed, not yet launched, or still inside its `start_period`. A task with no healthcheck reports `none` (the `api` and `migrate` entries above show the two cases side by side). Treat the list as open — a future prox may add a value, so clients should fall through to a neutral rendering rather than rejecting one they do not recognize.

| Field | Description |
|-------|-------------|
| `kind` | `"task"` for a `tasks:` entry; omitted (meaning a plain process) otherwise |
| `waiting_on` | The `depends_on` targets a `waiting` process/task is still resolving, in declaration order. Omitted (never an empty array) outside the `waiting` state |
| `blocked_on` | The `depends_on` targets that failed and left this process/task `blocked`, in declaration order. Omitted outside the `blocked` state |

`pid` is `0` for any state with no live process — `waiting`, `blocked`, `stopped`, and `completed` all report `0` here (the CLI table renders this as `-`).

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

The `healthcheck` block is present only when a `healthcheck:` **is** configured; a process reporting `"health": "none"` omits it entirely. Its `enabled` field reports whether the check loop is currently running: `false` once the process is stopped, or before it has ever been launched, in which case `health` is `unknown` because the configured check has produced no verdict.

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
snapshot of the daemon's current ring while continuing to consume the live
stream, closing gaps opened while the subscription was down (bounded by the
daemon ring and the project's registration lifetime). Snapshot replay and live
delivery are concurrent; monotonic same-ID upserts keep stale copies from
regressing a record, and reconnects re-deliver no already-seen record, so this
endpoint stays free of duplicates.

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `subdomain` | string | all | Filter by subdomain |
| `method` | string | all | Filter by HTTP method (GET, POST, etc.) |
| `min_status` | int | — | Minimum status code |
| `max_status` | int | — | Maximum status code |
| `since` | string | — | RFC3339 timestamp; only requests recorded at or after this time |
| `url_contains` | string | — | Case-insensitive substring match against the request URL (path+query only — never scheme/host) |
| `before_id` | string | — | Cursor: page strictly older than this request ID (see [Cursor pagination](#cursor-pagination) below) |
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

`hostname` is the request's Host header with the port stripped (e.g.
`api.local.dev`); it is omitted when not recorded (older records, or a
daemon-side record that predates this field).

`in_flight` is `true` on a request whose response is still streaming (the
backend has sent headers but the body hasn't finished); it is omitted
entirely once the request completes. `duration_ms` stays `0` while
`in_flight` is `true` — read it only once the field is gone. A request that
has been `in_flight` for more than 5 minutes also carries `"stale": true` —
this means the completion event may have been lost and the true outcome is
unknown, **not** that the request is broken: long-lived streams (SSE,
WebSocket-over-HTTP) and large transfers can legitimately sit at
`stale: true` for a long time while still live. `stale` is omitted once the
request completes.

#### Cursor pagination

Pass the previous page's `next_before_id` as `before_id` to fetch the next
older page. `next_before_id` is the ID of the *oldest scanned* record, not
the oldest *returned* one — a page that gets filtered down to zero results
still advances the cursor instead of stalling. It is omitted when the scan
reached the end of the ring (no more history).

Ring order is arrival order, not strict timestamp order (backfill and a
completion re-appending after its in-flight record was evicted can both put
a newer-by-timestamp record after an older one). An unknown, evicted, or
out-of-scope `before_id` — including a record from a different
`?subdomain=`/project scope — returns **`410 Gone`** with code
`CURSOR_GONE`; on that response, restart pagination without a cursor rather
than retrying the same one.

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

# Page backward through history
curl "http://localhost:5555/api/v1/proxy/requests?before_id=9f8e7d6c5b4a"
```

### GET /proxy/requests/{id}

Retrieve details for one proxied request.

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `include` | string | — | Set to `body` to include captured request and response body data |

Body data is available by default whenever the project's proxy is enabled — capture is on unless the project set `proxy.capture.enabled: false` or ran with `--no-capture` — but is not guaranteed for every record: a body may be absent when capture is unavailable in the daemon (`capture_available: false` in daemon status), after disk-budget eviction, or once the record ages out of the 1000-record captured-body window even though the ring keeps 5000 records, and the record still carries its metadata and headers. In shared mode this endpoint serves the project's own local copy of the ring, which enforces that 1000-record body bound itself (by request timestamp, since the daemon's own eviction publishes no event this endpoint could react to) rather than relying on the daemon to have already stripped the record. All of those report `unavailable_reason: "evicted"` (below). Stored `request_headers`/`response_headers` values — including `Authorization`, `Cookie`, and sensitive query params such as inside a `Location`/`Referer` header value — are captured verbatim, with no values altered or hidden. Bodies are likewise captured verbatim, up to `max_body_size`. See [Request Capture](configuration.md#request-capture) for the full cleartext posture and the disk-budget/eviction rules.

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
| `unavailable_reason` | Set (e.g. `evicted`) when `include=body` was requested but the body could no longer be loaded — its spilled file was disk-budget evicted, or the record aged past the newest-1000 captured-body window; `data` is absent, while metadata and headers remain |

**Decoded-body semantics:** captured bodies store the raw wire bytes and are decoded at serve time. Supported `content_encoding` values are `gzip`/`x-gzip`, `deflate` (zlib-wrapped per RFC 9110, with a fallback to raw deflate for servers that send it unwrapped), `zstd`, and `br` (brotli) — decoded automatically, capped at 10MB decoded size (`MaxDecodedBodySize`). When the body was not truncated and decodes cleanly, `is_binary` reflects the decoded content (readable JSON/text decodes to `is_binary: false`). Chained encodings (e.g. `gzip, br`), unrecognized tokens, truncated bodies, corrupt streams, and payloads whose decoded size would exceed the cap are served as the raw bytes, base64-encoded, with `is_binary: true` and `content_encoding` preserved. The stored (raw) binary classification and the served `is_binary` may therefore legitimately differ. The on-disk path of a spilled body is never exposed.

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

A proxied request emits **two** `data:` events sharing the same `id`: a
start event the moment the backend's response headers arrive
(`"in_flight":true`, `duration_ms` still `0`), and a completion event once
the body finishes (no `in_flight` field, final `duration_ms`). There is no
`event:` type field distinguishing them — consumers that key by `id` see a
clean upsert; naive line-oriented consumers see one extra `data:` line per
request. Early routing failures (no matching subdomain) emit only the
completion event, since nothing was proxied.

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
