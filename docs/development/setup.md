# Development Setup

## Prerequisites

- Go 1.24 or later
- mise (optional, for version management)

## Clone and Build

```bash
git clone https://github.com/charliek/prox.git
cd prox
make build
```

Or without make:

```bash
go build -o prox ./cmd/prox
```

If using mise:

```bash
mise install
make build
```

## Project Structure

```
prox/
├── cmd/
│   └── prox/
│       └── main.go           # CLI entrypoint
├── internal/
│   ├── api/                  # HTTP server and handlers
│   ├── cli/                  # CLI command definitions
│   ├── config/               # YAML parsing, validation
│   ├── constants/            # Shared constants (timeouts, buffer sizes, defaults)
│   ├── daemon/               # Background daemon lifecycle, PID file, state file
│   ├── domain/               # Core domain types and error definitions
│   ├── logs/                 # Log buffer, subscriptions
│   ├── proxy/                # Standalone proxy, request tracking, capture
│   ├── proxyd/               # Shared proxy daemon
│   ├── supervisor/           # Process orchestration
│   ├── tui/                  # Bubbletea TUI
│   └── version/              # Build/version metadata
├── docs/                     # Documentation
├── prox.yaml                 # Example config
├── go.mod
└── go.sum
```

## Running Tests

```bash
make test
```

Or without make:

```bash
go test -v ./...
```

## Linting

Install golangci-lint:

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

Run linter:

```bash
make lint
```

Or without make:

```bash
golangci-lint run
```

## Documentation

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
