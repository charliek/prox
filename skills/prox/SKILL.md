---
name: prox
description: "Use when a prox.yaml file exists in the project root, or the user mentions prox, or asks to start, stop, restart, or check development processes that prox manages, view their logs, reach a local service through the prox reverse proxy, inspect proxied requests, or work with the shared prox proxy daemon that lets several projects share a port. Also use when the user says a program is 'running via prox' or asks you to 'start it with prox'. This is specific to the prox CLI and prox.yaml; do not use it for lookalikes such as Proxmox, a generic nginx or Caddy reverse proxy, a corporate proxy.yaml, or foreman, pm2, or docker compose."
---

# Prox Process Manager

prox is a process manager for local development written in Go. It provides process supervision with automatic restarts, real-time log aggregation, an HTTP/HTTPS reverse proxy with subdomain routing, an interactive TUI, and a background daemon mode with a REST API. A shared proxy daemon lets multiple projects serve their domains through the same proxy port.

## First Step: Read prox.yaml

**Always read the project's `prox.yaml` before taking any action.** This file defines the project's local development topology — what processes exist, what commands they run, what ports they use, and whether proxy routing is enabled. Without reading this file you cannot help effectively.

If the config file is at a non-default path, the user or CLAUDE.md will specify it (the `--config` / `-c` flag overrides the default).

## Process Management

When a project has a `prox.yaml`, **use prox to manage processes** — do not run process commands directly.

| Task | Command |
|------|---------|
| Start all processes (daemon) | `prox up -d` |
| Start specific processes (daemon) | `prox up -d <name> [name...]` |
| Start a single stopped process | `prox start <name>` |
| Stop one process | `prox stop <name>` |
| Restart one process | `prox restart <name>` |
| Stop everything | `prox down` |
| Check what's running | `prox status` |
| Attach interactive TUI | `prox attach` |

**Always use `-d` (daemon mode)** when starting prox so the CLI returns control immediately. Do not start prox in the foreground — it will block.

**Never kill processes directly** (e.g., `kill <pid>`). Use prox commands so it can track state and handle restarts correctly.

## Shared Proxy and Multiple Projects

When a project enables a proxy (see "Making HTTP Requests" below), prox serves its services through a single background **proxy daemon** shared across all projects on the machine. This is what lets several projects expose their domains on the same port (e.g. 443) without conflicts. Routing is by full hostname, so two projects can share one port as long as their hostnames differ.

You normally never manage the daemon directly: `prox up` starts it (or registers with the already-running one) and `prox down` deregisters the project, stopping the daemon when the last project leaves.

These commands are for debugging "my service is unreachable" or "which project owns this domain" questions:

```bash
prox proxy status            # Is the shared daemon running?
prox proxy routes            # Every registered hostname and the project/port it maps to
prox proxy stop              # Stop the daemon (fails if projects still have routes)
prox proxy stop --force      # Stop anyway, disconnecting all projects
```

If a proxied request returns a connection error or an unexpected 404, check `prox proxy routes` first to confirm the target hostname is actually registered.

## Viewing Logs

prox aggregates output from all processes. When debugging, check logs first.

```bash
prox logs --lines 50                          # Recent 50 lines, all processes
prox logs --lines 50 --process api            # Recent 50 lines from "api"
prox logs -f --process api                    # Stream logs from "api"
prox logs --lines 100 --pattern ERROR         # Filter for "ERROR"
prox logs --lines 100 --pattern "err.*" --regex  # Regex filter
```

**Always use `--lines N`** to limit output. Without it, prox may return hundreds of lines that flood context.

**Pipe through bash tools when needed** — prox's built-in `--pattern` handles most filtering, but for counting (`| grep -c ERROR`), multi-pattern matching (`| grep -E "ERROR|WARN"`), or extracting specific fields, pipe through standard unix tools.

For daemon startup issues, check the daemon log directly: `cat .prox/prox.log`

## Making HTTP Requests

How to reach services depends on whether the proxy is configured in prox.yaml.

### With proxy enabled

Read the `proxy` and `services` sections of prox.yaml. Services are accessible via subdomain routing:

```
http://<service>.<domain>:<http_port>/path
```

For example, if prox.yaml contains:
```yaml
proxy:
  http_port: 6788
  domain: lvh.me

services:
  api: 8000
  app: 3000
```

Then:
- `curl http://api.lvh.me:6788/endpoint` → reaches the api service on port 8000
- `curl http://app.lvh.me:6788/` → reaches the app service on port 3000

For HTTPS, use `https://<service>.<domain>:<https_port>/path` (requires mkcert setup).

### Without proxy

Use direct ports from the process commands in prox.yaml. For example, if a process runs `uvicorn ... --port 8000`, reach it at `http://localhost:8000/endpoint`.

### Inspecting proxy traffic

```bash
prox requests                                  # Recent requests
prox requests -f                               # Stream in real-time
prox requests --subdomain api --min-status 400 # Filter for errors on api
prox requests <id>                             # Details for specific request
prox requests <id> --body                      # Include captured bodies
```

`prox requests` shows traffic that reached the proxy; if a request never arrives, use `prox proxy routes` (see "Shared Proxy and Multiple Projects") to check whether the service's hostname is registered at all.

## Configuration (prox.yaml)

Processes can be defined in simple form (string) or expanded form (object):

```yaml
processes:
  # Simple: just a command
  web: npm run dev

  # Expanded: command with environment and health check
  api:
    cmd: go run ./cmd/server
    env:
      PORT: "8080"
    env_file: .env.api
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
```

Environment variable precedence (later overrides earlier):
1. System environment
2. Global `env_file`
3. Process-specific `env_file`
4. Process-specific `env` map

For the full configuration reference including all proxy, service, and certificate fields, read `references/configuration.md`.

## CLI Help

For detailed flags and options on any command, run:
```bash
prox <command> --help
```

The CLI help is comprehensive and always up to date. Use it as the authoritative reference for command syntax.

## References

For detailed documentation beyond what's covered here:
- `references/configuration.md` — all prox.yaml fields (proxy, services, certs, health checks)
- `references/api.md` — HTTP API endpoints for scripting and automation

## Runtime State

prox stores state in `.prox/` within the project directory:
- `.prox/prox.state` — API port, PID, host (used for auto-discovery by CLI commands)
- `.prox/prox.pid` — process ID with file locking
- `.prox/prox.log` — daemon logs (stdout/stderr in background mode)

The `.prox/` directory should be in `.gitignore`.
