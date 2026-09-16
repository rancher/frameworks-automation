// Package renovateapprove implements the Renovate auto-approve sweep: for
// every configured repo, find open Renovate PRs that are old enough and have
// green CI, then approve them, regardless of whether GitHub-native auto-merge
// has been requested on the PR (many repos in scope have "Allow auto-merge"
// off). Approval satisfies each repo's `required_approving_review_count: 1`
// branch protection rule without an org-level bypass actor — the same mechanism
// rancher/rancher already uses in its own repo-local workflow.
//
// This is intentionally decoupled from the dependencies/*.yaml bump DAG
// (package config): the set of repos that want auto-approve doesn't have to
// match the set of repos in the cascade graph (e.g. lasso isn't in any
// dependencies/*.yaml, but does want auto-approve).
package renovateapprove

import (
	"fmt"
	"os"
	"regexp"

	"go.yaml.in/yaml/v3"
)

var (
	repoFormat   = regexp.MustCompile(`^[^/]+/[^/]+$`)
	envVarFormat = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// RepoConfig is one repo the sweep should manage.
type RepoConfig struct {
	// Repo is the GitHub owner/name identity (e.g. "rancher/steve").
	Repo string `yaml:"repo"`
	// TokenEnv names the environment variable holding the fine-grained
	// GitHub token used to approve PRs in this repo. Same Vault-backed
	// convention as config.Repo.TokenEnv (see mint-tokens).
	TokenEnv string `yaml:"token-env"`
}

// Config is the top-level renovate-approve policy + repo list.
type Config struct {
	// Author is the PR author login that qualifies for auto-approve.
	// Defaults to "renovate-rancher[bot]" (the REST API login for the
	// GitHub App — not the "app/renovate-rancher" form the GraphQL-backed
	// `gh` CLI uses).
	Author string `yaml:"author,omitempty"`
	// BranchPrefix filters candidate PRs by head branch, as defense in
	// depth alongside the author check. Defaults to "renovate/".
	BranchPrefix string `yaml:"branch-prefix,omitempty"`
	// MinWorkingDays is the soak time (Mon-Fri, UTC) a PR must sit open
	// before it's eligible for auto-approve. Defaults to 1.
	MinWorkingDays int `yaml:"min-working-days,omitempty"`

	Repos []RepoConfig `yaml:"repos"`
}

// Load reads and validates the renovate-approve config at path, filling in
// defaults for any omitted policy field.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Author == "" {
		c.Author = "renovate-rancher[bot]"
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = "renovate/"
	}
	if c.MinWorkingDays == 0 {
		c.MinWorkingDays = 1
	}
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("repos is required and must be non-empty")
	}
	if c.MinWorkingDays < 0 {
		return fmt.Errorf("min-working-days must be >= 0, got %d", c.MinWorkingDays)
	}
	seen := make(map[string]bool, len(c.Repos))
	for i, r := range c.Repos {
		if r.Repo == "" {
			return fmt.Errorf("repos[%d]: repo is required", i)
		}
		if !repoFormat.MatchString(r.Repo) {
			return fmt.Errorf("repos[%d]: repo %q must be owner/name", i, r.Repo)
		}
		if seen[r.Repo] {
			return fmt.Errorf("repos[%d]: repo %q listed twice", i, r.Repo)
		}
		seen[r.Repo] = true
		if r.TokenEnv == "" {
			return fmt.Errorf("repos[%d] (%s): token-env is required", i, r.Repo)
		}
		if !envVarFormat.MatchString(r.TokenEnv) {
			return fmt.Errorf("repos[%d] (%s): token-env %q must match %s", i, r.Repo, r.TokenEnv, envVarFormat)
		}
	}
	return nil
}

// Tokens resolves each repo's token-env against the process environment.
// Returns an error naming the first missing variable rather than partially
// populating the map — a missing token means that repo silently gets no
// coverage, which should fail loud, not sweep quietly short.
func (c *Config) Tokens(getenv func(string) string) (map[string]string, error) {
	out := make(map[string]string, len(c.Repos))
	for _, r := range c.Repos {
		v := getenv(r.TokenEnv)
		if v == "" {
			return nil, fmt.Errorf("missing required env %s (repo=%s)", r.TokenEnv, r.Repo)
		}
		out[r.Repo] = v
	}
	return out, nil
}
