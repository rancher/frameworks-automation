package reconcile

import (
	"context"
	"fmt"
	"log"

	"github.com/rancher/release-automation/internal/pr"
	"github.com/rancher/release-automation/internal/tracker"
)

// processBumpOp is the shared pipeline that takes a (dep, version, targets)
// triple and lands it: tracker find-or-create, supersede older versions,
// open bump PRs, persist any state changes back to the tracker body.
//
// Both pass1Dispatch (auto path) and RunBumpDep (manual path) call this
// after deriving their target set — the only difference between them is how
// the targets are computed.
//
// Caller owns the surrounding lifecycle (e.g. running passes 2-4 after).
func (r *Reconciler) processBumpOp(ctx context.Context, dep, version string, rawTargets []Target) error {
	if len(rawTargets) == 0 {
		log.Printf("bump-op: %s %s has no targets", dep, version)
		return nil
	}
	depRepo, ok := r.cfg.Repos[dep]
	if !ok {
		return fmt.Errorf("dep %q not in config", dep)
	}

	op := tracker.Op{
		Dep:     dep,
		Version: version,
		Targets: toTrackerTargets(rawTargets),
	}

	issue, err := tracker.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, &op)
	if err != nil {
		return err
	}
	log.Printf("bump-op: tracker for %s %s -> %s", dep, version, issue.URL)

	if err := tracker.Supersede(ctx, r.gh, r.settings.AutomationRepo, dep, version, issue.URL); err != nil {
		return fmt.Errorf("supersede older trackers for %s: %w", dep, err)
	}

	mutated := false
	for i := range op.Targets {
		changed, err := r.bumpTarget(ctx, dep, version, depRepo.Module, issue.URL, &op.Targets[i])
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
// Skipped (returns false, nil) when target.PR is already set — used by both
// pass1Dispatch (re-runs over partially-completed trackers) and RunBumpDep
// (the manual-bump entrypoint may target a branch already linked).
func (r *Reconciler) bumpTarget(ctx context.Context, dep, version, depModule, trackerURL string, target *tracker.Target) (bool, error) {
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
		BaseBranch: target.Branch,
		HeadBranch: bumpBranchName(dep, version),
		Module:     depModule,
		Version:    version,
		TrackerURL: trackerURL,
	}
	log.Printf("bump: opening %s -> %s base=%s head=%s", req.Module+"@"+req.Version, req.Repo, req.BaseBranch, req.HeadBranch)
	res, err := r.bumper.Open(ctx, req)
	if err != nil {
		return false, fmt.Errorf("bump %s on %s %s: %w", req.Module, req.Repo, req.BaseBranch, err)
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
