#!/usr/bin/env bash
# Bump prox's release version.
#
# Prox is a Go binary (distributed via tarballs + .debs + Homebrew tap),
# AND a Claude Code plugin (.claude-plugin/plugin.json). Two version
# surfaces:
#
#   * Go binary version → comes from a build-time ldflag
#     (-X .../internal/version.Version={{.Version}}) injected by
#     GoReleaser. NOT bumped in the source tree.
#
#   * Claude Code plugin version → lives in .claude-plugin/plugin.json,
#     read by Claude Code at install/update time. IS a source-tree
#     manifest that must be bumped to match the released tag.
#
# This script bumps plugin.json by delegating to scripts/set-version.sh
# (the existing, tested in-repo bumper). The convention requires the
# script to live at scripts/release/update-version.sh; this thin wrapper
# preserves the existing scripts/set-version.sh entry point.
#
# Contract (see cc-plugins:release-workflows references/update-version/README.md):
#   - one arg: semver string, no `v` prefix
#   - idempotent
#   - no network
#   - verifies its own work (scripts/set-version.sh does)
#   - doesn't `git add` (release skill stages + commits)

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <X.Y.Z>   e.g. $0 0.1.2" >&2
  exit 2
fi
V="$1"

if [[ ! "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  echo "error: '$V' is not semver (X.Y.Z or X.Y.Z-suffix)" >&2
  exit 2
fi

# Delegate to the existing plugin.json bumper.
"$(dirname "$0")/../set-version.sh" "$V"
