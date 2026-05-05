#!/usr/bin/env bash
# bump-webhook.sh — bumps webhook in a locally checked out rancher/rancher
# repo. Adapted from rancher/webhook's
# .github/workflows/scripts/release-against-rancher.sh, with three changes
# to fit the release-automation bumper's contract:
#
#  1. Single argument (new version). Previous version is derived from
#     build.yaml so the caller doesn't need to know it.
#  2. No git operations. The bumper does one final `git add -A && git commit`
#     after every script in the bundle has run.
#  3. When the previous and new versions match, exit 0 silently. The bumper
#     uses `git status` to detect the no-op case and skips the PR.
#
# Usage: bump-webhook.sh <new-version>
#   e.g. bump-webhook.sh v0.5.0-rc.14
#
# Required tools in PATH: yq (v4), go (for `go generate`).
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

if [[ ! -f ./build.yaml ]]; then
    echo "Error: ./build.yaml not found — run from a rancher repo root" >&2
    exit 1
fi

# build.yaml carries '<chart>+up<webhook>' (e.g. "108.0.2+up0.5.0-rc.13").
PREV_FULL=$(yq -r '.webhookVersion' ./build.yaml)
if [[ -z "$PREV_FULL" || "$PREV_FULL" == "null" ]]; then
    echo "Error: .webhookVersion missing from build.yaml" >&2
    cat ./build.yaml >&2
    exit 1
fi
PREV_SHORT="${PREV_FULL##*+up}"
PREV_CHART_VERSION="${PREV_FULL%%+*}"

if [[ "$PREV_SHORT" == "$NEW_SHORT" ]]; then
    echo "bump-webhook: already at $NEW_VERSION; nothing to do"
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

yq --inplace ".webhookVersion = \"${NEW_CHART_VERSION}+up${NEW_SHORT}\"" ./build.yaml

go generate ./...

echo "bump-webhook: bumped to ${NEW_CHART_VERSION}+up${NEW_SHORT}"
