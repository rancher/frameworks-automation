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
	"testing"

	"github.com/rancher/release-automation/internal/config"
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
			{Path: "github.com/rancher/remotedialer-proxy", Version: "v0.8.0-rc.4", Strategy: config.StrategyBumpRemotedialerProxy},
			{Path: "github.com/rancher/steve", Version: "v0.9.8", Strategy: config.StrategyGoGet},
			{Path: "github.com/rancher/webhook", Version: "v0.11.0-rc.6", Strategy: config.StrategyBumpWebhook},
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

// TestIntegration_BumpRancherSyncDeps replays the single-dep bump that
// surfaced the cross-go.mod drift this hook exists to fix
// (frameworks-automation #38: bumping steve to v0.7.46 against rancher
// 2782f51671cae849ae06c9c38f7028a762a6ad32). At that SHA root and
// pkg/apis/go.mod both pin norman v0.7.2; steve@v0.7.46 transitively
// requires norman v0.7.3, so the post-bundle tidy advances root to
// v0.7.3 while pkg/apis stays at v0.7.2. Rancher's own
// .github/scripts/check-for-go-mod-changes.sh then rejects the tree
// with "Diff found between ./go.mod and ./pkg/apis/go.mod".
//
// Same applyBundle path as the cascade-19 test (no push, no CreatePR)
// but with PostBundle and SyncModules populated as the reconciler would
// in production via downstream.PostBundle + Config.SyncModulesFor.
//
// Verified on this SHA: with PostBundle set to nil the validator fails
// with "github.com/rancher/norman is different ('v0.7.2' vs 'v0.7.3')";
// with the sync-deps hook enabled it passes because pkg/apis is fanned
// out to root's resolved norman version.
func TestIntegration_BumpRancherSyncDeps(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const (
		repoURL   = "https://github.com/rancher/rancher.git"
		pinnedSHA = "2782f51671cae849ae06c9c38f7028a762a6ad32"
	)

	ctx := context.Background()
	repoDir := filepath.Join(t.TempDir(), "repo")
	checkoutAtSHA(t, ctx, repoDir, repoURL, pinnedSHA)

	b := NewBumper(nil, nil)
	req := Request{
		Repo:       "rancher/rancher",
		BaseBranch: "main",
		HeadBranch: "test-rancher-sync-deps",
		Modules: []Module{
			{Path: "github.com/rancher/steve", Version: "v0.7.46", Strategy: config.StrategyGoGet},
		},
		PostBundle: []config.PostBundleHook{config.PostBundleSyncDeps},
		// Mirrors what config.SyncModulesFor("rancher") produces in
		// production: every module path published by every dep in rancher's
		// deps list. Modules absent from a sub-module's go.mod are skipped
		// by the hook, so listing the full set is harmless.
		SyncModules: []string{
			"github.com/rancher/apiserver",
			"github.com/rancher/norman",
			"github.com/rancher/remotedialer-proxy",
			"github.com/rancher/steve",
			"github.com/rancher/webhook",
			"github.com/rancher/wrangler/v3",
		},
	}
	result, err := b.applyBundle(ctx, repoDir, req)
	if err != nil {
		t.Fatalf("applyBundle: %v", err)
	}
	if result != nil && result.NoOp {
		t.Fatalf("applyBundle returned NoOp — expected a real diff against the pinned commit, got: %s", result.Notes)
	}

	// check-for-go-mod-changes.sh both re-tidies/verifies each go.mod and
	// asserts the rancher-family deps in pkg/apis/go.mod match the versions
	// in root go.mod. Without sync-deps this fails with "Diff found between
	// ./go.mod and ./pkg/apis/go.mod" on wrangler/v3 — confirmed by toggling
	// PostBundle to nil locally on this SHA.
	runOrFail(t, ctx, repoDir, toolchainEnv(repoDir), ".github/scripts/check-for-go-mod-changes.sh")
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
