package tracker

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// Issue is a thin wrapper carrying both the rendered body and the GitHub
// issue identity. Returned by FindOrCreate/Render-then-Update so callers
// can update the body without re-querying.
type Issue struct {
	Number int
	Title  string
	URL    string
	Body   string // current rendered body, including embedded metadata
}

// FindOrCreate looks up the open tracker for (op.Dep, op.Version) in the
// automation repo. If absent, creates one rendered from `op`. If present,
// merges any state already stored in its metadata block back into `op` so
// the caller sees existing PR links.
//
// Lookup: query `bump-op + dep:<name>` and filter the (≤ a handful) results
// by version parsed from the title. We deliberately don't use a per-version
// label — that would proliferate forever as releases ship.
func FindOrCreate(ctx context.Context, gh *ghclient.Client, automationRepo string, op *Op) (*Issue, error) {
	labels := Labels(op.Dep)
	candidates, err := gh.ListOpenIssues(ctx, automationRepo, labels)
	if err != nil {
		return nil, fmt.Errorf("find tracker for %s %s: %w", op.Dep, op.Version, err)
	}
	for _, existing := range candidates {
		if ParseVersionFromTitle(existing.Title, op.Dep) != op.Version {
			continue
		}
		st, err := ExtractState(existing.Body)
		if err != nil {
			return nil, fmt.Errorf("read state from tracker #%d: %w", existing.Number, err)
		}
		mergeState(op, st)
		return &Issue{Number: existing.Number, Title: existing.Title, URL: existing.URL, Body: existing.Body}, nil
	}

	body, err := renderForCreate(*op)
	if err != nil {
		return nil, err
	}
	created, err := gh.CreateIssue(ctx, automationRepo, Title(op.Dep, op.Version), body, labels)
	if err != nil {
		return nil, fmt.Errorf("create tracker for %s %s: %w", op.Dep, op.Version, err)
	}
	return &Issue{Number: created.Number, Title: created.Title, URL: created.URL, Body: body}, nil
}

// UpdateBody re-renders `op` and pushes the new body to the tracker issue.
// Call after mutating op.Targets (e.g. after opening a PR).
func UpdateBody(ctx context.Context, gh *ghclient.Client, automationRepo string, issueNum int, op Op) error {
	body, err := renderForCreate(op)
	if err != nil {
		return err
	}
	return gh.UpdateIssueBody(ctx, automationRepo, issueNum, body)
}

// Supersede closes any open tracker for `dep` whose version is older than
// `newVersion`, including all open PRs linked from that tracker. Each closed
// PR + tracker gets a comment pointing at `newURL` for traceability.
//
// "Older" is a strict semver comparison — equal versions don't supersede
// (FindOrCreate handles the dedupe for the same version).
func Supersede(ctx context.Context, gh *ghclient.Client, automationRepo string, dep, newVersion string, newTrackerURL string) error {
	open, err := gh.ListOpenIssues(ctx, automationRepo, Labels(dep))
	if err != nil {
		return fmt.Errorf("scan trackers for dep:%s: %w", dep, err)
	}
	for _, issue := range open {
		ver := ParseVersionFromTitle(issue.Title, dep)
		if ver == "" || ver == newVersion {
			continue
		}
		if semver.Compare(ver, newVersion) >= 0 {
			continue // not older
		}
		st, err := ExtractState(issue.Body)
		if err != nil {
			return fmt.Errorf("read state from tracker #%d: %w", issue.Number, err)
		}
		// Close every still-open PR linked from the older tracker. Use
		// per-target repo names to find owner/name — we stored "config-key"
		// not "owner/name" in the metadata, so we rely on the label-derived
		// dep + the downstream's owner being known. For pilot 1 the
		// downstream is always rancher/<name>; we cheat and look up via the
		// PR's repo from the URL stored alongside.
		for _, t := range st.Targets {
			if t.PR == 0 || t.PRURL == "" {
				continue
			}
			repo, err := repoFromPRURL(t.PRURL)
			if err != nil {
				return fmt.Errorf("tracker #%d target %s: %w", issue.Number, t.Repo, err)
			}
			comment := fmt.Sprintf("Superseded by %s.", newTrackerURL)
			if err := gh.ClosePR(ctx, repo, t.PR, comment); err != nil {
				// Best-effort: log via wrapping but keep going so one stuck
				// PR doesn't strand the whole supersede.
				return fmt.Errorf("close PR %s#%d: %w", repo, t.PR, err)
			}
		}
		comment := fmt.Sprintf("Superseded by %s.", newTrackerURL)
		if err := gh.CloseIssue(ctx, automationRepo, issue.Number, comment); err != nil {
			return fmt.Errorf("close tracker #%d: %w", issue.Number, err)
		}
	}
	return nil
}

// mergeState copies PR/state info from a stored Persistent block back into op.
// Matches by (Repo, Branch). Existing op.Targets entries are preserved if the
// stored state has no matching row (so newly-added targets show up).
func mergeState(op *Op, st Persistent) {
	known := make(map[string]Target, len(st.Targets))
	for _, t := range st.Targets {
		known[t.Repo+"|"+t.Branch] = t
	}
	for i, t := range op.Targets {
		if k, ok := known[t.Repo+"|"+t.Branch]; ok {
			op.Targets[i].PR = k.PR
			op.Targets[i].PRURL = k.PRURL
			op.Targets[i].State = k.State
		}
	}
}

// renderForCreate is Render but uses a fixed "now" placeholder when the
// caller hasn't provided one yet. Pass-through to Render with time.Now() —
// kept as a separate hook so tests can pin the timestamp.
var renderForCreate = func(op Op) (string, error) {
	return Render(op, nowFn())
}

// repoFromPRURL extracts "owner/name" from a PR HTML URL like
// https://github.com/rancher/rancher/pull/1234.
func repoFromPRURL(u string) (string, error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(u, prefix) {
		return "", fmt.Errorf("not a github PR URL: %q", u)
	}
	rest := strings.TrimPrefix(u, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 {
		return "", fmt.Errorf("malformed PR URL: %q", u)
	}
	return parts[0] + "/" + parts[1], nil
}
