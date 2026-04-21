package reconcile

import (
	"fmt"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
)

// Target identifies a single bump-PR slot: one branch in one downstream repo.
type Target struct {
	Repo   string // config key, e.g. "rancher"
	Branch string // e.g. "main", "release/v2.13"
}

// ComputeTargets is the pure heart of pass 1: given a release of `dep` at
// `version`, return the downstream branches that should receive a bump PR.
//
// `depTable` is the dep's own VERSION.md (used by the paired strategy to map
// dep.minor -> rancher.minor). `downstreamTables` provides each downstream's
// VERSION.md so we can resolve a rancher minor back to the branch hosting it.
//
// Strategy by dep.Kind:
//
//	leaf        nothing propagates from a leaf — returns no targets.
//	paired      target the downstream branch whose VERSION.md row pairs
//	            (column 3, "Pair") with the same rancher minor as dep's
//	            minor. For pilot 1: steve v0.7.5 (minor v0.7) -> Pair v2.13
//	            -> rancher branch release/v2.13.
//	independent target downstream's main only. release/* branches are
//	            notify-only (deferred until the notifications layer lands).
//
// The downstream's classification is irrelevant — the dep drives strategy.
func ComputeTargets(
	cfg *config.Config,
	dep, version string,
	depTable *config.VersionTable,
	downstreamTables map[string]*config.VersionTable,
) ([]Target, error) {
	r, ok := cfg.Repos[dep]
	if !ok {
		return nil, fmt.Errorf("dep %q not in config", dep)
	}
	dependents := cfg.Dependents(dep)
	if len(dependents) == 0 {
		return nil, nil
	}

	switch r.Kind {
	case config.KindLeaf:
		return nil, nil

	case config.KindIndependent:
		out := make([]Target, 0, len(dependents))
		for _, d := range dependents {
			out = append(out, Target{Repo: d, Branch: "main"})
		}
		return out, nil

	case config.KindPaired:
		if depTable == nil {
			return nil, fmt.Errorf("paired dep %q: missing VERSION.md table", dep)
		}
		minor := semver.MajorMinor(version) // "v0.7.5" -> "v0.7"
		if minor == "" {
			return nil, fmt.Errorf("invalid semver %q", version)
		}
		pair := depTable.LookupPair(minor)
		if pair == "" {
			return nil, fmt.Errorf("paired dep %q: minor %s not in VERSION.md", dep, minor)
		}
		out := make([]Target, 0, len(dependents))
		for _, d := range dependents {
			t, ok := downstreamTables[d]
			if !ok || t == nil {
				return nil, fmt.Errorf("paired downstream %q: missing VERSION.md table", d)
			}
			branch := t.BranchForMinor(pair)
			if branch == "" {
				// Downstream doesn't host that minor (yet). Skip — not an
				// error: e.g. rancher hasn't cut release/v2.13 yet.
				continue
			}
			out = append(out, Target{Repo: d, Branch: branch})
		}
		return out, nil

	default:
		return nil, fmt.Errorf("dep %q: unknown kind %q", dep, r.Kind)
	}
}

// ComputeTargetsForLeafBranch is the inverse axis: caller picks a leaf
// branch (e.g. rancher release/v2.13) and we fan out to every dependent of
// `dep` that ships against that leaf branch. Used by the manual `Bump X`
// workflows for the kind=independent older-branch case where the auto path
// in pass 1 would only touch `main`.
//
// Mapping per dependent of `dep`:
//
//	leaf itself          target leafBranch directly.
//	paired downstream    look up the row in its VERSION.md whose Pair column
//	                     matches the leaf's minor for leafBranch; that row's
//	                     Branch is the target. If no such row exists the
//	                     dependent is skipped (it doesn't ship against this
//	                     leaf line yet).
//	independent          skipped — independents have no per-leaf-branch
//	                     mapping. The user must run a separate manual bump
//	                     for each branch they want to land on.
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
		if d == leafRepo {
			out = append(out, Target{Repo: d, Branch: leafBranch})
			continue
		}
		switch cfg.Repos[d].Kind {
		case config.KindPaired:
			tbl, ok := dependentTables[d]
			if !ok || tbl == nil {
				return nil, fmt.Errorf("paired dependent %q: missing VERSION.md table", d)
			}
			branch := tbl.BranchForPair(leafMinor)
			if branch == "" {
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
