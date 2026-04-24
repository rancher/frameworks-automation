// Package config loads dependencies.yaml (the DAG + policy) and parses
// VERSION.md tables fetched from each managed repo.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Kind string

const (
	KindLeaf        Kind = "leaf"
	KindPaired      Kind = "paired"
	KindIndependent Kind = "independent"
)

// Strategy names the procedure run when bumping a dep into a downstream.
// Each value maps to a registered implementation in internal/pr; the bumper
// dispatches per-dep within a bundle. StrategyOrder is special: it expresses
// a sequencing-only edge (the downstream waits on this dep's bumps to merge
// before its own stage opens, but no in-tree action is taken).
type Strategy string

const (
	StrategyGoGet       Strategy = "go-get"
	StrategyChartBump   Strategy = "chart-bump"
	StrategyBumpWebhook Strategy = "bump-webhook"
	StrategyOrder       Strategy = "order"
)

// Dep is one entry in a Repo's deps list. Object form only — name is required;
// strategy defaults to go-get when omitted.
type Dep struct {
	Name     string   `yaml:"name"`
	Strategy Strategy `yaml:"strategy,omitempty"`
}

type Repo struct {
	Kind   Kind   `yaml:"kind"`
	Module string `yaml:"module"`
	// BranchTemplate, when set on a paired repo, replaces the VERSION.md
	// branch-resolution path. Only "{rancher-minor}" is recognized — it's
	// filled from the leaf rancher branch's own VERSION.md row. Used for
	// repos whose branches follow a fixed naming scheme rather than a
	// VERSION.md table (e.g. the Rancher chart's "dev-v2.16" branches).
	BranchTemplate string `yaml:"branch-template,omitempty"`
	Deps           []Dep  `yaml:"deps"`
}

type Config struct {
	Repos map[string]Repo `yaml:"repos"`
}

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

// applyDefaults fills omitted Strategy values with StrategyGoGet so the rest
// of the code can read Dep.Strategy without nil-checking. Mutates in place.
func (c *Config) applyDefaults() {
	for name, r := range c.Repos {
		for i := range r.Deps {
			if r.Deps[i].Strategy == "" {
				r.Deps[i].Strategy = StrategyGoGet
			}
		}
		c.Repos[name] = r
	}
}

func (c *Config) validate() error {
	for name, r := range c.Repos {
		switch r.Kind {
		case KindLeaf, KindPaired, KindIndependent:
		default:
			return fmt.Errorf("repo %q: invalid kind %q", name, r.Kind)
		}
		if r.Module == "" {
			return fmt.Errorf("repo %q: module is required", name)
		}
		if r.BranchTemplate != "" {
			if r.Kind != KindPaired {
				return fmt.Errorf("repo %q: branch-template is only valid on kind=paired", name)
			}
			if err := validateBranchTemplate(r.BranchTemplate); err != nil {
				return fmt.Errorf("repo %q: %w", name, err)
			}
		}
		seen := make(map[string]bool, len(r.Deps))
		for i, d := range r.Deps {
			if d.Name == "" {
				return fmt.Errorf("repo %q: deps[%d] missing name", name, i)
			}
			if seen[d.Name] {
				return fmt.Errorf("repo %q: dep %q listed twice", name, d.Name)
			}
			seen[d.Name] = true
			if _, ok := c.Repos[d.Name]; !ok {
				return fmt.Errorf("repo %q: dep %q not declared", name, d.Name)
			}
			if !knownStrategy(d.Strategy) {
				return fmt.Errorf("repo %q: dep %q has unknown strategy %q", name, d.Name, d.Strategy)
			}
		}
	}
	return nil
}

func knownStrategy(s Strategy) bool {
	switch s {
	case StrategyGoGet, StrategyChartBump, StrategyBumpWebhook, StrategyOrder:
		return true
	}
	return false
}

// validateBranchTemplate enforces the placeholder whitelist. Today only
// {rancher-minor} is accepted; expand here when new placeholders land.
func validateBranchTemplate(tpl string) error {
	const placeholder = "{rancher-minor}"
	rest := tpl
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return nil
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return fmt.Errorf("branch-template %q: unterminated placeholder", tpl)
		}
		token := rest[i : i+j+1]
		if token != placeholder {
			return fmt.Errorf("branch-template %q: unknown placeholder %q", tpl, token)
		}
		rest = rest[i+j+1:]
	}
}

// LeafRepos returns the keys of every repo with kind=leaf, sorted for
// stable iteration. Pilot 1 has exactly one (rancher), but the dashboard
// loop is written to handle N.
func (c *Config) LeafRepos() []string {
	var out []string
	for name, r := range c.Repos {
		if r.Kind == KindLeaf {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Dependents returns the set of repos that declare `dep` in their deps list.
func (c *Config) Dependents(dep string) []string {
	var out []string
	for name, r := range c.Repos {
		for _, d := range r.Deps {
			if d.Name == dep {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// DepStrategy returns the strategy this repo uses for a particular dep.
// Returns StrategyGoGet (the default) if `dep` isn't listed; callers should
// only invoke this for deps known to be in r.Deps.
func (r Repo) DepStrategy(dep string) Strategy {
	for _, d := range r.Deps {
		if d.Name == dep {
			return d.Strategy
		}
	}
	return StrategyGoGet
}

// ResolveBranch returns the paired repo's branch for `leafRancherMinor`.
//
// Two paths:
//
//   - BranchTemplate set → fill the {rancher-minor} placeholder. No VERSION.md
//     fetch needed; pairedTable may be nil.
//   - VERSION.md path → look up the row whose Pair column matches the leaf's
//     minor and return its Branch. Returns ("", nil) when no row matches —
//     callers decide whether that's an error or a silent skip. Returns an
//     error only when pairedTable is nil (the table itself is required for
//     the VERSION.md path).
//
// Callers wrap errors with the repo name.
func (r Repo) ResolveBranch(leafRancherMinor string, pairedTable *VersionTable) (string, error) {
	if r.BranchTemplate != "" {
		return strings.ReplaceAll(r.BranchTemplate, "{rancher-minor}", leafRancherMinor), nil
	}
	if pairedTable == nil {
		return "", fmt.Errorf("missing VERSION.md table")
	}
	return pairedTable.BranchForPair(leafRancherMinor), nil
}

// GitHubRepo derives "owner/name" from the module path, assuming a
// github.com/owner/name layout. Errors for non-GitHub modules.
func (r Repo) GitHubRepo() (string, error) {
	const prefix = "github.com/"
	if !strings.HasPrefix(r.Module, prefix) {
		return "", fmt.Errorf("module %q is not on github.com", r.Module)
	}
	parts := strings.SplitN(strings.TrimPrefix(r.Module, prefix), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("module %q: expected github.com/owner/name", r.Module)
	}
	return parts[0] + "/" + parts[1], nil
}

// ResolveDep returns the config key for the repo at github "owner/name".
// Used to translate a repository_dispatch payload (which carries the full
// GitHub identity) back to the short name used as a config key.
func (c *Config) ResolveDep(ghRepo string) (string, error) {
	for name, r := range c.Repos {
		gh, err := r.GitHubRepo()
		if err != nil {
			continue
		}
		if gh == ghRepo {
			return name, nil
		}
	}
	return "", fmt.Errorf("repo %q not in dependencies.yaml", ghRepo)
}
