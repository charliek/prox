# prox

A modern process manager for development with an API-first design.

## Features

- **Simple by default** - Procfile-like experience with minimal configuration
- **API-first** - Full process control and log access via HTTP
- **Interactive TUI** - Real-time log viewing with filtering and search
- **Health checks** - Optional health monitoring for processes

## Installation

```bash
go install github.com/charliek/prox/cmd/prox@latest
```

Or build from source:

```bash
git clone https://github.com/charliek/prox.git
cd prox
go build -o prox ./cmd/prox
```

## Quick Start

Create a `prox.yaml` in your project directory:

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

Start all processes:

```bash
prox up
```

Start with the interactive TUI:

```bash
prox up --tui
```

## Configuration

### Simple Form

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
```

### Expanded Form

```yaml
api:
  port: 5555
  host: 127.0.0.1

env_file: .env

processes:
  web: npm run dev

  api:
    cmd: go run ./cmd/server
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

## CLI Commands

```bash
prox up [processes...]           # Start processes (foreground)
prox up --tui [processes...]     # Start with interactive TUI
prox stop                        # Stop running instance
prox restart <process>           # Restart a process
prox status                      # Show process status
prox logs [process]              # Show recent logs
prox logs -f [process]           # Stream logs
```

## HTTP API

The API runs at `http://127.0.0.1:5555/api/v1` by default.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/status` | GET | Supervisor status |
| `/processes` | GET | List all processes |
| `/processes/{name}` | GET | Get process details |
| `/processes/{name}/start` | POST | Start a process |
| `/processes/{name}/stop` | POST | Stop a process |
| `/processes/{name}/restart` | POST | Restart a process |
| `/logs` | GET | Retrieve logs |
| `/logs/stream` | GET | Stream logs (SSE) |
| `/shutdown` | POST | Shutdown supervisor |

## Security

Configuration files are executed as code (via shell). Only use configuration from trusted sources, similar to Makefiles or Procfiles.

When binding to non-localhost interfaces, authentication is automatically enabled. A bearer token is generated and stored in `~/.config/prox/`.

## Documentation

See [docs/spec.md](docs/spec.md) for the complete specification including TUI keybindings, API details, and architecture.

## License

MIT
