# CLI Reference

## Usage

```
prox <command> [options]
```

## Global Options

| Flag | Description |
|------|-------------|
| `--config, -c` | Config file path (default: `prox.yaml`) |
| `--addr` | API address for client commands. Used only when passed explicitly (which also skips the project-ownership check); otherwise auto-discovered from `.prox/prox.state` |
| `--detach, -d` | Run in background (daemon mode) |
| `--verbose, -v` | Enable verbose output |
| `--version` | Show version information |

**No implicit `:5555` fallback.** Client commands (`status`, `logs`, `stop`, `start`, `restart`, `down`, `attach`, `requests`) discover the running instance from `.prox/prox.state` in the current directory — the single discovery source. A pinned `api.port` in `prox.yaml` is **not** a fallback: `prox up` always writes the state file before it binds the API listener, so a running instance always has one. If no state file is found and `--addr` was not passed explicitly, the command errors instead of silently dialing the compiled-in `127.0.0.1:5555` default — run the command from the project directory, or pass `--addr host:port`. `version`, `up`, `proxy`, and `completion` are unaffected (they never need discovery).

**Ownership check.** Before acting on a discovered address, client commands probe `GET /api/v1/status` (5s budget) and confirm the responding daemon's project directory is the directory you are standing in (compared by device+inode, so symlinked roots and `/tmp` vs `/private/tmp` match). Without this, a stale `.prox/prox.state` — or two projects pinning the same `api.port` — let `prox status` report another project's processes and `prox down` stop another project's daemon. If another project owns the port, prox refuses and names it:

```
Error: prox is not running for this project.
A prox for /Users/you/projects/other is listening on 127.0.0.1:5552.
Run commands from that directory, or target it deliberately with
  --addr http://127.0.0.1:5552
```

An explicit `--addr` skips both discovery and this check — it is the deliberate "I know what I'm targeting" escape hatch. A daemon too old to report its project directory is compared by config path instead, and one reporting neither identity is allowed through with a warning rather than locking you out mid-upgrade.

## Commands

### up

Start processes. By default runs in the foreground, where it opens the [interactive TUI](tui.md) if the terminal can host one and streams plain logs otherwise; use `--detach` for background/daemon mode.

```bash
prox up [processes...]
```

| Flag | Description |
|------|-------------|
| `--detach, -d` | Run in background (daemon mode) |
| `--tui` | Require the interactive TUI, and fail if the terminal cannot host one. The TUI is already the default in the foreground, so this is the explicit "I insist" form (foreground only, mutually exclusive with `--detach` — see below) |
| `--no-tui` | Never open the interactive TUI for this run; stream plain logs instead. Overrides `PROX_TUI`; mutually exclusive with `--tui` |
| `--api-port, -p` | Override API server port (otherwise dynamic) |
| `--http-port` | Override proxy HTTP port |
| `--https-port` | Override proxy HTTPS port |
| `--no-proxy` | Disable proxy even if configured |
| `--capture` | Force request/response body capture on for this run. Capture is already on by default whenever the proxy is enabled, so this flag is kept for explicitness/compatibility; mutually exclusive with `--no-capture` |
| `--no-capture` | Disable request/response body capture for this run (config-level opt-out: `proxy.capture.enabled: false`) |

**Examples:**

```bash
# Start all processes (foreground: the TUI in a terminal, plain logs under a pipe)
prox up

# Same, for specific processes
prox up web api

# Stream plain logs on a terminal too
prox up --no-tui

# Start in background (daemon mode), then watch it whenever you like
prox up -d
prox attach

# Require the TUI: fail rather than fall back if the terminal cannot host one
prox up --tui

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

**TUI mode resolution.** Whether `prox up` opens the interactive TUI is decided by, in order of precedence: a flag that asserts something, then the `PROX_TUI` environment variable, then whether the terminal can host it. A bare foreground `prox up` **prefers the TUI**: it opens on a terminal that can host one and falls back to plain log streaming everywhere else, without an error.

| Source | Effect |
|--------|--------|
| `--tui` | Open the TUI, and **fail** if the terminal cannot host one — an explicit request is an assertion |
| `--no-tui`, `--tui=false` | Never open the TUI |
| `--no-tui=false` | Asserts nothing (it is the flag's own default spelled out loud) — falls through to `PROX_TUI` and the default below |
| `PROX_TUI=1` (also `true`, `yes`, `on`) | Prefer the TUI (the default anyway) |
| `PROX_TUI=0` (also `false`, `no`, `off`) | Never open the TUI |
| neither | Prefer the TUI. Falls back to plain log streaming — silently, without an error — when the terminal cannot host one |

Quitting the TUI with `q` **stops your processes**, exactly as Ctrl-C does: the foreground `prox up` is their supervisor, so nothing outlives it. Use `prox up -d` and [`prox attach`](#attach) for a TUI you can walk away from.

If the TUI is preferred rather than required and it fails to start, `prox up` says so and degrades to plain log streaming rather than failing a command that never asked for a TUI. The fallback replays what the log buffer still holds — the most recent 1000 entries — so a quiet startup is recovered in full and a noisy one is not: entries the ring has already evicted are gone. (The replay also repeats the startup lines already printed above the TUI, so those appear twice on this path.) With an explicit `--tui` the same failure exits non-zero instead.

`PROX_TUI` values are matched case-insensitively and trimmed of surrounding whitespace; any other value (including an empty one) is reported as a warning and ignored. A flag that asserts something — `--tui`, `--tui=false`, `--no-tui` — wins over `PROX_TUI` outright: the variable is then not consulted at all, so a bad value next to such a flag produces no warning. `--no-tui=false` is the exception, because it asserts nothing: it falls through to `PROX_TUI` exactly as an absent flag does. `--detach` short-circuits the whole decision: a daemon has no terminal, so `PROX_TUI` is not read and `prox up -d` is unaffected by it.

**A terminal can host the TUI when** stdin *and* stdout are both terminals, `TERM` is set to something other than `dumb`, and the process is in the terminal's foreground process group (so a backgrounded `prox up &` is excluded — reading the keyboard from a background job raises `SIGTTIN` and stops it). `--tui` reports which of the three conditions failed.

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

**Unknown process names are an error (breaking change).** The positional argument and `--process` are validated against the running daemon's process list before the request is sent, in both normal and `--follow` mode. A name the daemon does not have now exits non-zero with `unknown process "<name>"` plus either the closest real name (`Did you mean "web"?`) or the full list of valid names — where a typo previously produced an empty result set, no output, and exit `0`, which was indistinguishable from a process that had simply logged nothing. A name that *does* match behaves exactly as before, including printing nothing (and exiting `0`) for a process with no log entries: only a wrong **name** is an error. If the daemon cannot be asked for its process list, the check is skipped and the command proceeds as it always did.

### start

Start a stopped process.

```bash
prox start <process>
```

Like `restart`, `start` re-reads `prox.yaml` and launches the process with its **current** config (see [Config reload on (re)start](#config-reload-on-restart) below). Editing a process's `cmd` and running `prox stop <process>` + `prox start <process>` applies the change, just as `prox restart <process>` does.

**Unknown process names.** `start`, `stop`, and `restart` name the process that was asked for and either the closest real name (`Did you mean "web"?`) or the full list, instead of the bare `PROCESS_NOT_FOUND: process not found` earlier versions printed. The valid names come from the running daemon, so they are the names it will actually accept.

**The "Is prox running?" hint.** Every client command appends `Is prox running? Try 'prox up' first.` only when the failure positively says nothing is listening (a refused or reset connection). An error the daemon itself produced — any HTTP status, or an unparseable response body — no longer carries it: prox had just answered, so the advice was false. Timeouts, cancellations, and unrecognized failures do not carry it either, since none of them establishes that the daemon is gone.

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

Attach the TUI to a running daemon. Opens an interactive terminal UI connected via the API — the same TUI a foreground `prox up` shows, so `attach` is for the daemons you started with `-d`.

```bash
prox attach
```

**Examples:**

```bash
# Start daemon
prox up -d

# Attach TUI to running daemon
prox attach

# TUI operations work the same as they do under `prox up`
# Press q to detach (daemon continues running, unlike q under `prox up`)
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

Body output works out of the box: capture is on by default whenever the proxy is enabled, so `--body` just works without any config. Headers, query params, and bodies (up to `max_body_size`) are all captured verbatim, with no values altered or hidden — see [Request Capture](configuration.md#request-capture) for the full cleartext posture and the private file permissions that back it. Capture applies in both standalone and shared-daemon mode — the effective config is propagated to the daemon at registration, and a daemon captures bodies only for the projects that have not opted out.

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
