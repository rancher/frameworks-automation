package renovateapprove

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

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
	EnableAutoMerge(ctx context.Context, repo, nodeID, mergeMethod string) error
	MergePR(ctx context.Context, repo string, number int, sha, mergeMethod string) error
}

// approveBody is the review comment left on every PR the sweep approves.
const approveBody = "Auto-approving Renovate PR."

// Outcome records what the sweep did (or didn't do) with one candidate PR.
type Outcome struct {
	Repo     string
	Number   int
	Title    string
	Approved bool
	// AutoMergeEnabled is true when the sweep requested auto-merge on the PR.
	AutoMergeEnabled bool
	// Merged is true when the PR was already mergeable (auto-merge refused)
	// and the sweep merged it directly.
	Merged bool
	Reason string // set when Approved is false: why it was skipped
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
func Run(ctx context.Context, gh gitHubClient, cfg *Config) Result {
	var res Result
	for _, r := range cfg.Repos {
		outcomes, err := sweepRepo(ctx, gh, cfg, r)
		if err != nil {
			log.Printf("renovate-approve: %s: %v", r.Repo, err)
			res.Failed++
			continue
		}
		res.Outcomes = append(res.Outcomes, outcomes...)
	}
	return res
}

func sweepRepo(ctx context.Context, gh gitHubClient, cfg *Config, rc RepoConfig) ([]Outcome, error) {
	repo := rc.Repo
	prs, err := gh.ListOpenPRs(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}

	var out []Outcome
	for _, pr := range prs {
		src, reason, err := ineligibleReason(ctx, gh, cfg, rc, pr)
		if err != nil {
			log.Printf("renovate-approve: %s#%d: %v", repo, pr.Number, err)
			continue
		}
		o := Outcome{Repo: repo, Number: pr.Number, Title: pr.Title, Reason: reason}
		wantAutoMerge := (reason == "" || reason == reasonAlreadyApproved) && src.AutoMerge && !pr.AutoMerge
		mergeNow := false
		// Auto-merge can only be enabled while the PR is still blocked, so
		// request it before approving: the approval then releases the merge.
		// An already-approved PR gets it too, so one approved before
		// auto-merge was configured (or by a human) isn't stranded.
		if wantAutoMerge {
			switch err := gh.EnableAutoMerge(ctx, repo, pr.NodeID, src.mergeMethod()); {
			case err == nil:
				o.AutoMergeEnabled = true
			case errors.Is(err, ghclient.ErrPRClean):
				// Nothing left to wait for; merge directly after approving.
				mergeNow = true
			default:
				log.Printf("renovate-approve: %s#%d: %v", repo, pr.Number, err)
			}
		}
		if reason == "" {
			// Pin the review to the head SHA whose checks we just read, so a
			// force-push landing mid-sweep can't get approved unreviewed.
			if err := gh.ApprovePR(ctx, repo, pr.Number, pr.HeadSHA, approveBody); err != nil {
				log.Printf("renovate-approve: %s#%d: approve: %v", repo, pr.Number, err)
				continue
			}
			o.Approved = true
		}
		if mergeNow {
			if err := gh.MergePR(ctx, repo, pr.Number, pr.HeadSHA, src.mergeMethod()); err != nil {
				log.Printf("renovate-approve: %s#%d: %v", repo, pr.Number, err)
			} else {
				o.Merged = true
			}
		}
		out = append(out, o)
	}
	return out, nil
}

const reasonAlreadyApproved = "already approved"

// ineligibleReason returns a non-empty reason string when pr should be left
// alone, or "" when it should be approved. The matched source is returned
// whenever the PR matched one, even if it was otherwise ineligible. The only
// error return is a GitHub-call failure (checks/reviews lookups) — callers
// should log and skip rather than treat it as a definitive ineligibility.
func ineligibleReason(ctx context.Context, gh gitHubClient, cfg *Config, rc RepoConfig, pr *ghclient.PR) (Source, string, error) {
	repo := rc.Repo
	src, reason := matchSource(cfg, rc, pr)
	if reason != "" {
		return src, reason, nil
	}
	approved, err := gh.HasApproval(ctx, repo, pr.Number)
	if err != nil {
		return src, "", fmt.Errorf("check existing approval: %w", err)
	}
	if approved {
		return src, reasonAlreadyApproved, nil
	}
	runs, err := gh.ListCheckRuns(ctx, repo, pr.HeadSHA)
	if err != nil {
		return src, "", fmt.Errorf("list check runs: %w", err)
	}
	if !allChecksGreen(runs) {
		return src, "checks not all green", nil
	}
	return src, "", nil
}

// matchSource returns the source pr matches: the global Renovate source or
// one of the repo's extra sources. When none match, the reason is that of
// the global source's miss.
func matchSource(cfg *Config, rc RepoConfig, pr *ghclient.PR) (Source, string) {
	global := Source{
		Author:       cfg.Author,
		BranchPrefix: cfg.BranchPrefix,
		AutoMerge:    cfg.AutoMerge,
		MergeMethod:  cfg.MergeMethod,
	}
	for _, s := range append([]Source{global}, rc.ExtraSources...) {
		if pr.Author == s.Author && strings.HasPrefix(pr.HeadRef, s.BranchPrefix) {
			return s, ""
		}
	}
	if pr.Author != cfg.Author {
		return Source{}, "not authored by " + cfg.Author
	}
	return Source{}, "head branch doesn't match " + cfg.BranchPrefix
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
