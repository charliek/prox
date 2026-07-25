# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

Capture-by-default (plan 012): a proxy-enabled project now records
request/response headers and bodies through the proxy with no extra config,
bounded by the disk-budget accounting from #69 and covered by capture-time
header/query-param redaction. Bodies are captured verbatim, though — see the
Breaking entry.

### Breaking

- **Capture is on by default whenever the proxy is enabled** (plan 012).
  Previously a proxy-enabled project recorded only request/response metadata
  (method, URL, status, timing) unless `--capture` was passed; now it also
  captures headers and bodies by default, spilling large bodies to
  `~/.prox/capture` under the shared disk-budget accounting (#69, 1GiB default
  across all projects on the daemon). To opt out, set `proxy.capture.enabled:
  false` in `prox.yaml`, or pass the new `--no-capture` flag for one run
  (`--capture` still exists for explicitness/compat but no longer does
  anything a default-on project doesn't already get; the two flags are
  mutually exclusive). Capture-time redaction (below) covers headers and URLs
  only — request/response **bodies are captured verbatim**, so a project
  whose bodies carry secrets (API keys, tokens, PII in JSON/form payloads)
  should opt out entirely rather than rely on redaction. Anything that assumes
  `~/.prox/capture` stays empty (disk-space checks, backup exclusions,
  sandboxed CI with a tight tmpfs) should account for the new default.
  `prox proxy status` gained `capture_available`/`capture_error` (JSON) and a
  `Capture: unavailable (<reason>)` line (human output) for the case where the
  daemon's own capture manager failed to initialize — distinct from a project
  simply choosing capture off.

### Added

- **Capture disk budget + record-group FIFO eviction** (#69).
  `proxy.capture.disk_budget` (e.g. `512MB`, `2GB`) bounds the shared daemon's
  total spilled-body bytes across every registered project; unset defaults to
  1GiB, and the daemon-wide effective bound is the **minimum** across every
  capture-enabled project's own budget (a project that leaves it unset
  contributes the default to that minimum). Once the bound is exceeded, the
  oldest record's spilled body files are evicted first (FIFO by record
  group).
- **O(1) ID index for the request ring** (#71). `RequestManager`'s
  `Upsert`/`GetByID`/cursor-anchoring lookups no longer linear-scan the ring,
  keeping `prox requests` and the TUI responsive as capture-by-default raises
  per-project request volume.
- **Capture-time redaction of sensitive headers and query params** (plan 012
  D4). On by default whenever a capture config exists (disable with `redact:
  false`); the built-in sets always redact the `Authorization`,
  `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Auth-Token`
  headers and the `access_token`, `refresh_token`, `id_token`, `token`,
  `api_key`, `apikey`, `client_secret`, `code` query params — including
  inside a `Location`/`Referer` redirect URL's query string — and are
  extendable per project via `redact_headers`/`redact_query_params` (these
  only add to the built-ins, never replace them). Limitation: redaction never
  touches request/response bodies — see the Breaking entry above.

## v0.2.2

An exit-contract polish pass (plan 011, PR #77): `prox status` and `prox
stop`/`prox down` exit codes now tell the whole truth (crashed children and
wedged daemon teardowns exit 1), a crashed project's proxied routes converge
in about one request instead of up to 30 seconds, and the repo gained a
CLAUDE.md for agent sessions.

> **Upgrading:** as with previous releases, the shared daemon requires an
> exact version match with its clients. After installing this release, stop
> the old daemon and restart every project: `prox proxy stop --force`, then
> `prox up -d` in each project — or run `prox up` in one project and let the
> idle-daemon auto-heal replace it. Scripts keying on `prox status` /
> `prox stop` exit codes: read the Breaking entries below.

### Breaking

- **`prox status` exits 1 when any child process is crashed** (#72). Previously
  `prox status` exited 0 while the table showed a `crashed` child, so scripts
  and coding agents keying on the exit code could miss a dead process entirely.
  It now exits non-zero whenever any process is in the `crashed` state: the
  table adds a `Crashed: <name>[, ...] — check 'prox logs <name>'.` line after
  the proxy line, and stderr carries `Error: N process(es) crashed` — unless the shared
  proxy is also down, in which case the proxy-down error takes stderr
  precedence while both signals still print (the JSON
  payload is unchanged — it already reports each process's `status`). The
  exit-0 contract is now exact: `prox status` exits 0 only when the supervisor
  query succeeded, no process is `crashed`, and any configured shared proxy is
  reachable — it does **not** assert every process is `running` or `healthy`
  (`starting`, `stopping`, `stopped`, and running-but-`unhealthy` all still
  exit 0). Note the supervisor marks *any* non-Stop-driven exit as `crashed`,
  including a one-shot child that exits 0 (a migration/seed step): such a
  project fails `prox status` until restarted, so one-shot helpers should live
  outside `prox.yaml` or expect a non-zero status. Scripts that need the old
  behavior must read `prox status --json` and inspect the per-process `status`
  they care about — not `prox status || true`, which would also mask discovery
  errors and an unreachable supervisor. This consolidates the status
  exit-code churn started in v0.2.1 (proxy-down → exit 1, #66) into one
  adoption window.
- **`prox stop`/`prox down` exit 1 when the daemon teardown wait times out**
  (#73). Previously, once the process-stop verdict was clean, a `prox stop`
  (or `prox down`) that then timed out waiting (up to ~15s) for the daemon's
  own state/PID files to disappear still printed `Stopped processes` plus a
  `Warning: the daemon is still finishing shutdown` to stderr and exited `0`.
  It now returns a `shutdown incomplete: daemon still finishing after 15s`
  error and exits `1`, joining the same `shutdown incomplete` family as the
  existing survivors contract (v0.1.4, #36) — an unconfirmed daemon teardown
  is no longer treated as a clean stop. The existing stdout/stderr messages
  are unchanged, but stderr gains the CLI's standard
  `Error: shutdown incomplete: …` line. Scripts that assumed `prox stop`/`prox down` always exit `0`
  once processes are reported stopped must check the exit code (or parse
  stderr for the `Warning:` line) if they need to distinguish a fully torn
  down daemon from one still finishing up.

### Lifecycle

- **Crashed projects' routes converge in about one request instead of up to
  30s** (#74). Previously, when a project crashed, the shared proxy kept
  serving its routes — returning `502 Backend unavailable` — until the periodic
  stale-PID sweep noticed the dead owner, which could take up to 30 seconds. The
  daemon now also probes on demand: when a route's backend transport fails and
  the proxy returns a 502, it checks that route's owning `prox up` process
  off the request path and, if it is dead, reaps the registration right away, so
  subsequent requests get a clean `404` (or fall through to a shared-port
  neighbor) almost immediately. The probe is flap-safe — it keys on the
  *owner's* identity, not the backend's, so a merely restarting or flapping
  backend under a live `prox up` is never deregistered — and cheap under load: a
  single in-flight probe per project, rate-limited, with a trailing probe so a
  502 that arrives right after the crash is never missed. The 30s sweep remains
  the backstop for cases with no live 502 to trigger on (backend-authored 502s,
  mid-stream aborts, and dead projects receiving no traffic).

### Docs

- **Repo CLAUDE.md** (#75): working conventions for agent-driven development
  — per-commit gate, plans convention, the port-15561 test-fixture pitfall,
  the daemon argv decoy, and the release convention.

## v0.2.1

A hardening pass on lifecycle signals and the requests pipeline: `prox up`/`prox status`/`prox stop` now give trustworthy exit codes and error messages instead of silently degrading, the shared proxy daemon self-heals and isolates projects from each other, and captured request/response bodies decode more content types. Plus a refreshed agent skill and docs.

> **Upgrading:** as with v0.2.0, the shared daemon requires an exact version
> match with its clients. After installing this release, stop the old daemon
> and restart every project: `prox proxy stop --force`, then `prox up` in
> each project — or just run `prox up` in one project and let the idle-daemon
> auto-heal (below) replace it for you.

### Breaking

- **`prox status` exits 1 when the shared proxy is down** (#66). Previously
  `prox status` only reflected the project supervisor's own process health,
  so a dead shared proxy daemon could go unnoticed behind a clean-looking
  status. It now probes the daemon and prints `Proxy: DOWN — shared proxy
  daemon unreachable (proxied routes are dead). Check 'prox proxy status'.`,
  exiting non-zero, even when every child process is healthy. Scripts that
  treated `prox status`'s exit code as pure process health must account for
  proxy health too.
- **Client commands no longer fall back to `:5555`** (#66). `status`,
  `logs`, `stop`, `start`, `restart`, `down`, `attach`, and `requests`
  previously dialed the compiled-in `127.0.0.1:5555` default when they
  couldn't discover a running instance from `.prox/prox.state` — silently
  talking to nothing, or the wrong daemon. They now error with remediation
  ("run from the project directory, or pass `--addr`") instead. Scripts
  invoking these commands from outside the project directory must pass
  `--addr` explicitly.
- **`prox up -d` exit code is now truthful** (was always 0). The detach
  parent used to print `prox started (pid N)` and exit 0 immediately after
  forking, before the child had loaded its config, bound its ports, or
  registered its routes — so a child that died during startup still reported
  success. `prox up -d` now polls up to 15s for the child to become ready
  (state file + `/health`) before exiting 0; it exits 1 with a
  `.prox/prox.log` tail on early child death or a never-ready timeout
  (killing the child on timeout).
- **Orphan ledger format is a forward-incompatible envelope** (#67). The
  supervisor's crash-recovery ledger (`.prox/prox.children`) is now
  `{"boot_marker":"...","children":[...]}` instead of a bare array, so a
  reboot can't let a PID/start-token collision reap an unrelated process
  group. A pre-upgrade `prox` binary cannot parse the new envelope — it
  fails to unmarshal and simply skips the reap (never unsafe, just less
  helpful) until every project has been restarted on this version. Downgrade
  from this version briefly loses crash-recovery reaping for the same
  reason.
- **New dependencies**: `github.com/klauspost/compress` (zstd decoding) and
  `github.com/andybalholm/brotli` (brotli decoding), both pure Go.

### Lifecycle

- **Version-skew hard fail with idle-daemon auto-heal**. A shared proxy
  daemon running a different version than the connecting `prox` used to
  silently fall back to a proxy-less standalone start. Now a version
  mismatch is a typed error: if the daemon still has registered projects,
  `prox up` fails hard, naming both versions, every registered project
  directory, and the exact remediation; if the daemon is idle, `prox up`
  auto-replaces it with a fresh daemon of the current version and prints a
  one-line notice. Standalone proxy create/start failures are likewise now
  fatal when a proxy is configured (`--no-proxy` is the escape hatch)
  instead of warn-and-continue — no code path starts a project with a
  silently-disabled proxy anymore.
- **Registration recovers from a draining daemon**. Registering during the
  shared daemon's brief shutdown grace used to be a fatal `SHUTTING_DOWN`
  error whose text said "retry" but nothing retried. `prox up` now polls for
  the old daemon to finish draining (up to 10s), starts a fresh daemon, and
  re-registers automatically; a second failure is still fatal.
- **Forwarder self-heal after daemon death** (#66). When the shared daemon
  dies, every registered project used to reconnect silently forever with no
  path back. Projects now detect a prolonged (15s+) connection failure and
  re-register with a fresh or recovered daemon automatically (damped to at
  most once per 30s), worst case within ~45s — no `prox proxy stop --force`
  required. The daemon's re-registration path was also made idempotent for a
  live same-identity holder, closing a would-be `REGISTRATION_CONFLICT`
  loop. Healing is suppressed while a project is itself shutting down, and a
  version-mismatch failure surfaces as a status state rather than
  restarting a daemon that might still be in use.
- **Boot-marker guard for the orphan ledger** (#67). See Breaking above —
  the ledger now discards itself across a reboot (and on Linux, when it
  predates this marker) instead of risking a PID/start-token collision.

### Requests

- **`prox status` / `GET /status` surface shared-proxy health** (#66). A new
  `proxy` block reports `mode` (`shared`/`standalone`/`disabled`),
  `daemon_reachable`, `daemon_version`, reconnect/drop/backfill counters,
  and `heal_state`. The CLI renders this as a `Proxy:` line (see Breaking).
- **Stale flag for stuck in-flight requests** (#53). A request that has been
  `in_flight` for more than 5 minutes now also carries `stale: true`
  (`stale?` in the CLI/TUI) — the completion event may have been lost and
  the outcome is unknown. Computed at serve time; no protocol or storage
  change. Long-lived streams and large transfers can legitimately show
  `stale?` while still live.
- **Drop and degradation counters**. The request stream's subscriber
  channels used to drop events silently on overflow. Dropped-event and
  backfill-failure counts are now tracked (atomics) and exposed through the
  `proxy` status block, so silent request loss becomes visible.
- **More captured-body content types decoded** (#50). `deflate` (zlib with a
  raw-deflate fallback), `zstd`, and `br` (brotli) captured bodies now
  decode for display, joining `gzip`/`x-gzip`; all four are bounded by the
  same 10MB decode cap. Chained or unrecognized encodings, truncated
  captures, and corrupt streams still fall back to raw bytes.
- **Hex preview for binary bodies in the TUI** (#50). The request detail
  view renders binary bodies as a bounded `hexdump -C`-style preview (first
  256 bytes, offset + hex + ASCII gutter) instead of an inert
  `[binary data]` placeholder.
- **Cursor pagination for `GET /proxy/requests`** (#50). A new `before_id`
  parameter pages strictly older than the given record, returning
  `next_before_id` (the oldest *scanned* record, so a fully-filtered page
  still advances) for the next page. An unknown, evicted, or out-of-scope
  cursor returns `410` with code `CURSOR_GONE`. `prox requests` (CLI) is
  unchanged — this is an API-only capability for now.
- **Per-project request rings and capture caps on the shared daemon**
  (#49). The daemon's single global request ring and capture cap used to let
  one chatty project evict another project's records and bodies, and a
  per-project `capture.max_body_size` was silently ignored on the daemon.
  Each registered project now gets its own full-capacity ring and its
  `max_body_size` is enforced per request on the shared capture path — one
  project flooding traffic cannot affect another's history or capture
  quota.

### Skill & Docs

- **Agent skill refreshed to post-hardening reality**. `skills/prox/references/api.md`
  and `SKILL.md` now document 12-char request IDs, `since`/`url_contains`,
  `in_flight`/`stale`/`captured_size`/`content_encoding`/`unavailable_reason`,
  the `before_id`/`next_before_id`/`CURSOR_GONE` cursor, the `proxy` status
  block, the no-`:5555`-fallback rule, and the truthful `prox up -d` exit
  code / version-mismatch behavior.
- **README, architecture, and reference docs updated to match**: the HTTP
  API table gains the `/proxy/requests` endpoints; `docs/development/architecture.md`
  documents the shared proxy daemon, the capture pipeline, and the forwarder
  bridge; `docs/reference/api.md`/`cli.md`/`configuration.md` document the
  `proxy` status block, `stale`, cursor pagination, the decoder list, and
  per-project `max_body_size`; `docs/guides/shared-proxy.md` documents
  self-healing and version-mismatch behavior.

## v0.2.0

The requests/capture overhaul: the shared proxy daemon now captures request and
response bodies per project, `prox requests` becomes an ngrok-style inspector,
crashed generations self-heal, and the TUI gains request/log search. Plus a set
of registration/lifecycle hardening fixes and supervisor orphan cleanup.

> **Upgrading:** the daemon requires an exact version match with its clients, so
> after installing this release, stop the old daemon and restart every project:
> `prox proxy stop --force`, then `prox up` in each project (the version gate
> makes a mismatch loud).

### Features

- **Body capture in the shared proxy daemon** (#40, plan 005). Daemon mode now
  captures request/response bodies per project under `~/.prox/capture`, gated by
  a per-project `proxy.capture` config (`enabled`, `max_body_size`). Records are
  scoped by project directory (not hostname), gzip/deflate bodies are decoded for
  display rather than corrupted, `content_encoding`/`captured_size` are recorded,
  binary bodies are detected integrity-first, and captured bodies are delivered to
  each project over the SSE bridge. Request IDs are now 12 hex chars.
- **`prox requests` as an inspector** (plan 005). New agent- and human-friendly
  filters: `--url <substr>`, `--since <5m|RFC3339>`, `--min-status`/`--max-status`,
  `--method`, `--subdomain`, `--json`, and `prox requests <id> --body` to view
  captured bodies. Responses expose `hostname`, `content_type`, `content_encoding`,
  and `unavailable_reason`. Body titles show the content type and pretty-print JSON.
- **In-flight request visibility** (#48, plan 006). Requests are recorded at
  response-header time with `in_flight: true` and completed via a defer, so a
  request is visible while it streams and an aborted stream still records instead
  of vanishing. Backed by a monotonic in-flight → final state machine
  (`RequestManager.Upsert`).
- **Snapshot backfill on (re)connect** (#51, plan 006). A project's forwarder
  fetches the daemon's existing records concurrently with the live stream on every
  (re)connect, so reconnecting no longer loses history.
- **TUI requests view: explicit cursor + search** (#47, plan 007). The requests
  view gains an ID-anchored cursor and `/`-search that jumps the cursor to matches
  with `n`/`N` navigation (wrapping), composing with the `s` filter rather than
  replacing it.
- **TUI detail live-refresh** (#54, plan 007). An open request-detail view
  refreshes in place when the request completes, instead of going stale.
- **TUI logs view: search-match navigation** (#56, plan 009). The logs view gains
  `/`-search that jumps the cursor to matching lines with `n`/`N` (wrapping) and
  match highlighting, mirroring the requests view. Note: logs `/` now **navigates**
  (non-matching lines stay visible) rather than filtering — use `s` for the live
  substring filter.

### Fixes

- **Crash-restart registration self-heal** (#55, plan 007). When a `prox up`
  crashes without deregistering, a restart of the same project now detects the
  dead registration and replaces it inline instead of failing with a 409, so a
  crashed generation recovers without `prox proxy stop --force`. Lifecycle
  transactions are serialized end-to-end, with a brief bind retry on every
  registration bind and an epoch-guarded graced shutdown check.
- **HTTPS certs for domains joining an existing listener** (#58, plan 008).
  A project whose HTTPS port is already bound by another project now gets its
  domain's certificate generated, so the shared TLS listener's SNI callback can
  serve it (previously the joining domain's handshakes failed).
- **Forced-stop teardown serialized with lifecycle transactions** (#60, plan 008).
  `prox proxy stop --force` now sets the shutdown flag before responding and
  drains any in-flight register/deregister under a barrier, so a concurrent
  registration can't interleave with physical teardown.
- **PID-reuse can no longer defeat liveness checks** (#61, plans 008/009).
  Registrations carry an opaque per-host process start token; the stale-PID sweep
  and crash-restart self-heal key liveness on `(pid, token)` so a reused PID
  naming a different process reads as dead, and the sweep's removal guard is
  token-aware so it can't tear down a live restart that reused a crashed PID.
- **Supervisor: reap orphaned child groups after `kill -9`** (#59, plan 009).
  When a `prox up` is killed with `kill -9`, the backend process groups it
  supervised are orphaned and keep holding their ports. The supervisor now
  persists an ownership ledger and, on the next `prox up`, reaps any leftover
  group it can positively identify (strict start-token match) — so the restarted
  generation rebinds its ports instead of 502'ing on a wedged orphan.

### Internal

- **Cert generation runs outside the cache lock** (plan 009). `EnsureDomain` no
  longer holds the certificate cache lock across the mkcert subprocess and key
  load, so a joining domain's first-time generation no longer stalls TLS
  handshakes for other domains on the shared listener.
- **CI runs on macOS as well as Linux** (plan 009). The `test` job now runs on
  both `ubuntu-latest` and `macos-latest`, exercising the darwin process
  start-token path on every change.

## v0.1.4

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

- **SSE streams are no longer cut at 30 seconds** (#42). `GET /api/v1/logs/stream`
  and `GET /api/v1/proxy/requests/stream` sat in the router's default 30s
  request-timeout class, so `prox logs -f`, `prox attach`, and proxy-request
  streams silently terminated after ~30s. The two SSE routes are now exempt from
  the request timeout: streams are long-lived and end only on client disconnect
  or daemon shutdown. Shutdown now also closes the shared-proxy-daemon request
  forwarder's subscriber channels, so an attached request stream no longer pins
  the API server to its full teardown-stage deadline.
- **`prox requests` now discovers the daemon's API address** (#43). `requests`
  was missing from the client-command discovery allowlist, so it always talked
  to the default `:5555` address and failed against daemons on dynamic API ports
  (the default) — the same gap `start` had. The allowlist is now pinned by a
  test so a new client command can't silently miss discovery.
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
- `GET /api/v1/logs/stream` now returns a clean JSON error (`STREAMING_NOT_SUPPORTED`)
  when the connection cannot stream, instead of writing SSE headers first (#40).
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

