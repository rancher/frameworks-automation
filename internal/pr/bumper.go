// Package pr opens a single bump PR end-to-end: clone the downstream repo,
// run `go get module@version` (+ `go mod tidy`, + `go mod vendor` if a
// vendor/ tree exists), commit, push, and open the PR via the github client.
//
// One Bumper instance per reconciler run. Each Open() call works in its own
// temp dir which is cleaned up on return.
package pr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
)

const (
	commitAuthorName  = "release-automation"
	commitAuthorEmail = "release-automation@users.noreply.github.com"
)

type Bumper struct {
	gh     *ghclient.Client
	tokens map[string]string // owner/name → fine-grained token, used in the git clone URL
}

func NewBumper(gh *ghclient.Client, tokens map[string]string) *Bumper {
	return &Bumper{gh: gh, tokens: tokens}
}

// Request describes a single bump-PR job. Modules may carry one entry (the
// regular bump path) or several (cascade stages bundle every dep that lands
// at a layer into one PR — see internal/cascade).
type Request struct {
	Repo       string   // downstream owner/name, e.g. "rancher/rancher"
	BaseBranch string   // e.g. "main", "release/v2.13"
	HeadBranch string   // e.g. "automation/bump-steve-v0.7.5"
	Modules    []Module // one or more (path, version) pairs to bump
	// Fork, when non-empty, is the "owner/name" of a fork to push the head
	// branch to. The PR is still opened against Repo (cross-repo PR). When
	// empty, head is pushed to origin (Repo itself) as before.
	Fork string
	// TrackerURL is included in the PR body so reviewers can find the op.
	TrackerURL string
	// Assignees lists GitHub logins to assign to the opened PR.
	Assignees []string
	// PostBundle is the ordered list of post-bundle hooks to run after the
	// strategies and tidy pass complete and before the commit. Sourced from
	// downstream-repo config.
	PostBundle []config.PostBundleHook
	// SyncModules is input for the sync-deps post-bundle hook: module paths
	// to align across every go.mod in the repo. Ignored when sync-deps isn't
	// in PostBundle.
	SyncModules []string
}

// Module is one (Go module path, target version) pair within a Request.
// Strategy picks the registered procedure that mutates the working tree;
// empty defaults to go-get for parity with the legacy single-strategy world.
type Module struct {
	Path     string          // e.g. "github.com/rancher/steve"
	Version  string          // e.g. "v0.7.5"
	Strategy config.Strategy // empty == config.StrategyGoGet

	// ChartRef, when non-empty, is exposed to script strategies via the
	// CHART_REF environment variable. Used by bump-webhook and
	// bump-remotedialer-proxy to resolve `<chart>+up<dep>` from
	// rancher/charts' index.yaml at that ref. Any git ref works (branch,
	// tag, or SHA) — production passes a branch name (e.g. "dev-v2.15"),
	// integration tests pin a SHA for reproducibility. Other strategies
	// ignore it.
	ChartRef string
}

type Result struct {
	PR    *ghclient.PR
	NoOp  bool   // already at requested version; no PR opened
	Reuse bool   // a PR for HeadBranch already existed; returned as-is
	Notes string // human-readable summary for logging
	// Bumped is the subset of Request.Modules whose strategies actually
	// changed the tree (each got its own commit, in original order).
	// Modules whose strategy silently no-op'd because the downstream was
	// already at target are dropped. Empty on NoOp / Reuse; populated on
	// the happy path. The PR title and body are built from this slice so
	// they describe the real diff instead of the intended bundle.
	Bumped []Module
}

// ErrNotAGoModule is returned when a go-get strategy is requested but the
// cloned repo has no go.mod at the root.
var ErrNotAGoModule = errors.New("repo has no go.mod at root")


// Open executes the bump end-to-end. Returns Result.NoOp when go.mod was
// already at the requested version (no commit, no PR). Returns Result.Reuse
// when an open PR with HeadBranch already exists in the downstream repo.
func (b *Bumper) Open(ctx context.Context, req Request) (*Result, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}

	if existing, err := b.findExistingPR(ctx, req); err != nil {
		return nil, err
	} else if existing != nil {
		return &Result{PR: existing, Reuse: true,
			Notes: fmt.Sprintf("existing open PR #%d for %s found; reusing", existing.Number, req.HeadBranch)}, nil
	}

	work, cleanup, err := mktemp()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	repoDir := filepath.Join(work, "repo")
	if err := b.clone(ctx, req.Repo, req.BaseBranch, repoDir); err != nil {
		return nil, err
	}

	result, err := b.applyBundle(ctx, repoDir, req)
	if err != nil {
		return nil, err
	}
	if result.NoOp {
		return result, nil
	}

	pushRemote := "origin"
	prHead := req.HeadBranch
	if req.Fork != "" {
		forkToken, ok := b.tokens[req.Fork]
		if !ok || forkToken == "" {
			return nil, fmt.Errorf("no token configured for fork %s", req.Fork)
		}
		forkURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", forkToken, req.Fork)
		if err := run(ctx, repoDir, nil, "git", "remote", "add", "fork", forkURL); err != nil {
			return nil, b.scrubAllTokens(err)
		}
		pushRemote = "fork"
		forkOwner := strings.SplitN(req.Fork, "/", 2)[0]
		prHead = forkOwner + ":" + req.HeadBranch
	}
	if err := run(ctx, repoDir, nil, "git", "push", "-u", pushRemote, req.HeadBranch); err != nil {
		return nil, b.scrubAllTokens(err)
	}

	// PR title / body describe result.Bumped (the modules that actually
	// landed), not req.Modules (the intended bundle), so a no-op'd
	// strategy doesn't get listed.
	pr, err := b.gh.CreatePR(ctx, req.Repo,
		commitTitle(result.Bumped),
		buildPRBody(req, result.Bumped),
		prHead,
		req.BaseBranch,
	)
	if err != nil {
		return nil, fmt.Errorf("create PR %s %s -> %s: %w", req.Repo, req.HeadBranch, req.BaseBranch, err)
	}
	if len(req.Assignees) > 0 {
		if err := b.gh.AddPRAssignees(ctx, req.Repo, pr.Number, req.Assignees); err != nil {
			return nil, fmt.Errorf("assign PR %s#%d: %w", req.Repo, pr.Number, err)
		}
	}
	result.PR = pr
	result.Notes = fmt.Sprintf("opened PR #%d", pr.Number)
	return result, nil
}

// applyBundle is the local-only middle of Open: configure git identity,
// branch off, run every strategy in req.Modules, run the post-bundle
// tidy/vendor pass, and commit per step. Pure working-tree mutation —
// no network, no GitHub API.
//
// Carved out so integration tests can replay the exact pipeline against
// a pre-cloned tree without triggering Open's push + CreatePR (which
// would push test branches to the real downstream and open real PRs).
//
// Per-strategy commits: each strategy that actually changes the tree
// gets its own commit (e.g. "Bump github.com/rancher/norman to v0.9.4");
// strategies that silently no-op produce no commit and drop out of
// Result.Bumped. The post-bundle tidy/vendor/PostBundle pass becomes
// its own trailing commit when it produces a diff. The PR is opened
// with these commits intact — reviewers see a per-step trail, and the
// merge strategy on the downstream (squash on rancher repos) collapses
// them at merge time.
//
// Always returns a non-nil Result on success: NoOp=true when no strategy
// produced changes (caller should skip push); otherwise Bumped holds the
// subset of req.Modules that actually committed and the caller proceeds
// with push + PR using Bumped for the title/body. If post-bundle dirtied
// the tree but every strategy no-op'd, the bundle is still NoOp — there
// is no module to credit the changes to and an orphan housekeeping PR
// is not useful.
func (b *Bumper) applyBundle(ctx context.Context, repoDir string, req Request) (*Result, error) {
	if err := configureIdentity(ctx, repoDir); err != nil {
		return nil, err
	}
	if err := run(ctx, repoDir, nil, "git", "checkout", "-b", req.HeadBranch); err != nil {
		return nil, err
	}

	hasGoMod := fileExists(filepath.Join(repoDir, "go.mod"))
	var bumped []Module
	for _, m := range req.Modules {
		strat := m.Strategy
		if strat == "" {
			strat = config.StrategyGoGet
		}
		if strat == config.StrategyGoGet && !hasGoMod {
			return nil, fmt.Errorf("%s: %w", req.Repo, ErrNotAGoModule)
		}
		impl, err := lookupStrategy(strat)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", req.Repo, m.Path, err)
		}
		if err := impl.Apply(ctx, repoDir, m); err != nil {
			return nil, err
		}
		// Per-strategy detection: stage and check if the index now differs
		// from HEAD. Empty index → strategy silently no-op'd (the downstream
		// was already at the target version); skip the commit so it drops
		// out of the PR's history and Bumped.
		committed, err := stageAndCommit(ctx, repoDir, commitMessage([]Module{m}))
		if err != nil {
			return nil, err
		}
		if committed {
			bumped = append(bumped, m)
		}
	}
	// Post-bundle Go housekeeping. The hasGoMod gate skips non-Go repos
	// (e.g. chart repos); within Go repos every go.mod found under repoDir
	// (vendor/ excluded) is tidied and vendored so sub-modules stay consistent.
	// Commit the whole pass as one step — splitting per-dir would be 2N commits
	// of noise (root + pkg/apis + pkg/client × tidy + vendor on rancher).
	if hasGoMod {
		dirs, err := findGoModDirs(repoDir)
		if err != nil {
			return nil, fmt.Errorf("find go.mod files for tidy: %w", err)
		}
		for _, dir := range dirs {
			if err := run(ctx, dir, nil, "go", "mod", "tidy"); err != nil {
				return nil, err
			}
			if hasVendor(dir) {
				if err := run(ctx, dir, nil, "go", "mod", "vendor"); err != nil {
					return nil, err
				}
			}
		}
		if len(bumped) > 0 {
			if _, err := stageAndCommit(ctx, repoDir, "go mod tidy / vendor"); err != nil {
				return nil, err
			}
		}
	}

	// One commit per PostBundle hook so each has its own audit-trail entry.
	for _, name := range req.PostBundle {
		h, err := lookupPostBundleHook(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Repo, err)
		}
		if err := h.Apply(ctx, repoDir, req); err != nil {
			return nil, fmt.Errorf("%s post-bundle %s: %w", req.Repo, name, err)
		}
		if len(bumped) > 0 {
			if _, err := stageAndCommit(ctx, repoDir, fmt.Sprintf("Post-bundle: %s", name)); err != nil {
				return nil, err
			}
		}
	}

	if len(bumped) == 0 {
		return &Result{NoOp: true,
			Notes: fmt.Sprintf("%s already at %s; nothing to commit", req.Repo, summarizeModules(req.Modules))}, nil
	}
	return &Result{Bumped: bumped}, nil
}

// stageAndCommit stages every working-tree change in dir and commits it
// under `msg`. Returns (false, nil) when the staging step produces no
// diff vs HEAD — the strategy / hook was a silent no-op. Returns (true,
// nil) on a successful commit.
func stageAndCommit(ctx context.Context, dir, msg string) (bool, error) {
	if err := run(ctx, dir, nil, "git", "add", "-A"); err != nil {
		return false, err
	}
	dirty, err := hasStagedChanges(ctx, dir)
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil
	}
	if err := run(ctx, dir, nil, "git", "commit", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// hasStagedChanges reports whether the index differs from HEAD. Uses
// `git diff --cached --quiet`, which exits 0 with no diff, 1 with a
// diff present, and >1 on a real error.
func hasStagedChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached --quiet in %s: %w", dir, err)
}

func (b *Bumper) findExistingPR(ctx context.Context, req Request) (*ghclient.PR, error) {
	head := req.HeadBranch
	if req.Fork != "" {
		forkOwner := strings.SplitN(req.Fork, "/", 2)[0]
		head = forkOwner + ":" + req.HeadBranch
	} else {
		repoOwner := strings.SplitN(req.Repo, "/", 2)[0]
		head = repoOwner + ":" + req.HeadBranch
	}
	prs, err := b.gh.ListOpenPRsByHead(ctx, req.Repo, head)
	if err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return prs[0], nil
}

func (b *Bumper) clone(ctx context.Context, repo, branch, dir string) error {
	token, ok := b.tokens[repo]
	if !ok || token == "" {
		return fmt.Errorf("no token configured for %s", repo)
	}
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, repo)
	// --depth=1 keeps the clone fast; we don't need history to bump a dep.
	if err := run(ctx, "", nil, "git", "clone", "--depth=1", "--branch="+branch, url, dir); err != nil {
		// Don't leak the token in the error.
		return fmt.Errorf("clone %s@%s: %w", repo, branch, b.scrubAllTokens(err))
	}
	return nil
}

func (req Request) validate() error {
	switch {
	case req.Repo == "":
		return errors.New("Repo is required")
	case req.BaseBranch == "":
		return errors.New("BaseBranch is required")
	case req.HeadBranch == "":
		return errors.New("HeadBranch is required")
	case len(req.Modules) == 0:
		return errors.New("Modules is required")
	}
	for i, m := range req.Modules {
		if m.Path == "" {
			return fmt.Errorf("Modules[%d].Path is required", i)
		}
		if m.Version == "" {
			return fmt.Errorf("Modules[%d].Version is required", i)
		}
	}
	return nil
}

// --- shell helpers ----------------------------------------------------------

func mktemp() (string, func(), error) {
	dir, err := os.MkdirTemp("", "release-automation-*")
	if err != nil {
		return "", nil, fmt.Errorf("mktemp: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func configureIdentity(ctx context.Context, dir string) error {
	if err := run(ctx, dir, nil, "git", "config", "user.name", commitAuthorName); err != nil {
		return err
	}
	return run(ctx, dir, nil, "git", "config", "user.email", commitAuthorEmail)
}

func runGoGet(ctx context.Context, dir, module, version string) error {
	env := append(toolchainEnv(dir), "GOFLAGS=-mod=mod")
	if err := run(ctx, dir, env, "go", "get", module+"@"+version); err != nil {
		return err
	}
	// Tidy after every go-get so each one leaves go.mod/go.sum
	// internally consistent. Without this, a follow-up strategy that
	// reads go.sum (e.g. a script that runs `go generate`) trips on
	// missing transitive entries `go get` doesn't necessarily add —
	// the post-bundle tidy at the end of Bumper.Open is too late for
	// that case. Keep -mod=mod for the same vendored-downstream reason
	// as the get above.
	return run(ctx, dir, env, "go", "mod", "tidy")
}

func hasVendor(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "vendor"))
	return err == nil && st.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// run executes argv in dir, streaming output for visibility in CI logs.
// Extra env entries (KEY=VALUE) are appended to os.Environ.
func run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (b *Bumper) scrubAllTokens(err error) error {
	s := err.Error()
	for _, t := range b.tokens {
		if t == "" {
			continue
		}
		s = strings.ReplaceAll(s, t, "***")
	}
	if s == err.Error() {
		return err
	}
	return errors.New(s)
}

// buildPRBody renders the PR description from the modules that actually
// landed (passed in, not read from req.Modules) plus the rest of the
// request's metadata. Taking the modules separately lets Open feed in
// Result.Bumped so the body describes the real diff.
func buildPRBody(req Request, mods []Module) string {
	var b strings.Builder
	if len(mods) == 1 {
		m := mods[0]
		fmt.Fprintf(&b, "Automated bump of `%s` to `%s` on `%s`.\n\n", m.Path, m.Version, req.BaseBranch)
	} else {
		fmt.Fprintf(&b, "Automated bump of %d dependencies on `%s`:\n\n", len(mods), req.BaseBranch)
		for _, m := range mods {
			fmt.Fprintf(&b, "- `%s` to `%s`\n", m.Path, m.Version)
		}
		b.WriteString("\n")
	}
	if req.TrackerURL != "" {
		fmt.Fprintf(&b, "Tracker: %s\n\n", req.TrackerURL)
	}
	b.WriteString("This PR was opened by the release-automation reconciler. ")
	b.WriteString("CI will run on push; review and merge as usual.\n")
	return b.String()
}

// commitTitle is the first line of the commit / PR title. Single-module case
// preserves the legacy "Bump <module> to <version>" string for parity with
// the regular bump-op path.
func commitTitle(mods []Module) string {
	if len(mods) == 1 {
		return fmt.Sprintf("Bump %s to %s", mods[0].Path, mods[0].Version)
	}
	return fmt.Sprintf("Bump %d dependencies", len(mods))
}

func commitMessage(mods []Module) string {
	if len(mods) == 1 {
		return commitTitle(mods)
	}
	var b strings.Builder
	b.WriteString(commitTitle(mods))
	b.WriteString("\n\n")
	for _, m := range mods {
		fmt.Fprintf(&b, "- %s to %s\n", m.Path, m.Version)
	}
	return b.String()
}

func summarizeModules(mods []Module) string {
	if len(mods) == 1 {
		return fmt.Sprintf("%s@%s", mods[0].Path, mods[0].Version)
	}
	parts := make([]string, len(mods))
	for i, m := range mods {
		parts[i] = fmt.Sprintf("%s@%s", m.Path, m.Version)
	}
	return strings.Join(parts, ", ")
}
