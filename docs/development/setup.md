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
│   ├── config/               # YAML parsing, validation
│   ├── supervisor/           # Process orchestration
│   ├── logs/                 # Log buffer, subscriptions
│   ├── api/                  # HTTP server and handlers
│   ├── tui/                  # Bubbletea TUI
│   └── cli/                  # CLI command definitions
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

The documentation site uses MkDocs with Material theme.

```bash
# Install dependencies
uv sync --group docs

# Local preview (http://127.0.0.1:7070)
uv run mkdocs serve

# Build static site
uv run mkdocs build
```
