#!/usr/bin/env bash
# chart-bump-webhook.sh — bumps webhook in a locally checked out
# rancher/charts repo. Adapted from rancher/webhook's
# .github/workflows/scripts/release-against-charts.sh, with three changes
# to fit the release-automation bumper's contract:
#
#  1. Single argument (new version). Previous version is derived from
#     packages/rancher-webhook/package.yaml's `url` field.
#  2. No git operations. The bumper does one final `git add -A && git commit`
#     after every script in the bundle has run.
#  3. When the previous and new versions match, exit 0 silently. The bumper
#     uses `git status` to detect the no-op case and skips the PR. The
#     release.yaml prepend is also gated on "entry not already present" so
#     a re-run produces no diff.
#
# Note: the chart package is named `rancher-webhook` (with the rancher-
# prefix), even though the dep name in dependencies.yaml is `webhook`.
#
# Chart version: <major>.0.<webhook patch>, the formula of rancher/webhook's
# .github/workflows/scripts/prepare-chart.sh, so the chart bumped here and the
# chart webhook stages for the same tag have the same version.
# The major is CHART_MAJOR (from the "Chart Major Version" column of
# rancher/webhook's VERSION.md) when package.yaml is on an older major — e.g. a
# freshly cut dev-v2.X branch still carrying dev-v2.X-1's version — and
# package.yaml's major otherwise. A package.yaml major newer than CHART_MAJOR
# is an error, and so is a version older than package.yaml's on the same major
# (a branch whose old counter is ahead of the upstream patch).
#
# Usage: chart-bump-webhook.sh <new-version>
#   e.g. chart-bump-webhook.sh v0.5.0-rc.14
#
# Optional env: CHART_MAJOR (e.g. 112).
# Required tools in PATH: yq (v4), make, sed, grep.
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <new-webhook-version>" >&2
    exit 2
fi
NEW_VERSION="$1"

if ! echo "$NEW_VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'; then
    echo "Error: version $NEW_VERSION must be vX.Y.Z or vX.Y.Z-rc.N" >&2
    exit 1
fi

NEW_SHORT="${NEW_VERSION#v}"

PKG=./packages/rancher-webhook/package.yaml
if [[ ! -f "$PKG" ]]; then
    echo "Error: $PKG not found — run from a charts repo root" >&2
    exit 1
fi

# package.yaml shape:
#   url: https://github.com/rancher/webhook/releases/download/v0.9.2-rc.2/rancher-webhook-0.9.2-rc.2.tgz
#   version: 108.0.2
PREV_URL=$(yq -r '.url' "$PKG")
PREV_VERSION=$(echo "$PREV_URL" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?' | head -n1)
if [[ -z "$PREV_VERSION" ]]; then
    echo "Error: could not extract previous webhook version from $PKG url=$PREV_URL" >&2
    exit 1
fi
PREV_SHORT="${PREV_VERSION#v}"

PREV_CHART_VERSION=$(yq -r '.version' "$PKG")
if [[ -z "$PREV_CHART_VERSION" || "$PREV_CHART_VERSION" == "null" ]]; then
    echo "Error: .version missing from $PKG" >&2
    exit 1
fi

if [[ "$PREV_SHORT" == "$NEW_SHORT" ]]; then
    echo "chart-bump-webhook: already at $NEW_VERSION; nothing to do"
    exit 0
fi

CHART_MAJOR="${CHART_MAJOR:-}"
if [[ -n "$CHART_MAJOR" ]] && ! [[ "$CHART_MAJOR" =~ ^[0-9]+$ ]]; then
    echo "Error: CHART_MAJOR=$CHART_MAJOR must be a number" >&2
    exit 1
fi
PREV_CHART_MAJOR="${PREV_CHART_VERSION%%.*}"

if [[ -n "$CHART_MAJOR" ]] && (( PREV_CHART_MAJOR > CHART_MAJOR )); then
    echo "Error: $PKG chart version $PREV_CHART_VERSION is newer than CHART_MAJOR=$CHART_MAJOR" >&2
    exit 1
elif [[ -n "$CHART_MAJOR" ]] && (( PREV_CHART_MAJOR < CHART_MAJOR )); then
    NEW_CHART_MAJOR="$CHART_MAJOR"
else
    NEW_CHART_MAJOR="$PREV_CHART_MAJOR"
fi

# <major>.0.<webhook patch>, like rancher/webhook's prepare-chart.sh does for
# the chart it stages, so both carry the same version for the same tag.
NEW_PATCH="${NEW_SHORT%%-*}"
NEW_PATCH="${NEW_PATCH##*.}"
NEW_CHART_VERSION="${NEW_CHART_MAJOR}.0.${NEW_PATCH}"
if (( NEW_CHART_MAJOR == PREV_CHART_MAJOR )) \
    && [[ "$(printf '%s\n' "$PREV_CHART_VERSION" "$NEW_CHART_VERSION" | sort -V | head -n1)" != "$PREV_CHART_VERSION" ]]; then
    echo "Error: chart version $NEW_CHART_VERSION would be older than $PKG chart version $PREV_CHART_VERSION" >&2
    exit 1
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

PACKAGE=rancher-webhook make charts

# Idempotent prepend: only insert if release.yaml doesn't already list this
# version. Key is `rancher-webhook` (with hyphen) so it must be quoted in
# the yq path.
ENTRY="${NEW_CHART_VERSION}+up${NEW_SHORT}"
if ! yq -e ".\"rancher-webhook\"[] | select(. == \"${ENTRY}\")" release.yaml >/dev/null 2>&1; then
    yq --inplace ".\"rancher-webhook\" = [\"${ENTRY}\"] + .\"rancher-webhook\"" release.yaml
fi

echo "chart-bump-webhook: bumped to ${ENTRY}"
