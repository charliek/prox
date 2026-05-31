# Changelog

All notable changes to this project will be documented in this file.

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
