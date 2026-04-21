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
// case where pass 1 only auto-bumps `main` and the older branches need a
// manual nudge.
//
// Targets are derived, not asked: caller picks one leaf branch and we fan
// out via VERSION.md (leaf direct, paired via Pair-column lookup) — see
// ComputeTargetsForLeafBranch. Independents are skipped (no leaf-branch
// mapping); they need a separate manual run if relevant.
//
// The rest of the pipeline (tracker find-or-create, supersede, PR opening,
// body update) is shared with pass1Dispatch via processBumpOp. mergeState's
// union semantics ensure an existing tracker (e.g. from an earlier
// auto-bump on `main`) keeps its targets.
func (r *Reconciler) RunBumpDep(ctx context.Context, dep, version, leafBranch string) error {
	if !semver.IsValid(version) {
		return fmt.Errorf("invalid version %q (not semver)", version)
	}
	if leafBranch == "" {
		return fmt.Errorf("leaf branch is required")
	}
	if _, ok := r.cfg.Repos[dep]; !ok {
		return fmt.Errorf("unknown dep %q", dep)
	}

	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return fmt.Errorf("expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafRepo := leaves[0]

	leafTable, err := r.fetchVersionTable(ctx, leafRepo)
	if err != nil {
		return fmt.Errorf("load leaf VERSION.md: %w", err)
	}
	dependentTables := make(map[string]*config.VersionTable)
	for _, d := range r.cfg.Dependents(dep) {
		if d == leafRepo || r.cfg.Repos[d].Kind != config.KindPaired {
			continue
		}
		tbl, err := r.fetchVersionTable(ctx, d)
		if err != nil {
			return fmt.Errorf("load %s VERSION.md: %w", d, err)
		}
		dependentTables[d] = tbl
	}

	rawTargets, err := ComputeTargetsForLeafBranch(r.cfg, dep, leafRepo, leafBranch, leafTable, dependentTables)
	if err != nil {
		return fmt.Errorf("compute targets: %w", err)
	}
	if len(rawTargets) == 0 {
		return fmt.Errorf("no targets for %s %s onto %s %s", dep, version, leafRepo, leafBranch)
	}

	if err := r.processBumpOp(ctx, dep, version, rawTargets); err != nil {
		return err
	}
	return r.passes234(ctx)
}
