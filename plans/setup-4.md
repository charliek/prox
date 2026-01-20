# Phase 4: CLI Commands

## Status: COMPLETE

## Objective

Build the CLI interface with all commands, supporting both local supervisor mode and remote API calls.

## Tasks

### CLI Framework (`internal/cli/root.go`)

- [ ] Root command setup
- [ ] Global flags (--config, --json)
- [ ] Version command
- [ ] Help command

### Up Command (`internal/cli/up.go`)

- [ ] Start supervisor
- [ ] Start API server
- [ ] Process selection (prox up web api)
- [ ] Terminal log subscriber (non-TUI mode)
- [ ] --tui flag (placeholder for Phase 6)
- [ ] --port flag override

### HTTP Client (`internal/cli/client.go`)

- [ ] Client for API calls
- [ ] Error handling and display
- [ ] JSON output support

### Status Command (`internal/cli/status.go`)

- [ ] GET /processes via API
- [ ] Pretty table output
- [ ] --json flag

### Logs Command (`internal/cli/logs.go`)

- [ ] GET /logs via API
- [ ] -f/--follow for streaming
- [ ] --process filter
- [ ] --pattern filter
- [ ] --regex flag
- [ ] --lines/-n flag
- [ ] --json output

### Control Commands (`internal/cli/control.go`)

- [ ] prox stop - POST /shutdown
- [ ] prox restart <process> - POST /processes/{name}/restart

## Commands Reference

```
prox up [processes...]           Start processes (foreground)
prox up --tui [processes...]     Start with TUI (Phase 6)
prox stop                        Stop via API
prox restart <process>           Restart process via API
prox status                      Show status
prox status --json               JSON output
prox logs [process]              Show recent logs
prox logs -f [process]           Stream logs
prox version                     Show version
prox help                        Show help
```

## Flags

| Flag | Commands | Description |
|------|----------|-------------|
| --tui | up | Enable TUI (Phase 6) |
| --config, -c | all | Config file path |
| --port, -p | up | Override API port |
| --lines, -n | logs | Number of lines |
| -f, --follow | logs | Stream continuously |
| --json | status, logs | JSON output |
| --process | logs | Filter by process |
| --pattern | logs | Filter by pattern |
| --regex | logs | Pattern is regex |

## Terminal Output (Non-TUI)

```
10:32:01 web    │ GET /api/users 200 12ms
10:32:01 api    │ connected to database
10:32:02 worker │ processing job 123
```

- Color-coded process names
- Timestamp prefix
- Pipe separator
- Wrapping for long lines

## Verification

```bash
go build -o prox ./cmd/prox

# Test up command
./prox up

# Test with process selection
./prox up web api

# In another terminal:
./prox status
./prox status --json
./prox logs --lines 20
./prox logs -f
./prox logs --process web --pattern ERROR
./prox restart web
./prox stop
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [ ] Root command with global flags
- [ ] Up command starts supervisor + API
- [ ] Terminal subscriber outputs colored logs
- [ ] Status command shows process table
- [ ] Logs command with all filter options
- [ ] Follow mode streams via SSE
- [ ] Stop command shuts down cleanly
- [ ] Restart command works
- [ ] JSON output works for status and logs
- [ ] Version command outputs version
- [ ] Help is comprehensive
