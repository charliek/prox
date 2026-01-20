# Development Setup

## Prerequisites

- Go 1.23 or later
- mise (optional, for version management)

## Clone and Build

```bash
git clone https://github.com/charliek/prox.git
cd prox
go build -o prox ./cmd/prox
```

If using mise:

```bash
mise install
go build -o prox ./cmd/prox
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
go test ./...
```

With verbose output:

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
