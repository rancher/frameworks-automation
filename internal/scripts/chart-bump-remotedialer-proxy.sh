#!/usr/bin/env bash
# chart-bump-remotedialer-proxy.sh — bumps remotedialer-proxy in a locally
# checked out rancher/charts repo. Adapted from rancher/remotedialer-proxy's
# .github/workflows/scripts/release-against-charts.sh, with three changes
# to fit the release-automation bumper's contract:
#
#  1. Single argument (new version). Previous version is derived from
#     packages/remotedialer-proxy/package.yaml's `url` field.
#  2. No git operations. The bumper does one final `git add -A && git commit`
#     after every script in the bundle has run.
#  3. When the previous and new versions match, exit 0 silently. The bumper
#     uses `git status` to detect the no-op case and skips the PR. The
#     release.yaml prepend is also gated on "entry not already present" so
#     a re-run produces no diff.
#
# Usage: chart-bump-remotedialer-proxy.sh <new-version>
#   e.g. chart-bump-remotedialer-proxy.sh v0.7.1
#
# Required tools in PATH: yq (v4), make, sed, grep.
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <new-remotedialer-proxy-version>" >&2
    exit 2
fi
NEW_VERSION="$1"

if ! echo "$NEW_VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'; then
    echo "Error: version $NEW_VERSION must be vX.Y.Z or vX.Y.Z-rc.N" >&2
    exit 1
fi

NEW_SHORT="${NEW_VERSION#v}"

PKG=./packages/remotedialer-proxy/package.yaml
if [[ ! -f "$PKG" ]]; then
    echo "Error: $PKG not found — run from a charts repo root" >&2
    exit 1
fi

# package.yaml shape:
#   url: https://github.com/rancher/remotedialer-proxy/releases/download/v0.6.0/remotedialer-proxy-0.6.0.tgz
#   version: 106.0.2
PREV_URL=$(yq -r '.url' "$PKG")
PREV_VERSION=$(echo "$PREV_URL" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?' | head -n1)
if [[ -z "$PREV_VERSION" ]]; then
    echo "Error: could not extract previous remotedialer-proxy version from $PKG url=$PREV_URL" >&2
    exit 1
fi
PREV_SHORT="${PREV_VERSION#v}"

PREV_CHART_VERSION=$(yq -r '.version' "$PKG")
if [[ -z "$PREV_CHART_VERSION" || "$PREV_CHART_VERSION" == "null" ]]; then
    echo "Error: .version missing from $PKG" >&2
    exit 1
fi

if [[ "$PREV_SHORT" == "$NEW_SHORT" ]]; then
    echo "chart-bump-remotedialer-proxy: already at $NEW_VERSION; nothing to do"
    exit 0
fi

bump_patch() {
    local v="$1"
    local major minor patch
    major="${v%%.*}"
    local rest="${v#*.}"
    minor="${rest%%.*}"
    patch="${rest#*.}"
    echo "${major}.${minor}.$((patch + 1))"
}

if echo "$PREV_SHORT" | grep -q -- '-rc'; then
    NEW_CHART_VERSION="$PREV_CHART_VERSION"
else
    NEW_CHART_VERSION=$(bump_patch "$PREV_CHART_VERSION")
fi

sed -i "s/${PREV_SHORT}/${NEW_SHORT}/g" "$PKG"
if [[ "$PREV_CHART_VERSION" != "$NEW_CHART_VERSION" ]]; then
    sed -i "s/${PREV_CHART_VERSION}/${NEW_CHART_VERSION}/g" "$PKG"
fi

# `make charts` invokes pull-scripts, which expects origin/automation-core
# to exist as a local tracking ref. The bumper clones with
# `--depth=1 --branch=<branch>` (single-branch by default), so without
# this fetch the pull-scripts step finds automation-core in FETCH_HEAD
# only and fails with "invalid object name 'origin/automation-core'".
git fetch --depth=1 origin "+refs/heads/automation-core:refs/remotes/origin/automation-core"

PACKAGE=remotedialer-proxy make charts

# Idempotent prepend: only insert if release.yaml doesn't already list this
# version. The original workflow script unconditionally prepends, which is
# fine for a one-shot CI run but not for the bumper, which may legitimately
# re-execute the same target.
ENTRY="${NEW_CHART_VERSION}+up${NEW_SHORT}"
if ! yq -e ".remotedialer-proxy[] | select(. == \"${ENTRY}\")" release.yaml >/dev/null 2>&1; then
    yq --inplace ".remotedialer-proxy = [\"${ENTRY}\"] + .remotedialer-proxy" release.yaml
fi

echo "chart-bump-remotedialer-proxy: bumped to ${ENTRY}"
