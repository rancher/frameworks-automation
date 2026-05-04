package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass1Dispatch reacts to a single tag-emitted event. Resolves the dep,
// derives the leaf branch this release lands on (paired: dep.minor → pair
// → leaf branch via VERSION.md; independent: always `main` — older
// release/* branches require a manual `Bump <dep>` workflow run), then
// hands off to runBump for the rest of the pipeline.
//
// Cascade coordination: before opening regular bump-op PRs, the dispatched
// tag is offered to every open cascade. If any cascade is awaiting a tag
// for this dep, the cascade claims it (records version+Tagged=true) and
// pass1Dispatch returns early — the cascade owns the downstream propagation
// for this tag, opening its next stage's bumps in passCascade.
func (r *Reconciler) pass1Dispatch(ctx context.Context, ev DispatchEvent) error {
	if !semver.IsValid(ev.Tag) {
		return fmt.Errorf("invalid tag %q (not semver)", ev.Tag)
	}
	dep, err := r.cfg.ResolveDep(ev.Repo)
	if err != nil {
		return err
	}
	if r.cfg.Repos[dep].Kind == config.KindLeaf {
		log.Printf("pass1: %s is a leaf — nothing to propagate", dep)
		return nil
	}

	claimed, err := r.tryClaimCascadeTag(ctx, dep, ev.Tag)
	if err != nil {
		return fmt.Errorf("offer tag to open cascades: %w", err)
	}
	if claimed {
		log.Printf("pass1: %s %s claimed by an open cascade — skipping regular bump", dep, ev.Tag)
		return nil
	}

	leafBranch, err := r.deriveLeafBranchForDispatch(ctx, dep, ev.Tag)
	if err != nil {
		return fmt.Errorf("derive leaf branch: %w", err)
	}
	if leafBranch == "" {
		log.Printf("pass1: %s %s has no matching leaf branch", dep, ev.Tag)
		return nil
	}
	return r.runBump(ctx, dep, ev.Tag, leafBranch)
}

// deriveLeafBranchForDispatch picks the leaf branch this auto-bump targets:
//
//	independent → "main" only. Older release/* branches require a manual
//	              `Bump <dep>` workflow run (the cron path doesn't infer
//	              them since there's no version-pair to consult).
//	paired      → dep.VERSION.md row whose Minor == version's minor gives
//	              Pair (= leaf.minor); leaf.VERSION.md row whose Minor ==
//	              that pair gives leaf.branch.
//
// Returns "" (no error) when the chain exists but the leaf hasn't cut the
// matching branch yet.
func (r *Reconciler) deriveLeafBranchForDispatch(ctx context.Context, dep, version string) (string, error) {
	if r.cfg.Repos[dep].Kind == config.KindIndependent {
		return "main", nil
	}
	depTable, err := r.fetchVersionTable(ctx, dep)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", dep, err)
	}
	minor := semver.MajorMinor(version)
	if minor == "" {
		return "", fmt.Errorf("invalid semver %q", version)
	}
	pair := depTable.LookupPair(minor)
	if pair == "" {
		return "", fmt.Errorf("dep %s minor %s not in VERSION.md", dep, minor)
	}

	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return "", fmt.Errorf("expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafTable, err := r.fetchVersionTable(ctx, leaves[0])
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", leaves[0], err)
	}
	return leafTable.BranchForMinor(pair), nil
}

func (r *Reconciler) fetchVersionTable(ctx context.Context, repoKey string) (*config.VersionTable, error) {
	repo := r.cfg.Repos[repoKey]
	if repo.VersionMD != "" {
		tbl, err := config.ParseVersionTable(repo.VersionMD)
		if err != nil {
			return nil, fmt.Errorf("parse inline version-md for %s: %w", repoKey, err)
		}
		return tbl, nil
	}
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
//
// Includes the leaf branch so two PRs for the same (dep, version) can
// coexist on different leaf lines (e.g. wrangler v0.5.1 onto rancher main
// AND rancher release/v2.13). Leaf branch is sanitized — slashes become
// dashes so the head-branch path doesn't acquire weird nesting.
func bumpBranchName(dep, version, leafBranch string) string {
	leaf := strings.ReplaceAll(leafBranch, "/", "-")
	return fmt.Sprintf("automation/bump-%s-%s-leaf-%s", dep, version, leaf)
}
