# CLAUDE.md

Working conventions for agent sessions in this repo. See `RELEASING.md` and
`skills/prox/SKILL.md` for deeper detail.

## Per-commit gate

Run before every commit:

```shell
make lint && make test && make test-race && make build
```

~4-5 min total (the integration suite alone is ~95s). Run these bare — do
NOT pipe through `| tail` or similar; that swallows the exit code and hides
failures.

## Plans

Panel-reviewed plans live OUTSIDE this repo, at
`~/.claude/plans/prox/NNN-<slug>.md` (next free number: 019). One plan → one
PR of small gated commits. The plan file is never committed here, so the PR
body must carry the plan's substance — reviewers and future readers only
have the PR, not the plan file.

## Test-fixture pitfall: stubborn_listener.py / port 15561

`testdata/scripts/stubborn_listener.py` deliberately ignores SIGTERM (it
simulates an orphaned grandchild holding a port). If the test suite is
killed mid-run, this process can be stranded holding port 15561
(`test/integration/restart_test.go`'s `stubbornListenerPort`). Symptom:
orphan-grandchild integration tests fail with `marker GRANDCHILD_PID= not
found`. Recover with:

```shell
lsof -nP -i :15561
kill -9 <pid>
```

## Shared daemon PID: argv is a decoy

The shared proxy daemon re-execs itself as `prox up --no-proxy`
(`internal/proxyd/daemon.go`), but that argv is a decoy — an env var
(`ProxyDaemonEnvVar`) intercepts it in `Execute()` and runs `RunDaemon()`
instead of a real `up --no-proxy`. Do not `pgrep`/match on that argv to find
the daemon PID. Use `prox proxy status --json` (reports `PID` directly).

## Releases

Use the release-workflows convention (`/release-workflows:release`): two
commits (changelog entry, then version bump) plus an annotated tag — see
`RELEASING.md` for the full mechanics. The shared daemon requires an exact
version match with its clients, so after
any release, restart the daemon and every project running against it onto
the new binary.
