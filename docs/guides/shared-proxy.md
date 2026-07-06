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
| `prox proxy status` | Show daemon version, PID, uptime, project count, route count, and listener ports |
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

## Constraints

| Constraint | Behavior |
|------------|----------|
| Scope | The daemon is per-user and per-machine. State lives in `~/.prox/`. |
| Hostname ownership | The same `hostname:port` cannot be registered by two projects. The second registration fails. |
| Protocol ownership | A listener port is HTTP or HTTPS. Mixing protocols on the same port is rejected. |
| Version matching | A project can join only when its `prox` version matches the running daemon. Use `prox proxy stop --force` to reset a stale daemon after upgrading. |
| Fallback | If `~/.prox/` is unavailable, prox runs a standalone per-project proxy. Port sharing is not available in fallback mode. |

## Files

Shared proxy state is stored under `~/.prox/`:

| File | Description |
|------|-------------|
| `~/.prox/proxy.sock` | Unix socket used by project processes to register routes |
| `~/.prox/proxy.pid` | Shared proxy daemon PID |
| `~/.prox/proxy.state` | Daemon state, including version and address metadata |
| `~/.prox/proxy.log` | Shared proxy daemon log |

Project supervisor state remains project-local in `.prox/`.
