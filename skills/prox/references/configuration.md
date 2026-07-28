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

## Dependencies, Tasks, and Process Gating

prox can wait on external resources and run one-shot setup commands before
your processes start, and gate a process's launch on either. Three pieces
work together:

- **`dependencies:`** — external resources (a database, a cache, another
  service) with a readiness check and an optional command to bring them up.
- **`tasks:`** — run-to-completion commands (migrations, seeding,
  registration) that run once per `prox up`.
- **`processes.<name>.depends_on`** — gates a process's launch on
  dependencies and/or tasks.

```yaml
dependencies:
  postgres:
    check:
      tcp: localhost:5432
    start: docker compose up -d postgres
    on_failure: fail

tasks:
  migrate:
    cmd: ./scripts/migrate.sh
    depends_on: [postgres]
    timeout: 2m

processes:
  api:
    cmd: go run ./cmd/server
    depends_on: [postgres, migrate]
```

### Dependency Fields

A `dependencies:` entry describes one external resource. Dependencies are
dependency-graph **roots**: they cannot themselves have a `depends_on` (a
`depends_on` key on a dependency is a validation error).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `check.tcp` | string | — | Dial `host:port`; a successful connect means ready. Exactly one of `check.tcp`/`check.url`/`check.cmd` must be set |
| `check.url` | string | — | `GET` an `http`/`https` URL; only a `2xx` response means ready. Redirects are **not** followed (a `3xx` is unhealthy); TLS uses the system trust store |
| `check.cmd` | string | — | Run via `sh -c`; exit `0` means ready |
| `check.timeout` | duration | `30s` | Total readiness budget for this dependency, **including** any `start` command's execution time. Must be `>=` `check.interval` |
| `check.interval` | duration | `1s` | Spacing between readiness probes. Must be greater than `0` |
| `start` | string | — | Command run via `sh -c` to bring the dependency up. Runs **only** when the initial check fails, and **at most once** per resolution — never when the initial check already passes |
| `on_failure` | string | `fail` | `fail` aborts the process(es) gated on this dependency; `warn` logs and lets them proceed anyway |

Each check attempt is additionally bounded by `min(2s, remaining budget)`, so
one hung dial/`GET`/command can never eat the whole `check.timeout` window —
the interval still governs spacing between attempts.

### Task Fields

A `tasks:` entry is a run-to-completion command — a migration, a seed
script, a one-time registration call.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run |
| `env` | map | — | Environment variables for this task |
| `env_file` | string | — | Task-specific `.env` file |
| `depends_on` | []string | — | Dependencies and/or other tasks that must be ready before this task runs. Processes are NOT valid targets |
| `timeout` | duration | `60s` | Run budget. `0` (or `0s`) means **unlimited** — distinct from omitting the field, which uses the `60s` default |
| `stop_timeout` | duration | inherits `shutdown_timeout` | This task's own SIGTERM→SIGKILL escalation budget |

A task exits `0` (natural) → **`completed`** (PID suppressed, uptime frozen
at completion). An *unexpected* exit — non-zero, a signal, or a `timeout`
kill — → **`crashed`**, which blocks any process or task gated on it. A
user-initiated `prox stop <task>` on a running task ends it in **`stopped`**,
not `crashed` — stopping a task is not a failure. A task runs
**once per `prox up` lifetime**: a dependent's restart does not re-run an
already-completed task. `prox restart <task>` (or `prox start <task>` after
`prox stop <task>`) re-runs it manually.

Plain `processes:` are unaffected — a process that exits `0` on its own is
still `crashed` (the pre-existing one-shot caveat — see "`prox status`
exit code, precisely" above). Use `tasks:` for anything meant to run once
and exit cleanly.

### Process `depends_on`

```yaml
processes:
  api:
    cmd: go run ./cmd/server
    depends_on: [postgres, migrate]
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `depends_on` | []string | — | Dependencies and/or tasks that must be ready before this process launches. Processes are NOT valid targets (process→process ordering is not supported) |

A process with a non-empty `depends_on` is **gated**: it starts in the
`waiting` state while its targets resolve in the background (`prox up`
returns without waiting on any of it), then either launches once every
target is ready, or settles into the terminal `blocked` state if a required
target failed. `prox restart <name>` on a running gated process re-resolves
every target (against a fresh config reload) **before** touching the running
instance — any resolution failure leaves it untouched.

### Validation Rules

- Names are unique across `processes`, `dependencies`, `tasks`
  (case-sensitive).
- **Unknown keys error everywhere in `prox.yaml`**, not just
  `dependencies:`/`tasks:`/`check:` — top level, `proxy:`, `proxy.capture:`,
  `api:`, `certs:`, every `processes.<name>` (incl. `healthcheck:`), every
  `services.<name>`. Format: `<path>: unknown field "<key>"`, e.g.
  `processes.web: unknown field "stop_timout"` (top level uses `config:` as
  the path). All structural errors in a file batch together under one
  `invalid configuration:` report; YAML-level defects (syntax, literal
  duplicate keys) surface immediately as `parsing yaml:` instead.
- Duplicate keys are rejected too: a literal duplicate is a YAML parse error
  (line-numbered); a duplicate created by an *aliased* key node is caught
  as `<path>: duplicate key "<key>"` (or by the YAML decoder itself when it
  duplicates a known typed-block field) — never silently collapsed.
- Anchors/aliases/`<<` merges are fully supported, including into typed
  blocks — a merge can't smuggle an unknown key past the destination's
  schema, and explicit keys win over merged ones (standard defaults idiom).
  Define an anchor at its first natural occurrence, not in a top-level
  `x-defaults:`-style container (top-level keys are schema-checked too, so
  that idiom is rejected) — e.g. `processes.a.env: &common {...}` then
  `processes.b.env: *common`.
- Exactly one of `check.tcp`/`check.url`/`check.cmd` must be present.
- `depends_on` targets must be dependencies or tasks — a process name or an
  unknown name is a load-time error.
- Duplicate `depends_on` entries are rejected.
- Cycles among tasks are rejected (task→task edges only).

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
| `proxy.capture.enabled` | bool | `true` | Capture request/response headers and bodies for proxied requests (see Request Capture below for the full field list) |
| `proxy.capture.max_body_size` | string | `1MB` | Maximum request or response body size to capture |

### Shared Proxy Daemon

When a proxy is configured, prox registers routes with a per-user shared proxy daemon under `~/.prox/`. This lets multiple projects use the same proxy port, including HTTPS on `443`, as long as each project owns distinct hostnames.

The behavior is automatic:

- `prox up` starts the daemon if needed and registers this project's services.
- `prox down` deregisters only this project.
- The daemon stops after the last project deregisters.
- `prox proxy status` and `prox proxy routes` show daemon state.

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

**Capture is on by default whenever the proxy is enabled.** A proxy-enabled project records request/response headers and bodies for the `prox requests <id>` detail view with no extra config — set `enabled: false` (or pass `--no-capture`) to opt out. Works identically standalone or through the shared daemon; a capture-disabled project sharing a daemon with capture-enabled ones still gets metadata-only records for itself.

```yaml
proxy:
  https_port: 6789
  domain: local.myapp.dev
  capture:
    enabled: true       # default; omit the whole capture: block to get this
    max_body_size: 1MB
    disk_budget: 1GB
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` when `proxy.enabled` is true | Master switch. `false` records metadata only (no headers/bodies) |
| `max_body_size` | string | `1MB` | Maximum body size to capture per record; larger bodies are truncated |
| `disk_budget` | string | `1GB` (1 GiB) | Ceiling on total spilled body bytes on disk; see below |

**How eviction works.** Only bodies over 64KB spill to disk; those files count against one budget — a project's own `disk_budget` in standalone mode, or, on a shared daemon, the **minimum** across every capture-enabled project's `disk_budget` (unset = `1GB` default). An explicit value can only lower the shared bound; raising it above the default needs every capture-enabled project to opt in (e.g. `{A: 2GB, B: unset}` → `min(2GB, 1GB)` = `1GB`; both set to `2GB` → `2GB`). Over budget, prox evicts the **oldest record group first** (a record's request+response spill files age together, FIFO by first-spill time, never LRU) across every project sharing the daemon. Only spilled files are removed — ring metadata survives, and a fetch for an evicted body reports it as no longer available.

**Captured data is stored in cleartext.** Request URLs and request/response headers are retained verbatim — including `Authorization`/`Cookie` headers, sensitive query params, URL fragments/userinfo, and URL-bearing `Location`/`Referer` values. No value is altered or hidden. Bodies are also verbatim up to `max_body_size` (truncation is length-only, never content-inspecting). Bodies over 64KB spill to `.prox/capture` (project) / `~/.prox/capture` (shared daemon); capture dirs are mode `0700` and spill files `0600`, so captures are private to your user. The only reliable per-project opt-out for secret-carrying traffic is `enabled: false`.

Captured body files are stored under `.prox/capture/` (standalone) or the shared daemon's `~/.prox/capture/` (one flat dir for every project) and cleaned up as request records age out of the in-memory request buffer. Make sure `.prox/` is in `.gitignore` (see above) — doubly important now that capture is on by default.

`prox proxy status --json` reports `capture_available`/`capture_error` (whether the daemon's own capture manager initialized) and `capture_disk_used`/`capture_disk_budget` (the effective daemon-wide totals above).

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

Custom domains require DNS entries pointing to `127.0.0.1`. The simplest option is a public localhost-DNS service (no `/etc/hosts` editing; requires internet for DNS resolution):
- `lvh.me` — `*.lvh.me` resolves to 127.0.0.1
- `sslip.io` / `nip.io` — embed the IP, e.g. `app.127.0.0.1.sslip.io`
- `localtest.me` — `*.localtest.me` resolves to 127.0.0.1

For a custom offline domain, add `/etc/hosts` entries manually:

```text
127.0.0.1 local.myapp.dev app.local.myapp.dev api.local.myapp.dev
```

## Security Note

Commands in `prox.yaml` are executed via shell. Only use configuration files from trusted sources, similar to Makefiles or Procfiles.

When binding to non-localhost interfaces (`host: 0.0.0.0`), authentication is automatically enabled. A bearer token is generated and stored in `~/.prox/token`.
