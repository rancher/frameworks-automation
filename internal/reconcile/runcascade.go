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
// dispatches the reconciler with -mode=cascade to walk a multi-source
// cascade up the DAG to a leaf branch, opening one stage of bump PRs at a
// time and prompting a re-tag at each intermediate layer.
//
// `independents` is the user-supplied source set: a map of independent dep
// name to target version. Empty means "no explicit independents — just
// pick up paired-latest into leaf". Paired deps are always picked up at
// the highest existing tag on the leaf-paired branch (paired-latest); the
// user doesn't (and shouldn't) supply versions for paired components.
//
// Pipeline:
//
//  1. Validate inputs; resolve leaf repo; assert each independent's version
//     is a published release.
//  2. Load VERSION.md tables (leaf + every paired in cfg).
//  3. cascade.ComputeStages → planned stages + sources (explicit + paired-latest).
//  4. FindOrCreate cascade tracker (per-leaf-branch identity); supersedes any
//     open cascade on the same leaf with a different explicit-source set.
//  5. Open stage 1 bump PRs; subsequent stages open as prior tags arrive
//     (handled in passCascade).
//  6. Persist state; run passes 2-4 so in-flight ops keep moving.
func (r *Reconciler) RunCascade(ctx context.Context, leafBranch string, independents map[string]string) error {
	if leafBranch == "" {
		return fmt.Errorf("leaf branch is required")
	}
	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return fmt.Errorf("expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafRepo := leaves[0]

	for name, version := range independents {
		if version == "" {
			return fmt.Errorf("source %q: version is required (omit the input to skip)", name)
		}
		if !semver.IsValid(version) {
			return fmt.Errorf("source %q: invalid version %q (not semver)", name, version)
		}
		repoCfg, ok := r.cfg.Repos[name]
		if !ok {
			return fmt.Errorf("source %q not in config", name)
		}
		if repoCfg.Kind != config.KindIndependent {
			return fmt.Errorf("source %q is kind=%s; only independents may be cascade inputs", name, repoCfg.Kind)
		}
		if err := r.assertReleaseExists(ctx, repoCfg, name, version); err != nil {
			return err
		}
	}

	leafTable, err := r.fetchVersionTable(ctx, leafRepo)
	if err != nil {
		return fmt.Errorf("load leaf VERSION.md: %w", err)
	}
	pairedTables := make(map[string]*config.VersionTable)
	for name, repo := range r.cfg.Repos {
		if repo.Kind != config.KindPaired {
			continue
		}
		// Branch-template paired repos resolve their branch from the leaf
		// rancher minor — no VERSION.md fetch required.
		if repo.BranchTemplate != "" {
			continue
		}
		tbl, err := r.fetchVersionTable(ctx, name)
		if err != nil {
			return fmt.Errorf("load %s VERSION.md: %w", name, err)
		}
		pairedTables[name] = tbl
	}

	resolver := func(name, branch string) (string, error) {
		return r.resolveLatestForBranch(ctx, name, branch)
	}

	sources, stages, err := cascade.ComputeStages(r.cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, resolver)
	if err != nil {
		return fmt.Errorf("compute cascade stages: %w", err)
	}

	// Reject cascades whose paired-latest sources (or their managed paired
	// deps) have unreleased commits. Example: norman was committed to but not
	// tagged; webhook@v0.7.2 (paired-latest) still pins the old norman version;
	// the cascade would silently skip the new commit unless we block here.
	leafMinor := leafTable.LookupMinor(leafBranch)
	if err := r.validatePairedLatestSources(ctx, sources, leafMinor, pairedTables); err != nil {
		return err
	}

	if err := r.fillTagPromptHints(ctx, stages, leafTable, pairedTables); err != nil {
		// Hints are advisory — log and continue with a barer prompt.
		log.Printf("cascade: fill tag prompt hints: %v", err)
	}

	op := cascade.Op{
		LeafRepo:   leafRepo,
		LeafBranch: leafBranch,
		Sources:    sources,
		Stages:     stages,
	}

	issue, err := cascade.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, &op, r.supersedeCascade)
	if err != nil {
		return err
	}
	log.Printf("cascade: tracker for %s %s -> %s", leafRepo, leafBranch, issue.URL)

	mutated, err := r.openCascadeStageBumps(ctx, &op, op.CurrentStage, issue.Number, issue.URL)
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

// supersedeCascade closes an existing cascade whose explicit-source set has
// been replaced by a re-trigger. Closes any open bump PRs first so the
// supersede comment appears in the timeline before the close marker, then
// closes the issue itself.
func (r *Reconciler) supersedeCascade(ctx context.Context, old *cascade.Issue) error {
	log.Printf("cascade: superseding cascade #%d (explicit-source set changed)", old.Number)
	st, err := cascade.ExtractState(old.Body)
	if err != nil {
		log.Printf("cascade: supersede #%d: extract state: %v", old.Number, err)
	} else {
		for _, stage := range st.Stages {
			for _, bp := range stage.Bumps {
				if bp.PR == 0 || bp.State == "merged" || bp.State == "closed" {
					continue
				}
				downstream, ok := r.cfg.Repos[bp.Repo]
				if !ok {
					continue
				}
				ghRepo, err := downstream.GitHubRepo()
				if err != nil {
					log.Printf("cascade: supersede #%d %s: %v", old.Number, bp.Repo, err)
					continue
				}
				comment := fmt.Sprintf("Superseded by a new cascade on %s with a different source set.", old.Title)
				if err := r.gh.ClosePR(ctx, ghRepo, bp.PR, comment); err != nil {
					log.Printf("cascade: supersede #%d close PR %s#%d: %v", old.Number, ghRepo, bp.PR, err)
				}
			}
		}
	}
	return r.gh.CloseIssue(ctx, r.settings.AutomationRepo, old.Number,
		"Superseded by a new cascade with a different explicit-source set.")
}

// openCascadeStageBumps opens one bump PR per Bump in `op.Stages[stage]`,
// bundling every Dep in the Bump into a single PR. Mutates op.Stages in
// place. Returns true when at least one Bump changed.
//
// A Bump is skipped if it already has a PR, or if any of its Deps still has
// Version=="" (we wait until every dep in the bundle is resolved before
// opening — bundling means we can't issue a partial PR and patch in the
// missing deps later).
func (r *Reconciler) openCascadeStageBumps(ctx context.Context, op *cascade.Op, stage, issueNum int, trackerURL string) (bool, error) {
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
			HeadBranch: cascadeBumpBranchName(issueNum, bp.Repo, bp.Branch),
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
	// No tag prompt for this bump → nothing to claim. Skip the VERSION.md
	// fetch and tag scan entirely (chart, for instance, has bump-only
	// stages and no VERSION.md to read).
	hasPrompt := false
	for _, tg := range op.Stages[stage].Tags {
		if tg.Repo == bp.Repo && tg.Branch == bp.Branch {
			hasPrompt = true
			break
		}
	}
	if !hasPrompt {
		return "", nil
	}
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
		if !ok {
			continue
		}
		// Reject the tag if the branch has advanced past it: unreleased commits
		// mean the tag doesn't represent the full current state of the branch and
		// a new release is required.
		ahead, err := r.gh.CommitsAheadOf(ctx, ghRepo, tag, bp.Branch)
		if err != nil {
			log.Printf("cascade: ahead-of check %s %s...%s: %v", ghRepo, tag, bp.Branch, err)
			continue
		}
		if ahead > 0 {
			return "", nil
		}
		return tag, nil
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
		out[i] = pr.Module{Path: d.Module, Version: d.Version, Strategy: d.Strategy}
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

// resolveLatestForBranch returns the highest existing release tag on
// `repoName`'s `branch` (matched by VERSION.md minor). Used by ComputeStages
// to pin paired-latest sources at cascade creation. "" with no error means
// the branch has no published release yet.
func (r *Reconciler) resolveLatestForBranch(ctx context.Context, repoName, branch string) (string, error) {
	repoCfg, ok := r.cfg.Repos[repoName]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", repoName)
	}
	ghRepo, err := repoCfg.GitHubRepo()
	if err != nil {
		return "", err
	}
	tbl, err := r.fetchVersionTable(ctx, repoName)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", repoName, err)
	}
	minor := tbl.LookupMinor(branch)
	if minor == "" {
		return "", fmt.Errorf("branch %q not in %s VERSION.md", branch, repoName)
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return "", err
	}
	var best string
	for _, t := range tags {
		if !semver.IsValid(t) || semver.MajorMinor(t) != minor {
			continue
		}
		if best == "" || semver.Compare(t, best) > 0 {
			best = t
		}
	}
	return best, nil
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

// validatePairedLatestSources blocks cascade creation when a paired-latest
// source (or any of its managed paired deps) has unreleased commits. Without
// this check the cascade would silently use a stale tag for a repo whose
// branch has moved on, ignoring committed-but-untagged work.
//
// Only paired-latest sources are checked (explicit sources are user-supplied
// and their version is already validated by assertReleaseExists). For each
// paired source we:
//
//  1. Confirm the source's own branch is not ahead of its latest tag.
//  2. Fetch go.mod at that tag and confirm every managed paired dep's branch
//     is not ahead of the version pinned there.
//
// Independent deps are skipped — their release cycle is decoupled by design.
func (r *Reconciler) validatePairedLatestSources(
	ctx context.Context,
	sources []cascade.Source,
	leafMinor string,
	pairedTables map[string]*config.VersionTable,
) error {
	moduleToRepo := map[string]string{}
	for name, repo := range r.cfg.Repos {
		if repo.Module != "" {
			moduleToRepo[repo.Module] = name
		}
	}

	var errs []string
	for _, src := range sources {
		if src.Explicit {
			continue
		}
		srcCfg, ok := r.cfg.Repos[src.Name]
		if !ok {
			continue
		}
		srcGHRepo, err := srcCfg.GitHubRepo()
		if err != nil {
			continue
		}
		srcBranch, err := r.branchForRepo(src.Name, leafMinor, pairedTables)
		if err != nil || srcBranch == "" {
			log.Printf("cascade validate: %s branch lookup: %v", src.Name, err)
			continue
		}

		// Check 1: source's own branch ahead of its paired-latest tag.
		ahead, err := r.gh.CommitsAheadOf(ctx, srcGHRepo, src.Version, srcBranch)
		if err != nil {
			log.Printf("cascade validate: %s ahead check: %v", src.Name, err)
		} else if ahead > 0 {
			errs = append(errs, fmt.Sprintf(
				"%s: branch %s is %d commit(s) ahead of %s — please tag a new release",
				src.Name, srcBranch, ahead, src.Version))
		}

		// Check 2: managed paired deps pinned in the source's go.mod have
		// unreleased commits on their branch.
		gomod, err := r.gh.FetchFile(ctx, srcGHRepo, src.Version, "go.mod")
		if err != nil {
			log.Printf("cascade validate: fetch %s@%s go.mod: %v", src.Name, src.Version, err)
			continue
		}
		mf, err := modfile.Parse("go.mod", []byte(gomod), nil)
		if err != nil {
			log.Printf("cascade validate: parse %s@%s go.mod: %v", src.Name, src.Version, err)
			continue
		}
		for _, req := range mf.Require {
			depName, ok := moduleToRepo[req.Mod.Path]
			if !ok {
				continue
			}
			depCfg, ok := r.cfg.Repos[depName]
			if !ok || depCfg.Kind != config.KindPaired {
				continue
			}
			depGHRepo, err := depCfg.GitHubRepo()
			if err != nil {
				continue
			}
			depBranch, err := r.branchForRepo(depName, leafMinor, pairedTables)
			if err != nil || depBranch == "" {
				continue
			}
			depAhead, err := r.gh.CommitsAheadOf(ctx, depGHRepo, req.Mod.Version, depBranch)
			if err != nil {
				log.Printf("cascade validate: %s dep %s ahead check: %v", src.Name, depName, err)
				continue
			}
			if depAhead > 0 {
				errs = append(errs, fmt.Sprintf(
					"%s@%s: dep %s branch %s is %d commit(s) ahead of %s — please tag a new %s release first",
					src.Name, src.Version, depName, depBranch, depAhead, req.Mod.Version, depName))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cascade has stale paired-latest sources; tag the affected repos first:\n  - %s",
			strings.Join(errs, "\n  - "))
	}
	return nil
}

// branchForRepo returns the branch of `repoName` that corresponds to
// `leafMinor`. Handles both VERSION.md paired repos and branch-template repos.
func (r *Reconciler) branchForRepo(repoName, leafMinor string, pairedTables map[string]*config.VersionTable) (string, error) {
	repoCfg, ok := r.cfg.Repos[repoName]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", repoName)
	}
	switch repoCfg.Kind {
	case config.KindIndependent:
		return "main", nil
	case config.KindPaired:
		br, err := repoCfg.ResolveBranch(leafMinor, pairedTables[repoName])
		if err != nil {
			return "", fmt.Errorf("resolve branch for %s: %w", repoName, err)
		}
		return br, nil
	}
	return "", fmt.Errorf("repo %q: unsupported kind %q", repoName, repoCfg.Kind)
}

// cascadeBumpBranchName is the canonical head-branch name for a cascade bump
// PR. Stable per (cascade issue, bump position) so:
//
//   - Re-runs idempotently dedupe via ListOpenPRsByHead (same branch → same
//     existing PR, not a duplicate).
//   - Different cascades on the same leaf branch (after supersede creates a
//     new issue with new sources) get distinct branch names — no collision
//     with the superseded cascade's now-closed PRs.
//
// The cascade issue number is the disambiguator; including the bump's
// (repo, branch) keeps multi-bump cascades on different head branches inside
// each downstream repo.
func cascadeBumpBranchName(cascadeIssue int, bumpRepo, bumpBranch string) string {
	br := strings.ReplaceAll(bumpBranch, "/", "-")
	return fmt.Sprintf("automation/cascade-%d-bump-%s-%s", cascadeIssue, bumpRepo, br)
}
