# prox

A modern process manager for development with an API-first design, enabling both human interaction and LLM tooling integration.

## Features

- **Simple by default** - Procfile-like experience with minimal YAML configuration
- **API-first** - Full process control and log access via HTTP, always available
- **Interactive TUI** - Real-time log viewing with filtering and search
- **HTTPS Proxy** - Subdomain routing with locally-trusted certificates
- **Health checks** - Optional health monitoring for processes
- **LLM-friendly** - Structured output and filtering to support AI-assisted debugging

## Quick Example

Create a `prox.yaml`:

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

## Current Scope

prox is designed for local development. It intentionally does not include:

- Log persistence to disk
- Process dependencies or startup ordering
- Distributed or multi-machine operation

See the [Roadmap](https://github.com/charliek/prox/blob/main/ROADMAP.md) for planned features.

## Next Steps

- [Quick Start](getting-started/quick-start.md) - Installation and first run
- [CLI Reference](reference/cli.md) - All commands and flags
- [TUI Reference](reference/tui.md) - Interactive terminal UI
- [Configuration](reference/configuration.md) - Full `prox.yaml` reference
- [HTTP API](reference/api.md) - API endpoints for programmatic control
