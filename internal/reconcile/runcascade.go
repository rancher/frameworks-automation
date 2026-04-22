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
	if err := r.fillTagPromptHints(ctx, stages, leafTable, dependentTables); err != nil {
		// Hints are advisory — log and continue with a barer prompt.
		log.Printf("cascade: fill tag prompt hints: %v", err)
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

// openCascadeStageBumps opens one bump PR per Bump in `op.Stages[stage]`,
// bundling every Dep in the Bump into a single PR. Mutates op.Stages in
// place. Returns true when at least one Bump changed.
//
// A Bump is skipped if it already has a PR, or if any of its Deps still has
// Version=="" (we wait until every dep in the bundle is resolved before
// opening — bundling means we can't issue a partial PR and patch in the
// missing deps later).
func (r *Reconciler) openCascadeStageBumps(ctx context.Context, op *cascade.Op, stage int, trackerURL string) (bool, error) {
	if stage < 0 || stage >= len(op.Stages) {
		return false, nil
	}
	mutated := false
	for i := range op.Stages[stage].Bumps {
		bp := &op.Stages[stage].Bumps[i]
		if bp.PR != 0 || !bumpReady(bp) {
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
			HeadBranch: cascadeBumpBranchName(op.Dep, op.Version, op.LeafBranch, bp.Repo, bp.Branch),
			Modules:    bumpModules(bp),
			TrackerURL: trackerURL,
		}
		log.Printf("cascade: opening stage %d %s %s -> %s base=%s head=%s",
			op.Stages[stage].Layer, bp.Repo, bp.Branch, req.Repo, req.BaseBranch, req.HeadBranch)
		res, err := r.bumper.Open(ctx, req)
		if err != nil {
			return mutated, fmt.Errorf("cascade bump %s on %s %s: %w", bp.Repo, req.Repo, req.BaseBranch, err)
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

// bumpReady reports whether every Dep in `bp` has a non-empty Version. We
// only open a Bump's PR once the whole bundle is resolved, since cascades
// can't go back and add deps to an existing PR without rebasing it.
func bumpReady(bp *cascade.Bump) bool {
	if len(bp.Deps) == 0 {
		return false
	}
	for _, d := range bp.Deps {
		if d.Version == "" {
			return false
		}
	}
	return true
}

func bumpModules(bp *cascade.Bump) []pr.Module {
	out := make([]pr.Module, len(bp.Deps))
	for i, d := range bp.Deps {
		out[i] = pr.Module{Path: d.Module, Version: d.Version}
	}
	return out
}

// fillTagPromptHints populates each TagPrompt's Expected (next-patch
// suggestion) and WorkflowURL by querying the prompt repo's releases. The
// minor used for filtering comes from each repo's own VERSION.md row for
// the prompt's branch — that's the version line the per-repo Release
// workflow validates against, so any future tag matching this minor is the
// correct cascade-mid tag.
//
// Hints are advisory: stale or missing hints don't break the cascade flow
// (the per-repo Release workflow validates the input version anyway).
func (r *Reconciler) fillTagPromptHints(
	ctx context.Context,
	stages []cascade.Stage,
	leafTable *config.VersionTable,
	dependentTables map[string]*config.VersionTable,
) error {
	for i := range stages {
		for j := range stages[i].Tags {
			tg := &stages[i].Tags[j]
			repo, ok := r.cfg.Repos[tg.Repo]
			if !ok {
				continue
			}
			ghRepo, err := repo.GitHubRepo()
			if err != nil {
				return fmt.Errorf("repo %s: %w", tg.Repo, err)
			}
			tg.WorkflowURL = fmt.Sprintf("https://github.com/%s/actions/workflows/release.yml", ghRepo)

			minor := minorForRepoBranch(tg.Repo, tg.Branch, leafTable, dependentTables)
			if minor == "" {
				continue
			}
			next, err := r.predictNextPatch(ctx, ghRepo, minor)
			if err != nil {
				log.Printf("cascade: predict next patch %s %s: %v", tg.Repo, tg.Branch, err)
				continue
			}
			tg.Expected = next
		}
	}
	return nil
}

// minorForRepoBranch returns the VERSION.md minor for `repo`'s `branch`.
// The leaf repo uses leafTable; everything else uses dependentTables.
// Returns "" if the table is unavailable or the branch isn't listed.
func minorForRepoBranch(repo, branch string, leafTable *config.VersionTable, dependentTables map[string]*config.VersionTable) string {
	if tbl := dependentTables[repo]; tbl != nil {
		return tbl.LookupMinor(branch)
	}
	if leafTable != nil {
		return leafTable.LookupMinor(branch)
	}
	return ""
}

// predictNextPatch fetches every release in `ghRepo`, picks the highest
// patch matching `minor` (e.g. "v0.7"), and returns minor + "." + (patch+1).
// Returns minor + ".0" when no prior release matches — cascade is the first
// patch on this minor.
func (r *Reconciler) predictNextPatch(ctx context.Context, ghRepo, minor string) (string, error) {
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return "", err
	}
	highest := -1
	prefix := minor + "."
	for _, t := range tags {
		if !semver.IsValid(t) {
			continue
		}
		if semver.MajorMinor(t) != minor {
			continue
		}
		// Strip the "minor." prefix; what remains is "<patch>" or
		// "<patch>-prerelease". Pre-releases bump the implied patch.
		rest := strings.TrimPrefix(t, prefix)
		if rest == "" {
			continue
		}
		patchStr := rest
		if i := strings.IndexAny(rest, "-+"); i >= 0 {
			patchStr = rest[:i]
		}
		var patch int
		if _, err := fmt.Sscanf(patchStr, "%d", &patch); err != nil {
			continue
		}
		if patch > highest {
			highest = patch
		}
	}
	if highest < 0 {
		return minor + ".0", nil
	}
	return fmt.Sprintf("%s.%d", minor, highest+1), nil
}

// cascadeBumpBranchName is the canonical head-branch name for a cascade bump
// PR. Includes the cascade identity (dep+version+leaf) and the bump's
// (repo, branch) so that:
//
//   - Two cascades for different versions of the same dep don't collide.
//   - Within a cascade, each stage's per-(repo, branch) bump gets a distinct
//     branch.
//   - Stable across reconciler runs so re-runs idempotently dedupe via
//     ListOpenPRsByHead.
func cascadeBumpBranchName(cascDep, cascVersion, leafBranch, bumpRepo, bumpBranch string) string {
	leaf := strings.ReplaceAll(leafBranch, "/", "-")
	br := strings.ReplaceAll(bumpBranch, "/", "-")
	return fmt.Sprintf("automation/cascade-%s-%s-leaf-%s-bump-%s-%s",
		cascDep, cascVersion, leaf, bumpRepo, br)
}
