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

**Shipped** (#76, plan 013): external-resource dependencies, one-shot tasks,
and process gating.

```yaml
dependencies:
  postgres:
    check:
      tcp: localhost:5432
    start: docker compose up -d postgres
    on_failure: fail

tasks:
  migrate:
    cmd: ./scripts/migrate.sh
    depends_on: [postgres]

processes:
  api:
    cmd: go run ./cmd/server
    depends_on: [postgres, migrate]
```

See the [Dependencies, Tasks, and Process Gating guide](docs/guides/dependencies.md)
and the [configuration reference](docs/reference/configuration.md#dependencies-tasks-and-process-gating).

**Not shipped, still future:** the original sketch on this page was
process→process ordering — one `processes:` entry launching only after
another process (rather than a dependency or task) is ready — plus ordering
across separate prox projects. Neither exists today. If real demand shows up
for expressing "start these processes as a group, in order" this will most
likely fold into a `groups:`-shaped feature (see Process Groups above) rather
than reopening `depends_on` to processes.

