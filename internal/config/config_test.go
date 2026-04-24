package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML drops `body` into a temp file and returns the path. Used so the
// tests exercise the real Load() path (parse + applyDefaults + validate).
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deps.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_ObjectDepsAndDefaultStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    module: github.com/x/rancher
    deps:
      - {name: steve}
      - {name: webhook, strategy: bump-webhook}
  steve:
    kind: paired
    module: github.com/x/steve
  webhook:
    kind: paired
    module: github.com/x/webhook
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rancher := cfg.Repos["rancher"]
	if len(rancher.Deps) != 2 {
		t.Fatalf("rancher deps: %+v", rancher.Deps)
	}
	if rancher.Deps[0].Name != "steve" || rancher.Deps[0].Strategy != StrategyGoGet {
		t.Errorf("steve dep should default to go-get: %+v", rancher.Deps[0])
	}
	if rancher.Deps[1].Name != "webhook" || rancher.Deps[1].Strategy != StrategyBumpWebhook {
		t.Errorf("webhook dep should preserve strategy: %+v", rancher.Deps[1])
	}
}

func TestLoad_BranchTemplate(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    module: github.com/x/rancher
    deps:
      - {name: chart, strategy: order}
  chart:
    kind: paired
    module: github.com/x/chart
    branch-template: "dev-v{rancher-minor}"
    deps: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	chart := cfg.Repos["chart"]
	if chart.BranchTemplate != "dev-v{rancher-minor}" {
		t.Errorf("branch-template: got %q", chart.BranchTemplate)
	}
}

func TestLoad_RejectsMissingDepName(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    module: github.com/x/rancher
    deps:
      - {strategy: go-get}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Errorf("want 'missing name' error, got %v", err)
	}
}

func TestLoad_RejectsUnknownStrategy(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    module: github.com/x/rancher
    deps:
      - {name: steve, strategy: nope}
  steve:
    kind: paired
    module: github.com/x/steve
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("want 'unknown strategy' error, got %v", err)
	}
}

func TestLoad_RejectsDuplicateDep(t *testing.T) {
	path := writeYAML(t, `
repos:
  rancher:
    kind: leaf
    module: github.com/x/rancher
    deps:
      - {name: steve}
      - {name: steve, strategy: bump-webhook}
  steve:
    kind: paired
    module: github.com/x/steve
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("want 'listed twice' error, got %v", err)
	}
}

func TestLoad_RejectsBranchTemplateOnNonPaired(t *testing.T) {
	path := writeYAML(t, `
repos:
  thing:
    kind: independent
    module: github.com/x/thing
    branch-template: "dev-v{rancher-minor}"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "branch-template is only valid on kind=paired") {
		t.Errorf("want branch-template/kind error, got %v", err)
	}
}

func TestLoad_RejectsUnknownPlaceholder(t *testing.T) {
	path := writeYAML(t, `
repos:
  chart:
    kind: paired
    module: github.com/x/chart
    branch-template: "dev-v{leaf-branch}"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Errorf("want unknown-placeholder error, got %v", err)
	}
}

func TestResolveBranch_BranchTemplate(t *testing.T) {
	// {rancher-minor} expands to a value that already includes the "v"
	// prefix (e.g. "v2.16" via VERSION.md.LookupMinor). Templates therefore
	// shouldn't repeat the "v" — "dev-{rancher-minor}" yields "dev-v2.16".
	r := Repo{Kind: KindPaired, Module: "github.com/x/chart", BranchTemplate: "dev-{rancher-minor}"}
	got, err := r.ResolveBranch("v2.16", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "dev-v2.16" {
		t.Errorf("got %q want %q", got, "dev-v2.16")
	}
}

func TestResolveBranch_VersionTable(t *testing.T) {
	r := Repo{Kind: KindPaired, Module: "github.com/x/steve"}
	tbl := &VersionTable{Rows: []VersionRow{
		{Branch: "release/v0.7", Minor: "v0.7", Pair: "v2.13"},
	}}
	got, err := r.ResolveBranch("v2.13", tbl)
	if err != nil || got != "release/v0.7" {
		t.Errorf("got %q err=%v", got, err)
	}
}

func TestResolveBranch_NoMatchReturnsEmpty(t *testing.T) {
	r := Repo{Kind: KindPaired, Module: "github.com/x/steve"}
	tbl := &VersionTable{Rows: []VersionRow{{Branch: "main", Minor: "v0.9", Pair: "v2.15"}}}
	got, err := r.ResolveBranch("v2.13", tbl)
	if err != nil || got != "" {
		t.Errorf("expected empty string + nil err for no-match, got %q err=%v", got, err)
	}
}

func TestResolveBranch_MissingTableErrors(t *testing.T) {
	r := Repo{Kind: KindPaired, Module: "github.com/x/steve"}
	if _, err := r.ResolveBranch("v2.13", nil); err == nil {
		t.Fatal("expected error for nil table on VERSION.md path")
	}
}

func TestDepStrategy(t *testing.T) {
	r := Repo{Deps: []Dep{
		{Name: "steve", Strategy: StrategyGoGet},
		{Name: "webhook", Strategy: StrategyBumpWebhook},
	}}
	if r.DepStrategy("webhook") != StrategyBumpWebhook {
		t.Errorf("webhook strategy: got %q", r.DepStrategy("webhook"))
	}
	if r.DepStrategy("missing") != StrategyGoGet {
		t.Errorf("missing dep should default to go-get, got %q", r.DepStrategy("missing"))
	}
}

func TestDependents_FindsAllRegardlessOfStrategy(t *testing.T) {
	cfg := &Config{Repos: map[string]Repo{
		"rancher": {Kind: KindLeaf, Module: "github.com/x/rancher", Deps: []Dep{
			{Name: "chart", Strategy: StrategyOrder},
			{Name: "webhook", Strategy: StrategyBumpWebhook},
		}},
		"chart":   {Kind: KindPaired, Module: "github.com/x/chart", BranchTemplate: "dev-v{rancher-minor}"},
		"webhook": {Kind: KindPaired, Module: "github.com/x/webhook"},
	}}
	got := cfg.Dependents("chart")
	if len(got) != 1 || got[0] != "rancher" {
		t.Errorf("Dependents should include order-edge dependents (cascade walker needs them): got %v", got)
	}
}
