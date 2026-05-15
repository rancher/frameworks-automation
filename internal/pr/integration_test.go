//go:build integration

// Integration tests for the bumper's subprocess pipeline. Build-tagged off
// the default suite because they shallow-clone real downstream repos and
// take several minutes; run explicitly with:
//
//	go test -tags=integration -run TestIntegration -timeout=15m ./internal/pr/
//
// Why these exist: the cascade #19 stage-3 failure on rancher/rancher (run
// 25750624366) couldn't be reproduced by any unit test in this package —
// it required a real downstream go.mod, a real `go get`, and the real
// strategy scripts running `go generate ./...` against a real working
// tree to surface the "missing go.sum entry" interaction. Pinning that
// scenario down here is the cheapest insurance against the missing-tidy
// regression coming back the next time someone refactors runGoGet.
package pr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
)

// TestIntegration_BumpCascade19Stage3 replays the exact bundle that
// failed in CI run 25750624366: rancher/rancher pinned at the same
// commit the failed run cloned (dbfeb8d41c72ba7a440f98c6dd34667eeb3e263d),
// driven through Bumper.applyBundle so we exercise the same configure-
// identity → checkout → strategy loop → post-bundle tidy/vendor → commit
// pipeline Bumper.Open runs in production. We stop short of push/CreatePR
// because those would touch the real rancher/rancher.
//
// After the bundle commits, two upstream pre-merge gate scripts run
// against the post-bump tree:
//
//   - .github/scripts/check-for-go-mod-changes.sh — reruns `go mod tidy`
//     and `go mod verify` on `.`, `./pkg/apis`, `./pkg/client`, then
//     asserts `git status --porcelain` is empty. Catches any go.mod /
//     go.sum drift the bumper left behind.
//   - .github/scripts/check-for-auto-generated-changes.sh — reruns
//     `go generate ./...` and asserts `git status --porcelain` is empty.
//     Catches any generator the bump scripts forgot to invoke or that
//     the strategy ordering left half-generated.
//
// Both scripts run during rancher's own CI on every PR, so a bump PR
// that doesn't pass them locally would be rejected upstream. Running
// them here surfaces the rejection in our own CI instead.
//
// Strategies executed (cascade #19 stage-3 dispatch order):
//   - go-get  apiserver           v0.9.4
//   - go-get  norman              v0.9.4
//   - script  remotedialer-proxy  v0.8.0-rc.4   (runs `go generate ./...`)
//   - go-get  steve               v0.9.8
//   - script  webhook             v0.11.0-rc.6  (runs `go generate ./...`)
func TestIntegration_BumpCascade19Stage3(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const (
		repoURL   = "https://github.com/rancher/rancher.git"
		pinnedSHA = "dbfeb8d41c72ba7a440f98c6dd34667eeb3e263d"
	)

	ctx := context.Background()
	repoDir := filepath.Join(t.TempDir(), "repo")

	// Pin rancher/rancher to the exact SHA the failing CI run cloned. A
	// branch-tip clone would drift as rancher main moves; we want this
	// test reading the same go.mod / go.sum / generators forever.
	checkoutAtSHA(t, ctx, repoDir, repoURL, pinnedSHA)

	// applyBundle doesn't touch b.gh or b.tokens, so a Bumper with nil
	// fields is fine. We're explicitly avoiding Open's network-side path.
	b := NewBumper(nil, nil)
	req := Request{
		Repo:       "rancher/rancher",
		BaseBranch: "main",
		HeadBranch: "test-cascade-19-stage-3",
		Modules: []Module{
			{Path: "github.com/rancher/apiserver", Version: "v0.9.4", Strategy: config.StrategyGoGet},
			{Path: "github.com/rancher/norman", Version: "v0.9.4", Strategy: config.StrategyGoGet},
			{Path: "github.com/rancher/remotedialer-proxy", Version: "v0.8.0-rc.4", Strategy: config.StrategyBumpRemotedialerProxy, ChartBranch: "dev-v2.15"},
			{Path: "github.com/rancher/steve", Version: "v0.9.8", Strategy: config.StrategyGoGet},
			{Path: "github.com/rancher/webhook", Version: "v0.11.0-rc.6", Strategy: config.StrategyBumpWebhook, ChartBranch: "dev-v2.15"},
		},
	}
	result, err := b.applyBundle(ctx, repoDir, req)
	if err != nil {
		t.Fatalf("applyBundle: %v "+
			"(if this is 'missing go.sum entry' from a script's go generate, "+
			"runGoGet stopped tidying)", err)
	}
	if result != nil && result.NoOp {
		t.Fatalf("applyBundle returned NoOp — expected a real diff against the pinned commit, got: %s", result.Notes)
	}

	// Run rancher's own pre-merge gates against the committed bump tree.
	// Both inspect `git status --porcelain` after rerunning their
	// canonical regeneration step; any drift means the bumper produced
	// a tree that rancher's CI would reject.
	for _, script := range []string{
		".github/scripts/check-for-go-mod-changes.sh",
		".github/scripts/check-for-auto-generated-changes.sh",
	} {
		runOrFail(t, ctx, repoDir, toolchainEnv(repoDir), script)
	}
}

// TestIntegration_BumpRemotedialerOnRancher replays the production failure
// where rancher/frameworks-automation#43 opened rancher/rancher#55105 with
// the misleading title "Bump dummy/fakek8s to v0.6.1" and a diff that only
// touched gotools/mockery/go.sum. Root cause: DiscoverModules picked up
// rancher/remotedialer's examples/fakek8s/go.mod (alphabetically before the
// root go.mod) and RootModulePath returned "dummy/fakek8s" instead of the
// canonical github.com/rancher/remotedialer.
//
// The test runs the same code path the reconciler uses in production —
// config.Load + DiscoverModules against a real GitHub client, then
// Bumper.applyBundle against rancher/rancher pinned at the SHA the bot
// cloned when it produced the broken PR — and asserts both:
//
//   1. RootModulePath("remotedialer") returns the canonical module.
//   2. The bumper's diff touches root go.mod and go.sum (not just unrelated
//      submodule go.sum churn from the post-bundle tidy).
//
// Pre-fix this fails at step 1; post-fix it passes through step 2.
func TestIntegration_BumpRemotedialerOnRancher(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const (
		rancherURL = "https://github.com/rancher/rancher.git"
		pinnedSHA  = "e69897f9b6b5c5ea986667a19971c38104191908"
		depTag     = "v0.6.1"
	)

	ctx := context.Background()
	repoDir := filepath.Join(t.TempDir(), "repo")
	checkoutAtSHA(t, ctx, repoDir, rancherURL, pinnedSHA)

	cfg, err := config.Load("../../dependencies/rancher.yaml")
	if err != nil {
		t.Fatalf("load dependencies/rancher.yaml: %v", err)
	}
	gh := ghclient.NewClient(ctx, nil) // unauthenticated public-repo reads
	if err := cfg.DiscoverModules(ctx, gh); err != nil {
		t.Fatalf("DiscoverModules: %v", err)
	}
	modPath := cfg.RootModulePath("remotedialer")
	if modPath != "github.com/rancher/remotedialer" {
		t.Fatalf("RootModulePath(remotedialer) = %q, want github.com/rancher/remotedialer "+
			"(if you see %q here, the discovery filter regressed and is picking up "+
			"the examples/fakek8s submodule again)", modPath, modPath)
	}

	b := NewBumper(nil, nil)
	req := Request{
		Repo:       "rancher/rancher",
		BaseBranch: "main",
		HeadBranch: "test-bump-remotedialer-v0.6.1",
		Modules: []Module{
			{Path: modPath, Version: depTag, Strategy: config.StrategyGoGet},
		},
	}
	result, err := b.applyBundle(ctx, repoDir, req)
	if err != nil {
		t.Fatalf("applyBundle: %v", err)
	}
	if result != nil && result.NoOp {
		t.Fatalf("applyBundle returned NoOp — expected a real diff "+
			"(rancher/rancher@%s should be on a remotedialer < %s)", pinnedSHA, depTag)
	}

	changed := captureGitDiffNames(t, ctx, repoDir, "HEAD~1", "HEAD")
	if !slices.Contains(changed, "go.mod") || !slices.Contains(changed, "go.sum") {
		t.Fatalf("expected root go.mod and go.sum in diff, got: %v "+
			"(if the diff is only gotools/<x>/go.sum, the dummy/fakek8s bug is back)", changed)
	}
}

// captureGitDiffNames returns `git diff --name-only <args...>` as a slice of
// paths, one per line, with empties dropped.
func captureGitDiffNames(t *testing.T, ctx context.Context, dir string, args ...string) []string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"diff", "--name-only"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --name-only %v in %s: %v", args, dir, err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// runOrFail executes name+args in dir, streams stdio, and t.Fatalf's on
// non-zero exit. extraEnv is appended to os.Environ() when non-nil.
func runOrFail(t *testing.T, ctx context.Context, dir string, extraEnv []string, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v in %s: %v", name, args, dir, err)
	}
}

// checkoutAtSHA brings down a specific commit of `url` into `dir` without
// pulling the full history. Uses init+fetch rather than `git clone --depth=1`
// because clone --depth=1 only accepts a branch/tag, not a raw commit.
// GitHub allows reachable-SHA fetches by default, which is what makes the
// shallow path work here.
func checkoutAtSHA(t *testing.T, ctx context.Context, dir, url, sha string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	steps := [][]string{
		{"git", "init", "--quiet"},
		{"git", "remote", "add", "origin", url},
		{"git", "fetch", "--depth=1", "--quiet", "origin", sha},
		{"git", "checkout", "--quiet", sha},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v in %s: %v", args, dir, err)
		}
	}
}
