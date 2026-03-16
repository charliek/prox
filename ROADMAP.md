# Roadmap

Features planned for future versions of prox.

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

