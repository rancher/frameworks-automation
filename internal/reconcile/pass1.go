package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass1Dispatch reacts to a single tag-emitted event. End-to-end:
//
//  1. Resolve dep name from ev.Repo.
//  2. Fetch dep VERSION.md (paired strategy needs the dep.minor -> rancher.minor map).
//  3. Fetch each downstream's VERSION.md.
//  4. Compute the target list.
//  5. Find or create the tracker; merge any existing per-target state from
//     its metadata block.
//  6. Supersede older trackers for the same dep (closes their open PRs).
//  7. For each target without a linked PR yet: run Bumper.Open and record
//     the resulting PR number/URL on the target.
//  8. Re-render the tracker body so the new PR links + state appear.
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
	if len(rawTargets) == 0 {
		log.Printf("pass1: %s %s has no targets", dep, ev.Tag)
		return nil
	}

	op := tracker.Op{
		Dep:     dep,
		Version: ev.Tag,
		Targets: toTrackerTargets(rawTargets),
	}

	issue, err := tracker.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, &op)
	if err != nil {
		return err
	}
	log.Printf("pass1: tracker for %s %s -> %s", dep, ev.Tag, issue.URL)

	if err := tracker.Supersede(ctx, r.gh, r.settings.AutomationRepo, dep, ev.Tag, issue.URL); err != nil {
		return fmt.Errorf("supersede older trackers for %s: %w", dep, err)
	}

	depRepo, ok := r.cfg.Repos[dep]
	if !ok {
		return fmt.Errorf("dep %q vanished from config", dep)
	}

	mutated := false
	for i, t := range op.Targets {
		if t.PR != 0 {
			log.Printf("pass1: %s %s already linked PR #%d on %s %s", dep, ev.Tag, t.PR, t.Repo, t.Branch)
			continue
		}
		downstream, ok := r.cfg.Repos[t.Repo]
		if !ok {
			return fmt.Errorf("target repo %q vanished from config", t.Repo)
		}
		downstreamGH, err := downstream.GitHubRepo()
		if err != nil {
			return fmt.Errorf("downstream %s: %w", t.Repo, err)
		}
		req := pr.Request{
			Repo:       downstreamGH,
			BaseBranch: t.Branch,
			HeadBranch: bumpBranchName(dep, ev.Tag),
			Module:     depRepo.Module,
			Version:    ev.Tag,
			TrackerURL: issue.URL,
		}
		log.Printf("pass1: opening bump %s -> %s base=%s head=%s", req.Module+"@"+req.Version, req.Repo, req.BaseBranch, req.HeadBranch)
		res, err := r.bumper.Open(ctx, req)
		if err != nil {
			return fmt.Errorf("bump %s on %s %s: %w", req.Module, req.Repo, req.BaseBranch, err)
		}
		log.Printf("pass1: %s", res.Notes)
		switch {
		case res.NoOp:
			op.Targets[i].State = "merged"
			mutated = true
		case res.PR != nil:
			op.Targets[i].PR = res.PR.Number
			op.Targets[i].PRURL = res.PR.URL
			op.Targets[i].State = "open"
			mutated = true
		}
	}

	if mutated {
		if err := tracker.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
			return fmt.Errorf("update tracker #%d body: %w", issue.Number, err)
		}
	}
	return nil
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
