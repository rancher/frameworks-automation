package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/rancher/release-automation/internal/dashboard"
	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass4Dashboards re-renders one issue per (leaf-repo, branch). Each tick
// pulls every open bump-op tracker, filters to the ones whose targets
// include this leaf+branch, and overwrites the dashboard body with that
// rolled-up view.
//
// The dashboard is read-only aggregation — no embedded state, no metadata
// block. Closing trackers naturally drop off the next render.
func (r *Reconciler) pass4Dashboards(ctx context.Context) error {
	leaves := r.cfg.LeafRepos()
	if len(leaves) == 0 {
		return nil
	}
	openTrackers, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo, []string{tracker.LabelOp})
	if err != nil {
		return fmt.Errorf("list trackers: %w", err)
	}
	for _, leaf := range leaves {
		if err := r.refreshLeafDashboards(ctx, leaf, openTrackers); err != nil {
			log.Printf("pass4: leaf %s: %v", leaf, err)
		}
	}
	return nil
}

func (r *Reconciler) refreshLeafDashboards(ctx context.Context, leaf string, openTrackers []*ghclient.Issue) error {
	tbl, err := r.fetchVersionTable(ctx, leaf)
	if err != nil {
		return fmt.Errorf("fetch %s VERSION.md: %w", leaf, err)
	}
	for _, row := range tbl.Rows {
		items, err := itemsForLeafBranch(leaf, row.Branch, openTrackers)
		if err != nil {
			return fmt.Errorf("build items for %s %s: %w", leaf, row.Branch, err)
		}
		if err := r.writeDashboard(ctx, leaf, row.Branch, items); err != nil {
			log.Printf("pass4: write dashboard %s %s: %v", leaf, row.Branch, err)
		}
	}
	return nil
}

func (r *Reconciler) writeDashboard(ctx context.Context, leaf, branch string, items []dashboard.Item) error {
	labels := dashboard.Labels(leaf, branch)
	body := dashboard.Render(leaf, branch, items, time.Now())

	existing, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo, labels)
	if err != nil {
		return fmt.Errorf("find dashboard: %w", err)
	}
	if len(existing) > 0 {
		return r.gh.UpdateIssueBody(ctx, r.settings.AutomationRepo, existing[0].Number, body)
	}
	_, err = r.gh.CreateIssue(ctx, r.settings.AutomationRepo, dashboard.Title(leaf, branch), body, labels)
	return err
}

// itemsForLeafBranch filters open trackers to those whose targets include
// (leaf, branch), and rolls each up into a dashboard.Item. Sorted by dep
// then version for stable rendering.
func itemsForLeafBranch(leaf, branch string, openTrackers []*ghclient.Issue) ([]dashboard.Item, error) {
	var items []dashboard.Item
	for _, t := range openTrackers {
		dep := depFromLabels(t.Labels)
		if dep == "" {
			continue
		}
		version := tracker.ParseVersionFromTitle(t.Title, dep)
		if version == "" {
			continue
		}
		st, err := tracker.ExtractState(t.Body)
		if err != nil {
			return nil, fmt.Errorf("tracker #%d: %w", t.Number, err)
		}
		hit := false
		merged := 0
		for _, tg := range st.Targets {
			if tg.Repo == leaf && tg.Branch == branch {
				hit = true
			}
			if tg.State == "merged" {
				merged++
			}
		}
		if !hit {
			continue
		}
		items = append(items, dashboard.Item{
			Dep:        dep,
			Version:    version,
			TrackerNum: t.Number,
			TrackerURL: t.URL,
			Total:      len(st.Targets),
			Merged:     merged,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Dep != items[j].Dep {
			return items[i].Dep < items[j].Dep
		}
		return items[i].Version < items[j].Version
	})
	return items, nil
}

