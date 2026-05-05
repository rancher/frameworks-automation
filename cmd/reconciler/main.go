// Reconciler entrypoint. Invoked by .github/workflows/reconciler.yml (cron),
// .github/workflows/tag-emitted.yml (repository_dispatch), per-config
// .github/workflows/cascade-<name>.yml manual workflows, and the per-dep
// .github/workflows/bump-<dep>.yml manual workflows.
//
// Mode selects the entry path:
//
//	-mode=cron      Full sweep over every upstream/downstream pair, in every
//	                loaded config.
//	-mode=dispatch  Scoped to a single just-emitted tag. Iterates every
//	                config that contains the dispatched repo, opening one
//	                bump-op tracker per matching config (independent of the
//	                others — see ARCHITECTURE.md "Multi-config isolation").
//	-mode=bump-dep  Manual fan-out of one (dep, version) across every repo
//	                that ships against a chosen leaf branch — used for the
//	                independent-lib release/* case where the auto path
//	                only touches `main`. Without -config, fans out across
//	                every config containing the dep.
//	-mode=cascade   Multi-source cascade onto a chosen leaf branch in one
//	                specific config. -config is required — different
//	                specialized cascades (rancher-chart-webhook,
//	                rancher-chart-remotedialer-proxy, …) live in different
//	                files and produce different cascade tracker issues.
//	-mode=validate-config
//	                Parse and validate every dependencies/<name>.yaml; exit
//	                0 on success, non-zero with the error otherwise. Used by
//	                CI to guard edits to the configs. Talks to nothing
//	                external — no env vars or GitHub credentials required.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/reconcile"
)

func main() {
	var (
		mode         = flag.String("mode", "cron", "cron|dispatch|bump-dep|cascade|validate-config")
		configDir    = flag.String("config-dir", "dependencies", "directory containing the per-path *.yaml configs")
		configName   = flag.String("config", "", "cascade|bump-dep mode: scope to one config (basename of dependencies/<name>.yaml). Required for cascade; optional for bump-dep (default: every config containing the dep).")
		repo         = flag.String("repo", "", "dispatch mode: owner/name of repo that emitted the tag")
		tag          = flag.String("tag", "", "dispatch mode: tag that was emitted (e.g. v0.7.5)")
		sha          = flag.String("sha", "", "dispatch mode: commit SHA the tag points at")
		dep          = flag.String("dep", "", "bump-dep mode: dep config key (e.g. wrangler)")
		version      = flag.String("version", "", "bump-dep mode: version to bump (e.g. v0.5.1)")
		leafBranch   = flag.String("leaf-branch", "", "bump-dep|cascade mode: leaf-repo branch the op targets (e.g. release/v2.13)")
		independents = flag.String("independents", "", "cascade mode: comma-separated independent=version pairs (e.g. wrangler=v0.5.2,lasso=v1.0.0). Empty means no explicit independents — paired deps still get picked up at their latest tag.")
	)
	flag.Parse()

	cfgs, err := config.LoadAll(*configDir)
	if err != nil {
		log.Fatalf("load configs: %v", err)
	}

	// validate-config exits before reaching envSettings() so it can run
	// in CI without any secrets configured. LoadAll already did the work.
	if *mode == "validate-config" {
		for _, name := range sortedNames(cfgs) {
			fmt.Printf("config %s: %d repos\n", name, len(cfgs[name].Repos))
		}
		return
	}

	settings := envSettings()
	ctx := context.Background()
	gh := ghclient.NewClient(ctx, settings.GitHubToken)

	reconcilers := make(map[string]*reconcile.Reconciler, len(cfgs))
	for _, name := range sortedNames(cfgs) {
		cfg := cfgs[name]
		if err := cfg.DiscoverModules(ctx, gh); err != nil {
			log.Fatalf("discover modules for %s: %v", name, err)
		}
		r, err := reconcile.New(name, cfg, settings)
		if err != nil {
			log.Fatalf("init reconciler for %s: %v", name, err)
		}
		reconcilers[name] = r
	}

	switch *mode {
	case "cron":
		runCron(ctx, cfgs, reconcilers)
	case "dispatch":
		if *repo == "" || *tag == "" {
			log.Fatalf("dispatch: -repo and -tag are required")
		}
		runDispatch(ctx, cfgs, reconcilers, reconcile.DispatchEvent{Repo: *repo, Tag: *tag, SHA: *sha})
	case "bump-dep":
		if *dep == "" || *version == "" || *leafBranch == "" {
			log.Fatalf("bump-dep: -dep, -version, -leaf-branch are all required")
		}
		runBumpDep(ctx, cfgs, reconcilers, *configName, *dep, *version, *leafBranch)
	case "cascade":
		if *configName == "" {
			log.Fatalf("cascade: -config is required (one of: %s)", strings.Join(sortedNames(cfgs), ", "))
		}
		if *leafBranch == "" {
			log.Fatalf("cascade: -leaf-branch is required")
		}
		r, ok := reconcilers[*configName]
		if !ok {
			log.Fatalf("cascade: unknown config %q (loaded: %s)", *configName, strings.Join(sortedNames(cfgs), ", "))
		}
		indep, err := parseIndependents(*independents)
		if err != nil {
			log.Fatalf("cascade: %v", err)
		}
		// Fail fast if the user passed an independent the chosen config
		// doesn't even know about — better an early error than a confusing
		// "source not in config" deep inside RunCascade.
		for name := range indep {
			if _, ok := cfgs[*configName].Repos[name]; !ok {
				log.Fatalf("cascade: independent %q not in config %q", name, *configName)
			}
		}
		if err := r.RunCascade(ctx, *leafBranch, indep); err != nil {
			log.Fatalf("cascade[%s]: %v", *configName, err)
		}
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

// runCron runs RunCron against every loaded reconciler. One config's failure
// is logged and the sweep continues — exit non-zero only when every config
// fails (otherwise a single broken config would blank the whole cron tick).
func runCron(ctx context.Context, cfgs map[string]*config.Config, reconcilers map[string]*reconcile.Reconciler) {
	failures := 0
	for _, name := range sortedNames(cfgs) {
		if err := reconcilers[name].RunCron(ctx); err != nil {
			log.Printf("cron[%s]: %v", name, err)
			failures++
		}
	}
	if failures == len(cfgs) {
		log.Fatalf("cron: all %d configs failed", failures)
	}
}

// runDispatch fans out one tag-emitted event to every config containing the
// dispatched repo. Each config's reconciler runs in its own scope (its own
// trackers, its own cascade-claim attempts), so a tag in N configs produces
// up to N bump-op trackers — one per matching config.
func runDispatch(ctx context.Context, cfgs map[string]*config.Config, reconcilers map[string]*reconcile.Reconciler, ev reconcile.DispatchEvent) {
	matched := 0
	failures := 0
	for _, name := range sortedNames(cfgs) {
		if _, err := cfgs[name].ResolveDep(ev.Repo); err != nil {
			continue
		}
		matched++
		if err := reconcilers[name].RunDispatch(ctx, ev); err != nil {
			log.Printf("dispatch[%s]: %v", name, err)
			failures++
		}
	}
	if matched == 0 {
		log.Printf("dispatch: %s not in any loaded config — ignoring", ev.Repo)
		return
	}
	if failures == matched {
		log.Fatalf("dispatch: all %d matching configs failed", failures)
	}
}

// runBumpDep fans out a manual bump-dep request. Without -config, every
// config containing the dep gets the bump (mirrors the symmetric multi-config
// model — one operator action lands across all configs that participate in
// that dep's release line). With -config, only that config runs.
func runBumpDep(ctx context.Context, cfgs map[string]*config.Config, reconcilers map[string]*reconcile.Reconciler, only, dep, version, leafBranch string) {
	targets := []string{}
	if only != "" {
		if _, ok := reconcilers[only]; !ok {
			log.Fatalf("bump-dep: unknown config %q (loaded: %s)", only, strings.Join(sortedNames(cfgs), ", "))
		}
		if _, ok := cfgs[only].Repos[dep]; !ok {
			log.Fatalf("bump-dep: dep %q not in config %q", dep, only)
		}
		targets = append(targets, only)
	} else {
		for _, name := range sortedNames(cfgs) {
			if _, ok := cfgs[name].Repos[dep]; ok {
				targets = append(targets, name)
			}
		}
		if len(targets) == 0 {
			log.Fatalf("bump-dep: dep %q not in any loaded config", dep)
		}
	}
	failures := 0
	for _, name := range targets {
		if err := reconcilers[name].RunBumpDep(ctx, dep, version, leafBranch); err != nil {
			log.Printf("bump-dep[%s]: %v", name, err)
			failures++
		}
	}
	if failures == len(targets) {
		log.Fatalf("bump-dep: all %d configs failed", failures)
	}
}

func sortedNames(cfgs map[string]*config.Config) []string {
	out := make([]string, 0, len(cfgs))
	for k := range cfgs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseIndependents parses "name=ver,name=ver" into a map. Whitespace and
// trailing commas tolerated so the workflow shell can concatenate optional
// inputs naively.
func parseIndependents(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("independent %q: want name=version", kv)
		}
		name := strings.TrimSpace(kv[:eq])
		version := strings.TrimSpace(kv[eq+1:])
		if name == "" || version == "" {
			return nil, fmt.Errorf("independent %q: name and version are both required", kv)
		}
		out[name] = version
	}
	return out, nil
}

func envSettings() reconcile.Settings {
	return reconcile.Settings{
		AutomationRepo: requireEnv("AUTOMATION_REPO"),
		GitHubToken:    requireEnv("GH_BOT_TOKEN"),
		GitHubActor:    os.Getenv("GITHUB_ACTOR"),
		SlackToken:     os.Getenv("SLACK_BOT_TOKEN"),
		SlackChannel:   os.Getenv("SLACK_CHANNEL_ID"),
	}
}

func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env: %s\n", name)
		os.Exit(2)
	}
	return v
}
