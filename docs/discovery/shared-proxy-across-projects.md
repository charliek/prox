# Shared Proxy Across Projects

## Status

Implemented in #12 on 2026-04-06 as an auto-started shared proxy daemon. The user-facing guide is [Shared Proxy Across Projects](../guides/shared-proxy.md).

## Problem Statement

A common usage pattern for prox is running the proxy on well-known ports (443 for HTTPS, 80 for HTTP). Users often have multiple projects on the same machine that each define their own `prox.yaml`, and ideally each project's domains would be served through the same proxy port.

For example:
- **Project A** (`~/projects/auth-service/prox.yaml`): `auth.local.stridelabs.ai` on port 443
- **Project B** (`~/projects/audit-service/prox.yaml`): `audit.local.stridelabs.ai` on port 443

Before the shared proxy daemon, each `prox up` invocation started its own independent proxy listener. If Project A already owned port 443, Project B could not bind the same port.

## Decision

prox uses Option C: an auto-started daemon on demand.

- `prox up` starts the shared daemon when a proxy is configured and no daemon is running.
- Each project registers its own service hostnames with the daemon.
- HTTP routes by Host header. HTTPS routes by SNI.
- `prox down` deregisters only the current project.
- The daemon stops automatically when the last project deregisters.
- `prox proxy status`, `prox proxy routes`, and `prox proxy stop` provide explicit inspection and control.

The daemon is per-user and per-machine. It stores state in `~/.prox/`, while each project's supervisor state remains in that project's `.prox/` directory.

## Previous Architecture

- All state is **per-project scoped**: `.prox/prox.state`, `.prox/prox.pid`, and `.prox/prox.log` live in each project's working directory.
- Each `prox up` creates its own `proxy.Service` that binds its own HTTP/HTTPS ports.
- There is no awareness of other prox instances running on the machine.
- The proxy routes requests by extracting the subdomain from the Host header and looking it up in a static `services` map built from the project's config at startup.

## Use Cases

1. **Multiple microservices in development**: Different repos/projects each define a service that should be reachable via its own subdomain, all on the same port.
2. **Shared base domain**: All services share a base domain (e.g., `*.local.stridelabs.ai`) but are developed in separate project directories.
3. **Independent lifecycle**: Each project should be able to `prox up` and `prox down` independently without affecting the other projects' routes.

## Options Explored

### Option A: Central Proxy Daemon at `~/.prox/`

A standalone, long-lived proxy process that multiple projects register with.

- An explicit command such as `prox proxy start` could start a background proxy daemon that owns port 443/80 and stores state in `~/.prox/`.
- `prox up` in a project detects the running proxy daemon, registers its domains/services via API, deregisters on shutdown.
- The proxy daemon has its own route table that projects dynamically add to and remove from.

**Pros:**
- Clean ownership model -- the proxy lifecycle is independent of any single project.
- Multiple projects can come and go freely.
- No "first project owns the port" ambiguity.

**Cons:**
- Extra thing to manage if startup is explicit.
- Requires a registration API/protocol between project instances and the daemon.
- Daemon needs its own logging, status, and management commands.

### Option B: First-Come-First-Served with Registration

The first `prox up` that wants a given port starts the proxy. Subsequent projects detect the existing proxy and register their routes with it via the existing API.

- No separate daemon -- the proxy lives inside whichever `prox up` process started first.
- Other projects register routes via the first instance's API.

**Pros:**
- Simpler, no extra command or process.
- Feels natural for the single-project case.

**Cons:**
- Fragile -- when the "owner" project does `prox down`, all other projects' routes die.
- Transferring proxy ownership between processes is complex and error-prone.
- The owner project's process manager and the shared proxy are coupled.

### Option C: Hybrid -- Auto-Start Daemon on Demand

Like Option A, but the daemon auto-starts when the first `prox up` needs a proxy port and no daemon is running. Subsequent `prox up` calls detect it and register.

- `prox up` checks `~/.prox/proxy.state` -- if no proxy daemon is running, it forks one off as a separate background process.
- The proxy daemon has its own PID, independent of any project.
- `prox proxy stop` (or stopping all registered projects) brings it down.
- An explicit start command was considered for manual control, but the shipped CLI relies on automatic startup.

**Pros:**
- No extra manual step for users -- seamless experience.
- Proxy daemon is independent of any project's lifecycle.
- Supports both implicit and explicit management.

**Cons:**
- "Magic" background process that users might not realize is running.
- Need clear status/stop commands (`prox proxy status`, `prox proxy stop`).
- Auto-start logic adds complexity.

## Resolved Design Questions

| Question | Resolution |
|----------|------------|
| Opt-in vs default | Shared proxy behavior is automatic for any project with a configured proxy. There is no `shared` config field. |
| Domain conflicts | Duplicate `hostname:port` registrations fail. |
| Certificate management | HTTPS certificates are managed by the shared daemon as routes are registered. |
| Stale route cleanup | The daemon tracks project PIDs and removes stale registrations for dead processes. |
| Visibility and debugging | `prox proxy status` and `prox proxy routes` expose daemon state. |
| Registration protocol | Projects communicate with the daemon over the Unix socket at `~/.prox/proxy.sock`. |

## Caveats

| Caveat | Behavior |
|--------|----------|
| Same hostname | A second project cannot take over an existing `hostname:port`; registration fails. |
| Mixed protocol | A port can be HTTP or HTTPS, not both. |
| Version mismatch | A project with a different `prox` version cannot join the running daemon. Reset with `prox proxy stop --force`. |
| Sandboxed state | If `~/.prox/` is unavailable, prox falls back to a standalone per-project proxy without port sharing. |
