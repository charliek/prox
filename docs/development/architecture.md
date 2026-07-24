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

The daemon tears down in stages, each with its own deadline computed at shutdown
time rather than one shared budget, so proxy/API teardown time can never eat
into the supervisor's process-stop budget. The API server is stopped **last** so
it outlives the supervisor stage and can still deliver the outcome to a
`wait=true` caller:

1. Close the launch gate — the supervisor refuses any new process launch for the
   rest of shutdown, so a lifecycle request arriving during the drain cannot
   start a process that teardown is about to orphan
2. Deregister from (or stop) the proxy — a short fixed stage deadline
3. Stop all processes (the supervisor stage), bounded by the maximum per-process
   stop budget currently in force plus a small margin. Each process is stopped
   concurrently on its **own** effective stop budget:
   1. Send SIGTERM to its entire process group (not just the leader), so
      descendants spawned by the command are included
   2. Poll the group's liveness until it exits or the graceful deadline passes.
      The graceful window is the process's configured stop budget minus a 2-second
      reserve for the SIGKILL/verify phase — by default ~8 seconds (a 10-second
      budget minus the 2-second reserve), but tunable via `shutdown_timeout` /
      `stop_timeout`
   3. If the group is still alive at the graceful deadline, send SIGKILL to the
      group and poll again until the kill deadline passes
   4. Verify the group is actually gone; if it survived SIGKILL, the stop reports
      `PROCESS_GROUP_NOT_REAPED` instead of succeeding
4. Publish the aggregate process-stop verdict to any `wait=true` shutdown caller
   (a latched broadcast: every waiter reads the same stored outcome)
5. Flush and close the log manager — this releases SSE log subscribers so their
   handlers return instead of pinning the API server open through the next stage
6. Stop the API server — a short fixed stage deadline. It stops accepting new
   connections and waits up to the deadline for in-flight requests (including the
   `wait=true` response's small JSON write); a still-running request is not
   force-closed
7. Cleanup and exit. In the foreground, a surviving process group makes `prox up`
   exit non-zero with a one-line `shutdown incomplete: …` summary (per-survivor
   detail is in the log stream)

**Waited shutdown flow.** `POST /api/v1/shutdown?wait=true` (used by `prox stop`
and `prox down`) triggers the sequence above, then blocks until stage 4 latches
the verdict. The handler returns HTTP 200 with the verdict — `success: true` on a
clean stop, or `success: false` plus a `failures` list naming each survivor. The
CLI then waits briefly for the daemon's state/PID files to disappear (confirming
the daemon process itself exited) before printing its summary. See
[POST /shutdown](../reference/api.md#post-shutdown) and
[`prox stop`](../reference/cli.md#stop).

### Process Restart

Every API-driven (re)start (`restart`, and `start` after a `stop`) re-reads `prox.yaml` and applies the target process's current config, so an edit → restart loop works without a full `prox stop` + `prox up`.

1. **Reload + validate + preflight (before any stop):** re-read the whole config file from its absolute path and validate it, look up the target process in the fresh config, build its new runtime (`cmd`, `healthcheck`, `stop_timeout`, and a rebuilt env-loader closure over the fresh global/per-process `env_file` and inline `env`), and preflight that env load. Any failure here leaves the running process **completely untouched** and returns a typed error: `CONFIG_RELOAD_FAILED` (invalid/unreadable file, or a missing referenced env file — whole-file validation means an invalid *unrelated* section also blocks the restart) or `PROCESS_NOT_IN_CONFIG` (target removed from the file). Renames, added/removed processes, and `services`/`proxy`/port changes are out of scope and require `prox up`
2. Stop the process: SIGTERM to its process group, graceful liveness polling, SIGKILL escalation to the group at the graceful deadline, then a post-kill liveness check. This stop half uses the process's **pre-edit** stop budget; a raised `stop_timeout` governs the next stop. If the group could not be reaped, restart aborts before starting a replacement and reports `PROCESS_GROUP_NOT_REAPED` (a surviving group is never shadowed by a new one)
3. Increment the restart counter
4. Start the process again, **swapping the reloaded config in atomically**: the swap happens inside the start's locked critical section, after the already-running / surviving-group guards pass and before the process launches, so a concurrent start that wins the race launches the old config and the loser is refused (`PROCESS_ALREADY_RUNNING`) without applying its swap — the running process and the stored config never mismatch. `env_file` files are re-read from disk on this start half. If the launch fails after the swap, the new config stays active for the next start ("the file is the truth")
5. Reset health check state

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
┌───────────────────────┐                          ┌──────────────────────────────────────────┐
│  Project A (prox up)  │                          │       Shared Proxy Daemon (proxyd)        │
│  ┌─────────┐ ┌──────┐ │  register / heartbeat    │  singleton (flock'd ~/.prox/proxy.pid)    │
│  │Forwarder│▶│Local │ │─────────────────────────▶│                                            │
│  │ bridge  │ │  RM  │ │◀─────────────────────────│  ┌──────────┐    ┌──────────────────────┐ │
│  └─────────┘ └──────┘ │  per-project SSE stream  │  │ Registry │    │  Hostname Router      │ │
└───────────────────────┘  + snapshot backfill     │  │ refcounted│──▶│  (Host header / SNI)  │ │
┌───────────────────────┐  over ~/.prox/proxy.sock │  │ listeners │   └──────────┬────────────┘ │
│  Project B (prox up)  │◀─────────────────────────│  └────┬─────┘              │              │
│    (forwarder + RM)   │─────────────────────────▶│       │        ┌───────────▼────────────┐ │
└───────────────────────┘                          │       │        │ httputil.ReverseProxy  │ │
                                                     │       ▼        │  → localhost:<port>   │ │
                                                     │  ┌───────────────────────────────────┐ │ │
                                                     │  │ map[projectDir]*RequestManager     │ │ │
                                                     │  │ (one ring per registered project)  │ │ │
                                                     │  └───────────────────────────────────┘ │ │
                                                     │  stale-PID sweep (30s) reaps dead regs  │ │
                                                     └──────────────────────────────────────────┘
```

Each project keeps its own local `RequestManager` and API surface — `prox requests`, the HTTP API, and the TUI always read from the project's own process, never from the daemon directly. The forwarder bridge is what keeps a project's local copy in sync with the daemon when routing is shared.

### Package Structure

```
internal/proxy/
├── proxy.go          # Standalone proxy service and request handling
├── requests.go       # Request manager (ring buffer, subscriptions, cursor pagination)
├── capture.go        # Request/response body capture (inline vs spilled, per-call caps)
├── body.go           # Captured-body content decoders (gzip, deflate, zstd, brotli)
├── certs/
│   └── certs.go      # mkcert integration for certificate management

internal/proxyd/
├── daemon.go         # Shared daemon lifecycle: singleton start, version check, stale-PID sweep
├── dynamic_proxy.go  # Runtime listener and route management, per-project capture caps
├── registry.go       # Project route registry, refcounted listeners, conflict/idempotent-reregister checks
├── managers.go       # map[projectDir]*proxy.RequestManager — one ring per registered project
├── server.go         # Unix-socket HTTP API for registration/control/request streaming
├── forwarder.go      # Project-side bridge: SSE consumption, snapshot backfill, health sink, self-heal
├── client.go         # Client used by project supervisors to talk to the daemon
└── certs.go          # Multi-domain certificate management
```

### Request Flow

1. Incoming HTTP or HTTPS request to `hostname:port`
2. Use the Host header for HTTP or SNI for HTTPS routing
3. Look up the full hostname in the route table
4. Forward request via `httputil.ReverseProxy`
5. Set `X-Forwarded-Proto` based on connection type (HTTP or HTTPS)
6. Record request (and, if capture is enabled for the owning project, its body) in that project's `RequestManager`
7. Return response to client

### Shared Proxy Daemon (proxyd)

When more than one project needs the same proxy port, prox routes through a per-user shared daemon (`internal/proxyd`) instead of each project binding its own listener.

- **Singleton.** The daemon locks `~/.prox/proxy.pid` via `flock`; only one instance can hold the lock at a time. `EnsureRunning` connects to the existing daemon's Unix socket (`~/.prox/proxy.sock`), or forks a new detached daemon process (`Setsid`) and polls until it answers `/health`. A version mismatch between the connecting client and a running daemon is a typed error (`VersionMismatchError`) — the client hard-fails when the daemon has registered projects, or auto-replaces an idle daemon.
- **Unix-socket API.** All daemon communication — registration, deregistration, status, route listing, shutdown, and the per-project request stream — goes over the socket via a small HTTP API (`server.go`), never a network port.
- **Registration and refcounted listeners.** Each project registers its hostnames and target ports (`registry.go`). Multiple projects can share one physical listener (e.g. `:443`) as long as their hostnames differ; the registry tracks how many projects reference a listener and only closes it when the last one deregisters. A same-PID re-register (a project reconnecting after a broken stream) is idempotent rather than a 409 conflict; a genuinely different live holder still is.
- **Per-project request rings.** The daemon keeps one full-capacity `RequestManager` ring per registered project (`managers.go`), guarded independently of the daemon's lifecycle lock so the hot request path never contends with registration/deregistration. This bounds the blast radius of a chatty project: it can fill its own ring but cannot evict another project's history. A manager is closed (releasing any blocked SSE subscribers) and removed when its project deregisters or is swept as stale.
- **Stale-PID sweep.** A periodic sweep (every 30s) checks each registered project's owning PID and start-token for liveness and reaps registrations left behind by a crashed or `kill -9`'d project, so a restart doesn't collide with a dead entry.
- **Self-heal re-registration.** If a project's connection to the daemon breaks (daemon restart, transient crash), its forwarder re-establishes the daemon connection and re-registers automatically once the failure persists past a threshold — see the Forwarder Bridge below.

### Capture Pipeline

Request/response body capture (`internal/proxy/capture.go`, `body.go`) is optional per project (`proxy.capture.enabled` or `prox up --capture`) and works identically whether the proxy is standalone or routed through the shared daemon.

- **Inline vs. spilled bodies.** Small captured bodies are kept inline with the request record; bodies larger than `DefaultCaptureInlineThreshold` (64KB) are spilled to a file under the capture directory and loaded lazily when `prox requests <id> --body` / `include=body` is requested.
- **Per-project caps, one capture path.** The shared daemon has a single `CaptureManager` and capture directory, but each project's `max_body_size` (sent at registration) is enforced per call via limit-aware capture (`CaptureRequestWithLimit`/`WrapResponseWriterWithLimit`) rather than one `CaptureManager` per project — one shared cleanup path, per-project ceilings.
- **Decoders.** Captured bodies store the raw wire bytes; `DecodeCapturedBody` decodes at serve time for `gzip`/`x-gzip`, `deflate` (zlib-wrapped, with a raw-deflate fallback), `zstd`, and `br` (brotli), bounded by `MaxDecodedBodySize` (10MB). Chained or unrecognized encodings, truncated captures, and corrupt streams fall back to serving the raw bytes as binary.

### Forwarder Bridge

Every project registered with the shared daemon runs a forwarder (`internal/proxyd/forwarder.go`) that bridges the daemon's per-project request stream into that project's own local `RequestManager`, so `prox requests`, the HTTP API, and the TUI behave identically whether the proxy is standalone or shared.

- **SSE + snapshot backfill.** On every (re)connect, the forwarder concurrently fetches a snapshot of the daemon's current ring for this project and subscribes to its live SSE stream, so a reconnect after a gap backfills history instead of losing it. Monotonic same-ID upserts keep the two feeds from regressing a record.
- **Health sink.** The forwarder tracks consecutive reconnect failures, last-successful-connect time, and dropped-event/backfill-failure counters (atomics), which feed directly into the `proxy` block of `GET /status` and the CLI's `Proxy:` line.
- **Self-heal.** After the connection has been down continuously for about 15s, the forwarder invokes a heal callback — re-`EnsureRunning` plus re-`Register` — at most once every ~30s (damping against a flapping daemon). Healing is suppressed while the project itself is shutting down, and a version-mismatch failure is surfaced as a status state rather than forcing a restart of a daemon that might still be in use.

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
