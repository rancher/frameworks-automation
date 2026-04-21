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
