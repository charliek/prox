# Changelog

All notable changes to this project will be documented in this file.

## v0.0.2

### Features

- Add `prox requests` command to view and stream proxy HTTP requests
  - Filter by subdomain, HTTP method, and minimum status code
  - Stream in real-time with `-f/--follow` flag
  - JSON output support with `--json` flag
- Add `prox start <process>` command to start stopped processes
- Add `prox stop <process>` command to stop individual processes

### Improvements

- Add TTY detection to LogPrinter for clean output when piping
- Add `setcap` to install target for privileged port binding
- Case-insensitive HTTP method filtering (`--method get` works)

## v0.0.1

Initial release of prox, a modern process manager for local development.

### Features

- Process supervision with automatic restarts and health checks
- Real-time log aggregation with filtering and search
- HTTPS reverse proxy with subdomain routing
- Interactive TUI for monitoring processes and logs
- Background daemon mode with `--detach` flag
- CLI built with Cobra framework with shell completions
- REST API for programmatic control
