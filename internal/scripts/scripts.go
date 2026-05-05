// Package scripts ships bump-strategy scripts as compiled-in assets.
//
// Per-strategy scripts (chart-bump, bump-webhook) live alongside this file
// and are embedded into the reconciler binary so they travel with the build
// instead of needing to exist in the downstream repo. At bump time, the
// internal/pr strategy materializes the embedded bytes to a temp file and
// runs that against a clone of the downstream — the script's working
// directory is the downstream tree, so it edits files there.
//
// To add a new script:
//  1. Drop it in this directory.
//  2. Add an exported var here with a //go:embed directive pointing at it.
//  3. Wire it into the registry in internal/pr/strategy.go.
package scripts

import _ "embed"

//go:embed chart-bump.sh
var ChartBump string

//go:embed bump-webhook.sh
var BumpWebhook string

//go:embed bump-remotedialer-proxy.sh
var BumpRemotedialerProxy string

//go:embed chart-bump-remotedialer-proxy.sh
var ChartBumpRemotedialerProxy string
