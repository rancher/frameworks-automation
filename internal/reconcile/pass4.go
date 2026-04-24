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
// queries the bump-op trackers labeled `leaf:<repo>:<branch>` directly and
// overwrites the dashboard body with that rolled-up view.
//
// The dashboard is read-only aggregation — no embedded state, no metadata
// block. Closing trackers drop off the next render automatically.
func (r *Reconciler) pass4Dashboards(ctx context.Context) error {
	for _, leaf := range r.cfg.LeafRepos() {
		if err := r.refreshLeafDashboards(ctx, leaf); err != nil {
			log.Printf("pass4: leaf %s: %v", leaf, err)
		}
	}
	return nil
}

func (r *Reconciler) refreshLeafDashboards(ctx context.Context, leaf string) error {
	tbl, err := r.fetchVersionTable(ctx, leaf)
	if err != nil {
		return fmt.Errorf("fetch %s VERSION.md: %w", leaf, err)
	}
	for _, row := range tbl.Rows {
		trackers, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo,
			[]string{tracker.LabelOp, tracker.LeafLabel(leaf, row.Branch)})
		if err != nil {
			log.Printf("pass4: list trackers for %s %s: %v", leaf, row.Branch, err)
			continue
		}
		items, err := rollUp(trackers)
		if err != nil {
			log.Printf("pass4: roll up %s %s: %v", leaf, row.Branch, err)
			continue
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
	_, err = r.gh.CreateIssue(ctx, r.settings.AutomationRepo, dashboard.Title(leaf, branch), body, labels, nil)
	return err
}

// rollUp converts a list of tracker issues already filtered to one
// (leaf, branch) into dashboard.Item rows. Sorted by dep then version.
func rollUp(trackers []*ghclient.Issue) ([]dashboard.Item, error) {
	items := make([]dashboard.Item, 0, len(trackers))
	for _, t := range trackers {
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
		merged := 0
		for _, tg := range st.Targets {
			if tg.State == "merged" {
				merged++
			}
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

