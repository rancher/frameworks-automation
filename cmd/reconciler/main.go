// Reconciler entrypoint. Invoked by .github/workflows/reconciler.yml (cron),
// .github/workflows/tag-emitted.yml (repository_dispatch), and the per-dep
// .github/workflows/bump-<dep>.yml manual workflows.
//
// Mode selects the entry path:
//
//	-mode=cron      Full sweep: passes 1-4 over every upstream/downstream pair.
//	-mode=dispatch  Scoped to a single just-emitted tag. Skips the upstream
//	                discovery (passes 2-4 still run for completeness).
//	-mode=bump-dep  Manual fan-out of one (dep, version) across every repo
//	                that ships against a chosen leaf branch — used for the
//	                independent-lib release/* case where the auto path
//	                only touches `main`.
//	-mode=cascade   Multi-source cascade onto a chosen leaf branch. Walks
//	                the DAG one stage at a time, prompting a re-tag at each
//	                intermediate layer. Independent dep versions come in
//	                via -independents=name=ver,name=ver; paired deps are
//	                always picked up at their paired-branch latest tag.
//	-mode=validate-config
//	                Parse and validate dependencies.yaml; exit 0 on success,
//	                non-zero with the error otherwise. Used by CI to guard
//	                edits to the config. Talks to nothing external — no env
//	                vars or GitHub credentials required.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/reconcile"
)

func main() {
	var (
		mode         = flag.String("mode", "cron", "cron|dispatch|bump-dep|cascade|validate-config")
		configPath   = flag.String("config", "dependencies.yaml", "path to dependencies.yaml")
		repo         = flag.String("repo", "", "dispatch mode: owner/name of repo that emitted the tag")
		tag          = flag.String("tag", "", "dispatch mode: tag that was emitted (e.g. v0.7.5)")
		sha          = flag.String("sha", "", "dispatch mode: commit SHA the tag points at")
		dep          = flag.String("dep", "", "bump-dep mode: dep config key (e.g. wrangler)")
		version      = flag.String("version", "", "bump-dep mode: version to bump (e.g. v0.5.1)")
		leafBranch   = flag.String("leaf-branch", "", "bump-dep|cascade mode: leaf-repo branch the op targets (e.g. release/v2.13)")
		independents = flag.String("independents", "", "cascade mode: comma-separated independent=version pairs (e.g. wrangler=v0.5.2,lasso=v1.0.0). Empty means no explicit independents — paired deps still get picked up at their latest tag.")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// validate-config exits before reaching envSettings() so it can run
	// in CI without any secrets configured. Load already did the work.
	if *mode == "validate-config" {
		fmt.Printf("config OK: %d repos\n", len(cfg.Repos))
		return
	}

	settings := envSettings()
	ctx := context.Background()
	gh := ghclient.NewClient(ctx, settings.GitHubToken)
	if err := cfg.DiscoverModules(ctx, gh); err != nil {
		log.Fatalf("discover modules: %v", err)
	}

	r, err := reconcile.New(cfg, settings)
	if err != nil {
		log.Fatalf("init reconciler: %v", err)
	}

	switch *mode {
	case "cron":
		if err := r.RunCron(ctx); err != nil {
			log.Fatalf("cron: %v", err)
		}
	case "dispatch":
		if *repo == "" || *tag == "" {
			log.Fatalf("dispatch: -repo and -tag are required")
		}
		if err := r.RunDispatch(ctx, reconcile.DispatchEvent{Repo: *repo, Tag: *tag, SHA: *sha}); err != nil {
			log.Fatalf("dispatch: %v", err)
		}
	case "bump-dep":
		if *dep == "" || *version == "" || *leafBranch == "" {
			log.Fatalf("bump-dep: -dep, -version, -leaf-branch are all required")
		}
		if err := r.RunBumpDep(ctx, *dep, *version, *leafBranch); err != nil {
			log.Fatalf("bump-dep: %v", err)
		}
	case "cascade":
		if *leafBranch == "" {
			log.Fatalf("cascade: -leaf-branch is required")
		}
		indep, err := parseIndependents(*independents)
		if err != nil {
			log.Fatalf("cascade: %v", err)
		}
		if err := r.RunCascade(ctx, *leafBranch, indep); err != nil {
			log.Fatalf("cascade: %v", err)
		}
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
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
