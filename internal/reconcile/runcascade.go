package reconcile

import (
	"context"
	"fmt"
	"log"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/cascade"
	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
)

// RunCascade is the cascade entrypoint. The .github/workflows/cascade.yaml
// dispatches the reconciler with -mode=cascade to walk a (dep, version)
// up the DAG to a leaf branch, opening one stage of bump PRs at a time and
// prompting a re-tag at each intermediate layer.
//
// The cascade is self-contained: it owns its own PRs, separate from the
// per-(dep, version) bump-op trackers used by the dispatch + bump-dep
// paths. Cascade-mid tags arriving via tag-emitted dispatch are claimed by
// open cascades and don't trigger regular bump-op PRs (see pass1Dispatch).
//
// Pipeline:
//
//  1. Validate inputs; resolve leaf repo.
//  2. Load VERSION.md tables (leaf + every paired in-scope dependent).
//  3. cascade.ComputeStages → planned stages.
//  4. FindOrCreate cascade tracker; merge any prior state.
//  5. Open stage 1 bump PRs (subsequent stages open as prior tags arrive,
//     handled in passCascade).
//  6. Persist state.
func (r *Reconciler) RunCascade(ctx context.Context, dep, version, leafBranch string) error {
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
	for name, repo := range r.cfg.Repos {
		if repo.Kind != config.KindPaired {
			continue
		}
		tbl, err := r.fetchVersionTable(ctx, name)
		if err != nil {
			return fmt.Errorf("load %s VERSION.md: %w", name, err)
		}
		dependentTables[name] = tbl
	}

	stages, err := cascade.ComputeStages(r.cfg, dep, version, leafRepo, leafBranch, leafTable, dependentTables)
	if err != nil {
		return fmt.Errorf("compute cascade stages: %w", err)
	}
	if len(stages) == 0 {
		return fmt.Errorf("compute cascade stages: empty plan")
	}

	op := cascade.Op{
		Dep:        dep,
		Version:    version,
		LeafRepo:   leafRepo,
		LeafBranch: leafBranch,
		Stages:     stages,
	}

	issue, err := cascade.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, &op)
	if err != nil {
		return err
	}
	log.Printf("cascade: tracker for %s %s -> %s %s -> %s", dep, version, leafRepo, leafBranch, issue.URL)

	mutated, err := r.openCascadeStageBumps(ctx, &op, op.CurrentStage, issue.URL)
	if err != nil {
		return err
	}
	if mutated {
		if err := cascade.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
			return fmt.Errorf("update cascade #%d body: %w", issue.Number, err)
		}
	}
	return r.passes234(ctx)
}

// openCascadeStageBumps opens a bump PR for every bump in `op.Stages[stage]`
// that has a Version set but no PR yet. Mutates op.Stages in place. Returns
// true when at least one bump changed.
//
// Bumps with Version=="" are skipped — those wait on a prior stage's tag.
func (r *Reconciler) openCascadeStageBumps(ctx context.Context, op *cascade.Op, stage int, trackerURL string) (bool, error) {
	if stage < 0 || stage >= len(op.Stages) {
		return false, nil
	}
	mutated := false
	for i := range op.Stages[stage].Bumps {
		bp := &op.Stages[stage].Bumps[i]
		if bp.PR != 0 || bp.Version == "" {
			continue
		}
		downstream, ok := r.cfg.Repos[bp.Repo]
		if !ok {
			return mutated, fmt.Errorf("cascade target repo %q vanished from config", bp.Repo)
		}
		downstreamGH, err := downstream.GitHubRepo()
		if err != nil {
			return mutated, fmt.Errorf("cascade downstream %s: %w", bp.Repo, err)
		}
		req := pr.Request{
			Repo:       downstreamGH,
			BaseBranch: bp.Branch,
			HeadBranch: cascadeBumpBranchName(op.Dep, op.Version, op.LeafBranch, bp.Dep, bp.Version),
			Module:     bp.Module,
			Version:    bp.Version,
			TrackerURL: trackerURL,
		}
		log.Printf("cascade: opening stage %d %s@%s -> %s base=%s head=%s",
			op.Stages[stage].Layer, req.Module, req.Version, req.Repo, req.BaseBranch, req.HeadBranch)
		res, err := r.bumper.Open(ctx, req)
		if err != nil {
			return mutated, fmt.Errorf("cascade bump %s on %s %s: %w", req.Module, req.Repo, req.BaseBranch, err)
		}
		log.Printf("cascade: %s", res.Notes)
		switch {
		case res.NoOp:
			bp.State = "merged"
			mutated = true
		case res.PR != nil:
			bp.PR = res.PR.Number
			bp.PRURL = res.PR.URL
			bp.State = "open"
			mutated = true
		}
	}
	return mutated, nil
}

// cascadeBumpBranchName is the canonical head-branch name for a cascade bump
// PR. Includes both the cascade identity (dep+version+leaf) AND the per-bump
// dep+version so that:
//
//   - Two cascades for different versions of the same dep don't collide.
//   - Within a cascade, each stage's bumps get a distinct branch.
//   - Stable across reconciler runs so re-runs idempotently dedupe via
//     ListOpenPRsByHead.
func cascadeBumpBranchName(cascDep, cascVersion, leafBranch, bumpDep, bumpVersion string) string {
	leaf := strings.ReplaceAll(leafBranch, "/", "-")
	return fmt.Sprintf("automation/cascade-%s-%s-leaf-%s-bump-%s-%s",
		cascDep, cascVersion, leaf, bumpDep, bumpVersion)
}
