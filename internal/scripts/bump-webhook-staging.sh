#!/usr/bin/env bash
# bump-webhook-staging.sh — bumps webhook in a locally checked out
# rancher/rancher repo to a chart staged by charts v2 (Rancher 2.16 and
# later), for the bump-webhook-staging strategy.
#
# webhook's release workflow stages the rancher-webhook chart, and
# rancher/charts indexes it on its staging branch. That index holds every
# Rancher release line, so the chart version is the entry whose appVersion is
# the new webhook version and whose catalog.cattle.io/rancher-version starts
# at RANCHER_MINOR (">= 2.17.0-0 < 2.18.0-0" for 2.17). Exactly one entry must
# match. Until the release workflow has staged the chart there is none, the
# script fails, and the reconciler retries on its next run.
#
# Bumper contract: no git operations (the bumper commits once after every
# script in the bundle), and exit 0 without changes when build.yaml already
# carries the version (the bumper detects the no-op with `git status`).
#
# Usage: RANCHER_MINOR=<X.Y> bump-webhook-staging.sh <new-version>
#   e.g. RANCHER_MINOR=2.17 bump-webhook-staging.sh v0.13.0-rc.11
#
# Required tools in PATH: yq (v4), curl, go (for `go generate`).
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: RANCHER_MINOR=<X.Y> $0 <new-webhook-version>" >&2
    exit 2
fi
NEW_VERSION="$1"
RANCHER_MINOR="${RANCHER_MINOR:-}"

if ! echo "$NEW_VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'; then
    echo "Error: version $NEW_VERSION must be vX.Y.Z or vX.Y.Z-rc.N" >&2
    exit 1
fi
if ! echo "$RANCHER_MINOR" | grep -qE '^[0-9]+\.[0-9]+$'; then
    echo "Error: RANCHER_MINOR env var must be the Rancher minor as X.Y (e.g. 2.17), got '${RANCHER_MINOR}'" >&2
    exit 1
fi

NEW_SHORT="${NEW_VERSION#v}"

# rancher/charts is public, so no auth is needed for raw.githubusercontent.com.
INDEX_URL="https://raw.githubusercontent.com/rancher/charts/staging/index.yaml"
MATCHES=$(curl -fsSL "$INDEX_URL" \
    | yq -r ".entries.\"rancher-webhook\"[]
        | select(.appVersion == \"${NEW_SHORT}\")
        | select((.annotations.\"catalog.cattle.io/rancher-version\" // \"\") | sub(\" \"; \"\") | test(\"^>=${RANCHER_MINOR}[.]\"))
        | .version")
if [[ -z "$MATCHES" ]]; then
    echo "Error: no rancher-webhook entry with appVersion=${NEW_SHORT} for Rancher ${RANCHER_MINOR} in ${INDEX_URL} — wait for webhook's release workflow to stage the chart" >&2
    exit 1
fi
if [[ $(echo "$MATCHES" | wc -l) -gt 1 ]]; then
    echo "Error: more than one rancher-webhook entry with appVersion=${NEW_SHORT} for Rancher ${RANCHER_MINOR} in ${INDEX_URL}:" >&2
    echo "$MATCHES" >&2
    exit 1
fi
NEW_FULL="$MATCHES"

if [[ ! -f ./build.yaml ]]; then
    echo "Error: ./build.yaml not found — run from a rancher repo root" >&2
    exit 1
fi

PREV_FULL=$(yq -r '.webhookVersion' ./build.yaml)
if [[ -z "$PREV_FULL" || "$PREV_FULL" == "null" ]]; then
    echo "Error: .webhookVersion missing from build.yaml" >&2
    cat ./build.yaml >&2
    exit 1
fi
if [[ "$PREV_FULL" == "$NEW_FULL" ]]; then
    echo "bump-webhook-staging: already at $NEW_FULL; nothing to do"
    exit 0
fi

yq --inplace ".webhookVersion = \"${NEW_FULL}\"" ./build.yaml

go generate ./...

echo "bump-webhook-staging: bumped to ${NEW_FULL}"
