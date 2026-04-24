package reconcile

import (
	"fmt"

	"github.com/rancher/release-automation/internal/config"
)

// Target identifies a single bump-PR slot: one branch in one downstream repo.
type Target struct {
	Repo   string // config key, e.g. "rancher"
	Branch string // e.g. "main", "release/v2.13"
}

// ComputeTargetsForLeafBranch fans a (dep, version) bump out to every repo
// that ships against `leafBranch`. The single target-derivation function:
// both the auto-dispatch path (pass 1 derives leafBranch from dep.minor →
// pair → leaf branch) and the manual `Bump <dep>` workflow (caller supplies
// leafBranch) feed into it.
//
// Mapping per dependent of `dep`:
//
//	leaf itself          target leafBranch directly.
//	paired downstream    look up the row in its VERSION.md whose Pair column
//	                     matches the leaf's minor for leafBranch; that row's
//	                     Branch is the target. If no such row exists the
//	                     dependent is skipped (it doesn't ship against this
//	                     leaf line yet).
//	independent          skipped — independents have no leaf-branch
//	                     pairing. They get their own auto-dispatch and
//	                     manual `Bump <dep>` runs to land releases.
func ComputeTargetsForLeafBranch(
	cfg *config.Config,
	dep, leafRepo, leafBranch string,
	leafTable *config.VersionTable,
	dependentTables map[string]*config.VersionTable,
) ([]Target, error) {
	if _, ok := cfg.Repos[dep]; !ok {
		return nil, fmt.Errorf("dep %q not in config", dep)
	}
	leaf, ok := cfg.Repos[leafRepo]
	if !ok {
		return nil, fmt.Errorf("leaf %q not in config", leafRepo)
	}
	if leaf.Kind != config.KindLeaf {
		return nil, fmt.Errorf("repo %q is not a leaf", leafRepo)
	}
	if leafTable == nil {
		return nil, fmt.Errorf("leaf %q: missing VERSION.md table", leafRepo)
	}
	leafMinor := leafTable.LookupMinor(leafBranch)
	if leafMinor == "" {
		return nil, fmt.Errorf("leaf %q: branch %q not in VERSION.md", leafRepo, leafBranch)
	}

	dependents := cfg.Dependents(dep)
	out := make([]Target, 0, len(dependents))
	for _, d := range dependents {
		// Order edges sequence the cascade DAG; they don't trigger a bump-PR
		// on their own when a new release of `dep` lands. Skip silently —
		// the cascade is what drives ordering-only edges.
		if cfg.Repos[d].DepStrategy(dep) == config.StrategyOrder {
			continue
		}
		if d == leafRepo {
			out = append(out, Target{Repo: d, Branch: leafBranch})
			continue
		}
		switch cfg.Repos[d].Kind {
		case config.KindPaired:
			branch, err := cfg.Repos[d].ResolveBranch(leafMinor, dependentTables[d])
			if err != nil {
				return nil, fmt.Errorf("paired dependent %q: %w", d, err)
			}
			if branch == "" {
				// No row pairs this dep's branch to the leaf minor — the
				// dependent doesn't ship against this leaf line yet. Skip.
				continue
			}
			out = append(out, Target{Repo: d, Branch: branch})
		case config.KindIndependent, config.KindLeaf:
			// independents have no leaf-branch mapping; another leaf is not
			// expected in the current single-leaf DAG.
			continue
		}
	}
	return out, nil
}
