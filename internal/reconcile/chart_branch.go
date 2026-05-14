package reconcile

import (
	"context"
	"fmt"

	"github.com/rancher/release-automation/internal/config"
)

// strategyUsesChartBranch reports whether `s` is a rancher-side script
// strategy that needs CHART_BRANCH to look up a chart-prefixed version from
// rancher/charts' index.yaml.
func strategyUsesChartBranch(s config.Strategy) bool {
	return s == config.StrategyBumpWebhook || s == config.StrategyBumpRemotedialerProxy
}

// chartBranchForLeaf returns the rancher/charts branch corresponding to the
// given leaf-rancher branch, for use as CHART_BRANCH by rancher-side bump
// scripts. The chart repo is identified by scanning the config for any repo
// that declares a chart-side bump strategy in its deps; this avoids
// hardcoding the chart's config-key name.
//
// Returns ("", nil) when the config declares no chart-side strategy (no
// chart bumping in this DAG, so no lookup is needed). Returns an error when
// the leaf branch isn't in rancher's VERSION.md or chart-branch resolution
// otherwise fails.
func (r *Reconciler) chartBranchForLeaf(ctx context.Context, leafBranch string) (string, error) {
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
