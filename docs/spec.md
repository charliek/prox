# prox

A modern process manager for development with an API-first design, enabling both human interaction and LLM tooling integration.

## Overview

prox is a Procfile-compatible process manager that combines the simplicity of tools like Honcho with a built-in HTTP API and interactive TUI. It's designed to run multiple development processes, aggregate their logs, and expose control and log access via an API that LLMs (like Claude Code) can interact with programmatically.

## Goals

- **Simple by default** — Procfile-like experience with minimal configuration
- **API-first** — full process control and log access via HTTP, always available
- **LLM-friendly** — structured output, filtering, and search to support AI-assisted debugging
- **Interactive TUI** — real-time log viewing with filtering and search for human users
- **Extensible config** — YAML format supporting simple and advanced process definitions

## Non-Goals (v1)

- Daemon mode / background operation
- Log persistence to disk
- Process dependencies / startup ordering
- Process groups
- Distributed or multi-machine operation

---

## Configuration

Config file: `prox.yaml` in working directory.

### Minimal Example

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

### Full Example

```yaml
api:
  port: 5555
  host: 127.0.0.1  # default, can be 0.0.0.0 for all interfaces

env_file: .env  # loaded for all processes

processes:
  # Simple form — string command
  web: npm run dev
  worker: python worker.py

  # Expanded form — full configuration
  api:
    cmd: go run ./cmd/server
    env:
      PORT: "8080"
      DEBUG: "true"
    env_file: .env.api  # additional env file for this process
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
```

### Configuration Reference

#### Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `api.port` | int | `5555` | HTTP API port |
| `api.host` | string | `127.0.0.1` | API bind address |
| `env_file` | string | — | Global .env file path |
| `processes` | map | required | Process definitions |

#### Process fields (expanded form)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run |
| `env` | map | — | Environment variables |
| `env_file` | string | — | Process-specific .env file |
| `healthcheck.cmd` | string | — | Health check command |
| `healthcheck.interval` | duration | `10s` | Time between checks |
| `healthcheck.timeout` | duration | `5s` | Check timeout |
| `healthcheck.retries` | int | `3` | Failures before unhealthy |
| `healthcheck.start_period` | duration | `30s` | Grace period on startup |

---

## Architecture

### Design Principles

1. **Subscriber-based log output** — terminal output is a subscriber to the log buffer, not a special case. This enables TUI, API streaming, and future daemon mode without architectural changes.

2. **API always available** — even in foreground mode, the HTTP API runs and accepts connections.

3. **Filter/search in core** — filtering primitives live in the log manager, exposed to all consumers (TUI, API, CLI).

### Internal Structure

```
┌─────────────────────────────────────────────────────────┐
│                     Supervisor                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
│  │  Process 1  │  │  Process 2  │  │  Process N  │     │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘     │
│         └────────────────┼────────────────┘            │
│                          ▼                              │
│                   ┌─────────────┐                       │
│                   │ Log Manager │                       │
│                   │ (ring bufs) │                       │
│                   └──────┬──────┘                       │
│                          │                              │
│         ┌────────────────┼────────────────┐            │
│         ▼                ▼                ▼            │
│   ┌──────────┐    ┌───────────┐    ┌───────────┐      │
│   │ Terminal │    │ HTTP API  │    │    TUI    │      │
│   │Subscriber│    │  + SSE    │    │(bubbletea)│      │
│   └──────────┘    └───────────┘    └───────────┘      │
└─────────────────────────────────────────────────────────┘
```

### Log Manager

- Ring buffer per process (configurable size, default 1000 lines or 1MB)
- Each entry: `{timestamp, process, stream (stdout|stderr), line}`
- Supports multiple concurrent readers/subscribers
- Filter primitives: by process, by pattern (substring or regex)
- Subscribers receive log entries via channels

### Process Manager

- Spawns and manages child processes
- Captures stdout/stderr, routes to log manager
- Handles graceful shutdown (SIGTERM → wait → SIGKILL)
- Tracks process state, PID, uptime, restart count
- Runs healthchecks if configured

---

## CLI Interface

Single binary with subcommands.

### Commands

```
prox up [processes...]           Start processes (foreground, default output)
prox up --tui [processes...]     Start processes with TUI
prox stop                        Stop running instance (via API)
prox restart <process>           Restart a process (via API)
prox status                      Show process status
prox status --json               Show process status as JSON
prox logs [process]              Show recent logs
prox logs -f [process]           Stream logs
prox version                     Show version
prox help                        Show help
```

### Flags

| Flag | Commands | Description |
|------|----------|-------------|
| `--tui` | `up` | Enable interactive TUI mode |
| `--config, -c` | all | Config file path (default: `prox.yaml`) |
| `--port, -p` | `up` | Override API port |
| `--lines, -n` | `logs` | Number of lines (default: 100) |
| `-f, --follow` | `logs` | Stream logs continuously |
| `--json` | `status`, `logs` | JSON output |
| `--process` | `logs` | Filter by process(es) |
| `--pattern` | `logs` | Filter by pattern |
| `--regex` | `logs` | Treat pattern as regex |

### Examples

```bash
# Start all processes
prox up

# Start specific processes with TUI
prox up --tui web api

# Restart the worker process
prox restart worker

# Get last 50 lines from api, errors only
prox logs --process api --pattern ERROR --lines 50

# Stream all logs as JSON (for piping)
prox logs -f --json

# Check status
prox status
```

---

## HTTP API

Base URL: `http://{host}:{port}/api/v1`

Default: `http://127.0.0.1:5555/api/v1`

### Error Format

All errors return:

```json
{
  "error": "human readable message",
  "code": "MACHINE_READABLE_CODE"
}
```

Error codes:
- `PROCESS_NOT_FOUND`
- `PROCESS_ALREADY_RUNNING`
- `PROCESS_NOT_RUNNING`
- `INVALID_PATTERN`
- `SHUTDOWN_IN_PROGRESS`

### Endpoints

#### `GET /status`

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

#### `GET /processes`

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

Status values: `running`, `stopped`, `starting`, `stopping`, `crashed`

Health values: `healthy`, `unhealthy`, `unknown` (no healthcheck configured)

#### `GET /processes/{name}`

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

#### `POST /processes/{name}/start`

Start a stopped process.

**Response:**
```json
{
  "success": true
}
```

#### `POST /processes/{name}/stop`

Stop a running process.

**Response:**
```json
{
  "success": true
}
```

#### `POST /processes/{name}/restart`

Restart a process (stop then start).

**Response:**
```json
{
  "success": true
}
```

#### `GET /logs`

Retrieve logs from buffer.

**Query parameters:**

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

#### `GET /logs/stream`

Stream logs via Server-Sent Events (SSE).

**Query parameters:** Same as `GET /logs` (except `lines` and `bytes`)

**Response:** SSE stream

```
data: {"timestamp":"2025-01-19T10:32:01.123Z","process":"web","stream":"stdout","line":"GET /api/users 200 12ms"}

data: {"timestamp":"2025-01-19T10:32:01.456Z","process":"api","stream":"stderr","line":"WARN: connection pool low"}
```

#### `POST /shutdown`

Gracefully shut down supervisor and all processes.

**Response:**
```json
{
  "success": true
}
```

Connection closes after response as supervisor terminates.

---

## TUI

Interactive terminal UI using bubbletea.

### Layout

```
┌─ processes ──────────────────────────────────────────────┐
│ [1] ● web     running   [2] ● api    running             │
│ [3] ● worker  starting  [4] ○ cron   stopped             │
├─ logs (showing: all) ────────────────────────────────────┤
│ 10:32:01 web    │ GET /api/users 200 12ms                │
│ 10:32:01 api    │ connected to database                  │
│ 10:32:02 worker │ processing job 123                     │
│ 10:32:02 web    │ GET /api/posts 200 8ms                 │
│ 10:32:03 api    │ WARN: connection pool running low      │
│ ...                                                      │
├──────────────────────────────────────────────────────────┤
│ [f]ilter [/]search [s]tring filter [r]estart [?]help [q] │
└──────────────────────────────────────────────────────────┘
```

### Keybindings

| Key | Action |
|-----|--------|
| `↑` / `↓` / `j` / `k` | Scroll logs |
| `PgUp` / `PgDn` | Scroll page |
| `scroll wheel` | Scroll logs |
| `Home` / `End` / `g` / `G` | Jump to start/end |
| `1-9` | Solo process (press again for all) |
| `f` | Open process filter (multi-select) |
| `/` | Search (highlight matches) |
| `n` / `N` | Next/previous search match |
| `s` | String filter (hide non-matching) |
| `Esc` | Clear filter/search, exit mode |
| `r` | Restart highlighted process |
| `?` | Show help overlay |
| `q` | Quit |

### Process Filter Mode

When `f` is pressed:

```
┌─ filter processes ───────────────────┐
│ [x] web                              │
│ [x] api                              │
│ [ ] worker                           │
│ [x] cron                             │
├──────────────────────────────────────┤
│ [space] toggle  [a]ll  [n]one        │
│ [enter] apply   [esc] cancel         │
└──────────────────────────────────────┘
```

### Search Mode

When `/` is pressed:
- Input field appears at bottom
- Matches highlighted in log view
- Logs continue to flow
- `n`/`N` navigate between matches
- `Esc` or `Enter` exits search mode (highlights remain until `Esc`)

### String Filter Mode

When `s` is pressed:
- Input field appears at bottom
- Only matching lines shown
- Non-matching lines hidden
- Filter indicator shown: `logs (filter: "ERROR")`
- `Esc` clears filter

### Help Overlay

When `?` is pressed, modal overlay shows all keybindings.

---

## Process Lifecycle

### Startup

1. Parse config file
2. Load environment (global .env, per-process env_file, per-process env)
3. Start HTTP API server
4. Start each process
5. Begin health checks (if configured)
6. Attach log subscribers (terminal or TUI)

### Shutdown (Ctrl+C or API)

1. Stop accepting new API requests
2. Send SIGTERM to all child processes
3. Wait for graceful shutdown (default 10 seconds, configurable)
4. Send SIGKILL to any remaining processes
5. Exit

### Process Restart

1. Send SIGTERM to process
2. Wait for shutdown (timeout → SIGKILL)
3. Reset restart counter if appropriate
4. Start process again
5. Reset health check state

### Health Checks

- Start after `start_period` elapses
- Run at `interval`
- Mark unhealthy after `retries` consecutive failures
- Mark healthy after one success
- Health state exposed via API and TUI

---

## Technologies

### Required

| Component | Technology | Notes |
|-----------|------------|-------|
| Language | Go 1.25+ | Concurrency, single binary |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) | Elm-architecture TUI framework |
| TUI styling | [lipgloss](https://github.com/charmbracelet/lipgloss) | Styling for bubbletea |
| HTTP router | [chi](https://github.com/go-chi/chi) or stdlib | Lightweight, idiomatic |
| YAML parsing | [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) | Standard YAML library |
| Env file | [godotenv](https://github.com/joho/godotenv) | .env file loading |

### Project Structure

Module path: `github.com/charliek/prox`

```
prox/
├── cmd/
│   └── prox/
│       └── main.go           # CLI entrypoint
├── internal/
│   ├── config/
│   │   └── config.go         # YAML parsing, validation
│   ├── supervisor/
│   │   ├── supervisor.go     # Main orchestrator
│   │   └── process.go        # Single process management
│   ├── logs/
│   │   ├── manager.go        # Ring buffer, subscriptions
│   │   ├── entry.go          # Log entry type
│   │   └── filter.go         # Filter/search logic
│   ├── api/
│   │   ├── server.go         # HTTP server setup
│   │   ├── handlers.go       # Request handlers
│   │   └── sse.go            # SSE streaming
│   ├── tui/
│   │   ├── app.go            # Bubbletea app
│   │   ├── model.go          # TUI state
│   │   ├── views.go          # Rendering
│   │   └── keys.go           # Keybindings
│   └── cli/
│       └── commands.go       # CLI command definitions
├── prox.yaml                  # Example config
├── go.mod
├── go.sum
└── README.md
```

### Development Setup

**.mise.toml:**

```toml
[tools]
go = "1.25"
```

**Initialize project:**

```bash
mkdir prox && cd prox
go mod init github.com/charliek/prox
mise install  # if using mise
```

---

## v1.5 / Future Enhancements

Features deferred from v1:

### Log Persistence

- Write logs to `~/.prox/logs/{project-hash}/`
- Configurable rotation (size, count)
- `prox logs` works after supervisor exits
- Auto-cleanup of old logs

### Global Config

- `~/.prox/config.yaml` for defaults
- Default API port, log retention, etc.

### Daemon Mode

- `prox up -d` starts detached
- `prox attach` connects to running instance
- `prox attach --tui` attaches with TUI
- PID file management

### Process Groups

```yaml
groups:
  backend:
    - api
    - worker
  frontend:
    - web
```

- `prox restart backend` restarts group
- TUI shows groups

### Dependencies

```yaml
processes:
  api:
    cmd: go run ./cmd/server
    depends_on:
      - db
  db:
    cmd: docker compose up postgres
```

- Startup order based on dependencies
- Wait for health before starting dependents

---

## Open Questions

Items to revisit during implementation:

1. **Config validation** — how strict? Warn on unknown fields or error?
2. **Color scheme** — configurable? Match terminal theme?
3. **Log buffer sizing** — lines vs bytes vs both? Per-process or global?
4. **Healthcheck output capture** — how much to retain?
5. **Signal handling** — expose choice of SIGTERM vs SIGINT vs SIGQUIT?

---

## References

- [Honcho](https://github.com/nickstenning/honcho) — Python Procfile runner
- [Foreman](https://github.com/ddollar/foreman) — Original Procfile runner
- [Overmind](https://github.com/DarthSim/overmind) — Go, tmux-based
- [PM2](https://pm2.keymetrics.io/) — Node.js process manager
- [Bubbletea](https://github.com/charmbracelet/bubbletea) — Go TUI framework
