// Package reconcile is the orchestration layer: it owns the 4-pass loop and
// dispatches into the focused packages (drift, pr, tracker, slack).
//
// Pass 1 — detect new upstream releases (or react to a dispatch event) and
//          open bump PRs in target downstreams. Materialize tracker issues.
// Pass 2 — poll PR state for every open tracker. Tick checkboxes, post Slack
//          replies in-thread, close trackers when all targets merged.
// Pass 3 — drift check on notify-only branches (independent libs on
//          release/*). Slack notice once per (dep, version).
// Pass 4 — re-render per-rancher-branch dashboard issues from open trackers.
//
// Pilot 1 implements pass 1 (dispatch path) and leaves passes 2-4 stubbed.
package reconcile

import (
	"context"
	"fmt"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/pr"
)

type Settings struct {
	AutomationRepo string // owner/name where tracker issues live
	GitHubToken    string
	// Slack settings are optional during pilot 1.
	SlackToken   string
	SlackChannel string
}

type DispatchEvent struct {
	Repo string // owner/name, e.g. "rancher/steve"
	Tag  string // e.g. "v0.7.5"
	SHA  string // commit SHA the tag points at
}

type Reconciler struct {
	cfg      *config.Config
	settings Settings
	gh       *ghclient.Client
	bumper   *pr.Bumper
}

func New(cfg *config.Config, s Settings) (*Reconciler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if s.AutomationRepo == "" {
		return nil, fmt.Errorf("AutomationRepo is required")
	}
	if s.GitHubToken == "" {
		return nil, fmt.Errorf("GitHubToken is required")
	}
	gh := ghclient.NewClient(context.Background(), s.GitHubToken)
	return &Reconciler{
		cfg:      cfg,
		settings: s,
		gh:       gh,
		bumper:   pr.NewBumper(gh, s.GitHubToken),
	}, nil
}

// RunCron is the safety-net path. Walks every upstream looking for new
// releases, then runs passes 2-4.
func (r *Reconciler) RunCron(ctx context.Context) error {
	if err := r.pass1Cron(ctx); err != nil {
		return fmt.Errorf("pass 1 (cron): %w", err)
	}
	return r.passes234(ctx)
}

// RunDispatch is the fast path. Scoped to a single just-emitted tag; skips
// upstream discovery and goes straight to opening PRs for that tag, then
// runs passes 2-4 so any in-flight ops keep moving.
func (r *Reconciler) RunDispatch(ctx context.Context, ev DispatchEvent) error {
	if err := r.pass1Dispatch(ctx, ev); err != nil {
		return fmt.Errorf("pass 1 (dispatch %s@%s): %w", ev.Repo, ev.Tag, err)
	}
	return r.passes234(ctx)
}

func (r *Reconciler) passes234(ctx context.Context) error {
	if err := r.pass2(ctx); err != nil {
		return fmt.Errorf("pass 2: %w", err)
	}
	if err := r.pass3(ctx); err != nil {
		return fmt.Errorf("pass 3: %w", err)
	}
	if err := r.passCascade(ctx); err != nil {
		return fmt.Errorf("pass cascade: %w", err)
	}
	if err := r.pass4(ctx); err != nil {
		return fmt.Errorf("pass 4: %w", err)
	}
	return nil
}

// --- passes 2-4 ------------------------------------------------------------

func (r *Reconciler) pass2(ctx context.Context) error {
	return r.pass2PollPRs(ctx)
}

func (r *Reconciler) pass3(ctx context.Context) error {
	// TODO: for each independent dep, check go.mod on every release/* of its
	// dependents. Slack notice (once per (dep, version)) when drift exists.
	return nil
}

func (r *Reconciler) pass4(ctx context.Context) error {
	return r.pass4Dashboards(ctx)
}
