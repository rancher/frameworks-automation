package reconcile

import (
	"context"
	"fmt"
	"sort"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass1Dispatch reacts to a single tag-emitted event. Resolves the dep,
// loads the VERSION.md tables it needs, computes the target list using the
// dep-minor mapping, and hands off to processBumpOp for the rest of the
// pipeline (tracker, supersede, PR opening, body update).
func (r *Reconciler) pass1Dispatch(ctx context.Context, ev DispatchEvent) error {
	if !semver.IsValid(ev.Tag) {
		return fmt.Errorf("invalid tag %q (not semver)", ev.Tag)
	}
	dep, err := r.cfg.ResolveDep(ev.Repo)
	if err != nil {
		return err
	}

	depTable, downstreamTables, err := r.loadVersionTables(ctx, dep)
	if err != nil {
		return fmt.Errorf("load VERSION.md tables: %w", err)
	}

	rawTargets, err := ComputeTargets(r.cfg, dep, ev.Tag, depTable, downstreamTables)
	if err != nil {
		return fmt.Errorf("compute targets: %w", err)
	}
	return r.processBumpOp(ctx, dep, ev.Tag, rawTargets)
}

// loadVersionTables fetches each repo's VERSION.md from its default branch
// (empty ref). VERSION.md is canonical on the default branch — older release
// branches can drift (a row added on main for a new release line may never
// be backported), so we always read the most up-to-date copy.
//
// Independent deps don't need their own table; only paired do.
func (r *Reconciler) loadVersionTables(ctx context.Context, dep string) (*config.VersionTable, map[string]*config.VersionTable, error) {
	depRepo := r.cfg.Repos[dep]
	dependents := r.cfg.Dependents(dep)

	downstream := make(map[string]*config.VersionTable, len(dependents))
	for _, d := range dependents {
		tbl, err := r.fetchVersionTable(ctx, d)
		if err != nil {
			return nil, nil, err
		}
		downstream[d] = tbl
	}

	if depRepo.Kind != config.KindPaired {
		return nil, downstream, nil
	}
	depTable, err := r.fetchVersionTable(ctx, dep)
	if err != nil {
		return nil, nil, err
	}
	return depTable, downstream, nil
}

func (r *Reconciler) fetchVersionTable(ctx context.Context, repoKey string) (*config.VersionTable, error) {
	repo := r.cfg.Repos[repoKey]
	ghRepo, err := repo.GitHubRepo()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repoKey, err)
	}
	raw, err := r.gh.FetchFile(ctx, ghRepo, "", "VERSION.md")
	if err != nil {
		return nil, fmt.Errorf("fetch VERSION.md from %s: %w", ghRepo, err)
	}
	tbl, err := config.ParseVersionTable(raw)
	if err != nil {
		return nil, fmt.Errorf("parse VERSION.md from %s: %w", ghRepo, err)
	}
	return tbl, nil
}

func toTrackerTargets(targets []Target) []tracker.Target {
	out := make([]tracker.Target, len(targets))
	for i, t := range targets {
		out[i] = tracker.Target{Repo: t.Repo, Branch: t.Branch}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Branch < out[j].Branch
	})
	return out
}

// bumpBranchName is the canonical head-branch name for a bump PR. Stable so
// re-runs idempotently dedupe via ListOpenPRsByHead.
func bumpBranchName(dep, version string) string {
	return fmt.Sprintf("automation/bump-%s-%s", dep, version)
}
