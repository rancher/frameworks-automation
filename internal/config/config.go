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

type Repo struct {
	Kind   Kind     `yaml:"kind"`
	Module string   `yaml:"module"`
	Deps   []string `yaml:"deps"`
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
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
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
		for _, d := range r.Deps {
			if _, ok := c.Repos[d]; !ok {
				return fmt.Errorf("repo %q: dep %q not declared", name, d)
			}
		}
	}
	return nil
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
			if d == dep {
				out = append(out, name)
				break
			}
		}
	}
	return out
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
