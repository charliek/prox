# Shared Proxy Across Projects

prox can run one shared proxy daemon per user account so multiple projects can use the same HTTP or HTTPS port. This is most useful for local HTTPS on port 443, where each project should have a stable hostname and no one should remember per-project ports.

The shared daemon is automatic. When `prox up` starts a project with a `proxy` section, prox starts the daemon if needed and registers that project's service hostnames. When `prox down` stops the project, prox deregisters its routes. The daemon stops after the last project leaves.

## Example

Project A can expose auth services:

```yaml
proxy:
  enabled: true
  https_port: 443
  domain: local.example.dev

services:
  auth: 3000
  authapi: 8000
```

Project B can expose app services on the same port:

```yaml
proxy:
  enabled: true
  https_port: 443
  domain: local.example.dev

services:
  app: 5173
  api: 8002
```

Start each project from its own directory:

```bash
cd ~/projects/auth
prox up -d

cd ~/projects/app
prox up -d
```

Both projects now share `:443`:

| URL | Target |
|-----|--------|
| `https://auth.local.example.dev` | Project A, `localhost:3000` |
| `https://authapi.local.example.dev` | Project A, `localhost:8000` |
| `https://app.local.example.dev` | Project B, `localhost:5173` |
| `https://api.local.example.dev` | Project B, `localhost:8002` |

Routing uses the full hostname. Projects can share a port when their hostnames are distinct.

## Inspect the Daemon

The shared proxy is normally managed by `prox up` and `prox down`. Use these commands when debugging routing or ownership:

| Command | Description |
|---------|-------------|
| `prox proxy status` | Show daemon version, PID, uptime, project count, route count, listener ports, and capture disk usage |
| `prox proxy status --json` | Show daemon status as JSON |
| `prox proxy routes` | List every registered hostname, protocol, target, project directory, and PID |
| `prox proxy routes --json` | Show registered routes as JSON |
| `prox proxy stop` | Stop the daemon only when no projects have active routes |
| `prox proxy stop --force` | Stop the daemon even when projects still have active routes |

```bash
prox proxy routes
```

Example output:

```text
HOSTNAME                    PORT  PROTOCOL  TARGET          PROJECT              PID
--------                    ----  --------  ------          -------              ---
auth.local.example.dev      443   https     localhost:3000  /projects/auth       12345
app.local.example.dev       443   https     localhost:5173  /projects/app        12391
```

**Capture is shared too.** Request/response capture (on by default per project) spills large bodies into one flat `~/.prox/capture` directory shared by every project on the daemon, bounded by one daemon-wide disk budget — the minimum across every capture-enabled project's own `disk_budget`, 1GiB by default. `prox proxy status` prints a `Capture: <used> used / <budget> budget on disk` line (or `unavailable` if the daemon's capture manager failed to start); see [Request Capture](../reference/configuration.md#request-capture) for the eviction rule and how the shared budget is computed.

## Health and Self-Healing

`prox status` in each project reports the shared daemon's health, not just its own process health — see the `Proxy:` line in [`prox status`](../reference/cli.md#status). If the shared daemon dies (crash, `kill -9`, a bad upgrade), every project registered with it prints `Proxy: DOWN` and exits `1` from `prox status`, even though their own processes are still running.

No operator action is required to recover: each project's forwarder detects the prolonged failure and re-registers with a fresh or recovered daemon automatically, worst case within about 45 seconds. Once the proxy heals, `prox status` goes back to `Proxy: shared (running, vX.Y.Z)` — and exits `0` **provided no child process is also in the `crashed` state**, which fails `prox status` independently of proxy health (see [`prox status`](../reference/cli.md#status)). Treat a brief `DOWN` reading as transient rather than something to page on.

## Constraints

| Constraint | Behavior |
|------------|----------|
| Scope | The daemon is per-user and per-machine. State lives in `~/.prox/`. |
| Hostname ownership | The same `hostname:port` cannot be registered by two projects. The second registration fails. |
| Protocol ownership | A listener port is HTTP or HTTPS. Mixing protocols on the same port is rejected. |
| Version matching | A project can join only when its `prox` version matches the running daemon. After upgrading `prox`, the next `prox up` against an idle daemon of the old version replaces it automatically with a one-line notice. If the old daemon still has projects registered, `prox up` fails hard instead — run `prox proxy stop --force`, then `prox up` (or `prox restart`) in every project listed in the error. |
| Fallback | If `~/.prox/` is unavailable, prox runs a standalone per-project proxy. Port sharing is not available in fallback mode. |
| Capture disk budget | Every capture-enabled project shares one `~/.prox/capture` directory and one daemon-wide budget (the minimum across each project's `disk_budget`, 1GiB default). One noisy project's large bodies can trigger eviction of another project's older captures — only the spilled body files, never metadata. See [Request Capture](../reference/configuration.md#request-capture). |

## Files

Shared proxy state is stored under `~/.prox/`:

| File | Description |
|------|-------------|
| `~/.prox/proxy.sock` | Unix socket used by project processes to register routes |
| `~/.prox/proxy.pid` | Shared proxy daemon PID |
| `~/.prox/proxy.state` | Daemon state, including version and address metadata |
| `~/.prox/proxy.log` | Shared proxy daemon log |

Project supervisor state remains project-local in `.prox/`.
