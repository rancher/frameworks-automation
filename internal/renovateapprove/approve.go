package renovateapprove

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// gitHubClient is the slice of *ghclient.Client this package needs. Kept
// narrow so tests can inject an in-memory fake without pulling in the real
// GitHub SDK.
type gitHubClient interface {
	ListOpenPRs(ctx context.Context, repo string) ([]*ghclient.PR, error)
	ListCheckRuns(ctx context.Context, repo, ref string) ([]ghclient.CheckRun, error)
	HasApproval(ctx context.Context, repo string, number int) (bool, error)
	ApprovePR(ctx context.Context, repo string, number int, sha, body string) error
}

// approveBody is the review comment left on every PR the sweep approves.
const approveBody = "Auto-approving Renovate PR with auto-merge enabled."

// Outcome records what the sweep did (or didn't do) with one candidate PR.
type Outcome struct {
	Repo     string
	Number   int
	Title    string
	Approved bool
	Reason   string // set when Approved is false: why it was skipped
}

// Result is the aggregate outcome of one sweep across every configured repo.
type Result struct {
	Outcomes []Outcome
	// Failed counts repos where a GitHub call itself errored (as opposed to
	// a PR being ineligible, which isn't a failure). A non-zero Failed
	// should make the caller exit non-zero so CI/monitoring notices.
	Failed int
}

// Approved returns the subset of outcomes that were actually approved.
func (r Result) Approved() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Approved {
			out = append(out, o)
		}
	}
	return out
}

// Run sweeps every repo in cfg, approving eligible Renovate PRs. One repo's
// GitHub error is logged and counted in Result.Failed; the sweep continues
// over the rest rather than aborting — mirrors the reconcile package's
// continue-on-error cron style, so one flaky repo never blocks approval
// everywhere else.
//
// force, when true, skips the MinWorkingDays age gate (mirrors the
// rancher/rancher workflow's manual workflow_dispatch force input).
func Run(ctx context.Context, gh gitHubClient, cfg *Config, now time.Time, force bool) Result {
	var res Result
	for _, r := range cfg.Repos {
		outcomes, err := sweepRepo(ctx, gh, cfg, r.Repo, now, force)
		if err != nil {
			log.Printf("renovate-approve: %s: %v", r.Repo, err)
			res.Failed++
			continue
		}
		res.Outcomes = append(res.Outcomes, outcomes...)
	}
	return res
}

func sweepRepo(ctx context.Context, gh gitHubClient, cfg *Config, repo string, now time.Time, force bool) ([]Outcome, error) {
	prs, err := gh.ListOpenPRs(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	var out []Outcome
	for _, pr := range prs {
		reason, err := ineligibleReason(ctx, gh, cfg, repo, pr, now, force)
		if err != nil {
			log.Printf("renovate-approve: %s#%d: %v", repo, pr.Number, err)
			continue
		}
		if reason != "" {
			out = append(out, Outcome{Repo: repo, Number: pr.Number, Title: pr.Title, Reason: reason})
			continue
		}
		// Pin the review to the head SHA whose checks we just read, so a
		// force-push landing mid-sweep can't get approved unreviewed.
		if err := gh.ApprovePR(ctx, repo, pr.Number, pr.HeadSHA, approveBody); err != nil {
			log.Printf("renovate-approve: %s#%d: approve: %v", repo, pr.Number, err)
			continue
		}
		out = append(out, Outcome{Repo: repo, Number: pr.Number, Title: pr.Title, Approved: true})
	}
	return out, nil
}

// ineligibleReason returns a non-empty reason string when pr should be left
// alone, or "" when it should be approved. The only error return is a
// GitHub-call failure (checks/reviews lookups) — callers should log and skip
// rather than treat it as a definitive ineligibility.
func ineligibleReason(ctx context.Context, gh gitHubClient, cfg *Config, repo string, pr *ghclient.PR, now time.Time, force bool) (string, error) {
	if pr.Author != cfg.Author {
		return "not authored by " + cfg.Author, nil
	}
	if !strings.HasPrefix(pr.HeadRef, cfg.BranchPrefix) {
		return "head branch doesn't match " + cfg.BranchPrefix, nil
	}
	if !pr.AutoMerge {
		return "auto-merge not requested", nil
	}
	if !force {
		if days := workingDaysBetween(pr.CreatedAt, now); days < cfg.MinWorkingDays {
			return fmt.Sprintf("too new (%d working day(s) old, need %d)", days, cfg.MinWorkingDays), nil
		}
	}
	approved, err := gh.HasApproval(ctx, repo, pr.Number)
	if err != nil {
		return "", fmt.Errorf("check existing approval: %w", err)
	}
	if approved {
		return "already approved", nil
	}
	runs, err := gh.ListCheckRuns(ctx, repo, pr.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("list check runs: %w", err)
	}
	if !allChecksGreen(runs) {
		return "checks not all green", nil
	}
	return "", nil
}

// allChecksGreen requires at least one check run (an unstarted CI run isn't
// "green", it's "unknown") and every run completed with an acceptable
// conclusion. Mirrors `gh pr checks --json bucket` requiring
// `length > 0 and all(.bucket == "pass")` in rancher/rancher's version.
func allChecksGreen(runs []ghclient.CheckRun) bool {
	if len(runs) == 0 {
		return false
	}
	for _, r := range runs {
		if r.Status != "completed" {
			return false
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return false
		}
	}
	return true
}

// workingDaysBetween counts weekdays (Mon-Fri, UTC calendar dates) in the
// half-open interval [created, now) — i.e. including created's own day if
// it's a weekday, excluding today. Mirrors the age gate in rancher/rancher's
// auto-approve-bot-prs.yml so behavior stays consistent across both.
func workingDaysBetween(created, now time.Time) int {
	created = created.UTC()
	now = now.UTC()
	day := time.Date(created.Year(), created.Month(), created.Day(), 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := 0
	for day.Before(today) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days++
		}
		day = day.AddDate(0, 0, 1)
	}
	return days
}
