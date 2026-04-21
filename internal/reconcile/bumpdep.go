package reconcile

import (
	"context"
	"fmt"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
)

// RunBumpDep is the manual-bump entrypoint. The per-dep `bump-X.yaml`
// workflow in this repo invokes the reconciler with -mode=bump-dep to land a
// dep version across every repo that ships against a chosen leaf branch
// (e.g. rancher release/v2.13). Used for the kind=independent older-branch
// case where the auto-dispatch path only touches `main`.
//
// Restricted to kind=independent: paired deps' leaf branch is fully
// determined by VERSION.md, so a manual leaf-branch override would let the
// user create a tracker that doesn't match the real pair. For paired deps,
// retrigger the dispatch path instead (or use the per-repo Release
// workflow on the right branch).
//
// The actual pipeline (target derivation, tracker lifecycle, PR opening)
// is shared with pass1Dispatch via runBump.
func (r *Reconciler) RunBumpDep(ctx context.Context, dep, version, leafBranch string) error {
	if !semver.IsValid(version) {
		return fmt.Errorf("invalid version %q (not semver)", version)
	}
	if leafBranch == "" {
		return fmt.Errorf("leaf branch is required")
	}
	depRepo, ok := r.cfg.Repos[dep]
	if !ok {
		return fmt.Errorf("unknown dep %q", dep)
	}
	if depRepo.Kind != config.KindIndependent {
		return fmt.Errorf("bump-dep is only valid for kind=independent deps; %q is %s — use the dispatch path", dep, depRepo.Kind)
	}

	if err := r.runBump(ctx, dep, version, leafBranch); err != nil {
		return err
	}
	return r.passes234(ctx)
}
