package config

import (
	"context"
	"log"
	"strings"

	"golang.org/x/mod/modfile"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// DiscoverModules walks each repo's default branch via the GitHub Trees API,
// fetches the **root** go.mod (path == "go.mod"), parses the module directive,
// and populates c.Modules. Nested go.mods (examples/, gotools/, etc.) are
// intentionally skipped — they aren't importable cross-repo deps and pulling
// them in led to RootModulePath returning a non-canonical path like
// "dummy/fakek8s" when the Trees API listed examples/fakek8s/go.mod first.
// If a real publishable submodule ever needs to be tracked, declare it
// explicitly in dependencies/<config>.yaml rather than re-broadening this
// scan.
//
// Call after config.Load and after the GitHub client is built. Per-repo
// failures are logged and skipped — they degrade downstream detection for
// that repo but do not abort the reconciler.
func (c *Config) DiscoverModules(ctx context.Context, gh *ghclient.Client) error {
	c.Modules = make(map[string][]string)
	total := 0
	for name, repo := range c.Repos {
		if repo.Kind == KindLeaf {
			continue
		}
		ghRepo, err := repo.GitHubRepo()
		if err != nil {
			log.Printf("discover modules: %s: resolve repo: %v", name, err)
			continue
		}
		paths, err := gh.GetGoModPaths(ctx, ghRepo)
		if err != nil {
			log.Printf("discover modules: %s: tree walk: %v", name, err)
			continue
		}
		for _, p := range paths {
			if p != "go.mod" {
				continue
			}
			content, err := gh.FetchFile(ctx, ghRepo, "", p)
			if err != nil {
				log.Printf("discover modules: %s: fetch %s: %v", name, p, err)
				continue
			}
			mf, err := modfile.Parse(p, []byte(content), nil)
			if err != nil {
				log.Printf("discover modules: %s: parse %s: %v", name, p, err)
				continue
			}
			modPath := strings.TrimSpace(mf.Module.Mod.Path)
			if modPath == "" {
				continue
			}
			c.Modules[name] = append(c.Modules[name], modPath)
			total++
		}
	}
	log.Printf("discover modules: found %d module(s) across %d repo(s)", total, len(c.Modules))
	return nil
}
