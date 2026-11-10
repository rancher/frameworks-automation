package reconcile

import (
	"context"
	"fmt"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
)

// strategyUsesChartRef reports whether `s` is a rancher-side script
// strategy that needs CHART_REF to look up a chart-prefixed version from
// rancher/charts' index.yaml.
func strategyUsesChartRef(s config.Strategy) bool {
	return s == config.StrategyBumpWebhook || s == config.StrategyBumpRemotedialerProxy
}

// strategyUsesChartMajor reports whether `s` is a chart-side script strategy
// that needs CHART_MAJOR to pick the rancher/charts major version.
func strategyUsesChartMajor(s config.Strategy) bool {
	return s == config.StrategyChartBumpWebhook || s == config.StrategyChartBumpRemotedialerProxy
}

// chartMajorForDep returns the rancher/charts major version (e.g. "112") for
// `dep` at `version`, read from the "Chart Major Version" column of dep's
// default-branch VERSION.md, using the row whose minor matches version's.
//
// Returns ("", nil) when dep's VERSION.md has no chart-major column, so the
// chart-bump scripts keep their previous behaviour for repos that haven't
// added it yet. Returns an error when version's minor isn't in the table.
func (r *Reconciler) chartMajorForDep(ctx context.Context, dep, version string) (string, error) {
	minor := semver.MajorMinor(version)
	if minor == "" {
		return "", fmt.Errorf("invalid semver %q", version)
	}
	tbl, err := r.fetchVersionTable(ctx, dep)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", dep, err)
	}
	major, ok := tbl.LookupChartMajor(minor)
	if !ok {
		return "", fmt.Errorf("%s minor %s not in VERSION.md", dep, minor)
	}
	return major, nil
}

// chartRefForLeaf returns the rancher/charts branch corresponding to the
// given leaf-rancher branch, for use as CHART_REF by rancher-side bump
// scripts. The chart repo is identified by scanning the config for any repo
// that declares a chart-side bump strategy in its deps; this avoids
// hardcoding the chart's config-key name.
//
// Returns ("", nil) when the config declares no chart-side strategy (no
// chart bumping in this DAG, so no lookup is needed). Returns an error when
// the leaf branch isn't in rancher's VERSION.md or chart-branch resolution
// otherwise fails.
func (r *Reconciler) chartRefForLeaf(ctx context.Context, leafBranch string) (string, error) {
	chartRepoName, chartCfg, ok := r.findChartRepo()
	if !ok {
		return "", nil
	}
	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return "", fmt.Errorf("expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafTable, err := r.fetchVersionTable(ctx, leaves[0])
	if err != nil {
		return "", fmt.Errorf("fetch leaf VERSION.md: %w", err)
	}
	leafMinor := leafTable.LookupMinor(leafBranch)
	if leafMinor == "" {
		return "", fmt.Errorf("leaf branch %q not in VERSION.md", leafBranch)
	}
	var chartTable *config.VersionTable
	if chartCfg.BranchTemplate == "" {
		chartTable, err = r.fetchVersionTable(ctx, chartRepoName)
		if err != nil {
			return "", fmt.Errorf("fetch %s VERSION.md: %w", chartRepoName, err)
		}
	}
	branch, err := chartCfg.ResolveBranch(leafMinor, chartTable)
	if err != nil {
		return "", fmt.Errorf("resolve %s branch for leaf minor %s: %w", chartRepoName, leafMinor, err)
	}
	return branch, nil
}

// findChartRepo returns the (config-key, repo) of whichever repo declares a
// chart-side bump strategy. Returns ok=false when none does.
func (r *Reconciler) findChartRepo() (string, config.Repo, bool) {
	for name, repo := range r.cfg.Repos {
		for _, d := range repo.Deps {
			if d.Strategy == config.StrategyChartBumpWebhook ||
				d.Strategy == config.StrategyChartBumpRemotedialerProxy {
				return name, repo, true
			}
		}
	}
	return "", config.Repo{}, false
}
