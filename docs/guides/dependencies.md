# Dependencies, Tasks, and Process Gating

Most local dev stacks need more than a list of commands to run: a database
has to be up before the API connects to it, a migration has to run before
either can be trusted, and a worker has no business starting until both are
ready. prox supports this with three cooperating pieces — `dependencies:`,
`tasks:`, and a process's `depends_on` — without taking over the resources
themselves. This guide walks a realistic project shape end to end.

## The shape of a gated project

Say the project runs against a compose-backed Postgres and Redis, plus a
Restate server, and needs a one-shot registration call before its worker can
start:

```yaml
processes:
  api:
    cmd: go run ./cmd/api
    depends_on: [postgres, redis]

  worker:
    cmd: go run ./cmd/worker
    depends_on: [postgres, redis, restate, register]

dependencies:
  postgres:
    check:
      tcp: localhost:5432
    start: docker compose up -d postgres
    on_failure: fail

  redis:
    check:
      tcp: localhost:6379
    start: docker compose up -d redis
    on_failure: fail

  restate:
    check:
      url: http://localhost:9070/health
    start: docker compose up -d restate
    on_failure: fail

tasks:
  register:
    cmd: ./scripts/register-worker.sh
    depends_on: [restate]
    timeout: 30s
```

`postgres`, `redis`, and `restate` are **dependencies** — things prox does
not own but needs to be reachable. Each has a `check` (how prox knows it's
ready) and a `start` (how prox brings it up if the check fails). `register`
is a **task** — a one-shot command that runs once, after `restate` is ready,
and is done. `api` and `worker` are ordinary processes gated with
`depends_on` on the pieces they need.

Run it the normal way:

```bash
prox up -d
```

`prox up` returns immediately — it does not block on any of this resolving.
`api` and `worker` sit in `waiting` while their dependencies and (for
`worker`) the `register` task resolve in the background.

## What prox will and will NOT do

This is the part worth internalizing before writing your own config:

- **prox never stops or tears down an external resource it brought up.**
  `start: docker compose up -d postgres` launches Postgres; **`prox down`
  leaves the compose services running.** prox has no concept of "owning"
  a database container or a system daemon — only of checking whether it's
  reachable and, optionally, running a command that might bring it up.
- **prox DOES kill its own still-running `start` helper on shutdown** — if a
  `start` command is still executing (has not yet exited) when the
  dependency resolves, when `prox up` shuts down, or when a config reload
  redefines the dependency, prox sends SIGTERM (then SIGKILL after a grace)
  to that command's own process group. This is scoped to the `start`
  command's own group; anything it already detached (a container runtime's
  background process, `docker compose`'s own daemon-side work) is untouched.
- **Daemonizing `start` commands is the intended pattern**, not a workaround.
  `docker compose up -d` is the canonical example: the `sh -c` wrapper exits
  almost immediately, its work continues in already-detached processes, and
  readiness comes from the `check` succeeding — never from `start` itself
  finishing. A `start` that blocks forever (e.g. `docker compose up`
  without `-d`) would just sit there consuming no budget signal of its own;
  the `check.timeout` budget is what actually bounds the wait.
- **A task never re-runs on its own.** `register` runs once, the first time
  something demands it. A later `prox restart worker` does not re-run
  `register` — only a manual `prox restart register` (or `prox stop
  register` + `prox start register`) does.

## Cold start vs. warm start

The **initial check always runs first**, before anything else. If Postgres
is already running from a previous session, the `tcp` check on
`localhost:5432` passes immediately, `postgres` goes straight to `healthy`,
and `start` is **never invoked** — prox does not run `docker compose up -d`
against an already-healthy dependency.

Only when the initial check **fails** does prox launch `start` (once) and
begin polling the check on `check.interval` until it passes or the
`check.timeout` budget (which includes the `start` command's own execution
time) runs out. This is why a cold start and a warm start of the same
project behave differently but converge to the same place: cold, you'll see
`postgres` sit in `waiting`/`polling` for a few seconds while `docker
compose up -d postgres` does its work; warm, it's `healthy` on the first
check.

## Status and exit codes

`prox status` decorates the STATUS column for gated processes and adds a
`Dependencies:` section listing every configured dependency's resolution
state.

Mid-resolution, right after `prox up -d`:

```text
$ prox status
Status: running
Uptime: 3s
Config: /path/to/prox.yaml

NAME      STATUS               PID    UPTIME  RESTARTS  HEALTH
----      ------               ---    ------  --------  ------
api       waiting(postgres, redis)  -      0s      0         unknown
worker    waiting(postgres, redis, restate, register)  -  0s  0  unknown
register  waiting(restate)     -      0s      0         unknown

Dependencies:
NAME      STATE     CHECK                          DETAIL
----      -----     -----                          ------
postgres  polling   tcp localhost:5432
redis     polling   tcp localhost:6379
restate   pending   url http://localhost:9070/health
```

A few seconds later, once everything converges:

```text
$ prox status
Status: running
Uptime: 12s
Config: /path/to/prox.yaml

NAME      STATUS     PID    UPTIME  RESTARTS  HEALTH
----      ------     ---    ------  --------  ------
api       running    41213  6s      0         healthy
worker    running    41240  2s      0         healthy
register  completed  -      9s      0         unknown

Dependencies:
NAME      STATE    CHECK                          DETAIL
----      -----    -----                          ------
postgres  healthy  tcp localhost:5432
redis     healthy  tcp localhost:6379
restate   healthy  url http://localhost:9070/health
```

`register` shows `completed` with `-` in the PID column and a frozen
uptime — it ran to completion and won't run again this lifetime. Exit code
is `0`: a completed task never trips the exit contract.

Now suppose `postgres`'s `check` never passes (say, `docker compose up -d
postgres` failed to pull an image) and `on_failure: fail` is set (the
default):

```text
$ prox status
...
NAME    STATUS         PID  UPTIME  RESTARTS  HEALTH
----    ------         ---  ------  --------  ------
api     blocked(postgres)  -  0s  0    unknown
worker  blocked(postgres)  -  0s  0  unknown

Blocked: api(postgres), worker(postgres)

Dependencies:
NAME      STATE   CHECK                          DETAIL
----      -----   -----                          ------
postgres  failed  tcp localhost:5432             dial tcp 127.0.0.1:5432: connect: connection refused
redis     healthy tcp localhost:6379
restate   healthy url http://localhost:9070/health

$ echo $?
1
```

`prox status` exits `1`: any process left `blocked` fails the status
contract, same as a `crashed` process. The exact precedence (when multiple
signals hold at once) is **proxy-down > crashed > blocked >
failed-dependency** — see [`prox status`](../reference/cli.md#status).

If `postgres` instead had `on_failure: warn`, the same exhausted check would
land it in `warned` (not `failed`), and `api`/`worker` would still launch —
`warned` counts as satisfied, exactly like `healthy`. `prox status` exits
`0` in that case; a warned dependency is visible in the table but is not a
failure.

## Cost of a crashed task

A task that exits non-zero (or times out) lands in `crashed`, and every
process or task gated on it goes `blocked`:

```text
$ prox status
NAME      STATUS      PID  UPTIME  RESTARTS  HEALTH
----      ------      ---  ------  --------  ------
register  crashed     -    -       0         unknown
worker    blocked(register)  -     0s        0    unknown

Crashed: register — check 'prox logs register'.
Blocked: worker(register)
```

Note `crashed` outranks `blocked` for the single primary exit-1 message even
when both print.

## Troubleshooting

**A dependency is stuck `failed` and its dependents are `blocked`.** Fix
whatever the check was probing (start the container manually, fix a
firewall rule, correct a typo'd `check.tcp` target), then re-demand it:

```bash
prox start api
```

`prox start` on a blocked, gated process resets its **failed** targets (only
the ones that actually failed — a healthy/warned target's cached result is
reused, not re-probed) and schedules a fresh resolution. If the dependency
now checks out, `api` converges to `running`.

**A task needs to run again** (you fixed the script, or need to re-seed):

```bash
prox restart register
```

This reloads `register`'s config, re-resolves its own `depends_on`, and — if
those are satisfied — runs it again. A completed task's dependents are not
automatically re-triggered by this; they already saw it complete once this
lifetime, so if they need the fresh run's output, restart them too.

**`prox up -d` returned fast but nothing looks like it's starting.** That's
expected — gated processes launch asynchronously. Poll `prox status --json`
(or `prox status` in a loop) rather than assuming a fast `up` means
everything is running; check the `Dependencies:` section and each process's
`waiting(...)` targets to see what's still resolving.

**A `docker compose` service refuses to stop after `prox down`.** This is
not a bug — see [What prox will and will NOT do](#what-prox-will-and-will-not-do)
above. `prox down` never tears down external resources; stop the compose
stack yourself (`docker compose down`) when you're done with it.

## See also

- [Configuration Reference: Dependencies, Tasks, and Process Gating](../reference/configuration.md#dependencies-tasks-and-process-gating) —
  full field tables and validation rules.
- [`prox status`](../reference/cli.md#status) — exit-code precedence and
  table rendering.
- [HTTP API: GET /status](../reference/api.md#get-status) — the
  `dependencies` array and each process's `waiting_on`/`blocked_on` fields.
