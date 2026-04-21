package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"

	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass1Cron is the safety-net path. For every non-leaf repo in the config,
// fetch its latest published release and synthesize a dispatch for it if we
// haven't already opened a tracker for that (dep, version). Catches releases
// the upstream Release workflow didn't (or couldn't) notify us about — e.g.
// a tag pushed before the per-repo workflow exists.
//
// One dep failing doesn't block the rest; we log and keep sweeping.
func (r *Reconciler) pass1Cron(ctx context.Context) error {
	for _, name := range upstreamRepos(r.cfg) {
		if err := r.checkUpstream(ctx, name); err != nil {
			log.Printf("pass1Cron: %s: %v", name, err)
		}
	}
	return nil
}

func (r *Reconciler) checkUpstream(ctx context.Context, name string) error {
	repo, ok := r.cfg.Repos[name]
	if !ok {
		return fmt.Errorf("vanished from config")
	}
	ghRepo, err := repo.GitHubRepo()
	if err != nil {
		return err
	}
	tag, err := r.gh.GetLatestReleaseTag(ctx, ghRepo)
	if err != nil {
		return fmt.Errorf("get latest release: %w", err)
	}
	if tag == "" {
		return nil // no releases yet
	}
	if !semver.IsValid(tag) {
		return nil // not semver — out of scope
	}
	processed, err := r.alreadyProcessed(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("check existing trackers: %w", err)
	}
	if processed {
		return nil
	}
	log.Printf("pass1Cron: synthesizing dispatch for %s %s (no tracker found)", name, tag)
	return r.pass1Dispatch(ctx, DispatchEvent{Repo: ghRepo, Tag: tag})
}

// alreadyProcessed reports whether a tracker for (dep, version) exists in
// any state. Open trackers alone are insufficient — pass 2 closes finished
// ones, and we don't want to re-open them.
func (r *Reconciler) alreadyProcessed(ctx context.Context, dep, version string) (bool, error) {
	issues, err := r.gh.ListIssuesAllStates(ctx, r.settings.AutomationRepo, tracker.Labels(dep))
	if err != nil {
		return false, err
	}
	for _, i := range issues {
		if tracker.ParseVersionFromTitle(i.Title, dep) == version {
			return true, nil
		}
	}
	return false, nil
}

// upstreamRepos returns every repo that's a dep of someone — i.e. anything
// the cron loop should sweep for new releases. Leaves are excluded
// (they're never deps of anything).
func upstreamRepos(cfg *config.Config) []string {
	seen := make(map[string]struct{})
	for _, r := range cfg.Repos {
		for _, d := range r.Deps {
			seen[d] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	// Stable order so logs are diffable across ticks.
	sort.Strings(out)
	return out
}
