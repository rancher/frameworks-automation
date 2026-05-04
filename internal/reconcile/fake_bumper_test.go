package reconcile

import (
	"context"
	"fmt"
	"sync"

	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/pr"
)

// fakeBumper records every Open call and returns a synthetic open PR by
// default. A scripted result keyed by (repo, base branch) overrides the
// default — used to simulate NoOp bumps when the downstream is already at
// the target version.
type fakeBumper struct {
	mu     sync.Mutex
	gh     *fakeGH
	nextPR int
	calls  []pr.Request
	script map[string]*pr.Result
}

func newFakeBumper(gh *fakeGH) *fakeBumper {
	return &fakeBumper{gh: gh, nextPR: 300, script: map[string]*pr.Result{}}
}

// scriptKey is the lookup key for the scripted-result map.
func bumperKey(repo, base string) string { return repo + "|" + base }

// scriptNoOp registers a NoOp result for the next Open call matching
// (repo, base) — useful for "branch already at target" scenarios.
func (b *fakeBumper) scriptNoOp(repo, base string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.script[bumperKey(repo, base)] = &pr.Result{NoOp: true, Notes: "scripted no-op"}
}

func (b *fakeBumper) Open(ctx context.Context, req pr.Request) (*pr.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, req)
	if scripted, ok := b.script[bumperKey(req.Repo, req.BaseBranch)]; ok {
		return scripted, nil
	}
	b.nextPR++
	prObj := &ghclient.PR{
		Number:  b.nextPR,
		Title:   fmt.Sprintf("Bump %d dependencies on %s", len(req.Modules), req.BaseBranch),
		State:   "open",
		HeadRef: req.HeadBranch,
		BaseRef: req.BaseBranch,
		URL:     fmt.Sprintf("https://github.com/%s/pull/%d", req.Repo, b.nextPR),
	}
	if b.gh != nil {
		b.gh.registerPR(prObj)
	}
	return &pr.Result{PR: prObj, Notes: fmt.Sprintf("opened PR #%d", b.nextPR)}, nil
}

// snapshotCalls returns the recorded Open calls in order.
func (b *fakeBumper) snapshotCalls() []pr.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]pr.Request, len(b.calls))
	copy(out, b.calls)
	return out
}
