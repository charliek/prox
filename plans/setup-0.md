# Phase 0: Project Setup

## Status: COMPLETE

## Objective

Initialize the Go project with proper module structure, dependencies, and directory layout.

## Tasks

- [x] Initialize go.mod with module path `github.com/charliek/prox`
- [x] Create directory structure
- [x] Add all required dependencies
- [x] Create initial main.go entry point
- [x] Verify build works

## Directory Structure to Create

```
prox/
├── cmd/prox/
│   └── main.go
├── internal/
│   ├── domain/
│   ├── config/
│   ├── logs/
│   ├── supervisor/
│   ├── api/
│   ├── tui/
│   └── cli/
├── testdata/
│   ├── configs/
│   └── scripts/
├── plans/
├── go.mod
└── go.sum
```

## Dependencies to Install

| Package | Purpose |
|---------|---------|
| github.com/charmbracelet/bubbletea | TUI framework |
| github.com/charmbracelet/lipgloss | TUI styling |
| github.com/charmbracelet/bubbles | TUI components |
| github.com/go-chi/chi/v5 | HTTP router |
| gopkg.in/yaml.v3 | YAML parsing |
| github.com/joho/godotenv | .env file loading |
| github.com/stretchr/testify | Test assertions |

## Commands

```bash
go mod init github.com/charliek/prox
go get github.com/charmbracelet/bubbletea@latest
go get github.com/charmbracelet/lipgloss@latest
go get github.com/charmbracelet/bubbles@latest
go get github.com/go-chi/chi/v5@latest
go get gopkg.in/yaml.v3@latest
go get github.com/joho/godotenv@latest
go get github.com/stretchr/testify@latest
```

## Verification

```bash
go build ./...
go vet ./...
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [x] go.mod created
- [x] All directories exist
- [x] Dependencies installed
- [x] main.go compiles
- [x] `go build ./...` succeeds
