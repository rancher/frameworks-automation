package cascade

import (
	"fmt"
	"sort"

	"github.com/rancher/release-automation/internal/config"
)

// ComputeStages plans the cascade stages from `dep` (the source) to
// `leafRepo`/`leafBranch` (the final target). The algorithm:
//
//  1. Find the in-scope repos: every repo on a reverse-dep path from `dep`
//     to `leafRepo`. `dep` is layer 0 (source, not a stage). `leafRepo` is
//     in scope by definition.
//  2. Layer assignment: for each in-scope repo R, layer(R) = 1 + max(layer
//     of R's in-scope direct deps). This puts each repo strictly after
//     every in-scope dep it transitively needs.
//  3. Stages: group in-scope repos by layer (ascending, layer ≥ 1).
//  4. Per-repo bumps: each repo R in a stage bumps every in-scope direct
//     dep. Stage-1 bumps are pre-filled with `version`; later-stage bumps
//     have Version="" until prior tags arrive.
//  5. Per-stage tags: a non-final stage's TagPrompts cover that stage's
//     repos (one prompt per repo, on its stage branch).
//
// Branch resolution per stage repo:
//   - leafRepo → leafBranch (verified to exist in leafTable)
//   - paired   → leafTable.LookupMinor(leafBranch) → tbl.BranchForPair(minor)
//   - independent → "main" (independents have no leaf-branch pairing; the
//     project policy is to land independent bumps on main only)
//
// Errors:
//   - dep or leafRepo not in cfg
//   - leafRepo not kind=leaf
//   - leafBranch not in leafTable
//   - paired dependent missing its VERSION.md table
//   - paired dependent's table has no row for the leaf minor (the dep
//     simply doesn't ship against this leaf line yet)
//   - no path from dep to leafRepo (cascade is a no-op)
func ComputeStages(
	cfg *config.Config,
	dep, version, leafRepo, leafBranch string,
	leafTable *config.VersionTable,
	dependentTables map[string]*config.VersionTable,
) ([]Stage, error) {
	if _, ok := cfg.Repos[dep]; !ok {
		return nil, fmt.Errorf("dep %q not in config", dep)
	}
	leaf, ok := cfg.Repos[leafRepo]
	if !ok {
		return nil, fmt.Errorf("leaf %q not in config", leafRepo)
	}
	if leaf.Kind != config.KindLeaf {
		return nil, fmt.Errorf("repo %q is not a leaf", leafRepo)
	}
	if leafTable == nil {
		return nil, fmt.Errorf("leaf %q: missing VERSION.md table", leafRepo)
	}
	leafMinor := leafTable.LookupMinor(leafBranch)
	if leafMinor == "" {
		return nil, fmt.Errorf("leaf %q: branch %q not in VERSION.md", leafRepo, leafBranch)
	}

	scope := inScopeRepos(cfg, dep, leafRepo)
	if !scope[leafRepo] {
		return nil, fmt.Errorf("no path from %q to leaf %q", dep, leafRepo)
	}

	layers := assignLayers(cfg, dep, scope)

	branchOf := func(repo string) (string, error) {
		switch {
		case repo == leafRepo:
			return leafBranch, nil
		case cfg.Repos[repo].Kind == config.KindPaired:
			tbl, ok := dependentTables[repo]
			if !ok || tbl == nil {
				return "", fmt.Errorf("paired repo %q: missing VERSION.md table", repo)
			}
			br := tbl.BranchForPair(leafMinor)
			if br == "" {
				return "", fmt.Errorf("paired repo %q: no branch pairs to leaf minor %q", repo, leafMinor)
			}
			return br, nil
		case cfg.Repos[repo].Kind == config.KindIndependent:
			return "main", nil
		}
		return "", fmt.Errorf("repo %q has unsupported kind %q", repo, cfg.Repos[repo].Kind)
	}

	// Group repos by layer for stage assembly. Skip layer 0 (the source dep).
	byLayer := map[int][]string{}
	for repo, layer := range layers {
		if layer == 0 {
			continue
		}
		byLayer[layer] = append(byLayer[layer], repo)
	}
	layerNums := make([]int, 0, len(byLayer))
	for l := range byLayer {
		layerNums = append(layerNums, l)
	}
	sort.Ints(layerNums)

	stages := make([]Stage, 0, len(layerNums))
	for idx, layer := range layerNums {
		repos := byLayer[layer]
		sort.Strings(repos)

		var bumps []Bump
		for _, repo := range repos {
			repoBranch, err := branchOf(repo)
			if err != nil {
				return nil, err
			}
			// The repo bumps every direct in-scope dep, sorted for stable
			// ordering. dep == cascade source is included like any other.
			deps := append([]string(nil), cfg.Repos[repo].Deps...)
			sort.Strings(deps)
			for _, d := range deps {
				if !scope[d] {
					continue
				}
				bp := Bump{
					Repo:   repo,
					Branch: repoBranch,
					Dep:    d,
					Module: cfg.Repos[d].Module,
				}
				if d == dep {
					// Stage-1 (and any later stage that bumps the source
					// dep directly) gets the source version up front.
					bp.Version = version
				}
				bumps = append(bumps, bp)
			}
		}

		var tags []TagPrompt
		isFinal := idx == len(layerNums)-1
		if !isFinal {
			for _, repo := range repos {
				repoBranch, err := branchOf(repo)
				if err != nil {
					return nil, err
				}
				tags = append(tags, TagPrompt{Repo: repo, Branch: repoBranch})
			}
		}

		stages = append(stages, Stage{Layer: layer, Bumps: bumps, Tags: tags})
	}
	return stages, nil
}

// inScopeRepos returns the set of repos on at least one reverse-dep path
// from `dep` to `leafRepo`. dep itself is included (layer 0). leafRepo is
// included iff a path exists.
func inScopeRepos(cfg *config.Config, dep, leafRepo string) map[string]bool {
	// Forward reachability from dep along reverse-dep edges (dep → R when
	// R declares dep in its Deps).
	forward := map[string]bool{dep: true}
	queue := []string{dep}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range cfg.Dependents(cur) {
			if !forward[r] {
				forward[r] = true
				queue = append(queue, r)
			}
		}
	}
	if !forward[leafRepo] {
		return map[string]bool{} // no path → empty scope
	}

	// Backward reachability from leafRepo along forward-dep edges.
	backward := map[string]bool{leafRepo: true}
	queue = []string{leafRepo}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range cfg.Repos[cur].Deps {
			if !backward[d] {
				backward[d] = true
				queue = append(queue, d)
			}
		}
	}

	out := map[string]bool{}
	for r := range forward {
		if backward[r] {
			out[r] = true
		}
	}
	return out
}

// assignLayers computes layer numbers for in-scope repos. dep == 0; every
// other in-scope repo R == 1 + max(layer of R's in-scope direct deps).
//
// Iterative relaxation until stable. Cycles (which the DAG validator should
// have rejected) would loop forever — guard with a max iteration cap.
func assignLayers(cfg *config.Config, dep string, scope map[string]bool) map[string]int {
	layers := map[string]int{dep: 0}
	const maxIter = 100
	for iter := 0; iter < maxIter; iter++ {
		changed := false
		for r := range scope {
			if r == dep {
				continue
			}
			best := -1
			for _, d := range cfg.Repos[r].Deps {
				if !scope[d] {
					continue
				}
				dl, ok := layers[d]
				if !ok {
					best = -1
					break
				}
				if dl > best {
					best = dl
				}
			}
			if best >= 0 {
				want := best + 1
				if cur, ok := layers[r]; !ok || cur < want {
					layers[r] = want
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return layers
}
