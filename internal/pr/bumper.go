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
	gh    *ghclient.Client
	token string // PAT/App token, used in the git clone URL
}

func NewBumper(gh *ghclient.Client, token string) *Bumper {
	return &Bumper{gh: gh, token: token}
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
}

// Module is one (Go module path, target version) pair within a Request.
// Strategy picks the registered procedure that mutates the working tree;
// empty defaults to go-get for parity with the legacy single-strategy world.
type Module struct {
	Path     string          // e.g. "github.com/rancher/steve"
	Version  string          // e.g. "v0.7.5"
	Strategy config.Strategy // empty == config.StrategyGoGet
}

type Result struct {
	PR    *ghclient.PR
	NoOp  bool   // already at requested version; no PR opened
	Reuse bool   // a PR for HeadBranch already existed; returned as-is
	Notes string // human-readable summary for logging
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
	if err := configureIdentity(ctx, repoDir); err != nil {
		return nil, err
	}
	if err := run(ctx, repoDir, nil, "git", "checkout", "-b", req.HeadBranch); err != nil {
		return nil, err
	}

	hasGoMod := fileExists(filepath.Join(repoDir, "go.mod"))
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
	}
	// Post-bundle Go housekeeping. The hasGoMod gate skips non-Go repos
	// (e.g. chart repos); within Go repos every go.mod found under repoDir
	// (vendor/ excluded) is tidied and vendored so sub-modules stay consistent.
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
	}

	dirty, err := hasChanges(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	if !dirty {
		return &Result{NoOp: true,
			Notes: fmt.Sprintf("%s already at %s; nothing to commit", req.Repo, summarizeModules(req.Modules))}, nil
	}

	if err := run(ctx, repoDir, nil, "git", "add", "-A"); err != nil {
		return nil, err
	}
	title := commitTitle(req.Modules)
	if err := run(ctx, repoDir, nil, "git", "commit", "-m", commitMessage(req.Modules)); err != nil {
		return nil, err
	}

	pushRemote := "origin"
	prHead := req.HeadBranch
	if req.Fork != "" {
		forkURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", b.token, req.Fork)
		if err := run(ctx, repoDir, nil, "git", "remote", "add", "fork", forkURL); err != nil {
			return nil, scrubToken(err, b.token)
		}
		pushRemote = "fork"
		forkOwner := strings.SplitN(req.Fork, "/", 2)[0]
		prHead = forkOwner + ":" + req.HeadBranch
	}
	if err := run(ctx, repoDir, nil, "git", "push", "-u", pushRemote, req.HeadBranch); err != nil {
		return nil, scrubToken(err, b.token)
	}

	pr, err := b.gh.CreatePR(ctx, req.Repo,
		title,
		buildPRBody(req),
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
	return &Result{PR: pr, Notes: fmt.Sprintf("opened PR #%d", pr.Number)}, nil
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
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", b.token, repo)
	// --depth=1 keeps the clone fast; we don't need history to bump a dep.
	if err := run(ctx, "", nil, "git", "clone", "--depth=1", "--branch="+branch, url, dir); err != nil {
		// Don't leak the token in the error.
		return fmt.Errorf("clone %s@%s: %w", repo, branch, scrubToken(err, b.token))
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
	return run(ctx, dir, []string{"GOFLAGS=-mod=mod"}, "go", "get", module+"@"+version)
}

func hasVendor(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "vendor"))
	return err == nil && st.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status in %s: %w", dir, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
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

func scrubToken(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

func buildPRBody(req Request) string {
	var b strings.Builder
	if len(req.Modules) == 1 {
		m := req.Modules[0]
		fmt.Fprintf(&b, "Automated bump of `%s` to `%s` on `%s`.\n\n", m.Path, m.Version, req.BaseBranch)
	} else {
		fmt.Fprintf(&b, "Automated bump of %d dependencies on `%s`:\n\n", len(req.Modules), req.BaseBranch)
		for _, m := range req.Modules {
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
