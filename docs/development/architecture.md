# Architecture

This document describes the internal design of prox for contributors.

## Design Principles

1. **Subscriber-based log output** — Terminal output is a subscriber to the log buffer, not a special case. This enables TUI, API streaming, and daemon mode without architectural changes.

2. **API always available** — Even in foreground mode, the HTTP API runs and accepts connections.

3. **Filter/search in core** — Filtering primitives live in the log manager and are exposed to all consumers (TUI, API, CLI).

## Internal Structure

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

## Log Manager

- Ring buffer per process (configurable size, default 1000 lines)
- Each entry: `{timestamp, process, stream (stdout|stderr), line}`
- Supports multiple concurrent readers/subscribers
- Filter primitives: by process, by pattern (substring or regex)
- Subscribers receive log entries via channels

## Process Manager

- Spawns and manages child processes
- Captures stdout/stderr, routes to log manager
- Handles graceful shutdown (SIGTERM → wait → SIGKILL)
- Tracks process state, PID, uptime, restart count
- Runs health checks if configured

## Process Lifecycle

### Startup

1. Parse config file
2. Load environment (global .env, per-process env_file, per-process env)
3. Start each process (and begin its health checks, if configured)
4. Start HTTP API server
5. Attach log subscribers (terminal or TUI)

### Shutdown (Ctrl+C or API)

1. Stop accepting new API requests
2. For each process, send SIGTERM to its entire process group (not just the leader), so descendants spawned by the command are included
3. Poll the group's liveness until it exits or the graceful deadline passes (by default ~8 seconds: a 10-second total shutdown budget minus a 2-second reserve for the SIGKILL/verify phase)
4. If the group is still alive at the graceful deadline, send SIGKILL to the group and poll again until the kill deadline passes
5. Verify the group is actually gone; if it survived SIGKILL, the stop reports `PROCESS_GROUP_NOT_REAPED` instead of succeeding
6. Exit

### Process Restart

1. Stop the process: SIGTERM to its process group, graceful liveness polling, SIGKILL escalation to the group at the graceful deadline, then a post-kill liveness check. If the group could not be reaped, restart aborts before starting a replacement and reports `PROCESS_GROUP_NOT_REAPED` (a surviving group is never shadowed by a new one)
2. Increment the restart counter
3. Start the process again — `env_file` files are re-read from disk on every start (including this restart's start half) and merged with the inline `env` values captured at `up` time
4. Reset health check state

### Health Checks

- Start after `start_period` elapses
- Run at `interval`
- Mark unhealthy after `retries` consecutive failures
- Mark healthy after one success
- Health state exposed via API and TUI

## HTTP/HTTPS Reverse Proxy

The optional reverse proxy provides hostname-based routing to local services over HTTP and/or HTTPS. In normal operation, projects register their routes with a per-user shared proxy daemon so several projects can use the same port.

### Proxy Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                   Shared HTTP/HTTPS Proxy                         │
│                                                                    │
│  Browser Request                                                   │
│  http(s)://app.local.dev:port/api/users                           │
│         │                                                          │
│         ▼                                                          │
│  ┌─────────────────────┐                                          │
│  │  Hostname Router    │  Match full hostname                     │
│  │  (extract + lookup) │                                          │
│  └──────────┬──────────┘                                          │
│             │                                                      │
│             ▼                                                      │
│  ┌─────────────────────┐     ┌─────────────────────┐              │
│  │   Route Table       │────▶│   Request Manager   │              │
│  │ app.local → :3000   │     │   (ring buffer)     │              │
│  │ api.local → :8000   │     └─────────────────────┘              │
│  └──────────┬──────────┘                                          │
│             │                                                      │
│             ▼                                                      │
│  ┌─────────────────────┐                                          │
│  │ httputil.ReverseProxy│                                         │
│  │ → localhost:3000    │                                          │
│  └─────────────────────┘                                          │
└──────────────────────────────────────────────────────────────────┘
```

### Package Structure

```
internal/proxy/
├── proxy.go          # Standalone proxy service and request handling
├── requests.go       # Request manager (ring buffer, subscriptions)
├── capture.go        # Optional request/response body capture
├── certs/
│   └── certs.go      # mkcert integration for certificate management

internal/proxyd/
├── daemon.go         # Shared daemon lifecycle and auto-start
├── dynamic_proxy.go  # Runtime listener and route management
├── registry.go       # Project route registry and conflict checks
├── server.go         # Unix-socket HTTP API for registration/control
├── client.go         # Client used by project supervisors
└── certs.go          # Multi-domain certificate management
```

### Request Flow

1. Incoming HTTP or HTTPS request to `hostname:port`
2. Use the Host header for HTTP or SNI for HTTPS routing
3. Look up the full hostname in the route table
4. Forward request via `httputil.ReverseProxy`
5. Set `X-Forwarded-Proto` based on connection type (HTTP or HTTPS)
6. Record request in RequestManager
7. Return response to client

## Technologies

| Component | Technology | Notes |
|-----------|------------|-------|
| Language | Go 1.24+ | Concurrency, single binary |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) | Elm-architecture TUI framework |
| TUI styling | [lipgloss](https://github.com/charmbracelet/lipgloss) | Styling for bubbletea |
| HTTP router | [chi](https://github.com/go-chi/chi) or stdlib | Lightweight, idiomatic |
| Reverse Proxy | [net/http/httputil](https://pkg.go.dev/net/http/httputil) | Standard library reverse proxy |
| YAML parsing | [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) | Standard YAML library |
| Env file | [godotenv](https://github.com/joho/godotenv) | .env file loading |
| Certificates | [mkcert](https://github.com/FiloSottile/mkcert) | Local CA for development certs |
