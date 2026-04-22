package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
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
	depRepo, ok := r.cfg.Repos[dep]
	if !ok {
		return fmt.Errorf("unknown dep %q", dep)
	}
	if err := r.assertReleaseExists(ctx, depRepo, dep, version); err != nil {
		return err
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
			// Branch is already at the target. If the latest published tag
			// on this minor also has every dep at target, no NEW tag is
			// needed — claim the existing tag for this stage's prompt and
			// let the cascade auto-advance.
			if claimed, err := r.maybeClaimExistingTag(ctx, op, stage, bp); err != nil {
				log.Printf("cascade: existing-tag check %s %s: %v", bp.Repo, bp.Branch, err)
			} else if claimed != "" {
				log.Printf("cascade: %s %s already at target via existing tag %s", bp.Repo, bp.Branch, claimed)
			}
		case res.PR != nil:
			bp.PR = res.PR.Number
			bp.PRURL = res.PR.URL
			bp.State = "open"
			mutated = true
		}
	}
	return mutated, nil
}

// maybeClaimExistingTag handles the "branch was already at target before we
// touched it" case. When the latest published tag on bp's branch lineage has
// every dep in bp pinned at its target version, that existing tag satisfies
// the cascade-mid prompt — no new release is required. Sets the matching
// TagPrompt's Version+Tagged and returns the claimed tag; returns "" when
// no satisfying tag was found.
func (r *Reconciler) maybeClaimExistingTag(ctx context.Context, op *cascade.Op, stage int, bp *cascade.Bump) (string, error) {
	tag, err := r.findExistingTagForBump(ctx, bp)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return "", nil
	}
	for j := range op.Stages[stage].Tags {
		tg := &op.Stages[stage].Tags[j]
		if tg.Repo == bp.Repo && tg.Branch == bp.Branch && !tg.Tagged {
			tg.Version = tag
			tg.Tagged = true
			return tag, nil
		}
	}
	// Final stage has no Tags — that's fine, no claim to make.
	return "", nil
}

// findExistingTagForBump returns the highest published release tag on bp's
// branch lineage (matched by minor) where go.mod already pins every dep in
// bp at its target version. Returns "" when no satisfying tag is found.
func (r *Reconciler) findExistingTagForBump(ctx context.Context, bp *cascade.Bump) (string, error) {
	repo, ok := r.cfg.Repos[bp.Repo]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", bp.Repo)
	}
	ghRepo, err := repo.GitHubRepo()
	if err != nil {
		return "", err
	}
	tbl, err := r.fetchVersionTable(ctx, bp.Repo)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", bp.Repo, err)
	}
	minor := tbl.LookupMinor(bp.Branch)
	if minor == "" {
		return "", nil
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return "", err
	}
	candidates := tags[:0:0]
	for _, t := range tags {
		if semver.IsValid(t) && semver.MajorMinor(t) == minor {
			candidates = append(candidates, t)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return semver.Compare(candidates[i], candidates[j]) > 0
	})
	for _, tag := range candidates {
		ok, err := r.tagSatisfiesBump(ctx, ghRepo, tag, bp.Deps)
		if err != nil {
			log.Printf("cascade: check %s@%s: %v", ghRepo, tag, err)
			continue
		}
		if ok {
			return tag, nil
		}
	}
	return "", nil
}

// tagSatisfiesBump returns true when go.mod at `tag` requires every dep in
// `deps` at its target version (exact match).
func (r *Reconciler) tagSatisfiesBump(ctx context.Context, ghRepo, tag string, deps []cascade.DepBump) (bool, error) {
	gomod, err := r.gh.FetchFile(ctx, ghRepo, tag, "go.mod")
	if err != nil {
		return false, err
	}
	mf, err := modfile.Parse("go.mod", []byte(gomod), nil)
	if err != nil {
		return false, fmt.Errorf("parse go.mod at %s@%s: %w", ghRepo, tag, err)
	}
	have := make(map[string]string, len(mf.Require))
	for _, req := range mf.Require {
		have[req.Mod.Path] = req.Mod.Version
	}
	for _, d := range deps {
		if have[d.Module] != d.Version {
			return false, nil
		}
	}
	return true, nil
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

// assertReleaseExists confirms `version` is a published release tag on
// `dep`'s repo. Pre-flight check so a typo (wrong version, wrong dep) fails
// before we create a tracker issue and try to clone downstreams.
//
// Released-tag check (not just any git tag): cascade is for finished
// releases — the per-repo Release workflow is what produces these tags, so
// "is there a Release with this tag" is the right question.
func (r *Reconciler) assertReleaseExists(ctx context.Context, depRepo config.Repo, dep, version string) error {
	ghRepo, err := depRepo.GitHubRepo()
	if err != nil {
		return fmt.Errorf("dep %s: %w", dep, err)
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return fmt.Errorf("list %s releases: %w", dep, err)
	}
	for _, t := range tags {
		if t == version {
			return nil
		}
	}
	return fmt.Errorf("dep %s has no published release %s on %s", dep, version, ghRepo)
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
