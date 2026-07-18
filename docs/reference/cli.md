# CLI Reference

## Usage

```
prox <command> [options]
```

## Global Options

| Flag | Description |
|------|-------------|
| `--config, -c` | Config file path (default: `prox.yaml`) |
| `--addr` | API address for client commands (auto-discovered from `.prox/prox.state`) |
| `--detach, -d` | Run in background (daemon mode) |
| `--verbose, -v` | Enable verbose output |
| `--version` | Show version information |

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
| `--capture` | Enable request/response body capture for proxied requests |

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

# Enable request/response body capture
prox up --capture
```

**Dynamic Port Allocation:**

When no port is specified (via `--api-port` or `api.port` in config), prox automatically finds an available port. The port is stored in `.prox/prox.state` and auto-discovered by CLI commands.

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

**Full stop waits for the outcome.** `prox stop` (no arguments) and `prox down` block until the daemon reports the process-stop verdict, then wait briefly (up to ~15s) for the daemon's state and PID files to disappear before printing a stopped summary and exiting **0**. Exit codes:

- **0** — all processes (and their process groups) stopped cleanly. A summary line is printed. If the daemon is still finishing its own exit when the bounded wait elapses, a `Warning: the daemon is still finishing shutdown` line is printed to stderr but the exit code stays `0` (the process-stop verdict already succeeded).
- **1** — one or more process groups survived shutdown. Each survivor is printed as a `process: error` line, followed by a one-line summary; the group still holds whatever ports it bound.
- **1** — the connection dropped mid-wait: the daemon may still be completing its shutdown and the outcome is unknown. When a daemon log file is present (detached mode), the message points at `.prox/prox.log` for the authoritative result.

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

Body output requires request capture to be enabled with `prox up --capture` or `proxy.capture.enabled: true`. Capture applies in both standalone and shared-daemon mode — the enablement is propagated to the daemon at registration, and a daemon captures bodies only for the projects that opted in. When a request has no captured detail, `prox requests <id>` shows `(capture not enabled - use 'prox up --capture' to enable)`; if the body was captured but has since been evicted from the request buffer, it shows `(body no longer available)` instead. See [Request Capture](configuration.md#request-capture) for capture directories and daemon-upgrade notes.

### proxy

Inspect and control the shared proxy daemon.

The proxy daemon is normally started and stopped automatically by `prox up` and `prox down`. These commands are for debugging route ownership, checking shared ports, and resetting a stale daemon.

```bash
prox proxy <command>
```

| Command | Description |
|---------|-------------|
| `prox proxy status` | Show daemon version, PID, uptime, projects, routes, and listener ports |
| `prox proxy status --json` | Output daemon status as JSON |
| `prox proxy routes` | List registered routes |
| `prox proxy routes --json` | Output registered routes as JSON |
| `prox proxy stop` | Stop the daemon when no active routes are registered |
| `prox proxy stop --force` | Stop the daemon even with active routes |

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
