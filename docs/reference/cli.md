# CLI Reference

## Usage

```
prox <command> [options]
```

## Global Options

| Flag | Description |
|------|-------------|
| `--config, -c` | Config file path (default: `prox.yaml`) |
| `--addr` | API address for client commands (default: `127.0.0.1:5555`) |

## Commands

### up

Start processes in the foreground.

```bash
prox up [processes...]
```

| Flag | Description |
|------|-------------|
| `--tui` | Enable interactive TUI mode |
| `--port, -p` | Override API port |

**Examples:**

```bash
# Start all processes
prox up

# Start specific processes
prox up web api

# Start with TUI
prox up --tui

# Start specific processes with TUI
prox up --tui web api

# Override API port
prox up --port 6000
```

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
| `--process` | Filter by process name(s), comma-separated |
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

# Stream logs from web and api
prox logs -f --process web,api

# Filter for errors
prox logs --pattern ERROR

# Regex filter
prox logs --pattern "GET|POST" --regex

# JSON output for piping
prox logs -f --json | jq .
```

### stop

Stop the running prox instance.

```bash
prox stop
```

Sends a shutdown signal via the API. All processes receive SIGTERM, then SIGKILL after a timeout.

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
