# Phase 2: Process Management

## Status: COMPLETE

## Objective

Build the supervisor that manages process lifecycles, captures output, and handles graceful shutdown.

## Tasks

### Process Runner (`internal/supervisor/runner.go`)

- [x] ProcessRunner interface
- [x] Process interface (PID, Wait, Signal, Stdout, Stderr)
- [x] execRunner implementation using exec.Cmd
- [x] Stdout/stderr pipe capture
- [x] Mock runner for testing

### Process Lifecycle (`internal/supervisor/process.go`)

- [x] ManagedProcess struct
- [x] State machine (starting → running → stopping → stopped/crashed)
- [x] Start process with environment
- [x] Signal handling (SIGTERM → wait → SIGKILL)
- [x] Output routing to LogManager
- [x] Track restart count, PID, uptime

### Supervisor (`internal/supervisor/supervisor.go`)

- [x] Supervisor struct
- [x] Start all processes
- [x] Stop all processes (graceful shutdown)
- [x] Individual process control (start/stop/restart)
- [x] Context-aware for cancellation
- [x] Event subscription for state changes

### Log Subscriptions (`internal/logs/subscription.go`) (Phase 1)

- [x] Subscriber interface
- [x] Concurrent subscriber management
- [x] Non-blocking send with buffered channels
- [x] Pattern filtering (substring, regex)

## Key Interfaces

### ProcessRunner
```go
type ProcessRunner interface {
    Start(ctx context.Context, config ProcessConfig, env map[string]string) (Process, error)
}

type Process interface {
    PID() int
    Wait() error
    Signal(sig os.Signal) error
    Stdout() io.Reader
    Stderr() io.Reader
}
```

### Supervisor
```go
type Supervisor interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Processes() []ProcessInfo
    Process(name string) (ProcessInfo, error)
    StartProcess(ctx context.Context, name string) error
    StopProcess(ctx context.Context, name string) error
    RestartProcess(ctx context.Context, name string) error
    Status() SupervisorStatus
    Subscribe() <-chan SupervisorEvent
}
```

## State Machine

```
         start()
    ┌───────────────┐
    ▼               │
┌────────┐    ┌──────────┐    ┌─────────┐
│stopped │───▶│ starting │───▶│ running │
└────────┘    └──────────┘    └─────────┘
    ▲                              │
    │                              │ stop() or crash
    │         ┌──────────┐         │
    └─────────│ stopping │◀────────┘
              └──────────┘
                   │
                   │ timeout
                   ▼
              ┌─────────┐
              │ crashed │
              └─────────┘
```

## Graceful Shutdown Sequence

1. SIGTERM to all processes
2. Wait up to 10 seconds for each
3. SIGKILL any remaining
4. Collect exit statuses

## Test Scripts to Create

```
testdata/scripts/
├── echo_hello.sh        # Simple script that exits
├── long_running.sh      # Script that runs until killed
├── crash_after_1s.sh    # Script that crashes
└── ignore_sigterm.sh    # Script that ignores SIGTERM (for testing SIGKILL)
```

## Verification

```bash
go test ./internal/supervisor/... -v
go test -race ./internal/supervisor/...

# Manual verification
go run ./cmd/prox up  # Should start processes from prox.yaml
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [x] ProcessRunner interface implemented
- [x] Exec runner captures stdout/stderr
- [x] Process state machine works correctly
- [x] Supervisor starts all configured processes
- [x] Graceful shutdown sends SIGTERM then SIGKILL
- [x] Process restarts work
- [x] Logs are captured and written to LogManager
- [x] Subscriptions deliver log entries (Phase 1)
- [x] Pattern filtering works (substring and regex) (Phase 1)
- [x] All tests pass
