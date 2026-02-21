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

### requests

Show or stream proxy requests.

```bash
prox requests [options]
```

| Flag | Description |
|------|-------------|
| `-f, --follow` | Stream requests continuously |
| `-n, --limit` | Number of requests to show (default: 100) |
| `--subdomain` | Filter by subdomain |
| `--method` | Filter by HTTP method (GET, POST, etc.) |
| `--min-status` | Filter by minimum status code (e.g., 400 for errors) |
| `--json` | Output as JSON |

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

# JSON output for piping
prox requests --json | jq .
```

**Request IDs:**

Each request is assigned a short hash ID (7 characters, git-style). These IDs are displayed in the output and can be used to reference specific requests.

### version

Show version information.

```bash
prox version
```

### certs

Manage HTTPS certificates for the proxy.

```bash
prox certs [options]
```

| Flag | Description |
|------|-------------|
| `--regenerate` | Force regenerate certificates |

**Examples:**

```bash
# Show certificate status
prox certs

# Regenerate certificates
prox certs --regenerate
```

### hosts

Manage /etc/hosts entries for proxy subdomains.

```bash
prox hosts [options]
```

| Flag | Description |
|------|-------------|
| `--show` | Show entries that would be added (default) |
| `--add` | Add entries to /etc/hosts (requires sudo) |
| `--remove` | Remove entries from /etc/hosts (requires sudo) |

**Examples:**

```bash
# Show required entries
prox hosts --show

# Add entries (requires sudo)
prox hosts --add

# Remove entries
prox hosts --remove
```

### help

Show help for any command.

```bash
prox help
prox help up
prox help logs
```
