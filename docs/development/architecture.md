# Architecture

This document describes the internal design of prox for contributors.

## Design Principles

1. **Subscriber-based log output** — Terminal output is a subscriber to the log buffer, not a special case. This enables TUI, API streaming, and future daemon mode without architectural changes.

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

- Ring buffer per process (configurable size, default 1000 lines or 1MB)
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
3. Start HTTP API server
4. Start each process
5. Begin health checks (if configured)
6. Attach log subscribers (terminal or TUI)

### Shutdown (Ctrl+C or API)

1. Stop accepting new API requests
2. Send SIGTERM to all child processes
3. Wait for graceful shutdown (default 10 seconds)
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

## HTTPS Reverse Proxy

The optional HTTPS reverse proxy provides subdomain-based routing to local services.

### Proxy Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                    HTTPS Reverse Proxy                            │
│                                                                    │
│  Browser Request                                                   │
│  https://app.local.dev:6789/api/users                             │
│         │                                                          │
│         ▼                                                          │
│  ┌─────────────────────┐                                          │
│  │  Subdomain Router   │  Extract "app" from host                 │
│  │  (extract + lookup) │                                          │
│  └──────────┬──────────┘                                          │
│             │                                                      │
│             ▼                                                      │
│  ┌─────────────────────┐     ┌─────────────────────┐              │
│  │   Route Table       │────▶│   Request Manager   │              │
│  │   app → :3000       │     │   (ring buffer)     │              │
│  │   api → :8000       │     └─────────────────────┘              │
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
├── proxy.go          # Main proxy service, router, request handling
├── requests.go       # Request manager (ring buffer, subscriptions)
├── certs/
│   └── certs.go      # mkcert integration for certificate management
└── hosts/
    └── hosts.go      # /etc/hosts management
```

### Request Flow

1. Incoming HTTPS request to `*.domain:port`
2. Extract subdomain from Host header
3. Look up service in route table
4. Forward request via `httputil.ReverseProxy`
5. Record request in RequestManager
6. Return response to client

## Technologies

| Component | Technology | Notes |
|-----------|------------|-------|
| Language | Go 1.23+ | Concurrency, single binary |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) | Elm-architecture TUI framework |
| TUI styling | [lipgloss](https://github.com/charmbracelet/lipgloss) | Styling for bubbletea |
| HTTP router | [chi](https://github.com/go-chi/chi) or stdlib | Lightweight, idiomatic |
| Reverse Proxy | [net/http/httputil](https://pkg.go.dev/net/http/httputil) | Standard library reverse proxy |
| YAML parsing | [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) | Standard YAML library |
| Env file | [godotenv](https://github.com/joho/godotenv) | .env file loading |
| Certificates | [mkcert](https://github.com/FiloSottile/mkcert) | Local CA for development certs |
