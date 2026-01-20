# Prox Implementation Epic

A modern process manager for development with an API-first design.

## Status: COMPLETE

| Phase | Name | Status | Notes |
|-------|------|--------|-------|
| 0 | Project Setup | COMPLETE | go.mod, dependencies, directory structure |
| 1 | Foundation | COMPLETE | domain types, config, logs |
| 2 | Process Management | COMPLETE | supervisor, process runner |
| 3 | HTTP API | COMPLETE | REST endpoints, SSE streaming |
| 4 | CLI Commands | COMPLETE | up, status, logs, stop, restart |
| 5 | Health Checks | COMPLETE | health check runner, integration |
| 6 | TUI | COMPLETE | bubbletea interface |
| 7 | Polish & Testing | COMPLETE | integration tests, go vet |

## Architecture Overview

```
prox/
├── cmd/prox/main.go                 # Entry point
├── internal/
│   ├── domain/                      # Core types (no dependencies)
│   ├── config/                      # YAML parsing, validation
│   ├── logs/                        # Ring buffer, subscriptions
│   ├── supervisor/                  # Process lifecycle management
│   ├── api/                         # HTTP server, handlers, SSE
│   ├── tui/                         # Bubbletea TUI
│   └── cli/                         # Command definitions
├── plans/                           # Implementation tracking
└── testdata/                        # Test fixtures
```

## Dependency Flow

```
              cmd/prox
                 │
                 ▼
               cli
                 │
    ┌────────────┼────────────┐
    ▼            ▼            ▼
   api          tui        client
    │            │
    └─────┬──────┘
          ▼
      supervisor
          │
    ┌─────┼─────┐
    ▼     ▼     ▼
  logs  runner health
    │     │     │
    └─────┼─────┘
          ▼
       domain
          │
          ▼
       config
```

## Key Design Decisions

1. **Interfaces for testability** - LogManager, Supervisor, ProcessRunner
2. **Ring buffer logs** - Fixed size, concurrent-safe, subscriber-based
3. **Process state machine** - starting → running → stopping → stopped/crashed
4. **API always available** - Even without TUI
5. **Chi router** - Lightweight, idiomatic Go

## Deviations & Notable Items

_Document any changes from the original spec here during implementation._

| Date | Phase | Item | Description |
|------|-------|------|-------------|
| | | | |

## Dependencies

```go
require (
    github.com/charmbracelet/bubbletea v1.x
    github.com/charmbracelet/lipgloss v1.x
    github.com/charmbracelet/bubbles v0.x
    github.com/go-chi/chi/v5 v5.x
    gopkg.in/yaml.v3 v3.x
    github.com/joho/godotenv v1.x
    github.com/stretchr/testify v1.x
)
```

## Running the Project

```bash
# After Phase 2: Basic process management
go run ./cmd/prox up

# After Phase 4: Full CLI
prox up --tui web api

# After Phase 6: Full TUI
prox up --tui
```

## Test Coverage Targets

| Package | Target |
|---------|--------|
| domain | 100% |
| config | 95% |
| logs | 95% |
| supervisor | 90% |
| api | 90% |
| tui | 80% |
| cli | 85% |
