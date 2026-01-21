# Roadmap

Features planned for future versions of prox.

## Log Persistence

- Write logs to `~/.prox/logs/{project-hash}/`
- Configurable rotation (size, count)
- `prox logs` works after supervisor exits
- Auto-cleanup of old logs

## Global Config

- `~/.prox/config.yaml` for user defaults
- Default API port, log retention, etc.

## Daemon Mode

- `prox up -d` starts detached
- `prox attach` connects to running instance
- `prox attach --tui` attaches with TUI
- PID file management

## Process Groups

```yaml
groups:
  backend:
    - api
    - worker
  frontend:
    - web
```

- `prox restart backend` restarts group
- TUI shows groups

## Dependencies

```yaml
processes:
  api:
    cmd: go run ./cmd/server
    depends_on:
      - db
  db:
    cmd: docker compose up postgres
```

- Startup order based on dependencies
- Wait for health before starting dependents

## Instance Registry & Dynamic Ports

Track running prox instances and dynamically assign API ports.

**Storage**: `~/.local/state/prox/instances/`
- JSON files with project-hash naming: `{hash}.json`
- Hash derived from canonical project path (SHA256, first 12 chars)

**Instance State**:
```json
{
  "pid": 12345,
  "port": 5555,
  "host": "127.0.0.1",
  "projectPath": "/path/to/project",
  "startedAt": "2025-01-19T10:30:00Z",
  "configFile": "prox.yaml"
}
```

**Dynamic Port Allocation**:
- `port: 0` in config means auto-assign
- Scan port range (default 5550-5650) and test socket binding
- Store assigned port in instance state

**Features**:
- `prox list` - show all running instances across projects
- `prox status` - show status of current project's instance
- Auto-cleanup of stale state files (validate PID with signal 0)
- Graceful shutdown updates state before exit

**Inspiration**: codelens project's ServerStateRepository pattern
