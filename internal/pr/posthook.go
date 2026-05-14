package pr

import (
	"context"
	"fmt"

	"github.com/rancher/release-automation/internal/config"
)

// PostBundleHook runs after the bundle's strategies and the post-bundle tidy
// pass have settled the working tree, before the dirty-check + commit. Each
// implementation must be idempotent — running twice on a tree that already
// satisfies the invariant must produce no diff. The bumper relies on
// `git status` afterward to detect the no-op case.
//
// Hook input lives on Request as typed fields (e.g. SyncModules for
// sync-deps), parallel to how Module carries Strategy-specific data.
type PostBundleHook interface {
	Apply(ctx context.Context, repoDir string, req Request) error
}

// postBundleHooks is the registry the bumper dispatches against.
// config.knownPostBundleHook gates the YAML side; both must agree.
var postBundleHooks = map[config.PostBundleHook]PostBundleHook{
	config.PostBundleSyncDeps: syncDepsHook{},
}

func lookupPostBundleHook(name config.PostBundleHook) (PostBundleHook, error) {
	h, ok := postBundleHooks[name]
	if !ok {
		return nil, fmt.Errorf("unknown post-bundle hook %q", name)
	}
	return h, nil
}
