#!/usr/bin/env bash
# bump-remotedialer-proxy.sh — bumps remotedialer-proxy in a locally checked
# out rancher/rancher repo. Adapted from rancher/remotedialer-proxy's
# .github/workflows/scripts/release-against-rancher.sh, with these changes
# to fit the release-automation bumper's contract:
#
#  1. Single positional argument (new version). The chart-prefixed full
#     version is looked up at runtime from rancher/charts' index.yaml on
#     the branch named by the CHART_BRANCH env var. The chart bump must
#     have merged into rancher/charts before this script runs (cascade
#     enforces that via the `order` edge).
#  2. No git operations. The bumper does one final `git add -A && git commit`
#     after every script in the bundle has run.
#  3. When the previous and new full versions match, exit 0 silently. The
#     bumper uses `git status` to detect the no-op case and skips the PR.
#
# Usage: bump-remotedialer-proxy.sh <new-version>
#   e.g. CHART_BRANCH=dev-v2.15 bump-remotedialer-proxy.sh v0.8.0-rc.4
#
# Required tools in PATH: yq (v4), curl, go (for `go generate`).
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <new-remotedialer-proxy-version>" >&2
    exit 2
fi
NEW_VERSION="$1"
CHART_BRANCH="${CHART_BRANCH:-}"

if ! echo "$NEW_VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'; then
    echo "Error: version $NEW_VERSION must be vX.Y.Z or vX.Y.Z-rc.N" >&2
    exit 1
fi
if [[ -z "$CHART_BRANCH" ]]; then
    echo "Error: CHART_BRANCH env var must be set to the rancher/charts branch (e.g. dev-v2.15)" >&2
    exit 1
fi

NEW_SHORT="${NEW_VERSION#v}"

# Resolve `<chart>+up<dep>` from rancher/charts' index.yaml. rancher/charts is
# public, so no auth is needed for raw.githubusercontent.com.
INDEX_URL="https://raw.githubusercontent.com/rancher/charts/refs/heads/${CHART_BRANCH}/index.yaml"
NEW_FULL=$(curl -fsSL "$INDEX_URL" \
    | yq -r ".entries.\"remotedialer-proxy\"[] | select(.appVersion == \"${NEW_SHORT}\") | .version" \
    | head -n1)
if [[ -z "$NEW_FULL" ]]; then
    echo "Error: no remotedialer-proxy entry with appVersion=${NEW_SHORT} in ${INDEX_URL} — wait for the chart bump to merge first" >&2
    exit 1
fi

if [[ ! -f ./build.yaml ]]; then
    echo "Error: ./build.yaml not found — run from a rancher repo root" >&2
    exit 1
fi

PREV_FULL=$(yq -r '.remoteDialerProxyVersion' ./build.yaml)
if [[ -z "$PREV_FULL" || "$PREV_FULL" == "null" ]]; then
    echo "Error: .remoteDialerProxyVersion missing from build.yaml" >&2
    cat ./build.yaml >&2
    exit 1
fi
if [[ "$PREV_FULL" == "$NEW_FULL" ]]; then
    echo "bump-remotedialer-proxy: already at $NEW_FULL; nothing to do"
    exit 0
fi

yq --inplace ".remoteDialerProxyVersion = \"${NEW_FULL}\"" ./build.yaml

go generate ./...

echo "bump-remotedialer-proxy: bumped to ${NEW_FULL}"
