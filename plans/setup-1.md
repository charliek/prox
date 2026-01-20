# Phase 1: Foundation

## Status: COMPLETE

## Objective

Build the core domain types, configuration parsing, and log management that all other components depend on.

## Tasks

### Domain Types (`internal/domain/`)

- [x] `process.go` - ProcessState, ProcessConfig, ProcessInfo
- [x] `log.go` - LogEntry, LogFilter, Stream enum
- [x] `health.go` - HealthConfig, HealthStatus, HealthState
- [x] `errors.go` - Domain-specific error types
- [x] Tests for all domain types

### Configuration (`internal/config/`)

- [x] `config.go` - Config struct, top-level parsing
- [x] `loader.go` - File loading, env file handling with godotenv
- [x] `validate.go` - Validation rules
- [x] Support simple form: `web: npm run dev`
- [x] Support expanded form with cmd, env, env_file, healthcheck
- [x] Tests with fixture files

### Log Manager (`internal/logs/`)

- [x] `buffer.go` - Ring buffer implementation
- [x] `manager.go` - LogManager implementation
- [x] `filter.go` - Filtering logic (by process, pattern)
- [x] `subscription.go` - Subscriber management (added)
- [x] Write/Query operations
- [x] Tests including concurrency tests

## Key Types

### ProcessState
```go
type ProcessState string

const (
    ProcessStateRunning  ProcessState = "running"
    ProcessStateStopped  ProcessState = "stopped"
    ProcessStateStarting ProcessState = "starting"
    ProcessStateStopping ProcessState = "stopping"
    ProcessStateCrashed  ProcessState = "crashed"
)
```

### ProcessConfig
```go
type ProcessConfig struct {
    Name        string
    Cmd         string
    Env         map[string]string
    EnvFile     string
    Healthcheck *HealthConfig
}
```

### LogEntry
```go
type LogEntry struct {
    Timestamp time.Time
    Process   string
    Stream    Stream
    Line      string
}
```

### LogManager Interface
```go
type LogManager interface {
    Write(entry LogEntry)
    Query(filter LogFilter, limit int) ([]LogEntry, int)
    Subscribe(filter LogFilter) (string, <-chan LogEntry)
    Unsubscribe(id string)
}
```

## Test Fixtures to Create

```
testdata/configs/
├── simple.yaml          # Basic process definitions
├── expanded.yaml        # Full config with healthchecks
├── env_file.yaml        # Config with env_file references
├── invalid_no_cmd.yaml  # Missing cmd validation
└── invalid_port.yaml    # Invalid port validation
```

## Verification

```bash
go test ./internal/domain/... -v
go test ./internal/config/... -v
go test ./internal/logs/... -v
go test -race ./internal/logs/...  # Concurrency test
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [x] All domain types implemented
- [x] Config parsing works for simple and expanded forms
- [x] Env file loading works
- [x] Config validation catches errors
- [x] Ring buffer handles overflow correctly
- [x] LogManager query filtering works
- [x] All tests pass
- [x] Race detector passes on logs package
