package cascade

import (
	"fmt"
	"sort"

	"github.com/rancher/release-automation/internal/config"
)

// LatestResolver returns the highest published release tag on `repo`'s
// `branch`. The cascade package uses it to pin paired-latest source versions
// at cascade creation. Returns "" with no error when no tag exists on the
// branch lineage (caller decides if that's fatal).
type LatestResolver func(repo, branch string) (string, error)

// ComputeStages plans a multi-source cascade. Inputs:
//
//   - independents: explicit user-supplied source independents → version.
//     Each entry's repo must exist in cfg and be kind=independent. Empty map
//     means "no explicit sources — just pick up paired-latest into leaf".
//   - leafRepo / leafBranch: the final consumer.
//   - leafTable / pairedTables: VERSION.md tables for branch resolution.
//   - resolveLatest: callback returning the latest release tag on a paired
//     dep's leaf-paired branch. Used when a paired dep is needed by a stage
//     repo but isn't itself in the propagation set (no re-cut required).
//   - staleRepos: optional set of paired repos with unreleased commits that
//     should be promoted into the propagation set (gets bump→tag stages)
//     instead of being consumed at their current paired-latest tag. Pass nil
//     when no staleness detection has been done.
//
// Algorithm:
//
//  1. propagation = forward(independents) ∩ backward(leaf) ∖ independents.
//     Repos that must be re-cut because they transitively depend on something
//     in `independents` and feed leaf.
//  2. staleRepos entries that are in backward(leaf) are merged into propagation.
//  3. stage_repos = propagation ∪ {leaf}. These get bump PRs (and non-final
//     stages get tag prompts).
//  4. For each stage repo, walk its direct deps:
//     - in independents → explicit source. Bundle entry pinned at given version.
//     - in propagation → bundle entry empty (Version="") until upstream tag arrives.
//     - paired and not in propagation → resolveLatest. Becomes implicit
//     paired-latest source.
//     - independent and not in independents → SKIP (out of scope).
//  5. Layer assignment: sources at layer 0; iterative relaxation for stage repos.
//  6. Stages = layers ≥ 1. Non-final stages get one TagPrompt per stage repo.
//
// Returns the ordered source list (explicit + paired-latest, for body display)
// and the planned stages.
func ComputeStages(
	cfg *config.Config,
	independents map[string]string,
	leafRepo, leafBranch string,
	leafTable *config.VersionTable,
	pairedTables map[string]*config.VersionTable,
	resolveLatest LatestResolver,
	staleRepos map[string]bool,
) ([]Source, []Stage, error) {
	leaf, ok := cfg.Repos[leafRepo]
	if !ok {
		return nil, nil, fmt.Errorf("leaf %q not in config", leafRepo)
	}
	if leaf.Kind != config.KindLeaf {
		return nil, nil, fmt.Errorf("repo %q is not a leaf", leafRepo)
	}
	if leafTable == nil {
		return nil, nil, fmt.Errorf("leaf %q: missing VERSION.md table", leafRepo)
	}
	leafMinor := leafTable.LookupMinor(leafBranch)
	if leafMinor == "" {
		return nil, nil, fmt.Errorf("leaf %q: branch %q not in VERSION.md", leafRepo, leafBranch)
	}

	for name := range independents {
		repo, ok := cfg.Repos[name]
		if !ok {
			return nil, nil, fmt.Errorf("source %q not in config", name)
		}
		if repo.Kind != config.KindIndependent {
			return nil, nil, fmt.Errorf("source %q is kind=%s; only independents may be cascade inputs", name, repo.Kind)
		}
	}

	explicitSet := make(map[string]bool, len(independents))
	for name := range independents {
		explicitSet[name] = true
	}

	backward := backwardClosure(cfg, leafRepo)
	forward := forwardClosure(cfg, explicitSet)

	propagation := map[string]bool{}
	for r := range forward {
		if backward[r] && !explicitSet[r] {
			propagation[r] = true
		}
	}
	// Stale paired repos (unreleased commits) are promoted from paired-latest
	// into the propagation set so they receive their own bump→tag stage rather
	// than being silently consumed at their current tag.
	for r := range staleRepos {
		if backward[r] && !explicitSet[r] {
			propagation[r] = true
		}
	}
	stageRepos := map[string]bool{leafRepo: true}
	for r := range propagation {
		stageRepos[r] = true
	}

	branchOf := func(repo string) (string, error) {
		rcfg := cfg.Repos[repo]
		switch {
		case repo == leafRepo:
			return leafBranch, nil
		case rcfg.Kind == config.KindPaired:
			br, err := rcfg.ResolveBranch(leafMinor, pairedTables[repo])
			if err != nil {
				return "", fmt.Errorf("paired repo %q: %w", repo, err)
			}
			if br == "" {
				return "", fmt.Errorf("paired repo %q: no branch pairs to leaf minor %q", repo, leafMinor)
			}
			return br, nil
		case rcfg.Kind == config.KindIndependent:
			return "main", nil
		}
		return "", fmt.Errorf("repo %q has unsupported kind %q", repo, rcfg.Kind)
	}

	// Walk every stage repo's direct deps once. This both shapes the bundles
	// and discovers paired-latest sources (a paired dep referenced by a stage
	// repo but not itself in propagation needs a pinned latest tag).
	pairedLatest := map[string]string{}
	addPairedLatest := func(name string) error {
		if _, seen := pairedLatest[name]; seen {
			return nil
		}
		br, err := branchOf(name)
		if err != nil {
			return fmt.Errorf("paired-latest %s: %w", name, err)
		}
		if resolveLatest == nil {
			return fmt.Errorf("paired-latest %s: resolver is nil", name)
		}
		v, err := resolveLatest(name, br)
		if err != nil {
			return fmt.Errorf("paired-latest %s on %s: %w", name, br, err)
		}
		if v == "" {
			return fmt.Errorf("paired-latest %s on %s: no published release", name, br)
		}
		pairedLatest[name] = v
		return nil
	}

	type stageDep struct {
		Dep, Module, Version string
		Strategy             config.Strategy
	}
	bundleByRepo := map[string][]stageDep{}
	for repo := range stageRepos {
		deps := append([]config.Dep(nil), cfg.Repos[repo].Deps...)
		sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
		for _, d := range deps {
			depCfg, ok := cfg.Repos[d.Name]
			if !ok {
				continue
			}
			// Order edges sequence the DAG (chart blocks rancher) but produce
			// no in-tree action. Drop them from the bundle here so the bumper
			// never sees them, while the layer assignment below still folds
			// them into the topological sort.
			if d.Strategy == config.StrategyOrder {
				continue
			}
			switch {
			case explicitSet[d.Name]:
				bundleByRepo[repo] = append(bundleByRepo[repo], stageDep{
					Dep: d.Name, Module: depCfg.Module, Version: independents[d.Name], Strategy: d.Strategy,
				})
			case propagation[d.Name]:
				bundleByRepo[repo] = append(bundleByRepo[repo], stageDep{
					Dep: d.Name, Module: depCfg.Module, Strategy: d.Strategy,
				})
			case depCfg.Kind == config.KindPaired:
				if err := addPairedLatest(d.Name); err != nil {
					return nil, nil, err
				}
				bundleByRepo[repo] = append(bundleByRepo[repo], stageDep{
					Dep: d.Name, Module: depCfg.Module, Version: pairedLatest[d.Name], Strategy: d.Strategy,
				})
			default:
				// Independent not in user input → out of scope. Skip.
			}
		}
	}

	var sources []Source
	for name, v := range independents {
		sources = append(sources, Source{Name: name, Version: v, Explicit: true})
	}
	for name, v := range pairedLatest {
		sources = append(sources, Source{Name: name, Version: v})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })

	sourceSet := map[string]bool{}
	for _, s := range sources {
		sourceSet[s.Name] = true
	}
	layers := assignLayers(cfg, sourceSet, stageRepos)

	byLayer := map[int][]string{}
	for repo := range stageRepos {
		l, ok := layers[repo]
		if !ok {
			return nil, nil, fmt.Errorf("layer assignment failed for %q (cycle or unreachable from sources)", repo)
		}
		byLayer[l] = append(byLayer[l], repo)
	}
	layerNums := make([]int, 0, len(byLayer))
	for l := range byLayer {
		layerNums = append(layerNums, l)
	}
	sort.Ints(layerNums)

	// needsTag captures which repos any later stage will consume via a
	// version-bearing edge (anything that isn't `order`). A repo not in this
	// set gets no tag prompt — chart, for instance, is sequenced before
	// rancher via an order edge but isn't go-get'd into anything, so the
	// cascade has no tag to wait on for it. Stage-only repos (in stageRepos
	// but consumed only via order) become bump-only.
	needsTag := map[string]bool{}
	for repo := range stageRepos {
		for _, d := range cfg.Repos[repo].Deps {
			if d.Strategy != config.StrategyOrder {
				needsTag[d.Name] = true
			}
		}
	}

	stages := make([]Stage, 0, len(layerNums))
	for idx, layer := range layerNums {
		repos := byLayer[layer]
		sort.Strings(repos)
		var bumps []Bump
		for _, repo := range repos {
			entries := bundleByRepo[repo]
			if len(entries) == 0 {
				continue
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Dep < entries[j].Dep })
			bundle := make([]DepBump, len(entries))
			for i, e := range entries {
				bundle[i] = DepBump{Dep: e.Dep, Module: e.Module, Version: e.Version, Strategy: e.Strategy}
			}
			br, err := branchOf(repo)
			if err != nil {
				return nil, nil, err
			}
			bumps = append(bumps, Bump{Repo: repo, Branch: br, Deps: bundle})
		}
		var tags []TagPrompt
		isFinal := idx == len(layerNums)-1
		if !isFinal {
			for _, repo := range repos {
				if !needsTag[repo] {
					continue
				}
				br, err := branchOf(repo)
				if err != nil {
					return nil, nil, err
				}
				tags = append(tags, TagPrompt{Repo: repo, Branch: br})
			}
		}
		stages = append(stages, Stage{Layer: layer, Bumps: bumps, Tags: tags})
	}

	hasBumps := false
	for _, st := range stages {
		if len(st.Bumps) > 0 {
			hasBumps = true
			break
		}
	}
	if !hasBumps {
		return nil, nil, fmt.Errorf("nothing to bump for %s %s (no in-scope deps)", leafRepo, leafBranch)
	}
	return sources, stages, nil
}

// backwardClosure returns every repo from which `root` is forward-reachable
// (i.e. root + every direct/transitive dep of root).
func backwardClosure(cfg *config.Config, root string) map[string]bool {
	out := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range cfg.Repos[cur].Deps {
			if !out[d.Name] {
				out[d.Name] = true
				queue = append(queue, d.Name)
			}
		}
	}
	return out
}

// forwardClosure returns every repo reachable from any element of `roots`
// along reverse-dep edges (R is reachable from X if R declares X in Deps).
// Roots themselves are included.
func forwardClosure(cfg *config.Config, roots map[string]bool) map[string]bool {
	out := make(map[string]bool, len(roots))
	var queue []string
	for r := range roots {
		out[r] = true
		queue = append(queue, r)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range cfg.Dependents(cur) {
			if !out[r] {
				out[r] = true
				queue = append(queue, r)
			}
		}
	}
	return out
}

// assignLayers computes layer numbers. Sources sit at layer 0; each stage
// repo R's layer = 1 + max(layer of R's in-scope direct deps), where in-scope
// means "source or stage repo". Iterative relaxation until stable.
func assignLayers(cfg *config.Config, sources, stageRepos map[string]bool) map[string]int {
	layers := map[string]int{}
	for r := range sources {
		layers[r] = 0
	}
	inScope := func(r string) bool { return sources[r] || stageRepos[r] }
	const maxIter = 100
	for iter := 0; iter < maxIter; iter++ {
		changed := false
		for r := range stageRepos {
			if sources[r] {
				continue
			}
			best := -1
			ready := true
			for _, d := range cfg.Repos[r].Deps {
				if !inScope(d.Name) {
					continue
				}
				dl, ok := layers[d.Name]
				if !ok {
					ready = false
					break
				}
				if dl > best {
					best = dl
				}
			}
			if !ready {
				continue
			}
			want := 1
			if best >= 0 {
				want = best + 1
			}
			if cur, ok := layers[r]; !ok || cur < want {
				layers[r] = want
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return layers
}
