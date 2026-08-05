# prox

A modern process manager for development with an API-first design.

## Features

- **Simple by default** - Procfile-like experience with minimal configuration
- **API-first** - Full process control and log access via HTTP
- **Interactive TUI** - Real-time log viewing with menu bar, themes, query filter bar, and mouse support
- **HTTP/HTTPS proxy** - Friendly local hostnames with shared multi-project port support
- **Health checks** - Optional health monitoring for processes

## Installation

### Homebrew (macOS)

```bash
brew install charliek/tap/prox
```

### Linux (apt)

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
  sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
  sudo tee /etc/apt/sources.list.d/apt-charliek.list
sudo apt update
sudo apt install prox
```

Tested on Pop!_OS 24.04 and Ubuntu 24.04+. Architectures: `amd64`, `arm64`. See [apt-charliek](https://github.com/charliek/apt-charliek) for the full repo.

### Linux (`.deb` download, no apt repo)

For one-off installs without configuring the apt repo (CI runners, locked-down hosts, etc.):

```bash
ARCH=$(dpkg --print-architecture)        # amd64 or arm64
VERSION=0.2.0                            # check https://github.com/charliek/prox/releases for the latest
curl -fLO "https://github.com/charliek/prox/releases/download/v${VERSION}/prox_${VERSION}_${ARCH}.deb"
sudo apt install -y "./prox_${VERSION}_${ARCH}.deb"
```

The `apt install ./...deb` form resolves dependencies; plain `dpkg -i` would skip that step.

### Other Methods

```bash
# Install via Go
go install github.com/charliek/prox/cmd/prox@latest

# Or build from source
git clone https://github.com/charliek/prox.git
cd prox
make build
```

### Claude Code / agent skills

prox ships a skill that teaches your coding agent to drive the prox CLI (read `prox.yaml`, start/stop processes, tail logs, reach services through the proxy, inspect requests). Install the CLI (above) first, since the skill drives it.

The general route ([`skills`](https://skills.sh)) installs into Claude Code, GitHub Copilot, OpenCode, and other agents:

```bash
npx skills add charliek/prox
```

For Claude Code, a native plugin is also available (it namespaces the skill as `prox:prox`):

```text
/plugin marketplace add charliek/prox
/plugin install prox@prox
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
# api: { port: 5555 }   # optional: pin the API port; omit for a dynamic one

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
prox restart <process>           # Restart a process (re-reads prox.yaml, applies its current config)
prox status                      # Show process status
prox logs [process]              # Show recent logs
prox logs -f [process]           # Stream logs
prox requests                     # Show recent proxy requests
prox proxy routes                 # Show shared proxy route ownership
```

## Proxy

Configure `proxy` and `services` to expose local processes through HTTP or HTTPS hostnames. When more than one project uses the same proxy port, prox automatically uses a shared per-user daemon so each project can own distinct hostnames on one port.

```yaml
proxy:
  enabled: true
  https_port: 443
  domain: local.example.dev

services:
  app: 3000
  api: 8000
```

With another project using the same `https_port`, both can participate on `443` as long as the service hostnames differ. Inspect ownership with:

```bash
prox proxy routes
```

## HTTP API

By default, the API binds to a dynamic (auto-assigned) localhost port. Discover the address with `prox status`, or pin a fixed port by setting `api.port` in `prox.yaml` (e.g. `api.port: 5555`).

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
| `/proxy/requests` | GET | List proxied requests |
| `/proxy/requests/{id}` | GET | Get proxied request details |
| `/proxy/requests/stream` | GET | Stream proxied requests (SSE) |
| `/shutdown` | POST | Shutdown supervisor |

## Security

Configuration files are executed as code (via shell). Only use configuration from trusted sources, similar to Makefiles or Procfiles.

When binding to non-localhost interfaces, authentication is automatically enabled. A bearer token is generated and stored in `~/.prox/token`.

## Documentation

Full documentation is available at [charliek.github.io/prox](https://charliek.github.io/prox/).

Local docs live under [docs](docs/), including the [CLI reference](docs/reference/cli.md), [configuration reference](docs/reference/configuration.md), [HTTP API reference](docs/reference/api.md), and [architecture notes](docs/development/architecture.md).

## Development

### Prerequisites

This project uses [mise](https://mise.jdx.dev/) to manage tool versions. With mise installed, all dependencies are set up automatically:

```bash
mise install
```

This installs the correct versions of Go and golangci-lint as defined in `.mise.toml`.

Alternatively, install manually:
- Go 1.24+
- golangci-lint v2 (`brew install golangci-lint` on macOS, or see [install docs](https://golangci-lint.run/docs/welcome/install/))

```bash
make build    # Build the binary
make test     # Run tests
make lint     # Run linters
make clean    # Remove build artifacts
```

## Documentation Development

The documentation site is built with [Zensical](https://zensical.org),
configured in `zensical.toml`.

```bash
# Install dependencies (--locked pins the exact theme commit from uv.lock)
uv sync --locked --group docs

# Local preview (http://127.0.0.1:7070)
uv run --locked zensical serve

# Build static site (--strict fails on broken links and anchors)
uv run --locked zensical build --strict
```

Documentation is automatically published to GitHub Pages on push to main.

## License

MIT
