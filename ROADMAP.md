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
