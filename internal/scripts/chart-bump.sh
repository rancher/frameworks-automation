#!/usr/bin/env bash
# chart-bump.sh — playground stand-in for the real `make upgrade <version>`
# that the chart repo would expose. Invoked by the release-automation
# bumper inside a clone of the chart downstream; produces a small diff so
# the bumper has something to commit and open a PR for.
#
# Idempotent: re-running with the same version produces the same file
# contents, so the bumper's `git status` no-op detection works.
#
# Usage: ./hack/chart-bump.sh <webhook-version>
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <webhook-version>" >&2
    exit 2
fi
version="$1"

# Single source of truth for the diff: rewrite a marker file. Any prior
# content is replaced, so re-running with the same version is idempotent.
cat > .webhook-version <<EOF
# Managed by release-automation. Do not edit manually.
webhook=${version}
EOF

echo "chart-bump: pinned webhook to ${version}"
