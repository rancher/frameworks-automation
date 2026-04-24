#!/usr/bin/env bash
# bump-webhook.sh — playground stand-in for the rancher-side webhook bump
# script. Invoked by the release-automation bumper inside a clone of the
# rancher downstream; produces a small diff so the bumper has something to
# commit and open a PR for.
#
# Idempotent: re-running with the same version produces the same file
# contents, so the bumper's `git status` no-op detection works.
#
# Usage: ./hack/bump-webhook.sh <webhook-version>
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <webhook-version>" >&2
    exit 2
fi
version="$1"

cat > .webhook-version <<EOF
# Managed by release-automation. Do not edit manually.
webhook=${version}
EOF

echo "bump-webhook: pinned webhook to ${version}"
