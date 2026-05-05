package reconcile

import (
	"context"
	"fmt"
	"log"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
	"github.com/rancher/release-automation/internal/tracker"
)

// runBump is the shared engine for landing a (dep, version) onto a single
// leaf branch. Both pass1Dispatch (auto path, derives leafBranch from the
// tag) and RunBumpDep (manual path, leafBranch from input) call this — the
// only thing they vary is how leafBranch is obtained.
//
// Pipeline:
//
//  1. Resolve the leaf and load VERSION.md tables (leaf + every paired
//     dependent of `dep`).
//  2. Fan out targets via ComputeTargetsForLeafBranch.
//  3. Find or create the (dep, version, leaf-branch) tracker.
//  4. Supersede older trackers on this same leaf-branch (closes their PRs).
//  5. Open one bump PR per target if not already linked.
//  6. Re-render the tracker body.
//
// Caller owns the surrounding lifecycle (e.g. running later passes after).
func (r *Reconciler) runBump(ctx context.Context, dep, version, leafBranch string) error {
	if leafBranch == "" {
		return fmt.Errorf("runBump: leafBranch is required")
	}
	if _, ok := r.cfg.Repos[dep]; !ok {
		return fmt.Errorf("runBump: dep %q not in config", dep)
	}

	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return fmt.Errorf("runBump: expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafRepo := leaves[0]

	leafTable, err := r.fetchVersionTable(ctx, leafRepo)
	if err != nil {
		return fmt.Errorf("load leaf VERSION.md: %w", err)
	}
	dependentTables := make(map[string]*config.VersionTable)
	for _, d := range r.cfg.Dependents(dep) {
		dCfg := r.cfg.Repos[d]
		if d == leafRepo || dCfg.Kind != config.KindPaired {
			continue
		}
		// Branch-template paired repos (e.g. chart with dev-v{rancher-minor})
		// don't ship a VERSION.md — branch resolution is template-driven.
		if dCfg.BranchTemplate != "" {
			continue
		}
		tbl, err := r.fetchVersionTable(ctx, d)
		if err != nil {
			return fmt.Errorf("load %s VERSION.md: %w", d, err)
		}
		dependentTables[d] = tbl
	}

	rawTargets, err := ComputeTargetsForLeafBranch(r.cfg, dep, leafRepo, leafBranch, leafTable, dependentTables)
	if err != nil {
		return fmt.Errorf("compute targets: %w", err)
	}
	if len(rawTargets) == 0 {
		log.Printf("runBump: %s %s onto %s %s has no targets", dep, version, leafRepo, leafBranch)
		return nil
	}

	op := tracker.Op{
		Dep:        dep,
		Version:    version,
		LeafRepo:   leafRepo,
		LeafBranch: leafBranch,
		Targets:    toTrackerTargets(rawTargets),
	}

	issue, err := tracker.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, r.configName, &op)
	if err != nil {
		return err
	}
	log.Printf("runBump[%s]: tracker for %s %s on %s %s -> %s", r.configName, dep, version, leafRepo, leafBranch, issue.URL)

	if err := tracker.Supersede(ctx, r.gh, r.settings.AutomationRepo, r.configName, dep, leafRepo, leafBranch, version, issue.URL); err != nil {
		return fmt.Errorf("supersede older trackers for %s on %s %s: %w", dep, leafRepo, leafBranch, err)
	}

	depModule := r.cfg.FirstModulePath(dep)

	mutated := false
	for i := range op.Targets {
		changed, err := r.bumpTarget(ctx, dep, version, leafBranch, depModule, issue.URL, &op.Targets[i])
		if err != nil {
			return err
		}
		if changed {
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

// bumpTarget opens (or adopts) a single bump PR for `target`. Mutates target
// in place with PR number/URL/state. Returns true when target was changed.
//
// Skipped (returns false, nil) when target.PR is already set — re-runs over
// partially-completed trackers idempotently no-op the already-linked rows.
func (r *Reconciler) bumpTarget(ctx context.Context, dep, version, leafBranch, depModule, trackerURL string, target *tracker.Target) (bool, error) {
	if target.PR != 0 {
		log.Printf("bump: %s %s already linked PR #%d on %s %s", dep, version, target.PR, target.Repo, target.Branch)
		return false, nil
	}
	downstream, ok := r.cfg.Repos[target.Repo]
	if !ok {
		return false, fmt.Errorf("target repo %q vanished from config", target.Repo)
	}
	downstreamGH, err := downstream.GitHubRepo()
	if err != nil {
		return false, fmt.Errorf("downstream %s: %w", target.Repo, err)
	}
	req := pr.Request{
		Repo:       downstreamGH,
		Fork:       downstream.Fork,
		BaseBranch: target.Branch,
		HeadBranch: bumpBranchName(r.configName, dep, version, leafBranch),
		Modules:    []pr.Module{{Path: depModule, Version: version, Strategy: downstream.DepStrategy(dep)}},
		TrackerURL: trackerURL,
	}
	log.Printf("bump: opening %s@%s -> %s base=%s head=%s", depModule, version, req.Repo, req.BaseBranch, req.HeadBranch)
	res, err := r.bumper.Open(ctx, req)
	if err != nil {
		return false, fmt.Errorf("bump %s on %s %s: %w", depModule, req.Repo, req.BaseBranch, err)
	}
	log.Printf("bump: %s", res.Notes)
	switch {
	case res.NoOp:
		target.State = "merged"
		return true, nil
	case res.PR != nil:
		target.PR = res.PR.Number
		target.PRURL = res.PR.URL
		target.State = "open"
		return true, nil
	}
	return false, nil
}
