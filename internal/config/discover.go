package config

import (
	"context"
	"log"
	"strings"

	"golang.org/x/mod/modfile"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// DiscoverModules fetches each non-leaf repo's root go.mod from its default
// branch and records the declared module path in c.Modules. Nested go.mods
// (examples/, gotools/, etc.) are intentionally ignored — they aren't
// importable cross-repo deps and reading them led to RootModulePath returning
// a non-canonical path like "dummy/fakek8s". If a real publishable submodule
// ever needs to be tracked, declare it explicitly in dependencies/<config>.yaml
// rather than reintroducing a tree scan.
//
// Call after config.Load and after the GitHub client is built. Per-repo
// failures are logged and skipped; a 404 (no root go.mod, e.g. rancher/charts)
// is expected and silent.
func (c *Config) DiscoverModules(ctx context.Context, gh *ghclient.Client) error {
	c.Modules = make(map[string][]string)
	for name, repo := range c.Repos {
		if repo.Kind == KindLeaf {
			continue
		}
		ghRepo, err := repo.GitHubRepo()
		if err != nil {
			log.Printf("discover modules: %s: resolve repo: %v", name, err)
			continue
		}
		content, err := gh.FetchFile(ctx, ghRepo, "", "go.mod")
		if err != nil {
			if ghclient.IsNotFound(err) {
				continue
			}
			log.Printf("discover modules: %s: fetch go.mod: %v", name, err)
			continue
		}
		mf, err := modfile.Parse("go.mod", []byte(content), nil)
		if err != nil {
			log.Printf("discover modules: %s: parse go.mod: %v", name, err)
			continue
		}
		modPath := strings.TrimSpace(mf.Module.Mod.Path)
		if modPath == "" {
			continue
		}
		c.Modules[name] = []string{modPath}
	}
	log.Printf("discover modules: indexed %d repo(s)", len(c.Modules))
	return nil
}
