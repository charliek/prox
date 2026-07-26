# CLI Reference

## Usage

```
prox <command> [options]
```

## Global Options

| Flag | Description |
|------|-------------|
| `--config, -c` | Config file path (default: `prox.yaml`) |
| `--addr` | API address for client commands. Used only when passed explicitly; otherwise auto-discovered from `.prox/prox.state` (or `prox.yaml` for a pinned `api.port`) |
| `--detach, -d` | Run in background (daemon mode) |
| `--verbose, -v` | Enable verbose output |
| `--version` | Show version information |

**No implicit `:5555` fallback.** Client commands (`status`, `logs`, `stop`, `start`, `restart`, `down`, `attach`, `requests`) discover the running instance from `.prox/prox.state` in the current directory. If no state file (or configured port) is found and `--addr` was not passed explicitly, the command errors instead of silently dialing the compiled-in `127.0.0.1:5555` default — run the command from the project directory, or pass `--addr host:port`. `version`, `up`, `proxy`, and `completion` are unaffected (they never need discovery).

## Commands

### up

Start processes. By default runs in the foreground; use `--detach` for background/daemon mode.

```bash
prox up [processes...]
```

| Flag | Description |
|------|-------------|
| `--detach, -d` | Run in background (daemon mode) |
| `--tui` | Enable interactive TUI mode (foreground only, mutually exclusive with `--detach`) |
| `--api-port, -p` | Override API server port (otherwise dynamic) |
| `--http-port` | Override proxy HTTP port |
| `--https-port` | Override proxy HTTPS port |
| `--no-proxy` | Disable proxy even if configured |
| `--capture` | Force request/response body capture on for this run. Capture is already on by default whenever the proxy is enabled, so this flag is kept for explicitness/compatibility; mutually exclusive with `--no-capture` |
| `--no-capture` | Disable request/response body capture for this run (config-level opt-out: `proxy.capture.enabled: false`) |

**Examples:**

```bash
# Start all processes (foreground)
prox up

# Start in background (daemon mode)
prox up -d

# Start specific processes
prox up web api

# Start with TUI (foreground only)
prox up --tui

# Start specific processes with TUI
prox up --tui web api

# Override API port
prox up --api-port 6000

# Daemon mode with specific port
prox up -d --api-port 6000

# Start with HTTP proxy only
prox up --http-port 6788

# Start with dual-stack proxy
prox up --http-port 6788 --https-port 6789

# Body capture is already on by default; --capture is kept for explicitness
prox up --capture

# Disable body capture for this run
prox up --no-capture
```

**Dynamic Port Allocation:**

When no port is specified (via `--api-port` or `api.port` in config), prox automatically finds an available port. The port is stored in `.prox/prox.state` and auto-discovered by CLI commands.

**`-d`/`--detach` readiness:** the parent process no longer exits `0` the instant it forks the child. It polls for up to 15s for the child to write a PID-matched `.prox/prox.state` and answer `GET /health`, then prints `prox started (pid N, api http://host:port)` and exits `0` — a truthful signal that the daemon is actually accepting requests. If the child dies during startup (bad config, port bind failure), `prox up -d` exits `1` and prints the last ~20 lines of `.prox/prox.log`. If the child never becomes ready within 15s, `prox up -d` prints the same diagnostics, sends SIGTERM to the child (SIGKILL after a 5s grace if it doesn't exit), and exits `1`.

**Shared-proxy version mismatch:** when this project's proxy would join the per-user shared daemon and the daemon's version doesn't match this `prox` binary's version, `prox up` no longer falls back to a proxy-less standalone start. If the daemon still has registered projects, `prox up` fails hard, naming both versions and the registered project directories, with remediation (`prox proxy stop --force`, then `prox up`/`prox restart` in each listed project). If the daemon is idle, prox auto-replaces it with a fresh daemon of the current version and prints a one-line notice.

### status

Show process status.

```bash
prox status
```

| Flag | Description |
|------|-------------|
| `--json` | Output as JSON |

**Examples:**

```bash
# Human-readable output
prox status

# JSON output (for scripting)
prox status --json
```

**Exit code:** `prox status` exits `0` only when the supervisor query succeeded, **and** no process is in the `crashed` or `blocked` state, **and** no `dependencies:` entry is in the terminal `failed` state, **and** any configured shared proxy is reachable. It does **not** assert that every process is `running` or `healthy`: `starting`, `stopping`, `waiting`, deliberately-`stopped`, `completed` (a task that ran to completion), and running-but-`unhealthy` processes all still exit `0` (health is a separate axis, kept out of the exit contract; a `warned` dependency also never trips it — its dependents still ran). It exits `1` when any of these hold:

- **any child is `crashed`** — the table adds a `Crashed: <name>[, ...] — check 'prox logs <name>'.` line and stderr carries `Error: N process(es) crashed`. Note the supervisor marks *any* non-Stop-driven exit as `crashed`, including a one-shot **plain process** that exits `0` (a migration/seed step run as a bare `processes:` entry); such a project fails `prox status` until restarted, so keep one-shot helpers out of `processes:` — use a [`tasks:`](configuration.md#dependencies-tasks-and-process-gating) entry instead, whose natural `exit 0` lands in the dedicated `completed` state and does **not** trip this exit contract. This plain-process behavior is unchanged and is a breaking change from an earlier version (see [#72](https://github.com/charliek/prox/issues/72)).
- **any process is `blocked`** — a gated process (`depends_on`) whose required dependency failed to become ready will never launch. The table adds a `Blocked: <name>(<target>[, ...])[, ...]` line naming the process and its failed targets, and stderr carries `Error: N process(es) blocked on failed dependencies`.
- **any `dependencies:` entry is `failed`** (fail-policy dependency that exhausted its check budget) — stderr carries `Error: N dependencies failed` (or the singular `1 dependency failed`). This is reported only when no crashed/blocked/proxy-down signal already outranks it (see precedence below); the failed dependency itself is always visible in the `Dependencies:` section regardless.
- **a configured shared proxy is unreachable** — output includes a `Proxy: DOWN — shared proxy daemon unreachable (proxied routes are dead). Check 'prox proxy status'.` line. This is a breaking change from earlier versions where `prox status` never consulted the shared proxy. The project self-heals in the background (worst case ~45s), so a brief `DOWN` reading is often transient.

**Precedence.** All applicable human-readable lines print regardless of which condition holds — a crashed process, a blocked process, a failed dependency, and a down proxy can all show up in the same output. The single primary stderr sentinel (and the process's own exit code stays `1` either way) follows a fixed precedence: **proxy-down > crashed > blocked > failed-dependency**. Scripts that need to react to a specific condition should parse `prox status --json` (its per-process `status`, and each dependency's `state`, are authoritative) rather than `prox status || true`, which also masks discovery errors and an unreachable supervisor.

**Proxy line:** when a proxy is configured, output includes a `Proxy:` line reporting the shared-proxy health tracked by [the `proxy` block of `GET /status`](api.md#get-status) — `Proxy: shared (running, vX.Y.Z)` when healthy, `Proxy: standalone` for an in-process proxy, or the `Proxy: DOWN` line above when the shared daemon is unreachable.

**STATUS column decoration.** A `waiting` process shows its still-resolving `depends_on` targets inline — `waiting(postgres, redis)` — and a `blocked` process shows the targets that failed it — `blocked(postgres)` — in declaration order. The JSON `status` field stays the bare state name in both cases (`waiting`/`blocked`); only the human table decorates it. The PID column shows `-` whenever there is no live PID, including a `waiting` process and a `completed` task (whose uptime is frozen at completion rather than reading `0`).

**Dependencies section.** When `dependencies:` are configured, output gains a `Dependencies:` table — one row per dependency with its `NAME`, `STATE` (`pending`/`checking`/`starting`/`polling`/`healthy`/`warned`/`failed`/`canceled`), a one-line `CHECK` summary (e.g. `tcp localhost:5432`), and, for a `failed` or `warned` row, the last probe error as `DETAIL`. See the [Dependencies and Tasks guide](../guides/dependencies.md) for a worked example end to end.

**Tasks in `up`/`start`/`stop`/`restart`.** A `tasks:` entry behaves like any other managed child for these commands, with one difference: it runs **once per `prox up` lifetime**. `prox up` (with or without a process subset) also demands every task with no unsatisfied `depends_on`. A task that has already reached `completed` is not re-run by a dependent's restart; `prox restart <task>` (or `prox stop <task>` + `prox start <task>`) re-runs it manually, reloading its config and re-resolving its own `depends_on` first. `prox stop <task>` on a running task stops it like any process (`stopped`, not `crashed`/`completed`).

### logs

Show or stream logs.

```bash
prox logs [process]
```

| Flag | Description |
|------|-------------|
| `-f, --follow` | Stream logs continuously |
| `-n, --lines` | Number of lines (default: 100) |
| `--process` | Filter by process name |
| `--pattern` | Filter by pattern (substring match) |
| `--regex` | Treat pattern as regex |
| `--json` | Output as JSON |

**Examples:**

```bash
# Show last 100 lines
prox logs

# Show last 50 lines from api process
prox logs --process api --lines 50

# Stream all logs
prox logs -f

# Stream logs from api
prox logs -f --process api

# Filter for errors
prox logs --pattern ERROR

# Regex filter
prox logs --pattern "GET|POST" --regex

# JSON output for piping
prox logs -f --json | jq .
```

### start

Start a stopped process.

```bash
prox start <process>
```

Like `restart`, `start` re-reads `prox.yaml` and launches the process with its **current** config (see [Config reload on (re)start](#config-reload-on-restart) below). Editing a process's `cmd` and running `prox stop <process>` + `prox start <process>` applies the change, just as `prox restart <process>` does.

**Examples:**

```bash
prox start api
prox start worker
```

### stop

Stop the running prox instance or a specific process.

```bash
prox stop [process]
```

Without arguments, sends a shutdown signal to the daemon. All processes receive SIGTERM, then SIGKILL after a timeout.

With a process name, stops only that process while keeping prox and other processes running.

The SIGTERM→SIGKILL timeout is configurable per process via `stop_timeout` (or globally via `shutdown_timeout`; default `10s`) — see [Stop Timeout](configuration.md#stop-timeout). A process with a large budget may make `prox stop` wait a while before returning: the server is authoritative and holds the request open until the process actually stops (up to the configured budget), so a long silent wait is expected rather than a hang. Pressing Ctrl-C on the CLI is safe — it only detaches the client; the daemon keeps stopping the process on its configured budget.

**Full stop waits for the outcome.** `prox stop` (no arguments) and `prox down` block until the daemon reports the process-stop verdict, then wait briefly (up to ~15s) for the daemon's state and PID files to disappear before printing a stopped summary. Exit codes:

- **0** — all processes (and their process groups) stopped cleanly, **and** the daemon confirmed its own teardown (state/PID files gone) within the bounded wait. A summary line is printed.
- **1** — one or more process groups survived shutdown. Each survivor is printed as a `process: error` line, followed by a one-line summary; the group still holds whatever ports it bound.
- **1** — the connection dropped mid-wait: the daemon may still be completing its shutdown and the outcome is unknown. When a daemon log file is present (detached mode), the message points at `.prox/prox.log` for the authoritative result.
- **1** — the process-stop verdict was clean but the daemon didn't finish its own teardown within the bounded wait. A `Stopped processes` summary and a `Warning: the daemon is still finishing shutdown` line (stderr) are printed, and the command returns a `shutdown incomplete: daemon still finishing after 15s` error — an unconfirmed daemon exit is treated the same as a survivors failure (breaking change, [#73](https://github.com/charliek/prox/issues/73)).

Against an older daemon that predates the waited protocol, `prox stop` falls back to the previous fire-and-forget behavior: it prints `Shutdown initiated` and exits `0`.

**Examples:**

```bash
# Stop entire prox instance
prox stop

# Stop only the api process
prox stop api
```

### down

Alias for `stop` (without arguments). Provides symmetry with `prox up --detach`.

```bash
prox down
```

### attach

Attach TUI to a running daemon. Opens an interactive terminal UI connected via the API.

```bash
prox attach
```

**Examples:**

```bash
# Start daemon
prox up -d

# Attach TUI to running daemon
prox attach

# TUI operations work the same as `prox up --tui`
# Press q to detach (daemon continues running)
```

**Connection Errors:**

If the daemon stops while the TUI is attached, the TUI will show a connection error. Press `q` to quit, then restart the daemon with `prox up -d`.

### restart

Restart a specific process.

```bash
prox restart <process>
```

**Examples:**

```bash
prox restart api
prox restart worker
```

The stop half of a restart uses the process's **pre-edit** stop budget (see [Stop Timeout](configuration.md#stop-timeout)); a changed `stop_timeout` takes effect on the next stop after the restart.

#### Config reload on (re)start

Every API-driven (re)start — `prox restart <process>`, and `prox start <process>` after a `prox stop <process>` — re-reads `prox.yaml` first and runs the process with **what the file now says**. This makes the edit → restart → observe loop work without a full `prox stop` + `prox up`.

**Applied on (re)start:**

- `cmd`
- `healthcheck`
- `stop_timeout` (the new value governs the *next* stop; the restart's own stop half uses the pre-edit budget)
- environment inputs — inline `env`, per-process `env_file`, and the global `env_file`, including **changed file paths** (the values are re-read from disk)

**NOT applied on (re)start (require `prox up`):**

- process **renames** (a rename is a delete + add)
- **added or removed** processes
- `services`, `proxy`, ports, and other top-level daemon settings

**Failure semantics (fail-closed):** the whole file is re-validated, so an invalid edit — even in an *unrelated* process or the proxy section — or a referenced env file that is missing aborts the (re)start with an error, and the existing process keeps running unchanged. Two cases have dedicated errors:

- an invalid/unreadable/malformed file → `CONFIG_RELOAD_FAILED` (HTTP 422)
- the target process no longer present in the file → `PROCESS_NOT_IN_CONFIG` (HTTP 409)

Fix the file (or run `prox up` to reconcile added/removed/renamed processes) and try again.

### requests

Show, stream, or inspect proxy requests.

```bash
prox requests [id] [options]
```

| Flag | Description |
|------|-------------|
| `-f, --follow` | Stream requests continuously |
| `-n, --limit` | Number of requests to show (default: 100) |
| `--subdomain` | Filter by subdomain |
| `--method` | Filter by HTTP method (GET, POST, etc.) |
| `--min-status` | Filter by minimum status code (e.g., 400 for errors) |
| `--max-status` | Filter by maximum status code (combine with `--min-status 400` for client errors only). Must be 100-599; when both `--min-status` and `--max-status` are set, `--min-status` must not exceed `--max-status` |
| `--since` | Filter to requests since this time — an RFC3339 timestamp or a Go duration (e.g. `5m`, `1h`) evaluated as "ago" from the moment the command runs |
| `--url` | Filter by URL substring (path+query only, case-insensitive) |
| `--json` | Output as JSON |
| `--body` | Include captured request/response bodies when showing one request by ID |

**Examples:**

```bash
# Show recent requests
prox requests

# Stream requests in real-time
prox requests -f

# Filter by subdomain
prox requests --subdomain api

# Filter by HTTP method
prox requests --method GET

# Show only errors (4xx and 5xx)
prox requests --min-status 400

# Show only client errors (4xx)
prox requests --min-status 400 --max-status 499

# Show requests from the last 5 minutes
prox requests --since 5m

# Filter by URL substring
prox requests --url /api

# Agent-oriented: recent 4xx traffic to /api in the last 5 minutes
prox requests --url /api --since 5m --min-status 400 --max-status 499

# JSON output for piping
prox requests --json | jq .

# Show details for one request
prox requests abc1234def56

# Include captured bodies for one request
prox requests abc1234def56 --body
```

**Request IDs:**

Each request is assigned a short hash ID (12 characters, git-style). These IDs are displayed in the output and can be used to reference specific requests.

Body output works out of the box: capture is on by default whenever the proxy is enabled, so `--body` just works without any config. Header/query-param values matching the built-in (or configured) redaction sets show as `[REDACTED]`/`REDACTED` rather than the real value — see [Request Capture](configuration.md#request-capture) for the full list and its limits (bodies are captured verbatim; redaction does not cover them). Capture applies in both standalone and shared-daemon mode — the effective policy is propagated to the daemon at registration, and a daemon captures bodies only for the projects that have not opted out.

When a request has no captured detail, `prox requests <id>` shows one of three hints depending on what's known: a `101`/WebSocket-upgrade record always shows `(metadata only - WebSocket/101 upgrade traffic is never captured, regardless of capture config)`; a project whose static config has capture off shows `(capture not enabled - proxy.capture.enabled is false or --no-capture was used; run 'prox up --capture' or drop --no-capture to enable)`; otherwise a neutral catch-all names every real possibility — `--no-capture` for this run, a metadata-only non-101 record (e.g. a routing error), or the daemon's capture manager being unavailable — and points at `prox proxy status` to check. See [Request Capture](configuration.md#request-capture) for capture directories, the disk budget, and daemon-upgrade notes.

**In-flight requests:** a request whose response is still streaming (headers received, body not yet finished) shows `...` in the DURATION column of the table, `(in flight)` instead of `(Nms)` in `-f/--follow` output, and `Duration: (in flight)` with `(request in flight — details arrive on completion)` in the detail view. Table and TUI rows update in place once the request completes; `-f/--follow` output is a stream, so it prints a second line for the completion event (same ID, no `in_flight`). `--json` carries this as `"in_flight": true`, omitted once done.

### proxy

Inspect and control the shared proxy daemon.

The proxy daemon is normally started and stopped automatically by `prox up` and `prox down`. These commands are for debugging route ownership, checking shared ports, and resetting a stale daemon.

```bash
prox proxy <command>
```

| Command | Description |
|---------|-------------|
| `prox proxy status` | Show daemon version, PID, uptime, projects, routes, listener ports, and capture disk usage |
| `prox proxy status --json` | Output daemon status as JSON |
| `prox proxy routes` | List registered routes |
| `prox proxy routes --json` | Output registered routes as JSON |
| `prox proxy stop` | Stop the daemon when no active routes are registered |
| `prox proxy stop --force` | Stop the daemon even with active routes |

**Capture line:** human output prints a `Capture:` line reporting the daemon-wide capture disk budget (see [Request Capture](configuration.md#request-capture)) — `Capture:    <used> used / <budget> budget on disk`, or `Capture:    unavailable (<reason>)` when the daemon's own capture manager failed to initialize at startup (distinct from any project simply choosing capture off, which never shows up here). `--json` carries the same information as `capture_disk_used`/`capture_disk_budget` (bytes, both `0` when the daemon has no capture manager) and `capture_available`/`capture_error`.

**Examples:**

```bash
# Show daemon status and routes
prox proxy status

# List route ownership
prox proxy routes

# Stop only when no projects are registered
prox proxy stop

# Reset a stale daemon after upgrading prox
prox proxy stop --force
```

See the [Shared Proxy Across Projects](../guides/shared-proxy.md) guide for multi-project behavior and constraints.

### version

Show version information.

```bash
prox version
```

### help

Show help for any command.

```bash
prox help
prox help up
prox help logs
```

### completion

Generate shell completion scripts.

```bash
prox completion [bash|zsh|fish|powershell]
```
