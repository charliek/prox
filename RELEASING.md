# Releasing prox

The general release framework is `cc-plugins:release-workflows`; this file
documents what's specific to this repo.

## TL;DR

    /release-workflows:release v0.1.2

That's it. Everything else is automatic.

## What happens

1. **`release-workflows:release`** (LLM, local):
   - Verifies branch (`main`) + clean tree + CI green on HEAD
   - Asks/confirms version
   - Drafts a CHANGELOG entry from `git log v<previous>..HEAD`, commits as
     `docs(changelog): vX.Y.Z entry`
   - Runs `scripts/release/update-version.sh X.Y.Z` → bumps
     `.claude-plugin/plugin.json` (delegated to `scripts/set-version.sh`)
   - Commits as `chore(version): bump to X.Y.Z`
   - Tags `vX.Y.Z` (annotated) on the version commit
   - `git push --follow-tags` (admin bypasses the ruleset)

2. **`release.yaml`** (CI, on tag push `v*`):
   - **`release`** (single job):
     - Checks out, sets up Go, runs `go test` + golangci-lint
     - Mints a release-bot App token scoped to `charliek/homebrew-tap`
     - Runs `goreleaser release --clean`, which:
       - Builds 4 targets (`linux/darwin` × `amd64/arm64`) with version
         injected via ldflag (`-X .../internal/version.Version={{.Version}}`)
       - Tarballs them as `prox_<os>_<arch>.tar.gz`
       - Builds .debs (amd64 + arm64) via `nfpms:` config
       - Uploads tarballs + .debs + `checksums.txt` to the GitHub Release
       - Auto-generates release notes from commits
       - Pushes `Formula/prox.rb` to `charliek/homebrew-tap` using the
         App-minted token (replaces the legacy `HOMEBREW_TAP_TOKEN` PAT)
     - Mints a release-bot App token scoped to `charliek/apt-charliek`
     - Dispatches `repository_dispatch` (`event_type=publish`) to
       `charliek/apt-charliek` with the App-minted token (replaces the
       legacy `APT_DISPATCH_TOKEN` PAT)

The maintainer runs step 1; everything else is automated.

## Version files this repo owns

`scripts/release/update-version.sh` (which delegates to
`scripts/set-version.sh`) bumps:

- `.claude-plugin/plugin.json` — `version` field, read by Claude Code at
  install/update time. This is the canonical source-tree version manifest.

NOT bumped:

- **Go binary version** — comes from a build-time ldflag injected by
  GoReleaser. The ldflag references `{{.Version}}`, which GoReleaser
  computes from `${GITHUB_REF_NAME#v}` at build time. No source-tree
  manifest to bump.
- `pyproject.toml` — for the mkdocs docs site; has its own version
  cadence and is NOT touched by releases.

## Snapshot / dev versioning

Not used. Main between releases shows the last released version. If you
want `prox --version` between releases to show commits-past-tag identity,
add `-X .../version.Commit={{.Commit}}` to `.goreleaser.yaml`'s
`builds[].ldflags`. Not currently wired.

## Secrets

| Secret | Purpose | Required? |
|---|---|---|
| `RELEASE_BOT_APP_ID` | `charliek-release-bot` GitHub App ID | required — minted at workflow time for both the homebrew-tap push (via GoReleaser) and the apt-charliek dispatch |
| `RELEASE_BOT_APP_KEY` | App private key (.pem) | required — same |

Retired (deleted during the convention adoption):

- `APT_DISPATCH_TOKEN` — replaced by the App-minted apt-charliek token
- `HOMEBREW_TAP_TOKEN` — replaced by the App-minted homebrew-tap token

GoReleaser still reads the env var named `HOMEBREW_TAP_TOKEN`; the
workflow sets it from `steps.tap.outputs.token` instead of from
`secrets`.

## Branch protection

`main` is protected by a ruleset (created during the convention
adoption) with `required_status_checks=[]` (none — prox's `ci.yml` runs
multiple separate jobs `test` / `lint` / `release-snapshot` and there's
no single aggregator check). Bypass actors:

- `charliek-release-bot` (App, type `Integration`) — lets the App push
  to the homebrew-tap, dispatch to apt-charliek, and any future
  post-build push back to prox's main
- Admin role (id `5`, type `RepositoryRole`) — lets
  `/release-workflows:release`'s push of the changelog + version commits
  + tag land

Inspect or edit at https://github.com/charliek/prox/rules.

## App installation

The release-bot App must be installed on three repos:

- `charliek/prox` itself (so the workflow's secrets resolve)
- `charliek/homebrew-tap` (so the minted token can push the formula)
- `charliek/apt-charliek` (so the minted token can fire the dispatch)

Verify all three via the `sanity-check-app.yml` workflow (Actions → Run
workflow). Each block must print the expected repo name.

## When things break

| Symptom | Cause | Fix |
|---|---|---|
| `git push` rejected: `Required status check ...` | Pusher not in ruleset bypass | Confirm both the App and the admin role are in `main`'s ruleset `bypass_actors` |
| GoReleaser fails at `brews` with `Bad credentials` | `RELEASE_BOT_APP_ID` unset OR App not installed on `homebrew-tap` | Confirm via `sanity-check-app.yml`'s homebrew-tap block; install the App on the tap if missing |
| `Trigger apt-charliek publish` step fails | App not installed on `apt-charliek` OR rate-limited | Confirm via `sanity-check-app.yml`'s apt-charliek block; install the App if missing. The dispatch step retries 3× internally; persistent failures usually mean install state, not network |
| `release` job's `go test` fails on the tagged commit | Real test failure | Fix on a branch, merge, cut a fresh patch tag (don't force-update the failed tag) |
| Formula push succeeded but `brew install` finds old version | Homebrew tap cache | `brew untap charliek/tap && brew tap charliek/tap` |
| Claude Code installs old plugin version after the release | `plugin.json` wasn't bumped before the tag | `/release-workflows:release` should have bumped it via `update-version.sh`; if not, bump manually with `scripts/set-version.sh <ver>` + commit + push to main, then redirect Claude Code users to reinstall |

## Break-glass recovery

### Homebrew formula push failed

If GoReleaser failed at the `brews` step but everything else uploaded:

```bash
# Re-run just GoReleaser locally with the App token. Export the token
# via gh's App-token flow (requires gh's app-token plugin or local mint).
# Easiest path: re-run the GitHub Actions workflow run from the UI —
# GoReleaser's `mode: replace` in .goreleaser.yaml reuses the existing
# Release without re-uploading.
```

### apt-charliek dispatch failed

The dispatch step retries 3× with backoff. If all three fail and the
release.yaml job is otherwise green: re-run just this step from the
Actions UI, OR wait for apt-charliek's next scheduled scan (it
self-heals by detecting unpublished .debs on the GitHub Release).

### plugin.json drifted from the released tag

If main's `.claude-plugin/plugin.json` doesn't match the latest released
tag (shouldn't happen with the new flow, but if it does):

```bash
scripts/set-version.sh <released-version>
git add .claude-plugin/plugin.json
git commit -m "chore: align plugin.json with v<released-version>"
git push origin main
```

## Adopting the convention (for new contributors)

If you're new to this repo and need to understand the release pipeline,
read [`cc-plugins/plugins/release-workflows/references/convention.md`](https://github.com/charliek/cc-plugins/blob/main/plugins/release-workflows/references/convention.md)
in the framework repo. It defines the contract every file in this
repo's `scripts/release/` and `.github/workflows/release.yaml` is
written against.

## Notes for this repo

- **No `version-check` job**: Go binaries don't have a single source-tree
  version manifest matching the tag — `.claude-plugin/plugin.json` is
  the only file that holds a literal version, and the Go binary's
  version is ldflag-injected at build time. A version-check job would
  need to compare the tag against plugin.json specifically; not added
  yet (low ROI since `update-version.sh` writes plugin.json
  deterministically).
- **No `ci-gate` job**: prox's `ci.yml` runs `test` / `lint` /
  `release-snapshot` as separate jobs with no aggregator. The
  `release.yaml`'s own `go test` + lint steps serve as the inline gate
  at tag time. Adding ci-gate would require either a `ci-success`
  aggregator in ci.yml or polling multiple check names.
- **`sync-version` job (removed)**: an earlier flow ran
  `scripts/set-version.sh` in CI after the release and bot-pushed the
  bumped `.claude-plugin/plugin.json` back to main. That work moved
  LOCAL via `scripts/release/update-version.sh`, which the release skill
  runs before tagging. Result: main always has plugin.json at the
  released version *before* the release workflow even fires.
