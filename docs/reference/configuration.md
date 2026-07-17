# Configuration Reference

## Config File

prox looks for `prox.yaml` in the current directory by default. Use `--config` to specify a different path.

## Minimal Example

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

## Full Example

```yaml
api:
  port: 5555
  host: 127.0.0.1

env_file: .env

# Default stop budget for every process (SIGTERM->SIGKILL escalation window).
shutdown_timeout: 10s

processes:
  # Simple form - string command
  web: npm run dev
  worker: python worker.py

  # Expanded form - full configuration
  api:
    cmd: go run ./cmd/server
    # Give this process a longer graceful-drain window than the global default.
    stop_timeout: 30s
    env:
      PORT: "8080"
      DEBUG: "true"
    env_file: .env.api
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
```

## Top-Level Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `api.port` | int | dynamic | HTTP API port. When unset (or `0`), a free port is auto-assigned; an explicit port is used as-is — if it's already in use the API server fails to start (logged as an error) rather than picking another port |
| `api.host` | string | `127.0.0.1` | API bind address |
| `api.auth` | bool | auto | Force authentication on (`true`) or off (`false`). Omitted: auth is enabled automatically unless `api.host` is localhost-only |
| `env_file` | string | — | Global .env file path, loaded for all processes |
| `shutdown_timeout` | duration | `10s` | Default stop budget for every process (the SIGTERM→SIGKILL escalation window; see [Stop Timeout](#stop-timeout)). Overridable per process via `stop_timeout`. Must be greater than `2s` and at most `10m` |
| `processes` | map | required | Process definitions |

## Process Fields

Processes can be defined in simple form (string) or expanded form (object).

### Simple Form

```yaml
processes:
  web: npm run dev
```

### Expanded Form

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run |
| `env` | map | — | Environment variables for this process |
| `env_file` | string | — | Process-specific .env file |
| `stop_timeout` | duration | inherits `shutdown_timeout` | This process's stop budget, overriding the global `shutdown_timeout` (see [Stop Timeout](#stop-timeout)). Must be greater than `2s` and at most `10m` |
| `healthcheck` | object | — | Health check configuration |

## Stop Timeout

When prox stops a process — via `prox stop <name>`, `prox restart <name>`, or a
full `prox stop` / Ctrl-C of the daemon — it first sends `SIGTERM` and waits for
the process to exit gracefully, then escalates to `SIGKILL`. The **stop budget**
controls that escalation:

- The effective budget for a process is its own `stop_timeout`, else the global
  `shutdown_timeout`, else the built-in default of `10s`.
- The budget is the **SIGTERM→SIGKILL escalation window**. A fixed `2s` at the
  tail is reserved for the `SIGKILL` phase, so the graceful window is
  **`budget − 2s`** (e.g. a `10s` budget gives ~8s of graceful drain before
  `SIGKILL`). The reserve is not configurable.
- Because of that reserve, the budget must be **greater than `2s`**; values of
  `2s` or less (including `0s` and negatives) are rejected at load with a
  field-named error. The upper bound is **`10m`**; `10m` exactly is accepted.
- The configured value bounds **escalation timing**, not strict total
  wall-clock — a process that is slow to flush its output can add a few seconds
  of drain time on top.
- The budget is honored on every stop path: `prox stop`, `prox restart` (the
  stop half), and full daemon shutdown, where each process is stopped on its
  **own** budget concurrently (a large budget on one process no longer forces a
  shared shorter deadline on the others).
- The effective budget is reported as `stop_timeout` in the
  [process detail API](api.md#get-processesname), so you can see the budget
  governing a long stop.

## Config Reload on Restart

Whenever a process is (re)started through the API — `prox restart <name>`, or
`prox start <name>` after a `prox stop <name>` — prox re-reads `prox.yaml` and
runs the process with the config the file **now** contains. So editing a
process and restarting it is enough; a full `prox stop` + `prox up` is not
needed for the fields below.

- Applied on (re)start: `cmd`, `healthcheck`, `stop_timeout`, and environment
  inputs (inline `env`, per-process and global `env_file`, including changed
  file paths). A changed `stop_timeout` governs the **next** stop — the
  restart's own stop half still uses the pre-edit budget.
- NOT applied (these require `prox up`): process renames, added or removed
  processes, and `services` / `proxy` / port changes.
- The reload is **fail-closed**: the whole file is re-validated, so an invalid
  edit anywhere in it — even in an unrelated process or the proxy section — or a
  missing referenced env file aborts the (re)start and the existing process keeps
  running unchanged. See the [restart API](api.md#post-processesnamerestart)
  reference for the exact error codes (`CONFIG_RELOAD_FAILED`,
  `PROCESS_NOT_IN_CONFIG`).

## Health Check Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run for health check |
| `interval` | duration | `10s` | Time between health checks |
| `timeout` | duration | `5s` | Timeout for each check |
| `retries` | int | `3` | Consecutive failures before marking unhealthy |
| `start_period` | duration | `30s` | Grace period after startup before checks begin |

Duration fields use Go's duration syntax (e.g. `500ms`, `30s`, `1m30s`). An invalid or negative duration makes `prox up` fail at startup with a clear error naming the field (e.g. `processes.api.healthcheck.interval: invalid duration "3x"`). Omitting a field — or setting it to `0` — uses the default shown above.

## Environment Variable Precedence

Environment variables are loaded in this order (later values override earlier):

1. System environment
2. Global `env_file` (if specified)
3. Process-specific `env_file` (if specified)
4. Process-specific `env` map (if specified)

## Duration Format

Duration fields accept Go duration strings:

- `5s` - 5 seconds
- `10m` - 10 minutes
- `1h30m` - 1 hour 30 minutes

Different duration fields enforce different bounds. Health check durations accept
any non-negative value (and treat `0` as "use the default"). The stop-budget
fields `shutdown_timeout` and `stop_timeout` must be greater than `2s` and at
most `10m` (see [Stop Timeout](#stop-timeout)); a value outside that range —
including `0s` or a negative — is rejected at load with a field-named error.

## Runtime State

When prox is running (in either foreground or daemon mode), runtime state is stored in the `.prox/` directory within your project:

| File | Description |
|------|-------------|
| `.prox/prox.state` | JSON file with port, PID, host, start time, config path |
| `.prox/prox.pid` | Process ID with file locking to prevent multiple instances |
| `.prox/prox.log` | Daemon logs (stdout/stderr redirected here in background mode) |

When running in daemon mode (`prox up -d`), all output that would normally go to stdout/stderr is redirected to `.prox/prox.log`. This is useful for debugging startup issues or reviewing daemon activity.

**State File Format:**

```json
{
  "pid": 12345,
  "port": 5555,
  "host": "127.0.0.1",
  "started_at": "2024-01-15T10:30:00Z",
  "config_file": "prox.yaml"
}
```

CLI commands automatically discover the API address by reading `.prox/prox.state`. This enables:

- Running multiple prox instances (different projects) simultaneously
- Dynamic port allocation without port conflicts
- No need to specify `--addr` for local commands

The `.prox/` directory is project-local, so add it to your `.gitignore`:

```gitignore
.prox/
```

## Proxy Configuration

prox can act as an HTTP and/or HTTPS reverse proxy, providing friendly subdomain URLs for your services. HTTP-only mode requires no certificate setup. HTTPS mode uses locally-trusted certificates via mkcert.

### HTTP-Only Example

The simplest proxy setup — no certificates required:

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  http_port: 6788
  domain: local.myapp.dev

services:
  app: 3000
  api: 8000
```

With this configuration:
- `http://app.local.myapp.dev:6788` → `http://localhost:3000`
- `http://api.local.myapp.dev:6788` → `http://localhost:8000`

### HTTPS Example

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  https_port: 6789
  domain: local.myapp.dev

services:
  app: 3000
  api: 8000

certs:
  dir: ~/.prox/certs
  auto_generate: true
```

With this configuration:
- `https://app.local.myapp.dev:6789` → `http://localhost:3000`
- `https://api.local.myapp.dev:6789` → `http://localhost:8000`

### Dual-Stack Example

Run both HTTP and HTTPS simultaneously:

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  http_port: 6788
  https_port: 6789
  domain: local.myapp.dev

services:
  app: 3000                    # Simple: subdomain → port
  api:                         # Expanded: with options
    port: 8000
    host: localhost

certs:
  dir: ~/.prox/certs
  auto_generate: true
```

### Proxy Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `proxy.enabled` | bool | auto | Enable reverse proxy (auto-enabled when a port is set) |
| `proxy.http_port` | int | — | Port for the HTTP proxy server |
| `proxy.https_port` | int | `6789` | Port for the HTTPS proxy server (default when enabled with no ports set) |
| `proxy.domain` | string | required | Base domain used to derive hostnames for shared proxy routing |
| `proxy.capture.enabled` | bool | `false` | Capture request and response body metadata for proxied requests |
| `proxy.capture.max_body_size` | string | `1MB` | Maximum request or response body size to capture |

### Shared Proxy Daemon

When a proxy is configured, prox registers routes with a per-user shared proxy daemon under `~/.prox/`. This lets multiple projects use the same proxy port, including HTTPS on `443`, as long as each project owns distinct hostnames.

The behavior is automatic:

- `prox up` starts the daemon if needed and registers this project's services.
- `prox down` deregisters only this project.
- The daemon stops after the last project deregisters.
- `prox proxy status` and `prox proxy routes` show daemon state.

See the [Shared Proxy Across Projects](../guides/shared-proxy.md) guide for examples and constraints.

### Service Fields

Services can be defined in simple form (port only) or expanded form (object).

#### Simple Form

```yaml
services:
  app: 3000
```

#### Expanded Form

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `port` | int | required | Target port to proxy to |
| `host` | string | `localhost` | Target host to proxy to |

### Certificate Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `certs.dir` | string | `~/.prox/certs` | Directory for storing certificates |
| `certs.auto_generate` | bool | `true` | Automatically generate certificates using mkcert |

### Request Capture

Request capture records request and response body metadata for the `prox requests <id>` detail view. Bodies up to `proxy.capture.max_body_size` are retained; larger bodies are truncated.

```yaml
proxy:
  https_port: 6789
  domain: local.myapp.dev
  capture:
    enabled: true
    max_body_size: 1MB
```

Captured body files are stored under `.prox/capture/` and cleaned up as request records age out of the in-memory request buffer.

### Prerequisites (HTTPS only)

HTTPS mode requires [mkcert](https://github.com/FiloSottile/mkcert) for certificate generation. HTTP-only mode has no prerequisites.

```bash
# macOS
brew install mkcert

# Linux
# See https://github.com/FiloSottile/mkcert#installation

# Install the CA (run once)
mkcert -install
```

### DNS Setup

Custom domains require DNS entries pointing to `127.0.0.1`. See the [Local DNS & Certificates](../guides/local-dns.md) guide for options including localhost DNS services that work without `/etc/hosts` editing.

## Security Note

Commands in `prox.yaml` are executed via shell. Only use configuration files from trusted sources, similar to Makefiles or Procfiles.

When binding to non-localhost interfaces (`host: 0.0.0.0`), authentication is automatically enabled. A bearer token is generated and stored in `~/.prox/token`.
