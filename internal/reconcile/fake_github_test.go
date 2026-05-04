package reconcile

import (
	"context"
	"fmt"
	"sync"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// fakeRepoState describes one repo's GitHub state for fixture-driven tests.
type fakeRepoState struct {
	DefaultBranch string                       // branch returned for FetchFile with ref=""
	Tags          []string                     // newest-first; matches real ListReleaseTags ordering
	Branches      map[string]fakeBranchState   // branch name → HEAD state
	TagFiles      map[string]map[string]string // tag → path → content
}

type fakeBranchState struct {
	Files   map[string]string // path → content (HEAD)
	AheadOf map[string]int    // base ref → commits this branch is ahead of base
}

// fakeGH is an in-memory implementation of every GitHub-client method the
// reconcile/cascade/tracker packages use. Issues and PRs created during the
// test live in fakeGH.issues / fakeGH.prs and are inspectable.
type fakeGH struct {
	mu        sync.Mutex
	repos     map[string]*fakeRepoState
	issues    []*ghclient.Issue
	prs       []*ghclient.PR
	nextIssue int
	nextPR    int

	closedPRs       []closedRef
	closedIssues    []closedRef
	deletedBranches []deletedBranchRef
}

type closedRef struct {
	Repo    string
	Number  int
	Comment string
}

type deletedBranchRef struct {
	Repo   string
	Branch string
}

func newFakeGH(repos map[string]*fakeRepoState) *fakeGH {
	if repos == nil {
		repos = map[string]*fakeRepoState{}
	}
	return &fakeGH{
		repos:     repos,
		nextIssue: 100,
		nextPR:    200,
	}
}

func (f *fakeGH) repoOrErr(repo string) (*fakeRepoState, error) {
	s, ok := f.repos[repo]
	if !ok {
		return nil, fmt.Errorf("fake: unknown repo %q", repo)
	}
	return s, nil
}

func (f *fakeGH) FetchFile(ctx context.Context, repo, ref, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.repoOrErr(repo)
	if err != nil {
		return "", err
	}
	if ref == "" {
		ref = s.DefaultBranch
	}
	if br, ok := s.Branches[ref]; ok {
		if c, ok := br.Files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("fake: %s@%s: file %q not found", repo, ref, path)
	}
	if files, ok := s.TagFiles[ref]; ok {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("fake: %s@%s: file %q not found", repo, ref, path)
	}
	return "", fmt.Errorf("fake: %s: ref %q not found", repo, ref)
}

func (f *fakeGH) GetLatestReleaseTag(ctx context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.repoOrErr(repo)
	if err != nil {
		return "", err
	}
	if len(s.Tags) == 0 {
		return "", nil
	}
	return s.Tags[0], nil
}

func (f *fakeGH) ListReleaseTags(ctx context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.repoOrErr(repo)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(s.Tags))
	copy(out, s.Tags)
	return out, nil
}

func (f *fakeGH) CommitsAheadOf(ctx context.Context, repo, base, head string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.repoOrErr(repo)
	if err != nil {
		return 0, err
	}
	br, ok := s.Branches[head]
	if !ok {
		return 0, fmt.Errorf("fake: %s: head branch %q not found", repo, head)
	}
	return br.AheadOf[base], nil
}

func (f *fakeGH) GetPR(ctx context.Context, repo string, number int) (*ghclient.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.Number == number {
			cp := *p
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("fake: %s#%d: PR not found", repo, number)
}

func (f *fakeGH) ClosePR(ctx context.Context, repo string, number int, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedPRs = append(f.closedPRs, closedRef{Repo: repo, Number: number, Comment: comment})
	for _, p := range f.prs {
		if p.Number == number {
			p.State = "closed"
		}
	}
	return nil
}

func (f *fakeGH) DeleteBranch(ctx context.Context, repo, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedBranches = append(f.deletedBranches, deletedBranchRef{Repo: repo, Branch: branch})
	return nil
}

func (f *fakeGH) ListOpenIssues(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ghclient.Issue
	for _, i := range f.issues {
		if i.State != "open" {
			continue
		}
		if !hasAllLabels(i.Labels, labels) {
			continue
		}
		cp := *i
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeGH) ListIssuesAllStates(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ghclient.Issue
	for _, i := range f.issues {
		if !hasAllLabels(i.Labels, labels) {
			continue
		}
		cp := *i
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeGH) CreateIssue(ctx context.Context, repo, title, body string, labels, assignees []string) (*ghclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextIssue++
	issue := &ghclient.Issue{
		Number: f.nextIssue,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
		URL:    fmt.Sprintf("https://github.com/%s/issues/%d", repo, f.nextIssue),
	}
	f.issues = append(f.issues, issue)
	cp := *issue
	return &cp, nil
}

func (f *fakeGH) UpdateIssueBody(ctx context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.issues {
		if i.Number == number {
			i.Body = body
			return nil
		}
	}
	return fmt.Errorf("fake: %s#%d: issue not found", repo, number)
}

func (f *fakeGH) CloseIssue(ctx context.Context, repo string, number int, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedIssues = append(f.closedIssues, closedRef{Repo: repo, Number: number, Comment: comment})
	for _, i := range f.issues {
		if i.Number == number {
			i.State = "closed"
		}
	}
	return nil
}

func hasAllLabels(have, want []string) bool {
	set := map[string]bool{}
	for _, l := range have {
		set[l] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// snapshotIssues returns a deep copy of every issue currently in the fake.
func (f *fakeGH) snapshotIssues() []ghclient.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ghclient.Issue, len(f.issues))
	for i, p := range f.issues {
		out[i] = *p
	}
	return out
}

// registerPR makes a PR (typically just opened by the fake bumper) findable
// via GetPR/ListOpenPRsByHead so subsequent reconciler passes can poll it.
func (f *fakeGH) registerPR(p *ghclient.PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *p
	f.prs = append(f.prs, &cp)
}
