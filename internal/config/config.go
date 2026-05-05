// Package config loads dependencies.yaml (the DAG + policy) and parses
// VERSION.md tables fetched from each managed repo.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var repoFormat = regexp.MustCompile(`^[^/]+/[^/]+$`)

type Kind string

const (
	KindLeaf        Kind = "leaf"
	KindPaired      Kind = "paired"
	KindIndependent Kind = "independent"
)

// NextTagStrategy names the rule used to suggest the next release tag for a
// repo when prompting for a cascade-mid retag. The suggestion is advisory —
// the per-repo Release workflow validates the actual input.
type NextTagStrategy string

const (
	// NextTagPatch is the default: bump the patch number on the highest
	// existing release matching the target minor (v0.7.5 → v0.7.6).
	NextTagPatch NextTagStrategy = "patch"
	// NextTagRC bumps the rc.N suffix on the highest existing release that
	// already carries one (v0.7.5-rc.1 → v0.7.5-rc.2). When the highest
	// existing release is a GA (no rc suffix), starts the next patch's rc
	// cycle (v0.7.5 → v0.7.6-rc.1).
	NextTagRC NextTagStrategy = "rc"
	// NextTagUnRC drops the -rc.N suffix from the highest existing rc tag
	// on the minor (v0.9.0-rc.4 → v0.9.0). When no rc is present (highest
	// is already GA, or no prior release on the minor) it returns empty —
	// there is nothing to unRC, and the cascade prompt tolerates a blank
	// hint. Selected only at run time via -tag-strategy-override; the
	// per-repo config keeps NextTagRC for the regular rc-bump cascade.
	NextTagUnRC NextTagStrategy = "unrc"
)

// Strategy names the procedure run when bumping a dep into a downstream.
// Each value maps to a registered implementation in internal/pr; the bumper
// dispatches per-dep within a bundle. StrategyOrder is special: it expresses
// a sequencing-only edge (the downstream waits on this dep's bumps to merge
// before its own stage opens, but no in-tree action is taken).
type Strategy string

const (
	StrategyGoGet                      Strategy = "go-get"
	StrategyChartBump                  Strategy = "chart-bump"
	StrategyBumpWebhook                Strategy = "bump-webhook"
	StrategyChartBumpWebhook           Strategy = "chart-bump-webhook"
	StrategyBumpRemotedialerProxy      Strategy = "bump-remotedialer-proxy"
	StrategyChartBumpRemotedialerProxy Strategy = "chart-bump-remotedialer-proxy"
	StrategyOrder                      Strategy = "order"
)

// Dep is one entry in a Repo's deps list. Object form only — name is required;
// strategy defaults to go-get when omitted.
type Dep struct {
	Name     string   `yaml:"name"`
	Strategy Strategy `yaml:"strategy,omitempty"`
}

type Repo struct {
	Kind Kind `yaml:"kind"`
	// Repo is the GitHub owner/name identity (e.g. "rancher/wrangler"). Used
	// for all clone/PR/API operations.
	Repo string `yaml:"repo"`
	// Fork, when set, causes bump branches to be pushed to this fork and PRs
	// opened as cross-repo PRs (head = "<fork-owner>:<branch>", base on Repo).
	Fork string `yaml:"fork,omitempty"`
	// BranchTemplate, when set on a paired repo, replaces the VERSION.md
	// branch-resolution path. Only "{rancher-minor}" is recognized — it's
	// filled from the leaf rancher branch's own VERSION.md row. Used for
	// repos whose branches follow a fixed naming scheme rather than a
	// VERSION.md table (e.g. the Rancher chart's "dev-v2.16" branches).
	BranchTemplate string `yaml:"branch-template,omitempty"`
	// VersionMD, when set, supplies the VERSION.md table inline rather
	// than fetching it from the repo's default branch. Same markdown
	// format as the file. Used for repos that don't ship a VERSION.md
	// (notably rancher itself).
	VersionMD string `yaml:"version-md,omitempty"`
	// NextTagStrategy selects the rule used to suggest the next release
	// tag at cascade-mid prompts. Defaults to NextTagPatch (patch+1). Set
	// to NextTagRC for repos whose release cadence bumps the rc.N suffix.
	NextTagStrategy NextTagStrategy `yaml:"next-tag-strategy,omitempty"`
	Deps            []Dep           `yaml:"deps"`
}

// GitHubRepo returns the GitHub owner/name for this repo.
func (r Repo) GitHubRepo() (string, error) {
	if r.Repo == "" {
		return "", fmt.Errorf("repo field is not set")
	}
	return r.Repo, nil
}

type Config struct {
	Repos map[string]Repo `yaml:"repos"`
	// Modules maps repo config key → Go module paths published by that repo.
	// Populated by DiscoverModules; empty until that call completes.
	Modules map[string][]string
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

// LoadAll reads every *.yaml file in `dir`, parses each via Load, and returns
// the parsed configs keyed by file basename (without extension). The basename
// is the config name used in tracker / cascade labels (config:<name>) and in
// the -config flag for cascade / bump-dep modes.
//
// Errors:
//   - dir doesn't exist or isn't readable
//   - dir contains no *.yaml files
//   - any single config fails to load or validate
//   - any single config doesn't have exactly one leaf repo (label-scoped queries
//     and cascade flow assume one leaf per config; enforce here so the failure
//     surfaces at startup rather than mid-run)
func LoadAll(dir string) (map[string]*Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read config dir %s: %w", dir, err)
	}
	out := make(map[string]*Config)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".yaml" {
			continue
		}
		key := strings.TrimSuffix(name, ".yaml")
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("config dir %s: duplicate basename %q", dir, key)
		}
		cfg, err := Load(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		leaves := cfg.LeafRepos()
		if len(leaves) != 1 {
			return nil, fmt.Errorf("config %s: must have exactly one leaf repo, found %d: %v", key, len(leaves), leaves)
		}
		out[key] = cfg
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("config dir %s: no *.yaml files", dir)
	}
	return out, nil
}

// applyDefaults fills omitted Strategy values with StrategyGoGet so the rest
// of the code can read Dep.Strategy without nil-checking. Also defaults
// Repo.NextTagStrategy to NextTagPatch. Mutates in place.
func (c *Config) applyDefaults() {
	for name, r := range c.Repos {
		for i := range r.Deps {
			if r.Deps[i].Strategy == "" {
				r.Deps[i].Strategy = StrategyGoGet
			}
		}
		if r.NextTagStrategy == "" {
			r.NextTagStrategy = NextTagPatch
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
		if r.Repo == "" {
			return fmt.Errorf("repo %q: repo is required", name)
		}
		if !repoFormat.MatchString(r.Repo) {
			return fmt.Errorf("repo %q: repo %q must be owner/name", name, r.Repo)
		}
		if r.Fork != "" && !repoFormat.MatchString(r.Fork) {
			return fmt.Errorf("repo %q: fork %q must be owner/name", name, r.Fork)
		}
		if r.BranchTemplate != "" {
			if r.Kind != KindPaired {
				return fmt.Errorf("repo %q: branch-template is only valid on kind=paired", name)
			}
			if err := validateBranchTemplate(r.BranchTemplate); err != nil {
				return fmt.Errorf("repo %q: %w", name, err)
			}
		}
		if r.VersionMD != "" {
			if _, err := ParseVersionTable(r.VersionMD); err != nil {
				return fmt.Errorf("repo %q: version-md: %w", name, err)
			}
		}
		if !KnownNextTagStrategy(r.NextTagStrategy) {
			return fmt.Errorf("repo %q: unknown next-tag-strategy %q", name, r.NextTagStrategy)
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

// KnownNextTagStrategy reports whether s is a recognized strategy. Exported
// so CLI parsing for the cascade-mode -tag-strategy-override flag can
// validate without reaching into config internals.
func KnownNextTagStrategy(s NextTagStrategy) bool {
	switch s {
	case NextTagPatch, NextTagRC, NextTagUnRC:
		return true
	}
	return false
}

func knownStrategy(s Strategy) bool {
	switch s {
	case StrategyGoGet,
		StrategyChartBump,
		StrategyBumpWebhook,
		StrategyChartBumpWebhook,
		StrategyBumpRemotedialerProxy,
		StrategyChartBumpRemotedialerProxy,
		StrategyOrder:
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
// stable iteration.
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

// ResolveDep returns the config key for the repo at github "owner/name".
// Used to translate a repository_dispatch payload (which carries the full
// GitHub identity) back to the short name used as a config key.
func (c *Config) ResolveDep(ghRepo string) (string, error) {
	for name, r := range c.Repos {
		if r.Repo == ghRepo {
			return name, nil
		}
	}
	return "", fmt.Errorf("repo %q not in dependencies.yaml", ghRepo)
}

// FirstModulePath returns the first Go module path known for the repo with
// the given config key. Returns "" when no modules have been discovered yet
// or the repo publishes no Go modules.
func (c *Config) FirstModulePath(repoName string) string {
	if paths := c.Modules[repoName]; len(paths) > 0 {
		return paths[0]
	}
	return ""
}

// ModuleToRepo builds a reverse index from Go module path → config key,
// using the runtime-discovered module map.
func (c *Config) ModuleToRepo() map[string]string {
	out := make(map[string]string)
	for name, paths := range c.Modules {
		for _, p := range paths {
			out[p] = name
		}
	}
	return out
}
