# Quick Start

## Installation

Install with Go:

```bash
go install github.com/charliek/prox/cmd/prox@latest
```

Or build from source:

```bash
git clone https://github.com/charliek/prox.git
cd prox
go build -o prox ./cmd/prox
```

## Create Configuration

Create a `prox.yaml` in your project directory:

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

## Start Processes

Start all processes:

```bash
prox up
```

You'll see aggregated logs from all processes with color-coded prefixes.

## Check Status

In another terminal, check process status:

```bash
prox status
```

Output:

```
NAME     STATUS    PID    UPTIME     RESTARTS  HEALTH
web      running   12345  5m30s      0         unknown
api      running   12346  5m30s      0         healthy
worker   running   12347  5m30s      1         unknown
```

## View Logs

View recent logs:

```bash
prox logs
```

Stream logs continuously:

```bash
prox logs -f
```

Filter logs by process:

```bash
prox logs --process api
```

## Interactive TUI

Start with the interactive terminal UI:

```bash
prox up --tui
```

Note: The `--tui` flag works in foreground mode only and is mutually exclusive with `--detach`. For background + TUI workflow, use `prox up -d` then `prox attach`.

The TUI provides:

- Real-time log viewing with scrollback
- Process filtering with number keys (1-9)
- Search with `/` and filter with `s`
- Process restart with `r`
- Press `?` for help, `q` to quit

## Background Mode

Run prox as a background daemon:

```bash
# Start in background
prox up -d

# Check status
prox status

# View logs
prox logs -f

# Attach TUI to running daemon
prox attach

# Stop the daemon
prox down
```

Background mode features:

- Processes continue running after terminal closes
- Multiple prox instances can run (different projects, different ports)
- CLI commands auto-discover the running daemon
- Daemon logs are written to `.prox/prox.log`

## HTTP API

The API runs at `http://127.0.0.1:5555/api/v1` by default.

Check supervisor status:

```bash
curl http://localhost:5555/api/v1/status
```

List processes:

```bash
curl http://localhost:5555/api/v1/processes
```

See the [API Reference](../reference/api.md) for all endpoints.
