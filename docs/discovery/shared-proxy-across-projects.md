# Shared Proxy Across Projects

## Problem Statement

A common usage pattern for prox is running the proxy on well-known ports (443 for HTTPS, 80 for HTTP). Users often have multiple projects on the same machine that each define their own `prox.yaml`, and ideally each project's domains would be served through the same proxy port.

For example:
- **Project A** (`~/projects/auth-service/prox.yaml`): `auth.local.stridelabs.ai` on port 443
- **Project B** (`~/projects/audit-service/prox.yaml`): `audit.local.stridelabs.ai` on port 443

Today, each `prox up` invocation starts its own independent proxy listener. If Project A already owns port 443, Project B's proxy silently fails to bind (see Issue 1: port conflict detection), and its domains are unreachable.

## Current Architecture

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

- `prox proxy start` starts a background proxy daemon that owns port 443/80, stores state in `~/.prox/`.
- `prox up` in a project detects the running proxy daemon, registers its domains/services via API, deregisters on shutdown.
- The proxy daemon has its own route table that projects dynamically add to and remove from.

**Pros:**
- Clean ownership model -- the proxy lifecycle is independent of any single project.
- Multiple projects can come and go freely.
- No "first project owns the port" ambiguity.

**Cons:**
- Extra thing to manage -- user needs to run `prox proxy start` (or it auto-starts).
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
- `prox proxy start` also available for explicit control.

**Pros:**
- No extra manual step for users -- seamless experience.
- Proxy daemon is independent of any project's lifecycle.
- Supports both implicit and explicit management.

**Cons:**
- "Magic" background process that users might not realize is running.
- Need clear status/stop commands (`prox proxy status`, `prox proxy stop`).
- Auto-start logic adds complexity.

## Open Design Questions

1. **Opt-in vs default**: Should port sharing be the default behavior, or should projects explicitly opt in (e.g., `proxy: shared: true` in `prox.yaml`)? The default behavior change could surprise existing users.

2. **Domain conflicts**: What happens if two projects both try to register the same domain (e.g., both claim `api.local.stridelabs.ai`)? Options: first-come-first-served, error on the second, or last-write-wins.

3. **Certificate management**: The shared proxy needs certs for all registered domains. Currently certs are per-project. A shared proxy would likely need wildcard certs or dynamic cert generation as routes are added.

4. **Stale route cleanup**: If a project crashes without deregistering, the proxy has a stale route pointing at a dead backend. Options: heartbeat-based expiry, health checking registered backends, or relying on the project's PID liveness.

5. **Visibility and debugging**: How does the user inspect the shared proxy's state? Something like `prox proxy routes` to list all registered domains and which project owns them.

6. **Registration protocol**: Should project instances communicate with the proxy daemon via HTTP API, Unix socket, or filesystem-based coordination (e.g., writing route files to `~/.prox/routes.d/`)?

## Current Recommendation

Option C (auto-start daemon) likely provides the best user experience, but Option A (explicit daemon) is simpler and more predictable. Option B should be avoided due to the fragile ownership model.

Before designing this in detail, Issue 1 (port conflict detection) should be resolved first, as it is a prerequisite -- clear error messages when ports conflict will be needed regardless of which shared-proxy approach is chosen.
