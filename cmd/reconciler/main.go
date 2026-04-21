// Reconciler entrypoint. Invoked by .github/workflows/reconciler.yml (cron)
// and .github/workflows/tag-emitted.yml (repository_dispatch).
//
// Mode selects the entry path:
//
//	-mode=cron      Full sweep: passes 1-4 over every upstream/downstream pair.
//	-mode=dispatch  Scoped to a single just-emitted tag. Skips the upstream
//	                discovery (passes 2-4 still run for completeness).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/reconcile"
)

func main() {
	var (
		mode       = flag.String("mode", "cron", "cron|dispatch")
		configPath = flag.String("config", "dependencies.yaml", "path to dependencies.yaml")
		repo       = flag.String("repo", "", "dispatch mode: owner/name of repo that emitted the tag")
		tag        = flag.String("tag", "", "dispatch mode: tag that was emitted (e.g. v0.7.5)")
		sha        = flag.String("sha", "", "dispatch mode: commit SHA the tag points at")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	r, err := reconcile.New(cfg, envSettings())
	if err != nil {
		log.Fatalf("init reconciler: %v", err)
	}

	ctx := context.Background()
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
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

func envSettings() reconcile.Settings {
	return reconcile.Settings{
		AutomationRepo: requireEnv("AUTOMATION_REPO"),
		GitHubToken:    requireEnv("GH_BOT_TOKEN"),
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
