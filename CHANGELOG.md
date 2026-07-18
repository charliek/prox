# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Breaking

- **Foreground `prox up` now exits non-zero when a process group survives
  shutdown** (#36). Previously foreground `prox up` (Ctrl-C or an API shutdown)
  always exited `0`, even if a process group could not be reaped and still held
  its ports. It now exits `1` with a one-line `shutdown incomplete: …` summary
  (per-survivor detail is written to the log stream). Scripts that asserted a `0`
  exit from a foreground `prox up` regardless of outcome must be updated. A clean
  shutdown still exits `0`.

### Features

- **Full-stop failure contract** (#36). `prox stop` (no arguments) and `prox down`
  now **wait for the shutdown outcome** instead of firing and forgetting. The
  daemon reports the process-stop verdict over a new `POST /api/v1/shutdown?wait=true`
  path, which responds **HTTP 200** with `{success, waited, failures[]}` — 200 even
  when a group survived, so the structured survivor list (each carrying the stable
  `PROCESS_GROUP_NOT_REAPED` code) is not discarded. The CLI maps this to exit
  codes: **0** when everything stopped cleanly (after a brief bounded wait for the
  daemon's state/PID files to disappear), **1** when a process group survived (each
  printed as a `process: error` line), and **1** when the connection drops mid-wait
  and the outcome is unknown. An older daemon that predates the `wait` parameter is
  detected (the response omits `waited`) and the CLI falls back to the legacy
  `Shutdown initiated` message with exit `0`. The daemon shutdown stages were
  reordered so the API server is stopped **last** (after the supervisor stage and
  the verdict publish), letting it deliver the waited response, with the launch
  gate closed first so a lifecycle request during the drain cannot orphan a
  process.
- **Concurrent stops return the same verdict** (#32). Two `Stop` calls against the
  same process (or a daemon shutdown overlapping an in-flight per-process stop) now
  resolve to the **same** result: a secondary waiter joins the primary's stop
  episode and observes its authoritative verdict — including a
  `PROCESS_GROUP_NOT_REAPED` failure — instead of returning success early when the
  leader is reaped. A caller whose own context is canceled first still gets
  `ctx.Err()`. A reap failure now emits a `process_crashed` event uniformly from
  both the per-process stop and full-stop paths.


- **`restart` and `start` apply the current `prox.yaml`** (#33). An API-driven
  (re)start — `prox restart <name>`, and `prox start <name>` after a
  `prox stop <name>` — now re-reads and validates the whole config file and runs
  the process with its **current** config, so the edit → restart → observe loop
  no longer needs a full `prox stop` + `prox up`. Applied on (re)start: `cmd`,
  `healthcheck`, `stop_timeout` (the new value governs the next stop; the
  restart's own stop half keeps the pre-edit budget), and environment inputs
  (inline `env`, per-process and global `env_file`, including changed file
  paths). Renames, added/removed processes, and `services`/`proxy`/port changes
  still require `prox up`. The reload is **fail-closed**: an invalid file (even an
  unrelated process or the proxy section), a missing referenced env file, or a
  removed target aborts the (re)start with the existing process left running
  unchanged, via two new error codes — `CONFIG_RELOAD_FAILED` (HTTP 422) and
  `PROCESS_NOT_IN_CONFIG` (HTTP 409). The config swap is applied atomically inside
  the start's locked critical section, so a start racing a restart never leaves
  the running process and stored config mismatched.
- **Configurable stop timeout** (#35). Two new duration fields control the
  SIGTERM→SIGKILL escalation budget: global `shutdown_timeout` (top-level) and
  per-process `stop_timeout` (overrides the global). The effective budget for a
  process is its own `stop_timeout`, else the global `shutdown_timeout`, else the
  built-in `10s` default. The value is the escalation window — a fixed `2s` is
  reserved for the SIGKILL phase, so the graceful drain window is `budget − 2s`.
  Values must be greater than `2s` and at most `10m`; anything outside that range
  (including `0s`/negatives) is rejected at load with a field-named error. The
  budget is honored end-to-end — `prox stop`, `prox restart` (the stop half), and
  full daemon shutdown — and the effective value is surfaced as `stop_timeout` in
  the `GET /processes/{name}` response.

### Changed

- **Daemon shutdown now uses per-stage deadlines instead of one shared 10s
  budget** (#35). Full `prox stop` / Ctrl-C previously wrapped proxy teardown,
  API-server shutdown, and *all* process stops in a single 10-second context, so
  slow proxy/API teardown silently ate into the time available to stop processes
  — truncating an otherwise-valid graceful drain. Teardown now runs in stages,
  each with its own deadline computed at shutdown time: the proxy and API server
  get short fixed deadlines, then every process is stopped concurrently, each on
  its **own** configured stop budget (read live, so a per-process budget raised at
  runtime is respected). With nothing configured, per-process escalation timing is
  unchanged (10s/2s); the daemon's outer window simply no longer truncates a stop.

### Fixes

- `prox start <name>` now discovers the daemon's API address from `.prox/prox.state`
  like the other client commands; previously it always used the default `:5555`
  address and failed against daemons on dynamic API ports (found during #33
  verification).
- **A second `POST /shutdown` no longer panics the daemon** (#36). The shutdown
  trigger was a bare `close(shutdownCh)`, so a duplicate or concurrent shutdown
  request (e.g. a rapid double `prox stop`) closed an already-closed channel and
  crashed the daemon. Shutdown is now latched through a `sync.Once` coordinator, so
  repeated triggers are safe no-ops.
- **`POST /shutdown` now works against a `--tui` daemon** (#36). The TUI event loop
  never observed the shutdown channel, so an API shutdown (and therefore
  `prox stop`) was silently inert against `prox up --tui` — the request returned
  200 but the daemon kept running. The trigger is now routed into the TUI so it
  quits and runs the normal shutdown sequence.
- **Healthcheck `interval`/`timeout`/`retries`/`start_period` are now honored**
  (#31). Previously only `healthcheck.cmd` took effect; the timing/retry fields
  were silently dropped and replaced by the built-in defaults (`10s`/`5s`/`3`/
  `30s`), so a tuned healthcheck ran at the wrong cadence and a slow starter got
  no `start_period` grace. Configured values now reach the health checker. An
  invalid or negative duration fails `prox up` at load with a clear,
  process-named error (e.g. `processes.api.healthcheck.interval: invalid
  duration "3x"`) instead of being silently ignored; `0`/omitted still means
  "use the default".

## v0.1.3

Bug-fix release for the `prox restart`/`stop` process lifecycle (#29):
a restart now reloads the process's `env_file`, and neither `stop` nor
`restart` leaves orphaned grandchild processes holding their ports.

### Fixes

- **`restart` reloads `env_file`; `stop`/`restart` no longer orphan
  grandchildren** (#29, #30). Previously `prox restart` could report
  success while running the replacement with a stale environment and
  leaving the old process's grandchildren alive — holding the listening
  port, so the replacement failed with `EADDRINUSE` — and `prox stop`
  could leave the same orphan behind. Now:
  - `env_file` (global + per-process) and inline `env` are re-read from
    disk on every start, so `start`/`restart` pick up edited values; a
    failed reload fails loudly instead of launching with a stale env.
  - `stop`/`restart` gate on the whole process group — SIGTERM, a
    time-based graceful wait, then SIGKILL of the group with reap
    verification — so a grandchild that ignores SIGTERM is still cleaned
    up and its port freed.
  - `prox stop <name>` / `prox restart <name>` now return a non-zero
    exit and a typed `PROCESS_GROUP_NOT_REAPED` error when a group can't
    be reaped, instead of always reporting success.
  - `restart` starts the replacement on the supervisor's context, so its
    health checker survives past the request that triggered it.

### Documentation

- Document the shared proxy daemon.

## v0.1.2

Release-pipeline + plugin distribution release. Prox is now a
distributable Claude Code plugin (this repo hosts the skill;
previously it lived in `charliek/cc-plugins`), and the release
pipeline is fully on the `cc-plugins:release-workflows` convention —
no more per-pipeline PATs, single release-bot App identity for both
the Homebrew tap push and the apt-charliek dispatch.

### Features

- Prox is now a distributable Claude Code plugin (#20). This repo is
  the canonical home for the `prox` skill. Removed from
  `charliek/cc-plugins` in a companion change.

### Release process

- **Adopt `cc-plugins:release-workflows` convention** (#21) — prox is
  the third consumer of the framework (after strix and roost).
  `scripts/release/update-version.sh` bumps `.claude-plugin/plugin.json`
  (delegating to the existing `scripts/set-version.sh`) with a
  grep-verify after the delegate so silent sed no-ops fail loudly;
  `RELEASING.md` documents the per-repo policy + break-glass recovery;
  multi-target `sanity-check-app.yml` verifies the App reach to
  homebrew-tap and apt-charliek; the previous CI-driven
  `sync-version` job is retired (plugin.json bump moves local).
- **Retire `HOMEBREW_TAP_TOKEN` and `APT_DISPATCH_TOKEN` PATs** — both
  replaced by `charliek-release-bot` App tokens minted at workflow
  time (scoped to the target repo via `actions/create-github-app-token`'s
  `owner` + `repositories` inputs). GoReleaser still reads
  `HOMEBREW_TAP_TOKEN` from env; the workflow now sets it from the
  App-minted token instead of from secrets. Legacy secrets deleted
  from the secret store.
- **Server-side `Verify plugin.json matches tag`** safety net in
  `release.yaml` — catches mismatches between the released tag and
  what's actually in `.claude-plugin/plugin.json` before any artifacts
  ship. Replaces the deleted `sync-version` job's contract.
- **Branch protection ruleset** on `main` with the release-bot App +
  admin role in `bypass_actors`.
- **`update-version.sh` is now grep-verifying** — the wrapper around
  `scripts/set-version.sh` reads the file back and asserts the new
  version made it in. Silent sed no-ops on malformed manifests now
  fail loudly.

### Docs

- Document the Linux (apt) install path + a direct-.deb fallback
  (#18, #19) — apt install prox once the apt-charliek repo is added;
  `apt install ./prox_X.Y.Z_amd64.deb` for the no-apt-repo fallback
  (resolves dependencies, unlike `dpkg -i`).
- `RELEASING.md` (new) — per-repo policy doc.

## v0.1.1

### Features

- Publish `.deb` packages for `amd64` and `arm64` on every release via GoReleaser's `nfpms:` block. Install on Pop!_OS / Ubuntu 24.04+ via `apt install prox` once the [`apt-charliek`](https://github.com/charliek/apt-charliek) repository is added.
- Fire `repository_dispatch` at `charliek/apt-charliek` after a successful release so `apt update` picks up the new version automatically. Bounded retries on the dispatch call to ride out transient API blips.

### Maintenance

- Add `release-snapshot` CI job that runs `goreleaser release --snapshot` on every PR and validates both the `amd64` and `arm64` `.deb` artifacts (`Package: prox`, payload at `/usr/local/bin/prox`).

## v0.1.0

### Features

- Add shared proxy daemon for multi-project port sharing
- Add Homebrew tap automation via GoReleaser
- Add Homebrew as recommended install method in README

### Fixes

- Fix WebSocket and SSE connections dying after 30s through proxy
- Fix SSE/streaming support in reverse proxy
- Make proxy port binding failures fatal with actionable errors

### Maintenance

- Upgrade deploy-pages to v5 for Node.js 24 support
- Upgrade GitHub Actions to Node.js 24-compatible versions
- Remove legacy plans and replaced watch-pr command
- Remove release command, moved to cc-plugins

## v0.0.3

### Features

- Add HTTP proxy support for dual-stack (HTTP + HTTPS) proxying
- Add request/response body capture for proxy inspection

### Improvements

- Remove `hosts` and `certs` CLI commands, replaced with documentation
- Upgrade golangci-lint to v2 with macOS support
- Fix CI: upgrade golangci-lint-action to v7
- Fix release workflow: upgrade golangci-lint-action to v7 for v2 support

### Tests

- Add comprehensive tests for request/response body capture

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

