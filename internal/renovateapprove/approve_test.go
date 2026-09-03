package renovateapprove

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// fakeGH is an in-memory gitHubClient for tests. Keyed by repo so a test can
// set up multiple repos with independent PR sets.
type fakeGH struct {
	prs        map[string][]*ghclient.PR
	checkRuns  map[string]map[string][]ghclient.CheckRun // repo -> sha -> runs
	approved   map[string]map[int]bool                   // repo -> number -> already approved
	listErr    map[string]error                          // repo -> error from ListOpenPRs
	checksErr  map[string]error                          // repo -> error from ListCheckRuns
	approveErr map[string]error                          // repo -> error from ApprovePR

	approveCalls []approveCall
	checkRefs    []string // refs passed to ListCheckRuns, in call order
}

type approveCall struct {
	Repo   string
	Number int
	SHA    string
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		prs:        map[string][]*ghclient.PR{},
		checkRuns:  map[string]map[string][]ghclient.CheckRun{},
		approved:   map[string]map[int]bool{},
		listErr:    map[string]error{},
		checksErr:  map[string]error{},
		approveErr: map[string]error{},
	}
}

func (f *fakeGH) ListOpenPRs(ctx context.Context, repo string) ([]*ghclient.PR, error) {
	if err := f.listErr[repo]; err != nil {
		return nil, err
	}
	return f.prs[repo], nil
}

func (f *fakeGH) ListCheckRuns(ctx context.Context, repo, ref string) ([]ghclient.CheckRun, error) {
	if err := f.checksErr[repo]; err != nil {
		return nil, err
	}
	f.checkRefs = append(f.checkRefs, ref)
	return f.checkRuns[repo][ref], nil
}

func (f *fakeGH) HasApproval(ctx context.Context, repo string, number int) (bool, error) {
	return f.approved[repo][number], nil
}

func (f *fakeGH) ApprovePR(ctx context.Context, repo string, number int, sha, body string) error {
	if err := f.approveErr[repo]; err != nil {
		return err
	}
	f.approveCalls = append(f.approveCalls, approveCall{Repo: repo, Number: number, SHA: sha})
	return nil
}

func testConfig() *Config {
	c := &Config{
		Repos: []RepoConfig{
			{Repo: "rancher/steve", TokenEnv: "GH_TOKEN_STEVE"},
		},
	}
	c.applyDefaults()
	return c
}

func greenRuns() []ghclient.CheckRun {
	return []ghclient.CheckRun{
		{Name: "ci", Status: "completed", Conclusion: "success"},
	}
}

// basePR returns an otherwise-eligible PR: right author, right branch
// prefix, auto-merge requested, old enough (10 days), green checks waiting
// to be wired up by the caller via fakeGH.
func basePR(number int, now time.Time) *ghclient.PR {
	return &ghclient.PR{
		Number:    number,
		Title:     fmt.Sprintf("Update dependency foo to v1.2.%d", number),
		Author:    "renovate-rancher[bot]",
		HeadRef:   "renovate/main-foo-1.2." + fmt.Sprint(number),
		HeadSHA:   fmt.Sprintf("sha-%d", number),
		AutoMerge: true,
		CreatedAt: now.AddDate(0, 0, -10),
	}
}

func TestRun_ApprovesEligiblePR(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) // Thursday
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}

	res := Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 1 || gh.approveCalls[0] != (approveCall{Repo: "rancher/steve", Number: 1, SHA: pr.HeadSHA}) {
		t.Fatalf("expected PR #1 to be approved, got calls %+v", gh.approveCalls)
	}
	approved := res.Approved()
	if len(approved) != 1 || !approved[0].Approved {
		t.Fatalf("expected one approved outcome, got %+v", res.Outcomes)
	}
	if res.Failed != 0 {
		t.Fatalf("expected no failures, got %d", res.Failed)
	}
}

func TestRun_SkipsWrongAuthor(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	pr.Author = "some-human"
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}

	res := Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals, got %+v", gh.approveCalls)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Reason == "" {
		t.Fatalf("expected a skip reason, got %+v", res.Outcomes)
	}
}

func TestRun_SkipsNonRenovateBranch(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	pr.HeadRef = "some-feature-branch"
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals, got %+v", gh.approveCalls)
	}
}

func TestRun_SkipsWithoutAutoMerge(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	pr.AutoMerge = false
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals, got %+v", gh.approveCalls)
	}
}

func TestRun_SkipsTooNew(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) // Thursday
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	pr.CreatedAt = now // opened today: zero working days old
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals for a same-day PR, got %+v", gh.approveCalls)
	}
}

func TestRun_ForceSkipsAgeGate(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	pr.CreatedAt = now // opened today
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}

	res := Run(context.Background(), gh, cfg, now, true /* force */)

	if len(gh.approveCalls) != 1 {
		t.Fatalf("expected force to bypass the age gate, got calls %+v", gh.approveCalls)
	}
	_ = res
}

func TestRun_SkipsWhenChecksNotGreen(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{
		pr.HeadSHA: {{Name: "ci", Status: "completed", Conclusion: "failure"}},
	}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals with failing checks, got %+v", gh.approveCalls)
	}
}

func TestRun_SkipsWhenChecksPending(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{
		pr.HeadSHA: {{Name: "ci", Status: "in_progress", Conclusion: ""}},
	}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals while checks are still running, got %+v", gh.approveCalls)
	}
}

func TestRun_SkipsWhenNoChecksAtAll(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	// No entry in gh.checkRuns at all — zero check runs reported.

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no approvals with zero check runs, got %+v", gh.approveCalls)
	}
}

func TestRun_SkipsAlreadyApproved(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}
	gh.approved["rancher/steve"] = map[int]bool{1: true}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.approveCalls) != 0 {
		t.Fatalf("expected no duplicate approval, got %+v", gh.approveCalls)
	}
}

func TestRun_OneRepoFailureDoesNotBlockOthers(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := &Config{
		Repos: []RepoConfig{
			{Repo: "rancher/broken", TokenEnv: "GH_TOKEN_BROKEN"},
			{Repo: "rancher/steve", TokenEnv: "GH_TOKEN_STEVE"},
		},
	}
	cfg.applyDefaults()
	gh.listErr["rancher/broken"] = errors.New("boom")
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}

	res := Run(context.Background(), gh, cfg, now, false)

	if res.Failed != 1 {
		t.Fatalf("expected exactly one repo failure, got %d", res.Failed)
	}
	if len(gh.approveCalls) != 1 {
		t.Fatalf("expected the healthy repo to still be swept, got %+v", gh.approveCalls)
	}
}

// TestRun_PinsApprovalToReviewedSHA guards the TOCTOU gap: the review has to
// name the same commit whose checks were inspected, so a Renovate force-push
// landing mid-sweep can't inherit an approval it never earned.
func TestRun_PinsApprovalToReviewedSHA(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	gh := newFakeGH()
	cfg := testConfig()
	pr := basePR(1, now)
	gh.prs["rancher/steve"] = []*ghclient.PR{pr}
	gh.checkRuns["rancher/steve"] = map[string][]ghclient.CheckRun{pr.HeadSHA: greenRuns()}

	Run(context.Background(), gh, cfg, now, false)

	if len(gh.checkRefs) != 1 {
		t.Fatalf("expected exactly one check-runs lookup, got %+v", gh.checkRefs)
	}
	if len(gh.approveCalls) != 1 {
		t.Fatalf("expected exactly one approval, got %+v", gh.approveCalls)
	}
	if gh.approveCalls[0].SHA != gh.checkRefs[0] {
		t.Errorf("approved SHA %q != reviewed SHA %q", gh.approveCalls[0].SHA, gh.checkRefs[0])
	}
	if gh.approveCalls[0].SHA != pr.HeadSHA {
		t.Errorf("approved SHA = %q, want PR head %q", gh.approveCalls[0].SHA, pr.HeadSHA)
	}
}

func TestWorkingDaysBetween(t *testing.T) {
	// Thursday 2026-08-13.
	now := time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		want    int
	}{
		{"same day", now, 0},
		{"yesterday (Wed)", now.AddDate(0, 0, -1), 1},
		{"Monday this week", time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC), 3}, // Mon, Tue, Wed
		{"over the weekend", time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC), 4},  // Fri..Wed weekdays, weekend excluded
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workingDaysBetween(tt.created, now)
			if got != tt.want {
				t.Errorf("workingDaysBetween(%s, %s) = %d, want %d", tt.created, now, got, tt.want)
			}
		})
	}
}
