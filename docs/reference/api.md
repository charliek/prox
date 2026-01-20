# HTTP API Reference

## Base URL

```
http://{host}:{port}/api/v1
```

Default: `http://127.0.0.1:5555/api/v1`

## Authentication

When prox binds to a non-localhost interface, authentication is required. A bearer token is generated and stored in `~/.config/prox/token`.

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

## Endpoints

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
  }
}
```

### POST /processes/{name}/start

Start a stopped process.

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
| `bytes` | int | — | Max bytes to return |
| `pattern` | string | — | Filter pattern |
| `regex` | bool | false | Treat pattern as regex |

If both `lines` and `bytes` are specified, whichever limit hits first applies.

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

**Query Parameters:** Same as `GET /logs` (except `lines` and `bytes`)

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

### POST /shutdown

Gracefully shut down supervisor and all processes.

**Response:**

```json
{
  "success": true
}
```

Connection closes after response as supervisor terminates.
