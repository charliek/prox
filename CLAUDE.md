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

## Docs build

**Not part of the per-commit gate above.** Only for commits that touch
`docs/`, `zensical.toml`, `pyproject.toml`, `uv.lock`, or the docs workflows
— the same set both docs workflows trigger on, since a dependency or
lockfile change can break the build just as easily as a content change:

```shell
uv run --locked zensical build --strict
```

The site is [Zensical](https://zensical.org) (not MkDocs — migrated in plan
025), configured in `zensical.toml`, built into `site-build/`. `--strict`
fails on broken links and anchors and is what both CI workflows run, so run
it locally before pushing docs changes. `uv run zensical serve` previews on
`http://127.0.0.1:7070`.

Two gotchas worth knowing: Zensical **silently ignores unknown config keys**
even under `--strict`, so a green build does not prove a config edit did
what you meant; and the `pymdownx.emoji` callables live in the
`zensical.extensions.emoji` namespace — the Material for MkDocs
`material.extensions.emoji` namespace aborts the build.

## Plans

Panel-reviewed plans live OUTSIDE this repo, at
`~/.claude/plans/prox/NNN-<slug>.md` (next free number: 026). One plan → one
PR of small gated commits. The plan file is never committed here, so the PR
body must carry the plan's substance — reviewers and future readers only
have the PR, not the plan file.

## Test-fixture pitfall: stubborn_listener.py (no longer port 15561)

`testdata/scripts/stubborn_listener.py` deliberately ignores SIGTERM (it
simulates an orphaned grandchild holding a port). If the suite is killed
mid-run, one of these can be stranded holding whatever port it had.

**Plan 027 removed the fixed-port form of this hazard.** The listener port is
no longer 15561: it is allocated dynamically per fixture
(`proxFixture.StubbornPort()`, `test/integration/fixture_test.go`), so a
stranded listener can no longer wedge a *later* run — it holds a port nothing
else wants. The old recovery recipe (`lsof -nP -i :15561`) is obsolete; there
is no fixed port to check.

A strand now costs a stray process rather than a broken suite. To find one:

```shell
pgrep -af stubborn_listener
kill -9 <pid>
```

The orphan-reaping tests register a `t.Cleanup` that kills the listener PID
they capture from the test marker, so this should be needed only if a run is
killed with SIGKILL.

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
